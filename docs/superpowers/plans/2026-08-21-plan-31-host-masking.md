# Plan 31 实现计划：list_servers host 掩码 + 错误路径清洗（v0.9 破坏性变更）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 兑现「接口级不暴露」——`list_servers` 默认回 `"hidden"` 掩码 host（per-server `expose_host` opt-in 放开），`sshbroker.Connect` 错误文本从源头清洗掉 host/IP/`host:port`（v0.9 破坏性变更）。

**Architecture:** 两项同层改动都落在接口投影层：① `ExposeHost` 布尔存 vault（models → DB 列 → 快照），`ListServersForProfile` 投影决定明文/`"hidden"`；② `sshbroker.Connect` 失败分支包一层 `redactAddr`（定向边界替换 + 无把握整体降级），MCP 工具错误分支原样传递即安全。vault 数据始终完整，掩码执行点 = 运行投影的 binary（在线 serve 端 / 离线 client 端）。

**Tech Stack:** Go 1.25.8（go.mod）、SQLite（modernc.org/sqlite v1.33.1）、bubbletea/huh v2（TUI）、testing 标准库。

**设计 spec（权威依据，遇到歧义以 spec 为准）:** `docs/superpowers/specs/2026-08-21-plan-31-host-masking-design.md`

## Global Constraints

