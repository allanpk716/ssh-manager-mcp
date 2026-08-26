# Plan 40 第一批 · 多实例离线缓存 CLI 闭环 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 同一台 client 机上 N 个 agent（各持不同 project token / 设备码 / profile）各自拥有独立的离线 cache 实例（目录 + DEK + 审计 + 时效策略），CLI 全闭环（pull / status / mcp / clear / roles），并把 MAX_OFFLINE 从进程 env 迁移为 per-instance 配置文件。

**Architecture:** 实例 = 设备码 name = profile 授权单元（三位一体）。命名实例落 `UserConfigDir()/ssh-manager/instances/<name>/`，DEK 落 `VaultDir()/cache-dek-<name>.key`（新增 `SSHMGR_CACHE_DEK_DIR` 目录级 env seam）。serve 经 `X-Sshmgr-Device-Name` 响应头下发设备码 name；client 在 `DoPull` 写盘前做身份门禁（默认实例三分支 + `--instance` 强一致 + 物理碰撞检测）。路径层以 `CachePathsFor(instance)` 参数化，`CachePaths()` 等旧签名全部保留为零改动 wrapper（TUI/doctor 第一批不动）。

**Tech Stack:** Go（既有依赖：cobra、SQLite、httptest；无新依赖）。

**Spec:** `docs/superpowers/specs/2026-08-26-plan-40-multi-instance-cache-design.md.rev3.md`（权威 = rev3；本 plan 从 spec 立论，执行者两份都读）。P0（spec §1）已独立合并（merge 8643858），**不在本 plan 内**。

## Global Constraints

- **worktree 纪律**：在独立 linked worktree 执行（superpowers:using-git-worktrees）；主 worktree 有其他并发 agent，绝不直接动。
- **零迁移承诺（spec §5）**：存量单实例机器无 flag 的 pull/mcp/status 行为与现版逐字节等价——现有测试零改动通过是硬门槛（spec §6.7）。`CachePaths()`/`CacheDekPath()`/`DekProvider` 旧调用面只增不改语义。
- **首批 enroll 不自动归位（spec §2.4）**：无 `--instance` 的首次 enroll 仍落默认目录；自动归位属第二批。
- **TUI/doctor/向导第一批不动**（spec §2.8）：TUI client 页保持默认实例视图；doctor 不感知命名实例（已知残余 §9.7）。
- **name 白名单（spec §2.1，冻结）**：`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$`（1-64，首尾字母数字）+ DOS 保留名按**首个 `.` 之前的首段**判定 `{CON,PRN,AUX,NUL,COM1-9,LPT1-9}`（casefold 后比对）。casefold = **ASCII 小写**（SQLite `lower()` 同语义）。
- **env × flag 互斥（spec §2.2，冻结）**：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 显式设置 **且** `--instance` 显式给出 → CLI 层报错。`SSHMGR_CACHE_DEK_DIR`（本 plan 新增的 DEK 目录 seam）与 `--instance` **可共存**（目录级重定位，天然按实例派生）。
- **DEK env seam 纪律**（grilling 定案）：每实例 DEK 的生产路径必须有 env seam → `SSHMGR_CACHE_DEK_DIR`。
- **门禁三分支（spec §2.4 rev3，冻结）**：默认目录 `cache.bin` 存在时——① meta 可解析且 `device_name` 非空且 ≠ 响应头 name → 拒（三选一文案）；② meta 可解析但 `device_name` 空串（存量未登记）→ 放行 + 随本次 pull 补记；③ meta 不可解析 → 拒（真异常态）。`cache.auth.json` 写序恒在 `DoPull` 成功之后。
- **换码 runbook（spec §2.4，冻结）**：清默认目录四件套 = `cache.auth.json` + `cache.bin` + `cache.meta.json` + `quarantine/`；`cache.config.json` **保留继承**。
- **MAX_OFFLINE（spec §3）**：优先级 env > `cache.config.json` > 关；只搬 MAX_OFFLINE（`--cache-max-age` 不进 config，YAGNI）；校验规则与 env 完全同构（≥1h，非法 fail-closed）；原子写用 `atomicWriteUnique`。
- **测试指引（spec §9.5）**：多实例测试走 `--instance` / `Instance` 参数，**不设** `SSHMGR_CACHE_DIR`；基目录用 `t.Setenv("APPDATA", dir)` + `t.Setenv("XDG_CONFIG_HOME", dir)` 重定向（roles_test/clear_test 先例）；per-instance DEK 用 `SSHMGR_CACHE_DEK_DIR` + 真 `FileKeyProvider`（不要 withDEK 共享 mem——那会抹掉 DEK 隔离）。`SSHMGR_CACHE_DEK` env 仅单实例场景。
- **提交纪律**：每任务一 commit（gofmt 过，`go build ./...` + `go test ./...` 绿才 commit）；commit message 带任务号；**禁止裸 `git stash`**（共享栈）。
- 目标版本面：v0.11.0（spec §5 compat 两行：v0.11×v0.10 受限 / v0.11×v0.11 全功能）。发版编排不在本 plan。

---

### Task 1: `internal/instname` — 设备码 name 白名单（双端共享）

**Files:**
- Create: `internal/instname/instname.go`
- Test: `internal/instname/instname_test.go`

**Interfaces:**
- Consumes: 无（零依赖小包）。
- Produces: `instname.Valid(name string) error`（nil=合法；错误文案含原因，可 `%v` 打印）；`instname.Fold(name string) string`（ASCII 小写折叠）。后续 store/cli/clientops/paths 都 import 它。

- [ ] **Step 1: 写失败测试**

```go
package instname

import "testing"

func TestValid(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"a", true}, {"agentA", true}, {"laptop-agentA", true},
		{"con.foo", false}, {"COM1.x", false}, {"nul.tar.gz", false}, // 首段保留名（实测 MkdirAll 必败）
		{"CON", false}, {"aux", false}, {"lpt9", false},
		{"foo.bar", true}, {"foo", true}, {"A1-b_2.c", true},
		{"foo.", false}, {".foo", false}, {"foo-", false}, {"-foo", false}, // 首尾必须字母数字
		{"a b", false}, {"a/b", false}, {"../x", false}, {"a\\b", false},   // 路径穿越/非法字符
		{"", false},
		{string(make([]byte, 0)) + "0123456789012345678901234567890123456789012345678901234567890123", true},  // 64
		{"01234567890123456789012345678901234567890123456789012345678901234", false}, // 65
	}
	for _, tc := range cases {
		err := Valid(tc.name)
		if tc.ok && err != nil {
			t.Errorf("Valid(%q) = %v, want nil", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("Valid(%q) = nil, want error", tc.name)
		}
	}
}

func TestFoldASCIIONLY(t *testing.T) {
	if Fold("AgentA") != "agenta" {
		t.Fatalf("Fold = %q", Fold("AgentA"))
	}
	// 非 ASCII 不折叠（与 SQLite lower() 同语义；Kelvin sign 不得折叠成 k）
	if Fold("K\xE2\x84\xAA") != "k\xE2\x84\xAA" && Fold("K") != "k" {
		t.Fatal("Fold must be ASCII-only")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/instname/`
Expected: FAIL（package 不存在 / Valid 未定义）。

- [ ] **Step 3: 实现**

```go
// Package instname validates device-code / cache-instance names (Plan 40 §2.1).
// One rule set shared by BOTH ends: the server rejects illegal names at
// cache-tokens add/bind (source gate), the client re-validates before any
// instance-directory write (defense) — the name becomes a directory/file name
// (instances/<name>/, cache-dek-<name>.key), so this closes path traversal and
// "dead on arrival" Windows filesystem forms.
package instname

import (
	"fmt"
	"regexp"
	"strings"
)

var pattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$`)

// dosReserved are Windows reserved device names. The check applies to the
// FIRST DOT-SEGMENT of the name (experiment-verified, spec §0.10): con.foo /
// COM1.x / nul.tar.gz pass a whole-name equality check but MkdirAll fails.
var dosReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Valid reports whether name is a legal device/instance name. The returned
// error text is standalone (no caller context needed) and always leads with
// "invalid device name" so wrapping call sites keep a stable grep anchor.
func Valid(name string) error {
	if !pattern.MatchString(name) {
		return fmt.Errorf("invalid device name %q: must be 1-64 chars matching ^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$ (letters/digits/dots/underscores/hyphens; alphanumeric first and last)", name)
	}
	seg := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		seg = name[:i]
	}
	if dosReserved[strings.ToUpper(seg)] {
		return fmt.Errorf("invalid device name %q: first dot-segment %q is a reserved device name on Windows (CON/PRN/AUX/NUL/COM1-9/LPT1-9)", name, seg)
	}
	return nil
}

