# Plan 16 — 固定路径 + FileKeyProvider（放弃 DPAPI/keyring）— Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 ssh-manager-mcp 的密钥与存储模型从"用户目录 + DPAPI/keyring 用户态密钥"换成"程序指定固定路径 + FileKeyProvider 裸文件 + 硬 ACL"，并用 kardianos/service 统一三平台 service 注册，让 boot 自起的 serve 可靠读 master.key。

**Architecture:** 一个 `paths` 包给出三平台固定路径（Win `C:\ProgramData\ssh-manager\` / Unix `/var/lib/ssh-manager/`）。`FileKeyProvider` 成为唯一生产密钥后端（裸文件 0600 + Windows ACL 硬化）。`KeyProvider` 接口 + `MemKeyProvider` 保留（测试 seam），删 `DpapiKeyProvider` + `KeyringKeyProvider` 生产实现。cache DEK + eval 一并迁 FileKeyProvider。serve install 用 kardianos 替换 Windows-only PowerShell/schtasks。新增 `migrate-path` 子命令迁移文件型 vault。

**Tech Stack:** Go 1.21+、`github.com/kardianos/service`（新增）、`golang.org/x/sys/windows`（ACL，已有间接依赖）、sqlite（modernc）。移除 `github.com/zalando/go-keyring`。

**Spec:** `docs/superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md` (v2)。xcheck 汇总 `.xcheck/20260813-153149/SUMMARY.md`。

## Global Constraints

（spec §3/§5 逐字搬运，每个 task 隐式遵守）

- **路径常量**（spec §3.1）：Windows `C:\ProgramData\ssh-manager\`、Linux `/var/lib/ssh-manager/`、macOS `/var/lib/ssh-manager/`。store.db / master.key / serve.log / cache-dek.key 全进这一棵。
- **env override**（spec §5.1）：`SSHMGR_STORE`（store.db 路径）、`SSHMGR_FILEKEY_PATH`（master.key 路径）覆盖默认固定路径。env 仅供测试/迁移/自定义，文档标注"生产不建议改"。`DefaultStorePath` 本次补读 `SSHMGR_STORE`（现状不读）。
- **master.key 文件名**（spec §4.2）：沿用现状的 `master.key.plain`（`masterkey_file.go:25`）——不改名，减少迁移面。cache DEK 文件名 `cache-dek.key`。
- **KeyProvider 接口保留**（spec §4.1/v2 共识 D）：`Get() ([]byte, error)` + `Set(key []byte) error`（`store/masterkey.go:25`）。保留 `MemKeyProvider`（测试 fake）。仅删 `DpapiKeyProvider`（masterkey_windows.go）、`KeyringKeyProvider`（masterkey.go）生产实现。
- **Windows ACL**（spec §5.2）：纯 Go `golang.org/x/sys/windows` 安全描述符 API，不调 `icacls`。目录 + `master.key` + `store.db` + `cache-dek.key` 同 ACL：`SYSTEM` + `Administrators` 完全控制 + service 账户读/写，`SE_DACL_PROTECTED` 禁用继承，显式移除 `BUILTIN\Users` + `Authenticated Users` + `Everyone`。
- **Unix 权限**（spec §5.2）：目录 `0700`、文件 `0600`，属主 service 账户；bootstrap 由 `serve install`（root 跑）建目录 + chown。
- **resolveMasterKey 两 tier**（spec §4.2）：`SSHMGR_MASTERKEY_HEX` env → FileKeyProvider（删 keychain tier）。
- **migrate-path 职责**（spec §5.3）：只搬文件型 vault；旧后端不可解报错提示 export/import（RDP session），不保留 DPAPI/keyring 读代码。
- **每 task 频繁 commit**，Go 标准格式 commit message（`feat(cli):` / `fix(store):` 等）。
- **代码风格**：match 现有（`internal/store` 用 `errors.Is` + `fs.ErrNotExist` 模式、atomic write via CreateTemp+Rename、comment 密度高）。

---

## File Structure

- **Create**:
  - `internal/paths/paths.go` — 三平台固定路径解析（build-tag 分 `paths_windows.go` / `paths_unix.go`，或运行时 `runtime.GOOS`）。职责：给 vaultDir / storePath / masterKeyPath / cacheDekPath / serveLogPath。
  - `internal/paths/paths_test.go` — 路径解析单测。
  - `internal/cli/migrate_path.go` — `migrate-path` 子命令。
  - `internal/cli/migrate_path_test.go` — migrate-path 单测。
  - `internal/cli/serve_service.go` — kardianos service install/uninstall/status（跨平台，无 build tag）。
  - `internal/store/acl_windows.go` — Windows ACL 设置（`x/sys/windows`）。
  - `internal/store/acl_windows_test.go` — ACL 单测（验证 DACL 三文件 + 目录）。
- **Modify**:
  - `internal/store/masterkey_file.go` — `path()` 默认改固定路径；`Set` 后调 ACL 硬化（Windows）。
  - `internal/store/store.go:56` — `DefaultStorePath()` 改固定路径 + 读 `SSHMGR_STORE`。
  - `internal/store/masterkey.go` — 删 `KeyringKeyProvider` + keyring import；保留接口 + Mem + DeriveFromPassphrase + Meta。
  - `internal/vault/vault.go:63` — `resolveMasterKey` 两 tier。
  - `internal/cli/cache_dek_windows.go` / `cache_dek_unix.go` — DEK provider 改 FileKeyProvider。
  - `internal/eval/broker.go` + `broker_test.go` — keyring 改 FileKeyProvider + `SSHMGR_FILEKEY_PATH`。
  - `internal/cli/serve_install_windows.go` → 删，逻辑迁 `serve_service.go`（kardianos）。
  - `internal/cli/serve_install_other.go` — 删（kardianos 跨平台）。
  - `internal/cli/unlock.go` — 删 Plan 15 migration plumbing（firstRunMigrator/postGetMigrator/outcome 枚举）。
  - `go.mod` / `go.sum` — 加 `kardianos/service`，删 `zalando/go-keyring`（在 eval 迁完后）。
- **Delete**:
  - `internal/store/dpapi_windows.go`
  - `internal/store/masterkey_windows.go`
  - `internal/cli/keychain_windows.go` / `keychain_unix.go`
  - `internal/cli/migrate_windows.go`

---

## Task 1: paths 包 — 三平台固定路径

**Files:**
- Create: `internal/paths/paths.go`, `internal/paths/paths_windows.go`, `internal/paths/paths_unix.go`
- Test: `internal/paths/paths_test.go`

**Interfaces:**
- Produces: `paths.VaultDir() (string, error)`、`paths.StorePath() (string, error)`、`paths.MasterKeyPath() (string, error)`、`paths.CacheDekPath() (string, error)`、`paths.ServeLogPath() (string, error)`。每个函数：先查 env override（`SSHMGR_STORE` 仅 `StorePath`、`SSHMGR_FILEKEY_PATH` 仅 `MasterKeyPath`），否则返回 `filepath.Join(VaultDir(), <filename>)`。

- [ ] **Step 1: 写失败测试**

```go
// internal/paths/paths_test.go
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVaultDir_FixedPath(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "")
	t.Setenv("SSHMGR_FILEKEY_PATH", "")
	got, err := VaultDir()
	if err != nil {
		t.Fatalf("VaultDir: %v", err)
	}
	want := winOrUnix("C:\\ProgramData\\ssh-manager", "/var/lib/ssh-manager")
	if got != want {
		t.Errorf("VaultDir = %q, want %q", got, want)
	}
}