- 工作目录：隔离 worktree `C:\WorkSpace\agent\ssh-manager-mcp\.claude\worktrees\plan-31-host-masking`（分支 `worktree-plan-31-host-masking`），一切命令在此目录执行。
- Go 代码注释用英文（仓库惯例）；TUI 界面文案、CLI flag help 用英文，TUI 表单 Label 沿用中文（与 editfields.go 现状一致）。测试函数名英文。
- 每个任务收尾必须过仓库格式门：`gofmt -l internal/` 输出为空（CI 有格式门）。
- 每个任务一个 commit，消息用仓库惯例格式（`feat:`/`test:`/`docs:` 前缀 + 中文描述），结尾加 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`。
- DB 列 `expose_host INTEGER NOT NULL DEFAULT 0`；SQL 列清单中它的位置固定为 `caveats` 之后、`created_at` 之前（全链 7 处一致，顺序错一列 Scan 就串位）。
- 快照 JSON 字段名固定 `expose_host`；`Snapshot.Version` **不 bump**（spec §2，双向兼容论证已定稿）。
- 掩码字面量固定 `"hidden"`；forward 守卫用大小写不敏感比较（`strings.EqualFold`）。
- 语义铁律（grilling 拍板，不得重新讨论）：默认 false=掩码；错误文本不得带 host/IP/host:port 组合；运行时级隐藏与出网管控不做。
- 仓库无 CHANGELOG 惯例——破坏性变更登记职能在 `docs/compat-matrix.md`。

---

### Task 1: store 层——`ExposeHost` 字段落库（models + DB 列 + migrate + 全部 row-path SQL 位点）

**Files:**
- Modify: `internal/models/models.go:42-60`（Server struct）
- Modify: `internal/store/store.go:204-221`（migrate 的 addColumnIfMissing 块）、`internal/store/store.go:399-426`（schemaSQL servers 表）、`internal/store/store.go:318-338`（rebuildServersNullable 两份列清单）
- Modify: `internal/store/servers.go:47/:58`（两个 SELECT）、`internal/store/servers.go:91-113`（scanServer）
- Modify: `internal/store/tx.go:26-50`（insertServerTx）、`internal/store/tx.go:54-71`（updateServerTx）、`internal/store/tx.go:73-78`（getServerTx——**最优先**，漏改 = GetServer 恒错 = list_servers 静默空列表）
- Test: `internal/store/servers_test.go`（追加两个测试）

**Interfaces:**
- Consumes: 无（起点任务）
- Produces: `models.Server.ExposeHost bool`（后续所有任务依赖此字段名）；store 全读写路径携带该字段；DB 具备 `expose_host` 列（Task 2 快照、Task 3 投影的前提）

- [ ] **Step 1: 写失败测试（行级 round-trip + 旧库迁移 fixture）**

追加到 `internal/store/servers_test.go`（文件已在 package store，测试惯例照抄同文件既有 helper；若该文件没有现成 open-store helper，用下面内联写法）：

```go
// openTestStore opens a fresh store in t.TempDir() (store-package test helper
// for Plan 31; mirrors the mcpserver newStore pattern).
func openTestStore(t *testing.T) *Store {
	t.Helper()
	mk, _ := GenerateMasterKey()
	st, err := Open(t.TempDir()+"/t.db", mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestExposeHostRoundTripRow: AddServer persists ExposeHost and every read
// path (by id / by name / list) returns it. Default (zero value) is false.
func TestExposeHostRoundTripRow(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddServer(&models.Server{
		Name: "exposed", Host: "h1", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, ExposeHost: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "masked", Host: "h2", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, // ExposeHost zero value = false
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetServer(id)
	if err != nil || got == nil {
		t.Fatalf("GetServer: %v %v", got, err)
	}
	if !got.ExposeHost {
		t.Fatal("GetServer: ExposeHost = false, want true")
	}
	byName, _ := st.GetServerByName("masked")
	if byName == nil || byName.ExposeHost {
		t.Fatal("GetServerByName: want ExposeHost=false (default)")
	}
	all, err := st.ListServers()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListServers: %d %v", len(all), err)
	}
	// ListServers ORDER BY name → masked < exposed
	if all[0].ExposeHost || !all[1].ExposeHost {
		t.Fatalf("ListServers ExposeHost = [%v %v], want [false true]", all[0].ExposeHost, all[1].ExposeHost)
	}
	// Full-row update must preserve the bit (updateServerTx writes the whole row).
	got.Name = "exposed2"
	if err := st.UpdateServer(got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetServer(id)
	if again == nil || !again.ExposeHost {
		t.Fatal("UpdateServer dropped ExposeHost")
	}
}

// TestMigrateAddsExposeHostToLegacyDB locks the migrate() ordering claim
// (addColumnIfMissing BEFORE the rebuildServersNullable check — spec §2) and
// the rebuild's two column lists carrying expose_host. A pre-Plan-8-shaped DB
// (no metadata columns, credential_id NOT NULL) goes through BOTH the
// addColumn block and the rebuild inside one Open.
func TestMigrateAddsExposeHostToLegacyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/legacy.db"
	// Craft the legacy shape directly (pre-initSchema), like the DB a
	// pre-Plan-8 binary would have left on disk.
	ldb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.Exec(`CREATE TABLE servers(
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, host TEXT NOT NULL,
		port INTEGER NOT NULL, user TEXT NOT NULL, auth_method TEXT NOT NULL,
		credential_id TEXT NOT NULL, sudo_credential_id TEXT, tags TEXT,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.Exec(`INSERT INTO servers(id,name,host,port,user,auth_method,credential_id,tags,created_at,updated_at)
		VALUES('srv-legacy','old-nuc','10.0.0.9',22,'ops','password','cred-x','[]',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := ldb.Close(); err != nil {
		t.Fatal(err)
	}

	mk, _ := GenerateMasterKey()
	st, err := Open(dbPath, mk) // runs migrate() then initSchema
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	defer st.Close()

	got, err := st.GetServerByName("old-nuc")
	if err != nil || got == nil {
		t.Fatalf("GetServerByName after migration: %v %v", got, err)
	}
	if got.ExposeHost {
		t.Fatal("legacy row must migrate to ExposeHost=false (v0.9 breaking default)")
	}
	// The row itself must survive the rebuild dance.
	if got.ID != "srv-legacy" || got.Host != "10.0.0.9" || got.User != "ops" {
		t.Fatalf("legacy row data lost: %+v", got)
	}
}
```

注意 `servers_test.go` 需要 `database/sql` import（`sql.Open`）；`modernc.org/sqlite` 已由 store 包文件侧 blank-import，无需重复。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run 'TestExposeHostRoundTripRow|TestMigrateAddsExposeHostToLegacyDB' -v`
Expected: FAIL，编译错误 `unknown field 'ExposeHost' in struct literal`（字段还没加）。

- [ ] **Step 3: 加字段与全部 SQL 位点（一次改齐 7 处 + schema + migrate）**

3a. `internal/models/models.go` Server struct——`Caveats` 行后、`CreatedAt` 前插入：

```go
	Caveats          string // operational gotchas; agent reads before acting
	// ExposeHost (Plan 31): owner opt-in to return the plaintext host in
	// list_servers. Default false = the projection masks it as "hidden".
	// Never affects the broker's own dialing — the vault always stores the
	// real host.
	ExposeHost       bool
```

3b. `internal/store/servers.go`——两处 SELECT 的列清单在 `caveats` 与 `created_at` 之间插入 `expose_host`（GetServerByName :47、ListServers :58）：

```go
`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at FROM servers WHERE name=?`
```
（ListServers 同句尾 `ORDER BY name` 不变。）

3c. `scanServer`（servers.go:101）——Scan 目的地在 `&srv.Caveats` 与 `&createdAt` 之间插 `&srv.ExposeHost`，并在 var 块注释说明（bool 从 INTEGER 0/1 扫描，仓库已有先例：export.go 审计行的 `&a.Sudo`）：

```go
	if err := sc.Scan(&srv.ID, &srv.Name, &srv.Host, &srv.Port, &srv.User, &authMethod, &credentialID, &sudoCredentialID, &tagsJSON, &srv.Description, &srv.Location, &srv.Hardware, &srv.Services, &srv.Role, &srv.Caveats, &srv.ExposeHost, &createdAt, &updatedAt); err != nil {
```

3d. `internal/store/tx.go`——三处：
- `insertServerTx` INSERT 列清单插 `expose_host`（caveats 后、created_at 前），VALUES 加一个 `?`（17→18 个占位符），实参在 `srv.Caveats,` 后插 `srv.ExposeHost,`：

```go
	`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
	id, srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), cred, sudo, string(tagsJSON), srv.Description,
	srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, srv.ExposeHost, ts, ts,
```

- `updateServerTx` UPDATE 语句 `caveats=?` 后插 `expose_host=?`，实参 `srv.Caveats,` 后插 `srv.ExposeHost,`：

```go
	`UPDATE servers SET name=?,host=?,port=?,user=?,auth_method=?,credential_id=?,sudo_credential_id=?,tags=?,description=?,location=?,hardware=?,services=?,role=?,caveats=?,expose_host=?,updated_at=? WHERE id=?`,
	srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), cred, sudo, string(tagsJSON), srv.Description,
	srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, srv.ExposeHost, now(), srv.ID,
```

- `getServerTx`（**最优先**，漏改 = GetServer 恒错 → list_servers 静默空列表）：SELECT 列清单同 3b 插法。

3e. `internal/store/store.go` schemaSQL 的 servers 表（:408-426）——`caveats TEXT DEFAULT '',` 行后插：

```sql
  expose_host INTEGER NOT NULL DEFAULT 0,
```

3f. `rebuildServersNullable`（:317-341）——**两份**列清单都要插：CREATE TABLE servers_new 的 `caveats` 行后加上面同一行；INSERT..SELECT 两个列清单在 `caveats` 与 `created_at` 之间插 `expose_host`：

```go
	`INSERT INTO servers_new (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at)
SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at FROM servers`,
```

3g. `migrate()` 的 Plan-8 addColumnIfMissing 块（:204-221）——`caveats` 那行后插（**必须留在 rebuildServersNullable 检查（:248）之前**——该块天然在前，别挪）：

```go
	// Plan 31: servers.expose_host (host masking opt-in). Must run BEFORE the
	// rebuildServersNullable check below — the rebuild's INSERT..SELECT
	// references this column, so on a legacy DB that triggers the rebuild the
	// source table must already have it (spec §2; mechanism proven by the
	// same-shaped SQL experiment).
	if err := addColumnIfMissing(db, "servers", "expose_host", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -v`
Expected: 全 PASS（含新增两测试与既有 store 全量——migrate_null_test 等不能破）。

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/   # 必须空输出
git add internal/models/models.go internal/store/
git commit -m "feat(store): servers.expose_host 列落库——models/scanServer/7 处 SQL 位点/migrate(rebuild 前置)/schema 同步

存量行迁移后=0=掩码(v0.9 破坏性变更本体); 旧库迁移 fixture 锁列序(rebuild 引用列必须先 addColumn)。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: store 层——快照携带 `expose_host`（SnapshotServer + Export/Import + round-trip 测试）

**Files:**
- Modify: `internal/store/export.go:31-49`（SnapshotServer）、`internal/store/export.go:114-136`（ExportSnapshot SELECT + Scan）、`internal/store/export.go:303-313`（ImportSnapshot INSERT）
- Test: `internal/store/export_test.go`（追加测试）

**Interfaces:**
- Consumes: Task 1 的 `models.Server.ExposeHost` 与 DB 列
- Produces: 快照 JSON 含 `"expose_host"` 字段（cache.bin 链路自动获得）；`Snapshot.Version` 保持 1（不 bump）

- [ ] **Step 1: 写失败测试（新→新 round-trip，两态都不丢）**

追加到 `internal/store/export_test.go`：

```go
// TestSnapshotExposeHostRoundTrip: export→import preserves ExposeHost in both
// states. Guards the SQL column lists AND the JSON field against silent
// regression — a lost bit silently degrades an owner opt-in back to masked
// (fail-safe direction, but still an owner-preference loss; spec §2/§6).
func TestSnapshotExposeHostRoundTrip(t *testing.T) {
	st := openTestStore(t) // helper landed in Task 1 (servers_test.go)
	if _, err := st.AddServer(&models.Server{
		Name: "exposed", Host: "h1", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, ExposeHost: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "masked", Host: "h2", Port: 22, User: "u",
		AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Field must exist on the wire (missing json field would decode as false).
	var sawExposed, sawMasked bool
	for _, sv := range snap.Servers {
		if sv.Name == "exposed" && sv.ExposeHost {
			sawExposed = true
		}
		if sv.Name == "masked" && !sv.ExposeHost {
			sawMasked = true
		}
	}
	if !sawExposed || !sawMasked {
		t.Fatalf("snapshot ExposeHost states wrong: exposed=%v masked=%v", sawExposed, sawMasked)
	}

	// Import into a fresh store and verify both states survive.
	mk2, _ := GenerateMasterKey()
	st2, err := Open(t.TempDir()+"/t2.db", mk2)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if err := st2.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"exposed": true, "masked": false} {
		got, err := st2.GetServerByName(name)
		if err != nil || got == nil {
			t.Fatalf("imported %s: %v", name, err)
		}
		if got.ExposeHost != want {
			t.Fatalf("imported %s ExposeHost = %v, want %v", name, got.ExposeHost, want)
		}
	}
}
```

（若 export_test.go 尚无 `openTestStore`——它定义于同包 servers_test.go，直接可用。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestSnapshotExposeHostRoundTrip -v`
Expected: FAIL，编译错误 `unknown field 'ExposeHost'`（SnapshotServer 还没有该字段）。

- [ ] **Step 3: 三处改动**

3a. `SnapshotServer`（export.go:31-49）`Caveats` 后、`CreatedAt` 前插：

```go
	ExposeHost       bool   `json:"expose_host"`
```

3b. `ExportSnapshot` 的 servers SELECT（export.go:119）在 `caveats` 与 `created_at` 之间插 `expose_host`，Scan 目的地 `&sv.Caveats,` 后插 `&sv.ExposeHost,`：

```go
	rs, err := s.db.Query(`SELECT id,name,host,port,user,auth_method,COALESCE(credential_id,''),COALESCE(sudo_credential_id,''),COALESCE(tags,''),description,location,hardware,services,role,caveats,expose_host,created_at,updated_at FROM servers ORDER BY id`)
	...
		if err := rs.Scan(&sv.ID, &sv.Name, &sv.Host, &sv.Port, &sv.User, &sv.AuthMethod,
			&sv.CredentialID, &sv.SudoCredentialID, &sv.TagsRaw, &sv.Description, &sv.Location,
			&sv.Hardware, &sv.Services, &sv.Role, &sv.Caveats, &sv.ExposeHost, &sv.CreatedAt, &sv.UpdatedAt); err != nil {
```

3c. `ImportSnapshot` 的 servers INSERT（export.go:309）列清单与 VALUES 同位插入，实参 `sv.Caveats,` 后加 `sv.ExposeHost,`（占位符 17→18）：

```go
		if _, err := tx.Exec(`INSERT INTO servers(id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sv.ID, sv.Name, sv.Host, sv.Port, sv.User, sv.AuthMethod, credArg, sudoArg, sv.TagsRaw, sv.Description, sv.Location, sv.Hardware, sv.Services, sv.Role, sv.Caveats, sv.ExposeHost, sv.CreatedAt, sv.UpdatedAt); err != nil {
```

**不要动 `Snapshot.Version`（export.go:115 的 `Version: 1`）**——spec §2 定稿：旧快照缺字段→false→掩码（fail-safe）；旧 binary 读新快照忽略未知字段。`serve_snapshot_test.go:66` 断言 `Version == 1` 会自动守住这一点。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -v`
Expected: 全 PASS（含 serve_snapshot_test 的 Version==1 断言）。

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/
git add internal/store/export.go internal/store/export_test.go
git commit -m "feat(store): 快照携带 expose_host(SnapshotServer+Export/Import), Version 不 bump(旧快照缺字段→掩码, fail-safe)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: mcpserver 投影——`list_servers` 默认回 `"hidden"`

**Files:**
- Modify: `internal/mcpserver/core.go:29-67`（ListServersForProfile）
- Modify: `internal/mcpserver/types.go:5-22`（ServerInfo.Host 描述 + 注释）
- Modify: `internal/mcpserver/server.go:63`（list_servers 工具 Description）
- Test: `internal/mcpserver/core_test.go`（追加测试）

**Interfaces:**
- Consumes: Task 1 的 `models.Server.ExposeHost`
- Produces: `ServerInfo.Host` 掩码语义（`"hidden"` 字面量）——agent-facing 契约，Task 6 回归网与 Task 10 文档依赖

- [ ] **Step 1: 写失败测试**

追加到 `internal/mcpserver/core_test.go`：

```go
// TestListServersHostMasking: ExposeHost=false (default) projects Host as the
// literal "hidden"; ExposeHost=true projects the plaintext host. This is the
// v0.9 breaking change itself — v0.8 always returned plaintext (spec §3).
func TestListServersHostMasking(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "masked", Host: "10.0.0.1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "exposed", Host: "10.0.0.2", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st), ExposeHost: true})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a, b})

	got, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]string{}
	for _, s := range got {
		hosts[s.Name] = s.Host
	}
	if hosts["masked"] != "hidden" {
		t.Fatalf("masked server Host = %q, want \"hidden\"", hosts["masked"])
	}
	if hosts["exposed"] != "10.0.0.2" {
		t.Fatalf("exposed server Host = %q, want plaintext", hosts["exposed"])
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run TestListServersHostMasking -v`
Expected: FAIL，`masked server Host = "10.0.0.1", want "hidden"`。

- [ ] **Step 3: 实现投影**

3a. `core.go` ListServersForProfile——`out = append(out, ServerInfo{` 前加两行，`Host:` 字段改投影（其余字段不动）：

```go
		// Plan 31 host masking: default "hidden"; plaintext only when the
		// owner opted in per-server (spec §3). Port is never exposed
		// (ServerInfo has no Port field by design).
		host := "hidden"
		if srv.ExposeHost {
			host = srv.Host
		}
		out = append(out, ServerInfo{
			ID:          srv.ID,
			Name:        srv.Name,
			Host:        host,
```

3b. `types.go` ServerInfo.Host 的 jsonschema 描述替换，并在 struct 注释（:5-8）尾部追加留痕：

```go
	Host        string   `json:"host" jsonschema:"server host; \"hidden\" = owner has not exposed it (default) — address the server via its id"`
```

struct 注释追加：

```go
// Plan 31: Host="hidden" is the ONE deliberate exception to the
// "empty string means explicitly none" invariant — host is never empty, and
// the literal lets the agent distinguish "owner withheld it" from "absent".
// Pathological collision is acknowledged: a real host literally named
// "hidden" with expose_host=true is indistinguishable from masked, and the
// forward_port guard rejects it — owners should avoid the name
// (managing-servers.md); structural elimination is out of scope (spec §3).
```

3c. `server.go:63` 工具 Description 中 `Returns id/name/host/user/has_sudo,` 改为：

```
Returns id/name/host/user/has_sudo (host is "hidden" unless the owner exposed it — address servers via id, never by host), plus owner-provided context: role, services (what's deployed), location, hardware, caveats (special handling — read before acting), tags, description. Never includes credentials.
```

- [ ] **Step 4: 跑测试确认通过（含包内全量）**

Run: `go test ./internal/mcpserver/ -v`
Expected: 全 PASS（已核：现有投影测试无一断言 Host 明文——listmetadata_test 只断言结构化字段）。

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/
git add internal/mcpserver/
git commit -m "feat(mcpserver): list_servers host 默认掩码为 \"hidden\"(expose_host opt-in 放开), jsonschema/工具描述同步

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: forward_port 的 `"hidden"` 误抄格杀（大小写不敏感）

**Files:**
- Modify: `internal/mcpserver/core.go`（ForwardForProfile，iron-rule gate 之后）
- Test: `internal/mcpserver/core_test.go`（追加测试）

**Interfaces:**
- Consumes: Task 3 的掩码字面量 `"hidden"` 语义
- Produces: `ForwardForProfile` 对 `remoteHost` 等值 `"hidden"`（任何大小写）返回错误——Task 6 回归网枚举此分支

- [ ] **Step 1: 写失败测试**

```go
// TestForwardRejectsMaskedLiteral: remoteHost == "hidden" (any case) is the
// one channel where the masked literal could be "used" — a malicious
// server-side resolver record for "hidden" would capture mistyped traffic.
// DNS is case-insensitive, so the guard must be too (spec §3).
func TestForwardRejectsMaskedLiteral(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "s", Host: "10.0.0.1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a})
	mgr := NewTunnelManager()

	for _, rh := range []string{"hidden", "Hidden", "HIDDEN"} {
		_, err := ForwardForProfile(context.Background(), st, "proj", pid, a, rh, 8080, 0, mgr)
		if err == nil {
			t.Fatalf("remoteHost %q must be rejected", rh)
		}
		if !strings.Contains(err.Error(), "hidden") {
			t.Fatalf("error should name the masked literal: %v", err)
		}
	}
	if mgr, ok := any(mgr).(*TunnelManager); ok && len(mgr.tunnels) != 0 { // nothing leaked into the registry
		t.Fatal("no tunnel should be registered for a rejected forward")
	}
}
```

（`mgr.tunnels` 为同包私有字段，直接可查。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run TestForwardRejectsMaskedLiteral -v`
Expected: FAIL——现实现会把 "hidden" 当普通 remote_host 拨号（大概率 DNS 失败但报错文本不同），断言 `err == nil` 或不含 "hidden"。

- [ ] **Step 3: 实现守卫**

`ForwardForProfile` 中 iron-rule gate（`if !contains(allowed, serverID) {...}` 块）之后、`st.GetServer` 之前插：

```go
	// Plan 31: the masked-host literal must never be dialed. It is the one
	// channel where the agent could "use" the list_servers mask value — a
	// malicious resolver record for "hidden" server-side would capture the
	// mistyped traffic. DNS is case-insensitive, so is the comparison.
	if strings.EqualFold(remoteHost, "hidden") {
		status = "error"
		err = errors.New("remote_host \"hidden\" is the list_servers masked-host literal, not a real host — pass the actual host:port to forward to")
		return
	}
```

（`strings` 已在 core.go import。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcpserver/ -run TestForwardRejectsMaskedLiteral -v` → PASS；再 `go test ./internal/mcpserver/` 全量 PASS。

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/
git add internal/mcpserver/
git commit -m "feat(mcpserver): forward_port 拒绝 remoteHost==\"hidden\"(大小写不敏感)——掩码字面量误抄格杀

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: sshbroker——`redactAddr` 两步清洗 + `addrRedactedError` + `JoinHostPort` 修复

**Files:**
- Create: `internal/sshbroker/redact.go`
- Create: `internal/sshbroker/redact_test.go`
- Modify: `internal/sshbroker/client.go:23-61`（Connect）

**Interfaces:**
- Consumes: 无（独立于 store/mcpserver）
- Produces: `redactAddr(err error, host string, port int) error`（包私有）；`Connect` 错误文本自此不含 host/IP/host:port 组合（Task 6 依赖）；`addrRedactedError` 实现 `error`+`net.Error`+`Unwrap`

- [ ] **Step 1: 写失败测试（golden 输出断言为主）**

`internal/sshbroker/redact_test.go` 全文：

```go
package sshbroker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// redactGolden: every case pins the EXACT output text (golden), so the
// acceptance regex and the redaction detector can never circularly validate
// each other (spec §6 methodology). The negative checks are auxiliary only.
func TestRedactAddrGolden(t *testing.T) {
	const host = "vault.example.internal"
	const port = 22
	refused := &net.OpError{
		Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 22},
		Err:  &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ip refused degrades to classified phrase",
			refused, "ssh dial: connect failed: connection refused"},
		{"dns failure degrades to DNS phrase",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: host, Err: "no such host"}},
			"ssh dial: connect failed: DNS lookup failed"},
		{"wrapped handshake keeps cached ip and degrades (fmt.wrapError caching)",
			fmt.Errorf("ssh: handshake failed: %w", &net.OpError{
				Op: "read", Net: "tcp",
				Source: &net.TCPAddr{IP: net.ParseIP("10.0.0.9"), Port: 53210},
				Addr:   &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 22},
				Err:    &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET},
			}),
			"ssh dial: connect failed"},
		{"dns case+trailing-dot form degrades (lookup regex)",
			&net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("lookup Vault.Example.Internal.: no such host")},
			"ssh dial: connect failed: DNS lookup failed"},
		{"dns search-domain form degrades (host=foo, name=foo.corp.internal)",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: "foo.corp.internal", Err: "no such host"}},
			"ssh dial: connect failed: DNS lookup failed"},
		{"ipv6 zone form degrades",
			errors.New("read tcp fe80::1%eth0->10.0.0.5:22: connection reset by peer"),
			"ssh dial: connect failed"},
		{"malformed addr error (legacy Sprintf join) degrades",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Addr: "2001:db8::1:22", Err: "too many colons in address"}},
			"ssh dial: connect failed"},
		{"address-free text passes through untouched",
			errors.New("ssh: handshake failed: EOF"),
			"ssh dial: ssh: handshake failed: EOF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactAddr(c.err, host, port).Error()
			if got != c.want {
				t.Fatalf("golden mismatch:\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// Targeted-replacement cases (step 1 of the two-step design): the host token
// and host:port forms ARE redacted in place when they appear standalone —
// boundary-aware, so a dotted longer name sharing the prefix is NOT clobbered.
func TestRedactAddrTargeted(t *testing.T) {
	// Standalone host token → [REDACTED], rest of text intact.
	got := redactAddr(errors.New("connect to vault.example.internal refused"), "vault.example.internal", 22)
	if want := "ssh dial: connect to [REDACTED] refused"; got.Error() != want {
		t.Fatalf("token: got %q want %q", got.Error(), want)
	}
	// host:port form → [REDACTED].
	got = redactAddr(errors.New("dial vault.example.internal:22: refused"), "vault.example.internal", 22)
	if want := "ssh dial: dial [REDACTED]: refused"; got.Error() != want {
		t.Fatalf("host:port: got %q want %q", got.Error(), want)
	}
	// Case-insensitive host match.
	got = redactAddr(errors.New("connect to VAULT.EXAMPLE.INTERNAL refused"), "vault.example.internal", 22)
	if want := "ssh dial: connect to [REDACTED] refused"; got.Error() != want {
		t.Fatalf("case-insensitive: got %q want %q", got.Error(), want)
	}
	// Short host known trade-off: standalone "db" is redacted (fail-safe —
	// only diagnostic damage), surrounding words untouched.
	got = redactAddr(errors.New("postgres db cluster check failed"), "db", 22)
	if want := "ssh dial: postgres [REDACTED] cluster check failed"; got.Error() != want {
		t.Fatalf("short host: got %q want %q", got.Error(), want)
	}
	// A LONGER dotted name containing the host as dot-joined prefix is NOT
	// boundary-matched; the residual address form then triggers degradation.
	got = redactAddr(errors.New("dial tcp: lookup foo.corp.internal: no such host"), "foo", 22)
	if want := "ssh dial: connect failed: DNS lookup failed"; got.Error() != want {
		t.Fatalf("search-domain: got %q want %q", got.Error(), want)
	}
}

// The wrapper must preserve errors.Is (audit classification) and satisfy
// net.Error via delegation (spec §5; the delegation experiment proved both).
func TestRedactAddrWrapperContract(t *testing.T) {
	wrapped := redactAddr(fmt.Errorf("ssh: handshake failed: %w", ErrHostKeyMismatch), "h", 22)
	if !errors.Is(wrapped, ErrHostKeyMismatch) {
		t.Fatal("errors.Is must traverse Unwrap to ErrHostKeyMismatch")
	}
	inner := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ETIMEDOUT}}
	w2 := redactAddr(inner, "h", 22)
	var ne net.Error
	if !errors.As(w2, &ne) || !ne.Timeout() {
		t.Fatal("wrapper must delegate net.Error (Timeout)")
	}
	var oe *net.OpError
	if !errors.As(w2, &oe) {
		t.Fatal("errors.As must reach the original *net.OpError")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/sshbroker/ -run 'TestRedact' -v`
Expected: FAIL，`undefined: redactAddr`。

- [ ] **Step 3: 实现 `internal/sshbroker/redact.go` 全文**

```go
package sshbroker

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"syscall"
)

// addrRedactedError carries a sanitized Error() text over the ORIGINAL error
// chain. Invariants (spec §5):
//   - Unwrap() returns the original chain verbatim, because core.go's audit
//     classification does errors.Is(err, ErrHostKeyMismatch) / As(*net.OpError)
//     on Connect errors — the chain must stay walkable.
//   - ⚠ The unwrapped chain carries host/IP PLAINTEXT. No log, audit, or
//     persistence path may ever print the cause chain's own text — only
//     Error() (the sanitized message) may flow outward.
//   - net.Error is delegated (Timeout/Temporary) so present and future call
//     sites doing err.(net.Error) keep working.
type addrRedactedError struct {
	msg string
	err error
}

func (e *addrRedactedError) Error() string { return e.msg }
func (e *addrRedactedError) Unwrap() error { return e.err }
func (e *addrRedactedError) Timeout() bool {
	var ne net.Error
	return errors.As(e.err, &ne) && ne.Timeout()
}
func (e *addrRedactedError) Temporary() bool {
	var ne net.Error
	return errors.As(e.err, &ne) && ne.Temporary()
}

// Address-shape detectors for the degradation fallback (step 2). Deliberately
// narrow-but-composable: IPv4, bracketed IPv6, zone-suffixed IPv6, any "::"
// IPv6 form, dotted-host:port, and the "lookup <name>" DNS form. False
// positives only cost a degrade to a generic phrase (fail-safe direction).
var (
	ipv4Re     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	brackV6Re  = regexp.MustCompile(`\[[0-9a-fA-F:.]+%?\w*\]`)
	zoneV6Re   = regexp.MustCompile(`\b[0-9a-fA-F]{0,4}(?::[0-9a-fA-F]{0,4})+%\w+\b`)
	dblColonRe = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:)*::(?:[0-9a-fA-F]{1,4}:?)*\b`)
	hostPortRe = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)+:\d{1,5}\b`)
	lookupRe   = regexp.MustCompile(`(?i)\blookup\s+\S+`)
)

// hostBoundaryRe builds the boundary-aware matcher for one host form: the
// token must not be dot-joined into a longer name on either side (a plain \b
// is NOT a boundary for dotted names — "foo" would match inside
// "foo.corp.internal", leaking the suffix after replacement; spec §5 exp ⑤).
func hostBoundaryRe(form string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^0-9A-Za-z.\-])` + regexp.QuoteMeta(form) + `($|[^0-9A-Za-z.\-])`)
}

// degradedText is the FROZEN classification→phrase map (spec §5: frozen in
// the plan; the golden tests pin these literals as acceptance input). A
// DNS-shaped chain OR any "lookup <name>" form in the original text maps to
// the DNS phrase — the DNSError struct is not required (opaque wrappers like
// fmt.Errorf("lookup %s: ...") carry the same shape).
func degradedText(err error) string {
	var dnsErr *net.DNSError
	isDNS := errors.As(err, &dnsErr) || lookupRe.MatchString(err.Error())
	switch {
	case isDNS:
		return "ssh dial: connect failed: DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ssh dial: connect failed: connection refused"
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "ssh dial: connect failed: timed out"
		}
		return "ssh dial: connect failed"
	}
}

// redactAddr returns err with address information removed from its rendered
// text, preserving the chain (Unwrap) and net.Error (delegation). Two steps
// (spec §5):
//  1. targeted, boundary-aware replacement of the known host/addr forms;
//  2. degradation fallback: if ANY address shape or DNS form survives — or is
//     detected at all in a DNS-error chain — the whole text is replaced by a
//     classified generic phrase. Per-segment scrubbing was disproven by
//     experiment (regex misses zones/search-domains; ParseIP rejects zone and
//     host:port tokens; substring replace mangles short hosts).
func redactAddr(err error, host string, port int) error {
	msg := err.Error()

	// Step 1: targeted replacement, longest forms first.
	forms := []string{
		net.JoinHostPort(host, fmt.Sprintf("%d", port)), // host:port / [host]:port
		strings.TrimSuffix(host, "."),
		host,
	}
	seen := map[string]bool{}
	for _, f := range forms {
		if f == "" || seen[strings.ToLower(f)] {
			continue
		}
		seen[strings.ToLower(f)] = true
		msg = hostBoundaryRe(f).ReplaceAllString(msg, "$1[REDACTED]$2")
	}

	// Step 2: degradation fallback.
	var dnsErr *net.DNSError
	addressSurvives := ipv4Re.MatchString(msg) || brackV6Re.MatchString(msg) ||
		zoneV6Re.MatchString(msg) || dblColonRe.MatchString(msg) ||
		hostPortRe.MatchString(msg) || lookupRe.MatchString(msg)
	if addressSurvives || errors.As(err, &dnsErr) {
		return &addrRedactedError{msg: degradedText(err), err: err}
	}
	return &addrRedactedError{msg: msg, err: err}
}
```

- [ ] **Step 4: 改 `client.go` Connect（包装 + JoinHostPort 修复）**

4a. import 块加 `"strconv"`。
4b. `:36` `addr := fmt.Sprintf("%s:%d", host, port)` 改（顺带修 IPv6 存量 bug——`%s:%d` 拼出畸形地址连不上，spec §5）：

```go
	addr := net.JoinHostPort(host, strconv.Itoa(port))
```

（import 加 `"net"`。）
4c. `:49` 失败分支改：

```go
		if r.err != nil {
			// Plan 31: scrub the dialed address (and any resolved-IP / DNS
			// residue) from the error text AT THE SOURCE, so every consumer —
			// MCP tool errors included — is safe by construction. The chain
			// survives via Unwrap (errors.Is classification) and net.Error is
			// delegated; see redact.go for the invariants.
			return nil, fmt.Errorf("ssh dial: %w", redactAddr(r.err, host, port))
		}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/sshbroker/ -v`
Expected: 全 PASS（golden、targeted、wrapper 三组 + 既有 sshbroker 测试）。

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -w internal/sshbroker/redact.go && gofmt -l internal/
git add internal/sshbroker/
git commit -m "feat(sshbroker): Connect 错误源头清洗——redactAddr 两步(边界感知定向替换+地址/DNS 形态整体降级), addrRedactedError 保留 Unwrap+委托 net.Error, 顺带修 JoinHostPort IPv6 存量 bug

golden 断言钉死 9 类形态; 降级措辞映射表冻结于此。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: MCP 错误分支回归网——全分支文本无 host/IP

**Files:**
- Test: `internal/mcpserver/core_test.go`（追加测试）

**Interfaces:**
- Consumes: Task 3 掩码、Task 4 守卫、Task 5 `redactAddr`（Connect 错误已洗）
- Produces: 结构化承诺网——把 grilling「connect_error **等**」钉成「每个错误分支」的自动断言（spec §6）

- [ ] **Step 1: 写测试（含 hostkey_mismatch 真触发 + connect_error 真拨号）**

```go
// assertNoLeak: the regression net's shared check — error text must contain
// neither the vault host substring NOR any address-shape literal. The host
// substring alone is blind for hostname servers (the leak would be the
// RESOLVED ip); the regex alone would circularly share the detector. Both.
// (No "lookup" term here on purpose: the DEGRADED DNS phrase legitimately
// contains the word "lookup" — raw lookup forms are pinned by Task 5 golden.)
var leakAddrRe = regexp.MustCompile(`(\b\d{1,3}(?:\.\d{1,3}){3}\b|\[[0-9a-fA-F:.]+\]|::)`)

func assertNoLeak(t *testing.T, err error, host string) {
	t.Helper()
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), host) {
		t.Fatalf("error text leaks host %q: %q", host, err.Error())
	}
	if leakAddrRe.MatchString(err.Error()) {
		t.Fatalf("error text leaks an address shape: %q", err.Error())
	}
}

