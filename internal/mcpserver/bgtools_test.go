package mcpserver

// Plan 32 T6: ExecBackgroundForProfile 全错误分支 + no-leak 网 + 成功路径。
// 断言形态照 Plan 31 core_test.go 的 assertBranch/assertNoLeak (同包复用);
// 审计行断言 Action=exec-bg-start 的分支词汇表 (spec §5)。
// Plan 32 T7 (追加于本文件尾部): exec_output / exec_stop 的 encoding 两态、
// 诚实降级、超前立即返回、unknown 三因、输入拒绝、stop 语义与零审计行。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

// TestExecBackgroundToolRegistered: MCP 层接线冒烟——exec_background 已注册
// (BrokerTools[6])、成功路径的 BgStartOutput 过 SDK 输出 jsonschema 校验
// (task_id + effective 回显 86400 = 生产 runCap 24h)、profile 外 id 是
// IsError 工具错误。分支语义断言在 ForProfile 层, 此处只钉注册与序列化。
func TestExecBackgroundToolRegistered(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "out:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	server, mgr, tasks, err := NewServer(st, pid, "proj-test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()
	defer tasks.CloseAll()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := server.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srvSess.Close()
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cliSess.Close()

	res, err := cliSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": srvID, "command": "smoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("exec_background should succeed: %+v", res.Content)
	}
	if !textContains(res, `"task_id":"`) || !textContains(res, `"effective_timeout_seconds":86400`) {
		t.Fatalf("output missing task_id / effective echo (runCap 默认 24h=86400): %+v", res.Content)
	}

	res2, _ := cliSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": "bogus", "command": "x"},
	})
	if !res2.IsError || !textContains(res2, "not in your profile") {
		t.Fatalf("out-of-profile must be an IsError tool error with iron-rule text: %+v", res2.Content)
	}
}

// 静态引用锚: BgStartOutput/ExecBackgroundInput 与 ForProfile 签名 (编译期)。
var (
	_ = BgStartOutput{TaskID: "", EffectiveTimeoutSeconds: 0, Status: ""}
	_ = ExecBackgroundInput{ServerID: "", Command: "", Sudo: false, TimeoutSeconds: 0}
)

// ---------- Plan 32 T7: exec_output / exec_stop ----------