func TestStorePath_EnvOverride(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "/tmp/custom/store.db")
	got, err := StorePath()
	if err != nil || got != "/tmp/custom/store.db" {
		t.Errorf("StorePath = %q,%v; want env override", got, err)
	}
}

func TestMasterKeyPath_NoEnvLandsInVaultDir(t *testing.T) {
	t.Setenv("SSHMGR_FILEKEY_PATH", "")
	got, _ := MasterKeyPath()
	dir, _ := VaultDir()
	want := filepath.Join(dir, "master.key.plain")
	if got != want {
		t.Errorf("MasterKeyPath = %q, want %q", got, want)
	}
}

func TestCacheDekPath(t *testing.T) {
	got, _ := CacheDekPath()
	dir, _ := VaultDir()
	want := filepath.Join(dir, "cache-dek.key")
	if got != want {
		t.Errorf("CacheDekPath = %q, want %q", got, want)
	}
}

func winOrUnix(win, unix string) string {
	if runtime.GOOS == "windows" {
		return win
	}
	return unix
}

var _ = os.Getenv // keep import if unused after edits
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/paths/`
Expected: FAIL — package 不存在 / 函数未定义。

- [ ] **Step 3: 写实现**

```go
// internal/paths/paths.go
package paths

import (
	"os"
	"path/filepath"
)

// MasterKeyFilename is the on-disk master key file (plaintext under L1+ threat model).
const MasterKeyFilename = "master.key.plain"

// CacheDekFilename is the offline-cache DEK file.
const CacheDekFilename = "cache-dek.key"

// StoreFilename is the encrypted vault database.
const StoreFilename = "store.db"

// ServeLogFilename is the serve process log.
const ServeLogFilename = "serve.log"

// VaultDir returns the program-fixed vault directory (env override via
// SSHMGR_STORE / SSHMGR_FILEKEY_PATH is handled per-file, not here).
// See spec §3.1. Platform root from vaultRoot() (paths_windows.go / paths_unix.go).
func VaultDir() (string, error) {
	root, err := vaultRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "ssh-manager"), nil
}

// StorePath returns the store.db path. SSHMGR_STORE overrides (test/migrate).
func StorePath() (string, error) {
	if v := os.Getenv("SSHMGR_STORE"); v != "" {
		return v, nil
	}
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, StoreFilename), nil
}

// MasterKeyPath returns the master.key path. SSHMGR_FILEKEY_PATH overrides (test/migrate).
func MasterKeyPath() (string, error) {
	if v := os.Getenv("SSHMGR_FILEKEY_PATH"); v != "" {
		return v, nil
	}
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, MasterKeyFilename), nil
}

// CacheDekPath returns the offline-cache DEK path.
func CacheDekPath() (string, error) {
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CacheDekFilename), nil
}

// ServeLogPath returns the serve log path.
func ServeLogPath() (string, error) {
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ServeLogFilename), nil
}
```

```go
// internal/paths/paths_windows.go
package paths

// vaultRoot returns the platform data root (Windows: ProgramData).
// See spec §3.1.
func vaultRoot() (string, error) {
	return "C:\\ProgramData", nil
}
```

```go
// internal/paths/paths_unix.go
//go:build !windows

package paths