// TestErrorBranchesNeverLeakHost: every reachable error branch of the four
// *ForProfile operations must return text free of the vault host / address
// shapes (spec §6 — the structural form of the "connect_error etc." promise).
func TestErrorBranchesNeverLeakHost(t *testing.T) {
	const vh = "vault.example.internal"
	st := newStore(t)
	granted, _ := st.AddServer(&models.Server{Name: "g", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	nocred, _ := st.AddServer(&models.Server{Name: "n", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword}) // credential-less
	unreach, _ := st.AddServer(&models.Server{Name: "u", Host: "127.0.0.1", Port: 1, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{granted, nocred, unreach})
	mgr := NewTunnelManager()

	// denied (all four ops, out-of-profile id)
	_, err := ExecCommandForProfile(context.Background(), st, "proj", pid, "bogus-id", "true", false, time.Second)
	assertNoLeak(t, err, vh)
	_, err = DownloadForProfile(context.Background(), st, "proj", pid, "bogus-id", "/x")
	assertNoLeak(t, err, vh)
	_, err = UploadForProfile(context.Background(), st, "proj", pid, "bogus-id", "/x", "/y")
	assertNoLeak(t, err, vh)
	_, err = ForwardForProfile(context.Background(), st, "proj", pid, "bogus-id", "127.0.0.1", 80, 0, mgr)
	assertNoLeak(t, err, vh)

	// not found (id in profile list? no — use a granted-shaped bogus: the
	// "server not found" branch needs an id that IS granted but has no row;
	// craft by deleting after grant)
	gone, _ := st.AddServer(&models.Server{Name: "gone", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	_ = st.GrantServers(pid, []string{gone})
	_ = st.DeleteServerCascading(gone)
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, gone, "true", false, time.Second)
	assertNoLeak(t, err, vh)

	// no_credential
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, nocred, "true", false, time.Second)
	assertNoLeak(t, err, vh)

	// no_sudo (exec only)
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, granted, "true", true, time.Second)
	assertNoLeak(t, err, vh)

	// connect_error — real dial to a refused port; host is an IP here so BOTH
	// checks bite (substring + shape).
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, unreach, "true", false, 2*time.Second)
	assertNoLeak(t, err, "127.0.0.1")
	_, err = ForwardForProfile(context.Background(), st, "proj", pid, unreach, "10.255.255.1", 65001, 0, mgr)
	assertNoLeak(t, err, "127.0.0.1")

	// forward hidden guard (Task 4)
	_, err = ForwardForProfile(context.Background(), st, "proj", pid, granted, "hidden", 80, 0, mgr)
	assertNoLeak(t, err, vh)

	// close-forward not-found
	err = CloseForwardForProfile(context.Background(), st, "proj", "no-such-tunnel", mgr)
	assertNoLeak(t, err, vh)
}

// TestConnectErrorHostKeyMismatchNoLeak: the hostkey-mismatch branch surfaces
// THROUGH the connect error path (HostKeyTOFU's constructor never errors —
// core.go's standalone herr branch is dead code), so its text must be washed
// the same way. Pre-trust garbage bytes: TOFU compares marshaled key bytes,
// so any non-matching blob triggers the mismatch branch.
func TestConnectErrorHostKeyMismatchNoLeak(t *testing.T) {
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srv := &models.Server{
		Name: "mismatch", Host: addr[:indexByte(addr, ':')], Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st),
	}
	id, _ := st.AddServer(srv)
	_ = st.SaveHostKey(srv.Host, srv.Port, []byte("not-the-real-host-key"))
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{id})

	_, err := ExecCommandForProfile(context.Background(), st, "proj", pid, id, "true", false, 5*time.Second)
	if err == nil {
		t.Fatal("want hostkey mismatch error")
	}
	assertNoLeak(t, err, srv.Host)
}
```

import 增补：`regexp`（core_test.go 已有 `context`/`errors`/`strings`/`time`/`models`/`testsshd`）。

- [ ] **Step 2: 跑测试**

Run: `go test ./internal/mcpserver/ -run 'TestErrorBranchesNeverLeakHost|TestConnectErrorHostKeyMismatchNoLeak' -v`
Expected: PASS（Task 5 已洗 Connect 错误；hostkey mismatch 经 connect 路径浮现，mismatch 文本本身无地址，走透传分支）。

- [ ] **Step 3: 全量 + gofmt + commit**

```bash
go test ./internal/mcpserver/ && gofmt -l internal/
git add internal/mcpserver/core_test.go
git commit -m "test(mcpserver): 四工具全错误分支回归网——文本无 vault host/IP 形态(hostkey mismatch 真触发+connect_error 真拨号)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: CLI——`servers add/edit --expose-host`

