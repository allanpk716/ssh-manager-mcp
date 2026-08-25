package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAuditRows(t *testing.T, s *Store, rows ...AuditRow) {
	t.Helper()
	for i, r := range rows {
		if r.TS.IsZero() {
			r.TS = time.Now()
		}
		if err := s.WriteAudit(r); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
}

// TestQueryAudit_FiltersAndOrder: 五维过滤 + OwnerOnly + 组合 + newest-first
// (id DESC = 插入序,与 ts 无关——后插入的"旧行"排在前) + 已删实体原串直配。
func TestQueryAudit_FiltersAndOrder(t *testing.T) {
	s := newTestStore(t)
	fresh := time.Now()
	old := fresh.Add(-2 * time.Hour)
	writeAuditRows(t, s,
		AuditRow{TS: fresh, ProjectID: "p1", ServerID: "srvA", Action: "exec", Command: "uptime", Status: "ok", ExitCode: 0, DurationMS: 5},
		AuditRow{TS: old, ProjectID: "p1", ServerID: "srvB", Action: "exec", Command: "whoami", Status: "error", ExitCode: 2, DurationMS: 9},
		AuditRow{TS: fresh, ProjectID: "", ServerID: "srvA", Action: "project.revoke", Status: "ok"},
		AuditRow{TS: old, ProjectID: "p-gone", ServerID: "", Action: "download", Command: "/etc/hostname", Status: "ok", DurationMS: 40},
	)

	// newest-first = 插入序:第 4 行(download,ts=old)最晚插入 → 排第一。
	rows, err := s.QueryAudit(AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[0].Action != "download" || rows[3].Command != "uptime" {
		t.Fatalf("id DESC order broken: %+v", rows)
	}

	if rows, err = s.QueryAudit(AuditFilter{ServerIDs: []string{"srvA"}}); err != nil || len(rows) != 2 {
		t.Fatalf("server filter: %v n=%d", err, len(rows))
	}
	// 已删 server 的历史行:原串直配命中,零存在性校验。
	if rows, err = s.QueryAudit(AuditFilter{ServerIDs: []string{"srv-deleted-long-ago"}}); err != nil || len(rows) != 0 {
		t.Fatalf("deleted-entity raw-id filter must be empty-not-error: %v n=%d", err, len(rows))
	}
	if rows, err = s.QueryAudit(AuditFilter{ProjectIDs: []string{"p1"}}); err != nil || len(rows) != 2 {
		t.Fatalf("project filter: %v n=%d", err, len(rows))
	}
	if rows, err = s.QueryAudit(AuditFilter{ProjectIDs: []string{"p-gone"}}); err != nil || len(rows) != 1 {
		t.Fatalf("deleted-project raw-id filter: %v n=%d", err, len(rows))
	}
	if rows, err = s.QueryAudit(AuditFilter{OwnerOnly: true}); err != nil || len(rows) != 1 || rows[0].Action != "project.revoke" {
		t.Fatalf("owner-only: %v %+v", err, rows)
	}
	if rows, err = s.QueryAudit(AuditFilter{Actions: []string{"exec"}, Statuses: []string{"error"}}); err != nil || len(rows) != 1 || rows[0].Command != "whoami" {
		t.Fatalf("action+status combo: %v %+v", err, rows)
	}
	if rows, err = s.QueryAudit(AuditFilter{Since: fresh.Add(-time.Minute).Unix()}); err != nil || len(rows) != 2 {
		t.Fatalf("since filter: %v n=%d", err, len(rows))
	}
	// 组合 + limit。
	if rows, err = s.QueryAudit(AuditFilter{Since: old.Add(-time.Minute).Unix(), ServerIDs: []string{"srvA"}, Actions: []string{"exec"}, Statuses: []string{"ok"}, Limit: 1}); err != nil || len(rows) != 1 || rows[0].Command != "uptime" {
		t.Fatalf("combo: %v %+v", err, rows)
	}
	// 未知枚举值静默空。
	if rows, err = s.QueryAudit(AuditFilter{Actions: []string{"nosuchaction"}}); err != nil || len(rows) != 0 {
		t.Fatalf("unknown action must be silent-empty: %v n=%d", err, len(rows))
	}
}

// TestQueryAudit_LimitGate: 0=不限;N=前 N;负值报错(冻结文案)。
func TestQueryAudit_LimitGate(t *testing.T) {
	s := newTestStore(t)
	writeAuditRows(t, s, AuditRow{Action: "exec"}, AuditRow{Action: "exec"}, AuditRow{Action: "exec"})

	if rows, err := s.QueryAudit(AuditFilter{Limit: 0}); err != nil || len(rows) != 3 {
		t.Fatalf("limit 0 = unlimited: %v n=%d", err, len(rows))
	}
	if rows, err := s.QueryAudit(AuditFilter{Limit: 2}); err != nil || len(rows) != 2 {
		t.Fatalf("limit 2: %v n=%d", err, len(rows))
	}
	if _, err := s.QueryAudit(AuditFilter{Limit: -1}); err == nil || err.Error() != "limit must be >= 0" {
		t.Fatalf("negative limit must error with frozen text, got: %v", err)
	}
}

// TestAuditRow_JSONMap_GoldenWithSidecar: JSONMap 是 sidecar 与 `audit --json`
// 的唯一构造(spec §3)——带着恶劣 payload 走 sidecar 路径,要求逐字节等于
// json.Marshal(JSONMap)(spec §6 矩阵 7 的 store 侧)。
func TestAuditRow_JSONMap_GoldenWithSidecar(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "cache-audit.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetReadOnly(f) // WriteAudit 进 sidecar 分支

	nasty := AuditRow{
		TS: time.Unix(1750000000, 0), ProjectID: "", ServerID: "srvA",
		Action: "exec", Status: "ok", ExitCode: 0, DurationMS: 7,
		Command: "echo \"<a>&\x1b]0;pwn\x7f\xfftwo",
	}
	if err := s.WriteAudit(nasty); err != nil {
		t.Fatal(err)
	}
	f.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(nasty.JSONMap())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(raw), want) {
		t.Fatalf("sidecar bytes != JSONMap bytes:\n got %s\nwant %s", raw, want)
	}
	// 九键恒在。
	var m map[string]any
	if err := json.Unmarshal(want, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "project_id", "server_id", "action", "command", "sudo", "status", "exit_code", "duration_ms"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("JSONMap missing key %q: %v", k, m)
		}
	}
}