// vaultRoot returns the platform data root (Linux/macOS: /var/lib).
// See spec §3.1. macOS uses /var/lib too (not Homebrew-tied) — xcheck consensus F.
func vaultRoot() (string, error) {
	return "/var/lib", nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/paths/`
Expected: PASS, 4/4。

- [ ] **Step 5: commit**

```bash
git add internal/paths/
git commit -m "feat(paths): program-fixed vault dir (Win ProgramData, Unix /var/lib)

Plan 16 T1. Spec §3.1. Replaces UserConfigDir-based paths. Env overrides
SSHMGR_STORE / SSHMGR_FILEKEY_PATH preserved per-file for tests/migrate."
```

---

## Task 2: store 路径 + FileKeyProvider 默认改固定路径

**Files:**
- Modify: `internal/store/store.go` (`DefaultStorePath`)
- Modify: `internal/store/masterkey_file.go` (`path()` default)
- Test: existing `internal/store/*_test.go` 跑通（不新建，验证回归）

**Interfaces:**
- Consumes: `paths.StorePath()` / `paths.MasterKeyPath()` (Task 1)。
- Produces: `DefaultStorePath()` 返回固定路径 + 读 `SSHMGR_STORE`；`FileKeyProvider.path()` 默认 `paths.MasterKeyPath()`。

- [ ] **Step 1: 写失败测试**（若 store 现有测试覆盖路径，改其断言；否则补一个）

```go
// 加到 internal/store/store_test.go（若无则新建）
package store

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultStorePath_FixedLocation(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "")
	got, err := DefaultStorePath()
	if err != nil {
		t.Fatalf("DefaultStorePath: %v", err)
	}
	dir := winOrUnix("C:\\ProgramData\\ssh-manager", "/var/lib/ssh-manager")
	want := filepath.Join(dir, "store.db")
	if got != want {
		t.Errorf("DefaultStorePath = %q, want %q", got, want)
	}
}

func TestDefaultStorePath_EnvOverride(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "/tmp/alt.db")
	got, _ := DefaultStorePath()
	if got != "/tmp/alt.db" {
		t.Errorf("env override lost: got %q", got)
	}
}

func winOrUnix(w, u string) string {
	if runtime.GOOS == "windows" {
		return w
	}
	return u
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestDefaultStorePath`
Expected: FAIL（现状返回 UserConfigDir）。

- [ ] **Step 3: 写实现**

`internal/store/store.go:56` 改：
```go
// DefaultStorePath returns the on-disk vault location (program-fixed, spec §3.1/§5.1).
// SSHMGR_STORE overrides (test/migrate). Falls back to paths pkg.
func DefaultStorePath() (string, error) {
	return paths.StorePath()
}
```
import 加 `"ssh-manager-mcp/internal/paths"`。

`internal/store/masterkey_file.go:19` 改：
```go
func (p FileKeyProvider) path() string {
	if p.Path != "" {
		return p.Path
	}
	// default: program-fixed path (spec §3.1). SSHMGR_FILEKEY_PATH read inside paths.
	pth, err := paths.MasterKeyPath()
	if err != nil || pth == "" {
		return "master.key.plain" // last-resort (test env with no fixed path)
	}
	return pth
}
```
import 加 `"ssh-manager-mcp/internal/paths"`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/`
Expected: PASS（新测试 + 现有 store 测试全绿——现有测试若依赖 UserConfigDir 需改用 `SSHMGR_STORE`/`SSHMGR_FILEKEY_PATH` 指向 t.TempDir()，逐个修）。

- [ ] **Step 5: commit**

```bash
git add internal/store/store.go internal/store/masterkey_file.go internal/store/store_test.go
git commit -m "feat(store): default paths → program-fixed (Plan 16 T2)

DefaultStorePath + FileKeyProvider.path use internal/paths (spec §3.1/§5.1).
Env overrides SSHMGR_STORE / SSHMGR_FILEKEY_PATH honored. Existing tests
rerouted to t.TempDir() via env."
```

---

## Task 3: resolveMasterKey 两 tier + 删 keychain seam + unlock plumbing 清理

**Files:**
- Modify: `internal/vault/vault.go` (`resolveMasterKey`)
- Delete: `internal/cli/keychain_windows.go`, `internal/cli/keychain_unix.go`
- Modify: `internal/cli/serve.go`, `internal/cli/mcp.go`（若用 `keychain` seam 变量，改直接 FileKeyProvider）
- Modify: `internal/cli/unlock.go`（删 Plan 15 migration plumbing）
- Delete: `internal/cli/migrate_windows.go`
- Test: `internal/vault/vault_test.go` 改两 tier 断言

**Interfaces:**
- Produces: `resolveMasterKey` 只剩 `SSHMGR_MASTERKEY_HEX` env → FileKeyProvider。

- [ ] **Step 1: 写失败测试**

`internal/vault/vault_test.go` — 改 resolveMasterKey 相关测试：移除 keychain tier 断言，保留 env + FileKeyProvider 两 tier。例如：
```go
func TestResolveMasterKey_TwoTier(t *testing.T) {
	// tier 1: env
	t.Setenv("SSHMGR_MASTERKEY_HEX", hexOf(bytes32))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(t.TempDir(), "no-such")) // tier 2 absent
	mk, err := resolveMasterKey(nil) // no keychain seam anymore
	if err != nil { t.Fatalf("env tier: %v", err) }
	if !bytes.Equal(mk, bytes32) { t.Errorf("env tier mismatch") }

	// tier 2: FileKeyProvider (no env)
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	fp := filepath.Join(t.TempDir(), "mk")
	os.WriteFile(fp, bytes32, 0o600)
	t.Setenv("SSHMGR_FILEKEY_PATH", fp)
	mk2, err := resolveMasterKey(nil)
	if err != nil || !bytes.Equal(mk2, bytes32) { t.Errorf("file tier: %v", err) }
}
```
（具体 hexOf/bytes32 helper 用文件现有 fixture 风格。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/vault/ -run TestResolveMasterKey`
Expected: FAIL（现状三 tier + 引用 keychain seam）。

- [ ] **Step 3: 写实现**

`internal/vault/vault.go:63` `resolveMasterKey` 简化为：
```go
// resolveMasterKey resolves the vault master key in two tiers (spec §4.2):
//  1. SSHMGR_MASTERKEY_HEX env (hex) — tests / explicit config
//  2. FileKeyProvider at the fixed path (SSHMGR_FILEKEY_PATH override or default)
//
// No keychain tier (Plan 16: DPAPI/keyring deleted).
func resolveMasterKey(kp store.KeyProvider) ([]byte, error) {
	if kp != nil {
		mk, err := kp.Get()
		if err == nil {
			return mk, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("master key: %w", err)
		}
	}
	if h := os.Getenv("SSHMGR_MASTERKEY_HEX"); h != "" {
		mk, err := hex.DecodeString(h)
		if err != nil { return nil, fmt.Errorf("SSHMGR_MASTERKEY_HEX: %w", err) }
		return mk, nil
	}
	fp := fileKeyProvider()
	mk, err := fp.Get()
	if err != nil { return nil, err }
	return mk, nil
}
```
（注：`kp` 参数保留——OpenStore(kp) 签名不变，kp 传 FileKeyProvider 或测试 MemKeyProvider；`fileKeyProvider()` 是现有 env-aware 构造器 vault.go:89。）

删除 `internal/cli/keychain_windows.go` + `keychain_unix.go`（两个 `var keychain` seam）。

`serve.go` / `mcp.go` 里 `vault.OpenStore(keychain)` 改 `vault.OpenStore(store.FileKeyProvider{})`（或 nil，让 resolve 走 file tier——实现时择一，保持一致）。

`unlock.go`:删 `firstRunOutcome` 枚举（`:35-49`）、`firstRunMigrator` var（`:62`）、`postGetMigrator` var（`:86`）、相关 init/log。unlock 的核心流程（prompt passphrase、`DeriveFromPassphrase`、`print SSHMGR_MASTERKEY_HEX`、`FileKeyProvider.Set`）保留。删后 unlock.go 应只做"读/生成 master key + 写 FileKeyProvider"。

删除 `internal/cli/migrate_windows.go`（migrateDpapiScope + postGetMigrator 注册，Plan 15 T3）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/vault/ ./internal/cli/ -run "Resolve|Unlock"`
Expected: PASS。全仓 `go build ./...` 应仍编译断（Task 4-7 未做，cache_dek/eval 还引用被删的 KeyringKeyProvider）——**Task 3 故意只删 keychain seam（master-key 路径），cache_dek/eval 在 Task 4/5 迁完后才删 KeyringKeyProvider 类型本身**。所以 Step 4 只跑 vault + unlock 相关测试。

- [ ] **Step 5: commit**

```bash
git rm internal/cli/keychain_windows.go internal/cli/keychain_unix.go internal/cli/migrate_windows.go
git add internal/vault/vault.go internal/vault/vault_test.go internal/cli/serve.go internal/cli/mcp.go internal/cli/unlock.go
git commit -m "refactor(cli): two-tier resolveMasterKey + drop keychain seam (Plan 16 T3)

resolveMasterKey: env → FileKeyProvider only (spec §4.2). Delete
keychain_windows.go / keychain_unix.go seams + migrate_windows.go
(Plan 15 plumbing) + unlock.go migration plumbing. KeyringKeyProvider
type itself deleted in T5 after cache_dek/eval migrate (T4)."
```

---

## Task 4: cache DEK 迁 FileKeyProvider

**Files:**
- Modify: `internal/cli/cache_dek_windows.go`, `internal/cli/cache_dek_unix.go`
- Test: `internal/cli/cache_test.go`, `mcp_cache_test.go`（改 dekProvider seam 注入）

**Interfaces:**
- Consumes: `paths.CacheDekPath()` (Task 1)。
- Produces: 两平台 `var dekProvider` 返回 `store.FileKeyProvider{Path: <fixed>/cache-dek.key}`。

- [ ] **Step 1: 写失败测试**

`cache_test.go` 现有用 `withDEK` 注入 MemKeyProvider——保留 seam（dekProvider 仍是 `var`，可被测试替换）。改测试断言：默认（未注入）dekProvider 返回的 provider 是 FileKeyProvider 且路径 = `paths.CacheDekPath()`：
```go
func TestDefaultDekProvider_IsFileKeyAtFixedPath(t *testing.T) {
	t.Setenv("SSHMGR_FILEKEY_PATH", "") // 不影响 cache-dek（用 SSHMGR_STORE 间接）
	dp := dekProvider()
	fp, ok := dp.(store.FileKeyProvider)
	if !ok { t.Fatalf("default dek not FileKeyProvider: %T", dp) }
	want, _ := paths.CacheDekPath()
	// FileKeyProvider.Path 在 dekProvider 里显式设了 CacheDekPath
	if fp.Path != want { t.Errorf("dek path = %q, want %q", fp.Path, want) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestDefaultDekProvider`
Expected: FAIL（现状 DpapiKeyProvider / KeyringKeyProvider）。

- [ ] **Step 3: 写实现**

`internal/cli/cache_dek_windows.go` 改（保留 build tag + 文件头注释更新）：
```go
//go:build windows
package cli

import "ssh-manager-mcp/internal/paths"

// dekProvider returns the cache-DEK KeyProvider (Plan 16: FileKeyProvider at the
// fixed cache-dek.key path, spec §3.1/§4.2). Was DpapiKeyProvider before Plan 16.
// A package seam so tests inject a fake (MemKeyProvider). See cache_test.go withDEK.
var dekProvider = func() store.KeyProvider {
	pth, err := paths.CacheDekPath()
	if err != nil || pth == "" {
		return &store.FileKeyProvider{} // last-resort default
	}
	return &store.FileKeyProvider{Path: pth}
}
```
（import `store` 保留；`SSHMGR_FILEKEY_PATH` 不影响 cache-dek——cache-dek 用 `paths.CacheDekPath()` 即 `<vaultDir>/cache-dek.key`。）

`internal/cli/cache_dek_unix.go` 同改（build tag `!windows`，删 `SSHMGR_KEYRING_SERVICE` env 读取）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -run "Dek|Cache"`
Expected: PASS（cache pull → mcp --cache roundtrip 在新介质下可解）。

- [ ] **Step 5: commit**

```bash
git add internal/cli/cache_dek_windows.go internal/cli/cache_dek_unix.go internal/cli/cache_test.go
git commit -m "feat(cli): cache DEK → FileKeyProvider at fixed path (Plan 16 T4)

Both platforms' dekProvider now FileKeyProvider (was DpapiKeyProvider Win /
KeyringKeyProvider Unix). Spec §4.2 (xcheck consensus A). Fixes the latent
cross-session custody bug (cache_dek_windows.go comment self-described
served-broker DPAPI reads)."
```

---

## Task 5: eval 迁 FileKeyProvider + 删 KeyringKeyProvider + 移除 go-keyring

**Files:**
- Modify: `internal/eval/broker.go` (`:143, :454`, `evalKeyringService`)
- Modify: `internal/eval/broker_test.go` (`:63`)
- Modify: `internal/store/masterkey.go` (删 `KeyringKeyProvider` + keyring import)
- Modify: `go.mod` / `go.sum`
- Test: `internal/eval/broker_test.go` 跑通

**Interfaces:**
- Consumes: `SSHMGR_FILEKEY_PATH`（eval 给 spawned 子进程指定临时 master.key，替代 `SSHMGR_KEYRING_SERVICE`）。
- Produces: `KeyringKeyProvider` 类型删除；`go-keyring` 依赖移除；eval 用 FileKeyProvider。

- [ ] **Step 1: 写失败测试**

`broker_test.go:63` 改：
```go
// was: gotMK, err := store.KeyringKeyProvider{Service: evalKeyringService}.Get()
evalMKFile := filepath.Join(t.TempDir(), "eval-master.key")
os.WriteFile(evalMKFile, seedMK, 0o600)
gotMK, err := (&store.FileKeyProvider{Path: evalMKFile}).Get()
```
并验证 spawned 子进程经 `SSHMGR_FILEKEY_PATH` 读到 eval 专用 key（Plan 12 CF1 隔离契约保持）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/eval/ -run Broker`
Expected: FAIL（现状用 keyring）。

- [ ] **Step 3: 写实现**

`broker.go`：
- `evalKeyringService` / `evalKeyringServiceLocked` 常量（`:21, :32`）改为 eval master key **文件路径**常量（eval 临时目录下，如 `<evalTmpDir>/master.key` 和 `master-locked.key`）。eval 若需 per-run 隔离，用 `SSHMGR_FILEKEY_PATH` env 传路径给 spawned 子进程（替代 `SSHMGR_KEYRING_SERVICE`）。
- `:143, :454` `store.KeyringKeyProvider{Service:...}` → `store.FileKeyProvider{Path: evalMKPath}`。
- `:159, :311, :468` spawned 子进程 env `"SSHMGR_KEYRING_SERVICE": ...` → `"SSHMGR_FILEKEY_PATH": evalMKPath`。

`masterkey.go`：删 `KeyringKeyProvider` 结构（`:37-82`）、`keyringService`/`keyringUser` 常量（`:17-18`）、`"github.com/zalando/go-keyring"` import（`:12`）。保留 `KeyProvider` 接口、`ErrNotFound`、`MemKeyProvider`、`GenerateMasterKey`、`DeriveFromPassphrase`、`Meta`/`LoadMeta`/`SaveMeta`。

`go.mod`：`go mod tidy` 后 `zalando/go-keyring` 及间接依赖（`danieljoos/wincred`、`godbus/dbus/v5`）移除。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/eval/ && go build ./... && go vet ./...`
Expected: PASS。**这是第一次全仓 `go build` 应通过**（Task 3 删了 keychain seam 但留了 KeyringKeyProvider 类型给 cache_dek/eval 用；Task 4/5 迁完后类型可删）。如果 build 仍有 KeyringKeyProvider 引用，grep 残留并清。

Run: `go test ./...`
Expected: 全绿（除 serve install 集成测试 gated，Task 7 处理）。

- [ ] **Step 5: commit**

```bash
git add internal/eval/broker.go internal/eval/broker_test.go internal/store/masterkey.go go.mod go.sum
git commit -m "refactor(eval,store): eval → FileKeyProvider, delete KeyringKeyProvider + go-keyring (Plan 16 T5)

eval uses SSHMGR_FILEKEY_PATH for spawn isolation (Plan 12 CF1 preserved).
KeyringKeyProvider type + zalando/go-keyring dep removed (spec §4.1/§4.2,
xcheck consensus G+D). go build ./... now clean."
```

---

## Task 6: Windows ACL 硬化（x/sys/windows）

**Files:**
- Create: `internal/store/acl_windows.go`, `internal/store/acl_windows_test.go`
- Modify: `internal/store/masterkey_file.go` (`Set` 后调 ACL)

**Interfaces:**
- Produces: `store.HardenACL(path string) error`（Windows only，build tag）——设 `SYSTEM`+`Administrators`+当前用户/service 账户，禁继承，移除 Users/Authenticated Users/Everyone。

- [ ] **Step 1: 写失败测试**

```go
// internal/store/acl_windows_test.go
//go:build windows
package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHardenACL_RemovesBroadGroups(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "master.key.plain")
	os.WriteFile(f, []byte("x"), 0o600)
	if err := HardenACL(f); err != nil { t.Fatalf("HardenACL: %v", err) }
	// 用 x/sys/windows 读 DACL，断言无 BUILTIN\Users / Authenticated Users / Everyone
	sd, err := getDACLForTest(f) // helper in acl_windows.go (exported via _test)
	if err != nil { t.Fatalf("read DACL: %v", err) }
	for _, banned := range []string{"BUILTIN\\Users", "Authenticated Users", "Everyone"} {
		if trusteeInACL(sd, banned) {
			t.Errorf("DACL still contains banned trustee %s", banned)
		}
	}
	// 断言 SE_DACL_PROTECTED（继承禁用）
	if !isDaclProtected(sd) { t.Error("DACL not protected (inheritance enabled)") }
}
```
（`getDACLForTest`/`trusteeInACL`/`isDaclProtected` 是测试辅助，放 `acl_windows.go` 或 test 文件。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestHardenACL`（Windows）
Expected: FAIL（HardenACL 未实现）。

- [ ] **Step 3: 写实现**

`internal/store/acl_windows.go`：
```go
//go:build windows
package store

import (
	"fmt"
	"unsafe"
	"golang.org/x/sys/windows"
)

// HardenACL sets a restrictive DACL on path (file or dir): SYSTEM + Administrators
// full control + current user read/write, inheritance disabled (SE_DACL_PROTECTED),
// and Users / Authenticated Users / Everyone explicitly removed. Spec §5.2.
func HardenACL(path string) error {
	// 1. Build new explicit DACL with SYSTEM, Administrators, current user.
	// 2. SetSecurityDescriptorDacl with bDaclPresent=TRUE, fDaclDefaulted=FALSE.
	// 3. SetNamedSecurityInfo with PROTECT_DACL_FROM_INHERITANCE.
	// Implementation via windows.CreateWellKnownSid + windows.BuildExplicitAccessWithName
	// (advapi32 SetNamedSecurityInfoW).
	// ... (full impl with error wrapping)
	return nil // placeholder
}
```
（具体 advapi32 调用见 https://learn.microsoft.com/windows/win32/secauthz/creating-a-security-descriptor — 用 `windows.SetNamedSecurityInfo`。实现者参考 `x/sys/windows` 的 `SECURITY_DESCRIPTOR` + `EXPLICIT_ACCESS` + `SetEntriesInAcl`。）

`masterkey_file.go` `Set` 末尾（rename 后）加：
```go
	if runtime.GOOS == "windows" {
		if err := HardenACL(p.path()); err != nil {
			// best-effort? NO — spec §5.2 says ACL is the only protection layer;
			// hard-fail so a silent ACL gap never leaves plaintext world-readable.
			return fmt.Errorf("harden ACL on master key: %w", err)
		}
	}
```
import 加 `"runtime"`。

**注意**（spec §5.2）：非特权进程无权设 ACL 时 HardenACL 报错——上层（serve install / unlock）需提示"需 admin"。Task 7 serve install 会 root/admin 跑。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestHardenACL`（Windows）
Expected: PASS。**CI 跑**：`.github/workflows/` 里 windows-latest job 跑此测试。

- [ ] **Step 5: commit**

```bash
git add internal/store/acl_windows.go internal/store/acl_windows_test.go internal/store/masterkey_file.go
git commit -m "feat(store): harden Windows ACL on master key (Plan 16 T6)

HardenACL via x/sys/windows: SYSTEM+Admins+user, inheritance disabled,
Users/Authenticated Users/Everyone removed (spec §5.2, xcheck consensus E).
Hard-fail on ACL set error (only protection layer under L1+). Covers
master.key; store.db + cache-dek.key ACL applied by their writers reusing HardenACL."
```

---

## Task 7: kardianos service install/uninstall/status

**Files:**
- Create: `internal/cli/serve_service.go`
- Delete: `internal/cli/serve_install_windows.go`, `internal/cli/serve_install_other.go`
- Modify: `internal/cli/serve.go`（subcommands 指向新实现）
- Modify: `go.mod`（加 `github.com/kardianos/service`）
- Test: `internal/cli/serve_install_integration_test.go`（改 gated CI）

**Interfaces:**
- Consumes: `github.com/kardianos/service`。
- Produces: `serve install/uninstall/status` 三平台实现。

- [ ] **Step 1: 写失败测试**

`serve_install_integration_test.go`（gated `SSHMGR_SERVE_INSTALL=1`）改：三平台 CI 真跑 install → status → uninstall。断言 status 四信号（service State、进程、HTTP、vault ok）。非 gated 时 `t.Skip`。
```go
func TestServeInstallRoundtrip(t *testing.T) {
	if os.Getenv("SSHMGR_SERVE_INSTALL") != "1" { t.Skip("gated") }
	// install --addr 127.0.0.1:7879
	// assert status: service Running
	// uninstall
	// assert status: not installed
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `SSHmgr_SERVE_INSTALL=1 go test ./internal/cli/ -run TestServeInstallRoundtrip`
Expected: FAIL（旧 Windows-only 实现 / stub）。

- [ ] **Step 3: 写实现**

`go.mod` 加 `github.com/kardianos/service`（`go get`）。

`internal/cli/serve_service.go`：
```go
package cli

import (
	"github.com/kardianos/service"
	// ...
)

// newServeInstallCmd / newServeUninstallCmd / newServeStatusCmd — kardianos-backed,
// cross-platform. Replace the Windows-only PowerShell/schtasks impl + the Unix stub.
// Spec §5.4.

func runServeInstall(addr string) error {
	cfg := &service.Config{
		Name:        "ssh-manager-serve",
		DisplayName: "ssh-manager serve",
		Description: "ssh-manager-mcp MCP broker",
		Executable:  os.Executable path,
		Arguments:   []string{"serve", "--addr", addr},
		// Option: platform-specific (Win recovery, systemd Restart=on-failure, launchd KeepAlive)
	}
	s, err := service.New(program{}, cfg)
	if err != nil { return err }
	// HardenACL on vault dir before install (Task 6 HardenACL reused on dir)
	if err := installWithACL(s, cfg); err != nil { return err }
	return s.Start()
}

// program implements service.Interface (Start/Stop) — wraps mcpserver.RunServe.
type program struct{}
func (program) Start(s service.Service) error { go program{}.run(s); return nil }
func (program) Stop(s service.Service) error { return nil }
func (program) run(s service.Service) { /* call mcpserver.RunServe with ctx from svc */ }

