# Plan 26：欠账清偿 — junction/符号目录上传语义 + clear 双角色文档化 + backlog 固化 + cosmetic 批

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 rev2 P3 欠账一次清掉：定义并钉住 upload 对符号链接目录（含 Windows junction）的语义、文档化 `clear` 的双角色解析、把散落在代码注释里的 backlog 固化成 docs/backlog.md、清掉 Plan 25 遗留的 4 处 cosmetic。

**Architecture:** 上传语义改动收敛在 `internal/sshbroker/upload.go` 的 `uploadDir`（根解析 + walk 回调里的符号目录显式拒绝），其余是测试、文档和注释级改动。不碰凭据路径，不碰 MCP schema（upload 工具描述仅补一句）。

**Tech Stack:** Go（标准库 `filepath.EvalSymlinks` / `os.Symlink` / Windows `mklink /J`），无新依赖。

## Global Constraints

- **铁律不动**：任何任务不得改变凭据的存取路径与暴露面。
- **无新依赖**：`go.mod` 零新增。
- **测试密闭**（2026-08-16 CI 首跑红的直接教训）：任何可能触达程序固定路径（vault dir、`/var/lib/...`、用户配置目录）的测试，必须先经 `SSHMGR_*` env seam 或 `t.TempDir()` 钉住路径；`t.Setenv` 自动回滚。凡是能给操作系统全局状态留下痕迹的操作一律禁止。
- **符号链接特权守卫**：`os.Symlink` 建链失败（Windows 无 Developer Mode/admin）时测试 `t.Skipf` 并说明原因（先例：`TestUploadBrokenSymlinkInDirErrors`）；Windows junction 一律用 `cmd /c mklink /J`（免特权），失败也 `t.Skipf`。CI windows-latest runner 是 admin，双 lane 必须真实跑到。
- **gofmt / go vet 干净**；实现完成后 `go build ./...`、`go vet ./...`、`gofmt -l .` 零输出。
- **文案与已实现行为一致**：文档/注释只描述真实行为，不写愿景。

---

## 背景（全部经本会话源码取证）

1. **嵌套符号目录现状**：`uploadDir`（upload.go:138）用 `filepath.Walk`（lstat 语义）。目录内的 symlink→dir 条目：`info.IsDir()==false` 且 `ModeSymlink` 置位 → Plan 24 的跟链 re-stat 只在 `ctr.cap > 0` 时执行，跟到的目标 IsDir 但没人看 → `uploadFile` 对目录 `os.Open` → Linux 读时 EISDIR、Windows 打开即失败——跨平台报错误导，语义未定义。
2. **junction/symlink 作上传根现状**：`Upload`（upload.go:57）入口 `os.Stat` **跟链** → IsDir=true → 走 `uploadDir`；但 `filepath.Walk` 对根用 **lstat** → junction 根报"非目录" → 掉进文件分支 → 同样的误导性失败。即"Stat 说是目录、Walk 当文件传"。
3. **Plan 24 语义先例**：symlink→**文件** = 跟链（gate 跟链、传输跟链），本计划不得破坏（有既有回归测试 `TestUploadSymlinkToSmallFileFollowsTarget`）。
4. **`clear` 双角色**：`scanClearTargets`（clear.go:119）**刻意不看 role**（server 机可残留 client 文件，反之亦然），role 仅用于表头标签与安全网决策；已有测试 `TestEnumClearTargets_ServerMachine` 钉住 server 机 + client 残留的枚举，但**双 role.json 同时在盘**的组合没钉，文档也没写这段语义。
5. **backlog 散落**：4 项 standing backlog 只活在代码注释里（tunnels.go:14 "tracked backlog item"、revoke_semantics_test.go:28 等），无统一清单。
6. **cosmetic 残留**（Plan 25 终审豁免项）：`internal/eval/summary_test.go:137` 日志字符串仍写 "~10 min idle-timeout"；`internal/mcpserver/types.go:47-48` 与 `core.go:289` 的 "(= the agent's) filesystem" 括号注释在 serve 模式下是错的（broker ≠ agent）。

