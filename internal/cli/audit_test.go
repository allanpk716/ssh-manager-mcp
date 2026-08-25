package cli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// nastyCmd carries the adversarial payload reused across seeds and assertions.
const nastyCmd = "echo \"<hi>&\x1b]0;pwn\x7f"

// seedAuditStore: SSHMGR_STORE 指向新 vault——profile "dev"、project "alpha"、
// server "gpu",六行审计行覆盖过滤维度(含已删实体原串、owner 行、恶劣 command)。
func seedAuditStore(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	db := dir + "/audit.db"
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{"SSHMGR_STORE": db, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	st, err := store.Open(db, mk)
	if err != nil {
		t.Fatal(err)
	}
	profID, err := st.AddProfile("dev")
	if err != nil {
		t.Fatal(err)
	}
	pid, _, err := st.AddProject("alpha", profID)
	if err != nil {
		t.Fatal(err)
	}
	srvID, err := st.AddServerWithCredentials(
		&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u"},
		&models.Credential{Type: models.CredPassword, Secret: []byte("x")},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	fresh := time.Now()
	for _, r := range []store.AuditRow{
		{TS: old, ProjectID: pid, ServerID: srvID, Action: "exec", Command: "uptime", Status: "ok", ExitCode: 0, DurationMS: 4},
		{TS: old, ProjectID: pid, ServerID: srvID, Action: "exec", Command: "whoami", Status: "error", ExitCode: 2, DurationMS: 6, Sudo: true},
		{TS: old, ProjectID: pid, ServerID: "srv-gone", Action: "forward", Command: "127.0.0.1:5432 id=t1", Status: "ok", DurationMS: 100},
		{TS: old, ProjectID: "", ServerID: "", Action: "project.revoke", Status: "ok"},
		{TS: old, ProjectID: "proj-gone", ServerID: srvID, Action: "download", Command: "/etc/hostname", Status: "ok", DurationMS: 40},
		{TS: fresh, ProjectID: pid, ServerID: srvID, Action: "exec", Command: nastyCmd, Status: "ok", DurationMS: 1},
	} {
		if err := st.WriteAudit(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return mk
}

func runAudit(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	full := append([]string{"audit"}, args...)
	root := NewRootCmd()
	root.SetArgs(full)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

func auditLines(t *testing.T, out *bytes.Buffer) []string {
	t.Helper()
	var ls []string
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if l != "" && !strings.HasPrefix(l, "unlimited query:") && !strings.HasPrefix(l, "showing first") {
			ls = append(ls, l)
		}
	}
	return ls
}

// TestAudit_FiltersAndDefaults: 默认全量 newest-first + name 解析 + 组合 +
// --owner + 原串直配 + --since 三种绝对字面量/相对形态/坏值。
func TestAudit_FiltersAndDefaults(t *testing.T) {
	seedAuditStore(t)

	out, err := runAudit(t)
	if err != nil {
		t.Fatal(err)
	}
	ls := auditLines(t, out)
	if len(ls) != 6 {
		t.Fatalf("default (limit 100) must show all 6, got %d: %q", len(ls), out.String())
	}
	if !strings.Contains(ls[0], `echo "<hi>&\x1b]0;pwn\x7f`) { // newest first + escaping (quote is NOT in the six-row escape table — brief's `\"` needle was a one-char bug)
		t.Fatalf("newest row first with escaping: %q", ls[0])
	}
	if strings.ContainsRune(ls[0], 0x1b) || strings.ContainsRune(ls[0], 0x7f) {
		t.Fatalf("raw control bytes leaked: %q", ls[0])
	}

	out, err = runAudit(t, "--project", "alpha")
	if err != nil || len(auditLines(t, out)) != 4 { // brief said 3 but four seeded rows carry pid (uptime/whoami/forward/nasty)
		t.Fatalf("--project name: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--project", "alpha", "--status", "error")
	if err != nil || len(auditLines(t, out)) != 1 || !strings.Contains(out.String(), "whoami") {
		t.Fatalf("--project+--status: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--server", "gpu")
	if err != nil || len(auditLines(t, out)) != 4 {
		t.Fatalf("--server name: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--server", "srv-gone")
	if err != nil || len(auditLines(t, out)) != 1 {
		t.Fatalf("--server raw id (zero existence check): %v %q", err, out.String())
	}
	out, err = runAudit(t, "--server", "gpu,srv-gone") // n>1: 逗号拆分(cobra StringSlice CSV)→ 多值 SQL IN
	if err != nil || len(auditLines(t, out)) != 5 {    // gpu 4 + srv-gone 1
		t.Fatalf("--server comma-split multi-value IN: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--owner")
	if err != nil || len(auditLines(t, out)) != 1 || !strings.Contains(out.String(), "(owner)") || !strings.Contains(out.String(), "(none)") {
		t.Fatalf("--owner render: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--project", "proj-gone")
	if err != nil || !strings.Contains(out.String(), "proj-gon…(deleted)") {
		t.Fatalf("deleted project render: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--action", "nosuchaction")
	if err != nil || !strings.Contains(out.String(), "no matching audit rows") {
		t.Fatalf("unknown action must be silent-empty: %v %q", err, out.String())
	}

	// --since: 相对(单数+单位)/复合拒/绝对三字面量/坏值文案。
	out, err = runAudit(t, "--since", "1h")
	if err != nil || len(auditLines(t, out)) != 1 {
		t.Fatalf("--since 1h: %v %q", err, out.String())
	}
	if _, err = runAudit(t, "--since", "1.5h"); err != nil {
		t.Fatalf("--since 1.5h must parse: %v", err)
	}
	if _, err = runAudit(t, "--since", "1h30m"); err == nil || !strings.Contains(err.Error(), "invalid --since") {
		t.Fatalf("--since 1h30m must be rejected: %v", err)
	}
	if _, err = runAudit(t, "--since", "2026-08-20T09:00:00+08:00"); err != nil {
		t.Fatalf("RFC3339 must parse: %v", err)
	}
	if _, err = runAudit(t, "--since", "2026-08-20T09:00:00"); err != nil {
		t.Fatalf("local offset-less datetime must parse: %v", err)
	}
	if _, err = runAudit(t, "--since", "2026-08-20"); err != nil {
		t.Fatalf("plain date must parse: %v", err)
	}
	_, err = runAudit(t, "--since", "nonsense")
	if err == nil || !strings.Contains(err.Error(), "invalid --since") || !strings.Contains(err.Error(), "30m") || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("bad --since error must list the forms: %v", err)
	}
	// RFC3339 实际过滤行为(过滤断言走 UTC 形态,避开本地时区分支)。
	out, err = runAudit(t, "--since", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
	if err != nil || len(auditLines(t, out)) != 1 {
		t.Fatalf("--since RFC3339 filter: %v %q", err, out.String())
	}

	// 互斥。
	if _, err = runAudit(t, "--owner", "--project", "alpha"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("--owner + --project must error: %v", err)
	}
}

// TestAudit_EscapingAndRender: 时间戳带偏移、转义集(spec §3 全行:反斜杠可逆/
// 控制字符 \xNN/C1+行分隔+bidi+不可见格式化 \uXXXX/中文原文)、动态字段统一转义。
// 注意:测试源码一律用显式 \u/\x 转义构造输入,绝不裸写不可见字符。
func TestAudit_EscapingAndRender(t *testing.T) {
	seedAuditStore(t)

	out, err := runAudit(t)
	if err != nil {
		t.Fatal(err)
	}
	first := auditLines(t, out)[0]
	// 时间戳 = 本地时区带偏移(自描述)。
	tsRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2}  `)
	if !tsRe.MatchString(first) {
		t.Fatalf("timestamp must carry local offset: %q", first)
	}
	// 恶劣 command:ESC/DEL 转义后可见、原样字节不出现。
	if !strings.Contains(first, `\x1b`) || !strings.Contains(first, `\x7f`) {
		t.Fatalf("control bytes must be escaped: %q", first)
	}
	if strings.ContainsRune(first, 0x1b) || strings.ContainsRune(first, 0x7f) {
		t.Fatalf("raw control bytes leaked: %q", first)
	}
	// 单元级:转义函数直测(每类取代表字符,全部显式转义构造)。
	in := "a\\b\nc\td\re\x1f\x7f" +
		string([]rune{0x9b, 0x2028, 0x2029, 0x200e, 0x202a, 0x2066, 0x00ad, 0x200b, 0x2060, 0xfeff}) +
		"中"
	want := `a\\b\nc\td\re\x1f\x7f\u009b\u2028\u2029\u200e\u202a\u2066\u00ad\u200b\u2060\ufeff中`
	if got := escapeAuditText(in); got != want {
		t.Fatalf("escapeAuditText:\n got %q\nwant %q", got, want)
	}
}

// TestAudit_JSON: JSONL 可解析、九键恒在、owner 行零值非缺失、--json 无 stderr 提示。
func TestAudit_JSON(t *testing.T) {
	seedAuditStore(t)

	out, err := runAudit(t, "--json", "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "unlimited query") || strings.Contains(out.String(), "showing first") {
		t.Fatalf("--json must stay silent on stderr hints: %q", out.String())
	}
	ls := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(ls) != 6 {
		t.Fatalf("--json --limit 0 must emit 6 lines, got %d", len(ls))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(ls[0]), &m); err != nil {
		t.Fatalf("json line: %v", err)
	}
	if len(m) != 9 {
		t.Fatalf("exactly nine keys, got %d: %v", len(m), m)
	}
	// nasty command:JSON round-trip + json.Marshal 转义可见(< 转为 \u003c、ESC 转为 \u001b)
	if m["command"] != nastyCmd {
		t.Fatalf("json round-trip of nasty command: %v", m["command"])
	}
	if !strings.Contains(ls[0], `\u003c`) || strings.Contains(ls[0], "\x1b") {
		t.Fatalf("json line must show json.Marshal escaping: %s", ls[0])
	}
	out, err = runAudit(t, "--json", "--owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["project_id"]; !ok || v != "" { // present-as-empty, not absent
		t.Fatalf("owner row project_id must be present-as-empty: %v", m)
	}
}

// TestAudit_LimitSemantics: 负值 CLI 闸、0 警告(人读)、截断提示与恰好不提示。
func TestAudit_LimitSemantics(t *testing.T) {
	seedAuditStore(t)

	if _, err := runAudit(t, "--limit", "-1"); err == nil || !strings.Contains(err.Error(), "--limit must be >= 0") {
		t.Fatalf("--limit -1: %v", err)
	}
	out, err := runAudit(t, "--limit", "0")
	if err != nil || !strings.Contains(out.String(), "unlimited query:") {
		t.Fatalf("--limit 0 human warning: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--limit", "2")
	if err != nil || len(auditLines(t, out)) != 2 || !strings.Contains(out.String(), "showing first 2 rows (more exist)") {
		t.Fatalf("truncation hint: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--limit", "6")
	if err != nil || strings.Contains(out.String(), "showing first") {
		t.Fatalf("exactly-limit must not hint: %v %q", err, out.String())
	}
}
