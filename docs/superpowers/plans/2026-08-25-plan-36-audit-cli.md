# Plan 36 · audit CLI 读路径 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** owner-only 审计读路径——`ssh-manager audit` 读 vault 主库 audit_log(过滤 + 人读/JSONL 双形态),零 MCP 暴露面。

**Architecture:** store 层加一个全占位符的 `QueryAudit(AuditFilter)`(既有 `AuditRows` 不动);CLI 单命令(root 直挂,同 `gc` 形态)走 `openUnlockedStore()`;`AuditRow.JSONMap()` 一个构造同时供离线 sidecar 与 `--json`(逐字节同构的机制保证);MCP 面以 `BrokerTools` 单源断言永不收 audit。

**Tech Stack:** Go + SQLite(database/sql 占位符)+ cobra(StringSlice flags)。

**Spec:** `docs/superpowers/specs/2026-08-25-plan-36-audit-cli-design.md`(本计划从其论证;执行者两份都读)

## Global Constraints

以下全部从 spec 逐字搬运,每个任务隐含遵守:

- **冻结文案**:`no matching audit rows`;`showing first %d rows (more exist) — use --limit 0 for full output`(截断,人读专属);`unlimited query: audit_log has no auto-cleanup — output may be large`(limit 0,人读专属);CLI 层 `--limit must be >= 0`;store 层 `limit must be >= 0`。
- **人读行**:`2006-01-02 15:04:05-07:00`(本地时区**带偏移**) + server 列 + project 列 + action + status + `exit=%d` + `%dms` + `[sudo]  `(仅 Sudo 时) + command;渲染四态:project 空→`(owner)`、server 空→`(none)`、id 解析不到名→`<id 前 8 字符>…(deleted)`(id ≤8 字符则整串)、正常→名字。
- **转义表**(统一应用于人读行**全部动态文本字段**:command、project/server 显示名、action、status):`\`→`\\`;<0x20 与 0x7f 中 `\n`/`\t`/`\r` 用字面名、其余 `\xNN`;U+0080-U+009F、U+2028/U+2029、bidi(U+200E/U+200F/U+202A-U+202E/U+2066-U+2069)、不可见格式化(U+00AD/U+200B-U+200D/U+2060/U+FEFF)→`\uXXXX`;其余非 ASCII 原文。
- **--json**:九字段(`ts/project_id/server_id/action/command/sudo/status/exit_code/duration_ms`)恒出现、零值照写、无 null 无 omitempty;编码 = Go `json.Marshal` 默认行为(转义 <0x20、`<>&`、U+2028/2029,无效 UTF-8→U+FFFD,0x7f 原样);实现复用 `AuditRow.JSONMap()`(map 字典序键序两侧一致);空结果零行不打印提示。
- **过滤器语义**:name 优先命中→id;未命中→原串直配;**零存在性校验**(查无=空结果 exit 0,不报错);StringSlice 可重复/逗号;`--owner` 与 `--project` 同给报错;未知 `--action`/`--status` 值静默空。
- **--since**:相对 = 单一「整数或小数 + 恰一个单位」(`30m`/`1.5h`/`7d`/`2w`;复合链 `1h30m` 拒);绝对按序尝试 `time.RFC3339` → `2006-01-02T15:04:05`(本地)→ `2006-01-02`(本地 00:00);坏值报错文案列出全部合法形态。
- **limit**:默认 100;`0` = 不限;负值 CLI 与 store **双闸报错**;截断探测 = 查 `limit+1` 行;`--json` 模式无任何 stderr 提示。
- **排序**:`ORDER BY id DESC`(主键自增 = 插入序 = newest-first)。
- **MCP 面铁律**:`BrokerTools` 与 server.go 工具注册**零改动**——本 plan 不新增任何 MCP 工具。
- **过程纪律**:非 ASCII 编辑后逐字节校验;commit 尾行 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`;本机 `-race` 环境性损坏(Plan 35 已记)——单测不带 -race,CI Linux 覆盖;worktree 内 gopls 诊断为噪声,以 `go build`/`go test` 为准;`WriteAudit` sidecar 分支重构后 `readonly_test.go`/`mcp_cache_test.go` 必须原样绿。

---

