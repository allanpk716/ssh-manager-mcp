package cli

// `sshmgr secrets` 命令组（票 #59）的行为测试。全部用例都从根命令进入，
// 只观察命令的退出状态、标准输出、标准错误、审计行与再导出的快照段；
// 测试值原文只允许出现在测试代码与快照段比对里，绝不允许出现在任何命令
// 输出、报错或审计摘要中。

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// testSecretValueA / testSecretValueB 是两组互不相同的测试机密项值。它们
// 只进测试代码、临时文件与快照段比对，断言输出里找不到它们。
const (
	testSecretValueA = "test-secret-value-alpha"
	testSecretValueB = "test-secret-value-beta\n"
)

// newSecretsEnv 建一个已解锁的权威库环境：SSHMGR_STORE 指向新库，主键经
// SSHMGR_MASTERKEY_HEX 注入，缓存目录钉到空临时目录，避免读到开发机真实
// 的 vault 或客户端缓存。
func newSecretsEnv(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	dbPath := filepath.Join(dir, "store.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
		"SSHMGR_CACHE_DIR":     t.TempDir(),
	})
	return dbPath, mk
}

// runSecretsRoot 从根命令执行一次机密项命令，返回标准输出、标准错误与错误。
// stdin 是喂给根命令的标准输入（空串表示不给输入）。
func runSecretsRoot(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

// mustSecretsCli 执行必须成功的机密项命令。
func mustSecretsCli(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, errOut, err := runSecretsRoot(t, stdin, args...)
	if err != nil {
		t.Fatalf("cli %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
	}
	return out
}

// secretsSnapshot 重开库再导出一次全量快照，供测试比对当前值、上一个值与
// 值版本号；命令行界面承诺不显示这些字段。
func secretsSnapshot(t *testing.T, dbPath string, mk []byte) *store.Snapshot {
	t.Helper()
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatalf("export snapshot: %v", err)
	}
	return snap
}

// secretSnapshotRow 从快照里取指定名字的机密项行；没有时报错终止。
func secretSnapshotRow(t *testing.T, snap *store.Snapshot, name string) *store.SnapshotSecret {
	t.Helper()
	for i := range snap.Secrets {
		if snap.Secrets[i].Name == name {
			return &snap.Secrets[i]
		}
	}
	t.Fatalf("snapshot has no secret %q: %+v", name, snap.Secrets)
	return nil
}

// writeSecretValueFile 把 value 原样写进临时文件并返回路径。
func writeSecretValueFile(t *testing.T, name string, value []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, value, 0o600); err != nil {
		t.Fatalf("write value file: %v", err)
	}
	return p
}

// TestSecretsLifecycle 覆盖新增、换值、改备注、删除与列表：列表字段齐全且
// 不含值；换值只改值与值的最后更改时间，备注与可写入服务器列表保持不变。
func TestSecretsLifecycle(t *testing.T) {
	dbPath, mk := newSecretsEnv(t)

	mustSecretsCli(t, testSecretValueA, "secrets", "add", "DB_PASSWORD", "--note", "数据库口令", "--stdin")
	// 换值前先确认可写入服务器列表不受换值影响：这里先留空，授权行为在
	// allow/disallow 用例里单独覆盖。
	mustSecretsCli(t, testSecretValueB, "secrets", "set", "DB_PASSWORD", "--stdin")

	out := mustSecretsCli(t, "", "secrets", "list")
	for _, want := range []string{"DB_PASSWORD", "数据库口令", "可写入服务器", "创建时间", "值最后更改"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output must contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, testSecretValueA) || strings.Contains(out, testSecretValueB) {
		t.Fatalf("list output must never contain the value:\n%s", out)
	}

	snap := secretsSnapshot(t, dbPath, mk)
	row := secretSnapshotRow(t, snap, "DB_PASSWORD")
	if string(row.Value) != testSecretValueB {
		t.Fatalf("current value = %q, want %q", row.Value, testSecretValueB)
	}
	if string(row.PreviousValue) != testSecretValueA {
		t.Fatalf("previous value = %q, want %q", row.PreviousValue, testSecretValueA)
	}
	if row.ValueVersion != 2 {
		t.Fatalf("value version = %d, want 2", row.ValueVersion)
	}
	if row.Note != "数据库口令" {
		t.Fatalf("note = %q, want 数据库口令", row.Note)
	}

	mustSecretsCli(t, "", "secrets", "edit", "DB_PASSWORD", "--note", "生产数据库口令")
	out = mustSecretsCli(t, "", "secrets", "list")
	if !strings.Contains(out, "备注: 生产数据库口令") || strings.Contains(out, "备注: 数据库口令\n") {
		t.Fatalf("edit must replace the note, got:\n%s", out)
	}

	mustSecretsCli(t, "", "secrets", "rm", "DB_PASSWORD")
	out = mustSecretsCli(t, "", "secrets", "list")
	if strings.Contains(out, "DB_PASSWORD") {
		t.Fatalf("rm must remove the secret from list, got:\n%s", out)
	}
}