// Fold lowercases ASCII letters only — the same casefold SQLite's lower()
// applies, which is what the server-side uniqueness queries rely on. Non-ASCII
// bytes pass through unchanged (a Unicode fold could merge legacy free-text
// names that Windows/SQLite treat as distinct).
func Fold(name string) string {
	b := []byte(name)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/instname/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/instname/
git commit -m "feat(instname): Plan 40 T1 设备码 name 白名单共享包——charset+首段 DOS 保留名+ASCII casefold"
```

---

### Task 2: `paths.CacheDekPathFor(instance)` — per-instance DEK 路径 + `SSHMGR_CACHE_DEK_DIR` seam

**Files:**
- Modify: `internal/paths/paths.go`（CacheDekPath 处，~line 69-82）
- Test: `internal/paths/paths_test.go`（追加）

**Interfaces:**
- Consumes: `instname.Valid`。
- Produces: `paths.CacheDekPathFor(instance string) (string, error)`——优先级 `SSHMGR_CACHE_DEK`（单文件完全覆盖，既有语义不变）> `SSHMGR_CACHE_DEK_DIR` env > `VaultDir()`；instance=="" → `cache-dek.key`；instance!="" → `cache-dek-<name>.key`，且 instance 先过 `instname.Valid`（fail-closed，路径穿越后盾）。`paths.CacheDekPath()` 变为零行为 wrapper。新常量 `CacheDekDirEnv = "SSHMGR_CACHE_DEK_DIR"`（供 clear 枚举与测试引用）。

- [ ] **Step 1: 写失败测试**（`paths_test.go` 追加）

```go
func TestCacheDekPathFor(t *testing.T) {
	vd := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", vd)
	// default instance: legacy filename, unchanged
	p, err := CacheDekPathFor("")
	if err != nil || filepath.Base(p) != "cache-dek.key" || filepath.Dir(p) != vd {
		t.Fatalf("default = %q, %v", p, err)
	}
	// named instance: per-instance variant
	p, err = CacheDekPathFor("agentA")
	if err != nil || filepath.Base(p) != "cache-dek-agentA.key" || filepath.Dir(p) != vd {
		t.Fatalf("agentA = %q, %v", p, err)
	}
	// single-file env wins over everything (existing escape hatch)
	t.Setenv("SSHMGR_CACHE_DEK", filepath.Join(vd, "x.key"))
	p, err = CacheDekPathFor("agentA")
	if err != nil || p != filepath.Join(vd, "x.key") {
		t.Fatalf("env override = %q, %v", p, err)
	}
	// illegal instance name: fail-closed (path-traversal backstop)
	t.Setenv("SSHMGR_CACHE_DEK", "")
	if _, err := CacheDekPathFor("../evil"); err == nil {
		t.Fatal("illegal instance must be refused")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/paths/ -run TestCacheDekPathFor`
Expected: FAIL（CacheDekPathFor 未定义）。

- [ ] **Step 3: 实现**（paths.go：替换 CacheDekPath 函数体）

```go
// CacheDekDirEnv relocates the WHOLE cache-DEK directory (default + every
// per-instance variant). The single-file SSHMGR_CACHE_DEK override still wins
// over it. Added with Plan 40: per-instance DEK paths are new production paths
// and must keep an env seam (the SSHMGR_CACHE_DEK lesson) — tests and
// migrations point this at a temp dir instead of the real vault dir.
const CacheDekDirEnv = "SSHMGR_CACHE_DEK_DIR"

// CacheDekPathFor returns the cache-DEK path for one cache instance
// ("" = default instance). Priority: SSHMGR_CACHE_DEK (single-file full
// override — mutually exclusive with --instance at the CLI layer) >
// SSHMGR_CACHE_DEK_DIR > VaultDir(). A named instance must pass the device-name
// whitelist before it reaches filepath.Join (path-traversal backstop, spec §4).
func CacheDekPathFor(instance string) (string, error) {
	if v := os.Getenv("SSHMGR_CACHE_DEK"); v != "" {
		return v, nil
	}
	root := os.Getenv(CacheDekDirEnv)
	if root == "" {
		vd, err := VaultDir()
		if err != nil {
			return "", err
		}
		root = vd
	}
	if instance == "" {
		return filepath.Join(root, CacheDekFilename), nil
	}
	if verr := instname.Valid(instance); verr != nil {
		return "", verr
	}
	return filepath.Join(root, "cache-dek-"+instance+".key"), nil
}

// CacheDekPath returns the DEFAULT instance's cache-DEK path (existing callers unchanged).
func CacheDekPath() (string, error) { return CacheDekPathFor("") }
```

（paths.go 需新增 import `"ssh-manager-mcp/internal/instname"`。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/paths/`
Expected: PASS（含既有用例零改动——`SSHMGR_CACHE_DEK_DIR` 未设时行为不变）。

- [ ] **Step 5: Commit**

```bash
git add internal/paths/
git commit -m "feat(paths): Plan 40 T2 CacheDekPathFor(instance)+SSHMGR_CACHE_DEK_DIR seam——per-instance DEK 变体+白名单后盾"
```

---

### Task 3: `clientops.CachePathsFor(instance)` + 实例根辅助

**Files:**
- Modify: `internal/clientops/clientops.go`（CachePaths 处，~line 126-138）
- Test: `internal/clientops/instance_paths_test.go`（新建）

**Interfaces:**
- Consumes: `instname.Valid`。
- Produces:
  - `clientops.CachePathsFor(instance string) (dir, bin, meta, audit string, err error)`——`SSHMGR_CACHE_DIR` env（完全覆盖，非空时忽略 instance）> `UserConfigDir()/ssh-manager/instances/<name>` > `UserConfigDir()/ssh-manager/`；instance 先过白名单。`CachePaths()` ≡ `CachePathsFor("")`（15 个存量 caller 零改动）。
  - `clientops.InstancesRoot() (string, error)`——与默认目录同源解析后的 `instances/`（env 覆盖时 = `<env>/instances`，自洽）。
  - `clientops.ListInstances() ([]string, error)`——`InstancesRoot()` 下全部子目录名（排序返回；root 不存在 → nil, nil）。

- [ ] **Step 1: 写失败测试**

```go
package clientops

import (
	"os"
	"path/filepath"
	"testing"
)

// redirectUserConfigDir pins os.UserConfigDir to a temp dir (roles_test /
// clear_test precedent). Multi-instance tests must NOT set SSHMGR_CACHE_DIR
// (spec §9.5: env and --instance are mutually exclusive).
func redirectUserConfigDir(t *testing.T) string {
	t.Helper()
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)       // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", userDir) // and on Unix
	t.Setenv("SSHMGR_CACHE_DIR", "")
	return userDir
}

func TestCachePathsFor_InstanceRouting(t *testing.T) {
	userDir := redirectUserConfigDir(t)
	base := filepath.Join(userDir, "ssh-manager")

	dir, bin, meta, audit, err := CachePathsFor("")
	if err != nil || dir != base || bin != filepath.Join(base, "cache.bin") ||
		meta != filepath.Join(base, "cache.meta.json") || audit != filepath.Join(base, "cache-audit.log") {
		t.Fatalf("default = %q,%q,%q,%q,%v", dir, bin, meta, audit, err)
	}
	// CachePaths() is the zero-change wrapper
	d2, _, _, _, _ := CachePaths()
	if d2 != base {
		t.Fatalf("CachePaths() = %q, want %q", d2, base)
	}

	idir := filepath.Join(base, "instances", "agentA")
	dir, bin, _, _, err = CachePathsFor("agentA")
	if err != nil || dir != idir || bin != filepath.Join(idir, "cache.bin") {
		t.Fatalf("agentA = %q,%q,%v", dir, bin, err)
	}

	// env wins entirely (escape hatch; CLI layer enforces the mutex)
	t.Setenv("SSHMGR_CACHE_DIR", filepath.Join(userDir, "override"))
	dir, _, _, _, _ = CachePathsFor("agentA")
	if dir != filepath.Join(userDir, "override") {
		t.Fatalf("env override must win: %q", dir)
	}

	// illegal name: fail-closed
	t.Setenv("SSHMGR_CACHE_DIR", "")
	if _, _, _, _, err := CachePathsFor("../evil"); err == nil {
		t.Fatal("illegal instance must be refused before Join")
	}
}

func TestListInstances(t *testing.T) {
	userDir := redirectUserConfigDir(t)
	root := filepath.Join(userDir, "ssh-manager", "instances")
	if got, err := ListInstances(); err != nil || len(got) != 0 {
		t.Fatalf("missing root = %v, %v", got, err)
	}
	for _, n := range []string{"agentB", "agentA"} {
		if err := os.MkdirAll(filepath.Join(root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-dir"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ListInstances()
	if err != nil || len(got) != 2 || got[0] != "agentA" || got[1] != "agentB" {
		t.Fatalf("ListInstances = %v, %v", got, err)
	}
	if r, err := InstancesRoot(); err != nil || r != root {
		t.Fatalf("InstancesRoot = %q, %v", r, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run 'TestCachePathsFor_InstanceRouting|TestListInstances'`
Expected: FAIL（CachePathsFor/ListInstances 未定义）。

- [ ] **Step 3: 实现**（clientops.go：替换 CachePaths 函数）

```go
// CachePathsFor resolves the cache directory for ONE instance ("" = the
// default instance — legacy single-instance machines keep byte-identical
// behavior). Priority: SSHMGR_CACHE_DIR (explicit full override — the
// CLI layer rejects combining it with --instance) > instances/<name> > the
// default dir. A named instance must pass the whitelist before Join.
func CachePathsFor(instance string) (dir, bin, meta, audit string, err error) {
	if instance != "" {
		if verr := instname.Valid(instance); verr != nil {
			return "", "", "", "", verr
		}
	}
	if dir = os.Getenv("SSHMGR_CACHE_DIR"); dir == "" {
		base, derr := os.UserConfigDir()
		if derr != nil {
			return "", "", "", "", derr
		}
		dir = filepath.Join(base, "ssh-manager")
		if instance != "" {
			dir = filepath.Join(dir, "instances", instance)
		}
	}
	return dir, filepath.Join(dir, "cache.bin"), filepath.Join(dir, "cache.meta.json"), filepath.Join(dir, "cache-audit.log"), nil
}

// CachePaths resolves the DEFAULT instance's paths (zero-change wrapper; every
// pre-Plan-40 caller — TUI client page, doctor, clear — keeps this view).
func CachePaths() (dir, bin, meta, audit string, err error) {
	return CachePathsFor("")
}

// InstancesRoot is where named instances live: "instances/" under the
// UserConfigDir base — deliberately NOT env-redirected: SSHMGR_CACHE_DIR is a
// single-slot full override (CachePathsFor ignores the instance when it is
// set), so following it here would create two competing instances/ roots.
func InstancesRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ssh-manager", "instances"), nil
}
```

**注意**：`InstancesRoot` 有意**不**看 `SSHMGR_CACHE_DIR`——env 覆盖时 `CachePathsFor(instance)` 已经直接返回 env 目录（instance 被忽略），如果 root 也跟着 env 走会造成"两套 instances/"；真实形态 = 实例恒在 UserConfigDir 下，env 只是单槽完全覆盖（上面代码已按此实现，注释与代码一致）。

```go
// ListInstances returns the sorted directory names under InstancesRoot()
// (nil, nil when the root does not exist — an empty machine). A directory is
// an instance SLOT; presence of material inside is the caller's concern.
func ListInstances() ([]string, error) {
	root, err := InstancesRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
```

（clientops.go 增 import `"sort"`、`"ssh-manager-mcp/internal/instname"`；`errors`/`io/fs` 已有。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/`
Expected: PASS（既有用例零改动——它们全部走默认实例或显式 env）。

- [ ] **Step 5: Commit**

```bash
git add internal/clientops/
git commit -m "feat(clientops): Plan 40 T3 CachePathsFor(instance)+InstancesRoot/ListInstances——默认路径零行为 wrapper"
```

---

### Task 4: `DekProvider` 实例参数化（seam 签名变更 + 机械接线）

**Files:**
- Modify: `internal/clientops/dek_windows.go`、`internal/clientops/dek_unix.go`、`internal/clientops/dek.go`
- Modify: `internal/clientops/quarantine.go:93`（`DekProvider()` → `DekProvider("")`，本任务先传空，Task 5 再穿透）
- Modify: `internal/clientops/clientops.go`（DoPull/LoadCacheSnapshot 内 `loadOrCreateDEK()`/`loadDEK()` → 传 `""`）
- Test: 既有全套（`internal/clientops/`、`internal/cli/`）+ helper 更新

**Interfaces:**
- Produces: `clientops.DekProvider` 变为 `var DekProvider = func(instance string) store.KeyProvider`；私有 `loadOrCreateDEK(instance string)` / `loadDEK(instance string)`。测试 seam 用法变为 `DekProvider = func(string) store.KeyProvider { return mem }`。

- [ ] **Step 1: 更新两个测试 helper（先红）**

`internal/clientops/helpers_test.go` 的 withDEK：

```go
func withDEK(t *testing.T) *store.MemKeyProvider {
	t.Helper()
	mem := &store.MemKeyProvider{}
	prev := DekProvider
	DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { DekProvider = prev })
	return mem
}
```

`internal/cli/` 侧同款副本（`rg -n "DekProvider" internal/cli --type go` 找齐：`cache_test.go`、`mcp_cache_test.go` 里的内联 swap 全部改为 `func(string) store.KeyProvider` 形态）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ ./internal/cli/`
Expected: 编译 FAIL（`DekProvider` 赋值签名不匹配）。

- [ ] **Step 3: 实现**

dek_windows.go / dek_unix.go（两份同型，只改 var 定义与注释）：

```go
var DekProvider = func(instance string) store.KeyProvider {
	pth, err := paths.CacheDekPathFor(instance)
	if err != nil || pth == "" {
		return &store.FileKeyProvider{} // last-resort default (test env with no fixed path)
	}
	return &store.FileKeyProvider{Path: pth}
}
```

dek.go：

```go
func loadOrCreateDEK(instance string) ([]byte, error) {
	kp := DekProvider(instance)
	...（其余不变）
}

func loadDEK(instance string) ([]byte, error) {
	return DekProvider(instance).Get()
}
```

clientops.go：`loadOrCreateDEK()` → `loadOrCreateDEK("")`（DoPull 内）、`loadDEK()` → `loadDEK("")`（LoadCacheSnapshot 内）；quarantine.go：`DekProvider()` → `DekProvider("")`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go build ./... && go test ./internal/clientops/ ./internal/cli/`
Expected: PASS（本任务是零行为机械变更）。

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "refactor(clientops): Plan 40 T4 DekProvider/loadOrCreateDEK/loadDEK 实例参数化——seam 签名先行,行为零变化"
```

---

### Task 5: clientops 实例穿透——For-变体全家 + `PullOpts.Instance`

**Files:**
- Modify: `internal/clientops/clientops.go`（ReadCacheCred/WriteCacheCred/CacheCredPath/LoadCacheSnapshot/MaybeLazyPull/NewCacheReloader/CacheReloader.Check/DoPull）
- Modify: `internal/clientops/quarantine.go`（QuarantineCacheFor）
- Test: `internal/clientops/instance_routing_test.go`（新建）

**Interfaces:**
- Consumes: Task 3 `CachePathsFor`、Task 4 `loadOrCreateDEK(instance)`。
- Produces（全部保留无后缀 wrapper = 旧调用面零改动）：
  - `PullOpts.Instance string`（""=默认实例）
  - `CacheCredPathFor(instance string) (string, error)`、`ReadCacheCredFor(instance string) (*CacheCred, error)`、`WriteCacheCredFor(instance string, cred *CacheCred) error`
  - `LoadCacheSnapshotFor(instance string) (*store.Snapshot, error)`
  - `MaybeLazyPullFor(instance string, maxAge time.Duration) error`
  - `NewCacheReloaderFor(instance string, maxAge time.Duration) *CacheReloader`（`CacheReloader` 增私有 `instance` 字段；`Check()` 内部改用 `LoadCacheSnapshotFor(r.instance)` 与 `MaybeLazyPullFor(r.instance, r.maxAge)`）
  - `QuarantineCacheFor(instance string, reason string) (QuarantineResult, error)`
  - `DoPull` 内部：路径/DEK 改用 `o.Instance` 解析；`WriteCacheCred` 不在 DoPull 内（CLI 层职责，不变）。
- **本任务不做门禁**（Task 9/10）——DoPull 只换路径/DEK 解析。

- [ ] **Step 1: 写失败测试**

```go
package clientops

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInstanceRouting_PullCredLoadQuarantine: one pinned pull with
// Instance="agentA" must land every artifact in instances/agentA/ (bin, meta,
// auth via WriteCacheCredFor), load back through LoadCacheSnapshotFor, and
// QuarantineCacheFor must destroy ONLY that instance's slot.
func TestInstanceRouting_PullCredLoadQuarantine(t *testing.T) {
	userDir := redirectUserConfigDir(t)
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir) // real per-instance FileKeyProvider
	t.Setenv("SSHMGR_CACHE_DEK", "")

	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(time.Now().UTC().Format(http.TimeFormat)), nil))
	if err := DoPull(url, "code", pin, PullOpts{Instance: "agentA"}); err != nil {
		t.Fatalf("instance pull: %v", err)
	}
	idir := filepath.Join(userDir, "ssh-manager", "instances", "agentA")
	if _, err := os.Stat(filepath.Join(idir, "cache.bin")); err != nil {
		t.Fatalf("instance bin missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(idir, "cache.meta.json")); err != nil {
		t.Fatalf("instance meta missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dekDir, "cache-dek-agentA.key")); err != nil {
		t.Fatalf("per-instance DEK missing: %v", err)
	}
	// default slot untouched
	if _, err := os.Stat(filepath.Join(userDir, "ssh-manager", "cache.bin")); !os.IsNotExist(err) {
		t.Fatal("default slot must stay empty")
	}

	// cred write/read round-trip in the instance slot
	if err := WriteCacheCredFor("agentA", &CacheCred{URL: url, Token: "code", Pin: pin}); err != nil {
		t.Fatalf("WriteCacheCredFor: %v", err)
	}
	cred, err := ReadCacheCredFor("agentA")
	if err != nil || cred == nil || cred.Token != "code" {
		t.Fatalf("ReadCacheCredFor = %+v, %v", cred, err)
	}
	if _, err := os.Stat(filepath.Join(idir, "cache.auth.json")); err != nil {
		t.Fatalf("instance auth missing: %v", err)
	}

	// load back (real FileKeyProvider DEK)
	snap, err := LoadCacheSnapshotFor("agentA")
	if err != nil || snap == nil {
		t.Fatalf("LoadCacheSnapshotFor: %v", err)
	}

	// quarantine destroys ONLY the instance slot (401-shaped trigger is DoPull's
	// business; here we call the routine directly)
	if _, qerr := QuarantineCacheFor("agentA", serverRejectedReason); qerr != nil {
		t.Fatalf("QuarantineCacheFor: %v", qerr)
	}
	if _, err := os.Stat(filepath.Join(idir, "cache.auth.json")); !os.IsNotExist(err) {
		t.Fatal("instance auth must be deleted by quarantine")
	}
	if _, err := os.Stat(dekDir + string(filepath.Separator) + "cache-dek-agentA.key"); !os.IsNotExist(err) {
		t.Fatal("instance DEK must be deleted by quarantine")
	}
	entries, _ := os.ReadDir(filepath.Join(idir, "quarantine"))
	if len(entries) == 0 {
		t.Fatal("quarantine/ under the instance dir must hold the isolated bin/manifest")
	}

	// a second instance survives untouched
	if err := DoPull(url, "code", pin, PullOpts{Instance: "agentB"}); err != nil {
		t.Fatalf("agentB pull: %v", err)
	}
	if _, err := QuarantineCacheFor("agentA", serverRejectedReason); err != nil {
		t.Fatalf("re-quarantine agentA (idempotent): %v", err)
	}
	if _, err := LoadCacheSnapshotFor("agentB"); err != nil {
		t.Fatalf("agentB must stay loadable: %v", err)
	}
}
```

（需 import `"net/http"`——`http.TimeFormat`。文件顶部补齐。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run TestInstanceRouting`
Expected: 编译 FAIL（PullOpts.Instance / For-变体未定义）。

- [ ] **Step 3: 实现**

clientops.go 逐点（全部"新函数 + 旧名 wrapper"模式）：

```go
type PullOpts struct {
	AllowPlain bool
	Timeout    time.Duration
	StatusOut  io.Writer
	// Instance routes this pull to instances/<name>/ ("" = the default slot).
	// Validated by CachePathsFor; combined with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK
	// it is rejected at the CLI layer (mutex, spec §2.2).
	Instance string
}
```

DoPull 内三处：
- `dek, err := loadOrCreateDEK("")` → `loadOrCreateDEK(o.Instance)`
- `_, bin, metaPath, _, err := CachePaths()` → `_, bin, metaPath, _, err := CachePathsFor(o.Instance)`（并把这句**上移到 EncryptWithKey 之前**——Task 9 的门禁要在写盘前读既有 meta，路径必须先解析；encrypt 用同一 dir 的变量）
- 其余（MkdirAll/写序/quarantine manifest 清理）不变。

```go
func CacheCredPathFor(instance string) (string, error) {
	dir, _, _, _, err := CachePathsFor(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache.auth.json"), nil
}
func CacheCredPath() (string, error) { return CacheCredPathFor("") }

func ReadCacheCredFor(instance string) (*CacheCred, error) { /* 原 ReadCacheCred 体，p 来自 CacheCredPathFor(instance) */ }
func ReadCacheCred() (*CacheCred, error) { return ReadCacheCredFor("") }

func WriteCacheCredFor(instance string, cred *CacheCred) error { /* 原体，p 来自 CacheCredPathFor(instance) */ }
func WriteCacheCred(cred *CacheCred) error { return WriteCacheCredFor("", cred) }

func LoadCacheSnapshotFor(instance string) (*store.Snapshot, error) {
	// 原 LoadCacheSnapshot 体，首行 CachePaths() → CachePathsFor(instance)；
	// loadDEK("") → loadDEK(instance)；QuarantineCache(...) → QuarantineCacheFor(instance, ...)
}
func LoadCacheSnapshot() (*store.Snapshot, error) { return LoadCacheSnapshotFor("") }
```

MaybeLazyPull → `MaybeLazyPullFor(instance string, maxAge time.Duration) error`（原体：`ReadCacheCred()` → `ReadCacheCredFor(instance)`、`CachePaths()` → `CachePathsFor(instance)`、`DoPull(..., PullOpts{Timeout:..., StatusOut:..., Instance: instance})`）；wrapper `MaybeLazyPull(maxAge)` = `MaybeLazyPullFor("", maxAge)`。

CacheReloader：

```go
type CacheReloader struct {
	bin      string
	instance string
	maxAge   time.Duration
	sum      []byte
}

func NewCacheReloaderFor(instance string, maxAge time.Duration) *CacheReloader {
	_, bin, _, _, err := CachePathsFor(instance)
	if err != nil {
		return &CacheReloader{instance: instance, maxAge: maxAge}
	}
	return &CacheReloader{bin: bin, instance: instance, maxAge: maxAge, sum: fileSumOf(bin)}
}
func NewCacheReloader(maxAge time.Duration) *CacheReloader { return NewCacheReloaderFor("", maxAge) }
```

`Check()` 内：`MaybeLazyPull(r.maxAge)` → `MaybeLazyPullFor(r.instance, r.maxAge)`；`LoadCacheSnapshot()` → `LoadCacheSnapshotFor(r.instance)`。

quarantine.go：

```go
func QuarantineCacheFor(instance string, reason string) (QuarantineResult, error) {
	// 原 QuarantineCache 体：CachePaths() → CachePathsFor(instance)；
	// DekProvider("") → DekProvider(instance)；CacheCredPath() → CacheCredPathFor(instance)
}
func QuarantineCache(reason string) (QuarantineResult, error) { return QuarantineCacheFor("", reason) }
```

（`expiry_load_test.go` 等若直接调 `QuarantineCache`——wrapper 保平安，零改动。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go build ./... && go test ./...`
Expected: 全绿（全仓；既有测试走 wrapper 行为不变）。

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat(clientops): Plan 40 T5 实例穿透——PullOpts.Instance+For 变体全家(cred/load/lazy/reloader/quarantine)"
```

---

### Task 6: store 端 name 纪律——add/bind 白名单 + casefold 终身唯一（事务内）

**Files:**
- Modify: `internal/store/cachetoken.go`（AddCacheToken / BindCacheToken）
- Test: `internal/store/cachetoken_test.go`（追加）

**Interfaces:**
- Consumes: `instname.Valid`。
- Produces: `AddCacheToken(name, profileID)` 签名不变，新增两道闸：① `instname.Valid(name)` 源头拒；② **事务内**（reclaim 之后、INSERT 之前）`SELECT COUNT(*) FROM cache_tokens WHERE lower(name)=lower(?) AND name<>?` > 0 → 拒（casefold 变体终身占用，含 revoked——`name<>?` 是 BINARY 精确比对，精确同名 revoked 行刚被 reclaim DELETE 清掉，剩下的全是真变体；精确同名 active 行由既有 UNIQUE 约束兜底）。`BindCacheToken` 加同款 Valid + 变体检（防御）。**`RevokeCacheToken` 永不校验**——它是存量非法 name 的修复通道。

- [ ] **Step 1: 写失败测试**

```go
func TestAddCacheToken_NameDiscipline(t *testing.T) {
	st, prof := newTokenStore(t) // Task 7 定义的 helper——本任务先落（见 Step 3 注）

	// 非法名：字符集 / 首段保留名 / 路径穿越
	for _, bad := range []string{"a b", "con.foo", "COM1.x", "../x", "foo.", "", "nul.tar.gz"} {
		if _, _, err := st.AddCacheToken(bad, prof); err == nil {
			t.Errorf("AddCacheToken(%q) must be refused", bad)
		}
	}
	// 合法名通过
	if _, _, err := st.AddCacheToken("laptop-agentA", prof); err != nil {
		t.Fatalf("legal name refused: %v", err)
	}
	// 大小写变体 active 冲突
	if _, _, err := st.AddCacheToken("LAPTOP-AGENTA", prof); err == nil || !strings.Contains(err.Error(), "case") {
		t.Fatalf("active casefold variant must be refused: %v", err)
	}
	// revoke 后：精确同名可重发（reclaim 语义保留）……
	if err := st.RevokeCacheToken("laptop-agentA"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop-agentA", prof); err != nil {
		t.Fatalf("exact re-issue after revoke must work: %v", err)
	}
	// ……但变体终身占用
	if err := st.RevokeCacheToken("laptop-agentA"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop-agenta", prof); err == nil || !strings.Contains(err.Error(), "case") {
		t.Fatalf("revoked casefold variant must stay refused: %v", err)
	}
}

func TestBindCacheToken_NameDiscipline(t *testing.T) {
	st, prof := newTokenStore(t)
	_, _, _ = st.AddCacheToken("agentA", prof)
	if err := st.BindCacheToken("bad name", prof); err == nil {
		t.Fatal("bind must validate the name too (defense)")
	}
}
```

（`newTokenStore` 在 cachetoken_test.go 就地定义——开临时 store + 一个 profile，返回 `(*Store, profileID)`：

```go
func newTokenStore(t *testing.T) (*Store, string) {
	t.Helper()
	mk, _ := GenerateMasterKey()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	pid, err := st.AddProfile("p1")
	if err != nil {
		t.Fatal(err)
	}
	return st, pid
}
```

若该文件已有等价 helper 则复用其一，勿重复定义。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run 'NameDiscipline'`
Expected: FAIL（非法名现在被接受）。

- [ ] **Step 3: 实现**（cachetoken.go）

AddCacheToken，profile 校验之后加：

```go
if verr := instname.Valid(name); verr != nil {
	return "", "", verr
}
```

事务内、reclaim DELETE 之后、INSERT 之前加：

```go
// Plan 40 §2.1: casefold variants of a device name are reserved for the
// name's LIFETIME (revoked rows included) — a re-issued variant would collide
// with the residual instance dir / per-instance DEK on the client. The exact
// same-name revoked rows were just reclaimed above, so any remaining
// lower(name) match is a true variant. In-tx = the cross-process double-open
// backstop (MaxOpenConns(1) already serializes in-process).
var variants int
if err := tx.QueryRow(`SELECT COUNT(*) FROM cache_tokens WHERE lower(name)=lower(?) AND name<>?`, name, name).Scan(&variants); err != nil {
	return "", "", err
}
if variants > 0 {
	return "", "", fmt.Errorf("device name %q collides case-insensitively with an existing or revoked device name — variants of a name are reserved for its lifetime; pick a different name", name)
}
```

BindCacheToken，profile 校验之后加：

```go
if verr := instname.Valid(name); verr != nil {
	return verr
}
var variants int
if err := s.db.QueryRow(`SELECT COUNT(*) FROM cache_tokens WHERE lower(name)=lower(?) AND name<>?`, name, name).Scan(&variants); err != nil {
	return err
}
if variants > 0 {
	return fmt.Errorf("device name %q collides case-insensitively with another device name", name)
}
```

（import `instname`。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ ./internal/mcpserver/ ./internal/cli/`
Expected: PASS。**若 mcpserver/cli 既有 fixture 用非法 name（空格等）调 AddCacheToken，按最小改动把 fixture 名改合法**（`rg -n "AddCacheToken\(" internal --type go` 核对）——这是行为收紧的预期连锁。

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat(store): Plan 40 T6 cache-tokens add/bind 白名单+casefold 终身唯一(事务内,revoked 含);revoke 不校验=修复通道"
```

---

### Task 7: serve 启动存量检测——active 行 casefold 碰撞 / 非法 name → fail-closed

**Files:**
- Modify: `internal/store/cachetoken.go`（追加 ScanCacheTokenNameAnomalies）
- Modify: `internal/mcpserver/serve.go`（NewServeRunner）
- Test: `internal/store/cachetoken_test.go` + `internal/mcpserver/serve_snapshot_test.go`（各追加）

**Interfaces:**
- Produces: `store.(*Store).ScanCacheTokenNameAnomalies() ([]string, error)`——扫全部 **active** 行：白名单不合规 / casefold 互相碰撞 → anomaly 描述行（空切片 = 干净）。`NewServeRunner(st)` 在 bodyLimit 解析前调用之，非空 → 返回 error（serve 拒绝启动，错误文案引导 revoke+add 改名）。

- [ ] **Step 1: 写失败测试**

store 侧：

```go
// 本文件自包含 helper（若 cachetoken_test.go 已有等价物则复用其一）
func newTokenStore(t *testing.T) (*Store, string) {
	t.Helper()
	mk, _ := GenerateMasterKey()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	pid, err := st.AddProfile("p1")
	if err != nil {
		t.Fatal(err)
	}
	return st, pid
}

func TestScanCacheTokenNameAnomalies(t *testing.T) {
	st, prof := newTokenStore(t)
	// 直接 SQL 插非法/碰撞行（绕过 add 的新闸——模拟自由文本时代存量；store 测试同包,st.db 可直达）
	ins := `INSERT INTO cache_tokens (id,name,token_hash,token_salt,token_prefix,status,profile_id,created_at,updated_at)
		VALUES (?,?,x'00',x'00',?,?,?,1,1)`
	exec := func(id, name, prefix, status string) {
		t.Helper()
		if _, err := st.db.Exec(ins, id, name, prefix, status, prof); err != nil {
			t.Fatal(err)
		}
	}
	exec("a1", "agentA", "p1", "active")
	exec("a2", "AGENTA", "p2", "active")   // casefold 碰撞
	exec("a3", "bad name", "p3", "active") // 白名单不合规
	exec("a4", "ok-name", "p4", "active")
	exec("a5", "legacy-bad", "p5", "revoked") // revoked 不参与
	got, err := st.ScanCacheTokenNameAnomalies()
	if err != nil || len(got) != 2 {
		t.Fatalf("anomalies = %v, %v (want 2: collision + illegal)", got, err)
	}
	// 干净库 → 空
	st2, _ := newTokenStore(t)
	if got, _ := st2.ScanCacheTokenNameAnomalies(); len(got) != 0 {
		t.Fatalf("clean = %v", got)
	}
}
```

mcpserver 侧（wiring 可测面拆为纯函数——脏库无法经公共 API 构造：add 新闸拒一切变体/非法名，snapshot 也不含 cache_tokens）：

```go
func TestFormatNameAnomalies_LeaksNothingSaysEverything(t *testing.T) {
	err := formatNameAnomalies([]string{
		`invalid device name "bad name" (...) — revoke and re-add with a valid name`,
		`case-insensitive collision "agentA" vs "AGENTA" — ...`,
	})
	if err == nil || !strings.Contains(err.Error(), "device-code name") ||
		!strings.Contains(err.Error(), "cache-tokens revoke") {
		t.Fatalf("startup refusal text must name the anomaly class + repair: %v", err)
	}
	// NewServeRunner 的接线由"干净库启动成功"既有用例守半边;
	// 拒绝半边 = NewServeRunner 内直接调 formatNameAnomalies 的三行,代码评审覆盖。
}
```

实现侧对应把 NewServeRunner 的错误构造提为纯函数 `formatNameAnomalies(anomalies []string) error`（含 "serve refusing to start: N device-code name anomal(y|ies)" 头 + 逐行 + `cache-tokens revoke <name>` 修复指引），NewServeRunner 内 `else if len(anomalies) > 0 { return formatNameAnomalies(anomalies) }`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ ./internal/mcpserver/ -run 'ScanCacheTokenNameAnomalies|TestFormatNameAnomalies'`
Expected: FAIL（未定义）。

- [ ] **Step 3: 实现**

cachetoken.go 追加：

```go
// ScanCacheTokenNameAnomalies checks every ACTIVE device-code row for Plan-40
// name discipline violations: whitelist-invalid names (free-text era legacy)
// and casefold collisions between two active rows. Revoked rows are excluded —
// revoke is the repair path. Empty slice = clean.
func (s *Store) ScanCacheTokenNameAnomalies() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM cache_tokens WHERE status='active' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var anomalies []string
	seen := map[string]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if verr := instname.Valid(name); verr != nil {
			anomalies = append(anomalies, fmt.Sprintf("invalid device name %q (%v) — revoke and re-add with a valid name", name, verr))
			continue
		}
		f := instname.Fold(name)
		if prev, ok := seen[f]; ok {
			anomalies = append(anomalies, fmt.Sprintf("case-insensitive collision %q vs %q — revoke one and re-add under a distinct name", prev, name))
			continue
		}
		seen[f] = name
	}
	return anomalies, rows.Err()
}
```

serve.go NewServeRunner 开头（upload-cap 解析之后）：

```go
// Plan 40 §2.1 legacy detection: active device-code names are about to be
// emitted as X-Sshmgr-Device-Name and used as client directory names — a
// casefold collision or an illegal legacy name must stop the serve BEFORE it
// serves, not mid-flight. Repair = revoke + re-add (never auto-rename).
if anomalies, aerr := st.ScanCacheTokenNameAnomalies(); aerr != nil {
	return nil, fmt.Errorf("serve startup: device-code name scan failed: %w", aerr)
} else if len(anomalies) > 0 {
	return nil, formatNameAnomalies(anomalies)
}
```

同文件追加纯函数（错误构造独立成函数——脏库无法经公共 API 构造，wiring 的可测面拆到这里）：

```go
// formatNameAnomalies builds the fail-closed startup refusal for Plan 40 §2.1
// legacy detection. Pure so the wording is unit-testable without a dirty DB
// (which cannot be built through the public API — the add gate refuses every
// illegal/colliding name).
func formatNameAnomalies(anomalies []string) error {
	plural := "ies"
	if len(anomalies) == 1 {
		plural = "y"
	}
	return fmt.Errorf("serve refusing to start: %d device-code name anomal%s:\n  - %s\nrepair on this machine: `ssh-manager cache-tokens revoke <name>` then `cache-tokens add --name <new-name> --profile <profile>`",
		len(anomalies), plural, strings.Join(anomalies, "\n  - "))
}
```

（serve.go 增 import `"strings"`。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ ./internal/mcpserver/`
Expected: PASS（既有 serve fixture 的 token 名都合法——`laptop`/`dev1` 等；若有个别非法，改 fixture 名）。

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat(serve): Plan 40 T7 启动存量检测——active name casefold 碰撞/非法 fail-closed 拒启"
```

---

### Task 8: serve 下发 `X-Sshmgr-Device-Name`

**Files:**
- Modify: `internal/mcpserver/serve.go`（handleSnapshot，scope 头旁边）
- Test: `internal/mcpserver/serve_snapshot_test.go`（追加）

**Interfaces:**
- Produces: 200 响应新增头 `X-Sshmgr-Device-Name: <ct.Name>`（鉴权后 `ct` 已在手，零查询）。非安全边界（pinned TLS 防篡改；老 client 忽略）。

- [ ] **Step 1: 写失败测试**

```go
func TestSnapshot_DeviceNameHeader(t *testing.T) {
	// 按 ScopedToBoundProfile 既有 fixture 形态：建 store+profile+绑定设备码
	r, url, code := newBoundServe(t /* 既有 helper 或照抄 ScopedToBoundProfile 前置 */, "laptop-agentA")
	req, _ := http.NewRequest("GET", url+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+code)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("X-Sshmgr-Device-Name"); got != "laptop-agentA" {
		t.Fatalf("X-Sshmgr-Device-Name = %q, want laptop-agentA", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run TestSnapshot_DeviceNameHeader`
Expected: FAIL（头为空）。

- [ ] **Step 3: 实现**（handleSnapshot，`X-Sshmgr-Snapshot-Scope` Set 之后）

```go
// Plan 40 §2.3: the device code's NAME rides the same trusted channel — the
// client uses it to route/verify instance identity (instances/<name>/). Not a
// security boundary (the name is not a secret; pinned TLS blocks tampering);
// old clients ignore it.
w.Header().Set("X-Sshmgr-Device-Name", ct.Name)
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcpserver/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/
git commit -m "feat(serve): Plan 40 T8 /snapshot 下发 X-Sshmgr-Device-Name"
```

---

### Task 9: 默认实例身份门禁（三分支）+ `cacheMeta.DeviceName`

**Files:**
- Modify: `internal/clientops/clientops.go`（cacheMeta 结构 + DoPull 写盘前门禁 + meta 写入带 DeviceName）
- Test: `internal/clientops/gate_test.go`（新建）

**Interfaces:**
- Consumes: Task 5 的 `CachePathsFor(o.Instance)`（路径已在写盘前解析）；Task 8 的响应头。
- Produces:
  - `cacheMeta` 增字段 `DeviceName string`（`json:"device_name"`，**无 omitempty**；存量 meta 读入 = 零值空串 = "未登记"）。
  - DoPull 内新私有函数 `gateDefaultInstance(bin, metaPath, dir, deviceName string, o PullOpts) error`——门禁只在 `o.Instance == ""` 且响应头 name 非空时进入判定；任何拒绝都发生在**任何写盘之前**（含 bin）。
  - `DeviceName` 只在 `pin != ""` 时取响应头值（pinned = 头不可注入；plaintext 恒记空串，不能给门禁喂可注入值）。
- 门禁判定（spec §2.4 三分支，生效条件 = 默认目录 `cache.bin` 存在）：
  1. meta 可解析 && `device_name` 非空 && ≠ 本次 name → 拒（三选一冻结文案）。
  2. meta 可解析 && `device_name` == "" → 放行 + 本次写盘的 meta 补记 name。
  3. meta 不可解析 → 拒（真异常态文案）。
  4. `cache.bin` 不存在（真空 / auth-only）→ 门禁不生效（§9.10 边缘：首次 pull 补记后自然闭合）。
  5. 响应头缺失（老 serve）且 bin 存在 → 门禁跳过 + StatusOut WARNING 升级提示；头缺失且 bin 不存在 → 现状行为直写。
  6. 头在但 name 过不了白名单 → 拒写盘（防御存量非法 name 下发）。

- [ ] **Step 1: 写失败测试**

```go
package clientops

// deviceSnapshotHandler: pinned serve 形态 + 可控 X-Sshmgr-Device-Name。
// name==nil → 不发头（老 serve fixture）；*name=="" → 发空值头。
func deviceSnapshotHandler(name *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if name != nil && *name != "" {
			w.Header().Set("X-Sshmgr-Device-Name", *name)
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}
}

func pullWith(t *testing.T, srvName *string) error {
	t.Helper()
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(srvName))
	return DoPull(url, "code", pin, PullOpts{})
}

func TestGate_DefaultInstance_ThreeBranches(t *testing.T) {
	withDEK(t) // 门禁不碰 DEK，但写盘路径需要
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	nX, nY := "laptop-agentA", "laptop-agentB"

	// ④ 真空首次 pull → 放行 + 记名
	if err := pullWith(t, &nX); err != nil {
		t.Fatalf("vacuum pull: %v", err)
	}
	m := readMetaForTest(t, mustCacheDir(t))
	if m.DeviceName != nX {
		t.Fatalf("vacuum pull must record device_name, got %q", m.DeviceName)
	}
	// 同名重拉 → 放行（branch 1 的等值侧）
	if err := pullWith(t, &nX); err != nil {
		t.Fatalf("same-name re-pull: %v", err)
	}
	// ① 异码 → 拒 + 既有材料字节不变
	before := dirSums(t, mustCacheDir(t))
	err := pullWith(t, &nY)
	if err == nil || !strings.Contains(err.Error(), "--instance") {
		t.Fatalf("cross-code pull must be refused with the three-choice text: %v", err)
	}
	if !strings.Contains(err.Error(), "device code") && !strings.Contains(err.Error(), "cache-tokens") {
		t.Fatalf("refusal must guide owner verification: %v", err)
	}
	assertDirSumsUnchanged(t, mustCacheDir(t), before) // 存在的 bin/meta/auth sha256 不变

	// ② 存量未登记（device_name 空）→ 放行 + 补记（零迁移生命线）
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	legacyMeta := `{"url":"https://old","pulled_at":1,"server_anchored":true,"scoped":false}`
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(legacyMeta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("old-encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pullWith(t, &nX); err != nil {
		t.Fatalf("legacy unregistered meta must be adopted: %v", err)
	}
	if m := readMetaForTest(t, dir); m.DeviceName != nX {
		t.Fatalf("adoption must backfill device_name, got %q", m.DeviceName)
	}

	// ③ bin 在 + meta 不可解析 → 拒
	dir2 := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir2})
	os.WriteFile(filepath.Join(dir2, "cache.bin"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir2, "cache.meta.json"), []byte("{not json"), 0o600)
	if err := pullWith(t, &nX); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unparseable meta + bin must refuse: %v", err)
	}

	// ⑥ 头在但非法 name → 拒写盘
	dir3 := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir3})
	bad := "../evil"
	if err := pullWith(t, &bad); err == nil || !strings.Contains(err.Error(), "invalid device name") {
		t.Fatalf("illegal header name must refuse: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir3, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("no write may happen on refusal")
	}
}

