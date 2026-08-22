package mcpserver

// Plan 32 T6: ExecBackgroundForProfile 全错误分支 + no-leak 网 + 成功路径。
// 断言形态照 Plan 31 core_test.go 的 assertBranch/assertNoLeak (同包复用);
// 审计行断言 Action=exec-bg-start 的分支词汇表 (spec §5)。

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/vault"
)

// bgStartRows 取全部 Action=exec-bg-start 审计行 (逐分支断言 + 计数防双写)。
func bgStartRows(t *testing.T, st *store.Store) []store.AuditRow {
	t.Helper()
	rows, err := st.AuditRows(50)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditRow
	for _, r := range rows {
		if r.Action == "exec-bg-start" {
			out = append(out, r)
		}
	}
	return out
}

// wantSoleStartRow 断言恰一行 exec-bg-start 且 Status/Command/Sudo/ServerID/
// ProjectID 全匹配——计数同时钉死「Start 已自落笔的分支不被 ForProfile 双写」。
func wantSoleStartRow(t *testing.T, st *store.Store, status, command string, sudo bool, serverID, projectID string) {
	t.Helper()
	rows := bgStartRows(t, st)
	if len(rows) != 1 {
		t.Fatalf("exec-bg-start rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Status != status || r.Command != command || r.Sudo != sudo ||
		r.ServerID != serverID || r.ProjectID != projectID {
		t.Fatalf("row = {Status:%q Command:%q Sudo:%v Server:%q Proj:%q}, want {Status:%q Command:%q Sudo:%v Server:%q Proj:%q}",
			r.Status, r.Command, r.Sudo, r.ServerID, r.ProjectID,
			status, command, sudo, serverID, projectID)
	}
}

// TestClampBgTimeout: 纯函数全分支 (spec §3): 0/负 → cap; >cap → cap; 中值直通;
// 边界 (恰等 cap) 直通。
func TestClampBgTimeout(t *testing.T) {
	const cap = time.Hour
	cases := []struct {
		name string
		in   int
		want time.Duration
	}{
		{"zero defaults to cap", 0, cap},
		{"negative defaults to cap (defensive; schema rejects)", -1, cap},
		{"mid passthrough", 60, 60 * time.Second},
		{"at cap unchanged (boundary)", 3600, cap},
		{"over cap clamped", 999999, cap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampBgTimeout(c.in, cap); got != c.want {
				t.Fatalf("clampBgTimeout(%d, %v) = %v, want %v", c.in, cap, got, c.want)
			}
		})
	}
}

// TestExecBackgroundDenied: iron rule——profile 外 server_id 拒绝, start 行
// status=denied 落笔 (Command=原文), 文本零 host 泄露。
func TestExecBackgroundDenied(t *testing.T) {
	const vh = "vault-bg.example.internal"
	st := newStore(t)
	in, _ := st.AddServer(&models.Server{Name: "in", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	out, _ := st.AddServer(&models.Server{Name: "out", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{in})
	m := newTestTM(t, 4)
	defer m.CloseAll()

	_, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, out, "echo hi", false, 0, m)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("want ErrNotInProfile, got %v", err)
	}
	assertBranch(t, err, "not in your profile")
	assertNoLeak(t, err, vh)
	wantSoleStartRow(t, st, "denied", "echo hi", false, out, "proj-bg")
}

// TestExecBackgroundNoCredential: 无凭据 server 连接前拒绝 (Plan 20 C0),
// status=no_credential, 文本带配置提示且零 host 泄露。
func TestExecBackgroundNoCredential(t *testing.T) {
	const vh = "vault-bg.example.internal"
	st := newStore(t)
	srvID, _ := st.AddServer(&models.Server{Name: "bare", Host: vh, Port: 22, User: "u"})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	defer m.CloseAll()

	_, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "echo hi", false, 0, m)
	if !errors.Is(err, vault.ErrNoCredential) {
		t.Fatalf("want vault.ErrNoCredential, got %v", err)
	}
	assertBranch(t, err, "no credential")
	assertNoLeak(t, err, vh)
	wantSoleStartRow(t, st, "no_credential", "echo hi", false, srvID, "proj-bg")
}

// TestExecBackgroundNoSudo: sudo=true 而 server 未配 sudo 凭据 → no_sudo。
// SudoPass 是 Start 入参, 解析必须先于连接 (词汇表与前台一致, 时序在 connect 前);
// fixture 用不可拨 host——分支确实在连接前触发 (无需 testsshd)。
func TestExecBackgroundNoSudo(t *testing.T) {
	const vh = "vault-bg.example.internal"
	st := newStore(t)
	srvID, _ := st.AddServer(&models.Server{Name: "nosudo", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	defer m.CloseAll()

	_, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "echo hi", true, 0, m)
	assertBranch(t, err, "sudo not configured")
	assertNoLeak(t, err, vh)
	wantSoleStartRow(t, st, "no_sudo", "echo hi", true, srvID, "proj-bg")
}

// TestExecBackgroundConnectError: 真拨 127.0.0.1:1 (拒连) → connect_error;
// 该行由 Start 落笔, ForProfile 不得双写 (恰一行)。
func TestExecBackgroundConnectError(t *testing.T) {
	st := newStore(t)
	srvID, _ := st.AddServer(&models.Server{Name: "u", Host: "127.0.0.1", Port: 1, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	defer m.CloseAll()

	_, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "true", false, 2, m)
	assertBranch(t, err, "ssh dial:")
	assertNoLeak(t, err, "127.0.0.1")
	wantSoleStartRow(t, st, "connect_error", "true", false, srvID, "proj-bg")
	if m.Len() != 0 {
		t.Fatalf("registry len = %d after refused connect, want 0 (reservation released)", m.Len())
	}
}

// TestExecBackgroundHostKeyMismatch: 真握手 + 垃圾预信任 → hostkey_mismatch;
// 行由 Start 落笔 (恰一行), 文本过同一清洗链。
func TestExecBackgroundHostKeyMismatch(t *testing.T) {
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	host := addr[:indexByte(addr, ':')]
	srv := &models.Server{
		Name: "mismatch", Host: host, Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st),
	}
	srvID, _ := st.AddServer(srv)
	_ = st.SaveHostKey(srv.Host, srv.Port, []byte("not-the-real-host-key"))
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	defer m.CloseAll()

	_, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "true", false, 5, m)
	assertBranch(t, err, "host key mismatch")
	assertNoLeak(t, err, srv.Host)
	wantSoleStartRow(t, st, "hostkey_mismatch", "true", false, srvID, "proj-bg")
}

// TestExecBackgroundLimit: max=1 白盒, 槽位被 running 任务占满 → 超限拒绝
// (哨兵 ErrBgTaskLimit), start 行 status=超限 归 ForProfile (T4 handoff)。
func TestExecBackgroundLimit(t *testing.T) {
	const vh = "vault-bg.example.internal"
	st := newStore(t)
	srvID, _ := st.AddServer(&models.Server{Name: "a", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 1)
	defer m.CloseAll()
	if _, err := m.Insert(runningSpec()); err != nil { // 占满唯一槽位 (恒 running)
		t.Fatal(err)
	}

	_, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "echo hi", false, 0, m)
	if !errors.Is(err, ErrBgTaskLimit) {
		t.Fatalf("want ErrBgTaskLimit, got %v", err)
	}
	assertBranch(t, err, "background task limit")
	assertNoLeak(t, err, vh)
	wantSoleStartRow(t, st, "超限", "echo hi", false, srvID, "proj-bg")
	if m.Len() != 1 {
		t.Fatalf("registry len = %d after limit refusal, want 1 (原任务仍在, 零预约泄漏)", m.Len())
	}
}

// TestExecBackgroundSuccess: testsshd 真链路——task_id 非空、status=running、
// effective 回显三态 (缺省→cap / 超 cap→cap / 中值直通)、start(ok) 行恰一条
// (Insert 持锁段内落笔, 不被 ForProfile 双写)、任务确在注册表跑真会话。
func TestExecBackgroundSuccess(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "out:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4) // runCap=1h → 缺省生效 3600s
	defer m.CloseAll()

	// 缺省 (0) → runCap=1h。
	out1, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "first", false, 0, m)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if out1.TaskID == "" {
		t.Fatal("task_id must be non-empty")
	}
	if out1.Status != bgStatusRunning {
		t.Fatalf("status = %q, want running", out1.Status)
	}
	if out1.EffectiveTimeoutSeconds != 3600 {
		t.Fatalf("effective = %d, want 3600 (0 → runCap 1h)", out1.EffectiveTimeoutSeconds)
	}
	// 任务确在跑 (真会话), 随后自然终态 done。
	if s := waitTerminal(t, m, out1.TaskID, 5*time.Second); s.status != bgStatusDone {
		t.Fatalf("first task status = %q, want done", s.status)
	}

	// 超 cap → 钳到 cap; 中值 → 直通。
	out2, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "second", false, 999999, m)
	if err != nil {
		t.Fatalf("start over-cap: %v", err)
	}
	if out2.EffectiveTimeoutSeconds != 3600 {
		t.Fatalf("over-cap effective = %d, want 3600", out2.EffectiveTimeoutSeconds)
	}
	out3, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "third", false, 60, m)
	if err != nil {
		t.Fatalf("start mid: %v", err)
	}
	if out3.EffectiveTimeoutSeconds != 60 {
		t.Fatalf("mid effective = %d, want 60 (直通)", out3.EffectiveTimeoutSeconds)
	}

	// start(ok) 行: 三次启动各恰一行, Command=原文, Sudo/ServerID/ProjectID 齐。
	rows := bgStartRows(t, st)
	if len(rows) != 3 {
		t.Fatalf("exec-bg-start rows = %d, want 3: %+v", len(rows), rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Status != "ok" || r.Sudo || r.ServerID != srvID || r.ProjectID != "proj-bg" {
			t.Fatalf("row = %+v, want ok/false/%s/proj-bg", r, srvID)
		}
		seen[r.Command] = true
	}
	for _, cmd := range []string{"first", "second", "third"} {
		if !seen[cmd] {
			t.Fatalf("missing start(ok) row for command %q; rows=%+v", cmd, rows)
		}
	}
}

