package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedClientAuditLog 按追加顺序（旧记录在前）写入一个实例的本机审计日志文件。
func seedClientAuditLog(t *testing.T, directory string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cache-audit.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCacheAuditReadsNamedInstanceNewestFirst(t *testing.T) {
	configRoot := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CONFIG_ROOT": configRoot,
		"SSHMGR_CACHE_DIR":   "",
		"SSHMGR_CACHE_DEK":   "",
	})
	seedClientAuditLog(t, filepath.Join(configRoot, "ssh-manager", "instances", "laptop"),
		`{"action":"exec","command":"uptime","duration_ms":12,"exit_code":3,"id":0,"project_id":"proj_1","server_id":"srv_1","source":"agent","status":"error","sudo":true,"ts":1800000000}`,
		`not-json`,
		`{"action":"cache.pull","command":"同步成功","duration_ms":0,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"web_ui","source_ip":"127.0.0.1","status":"ok","sudo":false,"ts":1800000100}`,
	)

	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"audit", "--instance", "laptop"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cache audit 应读取实例旁的审计日志文件：%v", err)
	}
	got := out.String()
	newest := strings.Index(got, "cache.pull")
	oldest := strings.Index(got, "exec")
	if newest < 0 || oldest < 0 || newest > oldest {
		t.Fatalf("输出应最新记录在前且包含全部可解析记录：\n%s", got)
	}
	for _, want := range []string{"uptime", "同步成功", "proj_1", "srv_1", "127.0.0.1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, got)
		}
	}
}

func TestCacheAuditLimitAndJSON(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR": cacheDir,
		"SSHMGR_CACHE_DEK": "",
	})
	seedClientAuditLog(t, cacheDir,
		`{"action":"cache.pull","command":"同步成功","duration_ms":0,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"web_ui","source_ip":"127.0.0.1","status":"ok","sudo":false,"ts":1800000100}`,
		`{"action":"exec","command":"date","duration_ms":5,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"agent","status":"ok","sudo":false,"ts":1800000200}`,
	)

	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"audit", "--limit", "1", "--json"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cache audit 应支持条数限制与 JSON 输出：%v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatalf("JSON 输出应是单行 JSON 对象：%v，输出=%q", err, out.String())
	}
	if row["action"] != "exec" || row["command"] != "date" {
		t.Fatalf("条数限制应保留最新记录：%v", row)
	}
}

func TestCacheAuditMissingFileAndBadFlags(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR": cacheDir,
		"SSHMGR_CACHE_DEK": "",
	})

	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"audit"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("审计日志文件不存在时命令不应失败：%v", err)
	}
	if !strings.Contains(out.String(), "no matching audit rows") {
		t.Fatalf("没有记录时应输出空结果提示：\n%s", out.String())
	}

	var errOut bytes.Buffer
	badLimit := newCacheCmd()
	badLimit.SetArgs([]string{"audit", "--limit", "-1"})
	badLimit.SetOut(&errOut)
	badLimit.SetErr(&errOut)
	if err := badLimit.Execute(); err == nil {
		t.Fatal("负数条数限制应返回错误")
	}
}