func TestGate_OldServe_SkipAndHint(t *testing.T) {
	withDEK(t)
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	// 先有材料（老 serve 无头也能拉——真空）
	if err := pullWith(t, nil); err != nil { // nil = 不发头（老 serve）
		t.Fatalf("old-serve first pull: %v", err)
	}
	// 老 serve + bin 在 → 门禁跳过 + WARNING
	var buf bytes.Buffer
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(nil))
	err := DoPull(url, "code", pin, PullOpts{StatusOut: &buf})
	if err != nil {
		t.Fatalf("old-serve re-pull must succeed (gate skipped): %v", err)
	}
	if !strings.Contains(buf.String(), "X-Sshmgr-Device-Name") || !strings.Contains(buf.String(), "upgrade") {
		t.Fatalf("old-serve hint missing: %q", buf.String())
	}
	if m := readMetaForTest(t, dir); m.DeviceName != "" {
		t.Fatalf("old-serve pull must leave device_name empty, got %q", m.DeviceName)
	}
}

func TestGate_PlaintextNeverRecordsDeviceName(t *testing.T) {
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sshmgr-Device-Name", "spoofed") // 注入头：明文通道不可信
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer plain.Close()
	if err := DoPull(plain.URL, "code", "", PullOpts{AllowPlain: true}); err != nil {
		t.Fatal(err)
	}
	if m := readMetaForTest(t, mustCacheDir(t)); m.DeviceName != "" {
		t.Fatalf("plaintext must never record device_name, got %q", m.DeviceName)
	}
}
```

（gate_test.go 文件内 helper（fileSum 复用 p0_anchor_test.go 的同名函数，同包）：

```go
func mustCacheDir(t *testing.T) string {
	t.Helper()
	d := os.Getenv("SSHMGR_CACHE_DIR")
	if d == "" {
		t.Fatal("test bug: SSHMGR_CACHE_DIR must be set")
	}
	return d
}

