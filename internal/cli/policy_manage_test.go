package cli

// `sshmgr policy` 规则管理命令组(ticket #15)的行为测试:权威端(本机
// vault)上的增、删、停用、恢复、列出与试算联动;客户端(无 vault、有
// 缓存)上修改命令被明确拒绝、列出与试算读缓存快照。审计断言直接查库
// (QueryAudit),不重复测管理层的判定语义。

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// newPolicyVaultEnv 建一个已解锁的权威库环境:SSHMGR_STORE 指向新库、
// 主键经 SSHMGR_MASTERKEY_HEX 注入、缓存目录钉到空临时目录(本机若有
// 真实 vault/缓存不至于串进来)。返回库路径与主键。
func newPolicyVaultEnv(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
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

// runCLI 以给定参数执行根命令,返回输出;err 非空时也返回输出供断言。
func runCLI(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := NewRootCmd()
	root.SetArgs(args)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

// openPolicyStore 用测试主键直接打开权威库断言内部状态(审计行、规则行)。
func openPolicyStore(t *testing.T, dbPath string, mk []byte) *store.Store {
	t.Helper()
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// policyAuditRows 查指定动作的 owner 审计行。
func policyAuditRows(t *testing.T, st *store.Store, action string) []store.AuditRow {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestPolicyAddCustomRule_Structured_ChangesTestAndExecutes:验收用例——
// 增加一条结构化自定义规则后,试算按它判定;审计写 policy-rule.add。
func TestPolicyAddCustomRule_Structured_ChangesTestAndExecutes(t *testing.T) {
	dbPath, mk := newPolicyVaultEnv(t)

	out, err := runCLI(t, "policy", "add",
		"--id", "NO-REBOOT", "--shell", "posix", "--command", "reboot",
		"--disposition", "block",
		"--why", "reboots are scheduled by the owner",
		"--instead", "ask the owner to schedule a reboot window")
	if err != nil {
		t.Fatalf("policy add: %v\n%s", err, out.String())
	}

	// 试算按自定义规则判定:原本放行的 reboot 现在拦下。
	out, err = runCLI(t, "policy", "test", "--shell", "posix", "reboot")
	if err != nil {
		t.Fatalf("policy test: %v", err)
	}
	if !strings.Contains(out.String(), "拦截") || !strings.Contains(out.String(), "NO-REBOOT") {
		t.Fatalf("policy test after add must report 拦截/NO-REBOOT, got:\n%s", out.String())
	}
	// 未命中的命令不受影响。
	out, err = runCLI(t, "policy", "test", "--shell", "posix", "uptime")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "放行") {
		t.Fatalf("uptime must stay allowed, got:\n%s", out.String())
	}

	// 审计:恰一行 policy-rule.add,摘要带规则编号与处置。
	st := openPolicyStore(t, dbPath, mk)
	rows := policyAuditRows(t, st, "policy-rule.add")
	if len(rows) != 1 || !strings.Contains(rows[0].Command, `"id":"NO-REBOOT"`) || rows[0].Status != "ok" {
		t.Fatalf("policy-rule.add rows = %+v", rows)
	}
}

// TestPolicyAddCustomRule_Regex:验收用例——正则高级选项一例,试算按它判定。
func TestPolicyAddCustomRule_Regex(t *testing.T) {
	dbPath, mk := newPolicyVaultEnv(t)

	out, err := runCLI(t, "policy", "add",
		"--id", "NO-FORCE-PUSH", "--regex", `git\s+push\s+.*--force`,
		"--disposition", "block",
		"--why", "force pushes rewrite shared history")
	if err != nil {
		t.Fatalf("policy add: %v\n%s", err, out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "posix", "git push --force origin main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "拦截") || !strings.Contains(out.String(), "NO-FORCE-PUSH") {
		t.Fatalf("policy test must report 拦截/NO-FORCE-PUSH, got:\n%s", out.String())
	}
	st := openPolicyStore(t, dbPath, mk)
	if rows := policyAuditRows(t, st, "policy-rule.add"); len(rows) != 1 {
		t.Fatalf("policy-rule.add rows = %d, want 1", len(rows))
	}
}

// TestPolicyDisableEnableDefaultRule:验收用例——停用一条默认规则后试算不再
// 命中;恢复后重新命中;两步各写审计。
func TestPolicyDisableEnableDefaultRule(t *testing.T) {
	dbPath, mk := newPolicyVaultEnv(t)
	const bomb = ":(){ :|:& };:"

	// 对照:默认规则在停用前命中。
	out, err := runCLI(t, "policy", "test", "--shell", "posix", bomb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "POSIX-FORK-BOMB") {
		t.Fatalf("fork bomb must hit the default rule first, got:\n%s", out.String())
	}

	if out, err = runCLI(t, "policy", "disable", "POSIX-FORK-BOMB"); err != nil {
		t.Fatalf("policy disable: %v\n%s", err, out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "posix", bomb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "放行") {
		t.Fatalf("fork bomb after disable must be allowed, got:\n%s", out.String())
	}

	if out, err = runCLI(t, "policy", "enable", "POSIX-FORK-BOMB"); err != nil {
		t.Fatalf("policy enable: %v\n%s", err, out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "posix", bomb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "拦截") || !strings.Contains(out.String(), "POSIX-FORK-BOMB") {
		t.Fatalf("fork bomb after enable must be blocked again, got:\n%s", out.String())
	}

	st := openPolicyStore(t, dbPath, mk)
	if rows := policyAuditRows(t, st, "policy-rule.disable"); len(rows) != 1 ||
		!strings.Contains(rows[0].Command, "POSIX-FORK-BOMB") {
		t.Fatalf("policy-rule.disable rows = %+v", rows)
	}
	if rows := policyAuditRows(t, st, "policy-rule.enable"); len(rows) != 1 ||
		!strings.Contains(rows[0].Command, "POSIX-FORK-BOMB") {
		t.Fatalf("policy-rule.enable rows = %+v", rows)
	}
}

// TestPolicyRmCustomRule:删除自定义规则后试算回到放行;审计写 policy-rule.rm。
func TestPolicyRmCustomRule(t *testing.T) {
	dbPath, mk := newPolicyVaultEnv(t)
	if out, err := runCLI(t, "policy", "add", "--id", "NO-REBOOT", "--command", "reboot",
		"--disposition", "block", "--why", "w"); err != nil {
		t.Fatalf("policy add: %v\n%s", err, out.String())
	}

	if out, err := runCLI(t, "policy", "rm", "NO-REBOOT"); err != nil {
		t.Fatalf("policy rm: %v\n%s", err, out.String())
	}
	out, err := runCLI(t, "policy", "test", "--shell", "posix", "reboot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "放行") {
		t.Fatalf("reboot after rm must be allowed, got:\n%s", out.String())
	}
	st := openPolicyStore(t, dbPath, mk)
	if rows := policyAuditRows(t, st, "policy-rule.rm"); len(rows) != 1 ||
		!strings.Contains(rows[0].Command, "NO-REBOOT") {
		t.Fatalf("policy-rule.rm rows = %+v", rows)
	}
}

// TestPolicyLs:列出默认规则与自定义规则,停用的默认规则带标注。
func TestPolicyLs(t *testing.T) {
	newPolicyVaultEnv(t)
	if out, err := runCLI(t, "policy", "add", "--id", "NO-REBOOT", "--command", "reboot",
		"--disposition", "block", "--why", "w"); err != nil {
		t.Fatalf("policy add: %v\n%s", err, out.String())
	}
	if out, err := runCLI(t, "policy", "disable", "POSIX-FORK-BOMB"); err != nil {
		t.Fatalf("policy disable: %v\n%s", err, out.String())
	}

	out, err := runCLI(t, "policy", "ls")
	if err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"POSIX-FORMAT", "POSIX-CRITICAL-DELETE", // 默认规则在场
		"NO-REBOOT", // 自定义规则在场
		"已停用",       // 停用标注
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("policy ls output must contain %q, got:\n%s", want, got)
		}
	}
	// 停用标注恰在 POSIX-FORK-BOMB 那一行,不在别的行。
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "POSIX-") {
			continue
		}
		hasMark := strings.Contains(line, "已停用")
		if strings.Contains(line, "POSIX-FORK-BOMB") && !hasMark {
			t.Fatalf("POSIX-FORK-BOMB line must carry the disabled mark: %q", line)
		}
		if !strings.Contains(line, "POSIX-FORK-BOMB") && hasMark {
			t.Fatalf("unexpected disabled mark on line %q", line)
		}
	}
}

// TestPolicyAddValidation:非法输入被拒——未知处置、未知 shell、坏正则、
// 撞默认规则编号、重复编号。
func TestPolicyAddValidation(t *testing.T) {
	newPolicyVaultEnv(t)
	cases := [][]string{
		{"policy", "add", "--id", "X1", "--command", "x", "--disposition", "explode", "--why", "w"},
		{"policy", "add", "--id", "X2", "--shell", "fish", "--command", "x", "--disposition", "block", "--why", "w"},
		{"policy", "add", "--id", "X3", "--regex", "(", "--disposition", "block", "--why", "w"},
		{"policy", "add", "--id", "POSIX-FORMAT", "--command", "x", "--disposition", "block", "--why", "w"},
		{"policy", "add", "--command", "x", "--disposition", "block", "--why", "w"}, // 缺 --id
		{"policy", "disable", "NOT-A-RULE"},
		{"policy", "rm", "NOT-A-RULE"},
	}
	for _, args := range cases {
		if _, err := runCLI(t, args...); err == nil {
			t.Fatalf("args %v must be refused", args)
		}
	}
	// 重复编号。
	if out, err := runCLI(t, "policy", "add", "--id", "DUP", "--command", "x",
		"--disposition", "block", "--why", "w"); err != nil {
		t.Fatalf("policy add: %v\n%s", err, out.String())
	}
	if _, err := runCLI(t, "policy", "add", "--id", "DUP", "--command", "x",
		"--disposition", "block", "--why", "w"); err == nil {
		t.Fatal("duplicate id must be refused")
	}
}

// seedClientCacheFixture 造一台「客户端机器」:无 vault(SSHMGR_STORE 指
// 向不存在的路径),缓存目录里有 cache.bin(带一条自定义规则与一条停用
// 记录的快照)与 cache.auth.json(姿态探测认得它)。返回缓存目录。
func seedClientCacheFixture(t *testing.T) string {
	t.Helper()
	// 权威侧种子库:自定义规则 + 停用记录 + 一台服务器一个 profile(快照
	// 不是空壳)。
	srcDir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	src, err := store.Open(filepath.Join(srcDir, "src.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []policy.ShellType{policy.ShellPOSIX},
		Commands: []string{"reboot"}, Disposition: policy.DispositionBlock,
		Explanation: "reboots are scheduled by the owner, never run by the agent",
		Alternative: "ask the owner to schedule a reboot window",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.DisableDefaultRule("POSIX-FORK-BOMB"); err != nil {
		t.Fatal(err)
	}
	srvID, _ := src.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u"})
	pid, _ := src.AddProfile("team-a")
	_ = src.GrantServers(pid, []string{srvID})
	snap, err := src.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}

	// cache.bin(DEK 加密)+ cache.auth.json(客户端姿态探测的依据)。
	cacheDir := t.TempDir()
	dek, _ := store.GenerateMasterKey()
	blob, err := vaultio.EncryptWithKey(dek, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	authBlob, _ := json.Marshal(clientops.CacheCred{URL: "https://serve.example.internal:7878", Token: "code-xyz"})
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), authBlob, 0o600); err != nil {
		t.Fatal(err)
	}

	// DEK 注入内存钥匙缝,让 LoadCacheSnapshotFor 能解密。
	mem := withDEK(t)
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}

	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "no-vault.db"),
		"SSHMGR_CACHE_DIR":     cacheDir,
		"SSHMGR_MASTERKEY_HEX": "",                                               // 不许真实主键从环境串进来
		"SSHMGR_FILEKEY_PATH":  filepath.Join(t.TempDir(), "no-such-master.key"), // 也不从默认固定路径拿到开发者本机的真实 master.key
	})
	return cacheDir
}

