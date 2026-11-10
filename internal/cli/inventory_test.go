package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// runInventoryRoot 从根命令执行一条软件清单命令，返回标准输出、标准错误
// 与错误。
func runInventoryRoot(t *testing.T, stdin string, args ...string) (string, string, error) {
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

// mustInventoryCli 执行必须成功的软件清单命令。
func mustInventoryCli(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, errOut, err := runInventoryRoot(t, stdin, args...)
	if err != nil {
		t.Fatalf("cli %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
	}
	return out
}

// inventoryDeclarationID 从新增命令输出中读取声明标识符。
func inventoryDeclarationID(t *testing.T, output string) string {
	t.Helper()
	fields := strings.Fields(output)
	if len(fields) == 0 {
		t.Fatalf("新增声明输出里没有标识符：%q", output)
	}
	return fields[len(fields)-1]
}

// TestInventoryDeclarationCLILifecycle 覆盖声明的命令行新增、查看、搜索、
// 修改、删除确认，以及查看与搜索的 JSON Lines 输出。
func TestInventoryDeclarationCLILifecycle(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "网络存储")
	mustInventoryCli(t, "", "devices", "add", "工作站")

	addOutput := mustInventoryCli(t, "", "inventory", "add", "网络存储",
		"--category", "service_program", "--name", "OpenSSH",
		"--version", "1:9.6p1-3ubuntu1",
		"--purl", "pkg:deb/ubuntu/openssh-server@1:9.6p1-3ubuntu1",
		"--cpe", "cpe:2.3:a:openbsd:openssh:9.6p1:-:*:*:*:*:*:*",
		"--listen", "0.0.0.0,22,tcp,public_network",
		"--belongs-to", "系统服务", "--note", "管理员登录入口")
	id := inventoryDeclarationID(t, addOutput)
	mustInventoryCli(t, "", "inventory", "add", "工作站",
		"--category", "system_package", "--name", "libc6", "--version", "2.39-0ubuntu8")

	view := mustInventoryCli(t, "", "inventory", "view", "网络存储")
	for _, want := range []string{
		"OpenSSH", "1:9.6p1-3ubuntu1", "0.0.0.0:22/tcp", "公网",
		"pkg:deb/ubuntu/openssh-server@1:9.6p1-3ubuntu1",
		"cpe:2.3:a:openbsd:openssh:9.6p1:-:*:*:*:*:*:*", "声明",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("按设备查看缺少 %q：\n%s", want, view)
		}
	}
	workstationView := mustInventoryCli(t, "", "inventory", "view", "工作站")
	if !strings.Contains(workstationView, "系统包：1 条") || strings.Contains(workstationView, "libc6") {
		t.Fatalf("系统包默认只显示数量：\n%s", workstationView)
	}
	workstationView = mustInventoryCli(t, "", "inventory", "view", "工作站", "--all-system-packages")
	if !strings.Contains(workstationView, "libc6") {
		t.Fatalf("加参数后应列出系统包：\n%s", workstationView)
	}

	jsonView := mustInventoryCli(t, "", "inventory", "view", "网络存储", "--json")
	var group map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonView)), &group); err != nil {
		t.Fatalf("按设备查看 --json 应输出一行 JSON：%v\n%s", err, jsonView)
	}
	if group["device_name"] != "网络存储" || group["category"] != "service_program" {
		t.Fatalf("按设备查看 JSON 行 = %+v", group)
	}
	items, _ := group["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("按设备查看 JSON 条目数量 = %d：\n%s", len(items), jsonView)
	}

	search := mustInventoryCli(t, "", "inventory", "search", "OPENSSH")
	if !strings.Contains(search, "网络存储") || !strings.Contains(search, "OpenSSH") {
		t.Fatalf("跨设备搜索结果应包含设备与软件名：\n%s", search)
	}
	jsonSearch := mustInventoryCli(t, "", "inventory", "search", "OPENSSH", "--json")
	var hit map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonSearch)), &hit); err != nil {
		t.Fatalf("跨设备搜索 --json 应输出一行 JSON：%v\n%s", err, jsonSearch)
	}
	if hit["device_name"] != "网络存储" {
		t.Fatalf("跨设备搜索 JSON 行 = %+v", hit)
	}

	mustInventoryCli(t, "", "inventory", "edit", id,
		"--version", "1:9.8p1-3ubuntu1",
		"--listen", "127.0.0.1,22,tcp,local_machine")
	view = mustInventoryCli(t, "", "inventory", "view", "网络存储")
	if !strings.Contains(view, "1:9.8p1-3ubuntu1") || !strings.Contains(view, "127.0.0.1:22/tcp") ||
		!strings.Contains(view, "仅本机") {
		t.Fatalf("修改声明失败：\n%s", view)
	}

	oldInteractive := inventoryStdinIsInteractive
	inventoryStdinIsInteractive = func() bool { return false }
	t.Cleanup(func() { inventoryStdinIsInteractive = oldInteractive })
	if _, _, err := runInventoryRoot(t, "", "inventory", "rm", id); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("非交互终端删除声明必须要求 --yes，实际：%v", err)
	}
	inventoryStdinIsInteractive = func() bool { return true }
	mustInventoryCli(t, "y\n", "inventory", "rm", id)
	view = mustInventoryCli(t, "", "inventory", "view", "网络存储")
	if strings.Contains(view, "OpenSSH") {
		t.Fatalf("声明删除后不应继续显示：\n%s", view)
	}
}