// dirSums snapshots the sha256 of every cache material file that EXISTS.
func dirSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range []string{"cache.bin", "cache.meta.json", "cache.auth.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			out[f] = fileSum(t, filepath.Join(dir, f))
		}
	}
	return out
}

func assertDirSumsUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	for f, sum := range before {
		if got := fileSum(t, filepath.Join(dir, f)); got != sum {
			t.Fatalf("%s changed on a refused pull", f)
		}
	}
}
```）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run 'TestGate_'`
Expected: FAIL（DeviceName 字段/门禁不存在；异码 pull 现在静默覆盖——§0.9 敞口）。

- [ ] **Step 3: 实现**

cacheMeta 增字段（Scoped 之后）：

```go
	// DeviceName records the pulling device code's name as asserted by the
	// pinned serve (X-Sshmgr-Device-Name). Empty on legacy metas (the zero
	// value — the §2.4 adopt-and-record branch) and on plaintext pulls (an
	// injectable header must never gate). No omitempty, same zero-value
	// semantics as ServerAnchored/Scoped.
	DeviceName string `json:"device_name"`
```

DoPull——anchor 块之后、`blob, err := vaultio.EncryptWithKey` 之前：

```go
	// Plan 40 §2.4: the device identity comes from the pinned response ONLY
	// (plaintext headers are injectable). The default-instance identity gate
	// runs BEFORE any write (bin included) — a refusal leaves the old cache
	// byte-identical.
	deviceName := ""
	if pin != "" {
		deviceName = res.Header.Get("X-Sshmgr-Device-Name")
	}
	if o.Instance == "" {
		if err := gateDefaultInstance(bin, metaPath, deviceName, o); err != nil {
			return err
		}
	}
```

