package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ssh-manager-mcp/internal/models"
)

// Pairing state machine (Plan 42 §3.3): the `pairing_pending` table is the
// cross-process pending queue; the serve process holds the ECDH private keys in
// memory only. Every window check is a TIME PREDICATE inside the CAS/finish
// transaction — lazy cleanup (ListPendingPairing) is table hygiene only and is
// never load-bearing. All clock reads go through s.nowUnix()/s.nowTime() so tests
// can inject Store.NowFn.

// Frozen window/TTL constants (spec §3.3-2/-6, rev4 — 一字不差).
const (
	// pairApprovedWindowSec: 批准 → finish 窗口 = 120 秒。
	pairApprovedWindowSec int64 = 120
	// pairDeliveredTTLSec: delivered 行保留 5 分钟供幂等重放,过后懒清理。
	// Stamped into approved_deadline AT DELIVERY TIME — the frozen schema has no
	// delivered_at column, so approved_deadline doubles as the delivered clock
	// once state leaves 'approved'.
	pairDeliveredTTLSec int64 = 300
	// pairReplayLimit: delivered 密文重放上限;超出置 expired 并拒绝。
	pairReplayLimit = 10
)

// Sentinel pairing errors — errors.Is-discriminable. T5 maps them onto HTTP
// codes (quota/pending-dup → 409/429-class, window → 410, replay limit → 410,
// name-active → 419); the store layer never speaks HTTP.
var (
	// ErrPairingQuota: enroll exceeded the per-IP or global pending quota.
	ErrPairingQuota = errors.New("pairing pending quota exceeded")
	// ErrPairingWindow: finish/approve attempted outside the row's window or on
	// a non-actionable state (pending/rejected/expired/delivered-TTL-over/unknown id).
	ErrPairingWindow = errors.New("pairing window expired or row not actionable")
	// ErrPairingReplayLimit: delivered row replayed more than pairReplayLimit times.
	ErrPairingReplayLimit = errors.New("pairing finish replay limit exceeded")
	// ErrPairingNameActive: auto-revoke recheck found the device name PULLED since
	// enroll — the code is in use and the whole finish transaction fails closed.
	ErrPairingNameActive = errors.New("device name pulled since enroll; auto-revoke refused")
)

// pairProjectPrefix is the deterministic project identity prefix for paired
// instances ("pair-<name>"); reuse rules key off it + the pair_generated flag.
const pairProjectPrefix = "pair-"

// pairProjectName is the project name minted/reused for device name.
func pairProjectName(name string) string { return pairProjectPrefix + name }

// PendingPairing is one row of the pairing queue. State:
// pending|approved|delivered|expired|rejected|failed.
// SAS carries the 6-digit comparison code the serve process derived at enroll
// (from its in-memory X25519 key + the transcript) — the approval surfaces
// read it here to render the frozen three-piece line. Empty = written by a
// pre-2026-09-01 serve (version skew) — render the no-SAS warning, never a
// fabricated code. Key material (priv/kAck/kCreds) is NEVER in this struct.
type PendingPairing struct {
	ID               []byte
	Name             string
	TargetURL        string
	ClientPub        []byte
	Cnonce           []byte
	ServerPub        []byte
	Snonce           []byte
	Sig              []byte
	SAS              string
	ProfileHint      string
	ReplaceInactive  bool
	State            string
	Profile          string
	SourceIP         string
	EnrollDeadline   int64
	ApprovedDeadline int64
	DeliveredSealed  []byte
	ReplayCount      int
}

