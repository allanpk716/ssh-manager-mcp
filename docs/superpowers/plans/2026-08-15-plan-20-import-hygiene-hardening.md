# Plan 20 实施计划：ssh-config 批量导入 + 无凭据模型 + 卫生票 + 安全加固

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 从 spec v2（`docs/superpowers/specs/2026-08-15-plan-20-import-hygiene-hardening-design.md.rev1.md`，commit 09f29d0）落地 14 个任务：`~/.ssh/config` 批量导入全链路（含无凭据 server 模型）、Plan 17-19 卫生票、仓库级安全加固（孤儿凭据级联/token env 通道/版本真值/half-close/HostKeyAlgorithms 旋钮）。

**Architecture:** 三条 stream、两处硬顺序（T1→T11：`clientops.SplitTokenPin` 导出先于 env 通道消费；T5→T6：无凭据 schema 迁移先于 store 事务层改造，两者同改 store 层必须串行）。导入链 = `internal/importer`（纯解析）→ CLI `servers import`（事务原子 + 批内 key 去重）→ TUI 补全循环（表单构造器同源复用）。

**Tech Stack:** Go 1.25（charm v2 工具链 floor）、`github.com/kevinburke/ssh_config` v1.6.0（新依赖，模块缓存已有）、SQLite（`mattn`/`modernc` 现有栈）、cobra、huh/bubbletea v2。

## Global Constraints

- 工具链 Go 1.25（CI 四处 pin 已是 1.25；`GOTOOLCHAIN=auto`）。
- 仓库为 PUBLIC：**任何代码/文档/测试不得出现真实 token、密码、指纹**。
- 每任务 TDD：先写失败测试再实现；每步后 `go build ./... && go vet ./...` 干净。
- 提交信息风格：`feat:`/`fix:`/`refactor:`/`test:`/`docs:` + 英文一行。
- 测试不允许触碰生产固定路径——所有 store 相关测试必须用 `SSHMGR_STORE`/`SSHMGR_FILEKEY_PATH`/`SSHMGR_CACHE_DEK` env seam（Plan 19 T7 事故教训）。
- 主 worktree 共享（多 agent 并发）：实施在 isolated linked worktree 进行（SDD controller 负责）。
- 既有测试不可弱化：删除/改写断言必须在任务报告里逐条说明理由。

---

### Task 1 (A1): 死代码/孪生清理 + `SplitTokenPin` 导出

**Files:**
- Modify: `internal/tui/mode.go`（删 `DetectMode`/`DetectModeWith`，:41/:62）
- Modify: `internal/tui/mode_test.go`（删对应测试；保留其余）
- Modify: `internal/tui/clientpage.go`（删 `client.timeout` 字段、`syncCmd`（仅测试引用））
- Modify: `internal/clientops/pin.go`（`stripEmbeddedPin` → 导出 `SplitTokenPin`）
- Modify: `internal/cli/cache.go`（删本地孪生 `stripEmbeddedPin` :41，改调 `clientops.SplitTokenPin`）
- Modify: `go.mod`/`go.sum`（`go mod tidy` 清 lipgloss `//indirect`）
- Test: `internal/tui/app_test.go`（新增 dispatch 表驱动测试）

**Interfaces:**
- Produces: `clientops.SplitTokenPin(token string) (code string, pin string, ok bool)` —— T11（env token）与 cache.go 现有两处调用点共用。语义与现 `stripEmbeddedPin` 完全一致（首个冒号分割、`ParsePin` 校验、失败原样返回 ok=false）。

- [ ] **Step 1: 删除 DetectMode 死代码（编译器引导）**

```bash
cd <worktree>
# 生产代码已无调用（Plan 19 后仅 mode_test.go 引用）。删除前先验证：
grep -rn "DetectMode" --include="*.go" internal/ | grep -v mode_test.go   # 期望: 仅 mode.go 定义处
```

删 `internal/tui/mode.go` 中 `DetectModeWith`（:41-60 附近）与 `DetectMode`（:62 附近）两个函数及其注释；删 `mode_test.go` 中 `TestDetectMode_ForceWins`、`TestDetectMode_Auto` 及引用 `DetectMode("")` 的用例（:82-116 段内相关断言）。保留文件其余部分。

- [ ] **Step 2: 删除 client.timeout / syncCmd 死代码**

```bash
grep -n "timeout" internal/tui/clientpage.go | head    # client struct 的 timeout 字段（无生产读写）
grep -n "syncCmd" internal/tui/clientpage.go internal/tui/*_test.go
```

删 `clientModel.timeout` 字段；删 `syncCmd` 函数（若测试引用它，把测试改为用 `syncCmdMode`——若测试语义依赖 syncCmd 本体则连同测试一起删，理由写进报告）。

- [ ] **Step 3: 导出 SplitTokenPin（先改测试锁定语义）**

`internal/clientops/pin_test.go` 已有 stripEmbeddedPin 测试——把断言目标改名为 `SplitTokenPin`：

```go
func TestSplitTokenPin(t *testing.T) {
	// 原 stripEmbeddedPin 用例逐条迁移，断言不变：
	// "<code>:sha256:<64hex>" → (code, pin, true)
	// "barecode" → ("barecode", "", false)
	// "<code>:notapin" → 原样返回 ok=false
}
```

然后 `internal/clientops/pin.go`：`func stripEmbeddedPin(...)` → `func SplitTokenPin(...)`，注释补一行「Plan 20: exported as the single split point (cli/cache.go twin deleted)」。

- [ ] **Step 4: 删 cli/cache.go 孪生并改调用**

删 `internal/cli/cache.go:31-48` 的 `stripEmbeddedPin`；`:107` 调用点改：

```go
if c, _, ok := clientops.SplitTokenPin(token); ok {
```

- [ ] **Step 5: dispatch 表驱动测试**

`internal/tui/app_test.go` 新增（覆盖 Plan 18 T6 欠账——按键 → 页签 → 动作映射）：

```go
func TestServersPageDispatch(t *testing.T) {
	cases := []struct{ key string; wantOverlay string }{
		{"a", "新增服务器"}, {"e", "编辑服务器"}, {"i", "导入"}, // i 在 T10 落地前先不进表
	}
	_ = cases // T1 阶段先锁 a/e/d/g：构造 App + serversPage + 按 a/e/g 断言 overlay 标题 / 动作 cmd 非空
}
```

实现方式：把 `app.go` servers 页 key dispatch 的 switch 抽成 `func (a *App) serversKey(k tea.Key) tea.Cmd`（纯分派，无副作用），测试对每个 key 断言返回的 cmd/overlay 状态。若重构动到生产路径，保持行为逐字节等价（现有测试全绿为准）。

- [ ] **Step 6: go mod tidy + 全量验证 + 提交**

```bash
go mod tidy && go build ./... && go vet ./... && go test ./internal/tui/... ./internal/clientops/... ./internal/cli/...
git add -A && git commit -m "refactor: drop DetectMode/syncCmd dead code, export clientops.SplitTokenPin"
```

---

### Task 2 (A2): 错误分类精确化 + UNIQUE 本地化 + URL trim

**Files:**
- Modify: `internal/tui/clientpage.go:340`（classifyPullError 401 精确匹配）
- Test: `internal/tui/clientpage_test.go`
- Modify: `internal/store/servers.go`（AddServer UNIQUE 本地化）、`internal/store/profiles.go`（AddProfile 同）
- Test: `internal/store/servers_test.go`
- Modify: `internal/tui/wizardserve.go` 或 tokenview（serve URL TrimSpace）

**Interfaces:** 无新接口（行为修正）。

- [ ] **Step 1: 失败测试——401 精确匹配**

```go
func TestClassifyPullError401(t *testing.T) {
	// 正例：真 401
	if got := classifyPullError(fmt.Errorf("Get \"...snapshot\": server returned 401 Unauthorized")); got != authErr401类 {
		t.Fatalf("真 401 未分类: %q", got)
	}
	// 负例：指纹 hex 含 "401" 子串 / 端口 4010
	for _, s := range []string{"dial tcp: 1401 connection refused", "pin sha256:aa4011... mismatch"} {
		if got := classifyPullError(errors.New(s)); got == authErr401类 {
			t.Fatalf("非 401 误分类: %q", s)
		}
	}
}
```

（`authErr401类` = 现有分类返回值/文案常量，按 clientpage.go 实际分支名取。）

- [ ] **Step 2: 改实现**

`clientpage.go:340`：

```go
case strings.Contains(s, "server returned 401"), strings.Contains(s, "authorization"):
```

（裸 `"401"` 改为带前缀的精确串；hex 无空格与小写字母 r/e/t/u/n 组合，撞不上；`4010`/`1401` 也不再命中。）

- [ ] **Step 3: UNIQUE 本地化（先测后改）**