## 任务间接口

- T1/T2 同文件（upload.go）按序执行，T2 依赖 T1 落位后的 uploadDir 形态；T3/T4/T5 相互独立，可任意序。
- T2 产出新错误文案 `symlinked directory not uploaded: <path>` —— T2 的文档步骤引用该精确措辞。
- T4 产出 `docs/backlog.md` —— T2 若需提及"受控监听地址"等直接链接它。

---

### Task 1: upload 根目录跟链解析（junction/symlink root）

**Files:**
- Modify: `internal/sshbroker/upload.go`（`uploadDir` 首行，约 :138-140）
- Test: `internal/sshbroker/upload_test.go`（追加）

**Interfaces:**
- Consumes: 既有 `testsshd.Start` + `connectTest` + `mcpUploadCap`（同文件既有 helper）。
- Produces: `uploadDir` 对根的 EvalSymlinks 解析——T2 的嵌套拒绝逻辑叠在同一函数里。

- [ ] **Step 1: 写失败测试**（追加到 upload_test.go 末尾；fixture 模式抄 `TestUploadSymlinkToSmallFileFollowsTarget`）

```go
// TestUploadDirSymlinkRootResolved (Plan 26): a symlink/junction used AS the
// upload root is resolved to its target — Upload's os.Stat already follows the
// link (says "dir"), but filepath.Walk lstats the root and would misclassify
// it as a file. EvalSymlinks at uploadDir entry makes root handling follow the
// operator's intent. Windows lane exercises this via a junction (mklink /J,
// no privilege needed); unix via os.Symlink (skip when unprivileged).
func TestUploadDirSymlinkRootResolved(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("root-link\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link-root")
	if err := makeDirLink(t, link, real); err != nil {
		t.Skipf("dir link creation failed on this host (%v); skipping", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-link-root")
	res, err := c.Upload(context.Background(), link, remoteDir, 0)
	if err != nil {
		t.Fatalf("symlink-root Upload: %v", err)
	}
	if res.Files != 1 || res.Bytes != int64(len("root-link\n")) {
		t.Fatalf("result = %+v, want {Files:1 Bytes:%d}", res, len("root-link\n"))
	}
	g, err := c.Download(context.Background(), filepath.Join(remoteDir, "a.txt"), 0)
	if err != nil || g.Content != "root-link\n" {
		t.Fatalf("round-trip: err=%v content=%q", err, g.Content)
	}
}

// makeDirLink creates link pointing at dir target: junction on Windows
// (privilege-free), symlink elsewhere.
func makeDirLink(t *testing.T, link, dir string) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mklink /J: %v: %s", err, out)
		}
		return nil
	}
	return os.Symlink(dir, link)
}
```

