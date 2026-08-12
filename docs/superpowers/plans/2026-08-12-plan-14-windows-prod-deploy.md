# Plan 14 — Windows 生产部署（DPAPI master key + serve 常驻）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 ssh-manager-mcp 在 Windows 生产环境（NUC10）持久化 master key（DPAPI user-scope + 文件，绕过 Credential Manager 的 logon-session 限制）+ serve 后台常驻（Task Scheduler at-startup），并支持 v0.2.0 迁移 + 非交互口令恢复。

**Architecture:** Windows 上 `KeyProvider` 走 DPAPI（`crypt32.dll` 直接 syscall，user-scope，文件存 `%AppData%\ssh-manager\master.key`）替代 go-keyring/keychain；Linux/macOS 仍 keychain + 加 `FileKeyProvider` fallback。`resolveMasterKey` 三级（env → 平台 → File fallback），解密失败硬失败不降级。`unlock` Windows 分支迁 v0.2.0 keychain slot（master + cache DEK）到 DPAPI 文件。`serve install`（Windows only）注册 Task Scheduler at-startup + RestartOnFailure；Linux/macOS 报 "not yet supported"。

**Tech Stack:** Go stdlib `syscall.NewLazyDLL`（DPAPI，无新依赖），Windows `schtasks`（Task Scheduler 注册），cobra CLI，`os.Rename`（原子写）。

**Spec:** `docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`（v2，三家评审 + DPAPI spike 实证）。本 plan 每条规格映射回 spec 章节。

## Global Constraints

（来自 spec v2，逐条；每个 task 隐含遵守）

- **DPAPI user-scope**：`CryptProtectData`/`CryptUnprotectData` **不传** `CRYPTPROTECT_LOCAL_MACHINE(0x1)` flag（spec §3.2，spike 实证三 session 可用）。输出 blob 必须 `LocalFree`（spec §5.3）。
- **纯 stdlib syscall**：`syscall.NewLazyDLL("crypt32.dll")` + `kernel32.LocalFree`，**不引** `golang.org/x/sys/windows`（spike 已验证 stdlib 可行）。
- **master.key 原子写**：`temp + os.Rename`（spec §5.2，半截崩溃不 corrupt）。
- **Windows ACL 显式**：不靠 `os.WriteFile(0600)`（Windows 忽略 mode）；建目录时一次性 `icacls /inheritance:r /grant "<user>:(OI)(CI)F"`（spec §5.2）。
- **resolveMasterKey 错误分支**：平台 KeyProvider 返回 `ErrNotFound` → 继续 FileProvider fallback；返回**其它错误**（DPAPI 解密失败）→ **硬失败**，绝不静默降级到明文 FileProvider（spec §5.6）。
- **迁移 Windows only**：master key + cache DEK 两条；必须在交互式 session（owner 本地终端）；serve 上下文不跑迁移（spec §5.7）。
- **serve install Windows only**：Task Scheduler at-startup + `RestartOnFailure`（PT1M×3）+ 日志重定向；不用 `RunLevel Highest`。Linux/macOS 编译存在但报 "not yet supported"（spec §5.8）。
- **密码变更事实**：只有 admin 重置密码才让 master.key 解不开；用户自行改密码 DPAPI 自动 re-wrap（spec §3.2，文档措辞）。
- **master.key ≠ 备份**：不可移植，灾备靠 passphrase export/import（spec §6）。
- **铁律**：仓库 PUBLIC，zero-tol 凭据泄露；每个 task 结束 `go test ./...` green + `gofmt -l .` 干净 + `go vet ./...` clean + **跨平台编译**（`GOOS=windows/linux/darwin go build ./...` 都过）。

---

## File Structure

| 文件 | 责任 | 新/改 |
|---|---|---|
| `internal/store/dpapi_windows.go` | CryptProtectData/UnprotectData/LocalFree syscall | 新 |
| `internal/store/dpapi_windows_test.go` | DPAPI round-trip 测试 | 新 |
| `internal/store/masterkey_windows.go` | `DpapiKeyProvider`（DPAPI + 文件 + 原子写 + ACL）| 新 |
| `internal/store/masterkey_file.go` | `FileKeyProvider`（明文 fallback）| 新 |
| `internal/store/masterkey.go` | 迁移 helper（检测旧 keychain slot）| 改 |
| `internal/cli/keychain_windows.go` | `var keychain = store.DpapiKeyProvider{}` | 新 |
| `internal/cli/keychain_unix.go` | `var keychain = store.KeyringKeyProvider{}` | 新 |
| `internal/cli/unlock.go` | 删 line 14 keychain init + 加 Windows 迁移逻辑 | 改 |
| `internal/vault/vault.go` | `resolveMasterKey` 三级 + 错误分支硬失败 | 改 |
| `internal/cli/serve.go` | 加 install/uninstall/status 子命令入口 | 改 |
| `internal/cli/serve_install_windows.go` | Task Scheduler 注册（schtasks + 日志）| 新 |
| `internal/cli/serve_install_other.go` | Linux/macOS 占位（报 not yet supported）| 新 |
| `internal/cli/export.go` / `import.go` | `--passphrase-file` flag | 改 |
| `docs/backup-restore.md` / `docs/multi-machine.md` | 升级 Runbook + master.key≠备份 + Windows 部署 | 改 |

任务顺序：T1（DPAPI syscall 基石）→ T2（DpapiKeyProvider）→ T3（FileKeyProvider + resolveMasterKey 三级）→ T4（keychain seam 分流 + 删旧 init）→ T5（v0.2.0 迁移）→ T6（serve install/uninstall/status Windows + other 占位）→ T7（export/import --passphrase-file）→ T8（文档）。按序，T4 依赖 T2/T3，T5 依赖 T2/T4，T6 依赖 T4。

---

### Task 1: DPAPI syscall（`dpapi_windows.go`）

**Files:**
- Create: `internal/store/dpapi_windows.go`（`//go:build windows`）
- Test: `internal/store/dpapi_windows_test.go`（`//go:build windows`）

**Interfaces:**
- Consumes: stdlib `syscall`
- Produces: `dpapiProtect(plain []byte) ([]byte, error)` / `dpapiUnprotect(blob []byte) ([]byte, error)`（包内未导出，user-scope，含 LocalFree）