（Task 5 已把路径解析上移到此处之前，`bin/metaPath` 在手。）新私有函数：

```go
// gateDefaultInstance enforces the §2.4 three-branch identity gate on the
// DEFAULT slot. Active only when the default cache.bin exists (auth.json alone
// is pull credentials, not material — §9.10). deviceName=="" means "no
// trustworthy name" (old serve, or plaintext): the gate is skipped with an
// upgrade hint (pre-existing exposure on old-serve topologies, not a new one).
func gateDefaultInstance(bin, metaPath, deviceName string, o PullOpts) error {
	if deviceName != "" {
		if verr := instname.Valid(deviceName); verr != nil {
			return fmt.Errorf("pull refused: %w — owner: revoke and re-add the device code with a valid name", verr)
		}
	}
	if _, serr := os.Stat(bin); serr != nil {
		if os.IsNotExist(serr) {
			return nil // vacuum / auth-only: no material to protect; the pull records identity
		}
		return serr
	}
	if deviceName == "" {
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: serve did not send X-Sshmgr-Device-Name (pre-Plan-40 serve) — the default-cache identity gate is inactive until the serve is upgraded\n")
		}
		return nil
	}
	m, merr := readCacheMeta(metaPath)
	switch {
	case merr == nil && m.DeviceName != "":
		if m.DeviceName == deviceName {
			return nil // same device re-pulling its own cache
		}
		return fmt.Errorf("refusing pull: this cache belongs to device %q but the presented device code is %q — pick one:\n"+
			"  1. this is a SECOND device on this machine: re-run the pull with --instance %q\n"+
			"  2. replace the default instance's device code: delete cache.auth.json + cache.bin + cache.meta.json + the quarantine/ dir in this cache directory and re-enroll\n"+
			"  3. owner: verify which device this code was issued for (`cache-tokens ls` on the server)", m.DeviceName, deviceName, deviceName)
	case merr == nil:
		return nil // legacy unregistered meta: adopt — the write below backfills device_name (§5 zero-migration)
	default:
		return fmt.Errorf("refusing pull: cache.bin exists but cache.meta.json is missing or unreadable (%v) — inconsistent/interrupted cache; delete cache.bin + cache.meta.json + cache.auth.json + the quarantine/ dir in this cache directory and re-enroll", merr)
	}
}
```

meta 写入（scoped 行处合并）：

```go
		scoped := res.Header.Get("X-Sshmgr-Snapshot-Scope") == "profile"
		mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: pulledAt, ServerAnchored: anchored, Scoped: scoped, DeviceName: deviceName})
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/ ./internal/cli/`
Expected: PASS。**既有用例注意**：`cli/mcp_cache_test.go` 的 plaintext e2e（无头）不受影响；若有测试断言 meta JSON 的完整键集，补 `device_name` 键期望（`rg -n '"scoped"' internal --type go` 核对）。

- [ ] **Step 5: Commit**

```bash
git add internal/clientops/
git commit -m "feat(clientops): Plan 40 T9 默认实例身份门禁三分支+meta.device_name——异码写盘前拒,存量补记,老serve跳过+提示"
```

---

### Task 10: `--instance` 拉取分支——头强一致 + 物理碰撞检测（含半写态）

**Files:**
- Modify: `internal/clientops/clientops.go`（DoPull：`o.Instance != ""` 分支的门禁）
- Test: `internal/clientops/gate_instance_test.go`（新建）

**Interfaces:**
- Consumes: Task 5 `PullOpts.Instance`、Task 8 头、`instname.Valid`。
- Produces: `DoPull` 内 `gateNamedInstance(dir, bin, metaPath, deviceName, instance string) error`（私有）。规则（spec §2.4 行 1 + §2.1）：
  1. `deviceName == ""`（头缺失 = 老 serve；或 plaintext）→ 拒 + 升级提示（`--instance` 强依赖新 serve）。
  2. `instname.Valid(deviceName)` 失败 → 拒（防御）。
  3. `deviceName != o.Instance` → 拒（张冠李戴强一致）。
  4. 目录已存在时：meta 可读且 `device_name` ∈ {"", deviceName} → 放行（"" = 本设计前半写残留可补记）；meta 可读且 ≠ → 拒；**bin 在而 meta 缺失/不可读 → 拒**（半写态，文案给清理路径）。

- [ ] **Step 1: 写失败测试**

```go
package clientops

func pullInstance(t *testing.T, instance string, srvName *string) error {
	t.Helper()
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(srvName))
	return DoPull(url, "code", pin, PullOpts{Instance: instance})
}

func instDir(t *testing.T, userDir, name string) string {
	return filepath.Join(userDir, "ssh-manager", "instances", name)
}

func TestGateNamedInstance(t *testing.T) {
	nA := "laptop-agentA"
	other := "laptop-agentB"

	t.Run("happy path writes instance slot", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		if err := pullInstance(t, nA, &nA); err != nil {
			t.Fatalf("instance pull: %v", err)
		}
		if _, err := os.Stat(filepath.Join(instDir(t, userDir, nA), "cache.bin")); err != nil {
			t.Fatal(err)
		}
		if m := readMetaForTest(t, instDir(t, userDir, nA)); m.DeviceName != nA {
			t.Fatalf("meta device_name = %q", m.DeviceName)
		}
		// 同码重拉 → 放行
		if err := pullInstance(t, nA, &nA); err != nil {
			t.Fatalf("re-pull: %v", err)
		}
	})

	t.Run("old serve header missing refused", func(t *testing.T) {
		redirectUserConfigDir(t)
		err := pullInstance(t, nA, nil) // 不发头
		if err == nil || !strings.Contains(err.Error(), "--instance requires") {
			t.Fatalf("old serve must refuse --instance: %v", err)
		}
	})

	t.Run("mismatch refused before any write", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		err := pullInstance(t, nA, &other) // 头=B,flag=A
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("mismatch must refuse: %v", err)
		}
		if _, serr := os.Stat(instDir(t, userDir, nA)); !os.IsNotExist(serr) {
			t.Fatal("no directory may be created on refusal")
		}
	})

	t.Run("illegal server name refused", func(t *testing.T) {
		redirectUserConfigDir(t)
		bad := "../evil"
		if err := pullInstance(t, "ok-name", &bad); err == nil || !strings.Contains(err.Error(), "invalid device name") {
			t.Fatalf("illegal name must refuse: %v", err)
		}
	})

	t.Run("physical collision: existing identity differs", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		if err := pullInstance(t, nA, &nA); err != nil { // 先占位
			t.Fatal(err)
		}
		// 手工把 meta 身份改成别的（模拟另一身份残留）
		dir := instDir(t, userDir, nA)
		os.WriteFile(filepath.Join(dir, "cache.meta.json"),
			[]byte(`{"url":"x","pulled_at":1,"server_anchored":true,"scoped":false,"device_name":"someone-else"}`), 0o600)
		if err := pullInstance(t, nA, &nA); err == nil || !strings.Contains(err.Error(), "different device identity") {
			t.Fatalf("physical collision must refuse: %v", err)
		}
	})

	t.Run("half-written: bin without readable meta refused", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		dir := instDir(t, userDir, nA)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("partial"), 0o600)
		if err := pullInstance(t, nA, &nA); err == nil || !strings.Contains(err.Error(), "re-enroll") {
			t.Fatalf("half-written state must refuse with cleanup path: %v", err)
		}
	})
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run TestGateNamedInstance`
Expected: FAIL（分支未实现——现在 `--instance` 拉取不看头）。

- [ ] **Step 3: 实现**

DoPull 门禁块扩为：

```go
	if o.Instance != "" {
		if err := gateNamedInstance(bin, metaPath, deviceName, o.Instance); err != nil {
			return err
		}
	} else if err := gateDefaultInstance(bin, metaPath, deviceName, o); err != nil {
		return err
	}
```

```go
// gateNamedInstance enforces §2.4 row 1 + §2.1 for an explicit --instance pull:
// the instance route REQUIRES a Plan-40 serve (header present), the header
// must name exactly the flagged instance (a mismatched code/flag pair would
// write one device's authorization into another's slot), and the physical slot
// must not hold a different identity or a half-written state.
func gateNamedInstance(bin, metaPath, deviceName, instance string) error {
	if deviceName == "" {
		return fmt.Errorf("refusing pull: --instance requires a Plan-40 serve (the response carries no X-Sshmgr-Device-Name) — upgrade the serve, or drop --instance to use the default cache slot")
	}
	if verr := instname.Valid(deviceName); verr != nil {
		return fmt.Errorf("pull refused: %w — owner: revoke and re-add the device code with a valid name", verr)
	}
	if deviceName != instance {
		return fmt.Errorf("refusing pull: --instance %q does not match the serve's device name %q — each device code pulls into its own instance; use --instance %q on the machine that code was issued for", instance, deviceName, deviceName)
	}
	if _, serr := os.Stat(bin); serr != nil {
		if !os.IsNotExist(serr) {
			return serr
		}
		return nil // fresh slot
	}
	// slot has a bin: its recorded identity must be this instance's (or blank).
	m, merr := readCacheMeta(metaPath)
	if merr != nil {
		return fmt.Errorf("refusing pull: instance directory %s holds cache.bin but no readable cache.meta.json (interrupted write?) — delete the instance directory and re-enroll", filepath.Dir(bin))
	}
	if m.DeviceName != "" && m.DeviceName != deviceName {
		return fmt.Errorf("refusing pull: instance directory %s already holds a different device identity (%q vs %q) — delete the instance directory and re-enroll", filepath.Dir(bin), m.DeviceName, deviceName)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/`