`internal/store/servers_test.go`：

```go
func TestAddServerDuplicateName(t *testing.T) {
	st := testStore(t) // 既有 helper
	must(t, st.AddServer(&models.Server{Name: "gpu", Host: "h", User: "u", CredentialID: "c1"}))
	_, err := st.AddServer(&models.Server{Name: "gpu", Host: "h2", User: "u", CredentialID: "c1"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want localized duplicate-name error, got %v", err)
	}
}
```

`AddServer` 末尾 wrap：

```go
if err != nil {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: servers.name") {
		return "", fmt.Errorf("server name %q already exists", srv.Name)
	}
	return "", err
}
```

`AddProfile` 同款（`profiles.name` → `profile name %q already exists`）。

- [ ] **Step 4: serve URL TrimSpace + 全量 + 提交**

tokenview/接入卡的 URL hint 组装处对 `d.ServeURL` 先 `strings.TrimSpace`（Plan 18 T7 M1）。`go test ./internal/tui/... ./internal/store/...` 全绿后：

```bash
git add -A && git commit -m "fix: precise 401 classification, localize UNIQUE errors, trim serve URL"
```

---

### Task 3 (A3): 向导 saveErr 可见 + role.json 并存测试 + GrantServers 预检

**Files:**
- Modify: `internal/tui/wizard.go`（vaultErr 视图补 saveErr 横幅）
- Test: `internal/tui/wizard_test.go`、`internal/roles/roles_test.go`、`internal/store/profiles_test.go`
- Modify: `internal/store/profiles.go:63`（GrantServers 事务内预检）

**Interfaces:** 无新接口。

- [ ] **Step 1: wizard vaultErr 视图显示 saveErr**

现状：`wizard.go:756`/`:795` 两处渲染 `saveErr` 横幅，但 `stepVaultErr`（vault 初始化失败）视图没有。在该视图的 render 函数里加同款：

```go
if w.saveErr != nil {
	b.WriteString("\n" + errStyle.Render(fmt.Sprintf("⚠ role.json 写入失败：%v", w.saveErr)) + "\n")
}
```

测试：构造 `saveErr != nil` + vaultErr step 的 model，`View()` 输出含「role.json 写入失败」。

- [ ] **Step 2: role.json 两处并存测试**

`internal/roles/roles_test.go` 新增：

```go
func TestRolePathPrecedenceVaultOverUserDir(t *testing.T) {
	// vault 角色文件（SSHMGR_STORE 指向的 vault 目录）与用户目录 role.json 同时存在
	// → RolePath/ResolveMode 必须取 vault 一侧（生产=VaultDir 等价，见 roles.go 注释）
}
```

用 env seam 分别布置两处 `role.json`（内容不同 role），断言 `ResolveMode` 结果与 vault 侧一致。**先跑一遍**——若现状并非 vault 优先，这是发现真 bug，按 vault 优先修（生产路径 `vaultRolePath` 从 store path 目录推导）。

- [ ] **Step 3: GrantServers 预检 fail-fast（先测后改）**

`internal/store/profiles_test.go`：

```go
func TestGrantServersUnknownIDFailsFast(t *testing.T) {
	st := testStore(t)
	pid, _ := st.AddProfile(&models.Profile{Name: "p"})
	err := st.GrantServers(pid, []string{"no-such-server"})
	if err == nil || !strings.Contains(err.Error(), "no-such-server") {
		t.Fatalf("want fail-fast unknown server error, got %v", err)
	}
	// 且 profile 仍存在（未产生孤儿空 profile 的前提是事务整体未半途污染）
}
```

`GrantServers` 事务开头加预检（替代逐条撞 FK）：

```go
for _, sid := range serverIDs {
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM servers WHERE id=?`, sid).Scan(&one); err == sql.ErrNoRows {
		return fmt.Errorf("server %s not found (grant aborted, nothing changed)", sid)
	}
}
```

- [ ] **Step 4: 全量 + 提交**

```bash
go test ./internal/tui/... ./internal/roles/... ./internal/store/...
git add -A && git commit -m "fix: wizard saveErr on vaultErr view, role.json precedence test, GrantServers fail-fast"
```

---

### Task 4 (A4): cert-before-mint + isTTY 修复 + 文档措辞

**Files:**
- Modify: `internal/cli/cache_tokens.go:36-44`（mint 前先 LoadOrCreateServeCert）
- Modify: `internal/tui/app.go:300`、`internal/tui/upgrade.go:136`（同序调整）
- Modify: `internal/tui/mode.go:155`（isTTY 用 GetConsoleMode）
- Modify: `docs/multi-machine.md`（:536 auto-pull 过度承诺、「每 30 分钟」→逐调用懒查）、`docs/quickstart-multi-machine.md`（同款措辞 + spawn-pull 缺文件分支）

**Interfaces:** 无新接口。

- [ ] **Step 1: cert-before-mint（三处同序）**

`cache_tokens.go` RunE 内：把 `mcpserver.LoadOrCreateServeCert()`（现 :40）**移到** `s.AddCacheToken(name)`（现 :36）**之前**——证书加载/生成失败时不再铸出孤儿设备码。TUI 两处（app.go:300 接入卡前、upgrade.go:136）同款重排。每处补一行注释：

```go
// cert first: a failing cert load must not mint an orphan device code (Plan 20 A4)
```

- [ ] **Step 2: isTTY 修复（先写复现测试的思路注记）**

`mode.go:155` 现实现按「stdin 是字符设备」判定——`tui < NUL` 时 NUL 也是字符设备 → 骗过 → 挂起。改 Windows 真终端判定（新文件 `internal/tui/istty_windows.go` + `istty_other.go`）：

```go
//go:build windows
package tui

import "syscall"

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var procGetConsoleMode = kernel32.NewProc("GetConsoleMode")

// isTerminal reports whether fd is an actual console (not NUL or any other
// character device). GetConsoleMode succeeds only for real consoles.
func isTerminal(fd uintptr) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode)))
	return r != 0
}
```

（unix 版：`syscall.Termios` ioctl 或维持现有 `os.Stat` ModeCharDevice 判定——NUL 问题仅 Windows。）`mode.go` 的 `isTTY()` 改为 `isTerminal(syscall.Handle(os.Stdin.Fd()))`（windows）。

验证（无法自动化真终端，本步以「`tui < NUL` 立即报错退出」为手工验收注记 + 单测 `isTerminal` 对 NUL 文件句柄返回 false）：

```go
func TestIsTerminalNUL(t *testing.T) {
	f, _ := os.Open(os.DevNull)
	defer f.Close()
	if isTerminal(f.Fd()) {
		t.Fatal("NUL must not be treated as a terminal")
	}
}
```

- [ ] **Step 3: 文档三处措辞**

- `multi-machine.md:536`（TLS 迁移 step 5）：删「自动拉取」过度承诺，改为「迁移机缺 cred 或持旧 pin，需手动带 `--pin` 重拉一次」。
- 两份 multi-machine 文档：「每 30 分钟自动拉新」→「运行中的会话在**每次工具调用前**懒检查（空闲会话不刷新）」。
- 补 spawn-pull 缺 cache.bin 分支一句：无 cache.bin 且 `--cache-max-age>0` 且有 cred 时视为无限旧必拉；拉失败则按原有「首次 pull 必须在线手动」报错。

- [ ] **Step 4: 全量 + 提交**

```bash
go build ./... && go vet ./... && go test ./...
git add -A && git commit -m "fix: load serve cert before token mint, GetConsoleMode isTTY, docs wording"
```

---

### Task 5 (C0): 无凭据 server 模型

**Files:**
- Modify: `internal/store/store.go`（migrate 表重建 + initSchema 新建表定义）
- Modify: `internal/store/servers.go`（scanServer NullString）
- Modify: `internal/vault/vault.go:113`（AuthForServer → ErrNoCredential）
- Modify: `internal/mcpserver/core.go`（exec 状态 `no_credential`）
- Modify: `internal/cli/servers.go`（add 放开凭据必填）
- Modify: `internal/tui/forms.go:282`（submitServer 同）
- Test: `internal/store/migrate_nullable_test.go`（新）、`internal/vault/vault_test.go`、`internal/mcpserver/core_test.go`

**Interfaces:**
- Produces: `vault.ErrNoCredential`（sentinel error）；server 行 `CredentialID=="" ⇔ AuthMethod==""` 的无凭据形态（T8/T10 依赖）。

- [ ] **Step 1: 失败测试——旧库迁移 + 无凭据 CRUD**

`internal/store/migrate_nullable_test.go`：

```go
func TestMigrateCredentialIDNullable(t *testing.T) {
	dbPath := t.TempDir() + "/store.db"
	// 手工建一个 v0.6 形态的库：credential_id TEXT NOT NULL + FK，插一台有凭据的 server
	seedLegacyDB(t, dbPath)
	// 打开（触发 migrate）
	st := openStoreAt(t, dbPath)
	defer st.Close()
	// 旧数据完好
	srv, err := st.GetServerByName("gpu")
	if err != nil || srv == nil || srv.CredentialID == "" {
		t.Fatalf("legacy server lost: %v %v", srv, err)
	}
	// 新增无凭据 server 成功
	_, err = st.AddServer(&models.Server{Name: "bare", Host: "h", Port: 22, User: "u"})
	if err != nil {
		t.Fatalf("credential-less insert failed: %v", err)
	}
	got, _ := st.GetServerByName("bare")
	if got.CredentialID != "" || got.AuthMethod != "" {
		t.Fatalf("want empty credential fields, got %+v", got)
	}
	// ListServers 两者都出
	all, _ := st.ListServers()
	if len(all) != 2 {
		t.Fatalf("want 2 servers, got %d", len(all))
	}
}
```

- [ ] **Step 2: 迁移实现（表重建）**

SQLite 无法 ALTER 放宽 NOT NULL → 守卫式表重建。`migrate()` 末尾追加（`addColumnIfMissing` 之后）：

```go
// Plan 20 C0: servers.credential_id becomes nullable (credential-less servers).
// SQLite can't relax NOT NULL in place — guarded table rebuild.
nullable, err := columnNullable(db, "servers", "credential_id")
if err != nil {
	return err
}
if !nullable {
	if err := rebuildServersNullable(db); err != nil {
		return err
	}
}
```

新函数（同文件）：

```go
// columnNullable reports whether table.column exists and lacks the NOT NULL flag.
func columnNullable(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return notnull == 0, nil
		}
	}
	return false, rows.Err() // column absent → fresh DB pre-initSchema, treat as "needs nothing"
}