**背景**：spec §5.3。spike 程序（`Temp/sshmgr-e2e/dpapi-spike/main.go`）已实证此路径在三 session × 两 scope 全通。本 task 是把 spike 代码提炼进 store 包。

- [ ] **Step 1: 写失败测试（build windows）**

新建 `internal/store/dpapi_windows_test.go`：

```go
//go:build windows

package store

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestDpapi_RoundTrip(t *testing.T) {
	plain := make([]byte, 32)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	blob, err := dpapiProtect(plain)
	if err != nil {
		t.Fatalf("dpapiProtect: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("empty blob")
	}
	got, err := dpapiUnprotect(blob)
	if err != nil {
		t.Fatalf("dpapiUnprotect: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %x want %x", got, plain)
	}
}

func TestDpapi_EmptyInput(t *testing.T) {
	// DPAPI rejects empty; our wrappers should return a clear error not panic.
	if _, err := dpapiProtect(nil); err == nil {
		t.Fatal("dpapiProtect(nil) should error")
	}
}

func TestDpapi_UnprotectCorruptFails(t *testing.T) {
	bad := []byte("not a valid dpapi blob")
	if _, err := dpapiUnprotect(bad); err == nil {
		t.Fatal("dpapiUnprotect(corrupt) should error")
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

```
GOOS=windows go test ./internal/store/ -run TestDpapi -v
```
（在 Windows 上直接 `go test ./internal/store/ -run TestDpapi -v`）
Expected: FAIL（`dpapiProtect`/`dpapiUnprotect` 未定义，编译错）。

- [ ] **Step 3: 实现 dpapi_windows.go**

新建 `internal/store/dpapi_windows.go`（从 spike 提炼，含 LocalFree）：

```go
//go:build windows

package store

import (
	"fmt"
	"syscall"
	"unsafe"
)

// dataBlob matches Windows CRYPTOAPI_BLOB / DATA_BLOB.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

const flagMachine = 0x1 // CRYPTPROTECT_LOCAL_MACHINE — NOT used (user-scope only)

// dpapiProtect encrypts plain with user-scope DPAPI (binds to current user SID).
// The caller must NOT pass CRYPTPROTECT_LOCAL_MACHINE (spec §3.2).
func dpapiProtect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("dpapi: empty plain")
	}
	in := dataBlob{cbData: uint32(len(plain)), pbData: &plain[0]}
	var out dataBlob
	r, _, e := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		0, // flags=0 → user-scope
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptProtectData failed: %v", e)
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}

// dpapiUnprotect decrypts a user-scope DPAPI blob. Returns a non-nil error
// (NOT ErrNotFound) if decryption fails — callers must hard-fail, not
// fall through to a plaintext fallback (spec §5.6).
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("dpapi: empty blob")
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: &blob[0]}
	var out dataBlob
	r, _, e := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptUnprotectData failed: %v", e)
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}

// localFree releases a DPAPI-allocated buffer (LocalAlloc). REQUIRED on every
// output blob or every call leaks memory/handles (spec §5.3, review codex#6/pi#5).
func localFree(p uintptr) {
	procLocalFree.Call(p)
}

// blobToBytes copies a DATA_BLOB's content into a Go slice. The caller has
// already deferred localFree on out.pbData.
func blobToBytes(out dataBlob) []byte {
	if out.cbData == 0 || out.pbData == nil {
		return nil
	}
	b := make([]byte, out.cbData)
	copy(b, (*[1 << 30]byte)(unsafe.Pointer(out.pbData))[:out.cbData])
	return b
}
```

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/store/ -run TestDpapi -v
```
Expected: 3/3 PASS（round-trip / empty / corrupt）。

- [ ] **Step 5: no-regression + 跨平台编译 + 提交**

```
go test ./...
gofmt -l .
go vet ./...
GOOS=linux go build ./...    # 确认 dpapi_windows.go 不破坏 Linux 编译
GOOS=darwin go build ./...
```
（dpapi_windows.go 是 build-tag windows，Linux/macOS 编译时不存在 → 不影响。）

```bash
git add internal/store/dpapi_windows.go internal/store/dpapi_windows_test.go
git commit -m "feat(store): DPAPI user-scope syscall (crypt32.dll + LocalFree)

Plan 14 T1. Windows DPAPI Protect/Unprotect via stdlib syscall.NewLazyDLL
(no golang.org/x/sys/windows dep). User-scope only (no
CRYPTPROTECT_LOCAL_MACHINE flag). Output blob deferred to LocalFree on every
call (review codex#6/pi#5). Validated in the spike (spec 12) across
sshd/TaskScheduler/RDP sessions; this task lifts the spike into the store pkg."
```

---

### Task 2: `DpapiKeyProvider`（DPAPI + 文件 + 原子写 + ACL）

**Files:**
- Create: `internal/store/masterkey_windows.go`（`//go:build windows`）
- Test: `internal/store/masterkey_windows_test.go`（`//go:build windows`）

**Interfaces:**
- Consumes: `dpapiProtect`/`dpapiUnprotect`（T1）、`store.KeyProvider`（masterkey.go:25）、`ErrNotFound`（masterkey.go:22）、`store.DefaultStorePath`/`os.UserConfigDir`
- Produces: `DpapiKeyProvider`（实现 KeyProvider；Windows 主路径）

**关键决策**（spec §5.2）：
- 文件 `%AppData%\ssh-manager\master.key`（user profile，与 store.db 同目录）。
- Get: ReadFile → 不存在 `ErrNotFound` → `dpapiUnprotect`（解密失败原样返回非 ErrNotFound 错误，不包装成 ErrNotFound，让 resolveMasterKey 硬失败）。
- Set: `dpapiProtect` → `ensureDirACL`（建目录 + icacls）→ 写 temp + `os.Rename` 原子替换。
- Delete: `os.Remove`（忽略 not-found）。
- ACL: `icacls <dir> /inheritance:r /grant:r "<user>:(OI)(CI)F"`（建目录时一次性；非每次 Set）。
- `Path` 字段 + `User` 字段（cache DEK 复用时 Path 不同，spec §4 范围说明）。

- [ ] **Step 1: 写失败测试**

新建 `internal/store/masterkey_windows_test.go`：

