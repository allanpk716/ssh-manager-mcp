package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// redirectCacheEnv 把测试从操作者真实 vault/cache 隔离出来：APPDATA/XDG 指向
// 新 temp 目录（形态同 cache_status_instances_test.go），三个 cache 环境变量清空，
// 返回 userDir（ssh-manager 根的父目录）。
func redirectCacheEnv(t *testing.T) string {
	t.Helper()
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
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