**Files:**
- Modify: `internal/cli/servers.go:20-106`（add：var 块 + srv 构造 + flag）、`internal/cli/servers.go:177-237`（edit：var 块 + Changed 应用 + flag）
- Test: `internal/cli/servers_hint_test.go` 或新建 `internal/cli/servers_expose_test.go`

**Interfaces:**
- Consumes: Task 1 的 `models.Server.ExposeHost`
- Produces: owner 侧 CLI 开关（compat-matrix 迁移列引用的补救命令 `servers edit <name> --expose-host`）

- [ ] **Step 1: 写失败测试**

新建 `internal/cli/servers_expose_test.go`（复用 `servers_hint_test.go` 的 harness：`newHintEnv` 钉隔离 vault、`runCaptured` 执行 cobra、事后 `store.Open` 检查落库）：

```go
package cli

import (
	"testing"

	"ssh-manager-mcp/internal/store"
)

// Plan 31: --expose-host persists the opt-in bit on add; edit toggles it
// (bare = on, --expose-host=false = off, omitted = keep current).
func TestServersExposeHostFlags(t *testing.T) {
	dbPath, mk := newHintEnv(t)

	runCaptured(t, "servers", "add", "--name", "t1", "--host", "h", "--user", "u", "--expose-host")
	runCaptured(t, "servers", "add", "--name", "t2", "--host", "h", "--user", "u")

	check := func(t *testing.T, name string, want bool) {
		t.Helper()
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		srv, _ := st.GetServerByName(name)
		if srv == nil {
			t.Fatalf("server %s missing", name)
		}
		if srv.ExposeHost != want {
			t.Fatalf("%s ExposeHost = %v, want %v", name, srv.ExposeHost, want)
		}
	}

	check(t, "t1", true)  // add --expose-host
	check(t, "t2", false) // add default

	runCaptured(t, "servers", "edit", "t2", "--expose-host")
	check(t, "t2", true) // edit bare = on

	runCaptured(t, "servers", "edit", "t2", "--expose-host=false")
	check(t, "t2", false) // edit explicit off

	runCaptured(t, "servers", "edit", "t2", "--role", "x")
	check(t, "t2", false) // edit without the flag keeps current (false)

	runCaptured(t, "servers", "edit", "t1", "--role", "x")
	check(t, "t1", true) // edit without the flag keeps current (true)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestServersExposeHostFlags -v`