// rebuildServersNullable recreates servers with a nullable credential_id inside one
// transaction (SQLite ALTER-rename dance: create new → copy → drop old → rename).
// FK enforcement is disabled for the dance (self-referential copies), re-enabled after.
func rebuildServersNullable(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys=ON`)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE servers_new (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			host TEXT NOT NULL,
			port INTEGER NOT NULL DEFAULT 22,
			user TEXT NOT NULL,
			auth_method TEXT NOT NULL DEFAULT '',
			credential_id TEXT REFERENCES credentials(id),
			sudo_credential_id TEXT,
			tags TEXT NOT NULL DEFAULT '[]',
			description TEXT NOT NULL DEFAULT '',
			location TEXT NOT NULL DEFAULT '',
			hardware TEXT NOT NULL DEFAULT '',
			services TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT '',
			caveats TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`INSERT INTO servers_new SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at FROM servers`,
		`DROP TABLE servers`,
		`ALTER TABLE servers_new RENAME TO servers`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("rebuild servers: %w", err)
		}
	}
	return tx.Commit()
}
```

⚠️ 以 `schemaSQL`（store.go:293 起）中现 `servers` 建表语句为基准核对列清单（上面若与现 schema 有出入——如 DEFAULT 细节——**以现 schema 为准**，唯一差异是 `credential_id` 去掉 `NOT NULL`）。`initSchema` 的 `CREATE TABLE IF NOT EXISTS servers` 同步改为 nullable 版（新库直建新形态）。

- [ ] **Step 3: scanServer 兼容 NULL**

`servers.go` scanServer：`&srv.CredentialID` → 仿 `sudoCredentialID` 用 `sql.NullString`：

```go
var credentialID sql.NullString
// Scan(&credentialID, ...)
srv.CredentialID = credentialID.String
```

（AddServer/UpdateServer 的 `nullableString(srv.CredentialID)` 已天然把 `""` 写为 NULL——无需改。）

- [ ] **Step 4: AuthForServer 结构化错误（先测后改）**

`internal/vault/vault.go`：

```go
// ErrNoCredential is returned by AuthForServer for a server that has no
// credential attached yet (credential-less model, Plan 20 C0). Callers map it
// to a "configure a credential first" action hint — never attempt a connect.
var ErrNoCredential = errors.New("server has no credential configured (set one with: ssh-manager servers edit <name> --password ... / --key ...)")

func AuthForServer(st *store.Store, srv *models.Server) (ssh.AuthMethod, error) {
	if srv.CredentialID == "" {
		return nil, ErrNoCredential
	}
	// ... 现有逻辑不动
}
```

`internal/mcpserver/core.go` exec 路径（`auth, aerr := vault.AuthForServer(st, srv)` 处）加分支：

```go
if errors.Is(aerr, vault.ErrNoCredential) {
	status = "no_credential"
	err = aerr
	return
}
```

测试：`core_test.go` 走既有 Exec 断言模式，加无凭据 server → status `no_credential`、err 含 "no credential"；vault_test 直接 `errors.Is(AuthForServer(...), ErrNoCredential)`。

- [ ] **Step 5: CLI/TUI add 放开凭据必填**

`internal/cli/servers.go:35-40`：删「required: exactly one of --password or --key」分支，保留互斥分支：

```go
if password != "" && keyPath != "" {
	return fmt.Errorf("--password and --key are mutually exclusive; provide one or neither")
}
```

`internal/tui/forms.go:282`：删 add 模式「凭据必填」error（`toServer` 在双空时本来就不铸凭据——正好落进无凭据形态）。

- [ ] **Step 6: 快照链路验证 + 全量 + 提交**

手工断言（写成测试）：`ExportSnapshot` → `ImportSnapshot` 对无凭据 server 双向无损（export.go 的 SnapshotServer/insert 已按 string 传，空串/NULL 由 nullableString 归一——补一个往返测试锁死）。

```bash
go test ./internal/store/... ./internal/vault/... ./internal/mcpserver/... ./internal/cli/... ./internal/tui/...
git add -A && git commit -m "feat: credential-less server model (nullable credential_id migration + ErrNoCredential)"
```

---

### Task 6 (B1): store 事务层 + 级联守卫 + gc

> ⚠️ 与 T5 同改 store 层——**必须排在 T5 之后**。

**Files:**
- Create: `internal/store/tx.go`（事务 API 全家）
- Modify: `internal/store/servers.go`（DeleteServer → 级联版）
- Modify: `internal/cli/servers.go`（add/edit 改走事务 API）、`internal/tui/forms.go`（submitServer 同）
- Create: `internal/cli/gc.go`（`ssh-manager gc`）
- Modify: `internal/cli/root.go`（注册 gc）
- Test: `internal/store/tx_test.go`（新）、`internal/cli/gc_test.go`（新）

**Interfaces:**
- Produces:
  - `func (s *Store) AddServerWithCredentials(srv *models.Server, cred, sudo *models.Credential) (string, error)` —— cred/sudo 可为 nil（无凭据）；`cred.ID != ""` 表示**复用既有凭据行**（T8 批内去重用），不铸新。
  - `func (s *Store) UpdateServerWithCredentials(srv *models.Server, cred, sudo *models.Credential) error` —— nil/空 = 保持现凭据；非空 ID = 复用；否则铸新并在同事务删旧（仅当旧凭据两列均无他处引用）。
  - `func (s *Store) DeleteServerCascading(id string) error` —— 删 server + 级联删其独占凭据（共享凭据只解除引用不删行）。
  - `func (s *Store) CountOrphanCredentials() (int, error)` / `func (s *Store) DeleteOrphanCredentials() (int64, error)`。
  - CLI：`ssh-manager gc [--apply]`。

- [ ] **Step 1: 失败测试（tx_test.go 全套）**

```go
func TestAddServerWithCredentialsAtomic(t *testing.T) {
	st := testStore(t)
	// 成功路径：cred+sudo+server 一事务
	id, err := st.AddServerWithCredentials(
		&models.Server{Name: "gpu", Host: "h", User: "u"},
		&models.Credential{Type: models.CredPassword, Secret: []byte("pw")},
		&models.Credential{Type: models.CredPassword, Secret: []byte("sudo")},
	)
	// 断言 server 行 + credentials 表恰好 2 行
	// 失败路径：srv 字段超限（description > 4096B 触发 validateServerText）→ 整体回滚，
	// credentials 表 0 行（零残留——这就是 G6 原子性断言）
}

func TestAddServerWithCredentialsReuseID(t *testing.T) {
	// 先建一台带凭据；再 AddServerWithCredentials 传 cred.ID=旧 id → credentials 行数不变
}

func TestDeleteServerCascadingSharedCredential(t *testing.T) {
	// 两台共享同一凭据（手动同 CredentialID）：删 A → 凭据仍在（B 引用）；删 B → 凭据行消失
	// sudo 凭据同款断言（第二引用列）
}

func TestUpdateServerWithCredentialsReplacesOld(t *testing.T) {
	// 换密码 → 新凭据生效；旧凭据（无他处引用）同事务删除
	// 旧凭据被另一台引用 → 只换指向不删旧行
}
```

- [ ] **Step 2: 实现 tx.go**

```go
package store

import (
	"database/sql"
	"fmt"

	"ssh-manager-mcp/internal/models"
)

// insertCredentialTx seals+inserts c inside tx. When c.ID is already set the
// row is assumed to exist (batch dedup reuse) and is NOT inserted again; the
// same id is returned.
func insertCredentialTx(tx *sql.Tx, masterKey []byte, c *models.Credential) (string, error) {
	if c.ID != "" {
		return c.ID, nil
	}
	secretBlob, err := seal(masterKey, c.Secret)
	if err != nil {
		return "", err
	}
	var passBlob []byte
	if len(c.Passphrase) > 0 {
		if passBlob, err = seal(masterKey, c.Passphrase); err != nil {
			return "", err
		}
	}
	id := newID()
	ts := now()
	if _, err := tx.Exec(
		`INSERT INTO credentials (id,type,secret_blob,passphrase_blob,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		id, string(c.Type), secretBlob, passBlob, ts, ts,
	); err != nil {
		return "", err
	}
	c.ID = id
	return id, nil
}