// TestSecretsValueInputModes 覆盖三种录入方式与两条拒绝路径。文件与标准输入
// 都按字节原样保存（末尾换行保留），通过再导出的快照段比对；交互输入两次
// 不一致时报错；非交互终端且没给来源时报错；两个来源同时给出时报错。
func TestSecretsValueInputModes(t *testing.T) {
	dbPath, mk := newSecretsEnv(t)

	fileValue := []byte("from-file-value\n\n")
	valueFile := writeSecretValueFile(t, "value.bin", fileValue)
	mustSecretsCli(t, "", "secrets", "add", "FILE_VALUE", "--from-file", valueFile)
	if got := secretSnapshotRow(t, secretsSnapshot(t, dbPath, mk), "FILE_VALUE").Value; string(got) != string(fileValue) {
		t.Fatalf("file value must keep every byte including trailing newlines, got %q want %q", got, fileValue)
	}

	stdinValue := []byte("stdin-value\r\n")
	mustSecretsCli(t, string(stdinValue), "secrets", "add", "STDIN_VALUE", "--stdin")
	if got := secretSnapshotRow(t, secretsSnapshot(t, dbPath, mk), "STDIN_VALUE").Value; string(got) != string(stdinValue) {
		t.Fatalf("stdin value must keep every byte including the trailing newline, got %q want %q", got, stdinValue)
	}

	// 交互输入：两次一致时保存第一行（不含换行符）。
	oldTTY, oldRead := secretsStdinIsTTY, secretsReadHiddenLine
	secretsStdinIsTTY = func() bool { return true }
	inputs := [][]byte{[]byte("interactive-value"), []byte("interactive-value")}
	secretsReadHiddenLine = func() ([]byte, error) {
		v := inputs[0]
		inputs = inputs[1:]
		return v, nil
	}
	t.Cleanup(func() {
		secretsStdinIsTTY = oldTTY
		secretsReadHiddenLine = oldRead
	})
	mustSecretsCli(t, "", "secrets", "add", "INTERACTIVE_VALUE")
	if got := secretSnapshotRow(t, secretsSnapshot(t, dbPath, mk), "INTERACTIVE_VALUE").Value; string(got) != "interactive-value" {
		t.Fatalf("interactive value = %q, want one line without the newline", got)
	}

	// 两次不一致：报错且不登记。
	inputs = [][]byte{[]byte("interactive-value"), []byte("different-value!")}
	_, _, err := runSecretsRoot(t, "", "secrets", "add", "MISMATCH_VALUE")
	if err == nil || !strings.Contains(err.Error(), "两次输入的机密项值不一致") {
		t.Fatalf("mismatched interactive input must be refused, got %v", err)
	}

	// 非交互终端且没有来源：明确报错。
	secretsStdinIsTTY = func() bool { return false }
	_, _, err = runSecretsRoot(t, "", "secrets", "add", "NO_SOURCE_VALUE")
	if err == nil || !strings.Contains(err.Error(), "--from-file") || !strings.Contains(err.Error(), "--stdin") {
		t.Fatalf("non-interactive input without a source must point at --from-file/--stdin, got %v", err)
	}

	// 两个来源同时给出：直接拒绝。
	_, _, err = runSecretsRoot(t, "", "secrets", "add", "BOTH_SOURCES", "--from-file", valueFile, "--stdin")
	if err == nil || !strings.Contains(err.Error(), "不能同时使用") {
		t.Fatalf("--from-file with --stdin must be refused, got %v", err)
	}
	out := mustSecretsCli(t, "", "secrets", "list")
	for _, absent := range []string{"MISMATCH_VALUE", "NO_SOURCE_VALUE", "BOTH_SOURCES"} {
		if strings.Contains(out, absent) {
			t.Fatalf("refused input must not register a secret, %s present:\n%s", absent, out)
		}
	}
}