Expected: FAIL——`unknown flag: --expose-host`。

- [ ] **Step 3: 实现**

3a. `serversAddCmd` var 块加 `var exposeHost bool`；srv 构造（:44-52）加字段 `ExposeHost: exposeHost,`；flag 注册（:87-101 区域末尾）加：

```go
	c.Flags().BoolVar(&exposeHost, "expose-host", false, "return the plaintext host in list_servers (default: masked as \"hidden\")")
```

3b. `serversEditCmd` var 块加 `exposeHost bool`；Changed 应用区（:205-237，`special-handling` 块后）加：

```go
			if cmd.Flags().Changed("expose-host") {
				srv.ExposeHost = exposeHost
			}
```

flag 注册加（与 add 同句）。pflag BoolVar 默认 `NoOptDefVal="true"`——裸 `--expose-host` = 开、`--expose-host=false` = 显式关，`Changed()` 惯例与现有 edit 字段一致。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -v`
Expected: 全 PASS。

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/
git add internal/cli/
git commit -m "feat(cli): servers add/edit --expose-host(owner 侧 opt-in 开关, 默认掩码)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: TUI——draft/prefill/toParts 带过 + 字段选择器布尔项 + 详情页 + importflow

**Files:**
- Modify: `internal/tui/forms.go:17-26`（serverDraft）、`internal/tui/forms.go:201-209`（prefill）、`internal/tui/forms.go:216-242`（toParts）
- Modify: `internal/tui/editfields.go:23-68`（editFields + 计数注释）、`internal/tui/editfields.go:145-163` 后（新 bool 字段构造器）、`internal/tui/editfields.go:165-185`（snapshotDraft）
- Modify: `internal/tui/servers.go:156`（Detail）、`internal/tui/clientpage.go:492-494`（clientServerDetail）
- Modify: `internal/tui/importflow.go:321-329`（submitSupplement）
- Test: `internal/tui/forms_test.go`、`internal/tui/editfields_test.go`、`internal/tui/editpage_test.go`

**Interfaces:**
- Consumes: Task 1 的 `models.Server.ExposeHost`
- Produces: TUI 编辑面完整开关；`serverDraft.ExposeHost`（editfields/snapshotDraft 依赖）

- [ ] **Step 1: 写失败测试（头号失败模式的自动防线 + 字段表锁同步）**

追加到 `internal/tui/forms_test.go`：

```go
// TestPrefillToPartsPreserveExposeHost: THE silent-reset guard (spec §4 —
// "头号失败模式"). updateServerTx writes the FULL row and toParts builds a
// FRESH models.Server, so both copy points must carry the bit or editing ANY
// other field silently flips an owner opt-in back to false.
func TestPrefillToPartsPreserveExposeHost(t *testing.T) {
	cur := &models.Server{
		Name: "n", Host: "h", Port: 22, User: "u",
		Description: "d", ExposeHost: true,
	}
	d := prefill(cur)
	if !d.ExposeHost {
		t.Fatal("prefill dropped ExposeHost")
	}
	srv, _, _, err := d.toParts()
	if err != nil {
		t.Fatal(err)
	}
	if !srv.ExposeHost {
		t.Fatal("toParts dropped ExposeHost")
	}
	// Round-trip through the edit-field Set (the picker persists via the
	// same draft): untoggle then retoggle.
	f := exposeHostEditField()
	f.Set(d, "false")
	if d.ExposeHost {
		t.Fatal("Set(\"false\") must clear")
	}
	f.Set(d, "true")
	if !d.ExposeHost {
		t.Fatal("Set(\"true\") must set")
	}
}
```

`internal/tui/editfields_test.go`——`TestEditFieldsKeysMatchSnapshot`（:97-98 锁「恰好 15 键」）改为 16 键并在 `snapshotDraft` 断言里加 `"exposehost": strconv.FormatBool(d.ExposeHost),` 的对应；:31/:37 的字段计数注释「编辑态 15 项」→「16 项」（新增态 14 不变——新字段仅编辑态，见 Step 3）。`editpage_test.go:135` 的字段标题清单在 `"清除凭据"` 后插 `"暴露 Host"`；**:199 的下标 8 不用动**（新字段插在 clearCredential 之后，clearcredential 仍是 8）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestPrefillToPartsPreserveExposeHost|TestEditFieldsKeysMatchSnapshot' -v`
Expected: FAIL——`d.ExposeHost undefined`、15 键断言红。