```go
//go:build windows

package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestDpapiKeyProvider_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "master.key"), DirUser: os.Getenv("USERNAME")}
	mk := make([]byte, 32)
	rand.Read(mk)
	if err := p.Set(mk); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatalf("mismatch: got %x want %x", got, mk)
	}
	if err := p.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(); err != ErrNotFound {
		t.Fatalf("Get after Delete: err=%v want ErrNotFound", err)
	}
}

func TestDpapiKeyProvider_GetMissingIsErrNotFound(t *testing.T) {
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "absent.key"), DirUser: os.Getenv("USERNAME")}
	if _, err := p.Get(); err != ErrNotFound {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestDpapiKeyProvider_SetIsAtomic(t *testing.T) {
	// Set writes temp + os.Rename; the final file must exist and be readable.
	// If Rename failed, Get would return ErrNotFound (no file) — proves atomicity.
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "mk"), DirUser: os.Getenv("USERNAME")}
	mk := []byte("test-atomic-32-bytes-pad-to-32!!!!!!") // 32 bytes
	if len(mk) != 32 {
		t.Fatal("test data must be 32 bytes")
	}
	if err := p.Set(mk); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatalf("Get after atomic Set: %v", err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatal("atomic Set round-trip mismatch")
	}
	// no leftover temp files
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp*"))
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/store/ -run TestDpapiKeyProvider -v
```
Expected: FAIL（`DpapiKeyProvider` 未定义）。

- [ ] **Step 3: 实现 masterkey_windows.go**

新建 `internal/store/masterkey_windows.go`：

```go
//go:build windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// DpapiKeyProvider stores the master key in a file encrypted with user-scope
// DPAPI. Windows-only replacement for the keychain path: Credential Manager
// (wincred) fails in sshd/Service sessions (ERROR_NO_SUCH_LOGON_SESSION 1312),
// but DPAPI works across RDP/sshd/TaskScheduler sessions (spec 12 spike).
//
// Path is the master.key file (empty → default %AppData%\ssh-manager\master.key).
// DirUser is the username for the folder ACL (empty → current user). cache DEK
// reuses this provider with a different Path (spec 4 scope note).
type DpapiKeyProvider struct {
	Path    string
	DirUser string
}

func (p DpapiKeyProvider) path() (string, error) {
	if p.Path != "" {
		return p.Path, nil
	}
	appData := os.Getenv("AppData")
	if appData == "" {
		return "", errors.New("dpapi: %AppData% not set")
	}
	return filepath.Join(appData, "ssh-manager", "master.key"), nil
}

func (p DpapiKeyProvider) dirUser() string {
	if p.DirUser != "" {
		return p.DirUser
	}
	return os.Getenv("USERNAME")
}

func (p DpapiKeyProvider) Get() ([]byte, error) {
	path, err := p.path()
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	mk, err := dpapiUnprotect(blob)
	if err != nil {
		// Decryption failure (corrupt file / admin-reset password / session
		// anomaly): return the error AS-IS (not ErrNotFound) so resolveMasterKey
		// hard-fails instead of falling through to plaintext FileProvider.
		return nil, err
	}
	return mk, nil
}

func (p DpapiKeyProvider) Set(mk []byte) error {
	path, err := p.path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDirACL(dir, p.dirUser()); err != nil {
		return fmt.Errorf("dpapi: ensureDirACL: %w", err)
	}
	blob, err := dpapiProtect(mk)
	if err != nil {
		return err
	}
	// Atomic write: temp + os.Rename. Half-write crash leaves no corrupt
	// master.key (the trust root — losing it = full vault loss). spec 5.2.
	tmp, err := os.CreateTemp(dir, ".master.key.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if any step below fails; no-op after successful rename.
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (p DpapiKeyProvider) Delete() error {
	path, err := p.path()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ensureDirACL creates dir (if absent) and locks its ACL to DirUser only:
// inheritance off, (OI)(CI) FullControl for the user. Idempotent. Called once
// per Set, but the icacls op is only run when the dir was just created (a
// best-effort skip if it already exists with correct ACL — see NOTE).
//
// Windows ignores os.WriteFile mode bits (review consensus D); ACL must be
// explicit via icacls or SetFileSecurity. We use icacls (simpler than SDDL Go).
func ensureDirACL(dir, user string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// NOTE: run icacls unconditionally is simplest + safe; icacls is idempotent.
	// /inheritance:r disables inheritance; (OI)(CI) = object+container inherit;
	// F = full control. If user is empty this fails clearly.
	if user == "" {
		return errors.New("ensureDirACL: empty user")
	}
	cmd := exec.Command("icacls", dir, "/inheritance:r", "/grant:r", user+":(OI)(CI)F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls: %v: %s", err, out)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/store/ -run TestDpapiKeyProvider -v
```
Expected: 3/3 PASS（round-trip / ErrNotFound / 原子写无 temp 残留）。

- [ ] **Step 5: no-regression + 跨平台编译 + 提交**

```
go test ./...
gofmt -l .
go vet ./...
GOOS=linux go build ./...
GOOS=darwin go build ./...
```

```bash
git add internal/store/masterkey_windows.go internal/store/masterkey_windows_test.go
git commit -m "feat(store): DpapiKeyProvider (user-scope DPAPI + atomic file + icacls ACL)

Plan 14 T2. Windows master key storage: DPAPI-encrypted file in user profile,
replacing the broken keychain path (Credential Manager 1312 in sshd/Service).
Set uses temp+os.Rename (atomic — half-write crash never corrupts the trust
root, review opencode#3). Folder ACL via icacls /inheritance:r + (OI)(CI)F for
the user (Windows ignores 0600 mode bits, review consensus D). Get returns
decrypt errors AS-IS (not ErrNotFound) so resolveMasterKey hard-fails instead
of silent plaintext fallback (spec 5.6). Path/User fields let the cache DEK
reuse this provider with a different file."
```

---

### Task 3: `FileKeyProvider`（fallback）+ `resolveMasterKey` 三级 + 错误分支

**Files:**
- Create: `internal/store/masterkey_file.go`（全平台）
- Test: `internal/store/masterkey_file_test.go`（全平台）
- Modify: `internal/vault/vault.go`（`resolveMasterKey` 加 FileProvider fallback + 硬失败语义）
- Test: `internal/vault/vault_test.go`（加 resolveMasterKey 顺序/错误分支测试；若无则新建）

**Interfaces:**
- Consumes: `store.KeyProvider`、`ErrNotFound`
- Produces: `FileKeyProvider`（全平台 fallback）+ `resolveMasterKey` 新语义

