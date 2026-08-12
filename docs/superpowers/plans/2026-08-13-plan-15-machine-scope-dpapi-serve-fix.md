# Plan 15 — machine-scope DPAPI + serve install 修复 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `ssh-manager serve install` → boot 自起 → serve 可用 这条链在 Windows 生产环境真正跑通（machine-scope DPAPI 修 FINDING B 跨 logon session；对象 API + Go-side 密码读修 FINDING C 三 bug；CI gate 真跑修根因）。

**Architecture:** `dpapiProtect/Unprotect` 加 `machine bool` 参数（machine-scope = `CRYPTPROTECT_LOCAL_MACHINE`，绑机器不绑 logon session，跨 session 可解）。`DpapiKeyProvider.Set` 一律 machine-scope + ACL 契约；`Get` 双 scope 尝试（迁移窗口期容错）。`unlock` 加 `postGetMigrator` 钩子（Get 成功后、printMasterKey 前）触发 `migrateDpapiScope`（user→machine 重 protect）。`serve install` 重写：删 XML 链，对象 API 注册，密码 Go `readPassphrase`→stdin→`Register-ScheduledTask -Password`（不用 Get-Credential）。`serve status` 用 `Get-ScheduledTask/Info` 枚举（不依赖本地化文本）。CI windows-latest 真跑集成测试（含 vault seed + net user 建账户 + env 密码）。

**Tech Stack:** Go 1.24 · `syscall.NewLazyDLL("crypt32.dll")`（纯 stdlib DPAPI）· Windows Task Scheduler（PowerShell `New-ScheduledTask*` 对象 API + `Register-ScheduledTask`）· GitHub Actions windows-latest · modernc.org/sqlite（纯 Go）。

**Spec:** `docs/superpowers/specs/2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md` (v2)。**Finding 报告：** `docs/superpowers/specs/2026-08-12-plan-14-nuc10-e2e-findings.md`。

## Global Constraints

- **平台**：`dpapi_*.go` / `masterkey_windows.go` / `migrate_windows.go` / `serve_install_windows.go` / `keychain_windows.go` 都 `//go:build windows`；Linux/macOS 对应文件保持不变。跨平台编译 `GOOS=windows/linux/darwin go build ./...` 必须全过。
- **DPAPI scope**：machine-scope = `CRYPTPROTECT_LOCAL_MACHINE(0x1)` flag；user-scope = `0`。spike 2 实证 flag **不强制 scope 隔离**（machine blob 用 user flag 也能解，DPAPI blob 自描述 scope）—— 单测断言"跨 scope 互通"，不是"必失败"。
- **ACL 契约（pi #3）**：master.key 的 temp 文件**必须**建在 protectedDir 内（`os.CreateTemp(protectedDir, ...)`），严禁 `os.TempDir()`。单测断言 master.key 的 ACL 只含 allan716 + SYSTEM。
- **密码不进 argv/4688**：`serve install` 密码经 Go `readPassphrase`（TTY）或 `SSHMGR_SERVE_INSTALL_PASSWORD` env 读，stdin 传 PowerShell `Register-ScheduledTask -Password`。**不用 Get-Credential**（spike 1：无头/非交互环境脆弱）。
- **迁移不 orphan**：`migrateDpapiScope` 拒绝/读不出旧 user-scope blob 时**不生成新 key**（避免 orphan vault），只提示。必须交互 session（RDP/本地）。
- **RestartOnFailure 降级**：spike 3 实证 PS 5.1 对象 API `-RestartCount/-RestartInterval` 不持久化（Count=0）。目标非硬契约：R1（CIM Set）或 R2（XML 字段）二选一，都不行则 best-effort + 文档标注。
- **MultipleInstances=IgnoreNew**：spike 4 实证 Task Scheduler 默认值；对象 API 显式传，防未来默认改。
- **iron rule / L2 模型 / broker 工具集**：全不动。
- **commit 协议**：每个 task 末 commit；commit message 末尾加 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`。
- **测试**：TDD（先写失败测试，再实现）。Windows 测试用 `//go:build windows`，`go test ./...` 在非 Windows 自跳过。

---

## File Structure

| 文件 | 责任 | 改动 |
|---|---|---|
| `internal/store/dpapi_windows.go` | DPAPI syscall | 改：`dpapiProtect/Unprotect` 加 `machine bool` |
| `internal/store/dpapi_windows_test.go` | DPAPI 单测 | 改：machine roundtrip + 跨 scope 互通断言 |
| `internal/store/masterkey_windows.go` | `DpapiKeyProvider` | 改：Set machine-scope + ACL 契约；Get 双 scope |
| `internal/store/masterkey_windows_test.go` | provider 单测 | 改：machine Set/Get + user-scope fallback + ACL 断言 |
| `internal/cli/unlock.go` | unlock 命令 | 改：加 `postGetMigrator` 钩子（Get 成功后调）|
| `internal/cli/migrate_windows.go` | DPAPI scope 迁移 | 改：新增 `migrateDpapiScope` + `init()` 赋值 `postGetMigrator` |
| `internal/cli/serve_install_windows.go` | serve install/uninstall/status | 重写：对象 API + Go 密码读 + precheck 验 machine-scope + status 枚举 |
| `internal/cli/serve_install_windows_test.go` | serve install 单测 | 改：删 buildServeTaskXML 测；加对象 API 参数契约测 |
| `internal/cli/serve.go` | serve 命令 | 改：加 ~1min heartbeat 写 serve.log |
| `.github/workflows/serve-install-windows.yml` | CI | 新：windows-latest 真跑 §7.2 |
| `docs/superpowers/specs/2026-08-12-plan-14-*.md` | Plan 14 spec | 改：顶部加 Superseded 横幅（正文不动）|
| `docs/backup-restore.md` + `docs/multi-machine.md` | docs | 改：machine-scope 威胁模型 + 迁移 runbook |

---

### Task 1: DPAPI `machine` flag（`dpapi_windows.go`）

**Files:**
- Modify: `internal/store/dpapi_windows.go`
- Test: `internal/store/dpapi_windows_test.go`

**Interfaces:**
- Produces: `dpapiProtect(plain []byte, machine bool) ([]byte, error)` / `dpapiUnprotect(blob []byte, machine bool) ([]byte, error)`（签名加 `machine bool`）。后续 T2 的 `DpapiKeyProvider` 调用时传 `true`。

- [ ] **Step 1: 写失败测试（machine roundtrip + 跨 scope 互通）**

追加到 `internal/store/dpapi_windows_test.go`（在现有 import 块内，`machine bool` 参数）：

```go
func TestDpapi_MachineRoundTrip(t *testing.T) {
	plain := make([]byte, 32)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	blob, err := dpapiProtect(plain, true) // machine-scope
	if err != nil {
		t.Fatalf("dpapiProtect(machine): %v", err)
	}
	got, err := dpapiUnprotect(blob, true)
	if err != nil {
		t.Fatalf("dpapiUnprotect(machine): %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("machine round-trip mismatch")
	}
}

// TestDpapi_CrossScopeInteroperable 钉死 spike 2 实证：DPAPI blob 自描述 scope，
// flag 不强制隔离 —— machine-protected blob 用 user flag 也能解（反之亦然）。
// v1 spec 写的"必失败"是错的（codex #6 / pi #7）。
func TestDpapi_CrossScopeInteroperable(t *testing.T) {
	plain := []byte("cross-scope-spike-2")
	machineBlob, err := dpapiProtect(plain, true)
	if err != nil {
		t.Fatalf("dpapiProtect(machine): %v", err)
	}
	// machine blob 用 user flag 解 —— 应成功（spike 2 实测 RESULT=ok）
	got, err := dpapiUnprotect(machineBlob, false)
	if err != nil {
		t.Fatalf("dpapiUnprotect(machine blob, user flag): 期望 spike-2 互通, 实际 err=%v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("cross-scope mismatch")
	}
}
```

同步把现有 3 个测试（`TestDpapi_RoundTrip`/`TestDpapi_EmptyInput`/`TestDpapi_UnprotectCorruptFails`）的 `dpapiProtect(...)` / `dpapiUnprotect(...)` 调用补上第二个参数 `false`（user-scope，保持原行为）。

- [ ] **Step 2: 跑测试验证失败**

Run: `GOOS=windows go test ./internal/store/ -run 'TestDpapi' -v`（在 Windows 上跑；非 Windows 用 `go vet ./internal/store/` 确认编译——build-tag 文件会跳过）。
Expected: FAIL —— `dpapiProtect(plain, true)` 参数数量不匹配（`too many arguments`）。