- [ ] **Step 3: 实现（6 处）**

3a. `serverDraft`（forms.go:17-26）`ClearCredential` 块后加：

```go
	// ExposeHost (Plan 31): owner opt-in for plaintext host in list_servers.
	// Carried through prefill/toParts — dropping it here silently resets the
	// bit on every TUI edit (spec §4's #1 failure mode; guarded by
	// TestPrefillToPartsPreserveExposeHost).
	ExposeHost bool
```

3b. `prefill`（forms.go:203-209）加 `ExposeHost: cur.ExposeHost,`。
3c. `toParts`（forms.go:220-226）srv 构造加 `ExposeHost: d.ExposeHost,`。
3d. `editfields.go`——`editFields` 的 editing 分支（:56-58）在 `clearCredentialEditField()` 后追加新字段：

```go
	if editing {
		fields = append(fields, clearCredentialEditField(), exposeHostEditField())
	}
```

（:23-27 的顺序注释同步：`……清除凭据(编辑态)/暴露 Host(编辑态)/硬件/……——编辑态 16 项，新增态 14 项`。）新构造器放 `clearCredentialEditField` 定义后：

```go
// exposeHostEditField: edit-mode-only bool (add mode defaults to false; flip
// it here after creation). Same Confirm pattern as clearCredentialEditField —
// Set accepts the canonical "true"/"已勾选" forms so snapshotDraft round-trips.
func exposeHostEditField() editField {
	return editField{
		Key:     "exposehost",
		Label:   "暴露 Host",
		Confirm: true,
		Get: func(d *serverDraft) string {
			if d.ExposeHost {
				return "已勾选"
			}
			return "未勾选"
		},
		Set: func(d *serverDraft, v string) { d.ExposeHost = v == "true" || v == "已勾选" },
		Build: func(d *serverDraft) *huh.Form {
			return huh.NewForm(huh.NewGroup(huh.NewConfirm().
				Title("暴露 Host 给 agent（list_servers 返回明文；默认隐藏为 \"hidden\"）").Value(&d.ExposeHost).
				Affirmative("暴露").Negative("隐藏")))
		},
	}
}
```

