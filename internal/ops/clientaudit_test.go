package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// writeClientAuditSidecar 按写入顺序（旧记录在前）写入客户端本机审计日志文
// 件；行内容手写为磁盘上的真实 JSON 格式，避免用被测实现重建期望值。
func writeClientAuditSidecar(t *testing.T, directory string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "cache-audit.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadClientAuditReturnsNewestFirstAndHonorsLimit(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", directory)
	writeClientAuditSidecar(t, directory,
		`{"action":"exec","command":"uptime","duration_ms":12,"exit_code":3,"id":0,"project_id":"proj_1","server_id":"srv_1","source":"agent","status":"error","sudo":true,"ts":1800000000}`,
		`{"action":"cache.pull","command":"同步成功","duration_ms":0,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"web_ui","source_ip":"127.0.0.1","status":"ok","sudo":false,"ts":1800000100}`,
	)

	rows, err := ReadClientAudit("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("不限条数时应返回全部 2 行，实际 %d 行：%+v", len(rows), rows)
	}
	newest := rows[0]
	if newest.Action != "cache.pull" || newest.Command != "同步成功" || newest.Source != store.AuditSourceWebUI ||
		newest.SourceIP != "127.0.0.1" || !newest.TS.Equal(time.Unix(1800000100, 0)) {
		t.Fatalf("最新一行的字段解析不符：%+v", newest)
	}
	oldest := rows[1]
	if oldest.Action != "exec" || oldest.ProjectID != "proj_1" || oldest.ServerID != "srv_1" ||
		oldest.Source != store.AuditSourceAgent || oldest.ExitCode != 3 || oldest.DurationMS != 12 || !oldest.Sudo {
		t.Fatalf("最早一行的字段解析不符：%+v", oldest)
	}

	limited, err := ReadClientAudit("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Action != "cache.pull" {
		t.Fatalf("条数限制应保留最新记录：%+v", limited)
	}
}

func TestReadClientAuditMissingFileReturnsNoRows(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", directory)

	rows, err := ReadClientAudit("", 100)
	if err != nil {
		t.Fatalf("审计日志文件不存在不应报错：%v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("审计日志文件不存在时应返回空结果，实际返回 %d 行", len(rows))
	}
}

func TestReadClientAuditSkipsDamagedLines(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", directory)
	writeClientAuditSidecar(t, directory,
		`{"action":"exec","command":"date","duration_ms":5,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"agent","status":"ok","sudo":false,"ts":1800000200}`,
		`not-json`,
		``,
		`{"action":`,
	)

	rows, err := ReadClientAudit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "exec" || rows[0].Command != "date" {
		t.Fatalf("损坏行应被跳过，只返回可解析记录：%+v", rows)
	}
}

func TestReadClientAuditRejectsInvalidInput(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	if _, err := ReadClientAudit("bad name", 100); err == nil {
		t.Fatal("非法实例名应返回错误")
	}
	if _, err := ReadClientAudit("", -1); err == nil {
		t.Fatal("负数条数限制应返回错误")
	}
}

func TestReadClientAuditResolvesNamedInstanceDirectory(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	directory := filepath.Join(configRoot, "ssh-manager", "instances", "laptop")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClientAuditSidecar(t, directory,
		`{"action":"instance.switch","command":"切换实例","duration_ms":0,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"web_ui","source_ip":"127.0.0.1","status":"ok","sudo":false,"ts":1800000300}`,
	)

	rows, err := ReadClientAudit("laptop", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "instance.switch" {
		t.Fatalf("命名实例的审计日志文件解析不符：%+v", rows)
	}
}