// credentialReferencedElseBy counts servers rows referencing credID via EITHER
// credential_id or sudo_credential_id, excluding the server row excludeID.
func credentialReferencedElseBy(tx *sql.Tx, credID, excludeID string) (int, error) {
	var n int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM servers
		  WHERE (credential_id=? OR sudo_credential_id=?) AND id<>?`,
		credID, credID, excludeID,
	).Scan(&n)
	return n, err
}

// AddServerWithCredentials inserts srv + (optionally) its credentials in ONE
// transaction — a mid-way failure leaves zero credential orphans (Plan 20 B1/G6).
// cred/sudo may be nil (credential-less). cred.ID != "" means reuse (no insert).
func (s *Store) AddServerWithCredentials(srv *models.Server, cred, sudo *models.Credential) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	if err := validateServerText(srv); err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	srv.CredentialID, srv.AuthMethod = "", ""
	if cred != nil {
		cid, err := insertCredentialTx(tx, s.masterKey, cred)
		if err != nil {
			return "", err
		}
		srv.CredentialID, srv.AuthMethod = cid, cred.Type.AuthMethodForServer()
	}
	if sudo != nil {
		sid, err := insertCredentialTx(tx, s.masterKey, sudo)
		if err != nil {
			return "", err
		}
		srv.SudoCredentialID = sid
	}
	id, err := insertServerTx(tx, srv)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// UpdateServerWithCredentials updates srv and swaps credentials atomically:
// nil/empty cred = keep current; cred.ID set = point at it; else mint new and
// delete the replaced one when nothing else references it (two-column check).
func (s *Store) UpdateServerWithCredentials(srv *models.Server, cred, sudo *models.Credential) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := validateServerText(srv); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := getServerTx(tx, srv.ID)
	if err != nil {
		return err
	}
	var dropOldCred, dropOldSudo string
	if cred != nil {
		cid, err := insertCredentialTx(tx, s.masterKey, cred)
		if err != nil {
			return err
		}
		if old.CredentialID != "" && old.CredentialID != cid {
			if n, err := credentialReferencedElseBy(tx, old.CredentialID, srv.ID); err == nil && n == 0 {
				dropOldCred = old.CredentialID
			}
		}
		srv.CredentialID, srv.AuthMethod = cid, cred.Type.AuthMethodForServer()
	} else {
		srv.CredentialID, srv.AuthMethod = old.CredentialID, old.AuthMethod
	}
	if sudo != nil {
		sid, err := insertCredentialTx(tx, s.masterKey, sudo)
		if err != nil {
			return err
		}
		if old.SudoCredentialID != "" && old.SudoCredentialID != sid {
			if n, err := credentialReferencedElseBy(tx, old.SudoCredentialID, srv.ID); err == nil && n == 0 {
				dropOldSudo = old.SudoCredentialID
			}
		}
		srv.SudoCredentialID = sid
	} else {
		srv.SudoCredentialID = old.SudoCredentialID
	}
	if err := updateServerTx(tx, srv); err != nil {
		return err
	}
	for _, id := range []string{dropOldCred, dropOldSudo} {
		if id == "" {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM credentials WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteServerCascading removes the server row and any credentials it EXCLUSIVELY
// owned (two-column reference check — shared credentials survive).
func (s *Store) DeleteServerCascading(id string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	srv, err := getServerTx(tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM servers WHERE id=?`, id); err != nil {
		return err
	}
	for _, cid := range []string{srv.CredentialID, srv.SudoCredentialID} {
		if cid == "" {
			continue
		}
		if n, err := credentialReferencedElseBy(tx, cid, id); err != nil {
			return err
		} else if n == 0 {
			if _, err := tx.Exec(`DELETE FROM credentials WHERE id=?`, cid); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// CountOrphanCredentials counts credential rows referenced by NEITHER column.
func (s *Store) CountOrphanCredentials() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM credentials
		  WHERE id NOT IN (SELECT credential_id FROM servers WHERE credential_id IS NOT NULL)
		    AND id NOT IN (SELECT sudo_credential_id FROM servers WHERE sudo_credential_id IS NOT NULL)`,
	).Scan(&n)
	return n, err
}

// DeleteOrphanCredentials removes exactly those rows (gc --apply).
func (s *Store) DeleteOrphanCredentials() (int64, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	res, err := s.db.Exec(
		`DELETE FROM credentials
		  WHERE id NOT IN (SELECT credential_id FROM servers WHERE credential_id IS NOT NULL)
		    AND id NOT IN (SELECT sudo_credential_id FROM servers WHERE sudo_credential_id IS NOT NULL)`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

`insertServerTx`/`getServerTx`/`updateServerTx`：把现 AddServer/UpdateServer/GetServer 的 SQL 抽成 tx 版本（`s.db.Exec` → `tx.Exec`），原方法委托之（消重）。

- [ ] **Step 3: 调用方切换**

- `internal/cli/servers.go` add：`SetCredential`+`AddServer` 两段 → 一次 `AddServerWithCredentials`（sudo 逻辑并入）；edit 的 re-credential 段 → `UpdateServerWithCredentials`。
- `internal/tui/forms.go` `toServer`/`submitServer` 同款（toServer 拆成「组 srv + 组 cred 指针」两段，submit 按 add/edit 调上两 API）。
- `servers rm`（DeleteServer 调用点）→ `DeleteServerCascading`。保留旧 `DeleteServer` 函数本体（兼容其他调用点/测试），生产 CLI 不再用。

- [ ] **Step 4: gc 命令（先测后写）**

`internal/cli/gc_test.go`：tmp store 种 2 孤儿凭据 + 1 在用 + host_keys 行 + cache_tokens 行 → dry-run 输出 count=2 且库不变；`--apply` 后孤儿消失、在用/host_keys/cache_tokens 原样。

`internal/cli/gc.go`：

```go
func newGCCmd() *cobra.Command {
	apply := false
	c := &cobra.Command{
		Use:   "gc",
		Short: "Find (and with --apply, delete) credential rows no server references",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			n, err := s.CountOrphanCredentials()
			if err != nil {
				return err
			}
			if !apply {
				fmt.Fprintf(cmd.OutOrStdout(), "%d orphan credential(s); rerun with --apply to delete (servers, host keys, cache tokens are never touched)\n", n)
				return nil
			}
			deleted, err := s.DeleteOrphanCredentials()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %d orphan credential(s)\n", deleted)
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "actually delete (default: dry-run)")
	return c
}
```

root.go 注册。

- [ ] **Step 5: 全量 + 提交**

```bash
go test ./...
git add -A && git commit -m "feat: transactional server+credential writes, cascade delete with shared-guard, gc command"
```

---

### Task 7 (C1): `internal/importer` 包

**Files:**
- Create: `internal/importer/importer.go`
- Create: `internal/importer/importer_test.go`
- Modify: `go.mod`/`go.sum`（`go get github.com/kevinburke/ssh_config@v1.6.0`——模块缓存已有，离线可解析）

**Interfaces:**
- Produces（T8/T10 消费，签名固定）:
  - `type Candidate struct { Name, Host string; Port int; User string; KeyPaths []string }`（User 已是回填后的最终值）
  - `type Skipped struct { Alias, Reason string }`（Reason ∈ `wildcard-pattern` / `bad-port` / `internal-duplicate`）
  - `type Result struct { Candidates []Candidate; Skipped []Skipped; MatchWarning bool }`
  - `func Parse(configPath, fallbackUser string) (*Result, error)`
  - `func ResolveKeyPaths(paths []string, configDir string) []string`

- [ ] **Step 1: 失败测试（表驱动，fixture 内联 tmp 文件）**

```go
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte(body), 0o600)
	return p
}