// TestPolicyClient_RefusesMutations:验收用例——客户端上的修改命令被明确
// 拒绝(带指引),列出与试算可用且按缓存快照里的规则判定。
func TestPolicyClient_RefusesMutations(t *testing.T) {
	seedClientCacheFixture(t)

	// 四个修改命令都被拒,错误里说清「客户端只读、规则在权威端管理」。
	mutations := [][]string{
		{"policy", "add", "--id", "X", "--command", "x", "--disposition", "block", "--why", "w"},
		{"policy", "rm", "NO-REBOOT"},
		{"policy", "disable", "POSIX-FORMAT"},
		{"policy", "enable", "POSIX-FORK-BOMB"},
	}
	for _, args := range mutations {
		_, err := runCLI(t, args...)
		if err == nil {
			t.Fatalf("client mutation %v must be refused", args)
		}
		if !strings.Contains(err.Error(), "authoritative") && !strings.Contains(err.Error(), "cache pull") {
			t.Fatalf("refusal must point at the authoritative machine / cache pull, got %v", err)
		}
	}

	// 列出:读缓存快照——自定义规则在场,停用的默认规则带标注。
	out, err := runCLI(t, "policy", "ls")
	if err != nil {
		t.Fatalf("client policy ls: %v", err)
	}
	if !strings.Contains(out.String(), "NO-REBOOT") || !strings.Contains(out.String(), "已停用") {
		t.Fatalf("client ls must reflect the pulled snapshot, got:\n%s", out.String())
	}

	// 试算:按缓存快照里的规则判定——自定义规则拦下,停用的默认规则放行。
	out, err = runCLI(t, "policy", "test", "--shell", "posix", "reboot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NO-REBOOT") {
		t.Fatalf("client policy test must judge by the snapshot rules, got:\n%s", out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "posix", ":(){ :|:& };:")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "放行") {
		t.Fatalf("fork bomb disabled in the snapshot must be allowed on the client, got:\n%s", out.String())
	}
}

