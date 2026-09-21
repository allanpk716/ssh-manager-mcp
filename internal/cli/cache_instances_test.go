package cli

// Plan 46 T2 — `cache instances ls/rm` 的 CLI 面测试:视图五列/半态/孤儿标注、
// 确认屏与配套双提示、默认槽拒绝、路径安全矩阵、幂等重跑。(RemoveInstance
// 本体的双根清理、partial 注入、进程内互斥在 internal/clientops 的
// removeinstance_test.go —— 互斥锁与注入 seam 都在那个包里。)

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// instancesTestEnv redirects both roots (UserConfigDir + SSHMGR_CACHE_DEK_DIR)
// and clears the single-slot envs. NO DekProvider swap: rm exercises the real
// FileKeyProvider delete path.
func instancesTestEnv(t *testing.T) (userDir, dekDir string) {
	t.Helper()
	userDir = t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	dekDir = t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":     "",
		"SSHMGR_CACHE_DEK":     "",
		"SSHMGR_CACHE_DEK_DIR": dekDir,
	})
	return userDir, dekDir
}

// seedSlot writes a slot dir with the named artifact subset; the DEK file is
// written for real so rm's double-root cleanup is observable.
func seedSlot(t *testing.T, userDir, dekDir, name string, artifacts ...string) {
	t.Helper()
	dir := filepath.Join(userDir, "ssh-manager", "instances", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range artifacts {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if name != "" {
		if err := os.WriteFile(filepath.Join(dekDir, "cache-dek-"+name+".key"), []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The confirm flow needs the stdin + output wiring per invocation, so tests
// drive a fresh root each time via runInstancesRm. extra carries CLI flags
// verbatim (e.g. "--yes") appended after the instance argument.
func runInstancesRm(t *testing.T, tty bool, stdin, arg string, extra ...string) (string, error) {
	t.Helper()
	prev := cacheInstancesStdinIsTTY
	cacheInstancesStdinIsTTY = func() bool { return tty }
	t.Cleanup(func() { cacheInstancesStdinIsTTY = prev })
	root := NewRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"cache", "instances", "rm", arg}, extra...))
	err := root.Execute()
	return out.String(), err
}

func runInstancesLs(t *testing.T) string {
	t.Helper()
	root := NewRootCmd()
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"cache", "instances", "ls"})
	if err := root.Execute(); err != nil {
		t.Fatalf("cache instances ls: %v", err)
	}
	return out.String()
}

