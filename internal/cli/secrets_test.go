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
	"strings"
	"testing"

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