func TestParseTable(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		fbUser  string
		want    []importer.Candidate
		skipped map[string]string
		matchW  bool
	}{
		{"literal+inherit", "Host gpu\n  HostName 192.0.2.10\n  User deploy\nHost db\n  HostName 192.0.2.20\nHost *\n  Port 2222\n  User fallback\n", "fb",
			[]importer.Candidate{{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy"},
				{Name: "db", Host: "192.0.2.20", Port: 2222, User: "fallback"}}, nil, false},
		{"wildcard skipped", "Host *\n  HostName x\nHost gpu-*\n  HostName y\nHost gpu1\n  HostName z\n", "u",
			[]importer.Candidate{{Name: "gpu1", Host: "z", Port: 22, User: "u"}},
			map[string]string{"*": "wildcard-pattern", "gpu-*": "wildcard-pattern"}, false},
		{"multi-name block", "Host a b\n  HostName 192.0.2.1\n", "u",
			[]importer.Candidate{{Name: "a", Host: "192.0.2.1", Port: 22, User: "u"}, {Name: "b", ...}}, nil, false},
		{"hostname defaults to alias", "Host jump\n", "u", []importer.Candidate{{Name: "jump", Host: "jump", ...}}, nil, false},
		{"internal dedup", "Host a\n  HostName h\n  User u\n  Port 22\nHost b\n  HostName h\n  User u\n  Port 22\n", "u",
			[]importer.Candidate{{Name: "a", ...}}, map[string]string{"b": "internal-duplicate"}, false},
		{"bad port", "Host x\n  Port abc\n", "u", nil, map[string]string{"x": "bad-port"}, false},
		{"multi identityfile ordered", "Host k\n  IdentityFile ~/.ssh/a\n  IdentityFile b_key\n", "u",
			[]importer.Candidate{{Name: "k", Host: "k", Port: 22, User: "u", KeyPaths: []string{"~/.ssh/a", "b_key"}}}, nil, false},
		{"match warning", "Match host gpu\n  User mu\nHost gpu\n  HostName h\n", "u",
			[]importer.Candidate{{Name: "gpu", Host: "h", ...}}, nil, true},
	}
	// 逐条断言
}

func TestResolveKeyPaths(t *testing.T) {
	dir := t.TempDir()
	home, _ := os.UserHomeDir()
	got := importer.ResolveKeyPaths([]string{"~/.ssh/id_ed25519", "keys/rel", "/abs/key", "~root/x"}, dir)
	// 期望: [filepath.Join(home,".ssh","id_ed25519"), filepath.Join(dir,"keys","rel"), "/abs/key"]
	// "~root/x"（非 ~ 前缀单字符用户形态）→ 原样保留（调用方读不到自然落 needs-credential）
}
```

- [ ] **Step 2: 实现**

```go
// Package importer turns an OpenSSH client config into vault import candidates.
// Pure logic: no store, no network. All path/semantic decisions are documented
// deviations where they differ from OpenSSH (see spec 2026-08-15 rev1 §C1).
package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

type Candidate struct {
	Name, Host string
	Port       int
	User       string     // final value (fallbackUser applied)
	KeyPaths   []string   // raw strings from config (resolution is a separate step)
}

type Skipped struct{ Alias, Reason string }

type Result struct {
	Candidates  []Candidate
	Skipped     []Skipped
	MatchWarning bool // raw config contains Match blocks — inherited values may diverge from real ssh
}

var matchLine = regexp.MustCompile(`(?im)^\s*match\s`)

// Parse reads configPath and produces literal-host candidates.
func Parse(configPath, fallbackUser string) (*Result, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	cfg, err := ssh_config.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	res := &Result{MatchWarning: matchLine.Match(raw)}
	seen := map[string]bool{} // host:port:user dedup
	for _, host := range cfg.Hosts {
		for _, pat := range host.Patterns {
			alias := pat.String()
			if strings.ContainsAny(alias, "*?!") {
				res.Skipped = append(res.Skipped, Skipped{alias, "wildcard-pattern"})
				continue
			}
			hostName, _ := cfg.Get(alias, "HostName")
			if hostName == "" {
				hostName = alias // ssh semantics: alias doubles as hostname
			}
			portStr, _ := cfg.Get(alias, "Port")
			port := 22
			if portStr != "" {
				n, err := strconv.Atoi(strings.TrimSpace(portStr))
				if err != nil || n < 1 || n > 65535 {
					res.Skipped = append(res.Skipped, Skipped{alias, "bad-port"})
					continue
				}
				port = n
			}
			user, _ := cfg.Get(alias, "User")
			if strings.TrimSpace(user) == "" {
				user = fallbackUser
			}
			keys, _ := cfg.GetAll(alias, "IdentityFile")
			dedup := fmt.Sprintf("%s:%d:%s", hostName, port, user)
			if seen[dedup] {
				res.Skipped = append(res.Skipped, Skipped{alias, "internal-duplicate"})
				continue
			}
			seen[dedup] = true
			res.Candidates = append(res.Candidates, Candidate{Name: alias, Host: hostName, Port: port, User: user, KeyPaths: keys})
		}
	}
	return res, nil
}

// ResolveKeyPaths expands "~/" to the user home dir and resolves relative paths
// against configDir — a DELIBERATE, documented deviation from OpenSSH (which
// resolves against the ssh process CWD at connect time). Other forms pass through.
func ResolveKeyPaths(paths []string, configDir string) []string {
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		switch {
		case p == "~" || strings.HasPrefix(p, "~/"):
			out = append(out, filepath.Join(home, strings.TrimPrefix(p, "~")))
		case filepath.IsAbs(p):
			out = append(out, p)
		default:
			out = append(out, filepath.Join(configDir, p))
		}
	}
	return out
}
```

注意：`host.Patterns` 遍历 + `cfg.Get(alias, ...)` 的组合会**重复**命中同一别名（多名块里每个 pattern 独立；同别名多块时 ssh_config 的 Hosts 顺序决定 first-wins）——`seen` 去重以 alias 也补一道（同名 alias 只取第一次出现）：在 wildcard 判断后加 `if seenAlias[alias] { continue }`，`seenAlias[alias]=true`。

- [ ] **Step 3: 验证 + 提交**

```bash
go test ./internal/importer/...
git add -A && git commit -m "feat: internal/importer — ssh_config literal-host candidates (GetAll keys, match warning, path policy)"
```

---

### Task 8 (C2): CLI `servers import`

> 依赖 T5（无凭据模型）、T6（AddServerWithCredentials 复用 ID 语义）、T7（importer）。

**Files:**
- Create: `internal/cli/servers_import.go`
- Test: `internal/cli/servers_import_test.go`
- Modify: `internal/cli/servers.go`（newServersCmd 注册 import 子命令）

**Interfaces:**
- Produces: CLI `ssh-manager servers import [--file <path>] [--dry-run] [--profile <name>]`；`importer.PlanImport`（冲突判定纯函数，T10 复用——见 Step 2）。
- 消费: `importer.Parse/ResolveKeyPaths`、`store.AddServerWithCredentials`（cred.ID 复用）、`store.GrantServers`（T3 后已 fail-fast）。

- [ ] **Step 1: 失败测试（smoke 全套，tmp store via env seam）**

```go
func TestServersImportFlow(t *testing.T) {
	// 布置: tmp store + fixture config（gpu 带未加密 key、bare 无 key、dup 与已有 server 同 host:port:user）
	// 1) --dry-run: 输出含 will-import/skip 表；库内 0 servers
	// 2) 真导入: gpu+bare 入库; dup skip-existing; 幂等重跑全 skip
	// 3) 批内去重: 两台共用同一 key 文件 → credentials 表恰好 1 行（该 key）
	// 4) --profile 不存在 → 导入前报错（连 dry-run 也报）
	// 5) --profile 存在 → grant 后 ServersForProfile 含两台; dry-run+--profile → 只打印不 grant
	// 6) 加密 key（带口令的 PEM）→ 导入成功 + 输出含 needs-passphrase 警示
}
```

fixture 生成未加密/加密 key：用 `crypto/ed25519.GenerateKey` + `x509.MarshalPKCS8PrivateKey`（无口令）；加密形态用 `x509.EncryptPEMBlock`（DEK-Info 头的老式加密——`ssh.ParsePrivateKey` 对它返回 `&PassphraseMissingError{}`，正是检测点）。

- [ ] **Step 2: 实现**

```go
func serversImportCmd() *cobra.Command {
	var file, profile string
	var dryRun bool
	c := &cobra.Command{
		Use:   "import",
		Short: "Batch-import servers from an OpenSSH client config (~/.ssh/config)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				home, _ := os.UserHomeDir()
				file = filepath.Join(home, ".ssh", "config")
			}
			file = expandTilde(file)
			fallbackUser := currentUserName()
			res, err := importer.Parse(file, fallbackUser)
			if err != nil {
				return err
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			var profID string
			if profile != "" { // precheck BEFORE anything (fail-fast)
				p, _ := s.GetProfileByName(profile)
				if p == nil {
					return fmt.Errorf("profile %q not found (create it first: profiles add)", profile)
				}
				profID = p.ID
			}
			return runImport(cmd.OutOrStdout(), s, res, filepath.Dir(file), dryRun, profID, profile)
		},
	}
	c.Flags().StringVar(&file, "file", "", "config file (default ~/.ssh/config)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen; write nothing")
	c.Flags().StringVar(&profile, "profile", "", "grant every imported server to this profile (must exist)")
	return c
}
```

`runImport` 核心循环（批内 key 去重 + 单条原子 + needs-passphrase 检测）：

```go
type importReport struct {
	name, note string
	imported   bool
}