Expected: PASS（含 Task 5 的路由测试——同名重拉走 meta 身份等值放行）。

- [ ] **Step 5: Commit**

```bash
git add internal/clientops/
git commit -m "feat(clientops): Plan 40 T10 --instance 门禁——头强一致/老serve拒/物理碰撞含半写态"
```

---

### Task 11: CLI flag 层——`--instance`（pull/mcp/status）+ env 互斥 + mcp 无默认 cache 时列实例

**Files:**
- Modify: `internal/cli/cache.go`（pull + status 的 `--instance`）
- Modify: `internal/cli/mcp.go`（`--instance` + 无默认 cache 报错列实例）
- Test: `internal/cli/instance_flag_test.go`（新建）

**Interfaces:**
- Produces:
  - cli 私有 `checkInstanceFlag(instance string) error`——非空时：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 显式设置 → 互斥报错（冻结文案）；`instname.Valid`。
  - `cache pull --instance <name>` → `PullOpts{Instance}` + `WriteCacheCredFor(instance, ...)`。
  - `mcp --cache --instance <name>` → `MaybeLazyPullFor` / `NewCacheReloaderFor` / `LoadCacheSnapshotFor` / audit 路径 `CachePathsFor`；**无 `--instance` 且默认目录无 `cache.bin` 且 `ListInstances()` 非空 → 报错列出实例清单**（不自动猜；spec §2.5——仅 `mcp --cache`，status 不受限）。
  - `cache status --instance <name>` → 单实例视图（Task 12 前先接 For-变体保持现格式）。

- [ ] **Step 1: 写失败测试**

```go
package cli

func TestInstanceFlagMutex(t *testing.T) {
	for _, env := range []string{"SSHMGR_CACHE_DIR", "SSHMGR_CACHE_DEK"} {
		t.Run(env, func(t *testing.T) {
			withEnv(t, map[string]string{env: t.TempDir()})
			if err := checkInstanceFlag("agentA"); err == nil ||
				!strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("%s + --instance must error: %v", env, err)
			}
			if err := checkInstanceFlag(""); err != nil {
				t.Fatalf("no flag = no mutex: %v", err)
			}
		})
	}
	t.Run("SSHMGR_CACHE_DEK_DIR composes", func(t *testing.T) {
		withEnv(t, map[string]string{"SSHMGR_CACHE_DEK_DIR": t.TempDir(), "SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
		if err := checkInstanceFlag("agentA"); err != nil {
			t.Fatalf("dir-level seam must compose with --instance: %v", err)
		}
	})
	t.Run("illegal flag name", func(t *testing.T) {
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
		if err := checkInstanceFlag("bad name"); err == nil {
			t.Fatal("illegal name must be refused at the flag layer")
		}
	})
}

// mcp 无默认 cache 且有实例 → 报错列实例（exit 前的 RunE 错误）
func TestMCP_NoDefaultCache_WithInstances_Errors(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
	for _, n := range []string{"agentA", "agentB"} {
		os.MkdirAll(filepath.Join(userDir, "ssh-manager", "instances", n), 0o700)
	}
	cmd := newMCPCmd()
	cmd.SetArgs([]string{"--token", "x", "--cache"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--instance") ||
		!strings.Contains(err.Error(), "agentA") || !strings.Contains(err.Error(), "agentB") {
		t.Fatalf("must refuse listing instances: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run 'TestInstanceFlagMutex|TestMCP_NoDefaultCache'`
Expected: FAIL。

- [ ] **Step 3: 实现**

`internal/cli/common.go`（或 cache.go 顶部）：

```go
// checkInstanceFlag enforces the §2.2 env×flag mutex and the name whitelist
// at the CLI layer: the single-file/dir CACHE envs fully override path
// resolution, so combining one with --instance would silently route the
// command to the wrong instance (or make all instances share one DEK).
// SSHMGR_CACHE_DEK_DIR deliberately composes — it is a directory-level seam
// that derives per-instance paths.
func checkInstanceFlag(instance string) error {
	if instance == "" {
		return nil
	}
	for _, env := range []string{"SSHMGR_CACHE_DIR", "SSHMGR_CACHE_DEK"} {
		if os.Getenv(env) != "" {
			return fmt.Errorf("--instance and %s are mutually exclusive — %s fully overrides the cache path/DEK resolution and would silently route this command to the wrong instance; unset the env or drop --instance", env, env)
		}
	}
	return instname.Valid(instance)
}
```

cachePullCmd：`var instance string` + `c.Flags().StringVar(&instance, "instance", "", ...)`；RunE 开头 `if err := checkInstanceFlag(instance); err != nil { return err }`；两处 DoPull 调用加 `Instance: instance`；成功后 `WriteCacheCred` → `WriteCacheCredFor(instance, ...)`。

cacheStatusCmd：`--instance` flag + `checkInstanceFlag` + 全部 For-变体（`CachePathsFor(instance)` / `LoadCacheSnapshotFor(instance)`）。

mcp.go：`--instance` flag + `checkInstanceFlag`；`useCache` 分支开头（MaybeLazyPull 之前）：

```go
				if instance == "" {
					// §2.5: reading an instance is ALWAYS explicit — never guess.
					if _, bin, _, _, perr := clientops.CachePaths(); perr == nil {
						if _, berr := os.Stat(bin); os.IsNotExist(berr) {
							if names, lerr := clientops.ListInstances(); lerr == nil && len(names) > 0 {
								return fmt.Errorf("no cache in the default slot, but %d named instance(s) exist: %s — pass --instance <name> (the default slot is never auto-guessed)", len(names), strings.Join(names, ", "))
							}
						}
					}
				}
```

随后 `MaybeLazyPull(cacheMaxAge)` → `MaybeLazyPullFor(instance, cacheMaxAge)`、`NewCacheReloader` → `NewCacheReloaderFor(instance, ...)`、`LoadCacheSnapshot` → `LoadCacheSnapshotFor(instance)`、`CachePaths()` → `CachePathsFor(instance)`（audit 路径）。（mcp.go 增 import `"strings"`。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/
git commit -m "feat(cli): Plan 40 T11 --instance flag(pull/mcp/status)+env 互斥+mcp 无默认 cache 列实例"
```

---

### Task 12: `cache status` 多实例视图

**Files:**
- Modify: `internal/cli/cache.go`（cacheStatusCmd 无 `--instance` 分支）
- Test: `internal/cli/cache_status_instances_test.go`（新建）

**Interfaces:**
- Consumes: `clientops.ListInstances`、`LoadCacheSnapshotFor`、`CachePathsFor`。
- Produces: `cache status` 无 flag = 纯列表命令（不报错）：默认槽一行（无材料则明确说 no cache）+ 每实例一行；单实例失败（DEK 缺/解密败）渲染为该行错误、不中断列表。`--instance` = 单实例详情（现格式）。

- [ ] **Step 1: 写失败测试**

```go
package cli

// 种一个实例槽（bin+meta 可解）——直接用 clientops.DoPull 太重，写最小材料:
// meta + withDEK seam + LoadCacheSnapshotFor 可载的 bin。此处只验证 VIEW,
// bin 用真加密太啰嗦:改为断言"列出了实例名 + 加载错误也成行不炸"。
func TestCacheStatus_ListsAllInstances(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	for _, n := range []string{"agentA", "agentB"} {
		dir := filepath.Join(userDir, "ssh-manager", "instances", n)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "cache.meta.json"),
			[]byte(fmt.Sprintf(`{"url":"https://s","pulled_at":1,"server_anchored":true,"scoped":false,"device_name":%q}`, n)), 0o600)
		os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("not-decryptable"), 0o600) // 触发行级错误
	}
	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"status"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status list mode must not fail overall: %v", err)
	}
	got := out.String()
	for _, want := range []string{"default", "agentA", "agentB", "device:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestCacheStatus_ListsAllInstances`
Expected: FAIL（现 status 在默认槽无 cache 时直接报错）。

- [ ] **Step 3: 实现**（cacheStatusCmd RunE 重构）

```go
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := checkInstanceFlag(instance); err != nil {
					return err
				}
				if instance != "" {
					return cacheStatusSingle(cmd, instance)
				}
				return cacheStatusList(cmd)
			},
```

`cacheStatusSingle` = 现函数体（For-化，meta 匿名结构体加 `DeviceName string \`json:"device_name"\``，输出加一行 `device:    %s`）。`cacheStatusList`：

```go
// cacheStatusList renders one line-group per instance slot: the default slot
// first (even when empty — said honestly), then every named instance. A slot
// that cannot LOAD (missing DEK, undecryptable bin) renders its error inline —
// listing is discovery, one broken slot must not hide the others (spec §2.6).
func cacheStatusList(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	dir, bin, metaPath, _, err := clientops.CachePaths()
	if err != nil {
		return err
	}
	printSlot := func(instance, d, binPath, mp string) {
		fmt.Fprintf(out, "instance: %s (%s)\n", map[bool]string{true: instance, false: "default"}[instance != ""], d)
		if _, serr := os.Stat(binPath); serr != nil {
			fmt.Fprintf(out, "  cache:   (no cache.bin)\n\n")
			return
		}
		var (
			device, url, profileLine = "(unknown)", "(unknown)", ""
			anchored                  = "-"
			scoped                    bool
		)
		if mb, merr := os.ReadFile(mp); merr == nil {
			var m struct {
				DeviceName     string `json:"device_name"`
				URL            string `json:"url"`
				ServerAnchored bool   `json:"server_anchored"`
				Scoped         bool   `json:"scoped"`
			}
			if json.Unmarshal(mb, &m) == nil {
				device, anchored = m.DeviceName, map[bool]string{true: "server", false: "local"}[m.ServerAnchored]
				url, scoped = m.URL, m.Scoped
			}
		}
		servers, creds := "-", "-"
		if snap, lerr := clientops.LoadCacheSnapshotFor(instance); lerr == nil && snap != nil {
			servers, creds = fmt.Sprint(len(snap.Servers)), fmt.Sprint(len(snap.Credentials))
			// profile 行仅 scoped 时显示（Plan 39 溯源纪律——与 single 视图同规则）
			if scoped {
				switch len(snap.Profiles) {
				case 1:
					profileLine = "  profile: " + snap.Profiles[0].Name + "\n"
				case 0:
					profileLine = "  profile: (none)\n"
				default:
					profileLine = "  profile: (multiple — pre-Plan-39 whole-vault snapshot)\n"
				}
			}
		} else {
			fmt.Fprintf(out, "  load:    ERROR %v\n", lerr) // 行级错误,不中断列表
		}
		fmt.Fprintf(out, "  device:  %s\n%s  anchor:  %s\n  servers: %s\n  creds:   %s\n  source:  %s\n\n",
			device, profileLine, anchored, servers, creds, url)
	}
	printSlot("", dir, bin, metaPath) // "" = 默认槽
	names, err := clientops.ListInstances()
	if err != nil {
		return err
	}
	for _, n := range names {
		id, ib, im, _, ierr := clientops.CachePathsFor(n)
		if ierr != nil {
			fmt.Fprintf(out, "instance: %s (path error: %v)\n\n", n, ierr)
			continue
		}
		printSlot(n, id, ib, im)
	}
	return nil
}
```

（single 视图 `cacheStatusSingle` 的 profile 判定块与上同规则——若愿意可提取 `profileLineFor(snap, scoped) string` 供两处共用，不强求。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/
git commit -m "feat(cli): Plan 40 T12 cache status 多实例列表——默认槽+全部实例,行级错误不中断"
```

---

### Task 13: MAX_OFFLINE 持久化——`cache.config.json` + `pull --max-offline`

**Files:**
- Create: `internal/clientops/config.go`
- Modify: `internal/clientops/expiry.go`（parseMaxOffline 共享化）
- Modify: `internal/clientops/clientops.go`（DoPull/LoadCacheSnapshotFor 切 resolveMaxOffline + 文案微调）
- Modify: `internal/cli/cache.go`（`--max-offline` flag + WARNING）
- Test: `internal/clientops/config_test.go`（新建）

**Interfaces:**
- Produces:
  - `clientops.resolveMaxOffline(dir string) (time.Duration, error)`（私有）：env 非空 → `cacheMaxOffline()`（env 胜出，含其错误）；否则读 `<dir>/cache.config.json`：缺失 → (0,nil)；损坏 → fail-closed 错误（注明 file 来源）；值解析与 env 同规则（`""`/`0` → off；<1h/非法 → fail-closed）。
  - `clientops.WriteCacheConfig(dir, value string) error`（**导出**，CLI 用）：原子写 `{"max_offline": value}`。
  - `parseMaxOffline(v, source string) (time.Duration, error)`（私有，expiry.go）：`cacheMaxOffline()` 与 file 路径共用的规则体；错误文案 `invalid %s %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)`——env 路径 source=`SSHMGR_CACHE_MAX_OFFLINE`（**与现文案逐字节一致**），file 路径 source=`max_offline in cache.config.json`。
  - `cache pull --max-offline <dur>`：flag 校验（`parseMaxOffline`）→ pull 成功后 `WriteCacheConfig(dir, flag)`；写失败 WARNING；**env 在场 → WARNING「config 在 env 清除前不生效」**。
  - DoPull / LoadCacheSnapshotFor 的读取点全部切 `resolveMaxOffline(dir)`（DoPull：路径解析后；plaintext 拒分支语义保持——env **或** file 任一开 + plaintext → 拒）。provenance 文案两处微调（加 "or cache.config.json"；测试只钉 `"no server-anchored time"`/`"no time anchor"` 子串，安全）。

- [ ] **Step 1: 写失败测试**

```go
package clientops

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestResolveMaxOffline_Priority(t *testing.T) {
	dir := t.TempDir()
	// 无 env 无 file → off
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	if d, err := resolveMaxOffline(dir); err != nil || d != 0 {
		t.Fatalf("off = %v, %v", d, err)
	}
	// file 生效
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"24h"}`), 0o600)
	if d, err := resolveMaxOffline(dir); err != nil || d != 24*time.Hour {
		t.Fatalf("file = %v, %v", d, err)
	}
	// env > file
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "168h")
	if d, err := resolveMaxOffline(dir); err != nil || d != 168*time.Hour {
		t.Fatalf("env must win: %v, %v", d, err)
	}
	// env 非法 → env 的错误胜出（fail-closed 不被 file 掩盖）
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "bogus")
	if _, err := resolveMaxOffline(dir); err == nil || !strings.Contains(err.Error(), "SSHMGR_CACHE_MAX_OFFLINE") {
		t.Fatalf("invalid env must error: %v", err)
	}
	// file 非法 → fail-closed 注明来源
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"30m"}`), 0o600)
	if _, err := resolveMaxOffline(dir); err == nil || !strings.Contains(err.Error(), "cache.config.json") {
		t.Fatalf("invalid file must error with source: %v", err)
	}
	// file 空/0 → off
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"0"}`), 0o600)
	if d, err := resolveMaxOffline(dir); err != nil || d != 0 {
		t.Fatalf("zero file = off: %v, %v", d, err)
	}
	// file 损坏 → fail-closed
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{`), 0o600)
	if _, err := resolveMaxOffline(dir); err == nil {
		t.Fatal("corrupt file must error")
	}
}