// TestInventoryClientWritesRefused 覆盖客户端上的软件清单写命令：它们必须
// 报错并提示到权威端操作。
func TestInventoryClientWritesRefused(t *testing.T) {
	seedClientCacheFixture(t)
	commands := [][]string{
		{"inventory", "add", "网络存储", "--category", "service_program", "--name", "nginx", "--version", "1.0"},
		{"inventory", "edit", "declaration-id", "--version", "1.1"},
		{"inventory", "rm", "declaration-id", "--yes"},
		{"inventory", "claim", "declaration-id", "--observed-key", "observed-key"},
		{"inventory", "unclaim", "declaration-id"},
	}
	for _, args := range commands {
		_, _, err := runInventoryRoot(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "权威端") {
			t.Fatalf("客户端命令 %v 必须提示到权威端，实际：%v", args, err)
		}
	}
}

// TestInventoryAcceptsAllExposureLevels 钉住声明可用的六档暴露级别。
func TestInventoryAcceptsAllExposureLevels(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "网络存储")
	addOutput := mustInventoryCli(t, "", "inventory", "add", "网络存储",
		"--category", "service_program", "--name", "nginx", "--version", "1.25.3",
		"--listen", "127.0.0.1,10001,tcp,local_machine",
		"--listen", "192.0.2.10,10002,tcp,specific_address",
		"--listen", "0.0.0.0,10003,tcp,all_addresses",
		"--listen", "192.168.1.10,10004,tcp,local_area_network",
		"--listen", "10.0.0.10,10005,tcp,virtual_local_area_network",
		"--listen", "0.0.0.0,10006,tcp,public_network")
	id := inventoryDeclarationID(t, addOutput)
	view := mustInventoryCli(t, "", "inventory", "view", "网络存储")
	for _, label := range []string{"仅本机", "监听特定地址", "监听所有地址", "局域网", "虚拟局域网", "公网"} {
		if !strings.Contains(view, label) {
			t.Fatalf("六档暴露级别缺少 %q：\n%s", label, view)
		}
	}
	mustInventoryCli(t, "", "inventory", "rm", id, "--yes")
}

// TestInventoryAuditSourceFromCLI 钉住命令行来源写入软件清单审计。
func TestInventoryAuditSourceFromCLI(t *testing.T) {
	dbPath, mk := newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "网络存储")
	addOutput := mustInventoryCli(t, "", "inventory", "add", "网络存储",
		"--category", "operating_system", "--name", "Synology DSM", "--version", "7.2.2-72806")
	id := inventoryDeclarationID(t, addOutput)
	mustInventoryCli(t, "", "inventory", "edit", id, "--note", "家庭存储")
	mustInventoryCli(t, "", "inventory", "rm", id, "--yes")

	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{
		"software-declaration-add", "software-declaration-edit", "software-declaration-rm",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("审计行数量 = %d，应为 3：%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceCLI || row.SourceIP != "" || row.Status != "ok" {
			t.Fatalf("命令行审计来源不正确：%+v", row)
		}
	}
}

// TestInventoryEncryptedBackupRoundTrip 走真实加密备份命令：备份文件导入新
// 主键的新库后，软件声明与监听端口仍然可以查询。
func TestInventoryEncryptedBackupRoundTrip(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "网络存储")
	mustInventoryCli(t, "", "inventory", "add", "网络存储",
		"--category", "service_program", "--name", "nginx", "--version", "1.25.3",
		"--listen", "0.0.0.0,443,tcp,public_network")

	backupDir := t.TempDir()
	touchMarker(t, backupDir)
	passFile := filepath.Join(t.TempDir(), "backup-passphrase.txt")
	if err := os.WriteFile(passFile, []byte("backup-passphrase-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCreate(t, backupDir, "--passphrase-file", passFile); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(backupDir, "vault-*.sme"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("加密备份文件 = %v（错误 %v），应恰好一个", matches, err)
	}

	targetDir := t.TempDir()
	targetKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(targetDir, "vault.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(targetKey),
	})
	mustInventoryCli(t, "", "import", matches[0], "--passphrase-file", passFile)
	view := mustInventoryCli(t, "", "inventory", "view", "网络存储")
	if !strings.Contains(view, "nginx 1.25.3") || !strings.Contains(view, "0.0.0.0:443/tcp") {
		t.Fatalf("加密备份恢复后的软件清单不完整：\n%s", view)
	}
}
