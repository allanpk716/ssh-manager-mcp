package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// mkPending builds a minimal pending pairing row for tests: id must be provided
// (caller keeps control of collisions), the rest filled with stand-in bytes.
func mkPending(id byte, name string, enrollDeadline int64) *PendingPairing {
	return &PendingPairing{
		ID:             bytes.Repeat([]byte{id}, 32),
		Name:           name,
		TargetURL:      "https://10.0.0.5:7878",
		ClientPub:      bytes.Repeat([]byte{1}, 32),
		Cnonce:         bytes.Repeat([]byte{2}, 16),
		ServerPub:      bytes.Repeat([]byte{3}, 32),
		Snonce:         bytes.Repeat([]byte{4}, 16),
		Sig:            bytes.Repeat([]byte{5}, 64),
		ProfileHint:    "dev profile",
		State:          "pending",
		SourceIP:       "10.0.0.9",
		EnrollDeadline: enrollDeadline,
	}
}

// TestPairingStateMachine drives the CAS/time-predicate core: enroll → approve
// (CAS + enroll window) → finish window off → ErrPairingWindow. Also pins the
// enroll zero-side-effect nail (a pending row mints NOTHING) and lazy cleanup.
func TestPairingStateMachine(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	p := mkPending(9, "laptop", now+600)
	if err := s.AddPendingPairing(p, 2, 32); err != nil {
		t.Fatalf("AddPendingPairing: %v", err)
	}
	// 未认证零副作用钉子:pending 存在即不改任何 token——enroll 阶段不得铸码/吊销。
	var tokCount, projCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cache_tokens`).Scan(&tokCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projCount); err != nil {
		t.Fatal(err)
	}
	if tokCount != 0 || projCount != 0 {
		t.Fatalf("enroll must have zero side effects, got tokens=%d projects=%d", tokCount, projCount)
	}
	// list: the row is visible as pending, unexpired.
	list, err := s.ListPendingPairing()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListPendingPairing = %v, %v (want 1 row)", list, err)
	}
	if list[0].Name != "laptop" || list[0].State != "pending" || !bytes.Equal(list[0].ID, p.ID) {
		t.Fatalf("listed row mismatch: %+v", list[0])
	}

	// CAS:过期行不可批准(时间谓词进 CAS,不依赖 lazy 清理)。
	p2 := mkPending(8, "ghost", now-1)
	if err := s.AddPendingPairing(p2, 2, 32); err != nil {
		t.Fatalf("AddPendingPairing(expired): %v", err)
	}
	if ok, err := s.ApprovePairing(p2.ID, "prof"); ok || err != nil {
		t.Fatalf("expired row must not approve: ok=%v err=%v", ok, err)
	}

	// 正常 CAS:批准成功,窗口盖章(approved_deadline = now+120)。
	if ok, err := s.ApprovePairing(p.ID, "prof"); !ok || err != nil {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	// 二次批准:CAS 败(state 已非 pending)。
	if ok, _ := s.ApprovePairing(p.ID, "prof"); ok {
		t.Fatal("double approve must lose the CAS")
	}
	list, err = s.ListPendingPairing()
	if err != nil {
		t.Fatal(err)
	}
	var approved *PendingPairing
	for i := range list {
		if bytes.Equal(list[i].ID, p.ID) {
			approved = &list[i]
		}
	}
	if approved == nil || approved.State != "approved" || approved.Profile != "prof" {
		t.Fatalf("approved row missing/mismatch: %+v", list)
	}
	if approved.ApprovedDeadline <= time.Now().Unix() || approved.ApprovedDeadline > time.Now().Unix()+121 {
		t.Fatalf("approved_deadline not stamped ~now+120: %d", approved.ApprovedDeadline)
	}
	// approve 落同事务 audit(pair.approve)。
	if got := countAuditAction(t, s, "pair.approve"); got != 1 {
		t.Fatalf("pair.approve audit rows = %d, want 1", got)
	}

	// finish 时间谓词:approved_deadline 过期 → ErrPairingWindow(无 mint 副作用)。
	s.NowFn = func() time.Time { return time.Now().Add(3 * time.Minute) }
	sealed, err := s.FinishPairing(p.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return []byte("sealed"), nil })
	if !errors.Is(err, ErrPairingWindow) {
		t.Fatalf("finish past window: err=%v, want ErrPairingWindow", err)
	}
	if sealed != nil {
		t.Fatal("no sealed bytes may be returned on window miss")
	}
	// 行仍在且 state 仍 approved(过期不依赖清理,行留给 lazy 清理)。
	var state string
	if err := s.db.QueryRow(`SELECT state FROM pairing_pending WHERE id=?`, p.ID).Scan(&state); err != nil || state != "approved" {
		t.Fatalf("row state after window miss = %q, %v (want approved, untouched)", state, err)
	}

	// 懒清理:读时把过期 pending/approved 行打 expired、过期 delivered 行删除。
	if _, err := s.ListPendingPairing(); err != nil { // NowFn 仍 +3min
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT state FROM pairing_pending WHERE id=?`, p2.ID).Scan(&state); err != nil || state != "expired" {
		t.Fatalf("expired pending row must be lazily marked expired, got %q %v", state, err)
	}
	if err := s.db.QueryRow(`SELECT state FROM pairing_pending WHERE id=?`, p.ID).Scan(&state); err != nil || state != "expired" {
		t.Fatalf("expired approved row must be lazily marked expired, got %q %v", state, err)
	}
	s.NowFn = nil // 恢复真实时钟
	if list, err := s.ListPendingPairing(); err != nil || len(list) != 0 {
		t.Fatalf("after lazy cleanup the queue must be empty, got %v, %v", list, err)
	}
}