### Task 1: store 层 — AuditFilter/QueryAudit + JSONMap + 负 limit 闸

**Files:**
- Modify: `internal/store/audit.go`(扩展:QueryAudit/scanAuditRows 抽取/JSONMap;WriteAudit sidecar 分支改用 JSONMap)
- Test: `internal/store/audit_query_test.go`(新建)

**Interfaces:**
- Consumes: 既有 `AuditRow`、`WriteAudit`、`newTestStore(t)`(store 测试夹具)。
- Produces(Task 2 依赖):
  - `type AuditFilter struct { Since int64; ServerIDs []string; ProjectIDs []string; OwnerOnly bool; Actions []string; Statuses []string; Limit int }`
  - `func (s *Store) QueryAudit(f AuditFilter) ([]AuditRow, error)`(newest-first;`Limit<0` 报错 `limit must be >= 0`)
  - `func (r AuditRow) JSONMap() map[string]any`(九键,与 sidecar 逐字节同构)

- [ ] **Step 1: 写失败测试**

新建 `internal/store/audit_query_test.go`:

```go
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store -run 'TestQueryAudit|TestAuditRow_JSONMap' -v`
Expected: FAIL——`s.QueryAudit undefined` / `s.JSONMap undefined`(编译错)。

- [ ] **Step 3: 最小实现**

`internal/store/audit.go` 顶部 import 加 `"fmt"`、`"strings"`;文件追加:

```go
// AuditFilter narrows QueryAudit. Zero value = all rows, newest first.
type AuditFilter struct {
	Since      int64 // unix seconds; 0 = no lower bound
	ServerIDs  []string
	ProjectIDs []string
	OwnerOnly  bool // only rows with an empty project_id (owner actions)
	Actions    []string
	Statuses   []string
	Limit      int // rows to return; 0 = unlimited; negative = error
}

// QueryAudit returns audit rows newest-first (ORDER BY id DESC — the
// AUTOINCREMENT pk IS the insertion order). Values go through placeholders
// exclusively; only the placeholder count is ever concatenated into SQL.
// Filter semantics are zero-existence-check by design: a value that matches
// nothing yields an empty result, never an error, so history rows of deleted
// entities stay reachable by their old ids (spec §1).
func (s *Store) QueryAudit(f AuditFilter) ([]AuditRow, error) {
	if f.Limit < 0 {
		// SQLite treats LIMIT -1 as UNLIMITED — never let a negative value
		// silently become a full scan (spec §2 second gate).
		return nil, fmt.Errorf("limit must be >= 0")
	}
	var conds []string
	var args []any
	if f.Since != 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, f.Since)
	}
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := strings.Repeat("?,", len(vals))
		conds = append(conds, col+" IN ("+ph[:len(ph)-1]+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	in("server_id", f.ServerIDs)
	in("project_id", f.ProjectIDs)
	in("action", f.Actions)
	in("status", f.Statuses)
	if f.OwnerOnly {
		conds = append(conds, "(project_id IS NULL OR project_id = '')")
	}
	q := "SELECT ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms FROM audit_log"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditRows(rows)
}

// scanAuditRows is the shared row→AuditRow scan (extracted from AuditRows so
// both read paths stay byte-identical in interpretation).
func scanAuditRows(rows *sql.Rows) ([]AuditRow, error) {
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var ts int64
		var projectID, serverID, action, command, status sql.NullString
		var sudo int
		var exitCode, durationMS sql.NullInt64
		if err := rows.Scan(&ts, &projectID, &serverID, &action, &command, &sudo, &status, &exitCode, &durationMS); err != nil {
			return nil, err
		}
		r.TS = time.Unix(ts, 0)
		r.ProjectID = projectID.String
		r.ServerID = serverID.String
		r.Action = action.String
		r.Command = command.String
		r.Sudo = sudo == 1
		r.Status = status.String
		r.ExitCode = int(exitCode.Int64)
		r.DurationMS = durationMS.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// JSONMap is the single nine-field map construction shared by the offline
// sidecar (WriteAudit read-only branch) and the owner-facing `audit --json`
// output — one construction, byte-identical encoding in both consumers
// (json.Marshal key order for maps is lexicographic). Zero values are written
// as-is: no null, no omitempty (spec §3).
func (r AuditRow) JSONMap() map[string]any {
	return map[string]any{
		"ts":          r.TS.Unix(),
		"project_id":  r.ProjectID,
		"server_id":   r.ServerID,
		"action":      r.Action,
		"command":     r.Command,
		"sudo":        r.Sudo,
		"status":      r.Status,
		"exit_code":   r.ExitCode,
		"duration_ms": r.DurationMS,
	}
}
```