func TestCacheInstancesLs_View(t *testing.T) {
	userDir, dekDir := instancesTestEnv(t)
	seedSlot(t, userDir, dekDir, "agentA", "cache.auth.json", "cache.bin", "cache.meta.json", "cache.config.json")
	seedSlot(t, userDir, dekDir, "agentB", "cache.bin") // 半态:缺 auth/meta/config
	// 孤儿 DEK:槽目录不存在。
	if err := os.WriteFile(filepath.Join(dekDir, "cache-dek-ghost.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 默认槽材料(直接在根目录)。
	for _, f := range []string{"cache.auth.json", "cache.bin", "cache.meta.json"} {
		if err := os.WriteFile(filepath.Join(userDir, "ssh-manager", f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := runInstancesLs(t)
	for _, want := range []string{
		"instance: (默认实例)",                   // 默认槽一行
		"instance: agentA",                   // 命名槽
		"auth=有 bin=有 meta=有 config=有 dek=有", // agentA 完整
		"⚠ 半态槽(缺 auth·meta·config)",          // agentB 显式标注
		"⚠ DEK 孤儿",                           // ghost 显式标注
		"instance: ghost",                    // 孤儿独立成行(名为 ghost)
		"age=",                               // cache 年龄列
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ls output missing %q:\n%s", want, got)
		}
	}
	// rm 提示恰好三处(agentA/agentB/ghost),默认槽行无 rm 提示。
	if n := strings.Count(got, "cache instances rm"); n != 3 {
		t.Fatalf("rm hint count = %d, want 3 (named rows + orphan only):\n%s", n, got)
	}
	defLine := got[strings.Index(got, "instance: (默认实例)"):]
	defLine = defLine[:strings.Index(defLine, "\n")]
	if strings.Contains(defLine, "cache instances rm") {
		t.Fatalf("default slot line must carry no rm hint: %q", defLine)
	}
}

// 真空默认行:空机器的默认槽连目录都无——合法 vacuum 态,列值照实全"缺"
// 但不准挂 ⚠ 半态标注(那只为"目录在而材料缺"的事故形态保留;判据与
// T3 picker 的 slotStat.dir 一致,真 stat 目录而非解析成功即算在)。
func TestCacheInstancesLs_VacuumDefaultNoHalfState(t *testing.T) {
	instancesTestEnv(t) // 不播任何种子——真空机器
	got := runInstancesLs(t)
	defLine := got[strings.Index(got, "instance: (默认实例)"):]
	defLine = defLine[:strings.Index(defLine, "\n")]
	if strings.Contains(defLine, "⚠") {
		t.Fatalf("directory-less default slot is the legal vacuum state — no ⚠ half-state annotation allowed: %q", defLine)
	}
	if !strings.Contains(defLine, "auth=缺") {
		t.Fatalf("vacuum row must still render its columns truthfully: %q", defLine)
	}
}

func TestCacheInstancesRm_ConfirmAndCompanionHints(t *testing.T) {
	userDir, dekDir := instancesTestEnv(t)
	seedSlot(t, userDir, dekDir, "agentA", "cache.auth.json", "cache.bin", "cache.meta.json", "cache.config.json")

	out, err := runInstancesRm(t, true, "agentA\n", "agentA")
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	for _, want := range []string{
		"输入实例名确认",
		"已删除实例 \"agentA\"",
		"sshmgr cache-tokens revoke agentA", // ① broker 侧吊销
		"--write-mcp",                       // ② 槽外副本泛化提示
		"不随 rm 清理",
		"cache.config.json 仅存 max_offline", // ②的原因明说
		"重启对应进程后生效",                        // 在用提醒
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rm output missing %q:\n%s", want, out)
		}
	}
	if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances", "agentA")); !os.IsNotExist(serr) {
		t.Fatalf("slot dir must be gone: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-agentA.key")); !os.IsNotExist(serr) {
		t.Fatalf("DEK must be gone: %v", serr)
	}
	// 幂等重跑:已空的实例再 rm 仍成功。
	out2, err := runInstancesRm(t, true, "agentA\n", "agentA")
	if err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if !strings.Contains(out2, "已删除实例") {
		t.Fatalf("re-run output:\n%s", out2)
	}
}

func TestCacheInstancesRm_ConfirmRefusalKeepsEverything(t *testing.T) {
	userDir, dekDir := instancesTestEnv(t)
	seedSlot(t, userDir, dekDir, "agentA", "cache.bin")

	out, err := runInstancesRm(t, true, "wrong\n", "agentA")
	if err != nil {
		t.Fatalf("wrong confirmation must cancel, not fail: %v", err)
	}
	if !strings.Contains(out, "已取消,未做任何改动。") {
		t.Fatalf("cancellation text missing:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances", "agentA")); serr != nil {
		t.Fatalf("slot must survive the cancellation: %v", serr)
	}
}

func TestCacheInstancesRm_NonTTYRefused(t *testing.T) {
	userDir, dekDir := instancesTestEnv(t)
	seedSlot(t, userDir, dekDir, "agentA", "cache.bin")

	_, err := runInstancesRm(t, false, "agentA\n", "agentA")
	// 文案钉全文:--yes 加入后此路径的拒绝文案必须一字不变。
	if err == nil || err.Error() != "cache instances rm 需要交互式终端(stdin 不是 TTY)——为防止脚本误删,拒绝执行" {
		t.Fatalf("non-TTY rm must be refused with the unchanged exact message: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances", "agentA")); serr != nil {
		t.Fatalf("slot must survive the refusal: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-agentA.key")); serr != nil {
		t.Fatalf("DEK must survive the refusal: %v", serr)
	}
}

// 非 TTY + --yes:确认点 assume-yes 直接执行,双根清理与 ls 复核消失——
// agent 无人值守清理的唯一放行通道。
func TestCacheInstancesRm_NonTTYYesRemovesBothRoots(t *testing.T) {
	userDir, dekDir := instancesTestEnv(t)
	seedSlot(t, userDir, dekDir, "agentA", "cache.auth.json", "cache.bin", "cache.meta.json", "cache.config.json")

	out, err := runInstancesRm(t, false, "", "agentA", "--yes")
	if err != nil {
		t.Fatalf("non-TTY rm --yes must delete: %v", err)
	}
	if strings.Contains(out, "输入实例名确认") {
		t.Fatalf("--yes must skip the typed-name confirm:\n%s", out)
	}
	if !strings.Contains(out, "已删除实例 \"agentA\"") {
		t.Fatalf("rm --yes output missing success text:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances", "agentA")); !os.IsNotExist(serr) {
		t.Fatalf("slot dir must be gone: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-agentA.key")); !os.IsNotExist(serr) {
		t.Fatalf("DEK must be gone: %v", serr)
	}
	// ls 复核:实例从列表消失(孤儿行也不许出现)。
	ls := runInstancesLs(t)
	if strings.Contains(ls, "instance: agentA") {
		t.Fatalf("ls must no longer list the removed instance:\n%s", ls)
	}
}

// TTY + --yes:跳过输名确认直接删。stdin 故意给空——若确认未被 --yes
// 短路,空输入必然落到"已取消"分支、槽存活;删除成功本身就是短路的行为证据。
func TestCacheInstancesRm_TTYYesSkipsTypedConfirm(t *testing.T) {
	userDir, dekDir := instancesTestEnv(t)
	seedSlot(t, userDir, dekDir, "agentA", "cache.bin")

	out, err := runInstancesRm(t, true, "", "agentA", "--yes")
	if err != nil {
		t.Fatalf("TTY rm --yes must delete without typed confirm: %v", err)
	}
	if strings.Contains(out, "输入实例名确认") {
		t.Fatalf("--yes must skip the typed-name confirm on TTY too:\n%s", out)
	}
	if strings.Contains(out, "已取消") {
		t.Fatalf("empty stdin must not reach the confirm branch under --yes:\n%s", out)
	}
	if !strings.Contains(out, "已删除实例 \"agentA\"") {
		t.Fatalf("output missing success text:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances", "agentA")); !os.IsNotExist(serr) {
		t.Fatalf("slot dir must be gone: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-agentA.key")); !os.IsNotExist(serr) {
		t.Fatalf("DEK must be gone: %v", serr)
	}
}

func TestCacheInstancesRm_DefaultSlotRefused(t *testing.T) {
	instancesTestEnv(t)
	_, err := runInstancesRm(t, false, "", "")
	if err == nil || !strings.Contains(err.Error(), "sshmgr clear") {
		t.Fatalf("default-slot rm must point at clear: %v", err)
	}
}

// TestCacheInstancesRm_PathSafetyMatrix:traversal/分隔符/保留名/绝对路径全拒,
// 且拒绝发生在任何删除之前(escape 目标的金丝雀必须原样)。
func TestCacheInstancesRm_PathSafetyMatrix(t *testing.T) {
	userDir, _ := instancesTestEnv(t)
	evil := filepath.Join(userDir, "ssh-manager", "evil")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(evil, "canary.txt")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../evil", "..", ".", "a/b", `a\b`, "CON", "nul.txt", "COM1", `C:\x`, "/abs", "a:b"} {
		if _, err := runInstancesRm(t, false, "", bad); err == nil {
			t.Fatalf("rm %q must be refused", bad)
		}
	}
	if _, serr := os.Stat(canary); serr != nil {
		t.Fatalf("escape target must survive every refusal: %v", serr)
	}
}

func TestCacheInstancesLs_SingleSlotEnvRefused(t *testing.T) {
	instancesTestEnv(t)
	t.Setenv("SSHMGR_CACHE_DIR", filepath.Join(t.TempDir(), "override"))
	root := NewRootCmd()
	root.SetArgs([]string{"cache", "instances", "ls"})
	if err := root.Execute(); err == nil {
		t.Fatal("ls with SSHMGR_CACHE_DIR set must be refused (the instance view would lie)")
	}
}