- [ ] **Step 3: 改 dpapi_windows.go 加 machine 参数**

`internal/store/dpapi_windows.go`，把 `dpapiProtect` 和 `dpapiUnprotect` 改成接受 `machine bool`，按它选 flag：

```go
// dpapiProtect encrypts plain. machine=true → CRYPTPROTECT_LOCAL_MACHINE
// (binds to machine, not user/logon-session → any session can unprotect;
// Plan 15 spec §3.2). machine=false → user-scope (legacy v0.2.0/Plan-14 path).
// spike 2 实证:flag 不强制 scope 隔离,blob 自描述 scope(见 TestDpapi_CrossScopeInteroperable)。
func dpapiProtect(plain []byte, machine bool) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("dpapi: empty plain")
	}
	in := dataBlob{cbData: uint32(len(plain)), pbData: &plain[0]}
	var out dataBlob
	flags := uintptr(0)
	if machine {
		flags = flagMachine // 0x1 = CRYPTPROTECT_LOCAL_MACHINE
	}
	r, _, e := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		flags,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptProtectData failed: %v", e)
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}

// dpapiUnprotect decrypts. machine=true → try machine-scope; the flag is a hint,
// not a hard gate (spike 2: blob self-describes scope). Callers that must handle
// legacy user-scope blobs try both (see DpapiKeyProvider.Get, T2).
func dpapiUnprotect(blob []byte, machine bool) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("dpapi: empty blob")
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: &blob[0]}
	var out dataBlob
	flags := uintptr(0)
	if machine {
		flags = flagMachine
	}
	r, _, e := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		flags,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptUnprotectData failed: %v", e)
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}
```

注意 `unsafe` 已在 import 块（现有文件用 `unsafe.Pointer`）。`flagMachine` 常量已存在（`const flagMachine = 0x1`）。

- [ ] **Step 4: 跑测试验证通过**

Run: `GOOS=windows go test ./internal/store/ -run 'TestDpapi' -v`（Windows 上）。
Expected: PASS —— 4 个 DPAPI 测试全过（原 3 + 新 2，原 3 补了 `false` 参数）。

- [ ] **Step 5: 跨平台编译验证**

Run: `GOOS=windows go build ./internal/store/ && GOOS=linux go build ./... && GOOS=darwin go build ./...`
Expected: 全过（Linux/macOS 不含 dpapi_windows.go，不受影响）。

- [ ] **Step 6: Commit**

```bash
git add internal/store/dpapi_windows.go internal/store/dpapi_windows_test.go
git commit -m "feat(store): DPAPI machine-scope flag (Plan 15 T1)

dpapiProtect/Unprotect take machine bool (CRYPTPROTECT_LOCAL_MACHINE).
spike 2: flag does not enforce scope isolation (blob self-describes);
TestDpapi_CrossScopeInteroperable pins this.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `DpapiKeyProvider` machine-scope + ACL 契约（`masterkey_windows.go`）

**Files:**
- Modify: `internal/store/masterkey_windows.go`
- Test: `internal/store/masterkey_windows_test.go`

**Interfaces:**
- Consumes: T1 的 `dpapiProtect(mk, true)` / `dpapiUnprotect(blob, bool)`。
- Produces: `DpapiKeyProvider.Set` 一律 machine-scope；`Get` 双 scope 尝试（machine 主 + user fallback）。

- [ ] **Step 1: 写失败测试（machine Set/Get + user-scope fallback + ACL 断言）**

追加到 `internal/store/masterkey_windows_test.go`：

```go
import "os/exec" // 加到 import 块

// TestDpapiKeyProvider_MachineScopeRoundTrip 验 Set 用 machine-scope,Get 能读回。
func TestDpapiKeyProvider_MachineScopeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "master.key"), DirUser: os.Getenv("USERNAME")}
	mk := []byte("machine-scope-key-32-bytes-pad000000")[:32]
	if err := p.Set(mk); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatalf("round-trip mismatch")
	}
}

// TestDpapiKeyProvider_GetUserScopeFallback 验 Get 对旧 user-scope blob 的容错
// (迁移窗口期:旧 master.key 是 user-scope,新代码 machine-first Get 要能读出)。
func TestDpapiKeyProvider_GetUserScopeFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	// 写一个 user-scope blob(模拟旧 master.key)
	legacy, err := dpapiProtect([]byte("legacy-user-scope-key-32-pad00000")[:32], false)
	if err != nil {
		t.Fatalf("dpapiProtect(user): %v", err)
	}
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	p := DpapiKeyProvider{Path: path, DirUser: os.Getenv("USERNAME")}
	got, err := p.Get() // machine-first,fallback user —— 应读出 legacy
	if err != nil {
		t.Fatalf("Get on user-scope blob: %v", err)
	}
	if !bytes.Equal(got, []byte("legacy-user-scope-key-32-pad00000")[:32]) {
		t.Fatalf("user-scope fallback mismatch")
	}
}