// TestPairingEnroll_Audit pins the 终审修复 I1: pair.enroll commits IN the enroll
// transaction (§3.3-8) — ① a successful enroll leaves exactly one pair.enroll row
// whose Command is the sanitized whitelist JSON (name/ip only; the row's key
// material never leaks) ② a failed INSERT (duplicate id → UNIQUE) rolls the audit
// back with the row ③ a quota refusal (pre-INSERT) writes nothing ④ expiry (lazy
// read-path hygiene) is not an audited event.
func TestPairingEnroll_Audit(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	p := mkPending(31, "laptop", now+600)
	if err := s.AddPendingPairing(p, 2, 32); err != nil {
		t.Fatalf("AddPendingPairing: %v", err)
	}
	if got := countAuditAction(t, s, "pair.enroll"); got != 1 {
		t.Fatalf("pair.enroll audit rows = %d, want 1", got)
	}
	rows, err := s.QueryAudit(AuditFilter{Actions: []string{"pair.enroll"}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("QueryAudit(pair.enroll) = %v, %v", rows, err)
	}
	// 白名单字节稳定:仅 name/ip(json.Marshal 键排序),公钥/nonce/sig 不落。
	if want := `{"ip":"10.0.0.9","name":"laptop"}`; rows[0].Command != want {
		t.Fatalf("pair.enroll Command = %q, want sanitized whitelist %q", rows[0].Command, want)
	}
	assertAuditNeverContains(t, s, []string{hexOf(p.ClientPub), hexOf(p.Sig)})

	// 同 id 二次 enroll → INSERT UNIQUE 失败 → 整事务回滚:audit 零新增。
	if err := s.AddPendingPairing(p, 2, 32); err == nil {
		t.Fatal("duplicate-id enroll must surface the UNIQUE error")
	}
	if got := countAuditAction(t, s, "pair.enroll"); got != 1 {
		t.Fatalf("failed enroll must leave audit untouched, got %d pair.enroll rows", got)
	}

	// 配额拒绝(pre-INSERT)→ 零 audit。
	if err := s.AddPendingPairing(mkPending(32, "overflow", now+600), 1, 32); !errors.Is(err, ErrPairingQuota) {
		t.Fatalf("quota refusal: err=%v, want ErrPairingQuota", err)
	}
	if got := countAuditAction(t, s, "pair.enroll"); got != 1 {
		t.Fatalf("quota refusal must write no audit, got %d pair.enroll rows", got)
	}

	// 过期 = 懒清理(读路径),不产生任何 audit 事件。
	s.NowFn = func() time.Time { return time.Now().Add(15 * time.Minute) }
	if _, err := s.ListPendingPairing(); err != nil {
		t.Fatal(err)
	}
	s.NowFn = nil
	if got := countAuditAction(t, s, "pair.enroll"); got != 1 {
		t.Fatalf("lazy expiry must not audit, got %d pair.enroll rows", got)
	}
}

// hexOf is a test helper for the never-audited assertions on binary fields.
func hexOf(b []byte) string { return fmt.Sprintf("%x", b) }

// TestPairingRejectAndQuota pins RejectPairing's CAS/predicate and AddPendingPairing's
// per-IP + global quotas (ErrPairingQuota) with the quota scope = live rows only
// (rejected/expired rows stop counting).
func TestPairingRejectAndQuota(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	a := mkPending(1, "a", now+600)
	b := mkPending(2, "b", now+600)
	if err := s.AddPendingPairing(a, 2, 32); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPendingPairing(b, 2, 32); err != nil {
		t.Fatal(err)
	}
	// per-IP 配额:第三发同 IP → ErrPairingQuota。
	if err := s.AddPendingPairing(mkPending(3, "c", now+600), 2, 32); !errors.Is(err, ErrPairingQuota) {
		t.Fatalf("third same-IP enroll: err=%v, want ErrPairingQuota", err)
	}
	// reject 一行(同谓词 CAS)后配额释放:同 IP 再发可行。
	if ok, err := s.RejectPairing(a.ID); !ok || err != nil {
		t.Fatalf("reject: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.RejectPairing(a.ID); ok {
		t.Fatal("double reject must lose the CAS")
	}
	if err := s.AddPendingPairing(mkPending(4, "d", now+600), 2, 32); err != nil {
		t.Fatalf("enroll after a reject freed the quota slot: %v", err)
	}
	// rejected 行不再可见、不可批准。
	list, _ := s.ListPendingPairing()
	for _, r := range list {
		if r.State == "rejected" {
			t.Fatalf("rejected row must not be listed: %+v", r)
		}
	}
	if ok, _ := s.ApprovePairing(a.ID, "prof"); ok {
		t.Fatal("rejected row must not approve")
	}
	// reject 落同事务 audit。
	if got := countAuditAction(t, s, "pair.reject"); got != 1 {
		t.Fatalf("pair.reject audit rows = %d, want 1", got)
	}

	// global 配额:不同 IP 也受 globalMax 遏制。
	s2 := newTestStore(t)
	c := mkPending(5, "g1", now+600)
	c.SourceIP = "1.1.1.1"
	if err := s2.AddPendingPairing(c, 10, 1); err != nil {
		t.Fatal(err)
	}
	d := mkPending(6, "g2", now+600)
	d.SourceIP = "2.2.2.2"
	if err := s2.AddPendingPairing(d, 10, 1); !errors.Is(err, ErrPairingQuota) {
		t.Fatalf("global quota: err=%v, want ErrPairingQuota", err)
	}
	// 已批准行仍占配额(在队)。
	if ok, _ := s2.ApprovePairing(c.ID, "prof"); !ok {
		t.Fatal("approve must succeed")
	}
	if err := s2.AddPendingPairing(d, 10, 1); !errors.Is(err, ErrPairingQuota) {
		t.Fatalf("approved rows still count against quota: %v", err)
	}
}

// TestFinishPairing_IdempotentReplayAndLimit pins the delivered replay path: the
// cached sealed blob is returned verbatim WITHOUT re-checking ack, replay_count
// increments per replay, >10 → row expired + ErrPairingReplayLimit, and past the
// delivered TTL → ErrPairingWindow.
func TestFinishPairing_IdempotentReplayAndLimit(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	p := mkPending(7, "replayer", now+600)
	if err := s.AddPendingPairing(p, 10, 100); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ApprovePairing(p.ID, "prof"); !ok || err != nil {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	sealed1, err := s.FinishPairing(p.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return []byte("sealed-first"), nil })
	if err != nil {
		t.Fatalf("first finish: %v", err)
	}
	// delivered 落库 + TTL 盖章(approved_deadline 被重写为 now+deliveredTTL)。
	var state string
	var replayCount int
	if err := s.db.QueryRow(`SELECT state, replay_count FROM pairing_pending WHERE id=?`, p.ID).Scan(&state, &replayCount); err != nil || state != "delivered" {
		t.Fatalf("post-finish state = %q, %v (want delivered)", state, err)
	}
	if replayCount != 0 {
		t.Fatalf("initial delivery must not touch replay_count, got %d", replayCount)
	}
	var approvedDeadline int64
	if err := s.db.QueryRow(`SELECT approved_deadline FROM pairing_pending WHERE id=?`, p.ID).Scan(&approvedDeadline); err != nil {
		t.Fatal(err)
	}
	if approvedDeadline < time.Now().Unix()+pairDeliveredTTLSec-2 || approvedDeadline > time.Now().Unix()+pairDeliveredTTLSec+2 {
		t.Fatalf("delivered TTL stamp = %d, want ~now+%d", approvedDeadline, pairDeliveredTTLSec)
	}

	// 重放:ackOK=false 也必须回吐同密文(幂等不验 ack)。
	for i := 1; i <= pairReplayLimit; i++ {
		sealedN, err := s.FinishPairing(p.ID, func() bool { return false },
			func(tx *sql.Tx) ([]byte, error) { return []byte("MUST-NOT-MINT"), nil })
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !bytes.Equal(sealedN, sealed1) {
			t.Fatalf("replay %d returned different sealed bytes", i)
		}
	}
	if err := s.db.QueryRow(`SELECT replay_count FROM pairing_pending WHERE id=?`, p.ID).Scan(&replayCount); err != nil || replayCount != pairReplayLimit {
		t.Fatalf("replay_count = %d, %v (want %d)", replayCount, err, pairReplayLimit)
	}
	// 超限:第 11 发 → 置 expired + ErrPairingReplayLimit。
	_, err = s.FinishPairing(p.ID, func() bool { return false },
		func(tx *sql.Tx) ([]byte, error) { return []byte("MUST-NOT-MINT"), nil })
	if !errors.Is(err, ErrPairingReplayLimit) {
		t.Fatalf("replay over limit: err=%v, want ErrPairingReplayLimit", err)
	}
	if err := s.db.QueryRow(`SELECT state FROM pairing_pending WHERE id=?`, p.ID).Scan(&state); err != nil || state != "expired" {
		t.Fatalf("over-limit row must be expired, got %q %v", state, err)
	}
	// expired 行再 finish → ErrPairingWindow(不是 replay 路径)。
	if _, err := s.FinishPairing(p.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return nil, nil }); !errors.Is(err, ErrPairingWindow) {
		t.Fatalf("finish on expired: err=%v, want ErrPairingWindow", err)
	}

	// delivered TTL 过期:新行走完 approve→finish,时钟 +10min → ErrPairingWindow。
	q := mkPending(11, "ttlrow", now+600)
	if err := s.AddPendingPairing(q, 10, 100); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ApprovePairing(q.ID, "prof"); !ok {
		t.Fatal("approve ttlrow")
	}
	if _, err := s.FinishPairing(q.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return []byte("sealed-ttl"), nil }); err != nil {
		t.Fatal(err)
	}
	s.NowFn = func() time.Time { return time.Now().Add(10 * time.Minute) }
	if _, err := s.FinishPairing(q.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return nil, nil }); !errors.Is(err, ErrPairingWindow) {
		t.Fatalf("delivered TTL over: err=%v, want ErrPairingWindow", err)
	}
	// TTL 过的 delivered 行被懒清理(删除)。
	if _, err := s.ListPendingPairing(); err != nil {
		t.Fatal(err)
	}
	s.NowFn = nil
	if err := s.db.QueryRow(`SELECT state FROM pairing_pending WHERE id=?`, q.ID).Scan(&state); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delivered row past TTL must be lazily deleted, got state=%q err=%v", state, err)
	}
	// 未知 id → ErrPairingWindow。
	if _, err := s.FinishPairing(bytes.Repeat([]byte{1}, 32), func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return nil, nil }); !errors.Is(err, ErrPairingWindow) {
		t.Fatalf("unknown id: err=%v, want ErrPairingWindow", err)
	}
}