// TestPolicyVaultAndSingleMachineSamePath:单机部署(无 serve)与权威端走
// 同一条 vault 代码路径——本文件全部权威端用例即单机形态,这里补一条
// 显式断言:本地库加规则、本地试算生效。
func TestPolicyVaultAndSingleMachineSamePath(t *testing.T) {
	newPolicyVaultEnv(t)
	if out, err := runCLI(t, "policy", "add", "--id", "NO-REBOOT", "--command", "reboot",
		"--disposition", "block", "--why", "w"); err != nil {
		t.Fatalf("policy add: %v\n%s", err, out.String())
	}
	out, err := runCLI(t, "policy", "test", "--shell", "posix", "sudo reboot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "NO-REBOOT") {
		t.Fatalf("single-machine policy test must judge by the local vault, got:\n%s", out.String())
	}
}

// TestPolicyWindowsRules_ListAndDisable:合并 #12 后,Windows 默认规则
// (WIN- 前缀)进列出面,停用/恢复按编号同样可用。
func TestPolicyWindowsRules_ListAndDisable(t *testing.T) {
	newPolicyVaultEnv(t)
	out, err := runCLI(t, "policy", "ls")
	if err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	if !strings.Contains(out.String(), "WIN-FORMAT") || !strings.Contains(out.String(), "WIN-LOCKOUT") {
		t.Fatalf("policy ls must list the windows default rules too, got:\n%s", out.String())
	}
	// 停用 WIN-FORMAT 后,Windows 判定里它不再命中;恢复后重新命中。
	if out, err = runCLI(t, "policy", "disable", "WIN-FORMAT"); err != nil {
		t.Fatalf("policy disable WIN-FORMAT: %v\n%s", err, out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "powershell", "Format-Volume -DriveLetter D")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "放行") {
		t.Fatalf("disabled WIN-FORMAT must stop matching, got:\n%s", out.String())
	}
	if out, err = runCLI(t, "policy", "enable", "WIN-FORMAT"); err != nil {
		t.Fatalf("policy enable WIN-FORMAT: %v\n%s", err, out.String())
	}
	out, err = runCLI(t, "policy", "test", "--shell", "powershell", "Format-Volume -DriveLetter D")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "拦截") || !strings.Contains(out.String(), "WIN-FORMAT") {
		t.Fatalf("re-enabled WIN-FORMAT must match again, got:\n%s", out.String())
	}
}