// TestSecretsValueLimits 覆盖空值、7 字节、8 字节、65536 字节与 65537 字节。
func TestSecretsValueLimits(t *testing.T) {
	dbPath, mk := newSecretsEnv(t)
	valueFile := func(t *testing.T, n int, fill byte) string {
		t.Helper()
		return writeSecretValueFile(t, "limit.bin", bytes.Repeat([]byte{fill}, n))
	}

	for _, tc := range []struct {
		n        int
		wantErr  string
		wantName string
	}{
		{n: 0, wantErr: "机密项值不能为空"},
		{n: 7, wantErr: "至少需要 8 个字节"},
		{n: 8, wantName: "EIGHT_BYTES"},
		{n: 65536, wantName: "MAX_BYTES"},
		{n: 65537, wantErr: "最多 65536 个字节"},
	} {
		name := tc.wantName
		if name == "" {
			name = "REJECTED"
		}
		_, _, err := runSecretsRoot(t, "", "secrets", "add", name, "--from-file", valueFile(t, tc.n, 'v'))
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("value of %d bytes must fail with %q, got %v", tc.n, tc.wantErr, err)
			}
			if strings.Contains(err.Error(), "当前") {
				t.Fatalf("value rejection must state the limit without disclosing the current byte count, got %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("value of %d bytes must be accepted, got %v", tc.n, err)
		}
	}
	out := mustSecretsCli(t, "", "secrets", "list")
	if strings.Contains(out, "REJECTED") {
		t.Fatalf("rejected values must not register secrets:\n%s", out)
	}
	for _, name := range []string{"EIGHT_BYTES", "MAX_BYTES"} {
		if !strings.Contains(out, name) {
			t.Fatalf("value of legal length must register %s:\n%s", name, out)
		}
	}
	snap := secretsSnapshot(t, dbPath, mk)
	if got := len(secretSnapshotRow(t, snap, "MAX_BYTES").Value); got != 65536 {
		t.Fatalf("65536-byte value length = %d, want 65536", got)
	}
}

// TestSecretsNameRules 覆盖名字只差大小写的冲突、非法字符与目标不存在。
func TestSecretsNameRules(t *testing.T) {
	newSecretsEnv(t)
	mustSecretsCli(t, "initial-value", "secrets", "add", "API_TOKEN", "--stdin")

	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{
			args:    []string{"secrets", "add", "api_token", "--stdin"},
			wantErr: "大小写不同的名字视为同一个",
		},
		{
			args:    []string{"secrets", "add", "9BAD_NAME", "--stdin"},
			wantErr: "机密项名字不合法",
		},
		{
			args:    []string{"secrets", "add", "BAD-NAME", "--stdin"},
			wantErr: "机密项名字不合法",
		},
		{
			args:    []string{"secrets", "add", "口令", "--stdin"},
			wantErr: "机密项名字不合法",
		},
		{
			args:    []string{"secrets", "set", "GHOST_NAME", "--stdin"},
			wantErr: "机密项不存在",
		},
		{
			args:    []string{"secrets", "edit", "GHOST_NAME", "--note", "x"},
			wantErr: "机密项不存在",
		},
		{
			args:    []string{"secrets", "rm", "GHOST_NAME"},
			wantErr: "机密项不存在",
		},
	} {
		_, _, err := runSecretsRoot(t, "replacement-value", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%v must fail with %q, got %v", tc.args, tc.wantErr, err)
		}
	}
}

// seedSecretServers 登记三个服务器条目，供可写入列表与筛选用例使用。
func seedSecretServers(t *testing.T) {
	t.Helper()
	for _, name := range []string{"web1", "web2", "db1"} {
		mustSecretsCli(t, "", "servers", "add", "--name", name, "--host", "192.0.2.10", "--user", "u")
	}
}