// TestMintPairing_NeverPullAutoRevoke: replaceInactive 复查——last_pull 为零(从未拉取)
// 的同名旧码被收编(吊旧发新 + pair.autorevoke audit);last_pull 非零 → ErrPairingNameActive
// 且整体零变更。
func TestMintPairing_NeverPullAutoRevoke(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "prof")

	// 腿 1:last_pull 为零 → 事务内复查后收编。
	_, oldPlain, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	deviceCode, _, err := s.MintPairingCredentials(tx, "laptop", pid, true)
	if err != nil {
		tx.Rollback()
		t.Fatalf("mint (never-pulled): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 新码验证为 active;旧码明文已死。
	ct, err := s.VerifyCacheToken(deviceCode)
	if err != nil || ct == nil || ct.Status != models.CacheTokenActive {
		t.Fatalf("minted device code must verify active, got %+v err=%v", ct, err)
	}
	if ct, _ := s.VerifyCacheToken(oldPlain); ct != nil {
		t.Fatal("revoked old plaintext must not verify")
	}
	// 旧行已被 addCacheTokenTx 的 revoke-reclaim 删除(标准重发路径)——同名恰一个 active 行。
	var nLaptop int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cache_tokens WHERE name='laptop'`).Scan(&nLaptop); err != nil || nLaptop != 1 {
		t.Fatalf("exactly one laptop row must exist after reclaim+reissue, got %d, %v", nLaptop, err)
	}
	// pair.autorevoke 同事务 audit;Command 脱敏(不含任何码明文)。
	if got := countAuditAction(t, s, "pair.autorevoke"); got != 1 {
		t.Fatalf("pair.autorevoke audit rows = %d, want 1", got)
	}
	assertAuditNeverContains(t, s, []string{oldPlain, deviceCode})

	// 腿 2:last_pull 非零(在用)→ ErrPairingNameActive,事务零变更。
	_, deskPlain, err := s.AddCacheToken("desk", pid)
	if err != nil {
		t.Fatal(err)
	}
	touchTokenForPull(t, s, "desk")
	before := snapshotCounts(t, s)
	tx2, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.MintPairingCredentials(tx2, "desk", pid, true)
	if !errors.Is(err, ErrPairingNameActive) {
		tx2.Rollback()
		t.Fatalf("mint on pulled name: err=%v, want ErrPairingNameActive", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}
	after := snapshotCounts(t, s)
	if before != after {
		t.Fatalf("ErrPairingNameActive must change nothing, before=%+v after=%+v", before, after)
	}
	if ct, _ := s.VerifyCacheToken(deskPlain); ct == nil {
		t.Fatal("the in-use device code must stay active after a refused mint")
	}
}

// TestMintPairing_ReuseRules covers the pair_generated × profile four-branch matrix:
//
//  1. no project           → created fresh, pair_generated=1, token verifies
//  2. pair_generated+same  → rotate in place (old token dead, same project id)
//  3. pair_generated+diff  → refused
//  4. owner project (flag 0) → refused
func TestMintPairing_ReuseRules(t *testing.T) {
	s := newTestStore(t)
	pidA := seedProfile(t, s, "prof-a")
	pidB := seedProfile(t, s, "prof-b")

	// 分支 1:无 project → 新建 + pair_generated=1。
	tx, _ := s.db.Begin()
	_, token1, err := s.MintPairingCredentials(tx, "laptop", pidA, false)
	if err != nil {
		tx.Rollback()
		t.Fatalf("mint branch1: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	proj, err := s.GetProjectByName("pair-laptop")
	if err != nil || proj == nil {
		t.Fatalf("pair-laptop must exist: %v, %v", proj, err)
	}
	if !proj.PairGenerated || proj.ProfileID != pidA {
		t.Fatalf("branch1 project flags: pairGenerated=%v profile=%q", proj.PairGenerated, proj.ProfileID)
	}
	if vp, _ := s.VerifyToken(token1); vp == nil || vp.ID != proj.ID {
		t.Fatalf("branch1 token must verify against the new project, got %+v", vp)
	}
	projID := proj.ID

	// 分支 2:pair_generated=1 + 同 profile → 原地 rotate。replaceInactive=true 是
	// 真实重配对姿态:分支 1 留下的同名 active 码(零拉取)按撞名规则被收编。
	tx, _ = s.db.Begin()
	_, token2, err := s.MintPairingCredentials(tx, "laptop", pidA, true)
	if err != nil {
		tx.Rollback()
		t.Fatalf("mint branch2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if token2 == token1 {
		t.Fatal("branch2 must rotate to a fresh token")
	}
	if vp, _ := s.VerifyToken(token1); vp != nil {
		t.Fatal("branch2: old token must stop verifying")
	}
	if vp, _ := s.VerifyToken(token2); vp == nil || vp.ID != projID {
		t.Fatal("branch2: new token must verify against the SAME project id")
	}
	if got := countAuditAction(t, s, "pair.project-rotate"); got != 1 {
		t.Fatalf("pair.project-rotate audit rows = %d, want 1", got)
	}

	// 分支 3:pair_generated=1 + 不同 profile → 拒绝且零变更(整 tx 回滚,
	// 包括 mint 已在事务内铸出的设备码)。
	before := snapshotCounts(t, s)
	tx, _ = s.db.Begin()
	_, _, err = s.MintPairingCredentials(tx, "laptop", pidB, true)
	if err == nil || errors.Is(err, ErrPairingNameActive) {
		tx.Rollback()
		t.Fatalf("branch3 must refuse rebind, got err=%v", err)
	}
	tx.Rollback()
	if after := snapshotCounts(t, s); before != after {
		t.Fatalf("branch3 refusal must change nothing, before=%+v after=%+v", before, after)
	}
	if vp, _ := s.VerifyToken(token2); vp == nil {
		t.Fatal("branch3: the working token must be untouched by the refusal")
	}

	// 分支 4:owner 手建同名 project(pair_generated=0)→ 拒绝(而非 UNIQUE 撞库)。
	if _, _, err := s.AddProject("pair-manual", pidA); err != nil {
		t.Fatal(err)
	}
	tx, _ = s.db.Begin()
	if _, _, err := s.MintPairingCredentials(tx, "manual", pidA, false); err == nil || !strings.Contains(err.Error(), "pair-generated") {
		tx.Rollback()
		t.Fatalf("branch4 must refuse a non-pair-generated project by name, got err=%v", err)
	}
	tx.Rollback()
	// pair.project-create audit 恰好一次(分支 1)。
	if got := countAuditAction(t, s, "pair.project-create"); got != 1 {
		t.Fatalf("pair.project-create audit rows = %d, want 1", got)
	}
}

// TestPairingAudit_InTransaction: mint 中途 error → 全回滚——token 零新增、
// project 零新增、audit 零行、pending 行停在 approved。
func TestPairingAudit_InTransaction(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "prof")
	now := time.Now().Unix()
	p := mkPending(21, "atomic", now+600)
	if err := s.AddPendingPairing(p, 10, 100); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ApprovePairing(p.ID, "prof"); !ok {
		t.Fatal("approve")
	}
	before := snapshotCounts(t, s)
	// mint 回调:MintPairingCredentials 在同一事务内成功写入三件套,
	// 然后人为报错 → FinishPairing 必须整体回滚。
	_, err := s.FinishPairing(p.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) {
			if _, _, merr := s.MintPairingCredentials(tx, "atomic", pid, false); merr != nil {
				return nil, merr
			}
			return nil, errors.New("boom: simulated mid-mint failure")
		})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("finish must surface the mint error, got %v", err)
	}
	after := snapshotCounts(t, s)
	if before != after {
		t.Fatalf("mid-mint failure must roll back EVERYTHING, before=%+v after=%+v", before, after)
	}
	var state string
	var sealed []byte
	if err := s.db.QueryRow(`SELECT state, delivered_sealed FROM pairing_pending WHERE id=?`, p.ID).Scan(&state, &sealed); err != nil {
		t.Fatal(err)
	}
	if state != "approved" || sealed != nil {
		t.Fatalf("row must be untouched-approved with no sealed blob, got state=%q sealed=%v", state, sealed)
	}
	// 对照组:同样的 mint 成功 → 落地,audit 行随事务落库。
	_, err = s.FinishPairing(p.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) {
			_, _, merr := s.MintPairingCredentials(tx, "atomic", pid, false)
			if merr != nil {
				return nil, merr
			}
			return []byte("sealed-ok"), nil
		})
	if err != nil {
		t.Fatalf("control finish: %v", err)
	}
	if got := countAuditAction(t, s, "pair.finish"); got != 1 {
		t.Fatalf("pair.finish audit rows = %d, want 1", got)
	}
	// 信息纪律:sealed 密文与 mint 出的两枚明文永不落 audit。
	assertAuditNeverContains(t, s, []string{"sealed-ok"})
}

// TestActiveCacheTokenInfo pins the pre-flight verdict helper (T5 consumes it at
// enroll): active + never pulled → (true,true); active + pulled → (false,true);
// absent or revoked-only → (false,false). Read-only: mints nothing.
func TestActiveCacheTokenInfo(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "prof")
	if zero, active, err := s.ActiveCacheTokenInfo("laptop"); zero || active || err != nil {
		t.Fatalf("absent name: (%v,%v,%v), want (false,false,nil)", zero, active, err)
	}
	_, plain, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatal(err)
	}
	if zero, active, err := s.ActiveCacheTokenInfo("laptop"); !zero || !active || err != nil {
		t.Fatalf("never-pulled: (%v,%v,%v), want (true,true,nil)", zero, active, err)
	}
	ct, err := s.VerifyCacheToken(plain)
	if err != nil || ct == nil {
		t.Fatalf("verify: %v %v", ct, err)
	}
	if err := s.TouchCacheToken(ct.ID); err != nil {
		t.Fatal(err)
	}
	if zero, active, err := s.ActiveCacheTokenInfo("laptop"); zero || !active || err != nil {
		t.Fatalf("pulled: (%v,%v,%v), want (false,true,nil)", zero, active, err)
	}
	if err := s.RevokeCacheToken("laptop"); err != nil {
		t.Fatal(err)
	}
	if zero, active, err := s.ActiveCacheTokenInfo("laptop"); zero || active || err != nil {
		t.Fatalf("revoked-only: (%v,%v,%v), want (false,false,nil)", zero, active, err)
	}
}

// TestPairingReadOnly_Refused: the new pairing mutations are guarded like every
// other mutation on an offline-cache store.
func TestPairingReadOnly_Refused(t *testing.T) {
	s := newTestStore(t)
	s.SetReadOnly(nil)
	if err := s.AddPendingPairing(mkPending(1, "x", time.Now().Unix()+60), 2, 32); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("AddPendingPairing: %v", err)
	}
	if _, err := s.ApprovePairing(bytes.Repeat([]byte{2}, 32), "p"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if _, err := s.RejectPairing(bytes.Repeat([]byte{2}, 32)); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("RejectPairing: %v", err)
	}
	if _, err := s.FinishPairing(bytes.Repeat([]byte{2}, 32), func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return nil, nil }); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("FinishPairing: %v", err)
	}
}

// TestPairingMigration pins the Plan 42 schema evolution on a LEGACY DB (pre-T4
// shape: projects without pair_generated, no pairing_pending table): Open adds the
// column with DEFAULT 0 (existing rows read as owner-created, never pair-reusable)
// and creates the queue table. Fresh DBs get both straight from schemaSQL — also
// asserted here via newTestStore round-trips.
func TestPairingMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE profiles (
		  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		);
		CREATE TABLE projects (
		  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		  token_hash BLOB NOT NULL, token_salt BLOB NOT NULL, token_prefix TEXT NOT NULL,
		  profile_id TEXT NOT NULL REFERENCES profiles(id),
		  status TEXT NOT NULL DEFAULT 'active',
		  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		);
		INSERT INTO profiles (id,name,created_at,updated_at) VALUES ('prof1','dev',1,1);
		INSERT INTO projects (id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at)
		VALUES ('p1','legacy-agent',x'00',x'00','pfx00000','prof1','active',1,1);
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(path, mk)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()
	// legacy 行读回 PairGenerated=false(默认 0),model 层可正常消费。
	ps, err := s.ListProjects()
	if err != nil || len(ps) != 1 {
		t.Fatalf("ListProjects after migrate = %v, %v", ps, err)
	}
	if ps[0].Name != "legacy-agent" || ps[0].PairGenerated {
		t.Fatalf("legacy row must read back pair_generated=0, got %+v", ps[0])
	}
	// pairing_pending 表已就位。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pairing_pending`).Scan(&n); err != nil {
		t.Fatalf("pairing_pending table missing after migrate: %v", err)
	}
	// 新库直读:FRESH schema 也带列和表(addProjectTx 的 INSERT 依赖列存在)。
	s2 := newTestStore(t)
	if _, _, err := s2.AddProject("fresh", func() string { pid, _ := s2.AddProfile("p"); return pid }()); err != nil {
		t.Fatalf("fresh-DB AddProject (pair_generated column must exist): %v", err)
	}
	if fp, _ := s2.GetProjectByName("fresh"); fp == nil || fp.PairGenerated {
		t.Fatalf("fresh AddProject must default PairGenerated=false, got %+v", fp)
	}
}