func TestWriteCacheConfig_RoundTripAndConcurrentReads(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCacheConfig(dir, "24h"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if !strings.Contains(string(b), `"max_offline":"24h"`) {
		t.Fatalf("config body = %s", b)
	}
	// 并发读永不半截（atomicWriteUnique 语义,spec §6.9）
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := resolveMaxOffline(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
						t.Errorf("reader saw torn config: %v", err)
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if err := WriteCacheConfig(dir, "24h"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// 换码 runbook 联动(spec §6.8⑤):清四件套重 enroll 后 config 保留继承。
func TestConfig_SurvivesCodeSwapRunbook(t *testing.T) {
	withDEK(t)
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(time.Now().UTC().Format(http.TimeFormat)), nil))
	if err := DoPull(url, "code1", pin, PullOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCacheConfig(dir, "24h"); err != nil {
		t.Fatal(err)
	}
	// 四件套清除(config 保留——目录/槽位策略,spec §2.4 runbook)
	for _, f := range []string{"cache.auth.json", "cache.bin", "cache.meta.json"} {
		os.Remove(filepath.Join(dir, f))
	}
	os.RemoveAll(filepath.Join(dir, "quarantine"))
	// 重 enroll → config 仍在且生效
	if d, err := resolveMaxOffline(dir); err != nil || d != 24*time.Hour {
		t.Fatalf("config must survive the code swap: %v, %v", d, err)
	}
}
```

（`newPinnedTLSServer`/`snapshotHandler`/`ptr` 均为 expiry_pull_test.go 既有同包 helper。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run 'TestResolveMaxOffline|TestWriteCacheConfig|TestConfig_Survives'`
Expected: FAIL。

- [ ] **Step 3: 实现**

expiry.go：`cacheMaxOffline()` 重构为薄壳：

```go
func cacheMaxOffline() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE"))
	return parseMaxOffline(v, "SSHMGR_CACHE_MAX_OFFLINE")
}

func parseMaxOffline(v, source string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || (d != 0 && d < cacheSkewTolerance) {
		return 0, fmt.Errorf("invalid %s %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", source, v)
	}
	return d, nil
}
```

（**注意**：现实现两段错误构造合一后文案逐字节不变——`invalid SSHMGR_CACHE_MAX_OFFLINE %q: ...`；TestCacheMaxOffline_Parsing 的精确文案断言必须继续通过。）

config.go（新建）：

```go
// Package clientops: Plan 40 §3 — the per-instance offline-cap config.
// cache.config.json is MACHINE(instance)-level state (env is process-level —
// the root cause P0 closed); plaintext by design (policy, not a credential).
package clientops

// resolveMaxOffline: env > <dir>/cache.config.json > off. An PRESENT env wins
// including its error (fail-closed is never masked by a file); the file uses
// the exact env grammar via parseMaxOffline with a file-sourced error label.
func resolveMaxOffline(dir string) (time.Duration, error) {
	if strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE")) != "" {
		return cacheMaxOffline()
	}
	blob, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cache.config.json unreadable: %w", err)
	}
	var c struct {
		MaxOffline string `json:"max_offline"`
	}
	if err := json.Unmarshal(blob, &c); err != nil {
		return 0, fmt.Errorf("corrupt cache.config.json: %w", err)
	}
	return parseMaxOffline(strings.TrimSpace(c.MaxOffline), `max_offline in cache.config.json`)
}

// WriteCacheConfig atomically persists the instance's offline cap (v is the
// raw duration string, same grammar as the env). Called by `cache pull
// --max-offline` AFTER a successful pull — a failed pull never rewrites policy.
func WriteCacheConfig(dir, v string) error {
	blob, err := json.Marshal(struct {
		MaxOffline string `json:"max_offline"`
	}{v})
	if err != nil {
		return err
	}
	return atomicWriteUnique(filepath.Join(dir, "cache.config.json"), blob)
}
```

clientops.go 切读取点：
- LoadCacheSnapshotFor：`maxOffline, err := cacheMaxOffline()` → 路径解析后 `resolveMaxOffline(dir)`；两处 provenance 文案的触发描述改为 `the offline cap is set (SSHMGR_CACHE_MAX_OFFLINE or cache.config.json)`（保留 `no time anchor` / `no server-anchored time` 子串）。
- DoPull：env precheck 移到路径解析之后改为 `maxOffline, err := resolveMaxOffline(dir)`（**plaintext 拒分支文案保持**——`SSHMGR_CACHE_MAX_OFFLINE is set:` 改为 `the offline cap is set (SSHMGR_CACHE_MAX_OFFLINE or cache.config.json):`，测试钉的子串 `refusing plaintext pull` 保留；`TestDoPull_InvalidEnv_RefusedBeforeHTTP` 用 env 构造——其文案断言 `invalid SSHMGR_CACHE_MAX_OFFLINE` 依旧来自 env 路径，通过；**但它还断言 0 hits（HTTP 前拒）**——路径解析不碰 HTTP，次序仍成立）。

cli/cache.go cachePullCmd：

```go
	var maxOfflineFlag string
	c.Flags().StringVar(&maxOfflineFlag, "max-offline", "", "persist this Go duration (e.g. 24h) as the instance's offline cap in cache.config.json (survives all processes; env SSHMGR_CACHE_MAX_OFFLINE still overrides while set)")
```

RunE：`--instance` 校验后——

```go
			maxOff := ""
			if maxOfflineFlag != "" {
				d, verr := clientops.ValidateMaxOffline(maxOfflineFlag) // 导出的 parseMaxOffline 薄壳,source="max_offline"
				if verr != nil {
					return verr
				}
				_ = d
				maxOff = maxOfflineFlag
			}
```

DoPull 成功 + WriteCacheCred(For) 之后：

```go
			if maxOff != "" {
				dir, _, _, _, derr := clientops.CachePathsFor(instance)
				if derr == nil {
					if werr := clientops.WriteCacheConfig(dir, maxOff); werr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: could not persist cache.config.json (the cap applies only while the env/file stays set): %v\n", werr)
					} else if os.Getenv("SSHMGR_CACHE_MAX_OFFLINE") != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: SSHMGR_CACHE_MAX_OFFLINE is set — the config just written takes effect only after the env is cleared\n")
					}
				}
			}
```

（`clientops.ValidateMaxOffline(v string) (time.Duration, error)` = 导出薄壳，加在 config.go。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/ ./internal/cli/`
Expected: PASS（`TestCacheMaxOffline_Parsing` / `TestDoPull_InvalidEnv_RefusedBeforeHTTP` / `TestDoPull_PlaintextRefusedWhenMaxOffline` 等既有用例零改动或仅按上文说明的文案联动核对）。

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat(cache): Plan 40 T13 MAX_OFFLINE 持久化——cache.config.json(env>file>off)+pull --max-offline+原子写+并发读"
```

---

### Task 14: `clear` 清全部实例 + DEK 变体；`roles` 实例感知

**Files:**
- Modify: `internal/cli/clear.go`（scanClearTargets + 删除循环目录处理）
- Modify: `internal/roles/roles.go`（cachePresent）
- Test: `internal/cli/clear_test.go` + `internal/roles/roles_test.go`（各追加）

**Interfaces:**
- Produces:
  - scanClearTargets 追加：`InstancesRoot()` 整树（存在子项时枚举为一条 `client: <root>`）+ `VaultDir()` 与 `SSHMGR_CACHE_DEK_DIR`（若设）两处的 `cache-dek-*.key` glob（默认 `cache-dek.key` 本就在清单）。
  - 删除循环：目标是目录 → `os.RemoveAll`（其余照旧 `os.Remove`）。
  - `roles.cachePresent()` = 默认槽 auth 可读 **或** `ListInstances()` 非空（仅命名实例的机器不再误判 wizard）。

- [ ] **Step 1: 写失败测试**

clear_test.go 追加：

```go
func TestClear_EnumeratesInstancesAndDekVariants(t *testing.T) {
	// 自包含重定向（clear_test.go 顶部已有同款 APPDATA/XDG 手法,形式一致）
	userDir, vault := t.TempDir(), t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":     "",
		"SSHMGR_CACHE_DEK":     "",
		"SSHMGR_STORE":         filepath.Join(vault, "store.db"), // clearVaultDir 从 store 路径派生
		"SSHMGR_CACHE_DEK_DIR": vault,                             // DEK 变体 glob 的 seam 基目录
	})
	instRoot := filepath.Join(userDir, "ssh-manager", "instances")
	os.MkdirAll(filepath.Join(instRoot, "agentA"), 0o700)
	os.WriteFile(filepath.Join(instRoot, "agentA", "cache.bin"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(vault, "cache-dek-agentA.key"), []byte("k"), 0o600)
	os.WriteFile(filepath.Join(vault, "cache-dek.key"), []byte("k"), 0o600)
	lines := enumClearTargets(roles.RoleClient)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, filepath.Join("instances")) || !strings.Contains(joined, "cache-dek-agentA.key") {
		t.Fatalf("enumeration missing instance artifacts:\n%s", joined)
	}
}
```

roles_test.go 追加：

```go
func TestResolveMode_OnlyNamedInstances_IsClient(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "") // 默认槽 auth 也不在
	os.MkdirAll(filepath.Join(userDir, "ssh-manager", "instances", "agentA"), 0o700)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchClient {
		t.Fatalf("named-instance-only machine must be a client: %+v %v", l, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ ./internal/roles/ -run 'TestClear_Enumerates|TestResolveMode_OnlyNamed'`
Expected: FAIL。

- [ ] **Step 3: 实现**

clear.go scanClearTargets，client cache dir 块之后追加：

```go
	// Plan 40 §2.7: named-instance trees + per-instance DEKs — a residual
	// instance dir IS residual credentials, which is exactly what clear exists
	// to remove. The whole instances/ tree enumerates as ONE target; DEK
	// variants glob from BOTH the vault dir and the SSHMGR_CACHE_DEK_DIR seam
	// (the pattern cannot match the default cache-dek.key — it requires the
	// "-<name>" infix).
	if root, rerr := clientops.InstancesRoot(); rerr == nil {
		if entries, derr := os.ReadDir(root); derr == nil && len(entries) > 0 {
			add("client", root)
		}
	}
	dekBases := []string{}
	if vd2, verr := paths.VaultDir(); verr == nil {
		dekBases = append(dekBases, vd2)
	}
	if d := os.Getenv(paths.CacheDekDirEnv); d != "" {
		dekBases = append(dekBases, d)
	}
	for _, base := range dekBases {
		if ms, gerr := filepath.Glob(filepath.Join(base, "cache-dek-*.key")); gerr == nil {
			for _, m := range ms {
				add("client", m)
			}
		}
	}
```

删除循环（runClear step 2）：

```go
		for _, t := range targets {
			if t.path == "" {
				continue
			}
			var err error
			if info, serr := os.Stat(t.path); serr == nil && info.IsDir() {
				err = os.RemoveAll(t.path) // instance trees (Plan 40 §2.7)
			} else {
				err = os.Remove(t.path)
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("删除 %s 失败: %w（重跑 clear 将跳过已完成步骤）", t.path, err)
			}
		}
```

（`add` 需要 dedupe——已有 `seen` map；`InstancesRoot` 的 dir 与单文件不冲突。）

roles.go：

```go
// cachePresent reports whether this machine is an enrolled client: a readable
// default-slot cache.auth.json (nil,nil = never enrolled) OR any named
// instance slot (Plan 40 §2.7 — a named-instance-only machine is still a
// client, not a wizard candidate).
func cachePresent() bool {
	if cred, err := clientops.ReadCacheCred(); err == nil && cred != nil {
		return true
	}
	names, err := clientops.ListInstances()
	return err == nil && len(names) > 0
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ ./internal/roles/`
Expected: PASS（既有 clear/roles 用例零改动）。

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat(clear,roles): Plan 40 T14 clear 清 instances/ 整树+DEK 变体;roles 认命名实例机器"
```

---

### Task 15: 双实例 e2e——拉取/隔离/交叉失败/吊销只毁本实例

**Files:**
- Test: `internal/cli/multiinstance_e2e_test.go`（新建）

**Interfaces:**
- Consumes: 前面全部任务的导出面。无新生产代码——这是 spec §6.1/6.2/6.5/6.12 的收口验收。

- [ ] **Step 1: 写测试**（一步到位——它验证的是既有面，无红绿循环；失败即前面某任务有洞）

```go
package cli

// TestDualInstance_E2E (Plan 40 §6.1/6.2/6.5/6.12): one pinned serve, two
// device codes on two profiles; A and B each pull into their own instance;
// the caches are mutually DEK-isolated; B's project token fails on A's cache;
// revoking A's device code quarantines ONLY A's instance.
func TestDualInstance_E2E(t *testing.T) {
	// --- client-side redirection (spec §9.5: instance tests never set SSHMGR_CACHE_DIR) ---
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	// NB: 不 swap DekProvider seam——默认 var 本就是 FileKeyProvider(CacheDekPathFor),
	// per-instance DEK 隔离要测的就是这个真实形态（任何前置测试的 swap 都有 t.Cleanup 还原）。

	// --- serve side: gpu→team-a, secret→team-b; projects projA/projB; codes laptop-agentA/B ---
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	src, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cid, _ := src.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	gpuID, _ := src.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	secretID, _ := src.AddServer(&models.Server{Name: "secret", Host: "192.0.2.99", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	profA, _ := src.AddProfile("team-a")
	profB, _ := src.AddProfile("team-b")
	src.GrantServers(profA, []string{gpuID})
	src.GrantServers(profB, []string{secretID})
	_, projTokenA, _ := src.AddProject("proj-a", profA)
	_, projTokenB, _ := src.AddProject("proj-b", profB)
	_, codeA, _ := src.AddCacheToken("laptop-agentA", profA)
	_, codeB, _ := src.AddCacheToken("laptop-agentB", profB)

	// --- pinned TLS serve (ed25519 self-signed, same shape as clientops' helper) ---
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	cert, _ := x509.ParseCertificate(der)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	r, err := mcpserver.NewServeRunner(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewUnstartedServer(r.HTTPHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pin := mcpserver.SPKIFingerprint(cert)

	// --- §6.1 dual pulls into separate slots ---
	for _, tc := range []struct{ instance, code string }{
		{"laptop-agentA", codeA}, {"laptop-agentB", codeB},
	} {
		if err := clientops.DoPull(srv.URL, tc.code, pin, clientops.PullOpts{Instance: tc.instance}); err != nil {
			t.Fatalf("pull %s: %v", tc.instance, err)
		}
	}
	iA := filepath.Join(userDir, "ssh-manager", "instances", "laptop-agentA")
	iB := filepath.Join(userDir, "ssh-manager", "instances", "laptop-agentB")
	for _, d := range []string{iA, iB} {
		if _, err := os.Stat(filepath.Join(d, "cache.bin")); err != nil {
			t.Fatalf("%s cache.bin: %v", d, err)
		}
	}

	// --- §6.5 DEK isolation: A's DEK cannot decrypt B's bin ---
	dekA, err := os.ReadFile(filepath.Join(dekDir, "cache-dek-laptop-agentA.key"))
	if err != nil {
		t.Fatal(err)
	}
	binB, _ := os.ReadFile(filepath.Join(iB, "cache.bin"))
	if _, derr := vaultio.DecryptWithKey(dekA, binB); derr == nil {
		t.Fatal("A's DEK must NOT decrypt B's cache.bin")
	}

	// --- §6.1 per-instance loads show each profile's set ---
	snapA, err := clientops.LoadCacheSnapshotFor("laptop-agentA")
	if err != nil || len(snapA.Servers) != 1 || snapA.Servers[0].Name != "gpu" {
		t.Fatalf("A view = %+v, %v", snapA, err)
	}
	snapB, _ := clientops.LoadCacheSnapshotFor("laptop-agentB")
	if len(snapB.Servers) != 1 || snapB.Servers[0].Name != "secret" {
		t.Fatalf("B view = %+v", snapB)
	}

	// --- §6.2 cross fail-closed: B's project token does not verify on A's cache ---
	hyd, err := store.Open(filepath.Join(dir, "hyd.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer hyd.Close()
	if err := hyd.ImportSnapshot(snapA); err != nil {
		t.Fatal(err)
	}
	if proj, verr := hyd.VerifyToken(projTokenB); verr == nil || proj != nil {
		t.Fatal("B's project token must NOT validate against A's instance cache")
	}
	if proj, verr := hyd.VerifyToken(projTokenA); verr != nil || proj == nil {
		t.Fatalf("A's own token must validate: %v", verr)
	}

	// --- §6.12 revoke A → A's next pull quarantines ONLY A's slot ---
	if err := src.RevokeCacheToken("laptop-agentA"); err != nil {
		t.Fatal(err)
	}
	err = clientops.DoPull(srv.URL, codeA, pin, clientops.PullOpts{Instance: "laptop-agentA"})
	if !errors.Is(err, clientops.ErrCacheQuarantined) {
		t.Fatalf("revoked A pull must quarantine: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(iA, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("A's bin must be gone (moved to quarantine/)")
	}
	if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-laptop-agentA.key")); !os.IsNotExist(serr) {
		t.Fatal("A's per-instance DEK must be deleted")
	}
	if _, serr := os.Stat(filepath.Join(iB, "cache.bin")); serr != nil {
		t.Fatal("B's instance must be untouched")
	}
	if _, lerr := clientops.LoadCacheSnapshotFor("laptop-agentB"); lerr != nil {
		t.Fatalf("B must stay loadable after A's quarantine: %v", lerr)
	}
}
```

（import 块齐全：crypto/ed25519、crypto/rand、crypto/tls、crypto/x509、crypto/x509/pkix、encoding/pem、errors、math/big、net、net/http/httptest、os、path/filepath、testing、time + 本仓五个包。）

- [ ] **Step 2: 跑测试**

Run: `go test ./internal/cli/ -run TestDualInstance_E2E -v`
Expected: PASS。若 FAIL——按失败点回溯对应任务（门禁/DEK/quarantine）修生产代码，不是修测试。

- [ ] **Step 3: 全仓回归**

Run: `go build ./... && go test ./... && gofmt -l internal/`
Expected: 构建+测试全绿；`gofmt -l` 无任何输出（有输出 = 先 gofmt -w 再继续）。

- [ ] **Step 4: Commit**

```bash
git add internal/cli/multiinstance_e2e_test.go
git commit -m "test(cli): Plan 40 T15 双实例 e2e——拉取/DEK 隔离/交叉 fail-closed/吊销只毁本实例"
```

---

### Task 16: 文档联动（第一批部分，spec §7）

**Files:**
- Modify: `docs/multi-machine.md`（多实例章节）
- Modify: `docs/threat-model.md`（§1.1 登记）
- Modify: `docs/agent-access.md`（一句话指向）
- Modify: `README.md`（flag 说明 + cache-first 措辞）
- Modify: `docs/compat-matrix.md`（两行 + 存量 name 启动失败行）
- Modify: `docs/backlog.md`（销项/登记）

**Interfaces:** 无代码。这是 spec §7 首批清单的落笔任务。

- [ ] **Step 1: multi-machine.md 增「多实例（同机多 agent）」章节**

必须覆盖（spec §7 逐条）：
- enroll 双 agent 流程：owner `cache-tokens add --name <机器-实例>` ×2（各绑各 profile）→ client 两次 `cache pull --url ... --token ... --pin ... --instance <name>`；
- `--instance` 用法：pull / status / mcp 三处 + `.mcp.json` stdio 形态示例（`ssh-manager mcp --cache --instance laptop-agentA --token ...`）；
- 第一批边界：TUI 同步仅默认实例（命名实例用 CLI/计划任务，`schtasks` wrapper 里加 `--instance` 即可）；首次 enroll 无 flag 仍落默认目录（自动归位二批）；doctor 暂不感知；
- 失窃响应（spec §4 措辞，**不得暗示吊销即消除外泄**）：吊销设备码 = 切断未来 pull + 销毁本机该实例材料（下次 pull 401 触发）；**已可能外泄的凭据必须轮换**（受影响 profile 全部凭据）；
- 吊销纪律：快速断 agent = 先吊 device code（下次 pull 销毁该实例 cache）再吊 project token（等 pull/过期）；
- 默认实例换码 runbook：清四件套（auth+bin+meta+quarantine/）后重 enroll；**`cache.config.json` 保留继承**（MAX_OFFLINE 是目录策略非设备码属性——有意拍板）；
- MAX_OFFLINE 持久化：`cache pull --max-offline 24h`；优先级 env > config > 关；
- 过渡期纪律（spec §1.3，直到双端 ≥v0.11.0/hotfix 部署完）：恢复只跑带 env 的 pull 通道；禁用 TUI `[s]` 同步与裸 CLI pull。

- [ ] **Step 2: 其余五处**

- `threat-model.md` §1.1：多凭据集落盘登记——N 实例 = N 份（各自 profile）凭据集；ACL 降低获取概率；MAX_OFFLINE 约束 loader 非密码学时效（DEK 同机无轮换，bin+DEK 同得 = 无限期解密）；每实例独立 DEK = 泄露不连坐。
- `agent-access.md`：离线多 agent 形态一句话 + 指向 multi-machine。
- `README.md`：`--instance` / `--max-offline` flag 行；cache-first 标准姿态措辞（http 直连降为辅助形态）。
- `compat-matrix.md`：v0.11.0×v0.10.0 行（老 serve 受限面：`--instance` 拒、默认门禁不生效、无自动归位——均带提示文案）+ v0.11.0×v0.11.0 全功能行 + 「存量 active name 碰撞/非法 → serve 升级后启动失败（revoke+add 改名恢复）」行。
- `backlog.md`：销项「多实例第一批」；登记二批（TUI 实例列表、向导 `--instance` 接入卡、首次 enroll 自动归位、表单换码预防性警告、`cache config` 子命令）+ doctor 二批 + `cacheMaxAge 不进 config`（YAGNI 决策）。

- [ ] **Step 3: 校对 + 提交**

Run: `go build ./... && go test ./...`（确认文档任务没顺手碰坏代码）

```bash
git add docs/
git commit -m "docs: Plan 40 T16 多实例文档联动——multi-machine 主章节+threat/compat/backlog/README"
```

---

## Self-Review 记录（plan 作者已跑）

- **Spec 覆盖**：§2.1 白名单→T1/T6/T7/T9/T10；§2.2 路径/互斥→T2/T3/T11；§2.3 头→T8；§2.4 门禁/runbook→T9/T10（runbook 语义测试在 T13 Step1 第三案）；§2.5/2.6→T11/T12；§2.7→T14；§2.8 第一批不动=无任务（约束）；§3→T13；§6 测试矩阵→T9(§6.8)、T10(§6.3/6.4)、T11(§6.10/§2.5)、T12(§6.11 status)、T13(§6.9)、T14(§6.11 clear/roles)、T15(§6.1/6.2/6.5/6.12)、§6.6/6.7=各任务"既有用例零改动"门槛；§7→T16。**§2.4 表格第二批列（自动归位）不在本 plan——约束区已声明。**
- **新 env seam**：`SSHMGR_CACHE_DEK_DIR`（T2）是 spec 未点名的实现决策——依据 = grilling 定案「per-instance DEK 生产路径必须有 env seam」+ 多实例测试需要真实 per-instance FileKeyProvider（spec §9.5）。与 `--instance` 可共存（目录级派生），不在 §2.2 冻结互斥清单内。
- **类型一致性**：`CachePathsFor(instance) (dir, bin, meta, audit string, err error)`、`DekProvider func(string) store.KeyProvider`、`PullOpts.Instance string`、`QuarantineCacheFor(instance, reason) (QuarantineResult, error)`、`WriteCacheConfig(dir, v string) error`、`ScanCacheTokenNameAnomalies() ([]string, error)` 全文一致。