// TestSecretsAllowDisallow 覆盖批量授权、取消授权、写错名字时整体不生效且
// 一次列出全部错误、重复执行幂等，以及 --query 与 --server 筛选。
func TestSecretsAllowDisallow(t *testing.T) {
	dbPath, mk := newSecretsEnv(t)
	seedSecretServers(t)
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "API_TOKEN", "--note", "接口令牌", "--stdin")
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "DB_PASSWORD", "--note", "数据库口令", "--stdin")

	mustSecretsCli(t, "", "secrets", "allow", "--secrets", "API_TOKEN,DB_PASSWORD", "--servers", "db1,web1")
	out := mustSecretsCli(t, "", "secrets", "list")
	if !strings.Contains(out, "可写入服务器: db1, web1") {
		t.Fatalf("allow must record both server entries in name order, got:\n%s", out)
	}
	if strings.Index(out, "API_TOKEN") > strings.Index(out, "DB_PASSWORD") {
		t.Fatalf("list must be sorted by name:\n%s", out)
	}

	// 筛选：按服务器条目与按名字/备注文本。
	out = mustSecretsCli(t, "", "secrets", "list", "--server", "web1")
	if strings.Count(out, "API_TOKEN") != 1 || strings.Count(out, "DB_PASSWORD") != 1 {
		t.Fatalf("--server web1 must keep both secrets:\n%s", out)
	}
	out = mustSecretsCli(t, "", "secrets", "list", "--server", "web2")
	if strings.Contains(out, "API_TOKEN") || strings.Contains(out, "DB_PASSWORD") {
		t.Fatalf("--server web2 must filter both secrets out:\n%s", out)
	}
	out = mustSecretsCli(t, "", "secrets", "list", "--query", "db_password")
	if !strings.Contains(out, "DB_PASSWORD") || strings.Contains(out, "API_TOKEN") {
		t.Fatalf("--query must match names case-insensitively:\n%s", out)
	}
	out = mustSecretsCli(t, "", "secrets", "list", "--query", "数据库")
	if !strings.Contains(out, "DB_PASSWORD") || strings.Contains(out, "API_TOKEN") {
		t.Fatalf("--query must also match notes:\n%s", out)
	}

	// 写错名字：机密项与条目混着错时，一次列出全部错误，整体不生效。
	_, _, err := runSecretsRoot(t, "", "secrets", "allow", "--secrets", "GHOST_TOKEN,DB_PASSWORD", "--servers", "web1,no-such-entry")
	if err == nil || !strings.Contains(err.Error(), "GHOST_TOKEN") || !strings.Contains(err.Error(), "no-such-entry") {
		t.Fatalf("invalid names must all be reported at once, got %v", err)
	}
	if snap := secretsSnapshot(t, dbPath, mk); len(snap.SecretAllows) != 4 {
		t.Fatalf("failed allow must change nothing, allow rows = %+v", snap.SecretAllows)
	}

	// 幂等：重复 allow 不改变结果，也不再写新的授权审计行。
	mustSecretsCli(t, "", "secrets", "allow", "--secrets", "API_TOKEN,DB_PASSWORD", "--servers", "db1,web1")
	if snap := secretsSnapshot(t, dbPath, mk); len(snap.SecretAllows) != 4 {
		t.Fatalf("idempotent allow must not add rows, got %+v", snap.SecretAllows)
	}

	mustSecretsCli(t, "", "secrets", "disallow", "--secrets", "API_TOKEN", "--servers", "db1")
	out = mustSecretsCli(t, "", "secrets", "list")
	if !strings.Contains(out, "API_TOKEN") || !strings.Contains(out, "可写入服务器: web1") {
		t.Fatalf("disallow must remove only the named pair, got:\n%s", out)
	}
	if strings.Count(out, "可写入服务器: db1, web1") != 1 {
		t.Fatalf("DB_PASSWORD must keep both entries, got:\n%s", out)
	}
}

// TestSecretsHelpMentionsNewlineAndNoteRisk 钉住帮助文字里的两条提醒：文件与
// 标准输入保留末尾换行；备注对 agent 可见，不要写敏感内容。
func TestSecretsHelpMentionsNewlineAndNoteRisk(t *testing.T) {
	newSecretsEnv(t)
	for _, args := range [][]string{
		{"secrets", "add", "--help"},
		{"secrets", "set", "--help"},
	} {
		out := mustSecretsCli(t, "", args...)
		if !strings.Contains(out, "末尾换行会保留") {
			t.Fatalf("%v help must mention that trailing newlines are kept:\n%s", args, out)
		}
		if !strings.Contains(out, "值没有任何命令参数入口") {
			t.Fatalf("%v help must state that no command argument carries the value:\n%s", args, out)
		}
	}
	out := mustSecretsCli(t, "", "secrets", "add", "--help")
	if !strings.Contains(out, "备注对 agent 可见，不要写敏感内容") {
		t.Fatalf("add help must warn about notes being visible to agents:\n%s", out)
	}
}

