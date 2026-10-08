package mcpserver

// reload_later_e2e_test.go — 后续各代换手端到端(票 05,进程边界真实):
//
//   - 三代链:伪宿主 → 首代桥(化为泵)→ 二代(被领养)→ 二代再换三代
//     (GenerationLater,继任直接继承泵侧管道句柄)→ 二代退出;泵全程存活
//     透传,宿主侧帧序无损;降级信号(gen 更晚、version 更低)同样触发。
//   - 二代继任服务中途被杀 → 泵退出,伪宿主观察到连接终止(同桥崩溃现状)。
//
// 测试名刻意含 "Generation" 以落进票面验证命令 -run 'HotSwap|Gen'。

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/updater"
)

// waitForStateProbe 轮询等待角色状态探针文件出现(roleBridgeMain 写,见
// reload_e2e_test.go)。
func waitForStateProbe(t *testing.T, dir, kind, version string, d time.Duration) {
	t.Helper()
	path := stateProbePath(dir, kind, version)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("state probe %q did not appear within %v", path, d)
}

// readStateProbePid 读出角色探针 pid 文件里的进程号(等文件出现后解析)。
func readStateProbePid(t *testing.T, dir, version string) int {
	t.Helper()
	path := stateProbePath(dir, "pid", version)
	waitForStateProbe(t, dir, "pid", version, 15*time.Second)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid probe %q not parseable: %v", raw, err)
	}
	return pid
}

// assertFrameIntegrity 宿主侧帧序无损的收尾断言:每一行都是合法 JSON,且
// 每个已发请求 id 恰好有一个应答。
func assertFrameIntegrity(t *testing.T, h *fakeHost, wantIDs []int64) {
	t.Helper()
	notifs := 0
	for _, w := range h.frames() {
		if !json.Valid([]byte(w.raw)) {
			t.Fatalf("recorded frame is not valid JSON (mid-line corruption?): %q", w.raw)
		}
		if w.method == "notifications/tools/list_changed" {
			notifs++
		}
	}
	t.Logf("frame integrity: %d frames, %d tools/list_changed notifications", len(h.frames()), notifs)
	for _, id := range wantIDs {
		count := 0
		for _, w := range h.frames() {
			if w.resp && w.id != nil && *w.id == id {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("request id %d got %d responses, want exactly 1", id, count)
		}
	}
}

// TestGenerationChainE2EDirect:三代链全链。链上版本自报(靠 SSHMGR_TEST_
// SUCCESSOR_ENV 逐代覆写):v1-gen → v2-gen → v3-gen;盘上信号两跳分别是
// 升级(v2-disk)与降级(v0.1-downgrade-disk:gen 更晚但版本串更低,照样
// 触发换手)。
func TestGenerationChainE2EDirect(t *testing.T) {
	storePath, masterHex, token := buildTestVault(t)
	stateDir := t.TempDir()
	h := spawnBridgeRole(t, roleBridgeDirect, map[string]string{
		"SSHMGR_STORE":          storePath,
		"SSHMGR_MASTERKEY_HEX":  masterHex,
		"SSHMGR_TEST_TOKEN":     token,
		"SSHMGR_TEST_STATE_DIR": stateDir,
		"SSHMGR_TEST_VERSION":   "v1-gen",
		// 首代换手的继任环境:二代自报 v2-gen;二代自己的继任环境再把三代
		// 覆写成 v3-gen(同键后值覆盖前值,经 BuildEnv 的环境合并规则)。
		"SSHMGR_TEST_SUCCESSOR_ENV": "SSHMGR_TEST_VERSION=v2-gen;SSHMGR_TEST_SUCCESSOR_ENV=SSHMGR_TEST_VERSION=v3-gen",
	})
	h.handshake(t)

	if names := h.listToolNames(t, 2); len(names) != len(authorityTools)+1 {
		t.Fatalf("gen1 face = %d tools, want %d", len(names), len(authorityTools)+1)
	}

	// 第一跳:首代泵化。盘上写 v2-disk(升级)。
	exe := testBinaryPath(t)
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })
	if _, err := updater.WriteGenerationSignal(exe, "v2-disk"); err != nil {
		t.Fatal(err)
	}
	out1 := h.callReloadSelf(t, 3, 30*time.Second)
	if out1.Handover != "started" || out1.Version != "v1-gen" || out1.SuccessorVersion != "v2-gen" {
		t.Fatalf("hop1 reload_self = %+v (stderr:\n%s)", out1, h.stderrFn())
	}
	h.awaitNotificationCount(t, "notifications/tools/list_changed", 1, 20*time.Second)
	waitForStateProbe(t, stateDir, "pid", "v2-gen", 15*time.Second)

	// 第二跳:二代换三代。盘上写降级信号(gen 更晚、版本串更低)——照样
	// 触发;应答由二代先写回(伪宿主此刻读到它),随后二代退出。
	if _, err := updater.WriteGenerationSignal(exe, "v0.1-downgrade-disk"); err != nil {
		t.Fatal(err)
	}
	out2 := h.callReloadSelf(t, 4, 30*time.Second)
	if out2.Version != "v2-gen" {
		t.Fatalf("hop2 reload_self must be answered by gen2, version = %q (stderr:\n%s)", out2.Version, h.stderrFn())
	}
	if out2.Handover != "started" {
		t.Fatalf("downgrade signal must trigger handover: %+v", out2)
	}
	if out2.DiskVersion != "v0.1-downgrade-disk" || out2.DiskGeneration <= 0 {
		t.Fatalf("downgrade disk fields: %+v", out2)
	}
	if out2.SuccessorVersion != "v3-gen" {
		t.Fatalf("hop2 successor_version = %q want v3-gen", out2.SuccessorVersion)
	}
	// 二代在应答写回之后退出(验收:应答先写回再退出)。
	waitForStateProbe(t, stateDir, "exit", "v2-gen", 15*time.Second)
	// 三代接管同一会话:它的 tools/list_changed 到达,自报 v3-gen 应答。
	h.awaitNotificationCount(t, "notifications/tools/list_changed", 2, 20*time.Second)
	out3 := h.callReloadSelf(t, 5, 30*time.Second)
	if out3.Version != "v3-gen" {
		t.Fatalf("gen3 must answer with v3-gen, got %+v (stderr:\n%s)", out3, h.stderrFn())
	}
	if out3.Handover != "no_new_generation" {
		t.Fatalf("gen3 handover = %q want no_new_generation (no newer signal)", out3.Handover)
	}

	// 泵(首代所化)全程存活透传:此刻所有交互仍经它;三代工具面完整,
	// 再打几个请求证帧序无损。
	if names := h.listToolNames(t, 6); len(names) != len(authorityTools)+1 {
		t.Fatalf("gen3 face = %d tools, want %d", len(names), len(authorityTools)+1)
	}
	for id := int64(7); id <= 9; id++ {
		if names := h.listToolNames(t, id); len(names) != len(authorityTools)+1 {
			t.Fatalf("tools/list id %d incomplete", id)
		}
	}
	assertFrameIntegrity(t, h, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9})

	// 宿主断开:三代读标准输入 EOF 自行退出,泵收干退出,首代进程收尾。
	h.stdinW.Close()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("pump (gen1) did not exit after host disconnect; stderr:\n%s", h.stderrFn())
	}
	waitForStateProbe(t, stateDir, "exit", "v3-gen", 15*time.Second)
	waitForStateProbe(t, stateDir, "exit", "v1-gen", 15*time.Second)
}