// TestExecBackgroundManagerClosedAuditedAsError: CloseAll 竞态下的 Reserve
// 拒绝 (ErrBgManagerClosed) 在 ForProfile 词汇表落 status=error 行——
// 区分于 Start 已自审计的 connect 分支 (不落双行)。
func TestExecBackgroundManagerClosedAuditedAsError(t *testing.T) {
	const vh = "vault-bg.example.internal"
	st := newStore(t)
	srvID, _ := st.AddServer(&models.Server{Name: "a", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	m.CloseAll() // 收口后一切启动拒绝

	_, err := ExecBackgroundForProfile(context.Background(), st, "proj-bg", pid, srvID, "echo hi", false, 0, m)
	if !errors.Is(err, ErrBgManagerClosed) {
		t.Fatalf("want ErrBgManagerClosed, got %v", err)
	}
	assertNoLeak(t, err, vh)
	wantSoleStartRow(t, st, "error", "echo hi", false, srvID, "proj-bg")
}

// 静态引用锚: BgStartOutput/ExecBackgroundInput 与 ForProfile 签名 (编译期)。
var (
	_ = BgStartOutput{TaskID: "", EffectiveTimeoutSeconds: 0, Status: ""}
	_ = ExecBackgroundInput{ServerID: "", Command: "", Sudo: false, TimeoutSeconds: 0}
)