// TestSecretsSingleMachineRole 单机部署与权威端走同一条 vault 路径：本机
// role.json 标记为单机时，机密项命令照常可用。
func TestSecretsSingleMachineRole(t *testing.T) {
	newSecretsEnv(t)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatalf("save standalone role: %v", err)
	}
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "API_TOKEN", "--stdin")
	if out := mustSecretsCli(t, "", "secrets", "list"); !strings.Contains(out, "API_TOKEN") {
		t.Fatalf("single-machine list must show the secret:\n%s", out)
	}
}

// TestSecretsImportOldSnapshotKeepsSecretsEmpty 导入没有机密项段的旧快照后，
// 机密项为空且命令照常可用。
func TestSecretsImportOldSnapshotKeepsSecretsEmpty(t *testing.T) {
	dir := t.TempDir()
	old := `{"version":1,"credentials":[],"servers":[],"profiles":[],"grants":[],"projects":[],"host_keys":[],"audit":[]}`
	inFile := filepath.Join(dir, "old-snapshot.json")
	if err := os.WriteFile(inFile, []byte(old), 0o600); err != nil {
		t.Fatalf("write old snapshot: %v", err)
	}
	newSecretsEnv(t)
	mustSecretsCli(t, "", "import", inFile)
	out := mustSecretsCli(t, "", "secrets", "list")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("old snapshot must import zero secrets, got:\n%s", out)
	}
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "API_TOKEN", "--stdin")
}

// TestSecretsAuditRows 钉住每个动作的审计行：动作名、状态与摘要白名单；换值
// 只记「值已更改」，改备注不记备注原文。
func TestSecretsAuditRows(t *testing.T) {
	dbPath, mk := newSecretsEnv(t)
	seedSecretServers(t)
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "API_TOKEN", "--note", "旧备注", "--stdin")
	mustSecretsCli(t, testSecretValueB, "secrets", "set", "API_TOKEN", "--stdin")
	mustSecretsCli(t, "", "secrets", "edit", "API_TOKEN", "--note", "新备注")
	mustSecretsCli(t, "", "secrets", "allow", "--secrets", "API_TOKEN", "--servers", "db1,web1")
	mustSecretsCli(t, "", "secrets", "disallow", "--secrets", "API_TOKEN", "--servers", "db1")
	mustSecretsCli(t, "", "secrets", "rm", "API_TOKEN")

	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{
		"secret-add", "secret-set", "secret-edit", "secret-allow", "secret-disallow", "secret-rm",
	}})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	want := []struct{ action, command string }{
		{"secret-rm", `{"name":"API_TOKEN"}`},
		{"secret-disallow", `{"after":"web1","before":"db1,web1","name":"API_TOKEN"}`},
		{"secret-allow", `{"after":"db1,web1","before":"","name":"API_TOKEN"}`},
		{"secret-edit", `{"change":"备注已更改","name":"API_TOKEN"}`},
		{"secret-set", `{"change":"值已更改","name":"API_TOKEN"}`},
		{"secret-add", `{"name":"API_TOKEN"}`},
	}
	if len(rows) != len(want) {
		t.Fatalf("audit rows = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i].Action != w.action || rows[i].Command != w.command || rows[i].Status != "ok" {
			t.Fatalf("row %d = action %s command %s status %s, want %s %s ok", i, rows[i].Action, rows[i].Command, rows[i].Status, w.action, w.command)
		}
		if rows[i].ProjectID != "" || rows[i].ServerID != "" {
			t.Fatalf("owner row %d must carry no project/server: %+v", i, rows[i])
		}
	}
}

// TestSecretsServerRemoveCascade 覆盖删除服务器条目的联动：条目从所有可写入
// 列表消失，并在同一事务里为每个受影响的机密项写 secret-disallow 审计。
func TestSecretsServerRemoveCascade(t *testing.T) {
	dbPath, mk := newSecretsEnv(t)
	seedSecretServers(t)
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "API_TOKEN", "--stdin")
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "DB_PASSWORD", "--stdin")
	mustSecretsCli(t, "", "secrets", "allow", "--secrets", "API_TOKEN,DB_PASSWORD", "--servers", "db1,web1")

	mustSecretsCli(t, "", "servers", "rm", "db1")
	out := mustSecretsCli(t, "", "secrets", "list")
	if strings.Contains(out, "db1") {
		t.Fatalf("removed entry must disappear from every allow list:\n%s", out)
	}
	if !strings.Contains(out, "web1") {
		t.Fatalf("other entries must survive the cascade:\n%s", out)
	}
	if snap := secretsSnapshot(t, dbPath, mk); len(snap.SecretAllows) != 2 {
		t.Fatalf("snapshot allow rows = %+v, want only the two web1 rows", snap.SecretAllows)
	}

	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"secret-disallow"}})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("server deletion must leave one secret-disallow row per affected secret, got %d rows: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if !strings.Contains(row.Command, "服务器条目已删除") ||
			!strings.Contains(row.Command, `"before":"db1,web1"`) ||
			!strings.Contains(row.Command, `"after":"web1"`) {
			t.Fatalf("cascade audit row = %s", row.Command)
		}
	}
}