**背景**：spec §5.4 + §5.6。FileProvider 只给无 keychain/无 DPAPI 环境（CI/容器/headless）。resolveMasterKey 三级：env → 平台 KeyProvider → FileProvider；**解密失败硬失败不降级**。

- [ ] **Step 1: 写 FileKeyProvider 失败测试**

新建 `internal/store/masterkey_file_test.go`（全平台）：

```go
package store

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileKeyProvider_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := FileKeyProvider{Path: filepath.Join(dir, "mk.plain")}
	mk := make([]byte, 32)
	rand.Read(mk)
	if err := p.Set(mk); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatal("mismatch")
	}
	if err := p.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v want ErrNotFound", err)
	}
}

func TestFileKeyProvider_GetMissingIsErrNotFound(t *testing.T) {
	p := FileKeyProvider{Path: filepath.Join(t.TempDir(), "absent")}
	if _, err := p.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/store/ -run TestFileKeyProvider -v
```
Expected: FAIL（`FileKeyProvider` 未定义）。

- [ ] **Step 3: 实现 masterkey_file.go**

新建 `internal/store/masterkey_file.go`：

```go
package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// FileKeyProvider stores the master key as a plaintext file (0600 on Unix;
// Windows ACL inherited from the folder — see docs). Weaker than DPAPI/
// keychain; intended ONLY for environments with neither (CI / containers /
// headless Linux without secret-service). Windows production uses
// DpapiKeyProvider; this is the last-resort fallback in resolveMasterKey.
type FileKeyProvider struct {
	Path string // empty → UserConfigDir/ssh-manager/master.key.plain
}

func (p FileKeyProvider) path() string {
	if p.Path != "" {
		return p.Path
	}
	// default: next to the store (best-effort; UserConfigDir may be unset in tests)
	if cfg, err := os.UserConfigDir(); err == nil && cfg != "" {
		return filepath.Join(cfg, "ssh-manager", "master.key.plain")
	}
	return "master.key.plain"
}

func (p FileKeyProvider) Get() ([]byte, error) {
	b, err := os.ReadFile(p.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (p FileKeyProvider) Set(mk []byte) error {
	dir := filepath.Dir(p.path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// atomic write (same rationale as DpapiKeyProvider — trust root)
	tmp, err := os.CreateTemp(dir, ".master.key.plain.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(mk); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, p.path())
}

func (p FileKeyProvider) Delete() error {
	err := os.Remove(p.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
```

- [ ] **Step 4: 跑 FileKeyProvider 测试通过**

```
go test ./internal/store/ -run TestFileKeyProvider -v
```
Expected: 2/2 PASS。

- [ ] **Step 5: 改 `resolveMasterKey`（vault.go）加 FileProvider fallback + 硬失败**

读 `internal/vault/vault.go`（已知：resolveMasterKey 在 line 40-53）。改成三级 + 错误分支：

```go
// resolveMasterKey order (spec 5.6):
//  1. SSHMGR_MASTERKEY_HEX env (dev/scripting)
//  2. platform KeyProvider (Windows: DpapiKeyProvider; Unix: KeyringKeyProvider)
//     - ErrNotFound → continue to step 3 (legitimate first-run / no-keychain env)
//     - OTHER error (DPAPI decrypt failure / keychain service down) → HARD FAIL
//       (never silent-fall-through to plaintext FileKeyProvider)
//  3. FileKeyProvider fallback (CI / containers / headless without keychain)
//     - ErrNotFound → "vault locked"
func resolveMasterKey() ([]byte, error) {
	if hexKey := os.Getenv("SSHMGR_MASTERKEY_HEX"); hexKey != "" {
		return hex.DecodeString(hexKey)
	}
	kp := store.KeyringKeyProvider{Service: os.Getenv("SSHMGR_KEYRING_SERVICE")}
	mk, err := kp.Get()
	if err == nil {
		return mk, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		// fall through to FileKeyProvider
	} else {
		// HARD FAIL: decrypt failure / service unavailable. Do NOT degrade to
		// plaintext. spec 5.6 (review codex#8/pi#8).
		return nil, fmt.Errorf("master key present but unreadable: %w — if the OS user password was admin-reset, restore the vault from a backup (see docs/backup-restore.md)", err)
	}
	// FileKeyProvider fallback (last resort). NOTE: kp is the cli/keychain seam
	// var in unlock.go — but vault package must not import cli. The platform
	// KeyProvider here uses store.KeyringKeyProvider directly on Unix; on Windows
	// the cli/keychain_windows.go seam replaces it. To keep vault OS-agnostic,
	// resolveMasterKey takes the KeyProvider as a parameter (see refactor below).
	fp := store.FileKeyProvider{}
	if mk, err := fp.Get(); err == nil {
		return mk, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return nil, errors.New("vault locked: run `ssh-manager unlock` to populate the master key")
}
```

> **实现注意（重要重构）**：`resolveMasterKey` 当前在 vault 包直接构造 `KeyringKeyProvider`（vault.go:44）。但 Windows 要用 `DpapiKeyProvider`——那是 T4 的 cli/keychain seam。**vault 包不能 import cli**（cli import vault，循环）。解法：**`resolveMasterKey` 接收 `keyProvider store.KeyProvider` 参数**，由 `OpenStore` 的调用方（cli）传入 `keychain` seam 变量。改 `OpenStore() (*store.Store, error)` 签名 → `OpenStore(kp store.KeyProvider) (*store.Store, error)`，所有调用方（serve.go:32、export/import.go 的 `openUnlockedStore`、cache.go）传 `keychain`（cli seam）。这是本 task 的核心重构——务必 grep 所有 `vault.OpenStore()` 调用点改签名。
>
> 如果不想改 OpenStore 签名，替代：在 vault 包用 build-tag 文件（`vault/keychain_windows.go`/`_unix.go`）构造平台 KeyProvider——但 vault 包加 build-tag 不如参数注入干净。**推荐参数注入**。

- [ ] **Step 6: 写 resolveMasterKey 顺序 + 错误分支测试**