// TestGenerationSuccessorDeathEndsPump:二代服务中途被杀 → 泵经泵侧管道
// EOF 退出,伪宿主观察到连接终止(桥侧标准输出断开 + 首代进程退出),
// 行为与今天的桥崩溃一致。
func TestGenerationSuccessorDeathEndsPump(t *testing.T) {
	storePath, masterHex, token := buildTestVault(t)
	stateDir := t.TempDir()
	h := spawnBridgeRole(t, roleBridgeDirect, map[string]string{
		"SSHMGR_STORE":              storePath,
		"SSHMGR_MASTERKEY_HEX":      masterHex,
		"SSHMGR_TEST_TOKEN":         token,
		"SSHMGR_TEST_STATE_DIR":     stateDir,
		"SSHMGR_TEST_VERSION":       "v1-kill",
		"SSHMGR_TEST_SUCCESSOR_ENV": "SSHMGR_TEST_VERSION=v2-kill",
	})
	h.handshake(t)

	exe := testBinaryPath(t)
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })
	if _, err := updater.WriteGenerationSignal(exe, "v2-kill-disk"); err != nil {
		t.Fatal(err)
	}
	out := h.callReloadSelf(t, 3, 30*time.Second)
	if out.Handover != "started" || out.SuccessorVersion != "v2-kill" {
		t.Fatalf("handover = %+v (stderr:\n%s)", out, h.stderrFn())
	}
	h.awaitNotificationCount(t, "notifications/tools/list_changed", 1, 20*time.Second)

	// 二代确实在服务(它自报 v2-kill 应答 reload_self)。
	out2 := h.callReloadSelf(t, 4, 30*time.Second)
	if out2.Version != "v2-kill" {
		t.Fatalf("gen2 must be serving, got %+v", out2)
	}

	// 服务中途杀死二代继任:泵退出,宿主侧表现为掉线。
	pid := readStateProbePid(t, stateDir, "v2-kill")
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill gen2 (pid %d): %v", pid, err)
	}

	select {
	case <-h.stdoutEOF:
	case <-time.After(20 * time.Second):
		t.Fatalf("pseudo host did not see the connection end after killing gen2; stderr:\n%s", h.stderrFn())
	}
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done: // 泵(首代)随泵侧 EOF 退出,整个链路终止
	case <-time.After(20 * time.Second):
		t.Fatalf("pump (gen1) did not exit after gen2 died; stderr:\n%s", h.stderrFn())
	}
}