// seedSecretVaultForExport 建一个带机密项的权威库：两个机密项，一个换过值
// （因此有上一个值与版本号 2），并把两个服务器条目授权给其中一个。
func seedSecretVaultForExport(t *testing.T) (string, []byte) {
	t.Helper()
	dbPath, mk := newSecretsEnv(t)
	seedSecretServers(t)
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "API_TOKEN", "--note", "接口令牌", "--stdin")
	mustSecretsCli(t, testSecretValueB, "secrets", "set", "API_TOKEN", "--stdin")
	mustSecretsCli(t, testSecretValueA, "secrets", "add", "DB_PASSWORD", "--note", "数据库口令", "--stdin")
	mustSecretsCli(t, "", "secrets", "allow", "--secrets", "API_TOKEN", "--servers", "db1,web1")
	return dbPath, mk
}

// assertSecretSnapshotSegments 比较两个快照的机密项段：字段原样恢复，包括
// 上一个值、值版本号与可写入服务器关联。
func assertSecretSnapshotSegments(t *testing.T, want, got *store.Snapshot) {
	t.Helper()
	if !reflect.DeepEqual(want.Secrets, got.Secrets) {
		t.Fatalf("secret snapshot segments differ:\nwant %+v\ngot  %+v", want.Secrets, got.Secrets)
	}
	if !reflect.DeepEqual(want.SecretAllows, got.SecretAllows) {
		t.Fatalf("secret allow snapshot rows differ:\nwant %+v\ngot  %+v", want.SecretAllows, got.SecretAllows)
	}
}

// TestSecretsExportImportRoundTrip 导出后再导入到另一个主键的新库：命令行
// 列表一致，再导出的机密项段（含上一个值与值版本号）逐字段一致。
func TestSecretsExportImportRoundTrip(t *testing.T) {
	dbA, mkA := seedSecretVaultForExport(t)
	want := secretsSnapshot(t, dbA, mkA)
	before := mustSecretsCli(t, "", "secrets", "list")

	dir := t.TempDir()
	outFile := filepath.Join(dir, "vault.sme")
	passFile := writeSecretValueFile(t, "pass.txt", []byte("export-passphrase-123"))
	mustSecretsCli(t, "", "export", "--out", outFile, "--passphrase-file", passFile)

	mkB, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key B: %v", err)
	}
	dbB := filepath.Join(dir, "store-b.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbB,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mkB),
	})
	mustSecretsCli(t, "", "import", outFile, "--passphrase-file", passFile)

	after := mustSecretsCli(t, "", "secrets", "list")
	if before != after {
		t.Fatalf("secrets list must survive export/import unchanged:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	assertSecretSnapshotSegments(t, want, secretsSnapshot(t, dbB, mkB))
}

// TestSecretsEncryptedBackupRoundTrip 走加密备份路径：backup create 生成的
// 加密备份导入新库后，机密项段与备份前完全一致。
func TestSecretsEncryptedBackupRoundTrip(t *testing.T) {
	dbA, mkA := seedSecretVaultForExport(t)
	want := secretsSnapshot(t, dbA, mkA)

	backupDir := t.TempDir()
	touchMarker(t, backupDir)
	passFile := writeSecretValueFile(t, "backup-pass.txt", []byte("backup-passphrase-123"))
	if _, err := runCreate(t, backupDir, "--passphrase-file", passFile); err != nil {
		t.Fatalf("backup create: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(backupDir, "vault-*.sme"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("encrypted backup products = %v (err %v), want exactly one", matches, err)
	}

	mkB, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key B: %v", err)
	}
	dir := t.TempDir()
	dbB := filepath.Join(dir, "store-b.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbB,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mkB),
	})
	mustSecretsCli(t, "", "import", matches[0], "--passphrase-file", passFile)
	assertSecretSnapshotSegments(t, want, secretsSnapshot(t, dbB, mkB))
}