// TestExpireInFlightPairings pins the T5→T6 handoff fix (Plan 42 批1): a serve
// restart loses the in-memory X25519 private keys, so every LIVE (pending/
// approved) row is unfinishable — RunServe expires them up front so a stale
// client's finish poll gets the frozen 410 (ErrPairingWindow) immediately.
// Terminal rows are untouched: delivered keeps its self-contained replay cache
// (finish replay needs no in-memory key), expired stays expired.
func TestExpireInFlightPairings(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	pPending := mkPending(1, "p-pending", now+600)
	if err := s.AddPendingPairing(pPending, 0, 0); err != nil {
		t.Fatalf("AddPendingPairing(pending): %v", err)
	}
	pApproved := mkPending(2, "p-approved", now+600)
	if err := s.AddPendingPairing(pApproved, 0, 0); err != nil {
		t.Fatalf("AddPendingPairing(approved): %v", err)
	}
	if ok, err := s.ApprovePairing(pApproved.ID, "prof"); !ok || err != nil {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	pDelivered := mkPending(3, "p-delivered", now+600)
	if err := s.AddPendingPairing(pDelivered, 0, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ApprovePairing(pDelivered.ID, "prof"); !ok || err != nil {
		t.Fatalf("approve(delivered-seed): ok=%v err=%v", ok, err)
	}
	if _, err := s.FinishPairing(pDelivered.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return []byte("sealed"), nil }); err != nil {
		t.Fatalf("finish(delivered-seed): %v", err)
	}
	pExpired := mkPending(4, "p-expired", now+600)
	pExpired.State = "expired"
	if err := s.AddPendingPairing(pExpired, 0, 0); err != nil {
		t.Fatal(err)
	}

	if err := s.ExpireInFlightPairings(); err != nil {
		t.Fatalf("ExpireInFlightPairings: %v", err)
	}

	stateOf := func(p *PendingPairing) string {
		t.Helper()
		var state string
		if err := s.db.QueryRow(`SELECT state FROM pairing_pending WHERE id=?`, p.ID).Scan(&state); err != nil {
			t.Fatalf("row lookup: %v", err)
		}
		return state
	}
	if got := stateOf(pPending); got != "expired" {
		t.Fatalf("pending row must expire, got %q", got)
	}
	if got := stateOf(pApproved); got != "expired" {
		t.Fatalf("approved row must expire, got %q", got)
	}
	if got := stateOf(pDelivered); got != "delivered" {
		t.Fatalf("delivered row must keep its replay cache, got %q", got)
	}
	if got := stateOf(pExpired); got != "expired" {
		t.Fatalf("already-expired row must stay expired, got %q", got)
	}
	// 过期后 finish 立刻 410(restart 场景的冻结语义)。
	if _, err := s.FinishPairing(pPending.ID, func() bool { return true },
		func(tx *sql.Tx) ([]byte, error) { return []byte("sealed"), nil }); !errors.Is(err, ErrPairingWindow) {
		t.Fatalf("finish after expire = %v, want ErrPairingWindow", err)
	}
	// delivered 行重启后重放仍有效(replay 缓存自包含,不经内存密钥;不重 mint)。
	sealed, err := s.FinishPairing(pDelivered.ID, func() bool { return false },
		func(tx *sql.Tx) ([]byte, error) { return nil, errors.New("mint must not run on replay") })
	if err != nil || string(sealed) != "sealed" {
		t.Fatalf("delivered replay after restart-expire = (%q, %v), want cached replay", sealed, err)
	}
}

// --- helpers ----------------------------------------------------------------

func countAuditAction(t *testing.T, s *Store, action string) int {
	t.Helper()
	rows, err := s.QueryAudit(AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

type storeCounts struct{ tokens, projects, audits int }

func snapshotCounts(t *testing.T, s *Store) storeCounts {
	t.Helper()
	var c storeCounts
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cache_tokens`).Scan(&c.tokens); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&c.projects); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&c.audits); err != nil {
		t.Fatal(err)
	}
	return c
}

// assertAuditNeverContains fails if any audit Command/row text embeds one of the
// secret values (信息纪律 §3.3-8:码明文/token 永不落 audit)。
func assertAuditNeverContains(t *testing.T, s *Store, secrets []string) {
	t.Helper()
	rows, err := s.AuditRows(500)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		for _, sec := range secrets {
			if sec != "" && strings.Contains(r.Command, sec) {
				t.Fatalf("audit Command must never embed a secret value (%q… leaked)", sec[:8])
			}
		}
	}
}

// touchTokenForPull marks the named ACTIVE device code as PULLED (last_pull_at
// non-NULL) via TouchCacheToken — the in-use state the auto-revoke recheck refuses.
func touchTokenForPull(t *testing.T, s *Store, name string) {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM cache_tokens WHERE name=? AND status='active'`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchCacheToken(id); err != nil {
		t.Fatal(err)
	}
}