func runImport(out io.Writer, s *store.Store, res *importer.Result, configDir string, dryRun bool, profID, profName string) error {
	if res.MatchWarning {
		fmt.Fprintln(out, "⚠ config 含 Match 块：相关继承值由库按 Host 模式近似求值，可能与真 ssh 不一致")
	}
	existing, err := s.ListServers()
	if err != nil {
		return err
	}
	occ := map[string]bool{} // host:port:user occupied
	for _, e := range existing {
		occ[fmt.Sprintf("%s:%d:%s", e.Host, e.Port, e.User)] = true
	}
	keyIDs := map[[32]byte]string{} // sha256(key content) -> credential id (batch dedup)
	var report []importReport
	var importedIDs []string
	for _, cand := range res.Candidates {
		if e, _ := s.GetServerByName(cand.Name); e != nil {
			report = append(report, importReport{cand.Name, "skip-existing (name)"})
			continue
		}
		if occ[fmt.Sprintf("%s:%d:%s", cand.Host, cand.Port, cand.User)] {
			report = append(report, importReport{cand.Name, "skip-existing (host:port:user)"})
			continue
		}
		var cred *models.Credential
		note := "needs-credential"
		for _, kp := range importer.ResolveKeyPaths(cand.KeyPaths, configDir) {
			keyBytes, err := os.ReadFile(kp)
			if err != nil {
				continue // try next key
			}
			sum := sha256.Sum256(keyBytes)
			if id, ok := keyIDs[sum]; ok {
				cred = &models.Credential{ID: id, Type: models.CredPrivateKey} // reuse (no mint)
			} else {
				cred = &models.Credential{Type: models.CredPrivateKey, Secret: keyBytes}
			}
			note = ""
			if _, err := ssh.ParsePrivateKey(keyBytes); err != nil {
				if _, missing := err.(*ssh.PassphraseMissingError); missing {
					note = "needs-passphrase ⚠（连接会失败；TUI 补全或 servers edit --key-passphrase）"
				}
			}
			// remember the minted id AFTER insert (handled below via cred.ID backfill)
			keyIDs[sum] = "" // provisional; backfilled post-insert
			break            // first readable key wins (decision ③)
		}
		if dryRun {
			report = append(report, importReport{cand.Name, "will-import (" + noteOrCred(note, cred) + ")"})
			continue
		}
		id, err := s.AddServerWithCredentials(&models.Server{
			Name: cand.Name, Host: cand.Host, Port: cand.Port, User: cand.User,
		}, cred, nil)
		if err != nil {
			report = append(report, importReport{cand.Name, "FAILED: " + err.Error()})
			continue
		}
		if cred != nil && cred.ID != "" {
			sum := sha256.Sum256(cred.Secret)
			keyIDs[sum] = cred.ID // insertCredentialTx backfilled c.ID — future candidates reuse
		}
		importedIDs = append(importedIDs, id)
		occ[fmt.Sprintf("%s:%d:%s", cand.Host, cand.Port, cand.User)] = true
		report = append(report, importReport{cand.Name, "imported " + noteOrCred(note, cred)})
	}
	// skipped 段（wildcard/bad-port/internal-duplicate）
	for _, sk := range res.Skipped {
		report = append(report, importReport{sk.Alias, "skip: " + sk.Reason})
	}
	for _, r := range report {
		fmt.Fprintf(out, "%-20s %s\n", r.name, r.note)
	}
	// --profile grant（dry-run 只打印）
	if profID != "" {
		if dryRun || len(importedIDs) == 0 {
			fmt.Fprintf(out, "grant: %d server(s) -> %s%s\n", len(importedIDs), profName, ternary(dryRun, " (dry-run, not granted)", ""))
			return nil
		}
		if err := s.GrantServers(profID, importedIDs); err != nil {
			return fmt.Errorf("imported but grant failed: %w", err)
		}
		fmt.Fprintf(out, "granted %d server(s) -> %s\n", len(importedIDs), profName)
	}
	fmt.Fprintln(out, "提示：原私钥文件仍在盘上（vault 另存一份）；结构化字段可进 TUI 逐台补全或 servers edit。")
	return nil
}
```

（`expandTilde`/`currentUserName`/`noteOrCred`/`ternary` 为小 helper：expandTilde 处理 `~/`；currentUserName 用 `user.Current().Username`；`noteOrCred` 返回 note 或 "key"/"no-key"。）**注意**：`keyIDs` 的 provisional 空串回填逻辑——同一批第二台命中同 key 时 `keyIDs[sum]` 尚为 `""`（第一台 insert 后才回填），顺序循环下天然正确（第一台先插入成功才轮到第二台）；防御性处理：复用分支检查 `id != ""` 才走复用，否则按新铸。

- [ ] **Step 3: 验证 + 提交**

```bash
go test ./internal/cli/...
git add -A && git commit -m "feat: ssh-manager servers import — dry-run, batch key dedup, atomic per-candidate, profile grant"
```

---

### Task 9 (C3a): TUI 表单构造器拆分（行为零变）

**Files:**
- Modify: `internal/tui/forms.go:26-53`
- Test: `internal/tui/forms_test.go`

**Interfaces:**
- Produces（T10 消费）:
  - `func structuredFields(d *serverDraft) []huh.Field`（hardware/location/role/services/caveats/description 六栏，顺序与现 newServerForm 第三组一致）
  - `func passwordField(d *serverDraft, editing bool) *huh.Input`（密码栏，editing 决定 title）
  - `func sudoPasswordField(d *serverDraft) *huh.Input`

- [ ] **Step 1: 先锁行为（失败测试）**

```go
func TestNewServerFormFieldTitles(t *testing.T) {
	d := &serverDraft{}
	f := newServerForm(d, false)
	// huh 无公开字段遍历 API → 改为断言构造器输出:
	// structuredFields 返回 6 个 field 且 Title 顺序 = 硬件/位置/角色/服务/Caveats/备注
	// passwordField(d,false).Title() == "密码（与密钥二选一）"
	// passwordField(d,true).Title()  == "密码（留空=保持不变）"
}
```

- [ ] **Step 2: 拆分**

`newServerForm` 重写为组装（组内顺序/字段逐字节等价）：

```go
func newServerForm(d *serverDraft, editing bool) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("名称（唯一）").Value(&d.Name).Validate(nonEmpty),
			huh.NewInput().Title("Host / IP").Value(&d.Host).Validate(nonEmpty),
			huh.NewInput().Title("SSH 用户").Value(&d.User).Validate(nonEmpty),
			portField(&d.Port),
		),
		huh.NewGroup(append([]huh.Field{
			passwordField(d, editing),
			huh.NewInput().Title("私钥路径（与密码二选一；编辑时留空=不变）").Value(&d.KeyPath),
			huh.NewInput().Title("密钥口令（可选）").Value(&d.KeyPass).EchoMode(huh.EchoModePassword),
			sudoPasswordField(d),
		})...),
		huh.NewGroup(structuredFields(d)...),
	)
}

func passwordField(d *serverDraft, editing bool) *huh.Input {
	title := "密码（与密钥二选一）"
	if editing {
		title = "密码（留空=保持不变）"
	}
	return huh.NewInput().Title(title).Value(&d.Password).EchoMode(huh.EchoModePassword)
}

func sudoPasswordField(d *serverDraft) *huh.Input {
	return huh.NewInput().Title("sudo 密码（可选）").Value(&d.SudoPassword).EchoMode(huh.EchoModePassword)
}