同时两处改写(行为零变):

1. `AuditRows` 的循环体换成 `return scanAuditRows(rows)`(查询部分不动);
2. `WriteAudit` 只读分支的字面 `rec := map[string]any{...}` 换成 `rec := r.JSONMap()`。

- [ ] **Step 4: 跑测试确认通过(含既有回归)**

Run: `go test ./internal/store -v`
Expected: PASS(新 3 测 + 既有 audit_test/readonly_test/export_test 全绿)。

- [ ] **Step 5: Commit**

```bash
git add internal/store/audit.go internal/store/audit_query_test.go
git commit -m "feat(store): QueryAudit 过滤查询 + AuditRow.JSONMap 双消费方同构 + 负 limit 闸(Plan 36 T1)"
```

---

### Task 2: CLI — `ssh-manager audit` 命令全套

**Files:**
- Create: `internal/cli/audit.go`
- Modify: `internal/cli/root.go:16`(AddCommand 列表加 `newAuditCmd()`,插在 `newGCCmd()` 后)
- Test: `internal/cli/audit_test.go`(新建)

**Interfaces:**
- Consumes(Task 1): `store.AuditFilter`、`(*Store).QueryAudit`、`(AuditRow).JSONMap`;既有 `openUnlockedStore`、`withEnv`/`hexEncode`(cli 测试既有夹具)、`(*Store).AddProfile/AddProject/GetServerByName/GetProjectByName/ListProjects/ListServers`。
- Produces: `newAuditCmd()`(root 注册);纯函数 `parseAuditSince`/`parseRelativeDuration`/`escapeAuditText`/`displayAuditEntity`/`displayAuditProject`。

- [ ] **Step 1: 写失败测试**

新建 `internal/cli/audit_test.go`:

```go
package cli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// nastyCmd carries the adversarial payload reused across seeds and assertions.
const nastyCmd = "echo \"<hi>&\x1b]0;pwn\x7f"

// seedAuditStore: SSHMGR_STORE 指向新 vault——profile "dev"、project "alpha"、
// server "gpu",六行审计行覆盖过滤维度(含已删实体原串、owner 行、恶劣 command)。
func seedAuditStore(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	db := dir + "/audit.db"
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{"SSHMGR_STORE": db, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	st, err := store.Open(db, mk)
	if err != nil {
		t.Fatal(err)
	}
	profID, err := st.AddProfile("dev")
	if err != nil {
		t.Fatal(err)
	}
	pid, _, err := st.AddProject("alpha", profID)
	if err != nil {
		t.Fatal(err)
	}
	srvID, err := st.AddServerWithCredentials(
		&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u"},
		&models.Credential{Type: models.CredPassword, Secret: []byte("x")},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	fresh := time.Now()
	for _, r := range []store.AuditRow{
		{TS: old, ProjectID: pid, ServerID: srvID, Action: "exec", Command: "uptime", Status: "ok", ExitCode: 0, DurationMS: 4},
		{TS: old, ProjectID: pid, ServerID: srvID, Action: "exec", Command: "whoami", Status: "error", ExitCode: 2, DurationMS: 6, Sudo: true},
		{TS: old, ProjectID: pid, ServerID: "srv-gone", Action: "forward", Command: "127.0.0.1:5432 id=t1", Status: "ok", DurationMS: 100},
		{TS: old, ProjectID: "", ServerID: "", Action: "project.revoke", Status: "ok"},
		{TS: old, ProjectID: "proj-gone", ServerID: srvID, Action: "download", Command: "/etc/hostname", Status: "ok", DurationMS: 40},
		{TS: fresh, ProjectID: pid, ServerID: srvID, Action: "exec", Command: nastyCmd, Status: "ok", DurationMS: 1},
	} {
		if err := st.WriteAudit(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return mk
}

func runAudit(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	full := append([]string{"audit"}, args...)
	root := NewRootCmd()
	root.SetArgs(full)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

func auditLines(t *testing.T, out *bytes.Buffer) []string {
	t.Helper()
	var ls []string
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if l != "" && !strings.HasPrefix(l, "unlimited query:") && !strings.HasPrefix(l, "showing first") {
			ls = append(ls, l)
		}
	}
	return ls
}

// TestAudit_FiltersAndDefaults: 默认全量 newest-first + name 解析 + 组合 +
// --owner + 原串直配 + --since 三种绝对字面量/相对形态/坏值。
func TestAudit_FiltersAndDefaults(t *testing.T) {
	seedAuditStore(t)

	out, err := runAudit(t)
	if err != nil {
		t.Fatal(err)
	}
	ls := auditLines(t, out)
	if len(ls) != 6 {
		t.Fatalf("default (limit 100) must show all 6, got %d: %q", len(ls), out.String())
	}
	if !strings.Contains(ls[0], `echo \"<hi>&\x1b]0;pwn\x7f`) { // newest first + escaping
		t.Fatalf("newest row first with escaping: %q", ls[0])
	}
	if strings.ContainsRune(ls[0], 0x1b) || strings.ContainsRune(ls[0], 0x7f) {
		t.Fatalf("raw control bytes leaked: %q", ls[0])
	}

	out, err = runAudit(t, "--project", "alpha")
	if err != nil || len(auditLines(t, out)) != 3 {
		t.Fatalf("--project name: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--project", "alpha", "--status", "error")
	if err != nil || len(auditLines(t, out)) != 1 || !strings.Contains(out.String(), "whoami") {
		t.Fatalf("--project+--status: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--server", "gpu")
	if err != nil || len(auditLines(t, out)) != 4 {
		t.Fatalf("--server name: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--server", "srv-gone")
	if err != nil || len(auditLines(t, out)) != 1 {
		t.Fatalf("--server raw id (zero existence check): %v %q", err, out.String())
	}
	out, err = runAudit(t, "--owner")
	if err != nil || len(auditLines(t, out)) != 1 || !strings.Contains(out.String(), "(owner)") || !strings.Contains(out.String(), "(none)") {
		t.Fatalf("--owner render: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--project", "proj-gone")
	if err != nil || !strings.Contains(out.String(), "proj-gon…(deleted)") {
		t.Fatalf("deleted project render: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--action", "nosuchaction")
	if err != nil || !strings.Contains(out.String(), "no matching audit rows") {
		t.Fatalf("unknown action must be silent-empty: %v %q", err, out.String())
	}

	// --since: 相对(单数+单位)/复合拒/绝对三字面量/坏值文案。
	out, err = runAudit(t, "--since", "1h")
	if err != nil || len(auditLines(t, out)) != 1 {
		t.Fatalf("--since 1h: %v %q", err, out.String())
	}
	if _, err = runAudit(t, "--since", "1.5h"); err != nil {
		t.Fatalf("--since 1.5h must parse: %v", err)
	}
	if _, err = runAudit(t, "--since", "1h30m"); err == nil || !strings.Contains(err.Error(), "invalid --since") {
		t.Fatalf("--since 1h30m must be rejected: %v", err)
	}
	if _, err = runAudit(t, "--since", "2026-08-20T09:00:00+08:00"); err != nil {
		t.Fatalf("RFC3339 must parse: %v", err)
	}
	if _, err = runAudit(t, "--since", "2026-08-20T09:00:00"); err != nil {
		t.Fatalf("local offset-less datetime must parse: %v", err)
	}
	if _, err = runAudit(t, "--since", "2026-08-20"); err != nil {
		t.Fatalf("plain date must parse: %v", err)
	}
	_, err = runAudit(t, "--since", "nonsense")
	if err == nil || !strings.Contains(err.Error(), "invalid --since") || !strings.Contains(err.Error(), "30m") || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("bad --since error must list the forms: %v", err)
	}
	// RFC3339 实际过滤行为(过滤断言走 UTC 形态,避开本地时区分支)。
	out, err = runAudit(t, "--since", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
	if err != nil || len(auditLines(t, out)) != 1 {
		t.Fatalf("--since RFC3339 filter: %v %q", err, out.String())
	}

	// 互斥。
	if _, err = runAudit(t, "--owner", "--project", "alpha"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("--owner + --project must error: %v", err)
	}
}


// TestAudit_EscapingAndRender: 时间戳带偏移、转义集(spec §3 全行:反斜杠可逆/
// 控制字符 \xNN/C1+行分隔+bidi+不可见格式化 \uXXXX/中文原文)、动态字段统一转义。
// 注意:测试源码一律用显式 \u/\x 转义构造输入,绝不裸写不可见字符。
func TestAudit_EscapingAndRender(t *testing.T) {
	seedAuditStore(t)

	out, err := runAudit(t)
	if err != nil {
		t.Fatal(err)
	}
	first := auditLines(t, out)[0]
	// 时间戳 = 本地时区带偏移(自描述)。
	tsRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2}  `)
	if !tsRe.MatchString(first) {
		t.Fatalf("timestamp must carry local offset: %q", first)
	}
	// 恶劣 command:ESC/DEL 转义后可见、原样字节不出现。
	if !strings.Contains(first, `\x1b`) || !strings.Contains(first, `\x7f`) {
		t.Fatalf("control bytes must be escaped: %q", first)
	}
	if strings.ContainsRune(first, 0x1b) || strings.ContainsRune(first, 0x7f) {
		t.Fatalf("raw control bytes leaked: %q", first)
	}
	// 单元级:转义函数直测(每类取代表字符,全部显式转义构造)。
	in := "a\\b\nc\td\re\x1f\x7f" +
		string([]rune{0x9b, 0x2028, 0x2029, 0x200e, 0x202a, 0x2066, 0x00ad, 0x200b, 0x2060, 0xfeff}) +
		"中"
	want := `a\\b\nc\td\re\x1f\x7f\u009b\u2028\u2029\u200e\u202a\u2066\u00ad\u200b\u2060\ufeff中`
	if got := escapeAuditText(in); got != want {
		t.Fatalf("escapeAuditText:\n got %q\nwant %q", got, want)
	}
}

// TestAudit_JSON: JSONL 可解析、九键恒在、owner 行零值非缺失、--json 无 stderr 提示。
func TestAudit_JSON(t *testing.T) {
	seedAuditStore(t)

	out, err := runAudit(t, "--json", "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "unlimited query") || strings.Contains(out.String(), "showing first") {
		t.Fatalf("--json must stay silent on stderr hints: %q", out.String())
	}
	ls := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(ls) != 6 {
		t.Fatalf("--json --limit 0 must emit 6 lines, got %d", len(ls))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(ls[0]), &m); err != nil {
		t.Fatalf("json line: %v", err)
	}
	if len(m) != 9 {
		t.Fatalf("exactly nine keys, got %d: %v", len(m), m)
	}
	// nasty command:JSON round-trip + json.Marshal 转义可见(< 转为 \u003c、ESC 转为 \u001b)
	if m["command"] != nastyCmd {
		t.Fatalf("json round-trip of nasty command: %v", m["command"])
	}
	if !strings.Contains(ls[0], `\u003c`) || strings.Contains(ls[0], "\x1b") {
		t.Fatalf("json line must show json.Marshal escaping: %s", ls[0])
	}
	out, err = runAudit(t, "--json", "--owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["project_id"]; !ok || v != "" { // present-as-empty, not absent
		t.Fatalf("owner row project_id must be present-as-empty: %v", m)
	}
}

// TestAudit_LimitSemantics: 负值 CLI 闸、0 警告(人读)、截断提示与恰好不提示。
func TestAudit_LimitSemantics(t *testing.T) {
	seedAuditStore(t)

	if _, err := runAudit(t, "--limit", "-1"); err == nil || !strings.Contains(err.Error(), "--limit must be >= 0") {
		t.Fatalf("--limit -1: %v", err)
	}
	out, err := runAudit(t, "--limit", "0")
	if err != nil || !strings.Contains(out.String(), "unlimited query:") {
		t.Fatalf("--limit 0 human warning: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--limit", "2")
	if err != nil || len(auditLines(t, out)) != 2 || !strings.Contains(out.String(), "showing first 2 rows (more exist)") {
		t.Fatalf("truncation hint: %v %q", err, out.String())
	}
	out, err = runAudit(t, "--limit", "6")
	if err != nil || strings.Contains(out.String(), "showing first") {
		t.Fatalf("exactly-limit must not hint: %v %q", err, out.String())
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli -run 'TestAudit' -v`
Expected: FAIL——编译错 `undefined: escapeAuditText`(audit.go 尚未创建,整个包编译不出)。

- [ ] **Step 3: 写实现**

新建 `internal/cli/audit.go`:

```go
package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
)

// newAuditCmd: `ssh-manager audit` — the owner's read path over the vault's
// audit_log (backlog #16). Owner-only BY CONSTRUCTION: it goes through
// openUnlockedStore() (the master-key gate) and is never registered as an MCP
// tool — audit rows carry other agents' full command text and may contain
// secrets (spec §4).
func newAuditCmd() *cobra.Command {
	var (
		since    string
		servers  []string
		projects []string
		owner    bool
		actions  []string
		statuses []string
		limit    int
		asJSON   bool
	)
	c := &cobra.Command{
		Use:   "audit",
		Short: "Read the vault audit log (owner-only)",
		Long: "Read audit_log rows from the vault, newest first.\n" +
			"Filters: --since (30m | 1.5h | 7d | 2w | RFC3339 | local datetime | date),\n" +
			"--server/--project (name or id; unknown values simply match nothing),\n" +
			"--owner (rows with no project = owner actions), --action/--status, --limit.\n" +
			"--limit 0 = unlimited (audit_log has no auto-cleanup — mind big vaults).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if owner && len(projects) > 0 {
				return fmt.Errorf("--owner and --project are mutually exclusive (--owner selects rows with NO project)")
			}
			if limit < 0 {
				return fmt.Errorf("--limit must be >= 0")
			}
			var sinceUnix int64
			if since != "" {
				v, err := parseAuditSince(since, time.Now())
				if err != nil {
					return err
				}
				sinceUnix = v
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()

			probe := limit
			if probe > 0 {
				probe++ // fetch one extra row to detect truncation (spec §1)
			}
			rows, err := s.QueryAudit(store.AuditFilter{
				Since:      sinceUnix,
				ServerIDs:  resolveServerIDs(s, servers),
				ProjectIDs: resolveProjectIDs(s, projects),
				OwnerOnly:  owner,
				Actions:    actions,
				Statuses:   statuses,
				Limit:      probe,
			})
			if err != nil {
				return err
			}
			truncated := limit > 0 && len(rows) > limit
			if truncated {
				rows = rows[:limit]
			}

			out, errs := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if asJSON {
				for _, r := range rows {
					b, err := json.Marshal(r.JSONMap())
					if err != nil {
						return err
					}
					fmt.Fprintln(out, string(b))
				}
				return nil
			}
			if limit == 0 {
				fmt.Fprintln(errs, "unlimited query: audit_log has no auto-cleanup — output may be large")
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "no matching audit rows")
				return nil
			}
			projNames, srvNames := auditNameMaps(s)
			for _, r := range rows {
				fmt.Fprintf(out, "%s  %s  %s  %s  %s  exit=%d  %dms  %s%s\n",
					r.TS.Local().Format("2006-01-02 15:04:05-07:00"),
					escapeAuditText(displayAuditEntity(r.ServerID, srvNames)),
					escapeAuditText(displayAuditProject(r.ProjectID, projNames)),
					escapeAuditText(r.Action),
					escapeAuditText(r.Status),
					r.ExitCode, r.DurationMS,
					sudoMark(r.Sudo),
					escapeAuditText(r.Command),
				)
			}
			if truncated {
				fmt.Fprintf(errs, "showing first %d rows (more exist) — use --limit 0 for full output\n", limit)
			}
			return nil
		},
	}
	c.Flags().StringVar(&since, "since", "", "lower bound: 30m/1.5h/7d/2w, RFC3339, local datetime, or plain date")
	c.Flags().StringSliceVar(&servers, "server", nil, "filter by server name or id (repeatable/comma)")
	c.Flags().StringSliceVar(&projects, "project", nil, "filter by project name or id (repeatable/comma)")
	c.Flags().BoolVar(&owner, "owner", false, "only owner (non-agent) rows — rows with no project")
	c.Flags().StringSliceVar(&actions, "action", nil, "filter by action (repeatable/comma; unknown values match nothing)")
	c.Flags().StringSliceVar(&statuses, "status", nil, "filter by status (repeatable/comma; unknown values match nothing)")
	c.Flags().IntVar(&limit, "limit", 100, "max rows (default 100; 0 = unlimited — audit_log has no auto-cleanup, mind big vaults)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSONL output (same nine fields as the offline sidecar)")
	return c
}

// parseAuditSince (spec §1): a single relative duration (integer or decimal
// number + exactly one of m/h/d/w), else — in order — RFC3339 with offset, a
// local offset-less datetime, or a plain local date.
func parseAuditSince(s string, now time.Time) (int64, error) {
	if d, ok := parseRelativeDuration(s); ok {
		return now.Add(-d).Unix(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("invalid --since %q: use a relative duration (one number + one unit, e.g. 30m, 1.5h, 7d, 2w) or an absolute time (RFC3339 like 2026-08-20T09:00:00+08:00, local datetime 2026-08-20T09:00:00, or date 2026-08-20)", s)
}

func parseRelativeDuration(s string) (time.Duration, bool) {
	units := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	if len(s) < 2 {
		return 0, false
	}
	d, ok := units[s[len(s)-1]]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return time.Duration(f * float64(d)), true
}

// resolveServerIDs / resolveProjectIDs: name-first, else the raw value as-is.
// Zero existence checking (spec §1): filters only narrow the result set — a
// value matching nothing yields an empty result, never an error, so history
// rows of deleted entities stay reachable by their old ids. Name uniqueness is
// schema-enforced (servers.name / projects.name are NOT NULL UNIQUE), so a
// name hit is always at most one entity.
func resolveServerIDs(s *store.Store, vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if srv, _ := s.GetServerByName(v); srv != nil {
			v = srv.ID
		}
		out = append(out, v)
	}
	return out
}

func resolveProjectIDs(s *store.Store, vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if p, _ := s.GetProjectByName(v); p != nil {
			v = p.ID
		}
		out = append(out, v)
	}
	return out
}

func auditNameMaps(s *store.Store) (proj, srv map[string]string) {
	proj, srv = map[string]string{}, map[string]string{}
	if ps, err := s.ListProjects(); err == nil {
		for _, p := range ps {
			proj[p.ID] = p.Name
		}
	}
	if ss, err := s.ListServers(); err == nil {
		for _, x := range ss {
			srv[x.ID] = x.Name
		}
	}
	return proj, srv
}

// displayAuditEntity: empty id → "(none)" (no server context, e.g. project-level
// owner actions); unresolvable id → first 8 chars + "…(deleted)"; else the name.
// Everything is escaped at the print site (spec §3).
func displayAuditEntity(id string, names map[string]string) string {
	if id == "" {
		return "(none)"
	}
	if n, ok := names[id]; ok && n != "" {
		return n
	}
	if len(id) > 8 {
		return id[:8] + "…(deleted)"
	}
	return id + "…(deleted)"
}

// displayAuditProject: empty project_id → "(owner)"; else the entity rules.
func displayAuditProject(id string, names map[string]string) string {
	if id == "" {
		return "(owner)"
	}
	return displayAuditEntity(id, names)
}

// escapeAuditText (spec §3): applied to EVERY dynamic text field of the human
// line. Closed set (backslash itself escapes first → reversible), line
// boundaries preserved, terminal control-sequence injection and invisible-
// character spoofing closed; other non-ASCII (CJK included) stays verbatim.
// Invalid UTF-8 bytes surface as U+FFFD via range-over-string, matching what
// json.Marshal does on the --json side.
func escapeAuditText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f, r == 0x2028 || r == 0x2029,
			r == 0x200e || r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069,
			r == 0x00ad, r >= 0x200b && r <= 0x200d, r == 0x2060, r == 0xfeff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sudoMark(b bool) string {
	if b {
		return "[sudo]  "
	}
	return ""
}
```

`internal/cli/root.go:16` 的 AddCommand 列表在 `newGCCmd(),` 后插入 `newAuditCmd(),`。

- [ ] **Step 4: 跑测试确认通过(含包级回归)**

Run: `go test ./internal/cli -v`
Expected: PASS(新 4 测 + 既有 cli 测试全绿)。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/audit.go internal/cli/audit_test.go internal/cli/root.go
git commit -m "feat(cli): ssh-manager audit 命令(过滤/转义/四态渲染/JSONL/截断提示,Plan 36 T2)"
```

---

### Task 3: MCP 面断言 + 文档联动 + 全量回归

**Files:**
- Create: `internal/mcpserver/audit_face_test.go`
- Modify: `README.md`(CLI 命令清单加 audit 行 + 用法例)、`docs/multi-machine.md`(权威端取证注记)、`docs/threat-model.md`(audit 段补一句)
- 无生产代码改动(纯测试 + 文档)

**Interfaces:**
- Consumes: `mcpserver.BrokerTools`(server.go:29 单源;e2e_test 已钉 tools/list 与其集合相等)。
- Produces: 无(验收面)。

- [ ] **Step 1: 写失败测试**(先写——注意此测试在实现"正确"的当前态就会 PASS;它的角色是**钉住**未来不破。TDD 语义此处为「契约钉子」:断言写好后,任何把 audit 挂上 MCP 面的改动都会红。)

新建 `internal/mcpserver/audit_face_test.go`:

```go
package mcpserver

import (
	"strings"
	"testing"
)

// TestBrokerToolsNoAuditFace (spec §6 matrix 8): audit data must NEVER be
// reachable through the MCP tool surface — audit rows carry other agents' full
// command text and may contain secrets (backlog #16 owner ruling). BrokerTools
// is the single source the server registers tools from (e2e_test pins the
// tools/list == BrokerTools set equality), so guarding it here guards the wire.
func TestBrokerToolsNoAuditFace(t *testing.T) {
	for _, name := range BrokerTools {
		if strings.Contains(strings.ToLower(name), "audit") {
			t.Fatalf("BrokerTools must not expose audit data over MCP: %q", name)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认通过**(钉子即刻绿是预期——若红说明有人已把 audit 挂上工具面,停下排查)

Run: `go test ./internal/mcpserver -run TestBrokerToolsNoAuditFace -v`
Expected: PASS。

- [ ] **Step 3: 文档联动**

1. `README.md`:在 CLI 命令清单(搜 `tunnels ls` 所在表)加一行(列形与邻行一致):
   `| \`audit\` | — | Read the vault audit log, newest first (owner-only): \`--since 30m|7d|RFC3339|date\`, \`--server/--project <name|id>\`, \`--owner\`, \`--action/--status\`, \`--limit\` (0 = all), \`--json\` for JSONL. |`
   并在用法示例区(如有)补一例:
   ```text
   ssh-manager audit --since 24h --project my-agent --status error
   ssh-manager audit --json --limit 0 > audit.jsonl   # nine fields, sidecar-compatible
   ```
   附已知取值参考(命令清单下方或 audit 行的说明区,精简一段):action = exec/download/upload/forward + owner 侧 project.rotate/disable/enable/revoke/delete;status = ok/error/timeout/bind_denied(取值随版本演进,未知值静默空)。
2. `docs/multi-machine.md`:在 serve/隧道相关小节附近加注记(语言随邻文):
   `The audit CLI reads the LOCAL vault only. In the serve topology agent actions are audited into the authoritative broker's vault — run \`ssh-manager audit\` on the broker (e.g. the NUC10) for full agent history.`
3. `docs/threat-model.md`:找到 audit 相关段落(搜 `audit`)补一句:
   `\`ssh-manager audit\` is an owner-side read path (master-key-gated CLI, never an MCP tool): audit rows contain full agent command text and possible secrets, so they are deliberately unreachable from the agent surface.`

- [ ] **Step 4: 全量回归**

Run: `go build ./... && go test ./...`
Expected: 全绿(17 包;conformance/eval 双门控 SKIP 为正常出口)。
Run: `gofmt -l internal/`
Expected: 空输出。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/audit_face_test.go README.md docs/multi-machine.md docs/threat-model.md
git commit -m "test+docs: audit 无 MCP 面钉子 + README/multi-machine/threat-model 联动(Plan 36 T3)"
```