func runServeStatus() error {
	// four signals: kardianos service.Status (not localized text) + process + HTTP + vault-ok
}
```
（`program` 包装现有 `mcpserver.RunServe`——serve.go 的 RunE 逻辑提取成可被 program.run 调用的函数。）

删除 `serve_install_windows.go` + `serve_install_other.go`。

`serve.go` subcommand 构造指向新 `runServeInstall/Uninstall/Status`。

- [ ] **Step 4: 跑测试确认通过**

Run（本地 Windows）：`SSHmgr_SERVE_INSTALL=1 go test ./internal/cli/ -run TestServeInstallRoundtrip`
Expected: PASS。**CI**（`.github/workflows/serve-install-*.yml`）：windows-latest + ubuntu-latest + macos-latest 三平台矩阵跑此测试。注意 spec §5.5 约束：ubuntu 容器 job 无 systemd、macOS 需 sudo——CI 脚本处理（`sudo`/systemd 探测，失败 t.Skip with reason）。

- [ ] **Step 5: commit**

```bash
git rm internal/cli/serve_install_windows.go internal/cli/serve_install_other.go
git add internal/cli/serve_service.go internal/cli/serve.go internal/cli/serve_install_integration_test.go go.mod go.sum .github/workflows/
git commit -m "feat(cli): kardianos cross-platform serve install/uninstall/status (Plan 16 T7)