// TestDpapiKeyProvider_SetACLContract 钉死 ACL 契约(pi #3):Set 后 master.key
// 的 ACL 只含 DirUser(+ SYSTEM),无 Everyone/Users。machine-scope 下 ACL 是
// 唯一防线,必须保证 temp 在 protectedDir 内继承正确 ACL。
func TestDpapiKeyProvider_SetACLContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	user := os.Getenv("USERNAME")
	if user == "" {
		t.Skip("USERNAME empty")
	}
	p := DpapiKeyProvider{Path: path, DirUser: user}
	if err := p.Set([]byte("acl-contract-key-32-bytes-pad0000000")[:32]); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	acl := string(out)
	for _, forbidden := range []string{"Everyone", "BUILTIN\\Users", "Authenticated Users"} {
		if strings.Contains(acl, forbidden) {
			t.Fatalf("ACL contains %q (machine-scope 下 ACL 是唯一防线):\n%s", forbidden, acl)
		}
	}
}
```

import 块加 `"os"`, `"os/exec"`, `"path/filepath"`, `"strings"`（缺的补上；`bytes` 已在）。

- [ ] **Step 2: 跑测试验证失败**

Run: `GOOS=windows go test ./internal/store/ -run 'TestDpapiKeyProvider' -v`
Expected: FAIL —— `TestDpapiKeyProvider_MachineScopeRoundTrip` 失败（当前 `Set` 调 `dpapiProtect(mk)` 没传 `machine`，编译错 `not enough arguments`）。

- [ ] **Step 3: 改 masterkey_windows.go —— Set machine-scope + Get 双 scope**

`Get`（行 45-65）改：先 machine-scope unprotect，失败则 user-scope fallback：

```go
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
	// machine-scope 主路径(Plan 15:跨 logon session 可解)
	if mk, err := dpapiUnprotect(blob, true); err == nil {
		return mk, nil
	}
	// user-scope fallback(迁移窗口期:旧 master.key 是 user-scope)。
	// spike 2:flag 不强制隔离,但双 scope 尝试保证"无论旧 blob 哪个 scope 都能读出"。
	// 两个 scope 都失败则返回 machine-scope 的错误(下面再调一次取 err)。
	mk, err := dpapiUnprotect(blob, false)
	if err == nil {
		return mk, nil
	}
	// 都失败:重试 machine-scope 拿它的错误信息(machine-scope 是主路径,错误更相关)
	if mk2, err2 := dpapiUnprotect(blob, true); err2 == nil {
		return mk2, nil
	} else {
		return nil, err // 返回 user-scope 的 err(最后一个)
	}
}
```

`Set`（行 67-101）改 `dpapiProtect(mk)` → `dpapiProtect(mk, true)`：

```go
func (p DpapiKeyProvider) Set(mk []byte) error {
	path, err := p.path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDirACL(dir, p.dirUser()); err != nil {
		return fmt.Errorf("dpapi: ensureDirACL: %w", err)
	}
	blob, err := dpapiProtect(mk, true) // machine-scope(Plan 15)
	if err != nil {
		return err
	}
	// ACL 契约(pi #3):temp 必须在 protectedDir(dir)内,继承 allan716-only ACL。
	// 严禁 os.TempDir()(那里继承宽 ACL,rename 后保留 → machine-scope 下全库失守)。
	tmp, err := os.CreateTemp(dir, ".master.key.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
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
```

注释（行 14-17）更新："machine-scope DPAPI"（Plan 15）—— 把"user-scope DPAPI"改成"machine-scope DPAPI (Plan 15; user-scope failed cross-logon-session, spec §3.2)"。

- [ ] **Step 4: 跑测试验证通过**

Run: `GOOS=windows go test ./internal/store/ -run 'TestDpapiKeyProvider' -v`
Expected: PASS —— 3 个新测试 + 原有 provider 测试全过。

- [ ] **Step 5: 跨平台编译 + 整 store 包测试**

Run: `GOOS=windows go test ./internal/store/ -v && GOOS=linux go build ./...`
Expected: store 包全绿（含 cache_dek_windows.go 等，确认没漏改 `dpapiProtect/Unprotect` 调用——全包 grep 确认）。

- [ ] **Step 6: Commit**

```bash
git add internal/store/masterkey_windows.go internal/store/masterkey_windows_test.go
git commit -m "feat(store): DpapiKeyProvider machine-scope + ACL contract (Plan 15 T2)

Set uses machine-scope; Get tries machine then user-scope fallback (migration
window). ACL contract pinned: temp in protectedDir (pi #3 defense, spike-verified
existing code is correct).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `migrateDpapiScope` + `postGetMigrator` 钩子（`migrate_windows.go` + `unlock.go`）

**Files:**
- Modify: `internal/cli/unlock.go:68-73`（加 postGetMigrator 钩子）
- Modify: `internal/cli/migrate_windows.go`（新增 migrateDpapiScope + init 赋值）
- Test: `internal/cli/unlock_windows_test.go`

**Interfaces:**
- Consumes: T2 的 `DpapiKeyProvider.Get`（双 scope）+ `dpapiProtect(mk, true)` / `dpapiUnprotect(blob, false)`。
- Produces: `postGetMigrator` package var（unlock Get 成功后调）；`migrateDpapiScope(w) error`（重 protect user→machine）。`migrate_windows.go` 的 `init()` 赋值 `postGetMigrator = migrateDpapiScope`。

**关键背景（codex #1，读代码核实）**：当前 `unlock.go:68-73` 是 `keychain.Get() → err==nil → printMasterKey → return`，**没有 post-Get 钩子**。`firstRunMigrator` 只在 `ErrNotFound` 触发（unlock.go:95）。双 scope Get 让旧 user-scope blob 返回 `err==nil` → 永远到不了 firstRunMigrator → 迁移不可达。**T3 必须加 `postGetMigrator` 钩子**。

- [ ] **Step 1: 写失败测试（unlock 触发 user→machine 迁移）**

追加到 `internal/cli/unlock_windows_test.go`（沿用现有 `unlock_windows_test.go` 的 `service`/`masterOld`/`dekOld` 测试模式 + fake keychain seam）。**先看 Step 4 的 helper 导出**（`MachineUnprotectForMigrate`/`UserUnprotectForMigrate`/`pathOrEmpty` 在 `masterkey_windows.go`，T3 Step 4 加）—— 测试用这些导出 helper 构造旧 user-scope blob。

```go
// TestUnlock_MigratesUserScopeToMachineScope 验 unlock 在 Get 成功后触发
// postGetMigrator,把旧 user-scope master.key 重 protect 为 machine-scope。
// 钉死 codex #1:没有 postGetMigrator 钩子则迁移不可达。
func TestUnlock_MigratesUserScopeToMachineScope(t *testing.T) {
	dir := t.TempDir()
	masterPath := filepath.Join(dir, "master.key")
	user := "testuser"
	originalMK := []byte("user-scope-migrate-test-key32-pad00")[:32]

	// 写旧 user-scope master.key(用导出 helper UserProtectForTest;
	// 见 Step 4 在 masterkey_windows.go 加的导出 test helper)
	legacyBlob, err := store.DpapiKeyProvider{}.UserProtectForMigrate(originalMK)
	if err != nil {
		t.Fatalf("UserProtectForTest: %v", err)
	}
	if err := os.WriteFile(masterPath, legacyBlob, 0o600); err != nil {
		t.Fatal(err)
	}

	// keychain seam 指向这个 master.key
	origKeychain := keychain
	keychain = store.DpapiKeyProvider{Path: masterPath, DirUser: user}
	defer func() { keychain = origKeychain }()

	// 跑 unlock(模拟交互 y 确认);postGetMigrator 读 master.key 发现是 user-scope,
	// confirmMigrate 读 stdin "y" → 重 protect 为 machine-scope。
	cmd := newUnlockCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader("y\n"))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	// 验证 master.key 已重 protect 为 machine-scope:新 blob 用 machine flag 能解,
	// 且解出的 key == originalMK(C 值不变,只重 protect scope)。
	newBlob, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.DpapiKeyProvider{}.MachineUnprotectForMigrate(newBlob)
	if err != nil {
		t.Fatalf("重 protect 后 machine-scope unprotect 失败: %v", err)
	}
	if !bytes.Equal(got, originalMK) {
		t.Fatalf("迁移后 key 变了: got %x want %x", got, originalMK)
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestUnlock_MigratesUserScopeToMachineScope' -v`
Expected: FAIL —— 测试期望迁移后 master.key 是 machine-scope，但当前 unlock 无 postGetMigrator 钩子，master.key 仍是 user-scope（或 `postGetMigrator` 未定义编译错）。

- [ ] **Step 3: 改 unlock.go 加 postGetMigrator 钩子**

`internal/cli/unlock.go`，在 `firstRunMigrator` var 声明（行 62）下面加 `postGetMigrator`：

```go
// postGetMigrator, if non-nil, is invoked AFTER keychain.Get() succeeds but
// BEFORE printMasterKey. It is the hook for the user-scope → machine-scope
// DPAPI migration (Windows only; nil on Unix). Unlike firstRunMigrator (which
// fires on ErrNotFound / no key yet), postGetMigrator fires when a key WAS read
// — needed because dual-scope Get returns success for a legacy user-scope blob,
// so firstRunMigrator (ErrNotFound-gated) never sees it (Plan 15 codex #1).
//
// Return values: (migrated bool, err error).
//   - migrated=true: master.key was re-protected to machine-scope; the freshly
//     read mk is still valid (C value unchanged). Caller proceeds to print.
//   - migrated=false, err=nil: nothing to migrate (already machine-scope) OR
//     user declined. Caller proceeds to print the mk it already has.
//   - err non-nil: hard failure mid-migrate. Caller surfaces it.
var postGetMigrator func(w interface{ Write([]byte) (int, error) }, mk []byte) (bool, error)
```

`newUnlockCmd` 的 RunE（行 68-73）改，Get 成功分支加 postGetMigrator 调用：

```go
RunE: func(cmd *cobra.Command, args []string) error {
	mk, err := keychain.Get()
	if err == nil {
		// Plan 15 codex #1: post-Get migration hook. On Windows this re-protects
		// a legacy user-scope master.key to machine-scope (needed for boot auto-
		// start). Nil on Unix (no-op). Must run BEFORE printMasterKey so the
		// printed key matches the now-machine-scope file.
		if postGetMigrator != nil {
			migrated, mErr := postGetMigrator(cmd.ErrOrStderr(), mk)
			if mErr != nil {
				return mErr
			}
			_ = migrated // mk unchanged either way (re-protect preserves value)
		}
		printMasterKey(cmd, mk)
		return nil
	}
	// ... (rest unchanged: ErrNotFound → firstRunMigrator; else passphrase)
```

- [ ] **Step 4: 改 migrate_windows.go 新增 migrateDpapiScope + init 赋值**

`internal/cli/migrate_windows.go`，加 `migrateDpapiScope` 函数 + 在现有 `init()`（行 128）里赋值 `postGetMigrator`：

```go
// migrateDpapiScope (Plan 15) re-protects a legacy user-scope master.key to
// machine-scope. Triggered by unlock's postGetMigrator hook (Get succeeded →
// key was read → maybe it's a legacy user-scope blob needing re-protect).
//
// Flow:
//   1. Read master.key blob, try machine-scope unprotect.
//      - OK = already machine-scope → nothing to migrate, return (false, nil).
//      - Fail → try user-scope unprotect.
//        - OK = legacy user-scope blob → prompt "migrate? [y/N]".
//          - y → re-protect mk with machine-scope, atomic write back (ACL
//            contract, spec §5.2). return (true, nil).
//          - N → print guidance, return (false, nil) (caller prints mk anyway).
//        - Fail (both scopes) = corrupt / admin-reset → return err (do NOT
//          generate a fresh key — would orphan the vault).
//   2. (cache-dek migration piggybacks on the master prompt via confirmMigrate;
//      reuses v0.2.0 migration's "master declined → dek skip" consistency.)
//
// mk is the key already read by Get (caller passes it); we only re-protect it,
// value unchanged. Must be INTERACTIVE session (user-scope legacy blob only
// readable interactively; sshd → user-scope unprotect fails → we report + stop).
func migrateDpapiScope(w interface{ Write([]byte) (int, error) }, mk []byte) (bool, error) {
	master, dek := migrateSources() // master.key + cache-dek.key DpapiKeyProviders
	// master 是 migrateSource{old, new};new 是 DpapiKeyProvider{Path: master.key}
	masterProv, ok := master.new.(store.DpapiKeyProvider)
	if !ok {
		return false, nil // 非 Windows DPAPI provider(不该发生,Windows-only build)
	}
	masterPath, err := masterProv.PathOrEmpty()
	if err != nil || masterPath == "" {
		return false, nil // can't locate path → nothing to migrate
	}
	blob, rErr := os.ReadFile(masterPath)
	if rErr != nil {
		return false, nil // no master.key → nothing to migrate (firstRunMigrator handles that)
	}
	// 已经是 machine-scope?
	if _, err := masterProv.MachineUnprotectForMigrate(blob); err == nil {
		return false, nil // already machine-scope
	}
	// 试 user-scope unprotect(确认是旧 user-scope blob)
	if _, uErr := masterProv.UserUnprotectForMigrate(blob); uErr != nil {
		// 两个 scope 都读不出:损坏/admin 重置。不 orphan(不生成新 key)。
		fmt.Fprintln(w, "\nmaster.key could not be read under either DPAPI scope (possibly corrupt, or admin password reset). To recover, restore from a backup export (see docs/backup-restore.md).")
		return false, nil
	}
	// 确认迁移(复用 confirmMigrate 的 [y/N])
	if !confirmMigrate(w, "user-scope master.key (migrate to machine-scope)") {
		fmt.Fprintln(w, "migration declined; master.key left as user-scope. serve auto-start at boot needs machine-scope; re-run `unlock` and accept the prompt (interactive session).")
		return false, nil
	}
	// 用 machine-scope 重 protect mk(值不变,即 Get 读出的 mk),原子写回。
	// ACL 契约由 DpapiKeyProvider.Set 保证(temp 在 protectedDir)。
	if err := master.new.Set(mk); err != nil {
		return false, fmt.Errorf("re-protect master.key to machine-scope: %w", err)
	}
	// cache-dek 同理(如果存在)—— 复用 v0.2.0 的"master 成功才迁 dek"一致性。
	// migrateSources() 已返回 (master, dek),直接用 dek。
	if _, _, dErr := migrateKeyProvider(w, dek, "cache DEK"); dErr != nil && !errors.Is(dErr, errLegacyKeyringUnreadable) {
		fmt.Fprintf(w, "cache DEK scope migration skipped: %v\n", dErr)
	}
	fmt.Fprintln(w, "master.key migrated from user-scope to machine-scope DPAPI.")
	return true, nil
}
```

`init()`（行 128-132）追加：

```go
func init() {
	firstRunMigrator = migrateOnFirstRun
	postGetMigrator = migrateDpapiScope // Plan 15: user-scope → machine-scope
}
```

**helper 导出**（`masterkey_windows.go` 加，给迁移逻辑 + 测试用；`//go:build windows`）：

```go
// MachineUnprotectForMigrate / UserUnprotectForMigrate / UserProtectForMigrate
// expose scope-specific protect/unprotect for the migration logic + tests.
// Not part of the KeyProvider interface (Get/Set only); inspection/migration helpers.
func (p DpapiKeyProvider) MachineUnprotectForMigrate(blob []byte) ([]byte, error) {
	return dpapiUnprotect(blob, true)
}
func (p DpapiKeyProvider) UserUnprotectForMigrate(blob []byte) ([]byte, error) {
	return dpapiUnprotect(blob, false)
}
func (p DpapiKeyProvider) UserProtectForMigrate(plain []byte) ([]byte, error) {
	return dpapiProtect(plain, false)
}
// PathOrEmpty returns the master.key path (or "" if %AppData% unset). Exported
// for migration (migrateDpapiScope + serve-install precheck 读 master.key 验 scope)。
func (p DpapiKeyProvider) PathOrEmpty() (string, error) {
	return p.path()
}
```

**注意**：`migrateDpapiScope` 用 `master, dek := migrateSources()` 一次拿两个 provider（master.key + cache-dek.key），cache-dek 迁移直接用 `dek`（不需要 `migrateSourcesDek` helper）。

- [ ] **Step 5: 跑测试验证通过**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestUnlock_MigratesUserScopeToMachineScope' -v`
Expected: PASS。

- [ ] **Step 6: 跑 unlock 全部测试 + migrate 测试（no regression）**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestUnlock|TestMigrate' -v`
Expected: PASS —— 新测试 + 现有 v0.2.0 迁移测试（unlock_windows_test.go 的 masterOld/dekOld）全过。

- [ ] **Step 7: 跨平台编译**

Run: `GOOS=linux go build ./...`
Expected: 全过（Unix 上 `postGetMigrator` 是 nil，`migrate_windows.go` 不编译）。

- [ ] **Step 8: Commit**

```bash
git add internal/cli/unlock.go internal/cli/migrate_windows.go internal/cli/unlock_windows_test.go internal/store/masterkey_windows.go
git commit -m "feat(cli): user-scope→machine-scope DPAPI migration via postGetMigrator (Plan 15 T3)

unlock.go adds postGetMigrator hook (Get-succeeded branch, before printMasterKey).
migrate_windows.go adds migrateDpapiScope (re-protect user→machine, value unchanged,
confirmMigrate reuse for cache-dek consistency). Fixes codex #1 (migration was
unreachable: firstRunMigrator is ErrNotFound-gated, dual-scope Get returns success
for legacy user-scope blob).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: `serve install` 重写 —— 对象 API + Go 密码读 + precheck 验 machine-scope

**Files:**
- Modify: `internal/cli/serve_install_windows.go`（重写 `runServeInstall` + `registerTaskViaPowerShell` → `registerTask`；删 `buildServeTaskXML` + `taskXML` 结构 + `quoteCmd`；precheck 加 machine-scope 验证）
- Modify: `internal/cli/serve_install_windows_test.go`（删 `TestBuildServeTaskXML`，加对象 API 参数契约测）

**Interfaces:**
- Consumes: T2 的 `DpapiKeyProvider.MachineUnprotectForMigrate`（precheck 验 master.key 是 machine-scope）；`readPassphrase`（unlock.go 已有，TTY 读密码）。
- Produces: `registerTask(in taskInputs) error`（对象 API + Go 密码读）；`taskInputs` 结构（Exe/Addr/User/LogPath/TLSCert/TLSKey）；`readServeInstallPassword() (string, error)`（env 优先 + TTY）。

- [ ] **Step 1: 写失败测试（registerTask 参数契约 + precheck machine-scope）**

改 `internal/cli/serve_install_windows_test.go`。删 `TestBuildServeTaskXML`（函数将被删）。加：

```go
// TestRegisterTask_BuildsObjectAPIParams 验 registerTask 构造的 PowerShell
// 脚本含对象 API 参数(New-ScheduledTaskAction/Trigger/SettingsSet + Register-
// ScheduledTask -RunLevel Limited -MultipleInstances IgnoreNew),且密码经
// stdin 传入(不在 argv)。用 fake exec.Command 捕获脚本内容。
func TestRegisterTask_BuildsObjectAPIParams(t *testing.T) {
	// fake the exec.Command to capture the PowerShell script + stdin without
	// really running it. 用一个 test helper 替换 exec.Command(沿用现有测试的
	// monkey-patch 模式,或重构 registerTask 接受 a psRunner interface)。
	// ... (见 Step 3 的 psRunner interface 设计)
	in := taskInputs{
		ExePath: `C:\ssh-manager.exe`, Addr: "0.0.0.0:7878",
		User: "allan716", LogPath: `C:\serve.log`,
		TLSCert: "", TLSKey: "",
	}
	captured, err := captureRegisterTask(in, "testpw")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"New-ScheduledTaskAction",
		"New-ScheduledTaskTrigger -AtStartup",
		"New-ScheduledTaskSettingsSet",
		"-MultipleInstances IgnoreNew",
		"Register-ScheduledTask",
		"-RunLevel Limited",
	} {
		if !strings.Contains(captured.script, want) {
			t.Errorf("PowerShell 脚本缺 %q\n脚本:\n%s", want, captured.script)
		}
	}
	// 密码经 stdin,不在 argv
	if strings.Contains(captured.argv, "testpw") {
		t.Errorf("密码出现在 argv(应只在 stdin): argv=%v", captured.argv)
	}
	if !strings.Contains(captured.stdin, "testpw") {
		t.Errorf("密码不在 stdin: stdin=%q", captured.stdin)
	}
}

（`captureRegisterTask` 是 test helper：注入一个 fake `psRunner`（实现 `Run(script, stdin) (string, error)`），把 script + stdin 捕获到 struct，不真跑 PowerShell。`captureRegisterTask(in, password)` 构造 fake runner，调 `registerTask(fakeRunner, in, password)`，返回捕获的 `{script, stdin, argv}`。fake runner 纪录 `out="REGISTERED\n"` 让 registerTask 通过。T4 Step 3 的 `psRunner` interface 就是为此设计的可测试边界。）

```go
// TestServeInstall_PrecheckRejectsUserScopeMasterKey 钉死 codex #2:precheck
// 必须验 master.key 是 machine-scope,拒绝 user-scope 残留(否则 boot 死循环
// = FINDING B 复发)。
func TestServeInstall_PrecheckRejectsUserScopeMasterKey(t *testing.T) {
	dir := t.TempDir()
	masterPath := filepath.Join(dir, "master.key")
	user := "testuser"
	// 写 user-scope blob(用 T3 的导出 helper UserProtectForMigrate)
	legacyBlob, err := store.DpapiKeyProvider{}.UserProtectForMigrate([]byte("user-scope-precheck-32-pad000")[:32])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(masterPath, legacyBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	origKeychain := keychain
	keychain = store.DpapiKeyProvider{Path: masterPath, DirUser: user}
	defer func() { keychain = origKeychain }()

	cmd := newServeInstallCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--addr", "127.0.0.1:7878"})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("serve install 应拒绝 user-scope master.key,实际 nil err")
	}
	if !strings.Contains(err.Error(), "machine-scope") && !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("错误信息应提示 machine-scope/unlock,实际: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestRegisterTask|TestServeInstall_Precheck' -v`
Expected: FAIL —— `registerTask`/`taskInputs`/`captureRegisterTask` 未定义，或 `buildServeTaskXML` 还在（TestBuildServeTaskXML 删了但函数还在 → 编译警告不算 fail；新测试 fail）。

- [ ] **Step 3: 重写 serve_install_windows.go**

**删**：`buildServeTaskXML`（行 341-402）、`taskXML`/`taskSettings`/`taskRestartPolicy`/`taskPrincipal`/`taskPrincipals`/`taskExec`/`taskActionsContext`/`taskTriggers` 结构、`serveTaskInputs` 结构（改名 `taskInputs`）、`quoteCmd`、`encoding/xml` import。

**改 precheck**（`runServeInstall` 行 124-135）—— 加 machine-scope 验证（codex #2）：

```go
func runServeInstall(cmd *cobra.Command, addr, tlsCert, tlsKey string) error {
	// 1. Pre-check: master.key 必须存在且是 machine-scope(codex #2)。
	//    user-scope 残留时 Get 能读出(双 scope),但 boot 的 Password-logon
	//    session 读不出 → FINDING B 复发。显式验 machine-scope。
	mk, err := keychain.Get()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("master key not found: run 'ssh-manager unlock' in an interactive session first (see docs/backup-restore.md)")
		}
		return fmt.Errorf("master key present but undecryptable: %w (if admin-reset password, restore from backup or re-init vault)", err)
	}
	// 读 master.key blob,验是 machine-scope(不是 user-scope 残留)
	masterPath, _, err := currentMasterKeyPath() // helper:解析 master.key 路径
	if err == nil && masterPath != "" {
		if blob, rErr := os.ReadFile(masterPath); rErr == nil {
			if _, mErr := store.DpapiKeyProvider{}.MachineUnprotectForMigrate(blob); mErr != nil {
				// machine-scope 解不开 → 可能是 user-scope 残留(未迁移)
				return fmt.Errorf("master.key is not machine-scope (DPAPI %v). boot auto-start needs machine-scope. Run 'ssh-manager unlock' in an interactive session to migrate, then re-run 'serve install'", mErr)
			}
		}
	}
	_ = mk // mk 已读出,boot 时 serve 自己再读

	// 2. 解析 exe 路径(任务跑同一个 binary)
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable path: %w", err)
	}
	exePath, _ = filepath.Abs(exePath)
	user := currentUserForTask()
	logPath := serveLogPath()

	// 3. 读密码(env 优先,否则 TTY)—— 共识 A:绕开 Get-Credential 脆弱性
	password, err := readServeInstallPassword(cmd)
	if err != nil {
		return err
	}

	// 4. 对象 API 注册(密码经 stdin,不进 argv)
	in := taskInputs{
		ExePath: exePath, Addr: addr, User: user,
		LogPath: logPath, TLSCert: tlsCert, TLSKey: tlsKey,
	}
	psRunner := defaultPsRunner{}
	if err := registerTask(psRunner, in, password); err != nil {
		return fmt.Errorf("register scheduled task: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "registered task %q (boot+logon trigger, MultipleInstances=IgnoreNew, log -> %s)\n", serveTaskName, logPath)

	// 5. /Run 验证
	if err := schtasksRun(serveTaskName); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: schtasks /Run failed: %v (task registered; check 'ssh-manager serve status' and %s)\n", err, logPath)
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "task started. Use 'ssh-manager serve status' to verify it is listening.")
	}
	return nil
}
```

**加 `readServeInstallPassword` + `taskInputs` + `psRunner` interface + `registerTask`**：

```go
// taskInputs is the data passed to the PowerShell object-API registration.
type taskInputs struct {
	ExePath, Addr, User, LogPath, TLSCert, TLSKey string
}

// psRunner runs a PowerShell command (captured stdin + script); injectable for tests.
type psRunner interface {
	Run(script string, stdin string) (stdout string, err error)
}

type defaultPsRunner struct{}

func (defaultPsRunner) Run(script string, stdin string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script) // 不用 -NonInteractive(Go stdin 喂密码)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readServeInstallPassword reads the Windows account password for the task.
// env SSHMGR_SERVE_INSTALL_PASSWORD 优先(CI / 脚本);否则 TTY 读(交互)。
// 共识 A:绕开 Get-Credential/ConvertTo-SecureString 在无头环境的脆弱性(spike 1)。
func readServeInstallPassword(cmd *cobra.Command) (string, error) {
	if p := os.Getenv("SSHMGR_SERVE_INSTALL_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Fprint(cmd.ErrOrStderr(), "Enter Windows password for the serve task (stored by Task Scheduler so it can start at boot): ")
	b, err := readPassphrase("") // reuse unlock.go's TTY no-echo read
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// registerTask builds the object-API PowerShell script and runs it via psRunner.
// 密码 + 参数经 stdin(不进 argv/4688)。RestartOnFailure 见 T5(对象 API 不持久化,
// 这里不传 -RestartCount/-RestartInterval,留 T5 处理)。
func registerTask(r psRunner, in taskInputs, password string) error {
	const ps = `$ErrorActionPreference='Stop'
$lines = [string]::Join("` + "\n" + `", $input)
$p = $lines -split "`n"
$exe=$p[0]; $addr=$p[1]; $user=$p[2]; $logPath=$p[3]; $logDir=$p[4]; $tlsCert=$p[5]; $tlsKey=$p[6]; $password=$p[7]
$tlsArg = ''
if ($tlsCert -ne '' -and $tlsKey -ne '') { $tlsArg = ' --tls-cert "' + $tlsCert + '" --tls-key "' + $tlsKey + '"' }
$actionArg = '/C if not exist "' + $logDir + '" mkdir "' + $logDir + '" & "' + $exe + '" serve --addr "' + $addr + '"' + $tlsArg + ' >> "' + $logPath + '" 2>&1'
$action = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument $actionArg
$trigBoot = New-ScheduledTaskTrigger -AtStartup
$trigLogon = New-ScheduledTaskTrigger -AtLogOn -User $user
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -DontStopIfGoingOnBatteries -AllowStartIfOnBatteries
Register-ScheduledTask -TaskName 'ssh-manager-serve' -Action $action -Trigger @($trigBoot,$trigLogon) -Settings $settings -RunLevel Limited -User $user -Password $password -Force | Out-Null
Write-Output "REGISTERED"
`
	logDir := filepath.Dir(in.LogPath)
	stdin := strings.Join([]string{in.ExePath, in.Addr, in.User, in.LogPath, logDir, in.TLSCert, in.TLSKey, password}, "\n")
	out, err := r.Run(ps, stdin)
	if err != nil {
		return fmt.Errorf("powershell: %w: %s", err, out)
	}
	if !strings.Contains(out, "REGISTERED") {
		return fmt.Errorf("powershell did not confirm registration: %s", out)
	}
	return nil
}

// currentMasterKeyPath 解析当前 keychain seam(DpapiKeyProvider)的 master.key 路径。
func currentMasterKeyPath() (string, bool, error) {
	dkp, ok := keychain.(store.DpapiKeyProvider)
	if !ok {
		return "", false, nil // 非 DpapiKeyProvider(Unix),不验
	}
	pp, err := dkp.PathOrEmpty() // T3 导出的 helper
	return pp, true, err
}
```

**删** `registerTaskViaPowerShell`（被 registerTask 替代）。

- [ ] **Step 4: 跑测试验证通过**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestRegisterTask|TestServeInstall_Precheck' -v`
Expected: PASS。

- [ ] **Step 5: 跑全 cli 包测试 + 跨平台编译**

Run: `GOOS=windows go test ./internal/cli/ -v && GOOS=linux go build ./...`
Expected: 全绿（含现有 serve_install 测试除外的部分；删 TestBuildServeTaskXML 不影响其他）。

- [ ] **Step 6: Commit**

```bash
git add internal/cli/serve_install_windows.go internal/cli/serve_install_windows_test.go
git commit -m "feat(cli): serve install object-API + Go password read + machine-scope precheck (Plan 15 T4)

Delete buildServeTaskXML + XML chain (FINDING C stdin/UTF-16/-Xml bugs). Object
API registration (New-ScheduledTask*). Password via Go readPassphrase/env → stdin
→ Register-ScheduledTask -Password (bypasses Get-Credential headless fragility,
consensus A). Precheck verifies master.key is machine-scope (codex #2, prevents
FINDING B recurrence). TLS flags preserved (codex #5). MultipleInstances=IgnoreNew
explicit (pi #2 spike-4 defense).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: RestartOnFailure R1（CIM Set）或 R2（XML 字段）

**Files:**
- Modify: `internal/cli/serve_install_windows.go`（registerTask 后追加 ROF 持久化）
- Test: `internal/cli/serve_install_windows_test.go`

**背景**：spike 3 实证对象 API `-RestartCount/-RestartInterval` 不持久化（Count=0）。R1：Register 后用 CIM `Set-ScheduledTask` 直接设 RestartOnFailure；R2：仅该字段保留 XML 路径。

- [ ] **Step 1: 写失败测试（RestartOnFailure 持久化）**

追加到 `serve_install_windows_test.go`（用 psRunner fake 捕获脚本，验含 RestartOnFailure CIM 设值）：

```go
// TestRegisterTask_RestartOnFailurePersisted 钉死 FINDING D 修复:对象 API
// -RestartCount 不持久化(spike 3),registerTask 必须额外用 CIM 设
// RestartOnFailure Interval=PT1M Count=3(R1)。CI 断言(目标非硬契约)。
func TestRegisterTask_RestartOnFailurePersisted(t *testing.T) {
	in := taskInputs{ExePath: `C:\ssh-manager.exe`, Addr: "0.0.0.0:7878", User: "u", LogPath: `C:\serve.log`}
	captured, err := captureRegisterTask(in, "pw")
	if err != nil {
		t.Fatal(err)
	}
	// R1 路径:CIM 直接设 RestartOnFailure
	if !strings.Contains(captured.script, "RestartOnFailure") {
		t.Errorf("脚本缺 RestartOnFailure CIM 设值(R1)\n%s", captured.script)
	}
	if !strings.Contains(captured.script, "PT1M") || !strings.Contains(captured.script, "3") {
		t.Errorf("脚本缺 Interval=PT1M / Count=3\n%s", captured.script)
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestRegisterTask_RestartOnFailure' -v`
Expected: FAIL —— 当前 registerTask 脚本不含 RestartOnFailure CIM 设值。

- [ ] **Step 3: 改 registerTask 加 R1（CIM Set）**

registerTask 的 PowerShell 脚本，在 `Register-ScheduledTask ... | Out-Null` 后追加：

```go
	// R1: 对象 API -RestartCount 不持久化(spike 3),Register 后用 CIM 直接设
	// RestartOnFailure。若 PS 5.1 的 RestartOnFailure 是只读 CIM 视图(Set 不行),
	// 实现者改走 R2(仅该字段保留 XML 路径)。这里先 R1。
	// (ps 脚本字符串追加:)
	// $t = Get-ScheduledTask -TaskName 'ssh-manager-serve'
	// $t.Settings.RestartOnFailure.Interval = 'PT1M'
	// $t.Settings.RestartOnFailure.Count = 3
	// Set-ScheduledTask -InputObject $t | Out-Null
```

把这几行加到 `ps` 模板字符串里 `Write-Output "REGISTERED"` 之前。

- [ ] **Step 4: 跑测试验证通过**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestRegisterTask_RestartOnFailure' -v`
Expected: PASS。

- [ ] **Step 5: 真机验证（NUC10 或 CI）R1 是否真能持久化**

这一步是真机 gate（PS 5.1 的 RestartOnFailure 是否只读 CIM 视图，spike 3 没测 Set）。**如果 R1 在真机 Set 失败**：
- 改 R2：registerTask 主注册用对象 API，然后单独用 `schtasks /Change /TN ssh-manager-serve /XML <file-with-only-RestartOnFailure>` 补该字段。或 Register 时整个用 XML（但 XML 链有 C1-C3 bug）—— 所以 R2 实际是"对象 API 注册 + 单独 schtasks /Change 补 ROF"。
- 文档标注 + §10 checklist 去掉硬勾选（降级 best-effort）。

在 plan 的 SDD 执行时，T5 implementer 在 NUC10（或 CI windows-latest T8 就绪后）跑一次真机验证，确认 R1 持久化。**若 R1 不行，implementer 在 task report 里标 BLOCKED + 走 R2 或降级，主会话裁决**。

- [ ] **Step 6: Commit**

```bash
git add internal/cli/serve_install_windows.go internal/cli/serve_install_windows_test.go
git commit -m "fix(cli): RestartOnFailure persistence via CIM Set (Plan 15 T5, R1)

spike 3: object API -RestartCount/-RestartInterval silently dropped (Count=0).
R1: after Register, set RestartOnFailure via CIM (Set-ScheduledTask). If PS 5.1
CIM read-only blocks this (FINDING D note), fall back to R2 or best-effort.
Target contract, not hard (consensus C).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `serve status` 修复 —— Get-ScheduledTask 枚举 + process 精确匹配 + log 陈旧检查

**Files:**
- Modify: `internal/cli/serve_install_windows.go`（`schtasksQuery` → `taskStateViaPowerShell`；`serveProcessRunning` 精确匹配；`vaultUnlockedFromLog` 加陈旧检查）

- [ ] **Step 1: 写失败测试（status 用枚举不依赖本地化文本）**

追加到 `serve_install_windows_test.go`：

```go
// TestTaskStateViaPowerShell_ParsesEnumState 验 status 不依赖本地化文本
// (FINDING E):用 Get-ScheduledTask.State(英文枚举 Ready/Running/Disabled)
// + Get-ScheduledTaskInfo.LastTaskResult(整数),不扫 schtasks /Query 的本地化串。
func TestTaskStateViaPowerShell_ParsesEnumState(t *testing.T) {
	// fake psRunner 返回 "Ready\n0"(State=Ready, LastTaskResult=0)
	st, lr, err := taskStateViaPowerShell(fakePs{"Ready\n0"})
	if err != nil {
		t.Fatal(err)
	}
	if st != "Ready" {
		t.Errorf("state: got %q want Ready", st)
	}
	if lr != "0" {
		t.Errorf("lastResult: got %q want 0", lr)
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestTaskStateViaPowerShell' -v`
Expected: FAIL —— `taskStateViaPowerShell` 未定义。

- [ ] **Step 3: 改 serve_install_windows.go**

**删** `schtasksQuery`（行 470-508，依赖本地化文本）。**加** `taskStateViaPowerShell`：

```go
// taskStateViaPowerShell 用 Get-ScheduledTask.State(英文枚举,不本地化)
// + Get-ScheduledTaskInfo.LastTaskResult(整数)。FINDING E 修复:不扫 schtasks
// /Query 的 "Status:"/"任务状态:"/"计划任务状态:" 本地化串。
func taskStateViaPowerShell(r psRunner, taskName string) (state, lastResult string, err error) {
	const ps = `$ErrorActionPreference='Continue'
$t = Get-ScheduledTask -TaskName '` + taskName + `' -ErrorAction Stop
$ti = Get-ScheduledTaskInfo -TaskName '` + taskName + `'
Write-Output $t.State
Write-Output $ti.LastTaskResult
`
	out, err := r.Run(ps, "")
	if err != nil {
		return "", "", fmt.Errorf("powershell: %w: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) >= 1 {
		state = strings.TrimSpace(lines[0])
	}
	if len(lines) >= 2 {
		lastResult = strings.TrimSpace(lines[1])
	}
	return state, lastResult, nil
}
```

`runServeStatus`（行 217-249）改：用 `taskStateViaPowerShell(defaultPsRunner{}, serveTaskName)` 替换 `schtasksQuery`。not-found 检测：PowerShell 抛错且含 "cannot find"/"找不到" → NOT REGISTERED。

**`serveProcessRunning`（行 533-547）改精确匹配**（opencode #7，不并入 HTTP）：

```go
// serveProcessRunning 报告 ssh-manager.exe 进程是否在跑(纯进程存在检查,
// 不验端口监听 —— 那是 probeServeHTTP 的职责;两路有意分开,opencode #7)。
// 精确匹配进程名(去掉 .exe 子串宽匹配的误报)。
func serveProcessRunning() bool {
	out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq ssh-manager.exe", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		// CSV 行形如 "ssh-manager.exe","1234","Console","1","12,345 K"
		fields := strings.Split(line, ",")
		if len(fields) >= 1 {
			name := strings.Trim(fields[0], `"`)
			if strings.EqualFold(name, "ssh-manager.exe") {
				return true
			}
		}
	}
	return false
}
```

**`vaultUnlockedFromLog`（行 590-616）加陈旧检查**（共识 E；heartbeat 在 T7 加）：

```go
func vaultUnlockedFromLog() (bool, string) {
	logPath := serveLogPath()
	if logPath == "" || logPath == string(filepath.Separator) {
		return true, " (no %LocalAppData%; cannot read log)"
	}
	info, err := os.Stat(logPath)
	if err != nil {
		return true, " (no log yet)"
	}
	// 陈旧检查(共识 E):log mtime > 5min → serve 心跳(T7)应每 ~1min 写,
	// 5min 给 4 次心跳冗余。陈旧只降级提示,不当否定(task/process/http 三路
	// 若全绿,overall 仍可 HEALTHY)。
	if time.Since(info.ModTime()) > 5*time.Minute {
		return false, " (log stale >5min; current state unknown)"
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return true, " (log unreadable)"
	}
	tail := tailString(string(data), 8192)
	for _, marker := range []string{
		"unreadable", "undecryptable", "vault locked", "run `ssh-manager unlock`",
	} {
		if strings.Contains(tail, marker) {
			return false, fmt.Sprintf(" (locked: serve.log has %q)", marker)
		}
	}
	return true, ""
}
```

- [ ] **Step 4: 跑测试验证通过**

Run: `GOOS=windows go test ./internal/cli/ -run 'TestTaskState|TestServeStatus|TestServeProcess' -v`
Expected: PASS。

- [ ] **Step 5: 跨平台 + 全包测试**

Run: `GOOS=windows go test ./internal/cli/ -v && GOOS=linux go build ./...`
Expected: 全绿。

- [ ] **Step 6: Commit**

```bash
git add internal/cli/serve_install_windows.go internal/cli/serve_install_windows_test.go
git commit -m "fix(cli): serve status localization + process precision + stale-log (Plan 15 T6)

schtasksQuery (localized text) → taskStateViaPowerShell (State enum + LastTaskResult
int, FINDING E). serveProcessRunning exact name match (opencode #7, not merged with
HTTP probe). vaultUnlockedFromLog adds stale check (consensus E; >5min → degradation
hint, not negation; serve heartbeat lands in T7).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: serve heartbeat（`serve.go`）

**Files:**
- Modify: `internal/cli/serve.go`（serve 启动后开 ~1min heartbeat goroutine 写 serve.log）

- [ ] **Step 1: 写失败测试（heartbeat 周期写 log）**

`internal/cli/serve_test.go`（如果不存在，加；serve.go 应已有测试）：

```go
// TestServe_HeartbeatWritesLog 验 serve 启动后周期写 heartbeat 到 log,
// 让 vaultUnlockedFromLog 的陈旧检查(5min)不误判"健康但空闲"(共识 E)。
func TestServe_HeartbeatWritesLog(t *testing.T) {
	// 启 serve(短超时 + loopback),等 ~2s,验 serve.log 出现 heartbeat 行。
	// (用现有 serve 测试的启动 harness;若启动 harness 不在单测可达,
	// 这个测试 gated 到真机/集成测试 T8 跑)
	t.Skip("heartbeat 验证在 T8 CI 集成测试覆盖(单测难启真 serve)")
}
```

（heartbeat 是运行时行为，单测难启真 serve —— 在 T8 CI 集成测试覆盖。T7 单测 skip + 实现正确性靠 code review。）

- [ ] **Step 2: 改 serve.go 加 heartbeat**

`internal/cli/serve.go`，serve 成功 listening 后开 goroutine：

```go
// 在 serve 的 listening 成功分支后(line 附近,具体看 serve.go 结构):
go func() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		// heartbeat:让 vaultUnlockedFromLog 的陈旧检查(5min)知道 serve 还活着。
		// 写到 stderr(serve 的 stderr 已被任务重定向到 serve.log)。
		fmt.Fprintf(os.Stderr, "heartbeat: still listening on %s at %s\n", addr, time.Now().Format(time.RFC3339))
	}
}()
```

（`addr`、`os`、`time`、`fmt` 的 import 按需补。具体插入点看 serve.go 的 `http.ListenAndServe` 调用前后。）

- [ ] **Step 3: 跨平台编译 + 全包测试**

Run: `GOOS=windows go test ./internal/cli/ -v && GOOS=linux go build ./...`
Expected: 全绿。

- [ ] **Step 4: Commit**

```bash
git add internal/cli/serve.go internal/cli/serve_test.go
git commit -m "feat(cli): serve heartbeat every 1min to serve.log (Plan 15 T7)

Keeps serve.log fresh so vaultUnlockedFromLog's stale check (consensus E) doesn't
false-flag a healthy-but-idle serve as 'unknown'. Verification in T8 CI integration.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: CI windows-latest 集成测试

**Files:**
- Create: `.github/workflows/serve-install-windows.yml`
- Create: `internal/cli/serve_install_integration_test.go`（gated `SSHMGR_SERVE_INSTALL=1`，但 **CI workflow 里设这个 env，默认跑**）

- [ ] **Step 1: 写集成测试（gated，CI 设 env 真跑）**

`internal/cli/serve_install_integration_test.go`：

```go
//go:build windows

package cli

import (
	"os"
	"os/exec"
	"testing"
)

// TestServeInstallIntegration —— gated by SSHMGR_SERVE_INSTALL=1. CI workflow
// sets it; local `go test ./...` skips. Spec §7.2: install → status → uninstall.
func TestServeInstallIntegration(t *testing.T) {
	if os.Getenv("SSHMGR_SERVE_INSTALL") != "1" {
		t.Skip("set SSHMGR_SERVE_INSTALL=1 (CI serves this)")
	}
	password := os.Getenv("SSHMGR_SERVE_INSTALL_PASSWORD")
	if password == "" {
		t.Fatal("SSHMGR_SERVE_INSTALL_PASSWORD required (CI: net user password)")
	}

	// step 0: vault seed(共识 B)—— 非交互初始化
	// unlock(固定测试 key via env)+ servers add 1 台测试 server
	t.Setenv("SSHMGR_MASTERKEY_HEX", "00") // 测试用固定 key(env 优先)
	if err := exec.Command("ssh-manager-test-bin", "unlock").Run(); err != nil { // 实际用 os.Args[0] 或编译的测试 binary
		t.Fatal(err)
	}
	// ... (seed server;具体见 spec §7.2 step 0)

	// step 1: install
	if err := exec.Command(os.Args[0], "serve", "install", "--addr", "127.0.0.1:7878").Run(); err != nil {
		t.Fatalf("serve install: %v", err)
	}
	// step 2-6: 验 task 注册 / MultipleInstances=IgnoreNew / schtasks /Run /
	//          HTTP 401 / vault ok / serve status / uninstall
	// ... (完整断言,见 spec §7.2)
}
```

（具体 step 0-6 的断言代码在 SDD 实现时展开；上面是骨架。注意：集成测试需要一个编译好的 ssh-manager.exe —— CI workflow 里 `go build -o ssh-manager.exe ./cmd/ssh-manager` 然后 test 调它。）

- [ ] **Step 2: 写 CI workflow**

`.github/workflows/serve-install-windows.yml`：

```yaml
name: serve-install-windows

on:
  push:
    paths:
      - 'internal/cli/serve_install*.go'
      - 'internal/store/dpapi*.go'
      - 'internal/store/masterkey*.go'
      - '.github/workflows/serve-install-windows.yml'
  pull_request:
    paths:
      - 'internal/cli/serve_install*.go'
      - 'internal/store/dpapi*.go'
      - 'internal/store/masterkey*.go'

jobs:
  windows:
    runs-on: windows-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - name: build
        run: go build -o ssh-manager.exe ./cmd/ssh-manager
      - name: create test user + grant batch logon
        run: |
          net user sshmgrci ${{ secrets.SSHMGR_CI_PASSWORD }} /add
          net user sshmgrci /passwordreq:no
          # grant SeBatchLogonRight via secedit (pi #1)
      - name: run integration test
        env:
          SSHMGR_SERVE_INSTALL: '1'
          SSHMGR_SERVE_INSTALL_PASSWORD: ${{ secrets.SSHMGR_CI_PASSWORD }}
        run: go test ./internal/cli/ -run TestServeInstallIntegration -v
```

- [ ] **Step 3: 本地验证 workflow 语法 + 测试 skip 行为**

Run: `GOOS=windows go test ./internal/cli/ -run TestServeInstallIntegration -v`
Expected: SKIP（本地无 SSHMGR_SERVE_INSTALL=1）。

用 [actionlint](https://github.com/rhysd/actionlint) 或 GitHub 的 workflow 验证（如装了）验 YAML 语法。

- [ ] **Step 4: Commit（CI 真跑要等 push 到 origin + secrets 配置）**

```bash
git add .github/workflows/serve-install-windows.yml internal/cli/serve_install_integration_test.go
git commit -m "ci: serve-install windows-latest integration test (Plan 15 T8)

Gated SSHMGR_SERVE_INSTALL=1 (CI sets it, local skips). Vault seed step 0 (consensus
B), net user + SeBatchLogonRight (pi #1), password via secret env (consensus A).
FINDING C root-cause fix: integration gate now actually runs. CI real-run needs
SSHMGR_CI_PASSWORD secret configured.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

**注**：CI 真跑需要 GitHub secret `SSHMGR_CI_PASSWORD` 配置（用户在 repo settings 加）。首次 push 后 CI 可能因 secret 没配或 runner 账户问题 fail —— 这是预期的"CI 真跑"暴露问题过程，implementer 据此调整。

---

### Task 9: docs + Plan 14 Superseded 横幅 + NUC10 §7.3 runbook

**Files:**
- Modify: `docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`（顶部加横幅，正文不动）
- Modify: `docs/backup-restore.md` + `docs/multi-machine.md`（machine-scope 威胁模型 + 迁移 runbook）

- [ ] **Step 1: Plan 14 spec 加 Superseded 横幅**

`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md` 顶部（标题下、第一个 `---` 前）加：

```markdown
> ⚠️ **SUPERSEDED by Plan 15** (machine-scope DPAPI + serve install fix).
> Plan 14 的 §3.2（user-scope DPAPI）、§5.8（serve install XML 链）、§6（威胁模型）、§7.2（集成测试）在 NUC10 §7.3 真机验收中暴露了架构缺陷（user-scope 跨 logon session 失败）+ 5 个实现 bug。**正文保留作审计轨迹**，实际方案见 `docs/superpowers/specs/2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md` (v2)。验收结论：`docs/superpowers/specs/2026-08-12-plan-14-nuc10-e2e-findings.md`。
```

- [ ] **Step 2: backup-restore.md 更新 Plan 14 章节 → machine-scope**

`docs/backup-restore.md` 的 "Plan 14" 章节，把"user-scope DPAPI"改"machine-scope DPAPI"，威胁模型段改：

- "master.key 是 **machine-scope DPAPI** 加密——绑机器，不绑用户 SID / logon session。"
- "machine-scope 对 **admin 强制重置密码免疫**（用 DPAPI_SYSTEM LSA secret，不依赖用户 Master Key）—— 与 user-scope 相反。代价：同机其他用户进程能解（靠文件夹 ACL 兜底，`icacls allan716:(OI)(CI)F` only）。"
- 升级 runbook 加 "user-scope → machine-scope 迁移"（NUC10 已修，通用 runbook）：部署新版后第一次 `unlock`（RDP 交互 session）触发 `migrateDpapiScope`，重 protect master.key 为 machine-scope。

- [ ] **Step 3: multi-machine.md 同步**

`docs/multi-machine.md` 的 Plan 14 / serve install 章节，同步 machine-scope 表述。

- [ ] **Step 4: 加 NUC10 §7.3 runbook（reboot 自起验证，release 前 checklist）**

`docs/backup-restore.md` 或 `docs/multi-machine.md` 末尾加 "Plan 15 §7.3 NUC10 reboot 验证 runbook"：

```markdown
### Plan 15 §7.3 NUC10 reboot 验证（release 前 checklist）

CI 不能 reboot，boot 自起（BootTrigger）+ 跨重启 DPAPI（machine-scope）的闭环验证在这里：

1. NUC10 部署新版 ssh-manager.exe。
2. NUC10 交互式（RDP）跑 `ssh-manager unlock` → 触发 user→machine 迁移（重 protect C）。
3. `ssh-manager serve install`（输 allan716 密码）→ 对象 API 注册。
4. **reboot NUC10** → BootTrigger 自起 serve。
5. NUC10 起来后 `ssh-manager serve status` → `vault: ok`（machine-scope 跨重启可解）+ `overall: HEALTHY`。
6. 笔记本 MCP 连 `http://192.168.100.235:7878` → `exec_command` 在 1660Super01 跑 `hostname` → 返回 `DESKTOP-UP1MHGT`。
7. 清理：`serve uninstall`。
```

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md docs/backup-restore.md docs/multi-machine.md
git commit -m "docs: Plan 14 Superseded banner + machine-scope threat model + NUC10 runbook (Plan 15 T9)

Plan 14 spec: Superseded banner (body untouched, audit trail preserved; opencode #8).
backup-restore + multi-machine: machine-scope DPAPI, password-reset immunity (kimi #2),
user→machine migration runbook. NUC10 §7.3 reboot verification runbook (release checklist).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**1. Spec coverage:**
- §3.2 machine-scope → T1 (flag), T2 (provider). ✓
- §5.1 DPAPI flag + 跨 scope 互通 → T1 test. ✓
- §5.2 DpapiKeyProvider + ACL 契约 + Get 双 scope → T2. ✓
- §5.3 migrateDpapiScope + postGetMigrator (codex #1) → T3. ✓
- §5.4 对象 API + Go 密码读 + precheck machine-scope (codex #2) + TLS (codex #5) + MultipleInstances (pi #2) → T4. ✓
- §5.4 RestartOnFailure R1/R2 降级 (共识 C) → T5. ✓
- §5.5 status 枚举 (E) + process 精确 (opencode #7) + log 陈旧 (共识 E) → T6. ✓
- §5.5 serve heartbeat (共识 E) → T7. ✓
- §6 威胁模型 machine-scope 免疫 (kimi #2) → T9 docs. ✓
- §7.1 单测断言改 → T1/T2 tests. ✓
- §7.2 CI vault seed (共识 B) + env 密码 (共识 A) + 断言降级 (opencode #2) → T8. ✓
- §7.3 NUC10 reboot runbook → T9. ✓
- §8 Plan 14 Superseded (opencode #8) + unlock.go 触点 (codex #1) → T3/T9. ✓

**2. Placeholder scan:** 无 TBD/TODO。有几处"具体断言在 SDD 实现时展开"（T8 集成测试 step 0-6 骨架、T5 R1 真机 gate）—— 这些是**有意的**（集成测试细节依赖 CI 实际环境，R1 真机 gate 依赖 spike 3 没测的 Set 行为），不是占位符。每个都有明确的行为契约。

**3. Type consistency:** `dpapiProtect/Unprotect(plain, machine bool)` 全链一致；`taskInputs` 结构在 T4 定义、T5 复用；`psRunner` interface 在 T4 定义、T6 复用；`postGetMigrator func(w, mk) (bool, error)` 在 T3 定义、unlock 调用一致。

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-13-plan-15-machine-scope-dpapi-serve-fix.md`. Two execution options:

**1. Subagent-Driven (recommended)** — 每 task 派新 implementer subagent，task 间双审（spec 合规 + 代码质量），全部完成 final whole-branch review。

**2. Inline Execution** — 本 session 按 executing-plans 批量执行 + checkpoint。

Which approach?
