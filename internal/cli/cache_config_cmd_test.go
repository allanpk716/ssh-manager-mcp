package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
)

// redirectCacheEnv 把测试从操作者真实 vault/cache 隔离出来：APPDATA/XDG 指向
// 新 temp 目录（形态同 cache_status_instances_test.go），三个 cache 环境变量清空，
// 返回 userDir（ssh-manager 根的父目录）。
func redirectCacheEnv(t *testing.T) string {
	t.Helper()
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":         "",
		"SSHMGR_CACHE_DEK":         "",
		"SSHMGR_CACHE_MAX_OFFLINE": "",
	})
	return userDir
}

// runCacheConfig 以独立缓冲执行 `cache config ...`，返回合并的 stdout+stderr。
func runCacheConfig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs(append([]string{"config"}, args...))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

// TestCacheConfig_DisplaySources — §8/§11.14 显示三源：off / file / env，
// 各自独立环境（重定向到同一 temp 根的不同阶段），逐段断言。
func TestCacheConfig_DisplaySources(t *testing.T) {
	userDir := redirectCacheEnv(t)
	dir := filepath.Join(userDir, "ssh-manager")

	// ① off：目录在、无 config → "off (no offline limit)"（off 分支无 "(source:" 后缀）
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := runCacheConfig(t)
	if err != nil {
		t.Fatalf("display off must succeed: %v\n%s", err, out)
	}
	for _, want := range []string{"instance: default", "off (no offline limit)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("[off] missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(source:") {
		t.Fatalf("[off] must not render a fake source label:\n%s", out)
	}

	// ② file：写 {"max_offline":"24h"} → "24h (source: file)"
	cfg := filepath.Join(dir, "cache.config.json")
	if err := os.WriteFile(cfg, []byte(`{"max_offline":"24h"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runCacheConfig(t)
	if err != nil {
		t.Fatalf("display file source must succeed: %v\n%s", err, out)
	}
	for _, want := range []string{"24h0m0s (source: file)", dir} {
		if !strings.Contains(out, want) {
			t.Fatalf("[file] missing %q:\n%s", want, out)
		}
	}

	// ③ env：SSHMGR_CACHE_MAX_OFFLINE=48h 在场 → env 压过 file → env 源生效
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "48h")
	out, err = runCacheConfig(t)
	if err != nil {
		t.Fatalf("display env source must succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "48h0m0s (source: env)") {
		t.Fatalf("[env] missing %q:\n%s", "48h0m0s (source: env)", out)
	}
}

// TestCacheConfig_WriteAndReadback — 写入持久化真实文件内容 + 回显 file 源；
// env 在场时写入成功但 stderr 出现 WARNING（persisted 生效需等 env 清除）。
func TestCacheConfig_WriteAndReadback(t *testing.T) {
	userDir := redirectCacheEnv(t)
	instDir := filepath.Join(userDir, "ssh-manager", "instances", "agentA")
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 写入：--instance agentA --max-offline 24h
	out, err := runCacheConfig(t, "--instance", "agentA", "--max-offline", "24h")
	if err != nil {
		t.Fatalf("write must succeed: %v\n%s", err, out)
	}
	wantPath := filepath.Join(instDir, "cache.config.json")
	if !strings.Contains(out, wantPath) || !strings.Contains(out, "agentA") {
		t.Fatalf("write confirmation missing path/instance:\n%s", out)
	}
	blob, rerr := os.ReadFile(wantPath)
	if rerr != nil {
		t.Fatalf("config file must exist after write: %v", rerr)
	}
	var parsed struct {
		MaxOffline string `json:"max_offline"`
	}
	if jerr := json.Unmarshal(blob, &parsed); jerr != nil || parsed.MaxOffline != "24h" {
		t.Fatalf("file content mismatch: %s (unmarshal err=%v)", blob, jerr)
	}

	// 再显：显示 file 源
	out, err = runCacheConfig(t, "--instance", "agentA")
	if err != nil {
		t.Fatalf("readback display must succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "24h0m0s (source: file)") {
		t.Fatalf("readback missing file-source line:\n%s", out)
	}

	// env 在场再写：仍成功（写不受 env 门禁），但必须出 WARNING 到 stderr
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "48h")
	out, err = runCacheConfig(t, "--instance", "agentA", "--max-offline", "24h")
	if err != nil {
		t.Fatalf("write under env must still succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WARNING: SSHMGR_CACHE_MAX_OFFLINE is set") ||
		!strings.Contains(out, "takes effect only after the env is cleared") {
		t.Fatalf("env-present write must emit the effectiveness WARNING:\n%s", out)
	}
}

// TestCacheConfig_MissingInstanceDir — 仅已存在实例目录可写/可显：
// 不存在的 instance 直接报错并给出 enroll 提示文案。
func TestCacheConfig_MissingInstanceDir(t *testing.T) {
	redirectCacheEnv(t)

	out, err := runCacheConfig(t, "--instance", "ghost", "--max-offline", "1h")
	if err == nil {
		t.Fatalf("writing a NON-existent instance must fail:\n%s", out)
	}
	for _, want := range []string{"ghost", "not found", "enroll first (cache pull --instance \"ghost\")"} {
		if !strings.Contains(err.Error(), want) && !strings.Contains(out, want) {
			t.Fatalf("error missing %q: err=%v\n%s", want, err, out)
		}
	}
}

// TestCacheConfig_InstanceEnvMutex — SSHMGR_CACHE_DIR 与 --instance 互斥，
// 由 checkInstanceFlag 统一报错。
func TestCacheConfig_InstanceEnvMutex(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

	out, err := runCacheConfig(t, "--instance", "foo")
	if err == nil {
		t.Fatalf("--instance combined with SSHMGR_CACHE_DIR must fail:\n%s", out)
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected checkInstanceFlag mutex error, got: %v\n%s", err, out)
	}
}

// redirectCacheEnvAllPlatforms 在 redirectCacheEnv 的基础上再隔离 HOME,并清空
// 轮询间隔环境变量:macOS 的 os.UserConfigDir 走 $HOME/Library,只设
// APPDATA/XDG_CONFIG_HOME 挡不住。缓存根目录按平台真实解析(不假设目录形状),
// 保证新测试在 macOS 上也能通过。
func redirectCacheEnvAllPlatforms(t *testing.T) string {
	t.Helper()
	userDir := redirectCacheEnv(t)
	t.Setenv("HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_SNAPSHOT_POLL_INTERVAL": ""})
	dir, _, _, _, err := clientops.CachePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCacheConfig_SnapshotPollInterval_SetReadAndEnvOverride — 快照修订号轮询间隔:
// 轮询间隔默认 5 分钟;`cache config --snapshot-poll-interval` 写入实例级
// cache.config.json;环境变量临时覆盖;与离线上限两个键互不覆盖。
func TestCacheConfig_SnapshotPollInterval_SetReadAndEnvOverride(t *testing.T) {
	dir := redirectCacheEnvAllPlatforms(t)

	// ① 无文件、无环境变量:显示默认值 5 分钟。
	out, err := runCacheConfig(t)
	if err != nil {
		t.Fatalf("显示默认轮询间隔必须成功: %v\n%s", err, out)
	}
	if !strings.Contains(out, "poll:") || !strings.Contains(out, "5m0s (default)") {
		t.Fatalf("默认显示应包含 5 分钟轮询间隔:\n%s", out)
	}

	// ② 写入 3m:文件里出现 snapshot_poll_interval,回显 file 来源。
	out, err = runCacheConfig(t, "--snapshot-poll-interval", "3m")
	if err != nil {
		t.Fatalf("写入轮询间隔必须成功: %v\n%s", err, out)
	}
	cfgPath := filepath.Join(dir, "cache.config.json")
	blob, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatalf("写入后 cache.config.json 必须存在: %v", rerr)
	}
	var parsed struct {
		MaxOffline           string `json:"max_offline"`
		SnapshotPollInterval string `json:"snapshot_poll_interval"`
	}
	if jerr := json.Unmarshal(blob, &parsed); jerr != nil || parsed.SnapshotPollInterval != "3m" {
		t.Fatalf("文件内容应为 snapshot_poll_interval=3m: %s (解码错误=%v)", blob, jerr)
	}
	out, err = runCacheConfig(t)
	if err != nil {
		t.Fatalf("回显轮询间隔必须成功: %v\n%s", err, out)
	}
	if !strings.Contains(out, "3m0s (file)") {
		t.Fatalf("回显应显示 file 来源的 3 分钟:\n%s", out)
	}

	// ③ 再写离线上限:两个键并存,互不覆盖。
	out, err = runCacheConfig(t, "--max-offline", "24h")
	if err != nil {
		t.Fatalf("同时写离线上限必须成功: %v\n%s", err, out)
	}
	blob, rerr = os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if jerr := json.Unmarshal(blob, &parsed); jerr != nil ||
		parsed.MaxOffline != "24h" || parsed.SnapshotPollInterval != "3m" {
		t.Fatalf("两个配置键应并存且互不覆盖: %s (解码错误=%v)", blob, jerr)
	}
	out, err = runCacheConfig(t)
	if err != nil {
		t.Fatalf("双键回显必须成功: %v\n%s", err, out)
	}
	if !strings.Contains(out, "24h0m0s (source: file)") || !strings.Contains(out, "3m0s (file)") {
		t.Fatalf("回显应同时显示离线上限与轮询间隔:\n%s", out)
	}

	// ④ 环境变量覆盖文件:轮询间隔显示 env 来源,离线上限仍走 file 来源。
	t.Setenv("SSHMGR_CACHE_SNAPSHOT_POLL_INTERVAL", "2m")
	out, err = runCacheConfig(t)
	if err != nil {
		t.Fatalf("环境变量覆盖后的显示必须成功: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2m0s (env)") || !strings.Contains(out, "24h0m0s (source: file)") {
		t.Fatalf("环境变量应只覆盖轮询间隔,不影响离线上限:\n%s", out)
	}

	// ⑤ 环境变量在场时写入:仍成功,但给出与离线上限同款警示。
	out, err = runCacheConfig(t, "--snapshot-poll-interval", "3m")
	if err != nil {
		t.Fatalf("环境变量在场时写入必须成功: %v\n%s", err, out)
	}
	if !strings.Contains(out, "SSHMGR_CACHE_SNAPSHOT_POLL_INTERVAL is set") ||
		!strings.Contains(out, "takes effect only after the env is cleared") {
		t.Fatalf("环境变量在场写入必须出警示:\n%s", out)
	}
}

// TestCacheConfig_SnapshotPollInterval_MinimumBound — 下限 1 分钟:过小或无法
// 解析的值被拒绝;恰为 1 分钟可以写入。
func TestCacheConfig_SnapshotPollInterval_MinimumBound(t *testing.T) {
	redirectCacheEnvAllPlatforms(t)

	for _, bad := range []string{"30s", "0s", "-1m", "abc"} {
		out, err := runCacheConfig(t, "--snapshot-poll-interval", bad)
		if err == nil {
			t.Fatalf("过小或非法的轮询间隔 %q 必须被拒绝:\n%s", bad, out)
		}
		if !strings.Contains(err.Error(), ">= 1m") {
			t.Fatalf("错误文案应说明下限 1 分钟,得到: %v", err)
		}
	}
	out, err := runCacheConfig(t, "--snapshot-poll-interval", "1m")
	if err != nil {
		t.Fatalf("恰好 1 分钟应允许写入: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wrote") {
		t.Fatalf("下限值写入应有确认输出:\n%s", out)
	}
}