func structuredFields(d *serverDraft) []huh.Field {
	return []huh.Field{
		huh.NewInput().Title("硬件").Value(&d.Hardware),
		huh.NewInput().Title("位置").Value(&d.Location),
		huh.NewInput().Title("角色").Value(&d.Role),
		huh.NewInput().Title("服务").Value(&d.Services),
		huh.NewInput().Title("Caveats（agent 行动前必读）").Value(&d.Caveats),
		huh.NewInput().Title("备注").Value(&d.Description),
	}
}
```

既有 add/edit 表单测试（13/13）必须原样全绿。

- [ ] **Step 3: 验证 + 提交**

```bash
go test ./internal/tui/...
git add -A && git commit -m "refactor: extract server form field constructors (zero behavior change)"
```

---

### Task 10 (C3b): TUI 导入 + 逐台补全循环 + ⚠ 过滤

**Files:**
- Create: `internal/tui/importflow.go`（导入向导 overlay 状态机）
- Modify: `internal/tui/app.go`（servers 页 `i` 键、`!` 过滤键）
- Modify: `internal/tui/servers.go`（⚠ 标记/置顶/过滤）
- Test: `internal/tui/importflow_test.go`、`internal/tui/servers_test.go`

**Interfaces:**
- 消费: T9 构造器、T7 importer、T6 `AddServerWithCredentials`/`UpdateServerWithCredentials`。
- Produces: `func serverNeedsAttention(s *models.Server) bool`（= `s.CredentialID == "" || s.Role == "" || hasTag(s, "needs-passphrase")`）——`needs-passphrase` 标签在导入加密 key 时写入、补口令成功后移除。

- [ ] **Step 1: ⚠ 判定 + 列表（先测后改）**

```go
func TestServerNeedsAttention(t *testing.T) {
	if !serverNeedsAttention(&models.Server{Name: "x"}) { t.Fatal("无凭据须 ⚠") }
	if !serverNeedsAttention(&models.Server{CredentialID: "c", Role: ""}) { t.Fatal("role 空须 ⚠") }
	if !serverNeedsAttention(&models.Server{CredentialID: "c", Role: "r", Tags: []string{"needs-passphrase"}}) { t.Fatal("缺口令标签须 ⚠") }
	if serverNeedsAttention(&models.Server{CredentialID: "c", Role: "r"}) { t.Fatal("完整不应 ⚠") }
}

func TestServersPageWarnSortFilter(t *testing.T) {
	p := &serversPage{items: []*models.Server{{Name: "ok", CredentialID: "c", Role: "r"}, {Name: "bare"}}}
	p.sortWarnFirst()
	if p.Rows()[0] != "bare" { t.Fatal("⚠ 置顶") }
	p.warnOnly = true
	if len(p.Rows()) != 1 || p.Rows()[0] != "bare" { t.Fatal("! 过滤") }
}
```

`servers.go`：`serverNeedsAttention`、`serversPage.sortWarnFirst()`（stable sort）、`warnOnly bool` 字段 + `Rows()` 里 `⚠ ` 前缀 + 过滤。`app.go` servers 页 dispatch 加 `!` 键切换 `warnOnly` 并 `sortWarnFirst()`；刷新数据后保持排序稳定。

- [ ] **Step 2: importflow 状态机**

`internal/tui/importflow.go`（骨架，overlay 接口同 formOverlay 模式）：

```go
// importFlow walks: file-path form → candidate multiselect → silent batch import
// → per-server supplement loop (Esc skips, q ends). States: statePathForm,
// statePick, stateImporting, stateSupplement(i), stateResult.
type importFlow struct {
	st       *store.Store
	state    int
	pathVal  string
	pick     []string            // chosen candidate names
	cands    []importer.Candidate
	imported []string            // server ids successfully imported
	report   []string
	suppIdx  int
	srv      *models.Server      // current supplement target
	d        *serverDraft        // supplement draft (structured + conditional secret)
	condPass bool                // supplement shows password field (credless target)
	condKey  bool                // supplement shows passphrase field (needs-passphrase target)
	err      error
}
```

关键行为（逐条绑定测试）：
- statePathForm：huh 单栏（预填 `~/.ssh/config` 展开），Esc → 整个 overlay 关闭（零动作）。
- statePick：`importer.Parse` → 排除 vault 冲突（同名/同 host:port:user，逻辑同 T8）→ huh MultiSelect（默认全选）→ Esc 关闭。
- stateImporting：一条 tea.Cmd 内联跑导入循环。**冲突过滤必须与 CLI 同源**：T8 实施时把「候选 vs 既有库冲突判定」抽成 importer 纯函数 `func PlanImport(cands []Candidate, existing []ExistingServer) (toImport []Candidate, skipped []SkippedReason)`（`ExistingServer{Name, Host string; Port int; User string}`），CLI 与 TUI 都消费它（T8 的测试相应分两层：PlanImport 表驱动 + runImport smoke）。加密 key → 写入 `needs-passphrase` tag；单条失败记 report 不中断。
- stateSupplement(i)：每台表单 = `structuredFields(d)` + `sudoPasswordField(d)` + 条件栏（`condPass` → `passwordField(d, true)` 语义「留空=稍后」；`condKey` → 「密钥口令（补全加密私钥）」EchoModePassword）。顶部只读行 `fmt.Sprintf("%s @ %s:%d (%s)", srv.Name, srv.Host, srv.Port, srv.User)`。提交 → `UpdateServerWithCredentials`（有密码/passphrase 时传 cred：补 passphrase 场景用「重读私钥文件+新口令」铸新凭据替换——私钥路径从哪来？supplement 循环持有 import 时的 keyBytes（内存），passphrase 场景 `models.Credential{Type: CredPrivateKey, Secret: keyBytes, Passphrase: input}`）→ 成功且曾带 needs-passphrase 标签则移除标签 → 下一台。Esc → 下一台（标 ⚠ 天然成立）；q → 直达 stateResult。
- stateResult：`导入 %d / 跳过 %d / 待补 %d（⚠ 列表按 ! 过滤后逐台 e）` + 任意键关闭。

- [ ] **Step 3: app.go 接线**

servers 页 dispatch：`i` 键 → `a.overlay = newImportFlow(a.st)`；`!` 键 → 过滤切换。footer 补键位说明。

- [ ] **Step 4: 全量 + 提交**

```bash
go test ./internal/tui/... ./internal/importer/... ./internal/cli/...
git add -A && git commit -m "feat: TUI ssh-config import flow with per-server supplement loop and warn filter"
```

---

### Task 11 (B2): token env 通道 + 生成器改造

> 依赖 T1（`clientops.SplitTokenPin` 已导出——本任务 env 值若出现在 cache pull 路径的 pin 分离也走它；`mcp --token` 本体两种模式都收 project token，env 与 flag 同源同义）。

**Files:**
- Modify: `internal/cli/mcp.go`（resolveToken + 去 MarkFlagRequired）
- Modify: `internal/cli/projects.go:234`（生成器 env 形态）
- Test: `internal/cli/mcp_test.go`（新）、`internal/cli/projects_test.go`
- Modify: `docs/agent-access.md`、`docs/getting-started.md`（Step 5 + gitignore 警告）

**Interfaces:**
- Produces: env `SSHMGR_TOKEN`（与 `--token` 同义同路径：flag 优先，env 兜底，双空报错）。

- [ ] **Step 1: 失败测试**

```go
func TestResolveToken(t *testing.T) {
	t.Setenv("SSHMGR_TOKEN", "from-env")
	if got := resolveToken("from-flag"); got != "from-flag" { t.Fatal("flag 必须优先") }
	if got := resolveToken(""); got != "from-env" { t.Fatal("env 兜底") }
	t.Setenv("SSHMGR_TOKEN", "")
	if got := resolveToken(""); got != "" { t.Fatal("双空返回空（RunE 报错）") }
}

func TestMcpJSONSnippetUsesEnv(t *testing.T) {
	var b bytes.Buffer
	printMcpJSON(&b, "tok123")
	out := b.String()
	if strings.Contains(out, `"--token"`) || strings.Contains(out, "tok123\"],") { t.Fatal("token 不得在 argv") }
	if !strings.Contains(out, `"SSHMGR_TOKEN":"tok123"`) { t.Fatal("须走 env 字段") }
}
```

- [ ] **Step 2: 实现**

`mcp.go`：

```go
// resolveToken: flag wins, SSHMGR_TOKEN env is the fallback (identical
// semantics & downstream parsing — same name, same meaning; Plan 20 B2).
// Removes the token from process argv (ps/proc visibility) when env is used.
func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("SSHMGR_TOKEN")
}
```

RunE 开头 `token = resolveToken(token)`；`if token == "" { return fmt.Errorf("--token or SSHMGR_TOKEN is required") }`；删 `c.MarkFlagRequired("token")`。flag help 补 `(or env SSHMGR_TOKEN)`。

`projects.go:234`：

```go
fmt.Fprintf(out, `{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp"],"env":{"SSHMGR_TOKEN":"%s"}}}}`+"\n", token)
```

- [ ] **Step 3: 文档**

getting-started Step 5 与 agent-access：新形态 JSON + 「env 仍可被同用户/root 经 `/proc/<pid>/environ` 读——本改进**消除的是 argv/ps 暴露面**，不是全部可见性」+ **PUBLIC 仓库警告**：`.mcp.json` 含 token，必须 gitignore（加粗，两处文档同款措辞）。

- [ ] **Step 4: 全量 + 提交**

```bash
go test ./internal/cli/...
git add -A && git commit -m "feat: SSHMGR_TOKEN env channel (flag parity), .mcp.json generator emits env form"
```

---

### Task 12 (B3): shared buildinfo 包

**Files:**
- Create: `internal/buildinfo/buildinfo.go`
- Modify: `internal/cli/root.go`（Version 引 buildinfo）、`internal/mcpserver/server.go:52`
- Modify: `.goreleaser.yml`（ldflags 目标）
- Test: `internal/cli/version_test.go`

**Interfaces:**
- Produces: `buildinfo.Version string`（唯一版本源；ldflags `-X ssh-manager-mcp/internal/buildinfo.Version`）。

- [ ] **Step 1: 实现 + 接线**

```go
// Package buildinfo is the single source of the release version, injectable
// via -ldflags -X. Both the CLI version command and the MCP serverInfo read
// it (cli→mcpserver import direction forbids mcpserver reading cli).
package buildinfo