// TestClampWaitSeconds: wait 预算钳制纯函数全分支 (spec §3): 0→0 (不等待
// 立即返回)、中值直通、边界 60 直通、>60→60; 负值防御性 0 (handler 层已拒)。
func TestClampWaitSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero stays zero (no wait)", 0, 0},
		{"mid passthrough", 30, 30},
		{"at cap unchanged (boundary)", 60, 60},
		{"over cap clamped", 61, 60},
		{"far over cap clamped", 9999, 60},
		{"negative defused to zero (handler rejects)", -1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampWaitSeconds(c.in); got != c.want {
				t.Fatalf("clampWaitSeconds(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestExecOutputEncodingTextInvalidUTF8: text 模式 = 与前台 exec_command 同
// 语义——原始字节按 UTF-8 直入 JSON 字符串, 非法 UTF-8 在 JSON 序列化时被
// 替换为 U+FFFD (有损, spec §4 编码语义)。断言走真实序列化边界 (json.Marshal
// 往返): ForProfile 返回的 Go string 保真原始字节, 损失发生在 agent 可见的
// JSON 文本上。
func TestExecOutputEncodingTextInvalidUTF8(t *testing.T) {
	m := newTestTM(t, 4)
	defer m.CloseAll()
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	tk.stdout.Write([]byte("A\xFFB")) // 0xFF 恒非法 (UTF-8 任一位置的无效字节)

	out, err := ExecOutputForProfile(context.Background(), newStore(t), "proj-bg", id, 0, 0, 0, "text", m)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stdout != "A\xFFB" {
		t.Fatalf("pre-marshal stdout = %q, want raw bytes A\\xFFB (Go string 层保真)", out.Stdout)
	}
	b, jerr := json.Marshal(out)
	if jerr != nil {
		t.Fatal(jerr)
	}
	var back BgReadOutput
	if uerr := json.Unmarshal(b, &back); uerr != nil {
		t.Fatal(uerr)
	}
	if back.Stdout != "A�B" {
		t.Fatalf("JSON round-trip stdout = %q, want A\\uFFFDB (U+FFFD 替换)", back.Stdout)
	}
}

// TestExecOutputEncodingBase64CrossWindowReassembly: 编码回归锚 (spec §7)——
// 3 字节多字节字符 (中 = E4 B8 AD) 被读取窗边界切断 (首窗尾 2 字节 + 次窗头
// 1 字节), base64 两窗独立解码后拼接 == 原始字节 (跨窗口重组无损, 二进制
// 安全; text 模式此处必损——U+FFFD 各损半)。offset 两窗恒为字节口径。
func TestExecOutputEncodingBase64CrossWindowReassembly(t *testing.T) {
	m := newTestTM(t, 4)
	defer m.CloseAll()
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	orig := "AAA中BBB" // 中 = 3 字节多字节字符
	bOrig := []byte(orig)
	cut := len("AAA中") - 1 // 首窗尾 = 中 的前 2 字节 (切断点)
	st := newStore(t)

	tk.stdout.Write(bOrig[:cut])
	out1, err := ExecOutputForProfile(context.Background(), st, "proj-bg", id, 0, 0, 0, "base64", m)
	if err != nil {
		t.Fatal(err)
	}
	tk.stdout.Write(bOrig[cut:])
	out2, err := ExecOutputForProfile(context.Background(), st, "proj-bg", id, 0, out1.NextStdoutOffset, 0, "base64", m)
	if err != nil {
		t.Fatal(err)
	}

	d1, derr := base64.StdEncoding.DecodeString(out1.Stdout)
	if derr != nil {
		t.Fatalf("window1 base64 decode: %v", derr)
	}
	d2, derr2 := base64.StdEncoding.DecodeString(out2.Stdout)
	if derr2 != nil {
		t.Fatalf("window2 base64 decode: %v", derr2)
	}
	reassembled := append(d1, d2...)
	if string(reassembled) != orig {
		t.Fatalf("cross-window reassembly = %q, want original %q (多字节字符切断后无损重组)", reassembled, orig)
	}
	// 游标恒为字节口径: 首窗恰 cut 字节, 次窗推到流尾。
	if out1.NextStdoutOffset != int64(cut) {
		t.Fatalf("window1 next_stdout_offset = %d, want %d (字节口径)", out1.NextStdoutOffset, cut)
	}
	if out2.NextStdoutOffset != int64(len(bOrig)) || out2.StdoutBytesTotal != int64(len(bOrig)) {
		t.Fatalf("window2 next=%d total=%d, want %d/%d", out2.NextStdoutOffset, out2.StdoutBytesTotal, len(bOrig), len(bOrig))
	}
}

// TestExecOutputHonestDegradationGap: 游标落后保留窗首 (gap) → truncated +
// lost_* 丢弃量 + 从缓冲首给可用字节 + next=total (spec §4 诚实降级, 经
// ForProfile 全链而非 view 白盒)。
func TestExecOutputHonestDegradationGap(t *testing.T) {
	m := newTestTM(t, 4)
	defer m.CloseAll()
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	// 一次写超整窗 (1 MiB+10): 只留尾 1 MiB, total=1 MiB+10, start=10。
	tk.stdout.Write(make([]byte, int(MaxOutputBytes)+10))

	out, err := ExecOutputForProfile(context.Background(), newStore(t), "proj-bg", id, 0, 1, 0, "text", m)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated {
		t.Fatal("gap offset must set Truncated=true")
	}
	if out.LostStdoutBytes != 9 {
		t.Fatalf("LostStdoutBytes = %d, want 9 (start-since = 10-1)", out.LostStdoutBytes)
	}
	if int64(len(out.Stdout)) != MaxOutputBytes {
		t.Fatalf("gap chunk len = %d, want whole retained window %d", len(out.Stdout), MaxOutputBytes)
	}
	if out.NextStdoutOffset != MaxOutputBytes+10 || out.StdoutBytesTotal != MaxOutputBytes+10 {
		t.Fatalf("gap cursors: next=%d total=%d, want %d/%d",
			out.NextStdoutOffset, out.StdoutBytesTotal, MaxOutputBytes+10, MaxOutputBytes+10)
	}
	if out.LostStderrBytes != 0 { // stderr 未写未落后: 无第二因降级
		t.Fatalf("stderr untouched: LostStderrBytes = %d, want 0", out.LostStderrBytes)
	}
}

// TestExecOutputAheadOffsetReturnsImmediately: 超前 offset (≥ 通道 total) 不
// 空等——wait=5s 但瞬时返回, 空 chunk、next 回拉到 total (自恢复, spec §4)。
func TestExecOutputAheadOffsetReturnsImmediately(t *testing.T) {
	m := newTestTM(t, 4)
	defer m.CloseAll()
	id, err := m.Insert(runningSpec())
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := m.lookup(id)
	tk.stdout.Write([]byte("0123456789"))

	start := time.Now()
	out, err := ExecOutputForProfile(context.Background(), newStore(t), "proj-bg", id, 5, 999, 0, "text", m)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > time.Second {
		t.Fatalf("ahead offset must return immediately, took %v", elapsed)
	}
	if out.Stdout != "" {
		t.Fatalf("ahead offset stdout = %q, want empty", out.Stdout)
	}
	if out.NextStdoutOffset != 10 || out.StdoutBytesTotal != 10 {
		t.Fatalf("cursor pull-back: next=%d total=%d, want 10/10", out.NextStdoutOffset, out.StdoutBytesTotal)
	}
}

// TestExecOutputUnknownThreeCauses: unknown task_id → 三因文案 (spec §4 原文
// verbatim, expired/evicted/restarted 字样齐——泛化文案防误导排障)。
// CloseAll 摘表后同文案 (manager 关闭同为 unknown)。
func TestExecOutputUnknownThreeCauses(t *testing.T) {
	m := newTestTM(t, 4)
	st := newStore(t)

	_, err := ExecOutputForProfile(context.Background(), st, "proj-bg", "no-such-task", 0, 0, 0, "", m)
	if !errors.Is(err, ErrBgUnknownTask) {
		t.Fatalf("want ErrBgUnknownTask, got %v", err)
	}
	for _, want := range []string{"never have existed", "expired", "evicted", "restarted", "in-process only"} {
		assertBranch(t, err, want)
	}

	id2, _ := m.Insert(runningSpec())
	m.CloseAll()
	_, err = ExecOutputForProfile(context.Background(), st, "proj-bg", id2, 0, 0, 0, "text", m)
	if !errors.Is(err, ErrBgUnknownTask) {
		t.Fatalf("after CloseAll: want ErrBgUnknownTask, got %v", err)
	}
}

// TestExecOutputRejectsBadInputs: handler 级输入校验 (SDK 反射式 jsonschema
// 表达不了 minimum/enum——T6 handoff): 负 wait/offset、非法 encoding (含大写
// 变体, 无归一化) 全拒, 且先于 unknown 判定 (unknown id 也先报参数错)。
func TestExecOutputRejectsBadInputs(t *testing.T) {
	m := newTestTM(t, 4)
	defer m.CloseAll()
	st := newStore(t)
	cases := []struct {
		name    string
		wait    int
		so, eo  int64
		enc     string
		wantSub string
	}{
		{"negative wait", -1, 0, 0, "", "wait_seconds must be >= 0"},
		{"negative stdout offset", 0, -1, 0, "", "stdout_offset must be >= 0"},
		{"negative stderr offset", 0, 0, -1, "", "stderr_offset must be >= 0"},
		{"bad encoding", 0, 0, 0, "gbk", `encoding must be "text" or "base64"`},
		{"encoding case variant not normalized", 0, 0, 0, "TEXT", `encoding must be "text" or "base64"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ExecOutputForProfile(context.Background(), st, "proj-bg", "any-unknown-id", c.wait, c.so, c.eo, c.enc, m)
			assertBranch(t, err, c.wantSub)
		})
	}
}

// TestExecStopForProfile: 立即返回语义 + 幂等 + unknown。运行中 (门控真任务)
// → 返回触发时刻 "running" (不阻塞等终态); 终态后幂等回 stopped; unknown →
// 三因文案。审计: stop 调用零行——start+end 恰两行, 幂等重复 stop 不新增。
func TestExecStopForProfile(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	defer m.CloseAll()
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	id, _ := startBg(t, m, st, addr, hk, "gated", 60)
	gate.waitEntered(t, "gated")

	out, err := ExecStopForProfile(context.Background(), st, "proj-bg", id, m)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != bgStatusRunning {
		t.Fatalf("stop on running task = %q, want running (触发时刻 status, 立即返回)", out.Status)
	}
	s := waitTerminal(t, m, id, 5*time.Second)
	if s.status != bgStatusStopped {
		t.Fatalf("terminal after stop = %q, want stopped", s.status)
	}

	// 幂等: 已终态再 stop → 回其终态, 零新审计行。
	out2, err := ExecStopForProfile(context.Background(), st, "proj-bg", id, m)
	if err != nil || out2.Status != bgStatusStopped {
		t.Fatalf("idempotent stop = (%q, %v), want stopped/nil", out2.Status, err)
	}

	// unknown → 三因文案。
	_, err = ExecStopForProfile(context.Background(), st, "proj-bg", "no-such", m)
	if !errors.Is(err, ErrBgUnknownTask) {
		t.Fatalf("stop unknown = %v, want ErrBgUnknownTask", err)
	}

	// 审计恰两行 (AuditRows 倒序——end 在前): stop 调用本身零行 (end 行由任务
	// goroutine 落笔, spec §5), 幂等重停不加行。
	rows, rerr := st.AuditRows(10)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want exactly 2 (start+end; stop 本身零行): %+v", len(rows), rows)
	}
	if rows[0].Action != "exec-bg-end" || rows[0].Status != bgStatusStopped || rows[0].Command != id {
		t.Fatalf("end row = %+v, want exec-bg-end/stopped/Command=taskID", rows[0])
	}
	if rows[1].Action != "exec-bg-start" || rows[1].Status != "ok" {
		t.Fatalf("start row = %+v, want exec-bg-start/ok", rows[1])
	}
}

// TestExecStopNaturalExitRace: stop 与自然退出并发触发 (spec §3 竞态边界)——
// 终态唯一 (stopped/done 二选一, 决胜于持锁先后) + end 审计行恰一行。
func TestExecStopNaturalExitRace(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	defer m.CloseAll()
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("gated")})
	defer cleanup()
	defer gate.open()

	id, _ := startBg(t, m, st, addr, hk, "gated", 60)
	gate.waitEntered(t, "gated")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 并发: 一侧放行自然退出, 一侧 exec_stop 触发
		defer wg.Done()
		gate.open()
	}()
	out, err := ExecStopForProfile(context.Background(), st, "proj-bg", id, m)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	// stop 返回值: 落在退出前 → running; 落在退出后 → 幂等回 done。
	if out.Status != bgStatusRunning && out.Status != bgStatusDone {
		t.Fatalf("race stop return = %q, want running or done", out.Status)
	}
	s := waitTerminal(t, m, id, 5*time.Second)
	if s.status != bgStatusStopped && s.status != bgStatusDone {
		t.Fatalf("terminal after race = %q, want stopped or done (终态唯一)", s.status)
	}

	var endCount, startCount int
	rows, rerr := st.AuditRows(10)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, r := range rows {
		if r.Action == "exec-bg-end" && r.Command == id {
			endCount++
		}
		if r.Action == "exec-bg-start" {
			startCount++
		}
	}
	if startCount != 1 || endCount != 1 {
		t.Fatalf("audit: start=%d end=%d, want 1/1 (终态唯一, 恰一行 end)", startCount, endCount)
	}
}

// TestExecOutputZeroAuditRows: exec_output 纯进程内读零审计行 (spec §4/§5)
// ——真任务 (start/end 行已由 Start/引擎落笔) 反复轮询后审计行数不变。
func TestExecOutputZeroAuditRows(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	defer m.CloseAll()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "out:" + cmd + "\n", "", 0 },
	})
	defer cleanup()

	id, _ := startBg(t, m, st, addr, hk, "poll", 60)
	waitTerminal(t, m, id, 5*time.Second)

	for i := 0; i < 3; i++ {
		if _, err := ExecOutputForProfile(context.Background(), st, "proj-bg", id, 0, 0, 0, "text", m); err != nil {
			t.Fatal(err)
		}
	}
	rows, rerr := st.AuditRows(10)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2 (start+end only; 三次 exec_output 零审计行): %+v", len(rows), rows)
	}
}

// TestExecOutputStopToolRegistered: MCP 层接线——exec_output/exec_stop 已注册
// (BrokerTools[7]/[8]); 输出过 SDK jsonschema 校验 (恒定字段全序列化); 负值/
// 非法 encoding 经 handler 校验以 IsError 工具错误回 (schema 层表达不了);
// unknown task_id 同为 IsError 三因文案。
func TestExecOutputStopToolRegistered(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "out:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	server, mgr, tasks, err := NewServer(st, pid, "proj-test")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()
	defer tasks.CloseAll()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := server.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srvSess.Close()
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cliSess.Close()
	ctx := context.Background()

	// 起任务, 轮询 exec_output 至终态 (wait=2 长轮询真实走一轮)。
	res, err := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": srvID, "command": "smoke"},
	})
	if err != nil || res.IsError {
		t.Fatalf("exec_background: err=%v res=%+v", err, res.Content)
	}
	// 精确 task_id: 经 JSON 反序列化 (MCP 输出文本即结构化 JSON)。
	var startOut struct {
		TaskID string `json:"task_id"`
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &startOut)
		}
	}
	if startOut.TaskID == "" {
		t.Fatalf("cannot parse task_id from %+v", res.Content)
	}

	done := false
	for i := 0; i < 5 && !done; i++ {
		res2, cerr := cliSess.CallTool(ctx, &mcp.CallToolParams{
			Name: "exec_output",
			Arguments: map[string]any{
				"task_id":      startOut.TaskID,
				"wait_seconds": 2,
			},
		})
		if cerr != nil {
			t.Fatal(cerr)
		}
		if res2.IsError {
			t.Fatalf("exec_output should succeed: %+v", res2.Content)
		}
		done = textContains(res2, `"status":"done"`)
	}
	if !done {
		t.Fatal("task did not reach done within 5 polls")
	}

	// base64 续读: offset 0 全量 = base64("out:smoke\n"), 游标字节口径。
	wantB64 := base64.StdEncoding.EncodeToString([]byte("out:smoke\n"))
	res3, cerr := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name: "exec_output",
		Arguments: map[string]any{
			"task_id": startOut.TaskID, "encoding": "base64",
		},
	})
	if cerr != nil || res3.IsError {
		t.Fatalf("exec_output base64: err=%v res=%+v", cerr, res3.Content)
	}
	if !textContains(res3, `"stdout":"`+wantB64+`"`) {
		t.Fatalf("base64 stdout mismatch: %+v", res3.Content)
	}
	if !textContains(res3, `"next_stdout_offset":10`) || !textContains(res3, `"lost_stderr_bytes":0`) {
		t.Fatalf("constant-field schema check failed: %+v", res3.Content)
	}

	// handler 级拒绝以 IsError 工具错误回 (schema 层表达不了 minimum/enum)。
	res4, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_output",
		Arguments: map[string]any{"task_id": startOut.TaskID, "wait_seconds": -1},
	})
	if !res4.IsError || !textContains(res4, "wait_seconds must be >= 0") {
		t.Fatalf("negative wait must be IsError: %+v", res4.Content)
	}
	res5, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_output",
		Arguments: map[string]any{"task_id": startOut.TaskID, "encoding": "gbk"},
	})
	if !res5.IsError || !textContains(res5, `encoding must be "text" or "base64"`) {
		t.Fatalf("bad encoding must be IsError: %+v", res5.Content)
	}

	// exec_stop: 已终态幂等回 done; unknown → IsError 三因文案。
	res6, serr := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_stop",
		Arguments: map[string]any{"task_id": startOut.TaskID},
	})
	if serr != nil || res6.IsError || !textContains(res6, `"status":"done"`) {
		t.Fatalf("exec_stop idempotent on terminal: err=%v res=%+v", serr, res6.Content)
	}
	res7, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_stop",
		Arguments: map[string]any{"task_id": "bogus"},
	})
	if !res7.IsError || !textContains(res7, "unknown task_id") {
		t.Fatalf("exec_stop unknown must be IsError with three-cause text: %+v", res7.Content)
	}
	res8, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_output",
		Arguments: map[string]any{"task_id": "bogus"},
	})
	if !res8.IsError || !textContains(res8, "unknown task_id") {
		t.Fatalf("exec_output unknown must be IsError with three-cause text: %+v", res8.Content)
	}
}