// AddPendingPairing enrolls one pairing request, enforcing the per-IP and global
// pending quotas (Plan 42 §3.3-1; the handler clamps both from env). Only LIVE
// rows (state pending/approved) count against quota — rejected/expired/delivered
// rows stop occupying slots. Zero side effects beyond the row itself: enroll NEVER
// touches tokens/projects (auto-revoke is deferred to the finish transaction).
// The pair.enroll audit row commits IN THE SAME transaction (§3.3-8) — a quota
// refusal or a failed INSERT rolls the audit back with the row, so a successful
// enroll and its audit line are inseparable. (Expired is NOT an audited event:
// expiry is lazy read-path hygiene and never writes.)
// Duplicate id surfaces as the raw UNIQUE error (the handler pre-checks for 409).
// perIP/globalMax <= 0 disables that limit (defensive; the handler always clamps >= 1).
func (s *Store) AddPendingPairing(p *PendingPairing, perIP, globalMax int) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if p == nil || len(p.ID) == 0 {
		return errors.New("pairing enroll: nil row or empty id")
	}
	if p.State == "" {
		p.State = "pending"
	}
	ri := 0
	if p.ReplaceInactive {
		ri = 1
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	if perIP > 0 {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pairing_pending WHERE source_ip=? AND state IN ('pending','approved')`,
			p.SourceIP,
		).Scan(&n); err != nil {
			return err
		}
		if n >= perIP {
			return fmt.Errorf("source %s: %w", p.SourceIP, ErrPairingQuota)
		}
	}
	if globalMax > 0 {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pairing_pending WHERE state IN ('pending','approved')`,
		).Scan(&n); err != nil {
			return err
		}
		if n >= globalMax {
			return fmt.Errorf("global queue full: %w", ErrPairingQuota)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO pairing_pending (
		   id,name,target_url,client_pub,cnonce,server_pub,snonce,sig,
		   profile_hint,replace_inactive,state,profile,source_ip,
		   enroll_deadline,approved_deadline,delivered_sealed,replay_count,sas
		 ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.TargetURL, p.ClientPub, p.Cnonce, p.ServerPub, p.Snonce, p.Sig,
		p.ProfileHint, ri, p.State, p.Profile, p.SourceIP,
		p.EnrollDeadline, p.ApprovedDeadline, p.DeliveredSealed, p.ReplayCount, p.SAS,
	); err != nil {
		return err
	}
	// pair.enroll 同事务 audit(§3.3-8):白名单只落 name/ip — token/码/密文/公钥
	// 等一律不落;INSERT 失败或配额拒绝时整事务回滚,audit 与行同生共死。
	if err := writeAuditTx(tx, AuditRow{
		TS:      s.nowTime(),
		Action:  "pair.enroll",
		Command: pairAuditJSON(map[string]string{"ip": p.SourceIP, "name": p.Name}),
		Status:  "ok",
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ListPendingPairing returns the actionable queue: rows in state pending (enroll
// window open) or approved (finish window open), oldest first. As a side effect it
// LAZILY cleans expired rows: past-window pending/approved rows are marked expired;
// delivered rows past the replay TTL are deleted (spec §3.3-6 表卫生). Read-only
// stores list without the cleanup writes.
func (s *Store) ListPendingPairing() ([]PendingPairing, error) {
	now := s.nowUnix()
	if !s.readOnly {
		tx, err := s.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback() // no-op after Commit
		if _, err := tx.Exec(`UPDATE pairing_pending SET state='expired' WHERE state='pending' AND enroll_deadline <= ?`, now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE pairing_pending SET state='expired' WHERE state='approved' AND approved_deadline <= ?`, now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM pairing_pending WHERE state='delivered' AND approved_deadline <= ?`, now); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.Query(
		`SELECT id,name,target_url,client_pub,cnonce,server_pub,snonce,sig,
		        profile_hint,replace_inactive,state,profile,source_ip,
		        enroll_deadline,approved_deadline,delivered_sealed,replay_count,sas
		 FROM pairing_pending
		 WHERE (state='pending' AND enroll_deadline > ?)
		    OR (state='approved' AND approved_deadline > ?)
		 ORDER BY enroll_deadline ASC, rowid ASC`, now, now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingPairing
	for rows.Next() {
		var (
			p  PendingPairing
			ri int
		)
		if err := rows.Scan(
			&p.ID, &p.Name, &p.TargetURL, &p.ClientPub, &p.Cnonce, &p.ServerPub, &p.Snonce, &p.Sig,
			&p.ProfileHint, &ri, &p.State, &p.Profile, &p.SourceIP,
			&p.EnrollDeadline, &p.ApprovedDeadline, &p.DeliveredSealed, &p.ReplayCount, &p.SAS,
		); err != nil {
			return nil, err
		}
		p.ReplaceInactive = ri != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApprovePairing flips a pending row to approved via CAS + time predicate (spec
// §3.3-3): `UPDATE ... SET state='approved' WHERE id=? AND state='pending' AND
// enroll_deadline > now` — a stale (expired) or already-adjudicated row loses the
// CAS and returns (false, nil) (the caller maps that to 409). On success the finish
// window is stamped (approved_deadline = now+120s) and the pair.approve audit row
// commits IN THE SAME transaction (§3.3-8). Approving never touches key material —
// the approval surface is key-state-free by design.
func (s *Store) ApprovePairing(id []byte, profile string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // no-op after Commit
	now := s.nowUnix()
	res, err := tx.Exec(
		`UPDATE pairing_pending SET state='approved', profile=?, approved_deadline=?
		 WHERE id=? AND state='pending' AND enroll_deadline > ?`,
		profile, now+pairApprovedWindowSec, id, now,
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // CAS miss: expired, non-pending, or unknown id
	}
	var name, sourceIP string
	if err := tx.QueryRow(`SELECT name, source_ip FROM pairing_pending WHERE id=?`, id).Scan(&name, &sourceIP); err != nil {
		return false, err
	}
	if err := writeAuditTx(tx, AuditRow{
		TS:      s.nowTime(),
		Action:  "pair.approve",
		Command: pairAuditJSON(map[string]string{"ip": sourceIP, "name": name, "profile": profile}),
		Status:  "ok",
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RejectPairing is ApprovePairing's sibling with the same CAS + time predicate;
// the row lands in the terminal 'rejected' state and stops counting against quota.
func (s *Store) RejectPairing(id []byte) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // no-op after Commit
	now := s.nowUnix()
	res, err := tx.Exec(
		`UPDATE pairing_pending SET state='rejected'
		 WHERE id=? AND state='pending' AND enroll_deadline > ?`,
		id, now,
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // same CAS contract as ApprovePairing
	}
	var name, sourceIP string
	if err := tx.QueryRow(`SELECT name, source_ip FROM pairing_pending WHERE id=?`, id).Scan(&name, &sourceIP); err != nil {
		return false, err
	}
	if err := writeAuditTx(tx, AuditRow{
		TS:      s.nowTime(),
		Action:  "pair.reject",
		Command: pairAuditJSON(map[string]string{"ip": sourceIP, "name": name}),
		Status:  "ok",
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// FinishPairing is the atomic credential-issuance step (spec §3.3-6). ONE SQLite
// transaction: ① state+window check (approved and approved_deadline > now, else
// ErrPairingWindow — never dependent on lazy cleanup) ② ackOK() adjudicates the
// HMAC (the store never sees key material; an ack miss aborts with a plain error,
// the handler maps it to 403) ③ mint(tx) runs the caller's credential minting on
// the SAME tx (MintPairingCredentials: auto-revoke recheck, code mint, project
// reuse/create, same-tx audit) ④ the row lands delivered with its replay-TTL stamp
// ⑤ commit — any failure rolls back EVERYTHING (zero tokens, zero audit rows).
//
// Idempotent replay (frozen): a delivered row inside its TTL returns the CACHED
// sealed bytes verbatim without re-checking ack and without re-minting; each replay
// bumps replay_count, and exceeding pairReplayLimit marks the row expired and fails
// with ErrPairingReplayLimit. Unknown/expired/rejected/pending rows → ErrPairingWindow.
//
// No SELECT ... FOR UPDATE: SQLite has no row locks; MaxOpenConns(1) serializes
// writers and the in-transaction state+deadline predicate is the concurrency gate.
func (s *Store) FinishPairing(id []byte, ackOK func() bool, mint func(tx *sql.Tx) ([]byte, error)) ([]byte, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if ackOK == nil || mint == nil {
		return nil, errors.New("pairing finish: nil ack/mint callback")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // no-op after Commit
	var (
		state            string
		approvedDeadline int64
		sealed           []byte
		replayCount      int
		name, sourceIP   string
		profile          string
	)
	err = tx.QueryRow(
		`SELECT state, approved_deadline, delivered_sealed, replay_count, name, source_ip, profile
		 FROM pairing_pending WHERE id=?`, id,
	).Scan(&state, &approvedDeadline, &sealed, &replayCount, &name, &sourceIP, &profile)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("pairing finish: unknown id: %w", ErrPairingWindow)
	}
	if err != nil {
		return nil, err
	}
	now := s.nowUnix()
	switch state {
	case "approved":
		// ① 时间谓词:窗口外 → ErrPairingWindow(过期行留给懒清理,不阻塞本判定)。
		if approvedDeadline <= now {
			return nil, fmt.Errorf("pairing finish: approval window over: %w", ErrPairingWindow)
		}
		// ② ack 裁决(失败不留痕:整个事务回滚,行保持 approved 可重试至窗口尽)。
		if !ackOK() {
			return nil, errors.New("pairing finish: ack mismatch")
		}
		// ③ mint(实现内做 auto-revoke 复查/铸码/project 复用/同事务 audit)。
		out, merr := mint(tx)
		if merr != nil {
			return nil, merr
		}
		// ④ delivered 落库;approved_deadline 自此转作 delivered 重放时钟。
		if _, err := tx.Exec(
			`UPDATE pairing_pending SET state='delivered', delivered_sealed=?, approved_deadline=? WHERE id=?`,
			out, now+pairDeliveredTTLSec, id,
		); err != nil {
			return nil, err
		}
		if err := writeAuditTx(tx, AuditRow{
			TS:      s.nowTime(),
			Action:  "pair.finish",
			Command: pairAuditJSON(map[string]string{"ip": sourceIP, "name": name, "profile": profile}),
			Status:  "ok",
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return out, nil
	case "delivered":
		// 幂等重放(冻结):不验 ack、不重 mint,直接回吐缓存密文。
		if approvedDeadline <= now {
			return nil, fmt.Errorf("pairing finish: delivered replay TTL over: %w", ErrPairingWindow)
		}
		replayCount++
		if replayCount > pairReplayLimit {
			if _, err := tx.Exec(`UPDATE pairing_pending SET state='expired' WHERE id=?`, id); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("pairing finish: replay limit exceeded: %w", ErrPairingReplayLimit)
		}
		if _, err := tx.Exec(`UPDATE pairing_pending SET replay_count=? WHERE id=?`, replayCount, id); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return sealed, nil
	default:
		return nil, fmt.Errorf("pairing finish: state %q: %w", state, ErrPairingWindow)
	}
}

// ExpireInFlightPairings marks every LIVE pairing row (state pending/approved)
// expired in one statement. RunServe calls it at startup (Plan 42 批1 T6): the
// serve process holds the pairing X25519 private keys in memory ONLY, so a
// restart makes every in-flight row unfinishable — expiring them up front gives
// a stale client's finish poll the frozen 410 (ErrPairingWindow) at once,
// instead of leaving zombie rows aging out through the window predicates while
// still occupying pending quotas. Delivered rows are deliberately untouched:
// the replay cache is self-contained (FinishPairing's delivered branch returns
// the stored ciphertext without any in-memory key), so a post-restart replay
// stays valid for its TTL.
func (s *Store) ExpireInFlightPairings() error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(`UPDATE pairing_pending SET state='expired' WHERE state IN ('pending','approved')`)
	return err
}

// MintPairingCredentials mints (or reuses) the pairing credentials INSIDE the
// caller's transaction — FinishPairing passes its tx so token minting, project
// reuse, audit and the delivered stamp all commit or roll back as one (§3.3-6).
//
// Steps: ① replaceInactive 复查 — the enroll-time "never pulled" verdict is
// re-checked IN the transaction (race-safe): a same-name ACTIVE code whose
// last_pull_at is non-NULL means the device came alive since enroll and the whole
// finish fails with ErrPairingNameActive; a never-pulled code is revoked
// (pair.autorevoke audit) and reclaimed by the re-issue path. ② the device code is
// minted via addCacheTokenTx (reclaims revoked residue, reserves casefold variants,
// enforces profile binding; pair.mint audit). ③ the project: an existing
// "pair-<name>" row is reused (token rotated in place — old token dies) ONLY when
// it carries pair_generated AND matches the profile; anything else is refused
// (owner-created or foreign-profile projects are never hijacked). A fresh row is
// created with pair_generated=1. Every step writes its audit row on the same tx.
//
// profile is a profile ID; both returned plaintexts are ONE-TIME (store keeps only
// hashes) and must never be logged or audited.
func (s *Store) MintPairingCredentials(tx *sql.Tx, name, profile string, replaceInactive bool) (deviceCode, projectToken string, err error) {
	if name == "" {
		return "", "", errors.New("pairing mint: empty device name")
	}
	if profile == "" {
		return "", "", errors.New("pairing mint: empty profile")
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM profiles WHERE id=?`, profile).Scan(&n); err != nil {
		return "", "", err
	}
	if n == 0 {
		return "", "", fmt.Errorf("profile %q not found", profile)
	}
	ts := s.nowTime()

	// ① auto-revoke 复查(事务内,竞态安全 — §3.3-6 ②)。
	if replaceInactive {
		var oldID string
		var lastPull sql.NullInt64
		err := tx.QueryRow(
			`SELECT id, last_pull_at FROM cache_tokens WHERE name=? AND status='active'`, name,
		).Scan(&oldID, &lastPull)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// 名下无 active 码 — 无可收编(enroll 时的判定已过时为"无撞名")。
		case err != nil:
			return "", "", err
		default:
			if lastPull.Valid {
				// enroll 时未激活、批准间隙被拉取过 = 在用码,宁可拒绝不可吞。
				return "", "", fmt.Errorf("device %q pulled since enroll (in use): %w", name, ErrPairingNameActive)
			}
			if err := revokeCacheTokenTx(tx, name); err != nil {
				return "", "", err
			}
			if err := writeAuditTx(tx, AuditRow{
				TS:      ts,
				Action:  "pair.autorevoke",
				Command: pairAuditJSON(map[string]string{"name": name}),
				Status:  "ok",
			}); err != nil {
				return "", "", err
			}
		}
	}

	// ② 铸设备码(addCacheTokenTx:reclaim revoked / casefold 变体保留 / profile 绑定)。
	if _, deviceCode, err = addCacheTokenTx(tx, name, profile); err != nil {
		return "", "", err
	}
	if err := writeAuditTx(tx, AuditRow{
		TS:      ts,
		Action:  "pair.mint",
		Command: pairAuditJSON(map[string]string{"name": name, "profile": profile}),
		Status:  "ok",
	}); err != nil {
		return "", "", err
	}

	// ③ project 复用(吊旧)或新建(pair_generated=1)— §3.3-6 ④/⑤。
	projName := pairProjectName(name)
	proj, err := lookupPairProjectTx(tx, projName)
	if err != nil {
		return "", "", err
	}
	if proj != nil {
		if !proj.PairGenerated {
			return "", "", fmt.Errorf("project %q exists but was not pair-generated; refusing to reuse — pick another instance name or remove the project first", projName)
		}
		if proj.ProfileID != profile {
			return "", "", fmt.Errorf("project %q is pair-generated but bound to profile %q, not %q; refusing to rebind", projName, proj.ProfileID, profile)
		}
		if proj.Status == models.ProjectRevoked {
			// 吊销不可逆(store 级铁律):配对流不得复活 owner 明确吊销的 identity。
			return "", "", fmt.Errorf("project %q is revoked (terminal); pick another instance name", projName)
		}
		if projectToken, err = rotateProjectTx(tx, proj.ID); err != nil {
			return "", "", err
		}
		if err := writeAuditTx(tx, AuditRow{
			TS:      ts,
			Action:  "pair.project-rotate",
			Command: pairAuditJSON(map[string]string{"name": projName, "profile": profile}),
			Status:  "ok",
		}); err != nil {
			return "", "", err
		}
	} else {
		if _, projectToken, err = addProjectTx(tx, projName, profile, true); err != nil {
			return "", "", err
		}
		if err := writeAuditTx(tx, AuditRow{
			TS:      ts,
			Action:  "pair.project-create",
			Command: pairAuditJSON(map[string]string{"name": projName, "profile": profile}),
			Status:  "ok",
		}); err != nil {
			return "", "", err
		}
	}
	return deviceCode, projectToken, nil
}

// lookupPairProjectTx resolves the pair-<name> project inside tx (nil, nil when
// absent). Reuses the shared owner-facing scan — same column set as GetProjectByName.
func lookupPairProjectTx(tx *sql.Tx, name string) (*models.Project, error) {
	return scanProjectRow(tx.QueryRow(
		`SELECT id,name,token_prefix,profile_id,status,pair_generated FROM projects WHERE name=?`, name,
	))
}

// ActiveCacheTokenInfo is the enroll-time name-collision verdict (spec §3.3-1 撞名
// 规则只查不改; T5 pre-flight): reports whether name has an ACTIVE device code and
// whether that code has NEVER pulled (last_pull_at IS NULL). active+zero → the
// enroll handler may record ReplaceInactive (the actual revoke happens at finish,
// in-transaction); active+pulled → the enroll is refused. Strictly read-only.
func (s *Store) ActiveCacheTokenInfo(name string) (lastPullZero bool, active bool, err error) {
	var lastPull sql.NullInt64
	err = s.db.QueryRow(
		`SELECT last_pull_at FROM cache_tokens WHERE name=? AND status='active'`, name,
	).Scan(&lastPull)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return !lastPull.Valid, true, nil
}

// pairAuditJSON renders the sanitized Command summary (§3.3-8 whitelist: instance
// name, profile, source IP, action outcome — NEVER token/code/pin/SAS/sealed/ack
// values). json.Marshal sorts map keys, so summaries are byte-stable.
func pairAuditJSON(fields map[string]string) string {
	b, err := json.Marshal(fields)
	if err != nil {
		return "{}" // unreachable for map[string]string; a marshal failure must never sink the tx
	}
	return string(b)
}