var Version = "dev"
```

- `cli/root.go`：`var Version = "dev"` → 删除，改引用 `buildinfo.Version`（version 子命令与所有引用点跟着改）。
- `mcpserver/server.go:52`：`Version: "v0.1.0"` → `Version: buildinfo.Version`。
- `.goreleaser.yml`：`-X ssh-manager-mcp/internal/cli.Version={{.Version}}` → `-X ssh-manager-mcp/internal/buildinfo.Version={{.Version}}`。

- [ ] **Step 2: 测试 + 提交**

```go
func TestVersionCmdPrintsBuildinfo(t *testing.T) {
	buildinfo.Version = "test-9.9.9"
	defer func() { buildinfo.Version = "dev" }()
	// 跑 version 子命令（既有测试模式），断言输出含 test-9.9.9
}
```

```bash
go build -ldflags "-X ssh-manager-mcp/internal/buildinfo.Version=probe" ./cmd/ssh-manager && ./ssh-manager version | grep probe
go test ./... 
git add -A && git commit -m "feat: shared buildinfo package — CLI and MCP serverInfo same version source"
```

---

### Task 13 (B4): forward half-close + HostKeyAlgorithms 旋钮

**Files:**
- Modify: `internal/sshbroker/tunnel.go:110-121`（handle 方向性半关闭）
- Modify: `internal/sshbroker/connect.go`（HostKeyAlgorithms env 旋钮）
- Modify: `internal/testsshd/sshd.go`（handleDirectTCP 对称半关闭——测试基建）
- Test: `internal/sshbroker/tunnel_test.go`（半关闭集成）、`internal/sshbroker/connect_knob_test.go`

**Interfaces:**
- Produces: env `SSHMGR_SSH_HOST_KEY_ALGORITHMS`（逗号分隔允许列表；空=默认；非法值 **fail-closed** 连接前报错）。

- [ ] **Step 1: 半关闭失败测试**

```go
func TestTunnelHalfClosePropagates(t *testing.T) {
	// echo 服务：读到 EOF 后写 "FIN" 再关
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		c, _ := ln.Accept()
		io.Copy(io.Discard, c)          // 读完请求侧
		c.Write([]byte("FIN"))          // EOF 后仍能回写——半关闭链路的核心断言
		c.(*net.TCPConn).CloseWrite()
	}()
	// testsshd 起 direct-tcpip 可达的 broker Client → ForwardLocal(0, "127.0.0.1", echoPort)
	// 客户端: dial 隧道 → 写请求 → CloseWrite() → io.ReadAll → 断言收到 "FIN"
	// （现实现第一个 done 就关双连接 → ReadAll 得不到 FIN 或 ErrClosed —— 失败即复现）
}
```

- [ ] **Step 2: handle 改造**

```go
// closeWriter is implemented by *net.TCPConn (local side) and gossh's channel
// net.Conn (remote side) — CloseWrite sends EOF without tearing down the read half.
type closeWriter interface{ CloseWrite() error }

func (t *Tunnel) handle(local net.Conn, remote string) {
	defer local.Close()
	rem, err := t.client.Dial("tcp", remote)
	if err != nil {
		return
	}
	defer rem.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(rem, local)
		if cw, ok := rem.(closeWriter); ok {
			_ = cw.CloseWrite() // propagate local EOF toward the remote peer
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, rem)
		if cw, ok := local.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done // wait for BOTH directions: half-close on one side must not kill the other
	<-done
}
```

`testsshd.go handleDirectTCP` 同款改造（对称，保证测试链路能传播半关闭）。更新 tunnel.go 原 half-close limitation 注释为已落地说明。

- [ ] **Step 3: HostKeyAlgorithms 旋钮（先测后改）**

```go
func TestHostKeyAlgoKnob(t *testing.T) {
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "rsa-sha2-512")
	if l := hostKeyAlgos(); !reflect.DeepEqual(l, []string{"rsa-sha2-512"}) { t.Fatal() }
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "not-an-algo")
	if _, err := hostKeyAlgosChecked(); err == nil { t.Fatal("非法值必须 fail-closed") }
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "")
	if l := hostKeyAlgos(); l != nil { t.Fatal("空=默认") }
}
// e2e: 对 testsshd（RSA host key）用 rsa-sha2-512 连通；typo → Connect 返回连接前错误
```

`connect.go`：

```go
// allowedHostKeyAlgos is the explicit allowlist for the knob (fail-closed on typos).
var allowedHostKeyAlgos = map[string]bool{
	"ssh-ed25519": true, "ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true, "rsa-sha2-256": true, "rsa-sha2-512": true, "ssh-rsa": true,
}

func hostKeyAlgos() []string {
	v := strings.TrimSpace(os.Getenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS"))
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func hostKeyAlgosChecked() ([]string, error) {
	l := hostKeyAlgos()
	for _, a := range l {
		a = strings.TrimSpace(a)
		if !allowedHostKeyAlgos[a] {
			return nil, fmt.Errorf("SSHMGR_SSH_HOST_KEY_ALGORITHMS: unknown algorithm %q (allowed: ed25519, ecdsa-sha2-nistp256/384/521, rsa-sha2-256/512, ssh-rsa)", a)
		}
	}
	return l, nil
}
```

`Connect` 开头 `if l, err := hostKeyAlgosChecked(); err != nil { return nil, err }` else `cfg.HostKeyAlgorithms = l`。文档：`docs/managing-servers.md` 增「连老机器（需要 ssh-rsa）」小节——含**开旋钮后服务端可能改呈不同算法的主机键，需清该机 host key 重走 TOFU** 提醒。

- [ ] **Step 4: 全量 + 提交**

```bash
go test ./internal/sshbroker/... ./internal/testsshd/... ./internal/conformance/...
git add -A && git commit -m "feat: directional half-close in tunnels, SSHMGR_SSH_HOST_KEY_ALGORITHMS knob (fail-closed)"
```

---

### Task 14 (C4): 文档 + 收尾验证

**Files:**
- Modify: `docs/getting-started.md`（Step 2 加「批量导入」旁门）
- Modify: `docs/managing-servers.md`（import 专节：CLI flags、dry-run、补全流程、Match 警示、相对路径**有意偏离**说明、needs-passphrase 补法、gc 命令）
- Modify: `README.md`（功能表加 import 行 + v0.7.0 release note 增补：import / 无凭据模型 / token env 通道 / gc / half-close）
- Modify: `docs/quickstart-multi-machine.md`（若 T4 改后仍有指针缺口）

**Interfaces:** 无。

- [ ] **Step 1: 逐文档写入**（内容对照最终代码逐条核对——README 功能表每行必须是代码里真实存在的命令/行为）

- [ ] **Step 2: 收尾全量验证**

```bash
go build ./... && go vet ./... && go test ./...
gofmt -l . | grep -v vendor   # 期望空
```

- [ ] **Step 3: 提交**

```bash
git add -A && git commit -m "docs: import guide, gc, token env channel, v0.7.0 release notes"
```

---

## 任务依赖图

```
T1(A1) ──→ T11(B2)
T2(A2) T3(A3) T4(A4)          [独立]
T5(C0) ──→ T6(B1) ──→ T8(C2) ──→ T10(C3b)
              └──────→ T7(C1) ──↗      ↑
T9(C3a) ────────────────────────────────┘
T12(B3) T13(B4)                [独立]
T14(C4)                        [最后]
```

串行执行顺序（subagent-driven 默认）：T1→T2→T3→T4→T5→T6→T7→T8→T9→T10→T11→T12→T13→T14。