Replaces Windows-only PowerShell/schtasks + Unix stub. Spec §5.4.
Three-platform CI integration test (gated SSHMGR_SERVE_INSTALL=1).
kardianos State enum avoids the localized-text bug (Plan 15 T6)."
```

---

## Task 8: migrate-path 子命令

**Files:**
- Create: `internal/cli/migrate_path.go`, `internal/cli/migrate_path_test.go`
- Modify: `internal/cli/root.go`（注册子命令）

**Interfaces:**
- Consumes: `paths` (Task 1)、`store.Open`、`FileKeyProvider`。
- Produces: `ssh-manager migrate-path` 子命令。

- [ ] **Step 1: 写失败测试**

```go
// internal/cli/migrate_path_test.go
package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigratePath_FileVault(t *testing.T) {
	// seed old file-vault at temp dir (FileKeyProvider format)
	oldDir := t.TempDir()
	oldStore := filepath.Join(oldDir, "store.db")
	oldMK := filepath.Join(oldDir, "master.key.plain")
	seedFileVault(t, oldStore, oldMK, 7) // helper: 7 servers

	t.Setenv("SSHMGR_STORE", oldStore)        // tell migrate-path where old is
	newDir := t.TempDir()
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(newDir, "master.key.plain"))
	// redirect fixed path to newDir via SSHMGR_STORE/new store path...
	// (migrate-path reads SSHMGR_STORE as OLD, writes to paths.StorePath() as NEW;
	//  to point NEW at newDir, override vault dir via a test-only env or paths seam)

	if err := runMigratePath(os.Stdout); err != nil { t.Fatalf("migrate: %v", err) }
	// assert: new store has 7 servers, old deleted (unless --keep-old)
	got := countServers(t, newStore)
	if got != 7 { t.Errorf("servers after migrate = %d, want 7", got) }
}