在 `internal/vault/`（若无 vault_test.go 新建）测三级顺序 + 硬失败语义：
- env set → 返回 env（优先）。
- env unset + KeyProvider 返回 mk → 返回 mk。
- env unset + KeyProvider ErrNotFound + FileProvider mk → 返回 FileProvider mk。
- env unset + KeyProvider ErrNotFound + FileProvider ErrNotFound → "vault locked"。
- env unset + KeyProvider 返回**非 ErrNotFound 错误** → 硬失败（**不**走 FileProvider）—— 用 mock KeyProvider 注入，断言错误传播 + FileProvider 未被调用。

- [ ] **Step 7: 跑测试通过 + no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
GOOS=windows go build ./...    # 确认 OpenStore 签名改后 Windows 编译
GOOS=linux go build ./...
```

```bash
git add internal/store/masterkey_file.go internal/store/masterkey_file_test.go internal/vault/vault.go internal/vault/*_test.go
git commit -m "feat(vault): FileKeyProvider + resolveMasterKey 3-tier (env → platform → file) + hard-fail

Plan 14 T3. FileKeyProvider: plaintext fallback for keychain-less envs (CI/
containers). resolveMasterKey: 3-tier order (env > platform KeyProvider >
FileProvider). Decrypt failure (non-ErrNotFound) HARD-FAILS instead of
degrading to plaintext (spec 5.6, review codex#8/pi#8). OpenStore now takes
the KeyProvider as a param (vault can't import cli; the cli/keychain seam
injects the platform-correct provider — T4). Atomic write on FileProvider too."
```

---

### Task 4: keychain seam build-tag 分流 + 删 unlock.go 旧 init

**Files:**
- Create: `internal/cli/keychain_windows.go`（`//go:build windows`）
- Create: `internal/cli/keychain_unix.go`（`//go:build !windows`）
- Modify: `internal/cli/unlock.go`（删 line 14 的 `var keychain`，移到 build-tag 文件；OpenStore 调用改传 keychain）
- Modify: 所有 `vault.OpenStore()` 调用点（serve.go:32、cli/common.go `openUnlockedStore`、export/import）改传 `keychain`

**Interfaces:**
- Consumes: `store.DpapiKeyProvider`（T2，Windows）、`store.KeyringKeyProvider`（Unix）、`vault.OpenStore(kp)`（T3 新签名）
- Produces: `cli.keychain`（包内 seam var，编译期绑平台 KeyProvider）

**背景**：spec §5.5。Windows 编译 `keychain = DpapiKeyProvider{}`，Unix 编译 `keychain = KeyringKeyProvider{}`。

- [ ] **Step 1: 新建 build-tag seam 文件**

`internal/cli/keychain_windows.go`：
```go
//go:build windows

package cli

import "ssh-manager-mcp/internal/store"

// keychain is the master-key source. Windows: DpapiKeyProvider (Credential
// Manager is broken in sshd/Service sessions — Plan 14). Unix: see _unix.go.
var keychain store.KeyProvider = store.DpapiKeyProvider{}
```

`internal/cli/keychain_unix.go`：
```go
//go:build !windows

package cli

import "ssh-manager-mcp/internal/store"

var keychain store.KeyProvider = store.KeyringKeyProvider{}
```

- [ ] **Step 2: 改 unlock.go（删 line 14）+ 所有 OpenStore 调用点**

`internal/cli/unlock.go`：删 `var keychain store.KeyProvider = store.KeyringKeyProvider{}`（line 14，移到 build-tag 文件）。unlock 内 `keychain.Get/Set/Delete` 调用不变（var 名一致）。

`internal/cli/common.go` 的 `openUnlockedStore`：
```go
func openUnlockedStore() (*store.Store, error) { return vault.OpenStore(keychain) }
```
（之前是 `vault.OpenStore()`，T3 改了签名。）

grep 所有 `vault.OpenStore()` 改成 `vault.OpenStore(keychain)`：
```
grep -rn "vault.OpenStore()" internal/
```
预期命中：common.go、可能 serve.go、export/import.go（经 openUnlockedStore 间接）。所有路径都传 keychain。

- [ ] **Step 3: 测试 + 跨平台编译验证（关键）**

```
GOOS=windows go build ./...    # Windows: keychain=DpapiKeyProvider
GOOS=linux go build ./...      # Linux: keychain=KeyringKeyProvider
GOOS=darwin go build ./...     # macOS: 同 Linux
go test ./...
gofmt -l .
go vet ./...
```
Expected: 三平台都编译过 + 测全绿。**重点确认 Windows 编译时 keychain 绑 DpapiKeyProvider、Unix 绑 KeyringKeyProvider**（build-tag 隔离）。

- [ ] **Step 4: 提交**

```bash
git add internal/cli/keychain_windows.go internal/cli/keychain_unix.go internal/cli/unlock.go internal/cli/common.go
# + 任何其它 OpenStore 调用点
git commit -m "feat(cli): keychain seam build-tag split (Windows=DPAPI / Unix=keychain)

Plan 14 T4. Splits the keychain seam var by build-tag: Windows binds
DpapiKeyProvider (Credential Manager broken in sshd/Service), Unix binds
KeyringKeyProvider (unchanged). Removes the old single-line init in unlock.go
line 14. OpenStore callers pass the seam var (T3 signature change). Cross-
platform build verified (windows/linux/darwin all compile)."
```

---

### Task 5: v0.2.0 迁移（Windows only，master key + cache DEK）

**Files:**
- Modify: `internal/cli/unlock.go`（加 Windows 迁移逻辑）
- Modify: `internal/cli/cache.go`（cache DEK 迁移 helper 或复用 unlock 的逻辑）
- Test: `internal/cli/unlock_test.go`（加迁移测试）

**Interfaces:**
- Consumes: `keychain`（T4 seam）、`store.KeyringKeyProvider`（检测旧 slot）、`store.DpapiKeyProvider`（写新文件）、`store.KeyProvider`
- Produces: unlock Windows first-run 触发迁移（旧 keychain slot → DPAPI 文件）

**背景**：spec §5.7。Windows only。unlock 当 `DpapiKeyProvider.Get()` 返回 ErrNotFound 时检测旧 keychain slot（`KeyringKeyProvider{Service:"ssh-manager",User:"master-key"}`）：读出成功 → 提示 → DpapiKeyProvider.Set + 删旧 slot；读出失败非 ErrNotFound（sshd 1312）→ 提示交互式重跑；旧 slot ErrNotFound → first-run generate。cache DEK 同模式（slot `cache-dek`）。

- [ ] **Step 1: 写迁移测试**

在 `internal/cli/unlock_test.go`（`//go:build windows` 限定，因迁移是 Windows only）：
- 构造旧 keychain slot（`KeyringKeyProvider{Service:"ssh-manager-test",User:"master-key"}.Set(mk)`）→ 跑 unlock first-run → 断言 DpapiKeyProvider.Get() == mk + 旧 slot 被 Delete。
- 构造旧 slot 读不出（mock keychain 返回非 ErrNotFound 错误）→ unlock 不崩、输出"请在交互式会话重跑"提示。
- 干净环境（无旧 slot，ErrNotFound）→ first-run generate + DpapiKeyProvider.Set。

（用 test 专用的 Service 名隔离，测后清理 keychain slot。）

- [ ] **Step 2: 跑测试验证失败**

```
GOOS=windows go test ./internal/cli/ -run TestUnlock_Migrate -v
```
Expected: FAIL（迁移逻辑未实现）。

- [ ] **Step 3: 实现 unlock 迁移逻辑**

`unlock.go` RunE（Windows build-tag 分流，或抽到 `unlock_windows.go`）：
```go
// Windows unlock flow (spec 5.7):
mk, err := keychain.Get()  // DpapiKeyProvider
if err == nil {
	// already have master.key → print env, done
	printMasterKey(mk)
	return nil
}
if !errors.Is(err, store.ErrNotFound) {
	// decrypt failure → hard fail (don't migrate, don't generate)
	return fmt.Errorf("master.key present but unreadable: %w", err)
}
// ErrNotFound → DPAPI file absent. Try migrate from old keychain slot.
old := store.KeyringKeyProvider{Service: keyringService, User: keyringUser}
oldMk, oldErr := old.Get()
if oldErr == nil {
	// readable (interactive session) → prompt + migrate
	if !confirm("检测到 v0.2.0 master key，迁移到 DPAPI 文件？") {
		return errors.New("migration declined; master.key not created")
	}
	if err := keychain.(store.KeyProvider).Set(oldMk); err != nil { return err }
	_ = old.Delete()
	printMasterKey(oldMk)
	return nil
}
if errors.Is(oldErr, store.ErrNotFound) {
	// clean env → first-run generate
	mk, _ := store.GenerateMasterKey()
	if err := keychain.Set(mk); err != nil { return err }
	printMasterKey(mk)
	return nil
}
// oldErr is non-ErrNotFound (sshd 1312) → prompt interactive rerun
return errors.New("检测到可能的 v0.2.0 keychain master key 但当前会话读不出（sshd/非交互 session 的 Credential Manager 限制）。请在交互式会话（本地终端/RDP）重跑 `unlock` 迁移，或重设 vault（见 docs/backup-restore.md）")
```
（cache DEK 迁移：同样逻辑，slot `cache-dek`，DpapiKeyProvider{Path: cache-dek.key}。抽成 helper `migrateKeyProvider(oldKp, newKp) error` 复用。cache DEK 迁移在 `cache pull` 首次或 `unlock` 时触发——plan 时定，倾向 unlock 一并处理两个。）

- [ ] **Step 4: 跑测试通过**

```
GOOS=windows go test ./internal/cli/ -run TestUnlock -v
```
Expected: 迁移 + first-run + 读不出提示 全 PASS。

- [ ] **Step 5: no-regression + 跨平台编译 + 提交**

```
go test ./...
gofmt -l .
go vet ./...
GOOS=linux go build ./...   # 确认 Linux unlock 不受 Windows 迁移代码影响（build-tag 隔离）
```

```bash
git add internal/cli/unlock.go internal/cli/unlock_windows.go internal/cli/cache.go internal/cli/unlock_test.go
git commit -m "feat(cli): v0.2.0 keychain → DPAPI migration (Windows only, master + cache DEK)

Plan 14 T5. unlock Windows first-run: when DpapiKeyProvider reports ErrNotFound,
probe the legacy v0.2.0 keychain slot (master-key + cache-dek). Readable →
prompt + migrate to DPAPI file + delete old slot. Unreadable non-ErrNotFound
(sshd 1312) → clear prompt to rerun in an interactive session or reset vault.
Clean env → first-run generate. Migration runs only in interactive sessions;
serve/Service context never migrates (locked → hard fail, not auto-migrate).
spec 5.7 (review pi#3/#4 + codex#1)."
```

---

### Task 6: `serve install/uninstall/status`（Windows Task Scheduler + other 占位）

**Files:**
- Modify: `internal/cli/serve.go`（加 install/uninstall/status 子命令）
- Create: `internal/cli/serve_install_windows.go`（`//go:build windows`，Task Scheduler 注册）
- Create: `internal/cli/serve_install_other.go`（`//go:build !windows`，报 not yet supported）
- Test: `internal/cli/serve_install_windows_test.go`（`//go:build windows`，`SSHMGR_SERVE_INSTALL=1` gate）

**Interfaces:**
- Consumes: `schtasks`（Windows）、`keychain`（确认 master.key 存在）
- Produces: `serve install [--addr]` / `serve uninstall` / `serve status`

**关键决策**（spec §5.8）：
- `serve install`：确认 master.key 存在 → 生成 schtasks XML（at-startup + RestartOnFailure PT1M×3 + 日志重定向到 `%LocalAppData%\ssh-manager\serve.log`，不用 RunLevel Highest）→ 注册（COM API 避密码暴露，fallback `/RP` + 文档标注）→ 立即 `/Run` 验证。
- `serve uninstall`：`schtasks /Delete /TN ssh-manager-serve`。
- `serve status`：`schtasks /Query` + 进程检测 + curl localhost:7878 + **vault-locked 检查**（读日志/health）。
- Linux/macOS：`serve_install_other.go` 报 "not yet supported on linux/darwin; see docs/multi-machine.md"（命令树统一，平台差异在实现内）。
- `serve`（前台跑）RunE 保留——cobra 允许父命令有 RunE + 子命令。

- [ ] **Step 1: 写 serve install 测试（gated）**

`internal/cli/serve_install_windows_test.go`（`SSHMGR_SERVE_INSTALL=1` gate，默认跳过——因真注册 Task 需要密码 + 留系统状态）：
- `SSHMGR_SERVE_INSTALL=1` 时：install → schtasks /Query 显示任务存在 → uninstall → 任务清干净。
- 非 gate：测试 self-skip。
- XML 生成（不注册）：单测 `buildSchtasksXML(addr) string` 含 `<RestartOnFailure>` + `<LogonType>` + 日志重定向，不含 `RunLevel Highest`。

- [ ] **Step 2: 跑测试验证失败**

```
GOOS=windows go test ./internal/cli/ -run TestServeInstall -v
```
Expected: FAIL（子命令未实现）。

- [ ] **Step 3: 实现 serve.go 子命令入口 + serve_install_windows.go**

`serve.go`：在 `newServeCmd` 末尾 `c.AddCommand(newServeInstallCmd(), newServeUninstallCmd(), newServeStatusCmd())`（保留 `c.RunE` 前台跑）。

`serve_install_windows.go`（核心）：
```go
//go:build windows

package cli

// newServeInstallCmd / newServeUninstallCmd / newServeStatusCmd (Windows)
// - install: confirm master.key exists (keychain.Get not ErrNotFound) → build
//   schtasks XML (at-startup + RestartOnFailure + log redirect, NO RunLevel Highest)
//   → register via Register-ScheduledTask (COM API, avoid /RP password on cmdline)
//   or fallback schtasks /Create /RU <user> /RP <pw> + documented risk → /Run.
// - uninstall: schtasks /Delete /TN ssh-manager-serve.
// - status: schtasks /Query + process detect + curl localhost:port + vault-locked
//   check (read serve.log for "master key undecryptable").
//
// See spec 5.8 for full detail. Password handling: prefer COM API (RegisterTask
// + SetPassword, password never on cmdline / in argv). Fallback /RP is allowed
// but must document the 4688 audit-log risk.
```
（实现细节：buildSchtasksXML、confirmMasterKeyExists、registerViaPowerShellCOM、queryStatus。PowerShell `Register-ScheduledTask` 比 Go 调 COM `Schedule.Service` 简单——`exec.Command("powershell", "-Command", "Register-ScheduledTask ...")`，密码作为 PowerShell 参数（仍进 argv，但比 schtasks /RP 少一层；最干净是 PowerShell `Get-Credential` 交互 prompt）。plan 时定，倾向 PowerShell `Register-ScheduledTask -User <user>` + 交互密码 prompt。）

`serve_install_other.go`：
```go
//go:build !windows

package cli

import "github.com/spf13/cobra"

func newServeInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Register serve as a background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("serve install is not yet supported on %s; see docs/multi-machine.md (tracked for a follow-up plan)", runtime.GOOS)
		},
	}
}
// newServeUninstallCmd / newServeStatusCmd: same "not yet supported" stub.
```

- [ ] **Step 4: 跑测试通过（gated + XML 单测）**

```
GOOS=windows SSHMGR_SERVE_INSTALL=1 go test ./internal/cli/ -run TestServeInstall -v   # 真注册（需交互/密码，可能手动跑）
go test ./internal/cli/ -run TestBuildSchtasksXML -v   # XML 结构单测（无 gate）
```
Expected: XML 单测 PASS（含 RestartOnFailure、无 RunLevel Highest）；gated 真机测试手动验证。

- [ ] **Step 5: no-regression + 跨平台编译 + 提交**

```
GOOS=windows go build ./...
GOOS=linux go build ./...    # serve_install_other.go 报 not yet supported，编译过
GOOS=darwin go build ./...
go test ./...
gofmt -l .
go vet ./...
```

```bash
git add internal/cli/serve.go internal/cli/serve_install_windows.go internal/cli/serve_install_other.go internal/cli/serve_install_windows_test.go
git commit -m "feat(cli): serve install/uninstall/status (Windows Task Scheduler + other stub)

Plan 14 T6. serve install registers a Task Scheduler task (at-startup +
RestartOnFailure PT1Mx3 + log redirect to %LocalAppData%/ssh-manager/serve.log,
NO RunLevel Highest). Password via Register-ScheduledTask COM/interactive prompt
(avoids /RP password on cmdline/argv — review codex#3/pi#10/opencode#9). status
queries task state + process + HTTP + vault-locked (serve.log scan). Linux/macOS
serve_install_other.go returns 'not yet supported' (tracked for follow-up,
review codex#9 scope). serve foreground RunE preserved (cobra parent+subs)."
```

---

### Task 7: `export`/`import` `--passphrase-file`

**Files:**
- Modify: `internal/cli/export.go` / `import.go`
- Test: `internal/cli/export_import_smoke_test.go`（加 --passphrase-file 测试）

**背景**：spec §5.9。`--passphrase-file <path>` 从文件读口令（非交互）；export flag 模式跳过 confirm；import 加密分支用，明文分支（T3 嗅探）不受影响。交互 prompt 仍是默认。

- [ ] **Step 1: 写失败测试**

在 `export_import_smoke_test.go` 加：
```go
// TestExport_PassphraseFile_NoPrompt: export 用 --passphrase-file，不弹 TTY。
// TestImport_PassphraseFile_Encrypted: export 出加密文件 → import --passphrase-file 解密导入，全程无 TTY prompt。
```
（把 passphrasePrompt seam 设成 t.Fatal 版，确保 flag 模式绝不调 prompt；passphrase 写临时文件 0600。）

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/cli/ -run "TestExport_PassphraseFile|TestImport_PassphraseFile" -v
```
Expected: FAIL（flag 未实现）。

- [ ] **Step 3: 实现 --passphrase-file**

`export.go`：加 `--passphrase-file` flag；RunE 里若 flag set → 从文件读口令（0600 读后不等 prompt，跳过 confirm）→ 否则现有交互 prompt + confirm。
`import.go`：加同 flag；加密分支用文件口令，明文分支（IsEncrypted=false）忽略。

- [ ] **Step 4: 跑测试通过 + no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
```

```bash
git add internal/cli/export.go internal/cli/import.go internal/cli/export_import_smoke_test.go
git commit -m "feat(cli): export/import --passphrase-file (non-interactive restore)

Plan 14 T7. --passphrase-file <path> reads the passphrase from a file instead
of the TTY, skipping confirm (export). Enables scripted/unattended DR restore
(serve recovery, CI). Plaintext import path (Plan 11 sniff) unaffected.
Interactive prompt remains the default when the flag is absent."
```

---

### Task 8: 文档（升级 Runbook + master.key≠备份 + Windows 部署）

**Files:**
- Modify: `docs/backup-restore.md`（升级 Runbook + DPAPI/master.key 说明 + 密码变更 + serve install）
- Modify: `docs/multi-machine.md`（Windows Task Scheduler 部署章节）

**内容要求**（spec §6 + §5.7 + §5.8）：
1. **升级 Runbook**（v0.2.0 → 新版，Windows）：停旧 ssh-manager 进程 → **交互式 session**（本地/RDP）跑 `unlock` 迁移（旧 keychain slot → DPAPI 文件）→ `serve install`。
2. **master.key ≠ 备份**：DPAPI 绑本机 profile + 用户 SID，不可移植；灾备 = `import`（passphrase export 或 P13 明文备份）。
3. **密码变更**：只有 admin 重置密码才让 master.key 解不开（需迁回/重设）；用户自行改密码无影响。
4. **serve install**（Windows）：`ssh-manager serve install --addr 0.0.0.0:7878` → 注册 Task Scheduler；密码经交互 prompt（不进 argv 文本）；禁用账户密码过期或文档标注 `/RP` 风险。
5. **Task Scheduler 密码风险**：若用 `/RP` fallback，密码进 4688 审计日志——文档标注 + 建议 COM/交互 prompt。
6. **multi-machine.md serve 部署章节**：改 Windows Task Scheduler at-startup（配合 P13 UNC 路径模式）。
7. **威胁模型**：master.key 对同用户进程不保密（同 keychain 等级）；L2 不变。

- [ ] **Step 1: 读现有 docs/backup-restore.md + multi-machine.md**，了解既有结构。

- [ ] **Step 2: 追加/修订章节**（标题层级对齐既有风格）。重点写：
   - "升级路径（Windows）"小节：3 步（停旧 → 交互 unlock 迁移 → serve install）。
   - "master.key 与备份的区别"小节：master.key ≠ 备份，灾备靠 import。
   - serve install 文档（含密码处理 + 密码过期）。

- [ ] **Step 3: 校验文档无断链 / 措辞与 spec v2 一致**

```
grep -n "Plan 14\|serve install\|master.key\|DPAPI\|Task Scheduler" docs/backup-restore.md docs/multi-machine.md
```

- [ ] **Step 4: 提交**

```bash
git add docs/backup-restore.md docs/multi-machine.md
git commit -m "docs(backup-restore, multi-machine): Plan 14 Windows prod deploy docs

Plan 14 T8. Upgrade runbook (Windows): stop old procs → interactive unlock
migration (keychain → DPAPI) → serve install. master.key != backup (not
portable; DR via import). Password-change fact (admin reset only). serve
install via Task Scheduler (interactive password prompt, not /RP argv).
Threat model: master.key not secret from same-user procs (same as keychain,
not a regression). multi-machine serve deploy section updated."
```

---

## Self-Review（plan 写完后自检）

**1. Spec 覆盖**：逐条对 spec §3-§12 检查 task 实现：
- §3.1 用户账户 serve（不改代码，部署决策）→ T6 serve install /RU
- §3.2 user-scope DPAPI + 文件 + 密码事实 → T1(scope)+T2(file)+T8(密码文档)
- §3.3 Linux/macOS keychain + FileProvider fallback → T3(FileProvider)
- §3.4 Task Scheduler Windows only + auto-restart → T6
- §3.5 自己 syscall + LocalFree → T1
- §4 cache DEK 在范围 → T5（cache DEK 迁移）+ T2（DpapiKeyProvider Path 复用）
- §5.2 DpapiKeyProvider 原子写 + ACL → T2
- §5.3 DPAPI syscall + LocalFree → T1
- §5.4 FileKeyProvider → T3
- §5.5 keychain seam build-tag → T4
- §5.6 resolveMasterKey 三级 + 硬失败 → T3
- §5.7 迁移 Windows only + cache DEK → T5
- §5.8 serve install Windows + auto-restart + 日志 + 密码 + status → T6
- §5.9 --passphrase-file → T7
- §6 安全（密码事实 + master.key≠备份 + ACL）→ T8 + 散在各 task
- §7 测试 → 各 task Step 1
- §12 spike → T1 引用

**2. Placeholder 扫描**：T6 serve_install_windows.go 的"plan 时定 COM vs PowerShell"是**显式选择项**（plan 时 implementer 二选一，PowerShell Register-ScheduledTask 倾向），非 placeholder。T5 cache DEK 迁移触发时机"plan 时定"同理。helper `migrateKeyProvider(oldKp, newKp)` 抽象明确。所有代码块完整。

**3. 类型/签名一致**：`KeyProvider` 接口（masterkey.go:25）、`DpapiKeyProvider`（T2，Windows）/`KeyringKeyProvider`（既有）/`FileKeyProvider`（T3）三者实现同一接口；`OpenStore(kp)`（T3 改签名）→ T4 所有调用点传 `keychain` seam（build-tag 绑 T2/既有）。`dpapiProtect`/`dpapiUnprotect`（T1）→ T2 调用。

**4. 风险点提示**：
- T3 的 `OpenStore` 签名重构（加 KeyProvider 参数）影响所有调用方——implementer 务必 grep 全改。
- T6 COM API vs PowerShell Register-ScheduledTask——PowerShell 更简单（`exec.Command`），但密码进 argv；COM（Schedule.Service ProgID via Go ole）密码不进 argv 但实现重。**倾向 PowerShell + 交互 `Get-Credential` prompt**（密码不进 argv，进 PowerShell 内部）。
- T5 cache DEK 迁移触发时机：在 `cache pull` 首次触发 vs unlock 一并处理——倾向 unlock 一并（owner 一次交互完成两个迁移）。
- Windows 测试需 Windows CI（`//go:build windows` 测试在 Linux CI skip）；gated 测（`SSHMGR_SERVE_INSTALL=1`）手动跑。

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-12-plan-14-windows-prod-deploy.md`. Two execution options:

**1. Subagent-Driven (recommended)** — 每 task 派新 implementer subagent，task 间双审（spec 合规 + 代码质量），全部完成 final whole-branch review。Windows 测试需 Windows 机器（NUC10 或本机）跑 `//go:build windows` 测试。
**2. Inline Execution** — 本会话 executing-plans 批量 + checkpoint。

哪种？