// TestSecretsClientRefused 在客户端（无 vault、有缓存）上执行全部写操作，每
// 条都被拒绝并提示去权威端。
func TestSecretsClientRefused(t *testing.T) {
	seedClientCacheFixture(t)
	commands := [][]string{
		{"secrets", "add", "API_TOKEN", "--stdin"},
		{"secrets", "set", "API_TOKEN", "--stdin"},
		{"secrets", "edit", "API_TOKEN", "--note", "x"},
		{"secrets", "rm", "API_TOKEN"},
		{"secrets", "allow", "--secrets", "API_TOKEN", "--servers", "web1"},
		{"secrets", "disallow", "--secrets", "API_TOKEN", "--servers", "web1"},
	}
	for _, args := range commands {
		_, _, err := runSecretsRoot(t, testSecretValueA, args...)
		if err == nil {
			t.Fatalf("client command %v must be refused", args)
		}
		if !strings.Contains(err.Error(), "权威端") {
			t.Fatalf("refusal must point at the authoritative machine, got %v", err)
		}
	}
}

// TestSecretsClientRefusedBeforeValueInput 钉住客户端拒绝的位置：角色判定
// 必须先于值录入提示，非交互终端上不能先收到「改用 --from-file/--stdin」。
func TestSecretsClientRefusedBeforeValueInput(t *testing.T) {
	seedClientCacheFixture(t)
	oldTTY := secretsStdinIsTTY
	secretsStdinIsTTY = func() bool { return false }
	t.Cleanup(func() { secretsStdinIsTTY = oldTTY })

	_, _, err := runSecretsRoot(t, "", "secrets", "add", "API_TOKEN")
	if err == nil || !strings.Contains(err.Error(), "权威端") {
		t.Fatalf("client add must be refused with the authoritative-machine hint before reading the value, got %v", err)
	}
	if strings.Contains(err.Error(), "--from-file") || strings.Contains(err.Error(), "--stdin") {
		t.Fatalf("client refusal must not be replaced by the input-source error, got %v", err)
	}
}

// TestSecretsValueNeverVisible 是规格允许的唯一内部观察点：成功与失败的命令
// 输出、报错文本、列表与审计输出里都检索不到值原文。
func TestSecretsValueNeverVisible(t *testing.T) {
	newSecretsEnv(t)
	seedSecretServers(t)
	var visible strings.Builder
	record := func(stdout, stderr string, err error) {
		visible.WriteString(stdout)
		visible.WriteString(stderr)
		if err != nil {
			visible.WriteString(err.Error())
		}
	}

	stdout, stderr, err := runSecretsRoot(t, testSecretValueA, "secrets", "add", "API_TOKEN", "--note", "接口令牌", "--stdin")
	record(stdout, stderr, err)
	stdout, stderr, err = runSecretsRoot(t, testSecretValueB, "secrets", "set", "API_TOKEN", "--stdin")
	record(stdout, stderr, err)
	stdout, stderr, err = runSecretsRoot(t, "short", "secrets", "add", "TOO_SHORT", "--stdin")
	record(stdout, stderr, err)
	stdout, stderr, err = runSecretsRoot(t, testSecretValueA, "secrets", "add", "api_token", "--stdin")
	record(stdout, stderr, err)
	stdout, stderr, err = runSecretsRoot(t, "", "secrets", "allow", "--secrets", "GHOST", "--servers", "no-such-entry")
	record(stdout, stderr, err)
	stdout, stderr, err = runSecretsRoot(t, "", "secrets", "list")
	record(stdout, stderr, err)
	stdout, stderr, err = runSecretsRoot(t, "", "audit", "--owner", "--limit", "0")
	record(stdout, stderr, err)

	got := visible.String()
	for _, secret := range []string{testSecretValueA, strings.TrimSuffix(testSecretValueB, "\n")} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret value leaked into visible output:\n%s", got)
		}
	}
	// 审计里必须能看到机密项动作本身，证明上面检索的不是空文本。
	if !strings.Contains(got, "secret-set") || !strings.Contains(got, "值已更改") {
		t.Fatalf("audit output must show secret actions without values:\n%s", got)
	}
}