`snapshotDraft`（:167-185）加一行：`"exposehost": strconv.FormatBool(d.ExposeHost),`。

3e. 详情页两处——`internal/tui/servers.go` Detail 的格式串（:156）在 `Caveats %s` 行后加 `"\n暴露Host %s"`，实参按 `exposeLabel(srv.ExposeHost)` 传；`internal/tui/clientpage.go`（:492-494）同型。helper（放 servers.go）：

```go
// exposeLabel renders the owner-facing host-exposure state line.
func exposeLabel(on bool) string {
	if on {
		return "已暴露（agent 可见明文）"
	}
	return "隐藏（agent 看到 \"hidden\"）"
}
```

3f. `importflow.go` submitSupplement（:321-329）构造处加 `ExposeHost: f.srv.ExposeHost,`。

- [ ] **Step 4: 跑测试确认通过（TUI 全量）**

Run: `go test ./internal/tui/ -v`
Expected: 全 PASS（含 editpage/editfields 既有测试——计数/标题清单已同步）。TUI 套件较慢（~89s，backlog #10 已知），耐心等完。

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -l internal/
git add internal/tui/
git commit -m "feat(tui): 服务器编辑面 expose_host——draft/prefill/toParts 带过(静默复位防线)+字段选择器布尔项+详情页+importflow