func TestMigratePath_UnreadableBackend_Errors(t *testing.T) {
	// point at a vault whose master.key is unreadable blob (simulate DPAPI fail)
	// expect: error mentioning "export/import in resolvable session"
	err := runMigratePath(os.Stdout)
	if err == nil { t.Fatal("expected error for unreadable backend") }
	// assert err.Error() contains guidance
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestMigratePath`
Expected: FAIL（未实现）。

- [ ] **Step 3: 写实现**

`internal/cli/migrate_path.go`：
```go
package cli

// runMigratePath migrates a file-vault from old location to paths.StorePath().
// Scope (spec §5.3): file-type vaults only. If old master.key is unreadable
// (e.g. DPAPI blob in sshd session), error out with guidance to run
// export + import in a resolvable (RDP/interactive) session.
func runMigratePath(w io.Writer) error {
	oldStore, _ := os.Getenv("SSHMGR_STORE"), nil
	// detect old: SSHMGR_STORE or UserConfigDir fallback
	// read old master key (FileKeyProvider only — no DPAPI/keyring read code)
	// open old store, write to new path, verify N/N, delete old (unless --keep-old)
	// on unreadable: return fmt.Errorf("old master key unreadable in this session; run `export` then `import --passphrase-file` in an RDP/interactive session")
}
```
注册到 root.go：`&cobra.Command{Use: "migrate-path", RunE: ...}`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -run TestMigratePath`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add internal/cli/migrate_path.go internal/cli/migrate_path_test.go internal/cli/root.go
git commit -m "feat(cli): migrate-path subcommand (Plan 16 T8)

File-vault relocation old→fixed path with N/N self-check (spec §5.3).
Unreadable old backend errors with export/import guidance. No DPAPI/keyring
read code (Q6/Q10 clean delete)."
```

---

## Task 9: docs + threat-model + Plan 15 Superseded 横幅

**Files:**
- Create: `docs/threat-model.md`
- Modify: `docs/getting-started.md`（路径表、密钥形态、service 安装、第三方包小节）
- Modify: `docs/multi-machine.md`（serve install 改 kardianos、cache DEK 介质改文件）
- Modify: `docs/backup-restore.md`（若引用 keychain，核查更新——kimi #5）
- Modify: `docs/superpowers/specs/2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md`（顶部加 Superseded 横幅）

**Interfaces:** 无代码，纯文档。

- [ ] **Step 1: 写 threat-model.md**（spec §6 内容落文档）— L1+ 等级、适用前提、残留风险 R1/R2/R3、升级路径 U1/U2/U3、`SSHMGR_MASTERKEY_HEX` env 标注"仅供测试"（O6）。

- [ ] **Step 2: 改 getting-started.md** — 路径表（三平台固定路径）、master.key 形态（裸文件 + ACL）、`serve install` kardianos 三平台命令、新增"第三方服务包"小节（NSSM/systemd/launchd 手动包）。

- [ ] **Step 3: 改 multi-machine.md** — serve install 改 kardianos 命令、cache DEK 介质改文件（`cache-dek.key` 在固定路径）。

- [ ] **Step 4: 核查 backup-restore.md** — grep keychain/DPAPI 引用，更新为 FileKeyProvider + migrate-path。若无引用，跳过。

- [ ] **Step 5: Plan 15 加横幅**

`docs/superpowers/specs/2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md` 顶部（标题后第一行）加：
```
> **⚠ Superseded by Plan 16**（`2026-08-13-plan-16-fixed-path-filekey-design.md`，2026-08-13）：machine-scope DPAPI 路线未验过即作废。根因是用户态密钥模型与"单用户可信机器 + 服务自起"部署形态不匹配，不是 scope 选错。见 Plan 16 §1。
```

- [ ] **Step 6: commit**

```bash
git add docs/threat-model.md docs/getting-started.md docs/multi-machine.md docs/backup-restore.md docs/superpowers/specs/2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md
git commit -m "docs(plan-16): threat-model + getting-started + Plan 15 Superseded (T9)

L1+ threat model + L2 upgrade path (spec §6). Three-platform service install
docs. Plan 15 marked Superseded by Plan 16."
```

---

## Task 10: 全量回归 + NUC10 验收就绪

**Files:** 无（验证 task）

- [ ] **Step 1: 全量测试**

Run: `go test ./...`
Expected: 全绿（含 gated serve install 集成测试在 CI 跑）。

- [ ] **Step 2: build 三平台**

Run: `GOOS=windows go build ./... && GOOS=linux go build ./... && GOOS=darwin go build ./...`
Expected: 三平台编译通过。

- [ ] **Step 3: go vet + go mod tidy**

Run: `go vet ./... && go mod tidy`
Expected: clean。确认 `go-keyring` 在 go.mod 消失，`kardianos/service` 在。

- [ ] **Step 4: secret scan（push 前必做）**

Run: 扫本次 plan 接触的所有文件 + NUC10 key D（绝不落盘）。
Expected: 无活 secret。

- [ ] **Step 5: 准备 NUC10 验收**（spec §7.2）— 把 Plan 16 二进制（ldflag `v0.3.0-rc-acceptance`）交叉编译 Windows，待 §7.2 Phase 1 部署。

- [ ] **Step 6: commit + 整 branch 交 code-reviewer**（subagent-driven-development 末尾的 whole-branch review）。

---

## Self-Review（controller 自查，不派 subagent）

**1. Spec 覆盖**：spec §2 目标 1-8 → T1(paths)/T2(store 路径)/T3(resolve+seam)/T4(cache DEK)/T5(eval+删keyring)/T6(ACL)/T7(kardianos)/T8(migrate-path)；§5 契约 5.1(T1+T2)/5.2(T6)/5.3(T8)/5.4(T7)/5.5(各 task 测试)；§6 threat-model(T9)；§4 删除(T3+T5)/改造(各)/新增(T1+T7+T8+T9)。全覆盖。

**2. Placeholder 扫描**：T6 HardenACL 的 advapi32 实现写了"参考 MSDN + x/sys/windows"——这是**实现指引**（具体 API 已点名），不是 placeholder（代码骨架 + 错误处理语义已定）。T7 program.run 的 `/* call mcpserver.RunServe */` 是指向现有函数的提取点，不是 TBD。可接受。

**3. 类型一致性**：`paths.VaultDir/StorePath/MasterKeyPath/CacheDekPath/ServeLogPath` 在 T1 定义、T2/T4/T7/T8 使用——签名一致。`store.HardenACL(path string) error` 在 T6 定义、T7 `installWithACL` 复用——一致。`runServeInstall/Uninstall/Status` + `runMigratePath` 在 T7/T8 定义、root.go 注册——一致。

**4. Task 顺序依赖**：T1→T2(paths)→T3(resolve，删 seam 但留 KeyringKeyProvider 类型)→T4(cache DEK 迁，不再用 KeyringKeyProvider)→T5(eval 迁 + 删 KeyringKeyProvider + go-keyring，**此时全仓首次 go build 通**)→T6(ACL)→T7(kardianos)→T8(migrate-path)→T9(docs)→T10(回归)。依赖链清晰，无环。

**5. 风险点**：T3 到 T5 之间全仓 `go build` 会断（KeyringKeyProvider 类型被 T3 删了 seam 但 cache_dek/eval 还引用）——**这是故意的**（T3 只删 keychain seam，不删类型；T5 删类型）。每个 task 的 Step 4 只跑该 task 相关测试，不要求全仓 build 直到 T5。subagent-driven 执行时 controller 需知晓此中间态。