（`makeDirLink` 是 T1/T2 共用 helper，一次引入。注意 import 增补：`os/exec`、`runtime`——按 gofmt 分组。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/sshbroker/ -count=1 -run TestUploadDirSymlinkRootResolved -v`
Expected: FAIL（现状：walk 把 junction/symlink 根当文件传，open/read 目录报错，错误不含我们的语义）——unix 上若本机无建链权限会 SKIP，此时以 CI 为准，本地至少不红。

- [ ] **Step 3: 最小实现**——`uploadDir` 首行插入（现有签名与 doc 注释不动，doc 注释末尾追加一句）：

```go
func uploadDir(sc *sftp.Client, localRoot, remoteRoot string, ctr *countingWriter, res *UploadResult) error {
	// Root resolution (Plan 26): Upload's entry os.Stat FOLLOWS links (so a
	// linked dir root reaches here), but filepath.Walk lstats the root and
	// would misclassify it as a file. Resolve once up front so the walk starts
	// at the real directory; nested entries keep lstat semantics (Task 2 adds
	// an explicit refusal for symlinked sub-directories).
	resolved, rerr := filepath.EvalSymlinks(localRoot)
	if rerr != nil {
		return rerr
	}
	localRoot = resolved
	walkErr := filepath.Walk(localRoot, func(walkPath string, info os.FileInfo, err error) error {
```

（即在原 `walkErr := filepath.Walk(...)` 之前插入四行 + 赋值替换。）

- [ ] **Step 4: 跑测试确认通过 + 既有回归不破**

Run: `go test ./internal/sshbroker/ -count=1`
Expected: 全绿（含 Plan 24 的 `TestUploadSymlinkToSmallFileFollowsTarget` / `TestUploadBrokenSymlinkInDirErrors`）。

- [ ] **Step 5: Commit**

```bash
git add internal/sshbroker/upload.go internal/sshbroker/upload_test.go
git commit -m "feat(upload): symlink/junction upload root resolved via EvalSymlinks (Plan 26 T1)"
```

---

### Task 2: upload 嵌套符号目录显式拒绝（cap 无关）+ 语义文档

**Files:**
- Modify: `internal/sshbroker/upload.go`（walk 回调符号链接块，约 :154-172）
- Modify: `internal/mcpserver/server.go`（upload_file 工具 Description，补一句）
- Modify: `docs/scenarios.md`（upload 小节）
- Test: `internal/sshbroker/upload_test.go`（追加 3 个测试）

**Interfaces:**
- Consumes: T1 的 `makeDirLink` helper。
- Produces: 精确错误文案 `symlinked directory not uploaded: <path> — upload the target directory directly (following directory links recursively is not supported)`；文档引用此措辞。

**语义决策（本计划定死，评审按此判）**：根 = 跟链解析（T1）；嵌套 symlink→文件 = 跟链上传（Plan 24 不变）；**嵌套 symlink→目录 = 显式拒绝**（不递归跟链——环风险 + 双重访问风险，操作者应直传目标目录）；拒绝 **cap 无关**（cap==0 也要拒，语义一致性优先于省一次 stat）；断链 = 报错点名路径（Plan 24 不变）。

- [ ] **Step 1: 写失败测试**（追加；沿用 `mcpUploadCap`、`t.Skipf` 守卫）

```go
// TestUploadDirNestedSymlinkedDirRefused (Plan 26): a symlink→directory
// nested inside the upload root is REFUSED with a named error — pre-fix it
// fell into the file branch and died inside uploadFile's open/read with a
// misleading platform-dependent error. Refusal is cap-INDEPENDENT (armed here).
func TestUploadDirNestedSymlinkedDirRefused(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := makeDirLink(t, filepath.Join(src, "z-link"), t.TempDir()); err != nil {
		t.Skipf("dir link creation failed on this host (%v); skipping", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-nested-link")
	_, err := c.Upload(context.Background(), src, remoteDir, mcpUploadCap)
	if err == nil || !strings.Contains(err.Error(), "symlinked directory not uploaded") || !strings.Contains(err.Error(), "z-link") {
		t.Fatalf("want named refusal naming z-link, got: %v", err)
	}
	// Walk order is lexical: a.txt (< z-link) is uploaded BEFORE the refusal —
	// already-completed files remain (same contract as cap refusal, Plan 23).
	if g, derr := c.Download(context.Background(), filepath.Join(remoteDir, "a.txt"), 0); derr != nil || g.Content != "first\n" {
		t.Fatalf("a.txt must remain uploaded (derr=%v content=%q)", derr, g.Content)
	}
}

// TestUploadDirNestedSymlinkedDirRefusedNoCap: same refusal with cap==0 —
// the dir-symlink check must not live under the cap-armed branch.
func TestUploadDirNestedSymlinkedDirRefusedNoCap(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	if err := makeDirLink(t, filepath.Join(src, "z-link"), t.TempDir()); err != nil {
		t.Skipf("dir link creation failed on this host (%v); skipping", err)
	}
	if _, err := c.Upload(context.Background(), src, filepath.Join(t.TempDir(), "up"), 0); err == nil || !strings.Contains(err.Error(), "symlinked directory not uploaded") {
		t.Fatalf("cap==0 must still refuse, got: %v", err)
	}
}

// TestUploadJunctionNestedRefused_windows: the real-world Windows case —
// a junction inside the upload tree (OneDrive / dev-drive junctions).
// Windows-only (build-tag free: skips elsewhere).
func TestUploadJunctionNestedRefused_windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows junction test")
	}
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	if err := makeDirLink(t, filepath.Join(src, "z-junc"), t.TempDir()); err != nil {
		t.Skipf("junction creation failed (%v); skipping", err)
	}
	if _, err := c.Upload(context.Background(), src, filepath.Join(t.TempDir(), "up"), 0); err == nil || !strings.Contains(err.Error(), "symlinked directory not uploaded") {
		t.Fatalf("junction must be refused like a dir symlink, got: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/sshbroker/ -count=1 -run 'TestUploadDirNestedSymlinkedDirRefused|TestUploadJunctionNestedRefused' -v`
Expected: 嵌套拒绝两个 FAIL（现状报的是 open/read 目录的误导性错误，不含 "symlinked directory not uploaded"）。

- [ ] **Step 3: 实现**——把 walk 回调里的 Plan 24 符号链接块（现为 `if ctr.cap > 0 && info.Mode()&os.ModeSymlink != 0 { ... }`）整体替换为 cap 无关的三段式：

```go
		// Symlink handling (Plan 24 cap alignment, Plan 26 dir semantics):
		// Walk's FileInfo is lstat-based. For ANY symlink entry, re-stat with
		// follow. Target is a DIRECTORY → refuse with a named error (following
		// directory links recursively is not supported — loop/double-visit
		// risk; upload the target directory directly). Target is a file → the
		// followed size participates in the per-file cap gate when armed (the
		// transfer follows the link, so the check must too — Plan 24). A broken
		// link fails the re-stat and propagates as a walk error naming the path.
		// The dir-refusal is deliberately cap-INDEPENDENT.
		size := info.Size()
		if info.Mode()&os.ModeSymlink != 0 {
			st, err := os.Stat(walkPath)
			if err != nil {
				return err
			}
			if st.IsDir() {
				return fmt.Errorf("symlinked directory not uploaded: %s — upload the target directory directly (following directory links recursively is not supported)", walkPath)
			}
			size = st.Size()
		}
		if ctr.cap > 0 && size > ctr.cap {
			return capRefusedError(walkPath, size, ctr.cap)
		}
		return uploadFile(sc, walkPath, target, ctr, res)
```

注意：替换后 cap==0 时 symlink 条目也会跟链 re-stat（非 symlink 条目不受影响、无额外 syscall）——这是语义一致性的一部分，不是回归。

- [ ] **Step 4: 跑全包测试**

Run: `go test ./internal/sshbroker/ -count=1`
Expected: 全绿（T1 + 本任务 3 个新测试 + Plan 23/24 全部既有）。

- [ ] **Step 5: 语义文档**（三处，文案引用 Step 3 精确措辞）
  - `internal/mcpserver/server.go` upload_file Description：在 "a directory is uploaded recursively, preserving relative paths" 后补 `; a symlinked directory as the upload root is resolved to its target, while symlinked sub-directories inside it are refused (upload the target directly)`。
  - `docs/scenarios.md` upload 小节：加 2-3 行说明根跟链/嵌套拒绝/文件链跟传（Plan 24）三态 + 拒绝错误长什么样。
  - `docs/managing-servers.md` 若有 upload 段落同步；无则跳过（以 grep `upload` 定位为准，不凭记忆）。

- [ ] **Step 6: Commit**

```bash
git add internal/sshbroker/upload.go internal/sshbroker/upload_test.go internal/mcpserver/server.go docs/scenarios.md
git commit -m "feat(upload): nested symlinked dirs refused by name (cap-independent); root follows link — semantics documented (Plan 26 T2)"
```

---

### Task 3: `clear` 双角色补测 + 文档段落

**Files:**
- Test: `internal/cli/clear_test.go`（追加 1 个测试，复用 `withClearDirs`/`seedClearVault`/`stubClearExternals`）
- Modify: `clear` 的用户文档（以 `git grep -n "clear" docs/ README.md` 实际命中为准；预期 `README.md` 命令表附近或 `docs/getting-started.md`）

**Interfaces:**
- Consumes: 既有 `withClearDirs(t)` / `seedClearVault` / `roles.Save` / `stubClearExternals`。
- Produces: 无代码接口变化——纯钉住 + 文档。

- [ ] **Step 1: 写测试**（追加；双 role.json 同时在盘的组合）

```go
// TestEnumClearTargets_DualRoleMachine: a machine holding BOTH role.json
// locations (e.g. mid-migration) enumerates BOTH — the scan is deliberately
// role-blind (clear.go's scanClearTargets contract: catch every leftover
// regardless of what role.json claims).
func TestEnumClearTargets_DualRoleMachine(t *testing.T) {
	vd, _ := withClearDirs(t)
	seedClearVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	// Second role.json at the CLIENT location (roles.Save writes per-role
	// paths; write the client one by hand via a second Save + move, or call
	// roles.Save(RoleClient) if RolePath differs — see roles.RolePath).
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	stubClearExternals(t, nil, nil)

	got := enumClearTargets(roles.RoleServer)
	if n := strings.Count(strings.Join(got, "\n"), "role.json"); n != 2 {
		t.Fatalf("dual-role machine must enumerate BOTH role.json locations, got %d:\n%s", n, strings.Join(got, "\n"))
	}
}
```

（若 `roles.Save` 两个 role 写到同一 `vaultRolePath`（实现细节），改为直接 `os.WriteFile` 到 `roles.RolePath(roles.RoleClient)`——以 roles.go 实际为准，测试目标不变：两条 role 行。）

- [ ] **Step 2: 跑测试**——若双位置行为如预期则绿（这是 characterization：钉现状，不改 clear.go）。若红（只枚举一条），**停下上报**，不要擅自改 clear.go 语义——这是"文档化"任务不是行为变更任务。

Run: `go test ./internal/cli/ -count=1 -run TestEnumClearTargets -v`

- [ ] **Step 3: 文档段落**（在 Step 1 grep 命中的位置加）：

> `clear` 的扫描**与 role.json 声明的角色无关**（server 机可能残留 client 文件，反之亦然）：两处 role.json 位置、vault 目录与程序固定目录（cache DEK / serve log）双解析、env 覆盖（SSHMGR_SERVE_CERT 等）优先——全部 Stat-gated 枚举。role 只影响表头标签与安全网决策，不影响扫描范围。

- [ ] **Step 4: Commit**

```bash
git add internal/cli/clear_test.go README.md
git commit -m "test+docs: clear dual-role enumeration pinned and documented (Plan 26 T3)"
```

---

### Task 4: docs/backlog.md 固化 + 注释指向

**Files:**
- Create: `docs/backlog.md`
- Modify: `internal/mcpserver/tunnels.go:14`（"tracked backlog item" 处）、`internal/mcpserver/revoke_semantics_test.go:28`、`docs/agent-access.md`（若提及隧道急停/backlog 处，grep 定位）
- Modify: `docs/README.md`（若有文档索引，加一行；无索引则跳过）

**Interfaces:**
- Consumes: xcheck 闭环结论（CLOSE.md M6 / 执行附录 A3 / A9——引用其语义，不引用 .xcheck 内部路径，那是 gitignore 的会话产物）。
- Produces: `docs/backlog.md`——后续 plan 的取货架。

- [ ] **Step 1: 创建 docs/backlog.md**，固定 5 项（每项：一句话语义、为什么不现在做、来源计划）：

```markdown
# Backlog（已裁决、未排期）

决策记录在案（xcheck 收敛 2026-08-16 / Plan 25），均为 owner 拍板"暂不改行为"：

1. **serve 隧道 owner 急停**（kill-tunnel CLI 或 revoke 级联拆隧道）——现状：revoke 后既有隧道存活至创建后 ~10 分钟回收或 broker 重启，无 owner 侧拆除手段（close_port 是 MCP 请求，revoke 后 401）。
2. **隧道回收活动感知**（Touch 活动刷新）——现状：回收按创建时间计（Touch 无生产调用方），持续流量不延长。
3. **离线 cache 快照失效机制**（snapshot epoch/serial）——现状：revoke/rotate 不擦已落盘快照，唯一失效手段是轮换服务器凭据；`cache-tokens revoke` 只断"拉新"。
4. **受控监听地址**（forward_port listen_host）——现状：转发只绑 broker 机环回，远程 serve 模式下 agent 拿到的地址本机不可达（文档已如实声明）。
5. **doctor serve 探活二期**（绿/黄/红语义）——现状：doctor 首版只做本机自检（P4，独立 plan）。
```

- [ ] **Step 2: 注释指向**——tunnels.go:14 与 revoke_semantics_test.go:28 的 "backlog" 字样后补 `(see docs/backlog.md)`；`docs/agent-access.md` 断连语义小节末尾加一行 "未实现的拆除手段见 docs/backlog.md"。
- [ ] **Step 3: 验证无孤儿**——`git grep -n "backlog" -- internal/ docs/ README.md` 输出逐条过目，每条要么指向 docs/backlog.md 要么本来就是引用它。
- [ ] **Step 4: Commit**

```bash
git add docs/backlog.md internal/mcpserver/tunnels.go internal/mcpserver/revoke_semantics_test.go docs/agent-access.md docs/README.md
git commit -m "docs: consolidate standing backlog into docs/backlog.md; code comments point at it (Plan 26 T4)"
```

---

### Task 5: cosmetic 批（Plan 25 终审豁免的 4 处）

**Files:**
- Modify: `internal/eval/summary_test.go:137`（t.Log 字符串）
- Modify: `internal/mcpserver/types.go:47-48`（注释）
- Modify: `internal/mcpserver/core.go:289`（注释）
- Modify: `internal/mcpserver/core.go:412`、`internal/mcpserver/run.go:43`、`internal/mcpserver/tunnels.go` "idle sweeper" 字样（注释措辞，标识符不动）

**Constraints（本任务特有）**：全部是注释/日志字符串级改动，**零行为变化**；涉及非 ASCII 标点的行编辑后必须逐字节复核（Plan 25 教训：haiku 修 U+201C/U+201D 引入过回归）；`t.Log` 字符串改动不需要新断言。

- [ ] **Step 1: 四类修改**
  1. summary_test.go:137：`~10 min idle-timeout` → `~10 min after-creation auto-close`（仅此片段，行内其余不动）。
  2. types.go:47-48：`LocalPath is read from the broker's (= the agent's) filesystem` → `LocalPath is read from the broker's filesystem`（serve 模式下 broker ≠ agent；jsonschema 行已有 "on the machine the broker runs on"）。
  3. core.go:289：同样删 `(= the agent's)` 括号。
  4. "idle sweeper" 注释措辞（core.go:412、run.go:43、tunnels.go 命中处）：改为 "tunnel sweeper" 或补 "(creation-based)"——原则：注释不得暗示活动感知；标识符（forwardSweepInterval、SweepIdle）**不动**。
- [ ] **Step 2: 字节级复核**——`git diff` 逐行过目，确认改动行内未引入任何意外的标点/空白变化（对照 Plan 25 T4 回归教训）。
- [ ] **Step 3: 全量验证**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿零输出。

- [ ] **Step 4: Commit**

```bash
git add internal/eval/summary_test.go internal/mcpserver/types.go internal/mcpserver/core.go internal/mcpserver/run.go
git commit -m "docs(cosmetic): creation-based reclaim wording, broker-filesystem parentheticals, idle-sweeper naming (Plan 25 leftovers, Plan 26 T5)"
```

---

## 验收（整 plan）

1. `go test ./... -count=1` 本地全绿；push 后 **CI 双 lane 绿**（ci.yml 现已是门禁）。
2. junction/符号目录语义三态有测试钉住且文档与错误文案一致。
3. `git grep -n "idle" -- internal/ docs/ README.md` 无"无活动回收"残留表述。
4. `docs/backlog.md` 存在且被代码注释指向。