snapshotDraft 16 键; 编辑态 16 项; 下标 8 不变(新字段插后位)。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: eval——seedBroker 覆盖暴露态

**Files:**
- Modify: `internal/eval/broker.go:107-113`（seed 构造）
- Test: 既有 `internal/eval/broker_test.go`（零改动，跑绿即可）

**Interfaces:**
- Consumes: Task 1 的 `models.Server.ExposeHost`
- Produces: eval e2e 覆盖 expose=true 态（`broker_test.go:109` 是 owner 侧 `st.ListServers()` 断言，store 层永掩码，本就与投影无关——加 true 的唯一理由是 e2e 覆盖暴露态，spec §6）

- [ ] **Step 1: 改 seed 构造**

`broker.go:108-113` 的 `models.Server{...}` 加一行：

```go
			ExposeHost:       true, // e2e coverage of the exposed state (spec §6)
```

- [ ] **Step 2: 跑 eval（需要 Docker；无 Docker 环境则注明跳过理由并留给 CI）**

Run: `go test ./internal/eval/ -run TestWireBroker -v`
Expected: PASS（:109 断言读 owner store，不受投影影响）。再全量 `go test ./internal/eval/`。

- [ ] **Step 3: commit**

```bash
git add internal/eval/broker.go
git commit -m "test(eval): seedBroker 设 ExposeHost=true——e2e 覆盖暴露态(broker_test:109 属 owner-store 断言, 与投影无关)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: docs 七处 + 全仓终验

**Files:**
- Modify: `docs/threat-model.md`、`docs/concepts.md`、`docs/compat-matrix.md`、`docs/agent-tools.md`、`README.md`、`docs/agent-access.md`、`docs/managing-servers.md`

**Interfaces:**
- Consumes: 前九个任务的最终行为
- Produces: v0.9 发布文档面（承诺边界 / 破坏性变更登记 / agent & owner 指南）

- [ ] **Step 1: threat-model.md 新节（插在 §3 残留风险之后、§4 之前）**

```markdown
---

## 3.5 agent 可见性承诺边界（v0.9 起）

「接口级不暴露」的准确边界（Plan 31，backlog #12）：

- **承诺**：ssh-manager 的 MCP 接口默认不披露 vault 内 host:port 与凭据——`list_servers` 默认回 `"hidden"`（owner 可按服务器 `expose_host=true` 显式放开，host:port 组合口径——孤立端口号不构成披露）；工具错误文本不含 host / IP 字面量 / host:port 组合。
- **不算违约**：agent 在服务器上主动执行 `ip addr` / `hostname` 等命令探出的地址。
- **明确不防的运行时逃逸**：agent 调用本机 ssh-manager owner CLI（`projects ls` 明文打印 `user@host:port`）；agent 读到离线 client 上的 cache.bin（整仓快照，设计上含全部 host 明文）——这两类属「agent 宿主已被完全信任」范畴，见上方 (d) 类定级。
- **明确不做**：运行时级隐藏（命令过滤 / 输出脱敏 / 网络盲化）与服务器出网管控（backlog 不做清单）。
```

- [ ] **Step 2: concepts.md 类比表后加小节**（「设备码的两种输入形态」节之前）

```markdown
## agent 看得见什么（可见性边界）

project token 背后的 agent 通过 `list_servers` 看到：服务器元数据（name/role/services/caveats/location/hardware/tags/description/user/has_sudo）+ **可选的 host**——默认是字面量 `"hidden"`，owner 逐台用 `expose_host` 放开才有明文；**永远看不到**凭据与端口。工具错误文本同样不含主机地址（连接失败时给分类原因，不给 host:port）。这是「接口级不暴露」承诺的全部边界：agent 在服务器上跑 `ip addr` 探出的地址不算违约，本机 owner CLI / cache.bin 的明文也不在本承诺防护范围（见 threat-model.md §3.5）。
```

- [ ] **Step 3: compat-matrix.md 破坏性变更表（:20-24 表）追加行**

```markdown
| v0.9.0 | `list_servers` host 默认掩码为 `"hidden"`（per-server `expose_host` opt-in）；工具错误文本清洗（不含 host/IP/host:port） | 依赖 host 明文的 agent 流程当场断；v0.9 serve + 未升级 client 的离线模式仍回明文；旧 binary 导入新快照丢 `expose_host=true` 偏好（fail-safe：折回掩码） | 顺序按铁律 client 先、serve 后（技术上无硬约束；该顺序服务于「掩码尽快全生效」——在线随 serve 升级即刻生效、离线需 client 升级）。**依赖 host 明文的流程唯一补救 = 升级前 `ssh-manager servers edit <name> --expose-host`** |
```

- [ ] **Step 4: agent-tools.md 三处**

- `:48` 字段表 host 行改：`` | `host` / `user` | SSH 用户；host 默认是 `"hidden"`（owner 未披露，用 id 寻址；owner 逐台放开后才是明文） | ``
- `:207` 字段清单行同步（清单里 `host` 后加括注）。
- 错误表（:148 起）加一行：`` | `ssh dial: connect failed: connection refused`（等分类短语） | 连不上目标机（地址细节已按可见性边界清洗） | 核对 server_id 是否正确；网络问题报告 owner | ``

- [ ] **Step 5: README.md:42 表 + callout**

- "What the agent gets" 表的 host 项改 `"hidden"（默认；owner 逐台放开）`。
- 表下方循 v0.4.0 callout 先例加：`> **v0.9.0 破坏性变更**：list_servers 的 host 默认掩码为 "hidden"（`servers edit <name> --expose-host` 逐台放开）；错误文本不再包含主机地址。详见 docs/compat-matrix.md。`

- [ ] **Step 6: agent-access.md:100 验证句同步**

`返回…id / name / host / user / has_sudo…` 改为 `返回…id / name / host（默认 "hidden"，owner 逐台 opt-in）/ user / has_sudo…`。

- [ ] **Step 7: managing-servers.md 两处**

- add flag 块（:44-61）与 edit flag 全表（:193-209）各加一行：`--expose-host`（bool，默认 false；`servers add` 与 `servers edit` 均可用，edit 裸用=开、`--expose-host=false`=关）。
- 心智模型节（:13 附近）加一句：**避免给真实主机命名 `hidden`**——该字面量是 list_servers 的掩码哨兵，撞名后投影不可区分且 forward_port 会拒绝。

- [ ] **Step 8: 全仓终验**

```bash
gofmt -l internal/          # 空
go vet ./...
go test ./...               # 全绿（Windows 本机；conformance 不需要 Docker 的部分全跑）
```

- [ ] **Step 9: commit**

```bash
git add docs/ README.md
git commit -m "docs: Plan 31 可见性承诺边界落地——threat-model §3.5/concepts/compat-matrix v0.9 破坏性变更登记/agent-tools/README/agent-access/managing-servers

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## 收尾（SDD 之外，owner 手工）

- 发版 v0.9.0（tag + GoReleaser ldflags 注入，惯例流程）。
- 手工双端验证（spec §6）：NUC10 serve 升 v0.9 + 笔记本 ①在线全 `"hidden"` ②connect_error 无 host ③`--expose-host` 放开一台回明文 ④cache pull 后离线态与 expose 状态一致。验证后 compat-matrix 已验证组合表回写一行（惯例）。
