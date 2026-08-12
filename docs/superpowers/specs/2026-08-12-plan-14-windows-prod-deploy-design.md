# Plan 14 — Windows 生产部署（DPAPI master key + serve 常驻）— Design Spec

**Date:** 2026-08-12
**Status:** Design v2 — 三家 xcheck 评审（codex/opencode/pi）+ DPAPI spike 实证（推翻共识 A）+ 评审必改项（B/C/D/E + 应改）落地；pending implementation plan
**Worktree/branch:** `plan-14-windows-prod-deploy`（待开）

> 把 ssh-manager-mcp 从"测试里跑通"推进到"NUC10 真机生产部署"。修两个 E2E 暴露的生产阻断：
> ① Windows Credential Manager 在 sshd/Service session 报 `ERROR_NO_SUCH_LOGON_SESSION (1312)` → master key 存不进 keychain；
> ② serve 无法 sshd 后台常驻。方案：DPAPI + 文件存 master key + Task Scheduler 让 serve 以用户账户常驻。
> **成功标准**：NUC10 `serve install` → 重启 NUC10 → serve 自起 → 笔记本 agent 连上验证。

## 1. 问题（E2E 2026-08-12 实测暴露，spec §13 参考）

E2E 实测验证了 Plan 10–13 架构核心成立（serve 鉴权、MCP 握手、Plan 12 disjoint-auth 基石、跨机迁移、cache pull、P13 backup/verify），但暴露**两个生产阻断**：

### 1.1 FINDING 9 — Windows Credential Manager 在 sshd/Service session 失效

**现象**：NUC10 经 ssh 跑 `ssh-manager unlock` → `keychain.Get()` 报 `ERROR_NO_SUCH_LOGON_SESSION (1312)` ("A specified logon session does not exist")；本机 RDP 交互式 session 跑 wincred 直调正常。**已用最小 wincred 复现锁定**。

**根因**：Windows Credential Manager（CredRead/CredWrite，wincred 库）操作 CurrentUser 凭据要求进程在**交互式 logon session** 里。sshd 的 network logon、Windows Service 的 session、非交互 Task 都不建立这种 logon session → `1312`。这**不是 go-keyring bug、不是 wincred bug**，是 Credential Manager 的 session 模型与无人值守部署模型根本不兼容。

**影响**：master key 存不进 keychain → serve/Service 拿不到 master key → 整个 Plan 10–13 的生产部署路径不通。env master key（`SSHMGR_MASTERKEY_HEX`）只是测试/脚本 workaround（重启 shell 丢失、服务化注入脆弱）。

### 1.2 FINDING 10 — serve 无法 sshd 会话后台常驻

**现象**：`Start-Process` / `start /B` / PowerShell Job 起的 serve 都随 ssh 退出被杀；前台 ssh 同步跑 serve 能成（HTTP 401/200）但断 ssh 即死。

**根因**：Windows sshd 会话退出时清理所有子进程；Linux 的 nohup/disown 在 Windows 无对应。生产 serve 必须靠 OS 级常驻机制（Windows Service / Task Scheduler at-startup）。

**影响**：serve 无法 7×24 跑 → 多机架构不可用。

## 2. 目标

1. master key 在 Windows 上**持久化、跨重启、serve 能拿、owner 能拿**——不依赖交互式 logon session。
2. serve 在 Windows 上**后台常驻、重启自起**——不依赖保持 ssh 会话。
3. v0.2.0 → 新版的**迁移路径**（旧 keychain slot 读得出就迁）。
4. export/import 支持**非交互口令**（`--passphrase-file`）→ 恢复流程可脚本化。
5. **成功标准**：NUC10 `serve install` → 重启 → serve 自起 → 笔记本 agent MCP 连上 → `exec_command` 在真实目标机执行。

## 3. 关键决策（brainstorm + 用户拍板）

### 3.1 serve 以**用户账户**（`allan716`）跑，非 LocalSystem

**根决策**。最初选 LocalSystem（"Windows Service 默认账户"的技术惯性），但理清后发现真实需求是"serve 常驻"，不是"必须 LocalSystem"。

**为什么用户账户**：
- owner（`allan716` shell 跑 `servers add`）和 serve（`allan716` 账户）**同一账户域** → 都能解 master key（user-scope DPAPI 绑用户 SID）。
- LocalSystem 是不同账户域 → 要么 machine-scope DPAPI（同机任何进程能解，弱化）要么 user-scope under LocalSystem（owner 解不了，owner 日常 vault 管理断裂）。
- 贴合 v0.2.0 使用习惯（v0.2.0 的 mcp 进程就是 allan716 用户会话起的）。

**后果**：vault (store.db) + master key 都在 `allan716` 用户 profile（`%AppData%\ssh-manager\`，已是默认路径）。**不是机器级**（推翻早期"机器级"想法）。

### 3.2 master key = **user-scope DPAPI under allan716** + 文件

- 文件：`%AppData%\ssh-manager\master.key`
- 内容：`CryptProtectData`（**不加** `CRYPTPROTECT_LOCAL_MACHINE`）加密的 32 字节 mk
- user-scope DPAPI 绑 `allan716` 的 SID → **只有 `allan716`（owner 或 serve）能解**，同机其他用户进程解不开
- ACL：显式 `icacls`（见 §5.2；0600 在 Windows 是 no-op，不靠它）

**user-scope vs machine-scope 决策理由**（v2 评审 opencode #4 + spike 实证硬化）：
- **spike 实测**（附录 §12）：user-scope DPAPI 在 **RDP / sshd / Task Scheduler 三种 session** 都正常 Protect/Unprotect，且**跨 session 能解**（sshd protect → Task Scheduler unprotect = ok）。user-scope 在生产场景完全可用，machine-scope 的"省事"优势（不需 Task Scheduler 存密码、扛 admin 重置）在实测下消失。
- user-scope 安全性严格更强（对同机其他用户保密），且实测可用 → **选 user-scope**。
- machine-scope（`CRYPTPROTECT_LOCAL_MACHINE`）作为 §9 未来选项（多用户共享 vault 场景），本 plan 不用。

**安全模型（诚实，v2 修密码事实 opencode #2 + pi #2）**：
- ✓ master key 对同机其他用户保密（user-scope DPAPI 绑账户 SID）
- ✓ master key 对 agent 保密（agent 进程无 mk，走 broker）
- ⚠ master key 对 `allan716` 跑的**任意进程**不保密（任何 `allan716` 进程能 `CryptUnprotectData` 解 master.key）—— 这与 v0.2.0 keychain 等级相同（keychain 也对同用户进程不设防），**不是 regression**
- ⚠ **只有管理员强制重置密码**（admin reset，不知旧密码、无法 re-wrap DPAPI Master Key）才让 master.key 解不开。**用户自行改密码不会断**（Windows 自动用旧密码 re-wrap DPAPI Master Key，已有密文仍可解）—— 旧版 spec 写"改密码就断"是事实错误，已更正。运维只需在 admin 重置密码前迁移，日常改密码无影响。

### 3.3 Linux/macOS：保持 keychain + 加 FileKeyProvider fallback（不写迁移）

- Linux secret-service / macOS keychain（go-keyring）**在 daemon session 理论上无 Windows 那种 logon-session 问题**——本次不验证（留后续），保持 `KeyringKeyProvider` 不变。
- 加 `FileKeyProvider`（0600 明文文件，全平台）作为**无 keychain 环境 fallback**（CI / 容器 / 无 secret-service 的 headless Linux）。Windows 不用它（DPAPI 优先）。
- **不写 Linux/macOS 的 v0.2.0 迁移**（v0.2.0 的 keychain slot 迁移逻辑在所有平台一致，但实测只在 Windows 跑）。

### 3.4 常驻机制用 **Task Scheduler**（Windows only，v2 scope 收窄）

- **v2 scope 收窄**（评审 codex #9）：§4 已说"不验证 Linux/macOS"，那 `serve install` 也**只实现 Windows Task Scheduler**。Linux systemd / macOS launchd 的 serve install **defer 到专门 plan**（避免未测试代码 + 各有平台陷阱：linger 权限、D-Bus session、LaunchAgent 仅 GUI login 后启动）。本 plan 的 Linux/macOS master key 存储（§3.3）保持，但 serve install 不做。
- Windows：Task Scheduler `at-startup`（schtasks，以 `allan716` 身份）。
- **崩溃恢复**（评审 codex #4）：Task Scheduler 配 `RestartOnFailure`（Interval=1min, Count=3）。
- `serve install/uninstall/status` 子命令生成 schtasks 配置 + 注册。
- **理由**：无新 Go 依赖（kardianos/service 要 go get）；贴合 P13 文档已写的 schtasks 模式。
- ⚠ **kardianos/service 是备选**（若 Task Scheduler 实测可靠性不可接受，切到 kardianos/service——它 Windows 也支持配置用户账户）。

### 3.5 DPAPI **自己 syscall**（不用第三方库，spike 已实证可行）

- DPAPI 调用走 `crypt32.dll` 的 `CryptProtectData`/`CryptUnprotectData`（标准库 `syscall.NewLazyDLL`，不用 `golang.org/x/sys/windows`——spike 程序用纯 stdlib syscall 跑通）。
- ~30 行，含 `DATA_BLOB` 结构 + **`LocalFree` 释放输出 blob**（评审 codex #6 + pi #5：输出 blob 由 DPAPI LocalAlloc 分配，调用方必须 LocalFree，否则每次调用泄漏内存/句柄）。
- spike 程序（附录 §12）已实证这条路径在三 session × 两 scope 全跑通，含 LocalFree。
- **理由**：无新依赖（项目铁律：依赖最小）；第三方库（`AdRoll/go-dpapi` 等）都只是这层的薄包装。

## 4. 非目标（v1 不做）

- **不验证 Linux/macOS daemon session 的 keychain 行为**（§3.3，留后续）。
- **不做机器级 vault**（§3.1，用户级）。
- **不重新设计 master key 生命周期**（保持 unlock 生成 + 持久化的现有模型，只换存储介质）。
- **不改 serve 的 HTTP/MCP 协议**（Plan 10 的 serve 不变，只加常驻外壳）。
- **不改 broker 工具集**（Plan 6 的 6 工具不变）。
- **不修 serverInfo.version 硬编码**（Plan 9 land-later，与部署无关，留单独修）。
- **不修 `mcp` 无 serve 客户端模式**（FINDING 11，serve 用户侧用 MCP 客户端连 HTTP 是设计如此，不改）。

**范围说明**：Plan 12 的 **cache DEK** 同样用 `KeyringKeyProvider`（keychain slot `cache-dek`），FINDING 9 同样影响它（Windows sshd/Service session 读不出）→ **cache DEK 的 KeyProvider 也走 §5.5 的 build-tag 分流**（Windows DPAPI-file、Unix keychain），**在本 plan 范围内**（同一根因、同一修复模式，割裂反而出错）。具体：cache DEK 复用 `DpapiKeyProvider` 但 `Path` 不同（`master.key` vs `cache-dek.key`），保持 master key 与 cache DEK 文件分离。

## 5. 设计

### 5.1 组件总览

| 组件 | 文件 | 平台 | 作用 |
|---|---|---|---|
| `DpapiKeyProvider` | `internal/store/masterkey_windows.go`（新） | Windows | DPAPI user-scope + 文件 |
| `FileKeyProvider` | `internal/store/masterkey_file.go`（新） | 全平台 | 0600 明文 fallback |
| DPAPI syscall | `internal/store/dpapi_windows.go`（新） | Windows | CryptProtectData/UnprotectData |
| keychain seam 分流 | `internal/cli/keychain_windows.go` / `_unix.go`（新） | 全平台 | 编译期绑 KeyProvider |
| v0.2.0 迁移 | `internal/cli/unlock.go`（改）+ `internal/store/masterkey.go`（改） | 全平台 | 检测旧 slot → 迁新存储 |
| `serve install/uninstall/status` | `internal/cli/serve_install_*.go`（新，build-tag）+ `internal/cli/serve.go`（改） | 全平台 | Task Scheduler / systemd / launchd 注册 |
| export/import `--passphrase-file` | `internal/cli/export.go` / `import.go`（改） | 全平台 | 非交互口令 |

### 5.2 `DpapiKeyProvider`（Windows 主路径）

**接口**：实现已有 `store.KeyProvider`（`Get() ([]byte, error)` / `Set([]byte) error` / `Delete() error`）。

**文件路径**：`%AppData%\ssh-manager\master.key`（用户 profile，与 store.db 同目录）。

**Get**：
```
blob = os.ReadFile(path)
if not exist → return ErrNotFound
mk, err = CryptUnprotectData(blob)   // user-scope，绑当前用户 SID
if err != nil → return err   // ⚠ 解密失败硬失败（见 §5.6，不静默降级到 FileProvider）
return mk
```
**Set**（v2 修：原子写 + ACL 显式，评审 opencode #3 + codex #2 + pi #11）：
```
blob = CryptProtectData(mk)          // user-scope
ensureDir(%AppData%\ssh-manager\)    // 见下方 ACL（建目录时一次性设，非每次 Set）
tmp = path + ".tmp.<rand>"
os.WriteFile(tmp, blob)              // 写 temp（不加 0600——Windows 忽略 mode，靠文件夹 ACL 继承）
icacls(tmp) → 仅 allan716            // 文件单独设 ACL（继承可能不够；ACE: ContainerInherit|ObjectInherit）
os.Rename(tmp, path)                 // 原子替换（同卷 rename 原子）—— 半截崩溃不 corrupt master.key
```
**Delete**：`os.Remove(path)`（忽略 not-found）。

**ACL**（v2 修：显式 + 顺序 + inherit flag，评审 codex #2 + opencode #7）：
- 文件夹 `C:\Users\allan716\AppData\Roaming\ssh-manager\`（在用户 profile 下，自 Vista 起默认继承就是 user-only，但显式设以确定）。
- **建目录时设一次**（非每次 Set 重设——评审 pi #6）：`icacls <dir> /inheritance:r /grant:r "<user>:(OI)(CI)F"`（`/inheritance:r` 关继承、`(OI)(CI)` 容器+对象继承、`F` 完全控制）。
- master.key 文件本身：靠文件夹的 `(OI)(CI)` 继承；若实测继承不到位，install 时对文件单独 `icacls`。
- **不靠 `os.WriteFile(0600)`**——Go 在 Windows 忽略 mode 位（评审三家共识 D），ACL 必须显式 `icacls` 或 Windows Security API（`SetFileSecurity` + SDDL）。
- 文件夹 ACL 操作用 `icacls` 子进程（实现简单）或 Go 调 `SetFileSecurity`（无子进程；纯 Go SDDL 略重，`icacls` 更省）—— plan 时定，倾向 `icacls`。

### 5.3 DPAPI syscall（`dpapi_windows.go`，spike 已实证）

```go
//go:build windows

package store

import "syscall"   // 纯 stdlib，不用 golang.org/x/sys/windows（spike 验证可行）

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree          = kernel32.NewProc("LocalFree")   // ⚠ 必须：释放输出 blob
)
```
（实现细节，spike 程序已跑通此路径：`Proc.Call` + `DATA_BLOB` 指针参数 + **`defer LocalFree(out.pbData)` 释放输出 blob**（评审 codex #6 + pi #5：CryptProtectData/UnprotectData 的输出 pbData 由 LocalAlloc 分配，必须 LocalFree，否则泄漏）。byte slice ↔ DATA_BLOB 转换：`(*[1<<30]byte)(unsafe.Pointer(out.pbData))[:out.cbData]`。~40 行含 LocalFree。user-scope = 不传 `CRYPTPROTECT_LOCAL_MACHINE(0x1)` flag。）

### 5.4 `FileKeyProvider`（全平台 fallback）

```go
// 0600 文件存 mk 明文（无加密）。仅给无 keychain/无 DPAPI 环境（CI/容器/headless）。
// Windows 主路径用 DpapiKeyProvider（DPAPI 优先），FileKeyProvider 是 env unset + keychain 不可用时的兜底。
type FileKeyProvider struct { Path string }  // 空则 UserConfigDir/ssh-manager/master.key.plain
```
Get/Set/Delete 同 DpapiKeyProvider 但**不加密**。**文档明确**：明文文件安全性弱于 DPAPI/keychain，只用于受控环境。

### 5.5 keychain seam 分流（build-tag）

`internal/cli/keychain_windows.go`：
```go
//go:build windows
package cli
import "ssh-manager-mcp/internal/store"
var keychain store.KeyProvider = store.DpapiKeyProvider{}
```
`internal/cli/keychain_unix.go`：
```go
//go:build !windows
var keychain store.KeyProvider = store.KeyringKeyProvider{}
```
（替换现有 `unlock.go:14` 的 `var keychain store.KeyProvider = store.KeyringKeyProvider{}`。）

### 5.6 `resolveMasterKey` 顺序（`vault/vault.go`，改；v2 修错误分支 codex #8 + pi #8）

```
1. SSHMGR_MASTERKEY_HEX env（dev/脚本）→ hex decode 返回
2. 平台 KeyProvider（keychain seam）Get：
   - Windows: DpapiKeyProvider
   - Unix: KeyringKeyProvider
   成功 → 返回
   ErrNotFound → 继续下一步 3
   ⚠ 其它错误（DPAPI 解密失败 / keychain 服务不可用）→ **硬失败，报清晰错误**（不 fall-through）
3. FileKeyProvider Get（fallback）：
   成功 → 返回
   ErrNotFound → "vault locked: run `ssh-manager unlock`"
```

**错误分支语义**（v2 明确，评审 codex #8 + pi #8）：
- 平台 KeyProvider 返回 `ErrNotFound`（master key 未初始化）→ 继续 FileProvider fallback（合法首次运行/无 keychain 环境）。
- 平台 KeyProvider 返回**其它错误**（DPAPI 解密失败 = master.key corrupt / 密码被 admin 重置 / session 异常；Linux secret-service 连接错误）→ **硬失败 + 清晰报错**（如 "master key present but undecryptable: <err>; if admin-reset password, restore from backup or re-init vault"）。**绝不静默 fall-through 到 FileProvider**（明文回退 = 安全降级，必须显式，不能因解密失败悄悄走明文）。
- FileProvider 只覆盖"无 keychain/无 DPAPI"环境（CI/容器），不覆盖"解密失败"——解密失败是安全事件，不是 fallback 场景。

### 5.7 v0.2.0 迁移（Windows only；`unlock` 首次运行）

**仅 Windows**：v0.2.0 在所有平台都用 `KeyringKeyProvider`（keychain）存 master key，但只有 **Windows** 上 keychain 在 sshd/Service session 读不出（FINDING 9 的 1312）→ Windows 迁移有真实价值（把能读出的旧 slot 迁到 DPAPI 文件）。Linux/macOS 的 v0.2.0 keychain slot 若存在，新版本仍用 `KeyringKeyProvider`（§3.3 保持不变，新旧存储介质相同）→ **无需迁移**。

`unlock`（Windows）当 `DpapiKeyProvider.Get()` 返回 `ErrNotFound`（DPAPI 文件不存在）时，**首次运行逻辑**：
1. 检测旧 keychain slot：`KeyringKeyProvider{Service:"ssh-manager",User:"master-key"}.Get()`。
2. 读出成功（交互式 session，wincred 正常）→ 提示"检测到 v0.2.0 master key，迁移到 DPAPI 文件？[y/N]" → `DpapiKeyProvider.Set(mk)` + `KeyringKeyProvider.Delete()`。
3. 读出失败但**非 ErrNotFound**（sshd session 报 `1312`）→ 捕获，提示：
   > "检测到可能的 v0.2.0 keychain master key 但当前会话读不出（sshd/非交互 session 的 Windows Credential Manager 限制）。请在**交互式会话**（本地终端/RDP）重跑 `unlock` 迁移，或重设 vault（见 docs/backup-restore.md）。"
4. 旧 slot `ErrNotFound`（干净环境）→ 正常 first-run generate + `DpapiKeyProvider.Set`。

**UX 边界**：迁移**必须**在交互式 session（owner 本地终端）跑；serve/Service 上下文不跑迁移（serve 启动时 mk 不存在 = 报"locked"，不自动迁移）。

**cache DEK 迁移**（v2 补，评审 pi #4）：Plan 12 的 cache DEK 也存 keychain slot `cache-dek`（同样受 FINDING 9 影响）。迁移逻辑平行于 master key：unlock 检测旧 `cache-dek` slot → 读出 → 写 `DpapiKeyProvider{Path:"cache-dek.key"}`（与 master.key 同目录但不同文件，保持分离）+ 删旧 slot。session 约束同上（交互式 session）。

**升级 Runbook**（v2 补，评审 pi #3 + codex #1）：已有 v0.2.0 vault 的机器升级流程**必须**：
1. 停旧 ssh-manager 进程（v0.2.0 mcp 等，见 E2E FINDING 5）。
2. **在交互式 session（本地终端/RDP，非 ssh）跑 `ssh-manager unlock`** → 触发迁移（旧 keychain slot → DPAPI 文件）。
3. 迁移成功后，`serve install` / SSH CLI 管理才能用新 master key。
4. **不跑步骤 2 直接 `serve install`** → serve 读不到 master key（旧 slot 在非交互 session 读不出）→ 启动失败。成功标准（§7.3）必须显式包含此步骤。

### 5.8 `serve install/uninstall/status`（v2：Windows only + auto-restart + 日志 + 密码安全 + status vault 检查）

**Windows**（`serve_install_windows.go`）—— Task Scheduler：
- `serve install [--addr 0.0.0.0:7878]`：
  1. 确认 master.key 存在（DpapiKeyProvider.Get 不报 ErrNotFound）；不存在 → 提示先 `unlock`（交互式）。
  2. 生成 schtasks XML：at-startup 触发 + **`RestartOnFailure`（Interval=PT1M, Count=3）**（评审 codex #4 崩溃恢复）+ 以 `allan716` 身份。**不用 RunLevel Highest**（评审 opencode #6 不必要提权，filtered token 足够读 profile + 监听端口）。
  3. **日志重定向**（评审 opencode #5）：XML 里 `<Exec>` 命令包一层 cmd `serve ... > serve.log 2>&1`，或用 Task Scheduler 的 stdout/stderr 重定向到 `%LocalAppData%\ssh-manager\serve.log`（保证 headless 失败可诊断）。
  4. **密码安全**（评审 codex #3 + pi #10 + opencode #9）：避免 `schtasks /Create /RP <密码>`（密码明文进命令行 + 审计日志 4688）。优先用 **Task Scheduler COM API**（`Schedule.Service` ProgID → `RegisterTask` + `Definition.LogonType=TaskLogonType.Password` + `SetPassword`，密码不经命令行）；COM 实现 ~50 行 Go（`ole` 调用）或用 PowerShell `Register-ScheduledTask -Password`（仍有进程列表风险但优于 /RP）。若 COM 太重，fallback 到 `/RP` + **文档明确标注风险**（密码进 4688 审计日志）+ 单用户本地账户禁用密码过期（opencode #10）。**"自动登录 + At logon" 不作推荐**（pi #10：把密码写 `HKLM\Winlogon\DefaultPassword` 注册表，比 Task Scheduler 的 LSA 存储更弱）。
  5. 立即 `schtasks /Run` 启动一次（验证 + 日志生成）。
- `serve uninstall`：`schtasks /Delete /TN ssh-manager-serve` + 确认 serve 进程停。
- `serve status`（v2 补，评审 codex #7 区分进程活/vault 可用）：
  - `schtasks /Query` 任务状态（Running/Ready/Failed + Last Result 码）
  - 检测 serve 进程在跑
  - curl localhost:7878（鉴权 401 = HTTP 活）
  - **vault-locked 检查**：读日志/health 端点确认 serve 启动时 master key 解密成功（进程活 + HTTP 响应 ≠ vault 已解锁）。serve 启动失败写日志 `master key undecryptable` → status 报"vault locked"。

**Linux/macOS**（v2 砍，评审 codex #9）：serve install **本 plan 不实现**（defer 到专门 plan）。Linux systemd --user / macOS launchd 各有平台陷阱（linger 权限 codex #5、D-Bus session、LaunchAgent 仅 GUI login 后启动非 boot pi #9），且 §4 不验证 Linux/macOS。本 plan 的 Linux/macOS master key 存储（§3.3）保持，但 serve 常驻留后续。

**`serve`（前台跑）保留不变**——install 只是把 serve 包成常驻。

### 5.9 `export`/`import` `--passphrase-file`

- `export --passphrase-file <path>`：从文件读口令（替代 `passphrasePrompt`）；保留 `passphraseConfirmPrompt` 或跳过 confirm（flag 模式不 confirm）。
- `import --passphrase-file <path>`：加密分支从文件读口令；明文分支（T3 嗅探）不受影响。
- 交互 prompt 仍是默认（无 flag 时）。

### 5.10 文件布局（Windows 部署后）

```
C:\Users\allan716\AppData\Roaming\ssh-manager\
  ├── store.db                          # vault（已有，用户级）
  ├── master.key                        # DPAPI user-scope 密文（新）
  └── (cache.bin 等 Plan 12 产物)
Task Scheduler: ssh-manager-serve        # at-startup, allan716 身份
```

## 6. 安全考虑（写进文档，诚实；v2 修密码事实 + master.key≠备份）

- **DPAPI user-scope 的边界**：master key 对 `allan716` 的任意进程不保密（同 keychain 等级）；对同机其他用户、对 agent 保密。**不是 regression**。
- **密码变更**（v2 修事实，§3.2）：**只有管理员强制重置密码**才让 master.key 解不开；**用户自行改密码 DPAPI 自动 re-wrap，无影响**。Runbook 只在 admin 重置前迁移。
- **`master.key` ≠ 备份，不可移植**（v2 补，评审 pi #7）：master.key 被 user-scope DPAPI 绑死本机 profile + 用户 SID——换机/重装/admin 重置密码后即废物。**唯一可移植的灾备恢复手段是 passphrase export 信封**（Plan 11）。Runbook 必须点破：灾备 = 在新机 `ssh-manager import <file> --passphrase-file <p>`（从 NAS 的 P13 明文备份或 export 加密文件恢复）。master.key 只是"本机 serve/owner 日常解锁"的缓存，不是备份。
- **Task Scheduler 密码**（v2，§5.8）：优先 COM API 避免命令行暴露；fallback `/RP` + 文档标注风险（进 4688 审计日志）+ 禁用密码过期。"自动登录 + At logon" 不推荐（注册表存密码更弱）。
- **master.key 文件 ACL**：建目录时一次性 `icacls /inheritance:r /grant "<user>:(OI)(CI)F"`（§5.2），不靠 0600。
- **恢复流程脚本化**：`--passphrase-file` 让 import 无人值守——passphrase 文件本身受控（0600 + 不进 git + 恢复后删）。
- **威胁模型更新**：L2 模型不变（agent 不触凭据）；新增"master key 持久化文件"是信任根，物理/同机进程访问控制是其防线。

## 7. 测试

### 7.1 单元测试
- **`dpapi_windows_test.go`**（build windows）：`Protect→Unprotect` round-trip（随机 32 字节）；空输入；大输入；确认 user-scope（不同用户解不开——但测试在同一 user 跑，用 mock 验证 scope flag 正确）。
- **`masterkey_windows_test.go`**：`DpapiKeyProvider` Set→Get→Delete round-trip；文件 ACL（0600 + 文件夹仅 allan716）；不存在 → ErrNotFound。
- **`masterkey_file_test.go`**：`FileKeyProvider` round-trip；0600。
- **迁移测试**（`unlock_test.go`）：构造旧 keychain slot → unlock 触发 → 新存储生成 + 旧 slot 删；旧 slot 读不出 → 正确提示（不崩）。
- **`resolveMasterKey` 测试**：env 优先 → 平台 KeyProvider → FileProvider fallback 顺序。

### 7.2 集成测试
- **`serve_install_windows_test.go`**（build windows，可选 `SSHMGR_SERVE_INSTALL=1` gate）：install → status 显示注册 → uninstall 清干净（不残留 Task）。
- **export/import `--passphrase-file`**：round-trip 不弹 TTY。

### 7.3 端到端（真机，成功标准）
**`SSHMGR_E2E_DEPLOY=1` gate**（默认跳过，需真机）：
1. NUC10 部署新版 ssh-manager.exe。
2. NUC10 交互式（RDP/本地）跑 `ssh-manager unlock`（first-run 生成 master.key DPAPI）。
3. NUC10 跑 `ssh-manager serve install`（输 allan716 密码）→ Task Scheduler 注册。
4. **重启 NUC10**（人工触发，因影响其他服务）。
5. NUC10 起来后：`ssh-manager serve status` 显示 running；`curl localhost:7878` → 401（鉴权工作）。
6. 笔记本：发 project token（NUC10 上 `projects add`）→ MCP 客户端连 `http://192.168.100.235:7878` → `tools/call exec_command` 在某台真实 server（如 1660Super01）跑 `hostname` → 返回 `DESKTOP-UP1MHGT`。
7. 清理：`serve uninstall`。

### 7.4 No-regression
- `go test ./...` green（含跨平台 build-tag 测，Windows + Linux 都跑 gofmt/vet）。
- `gofmt -l .` 干净（master 已清 P13 那批）。
- `go vet ./...` clean。
- **跨平台编译**：`GOOS=windows/linux/darwin go build ./...` 都过。

## 8. 实现触点（file-by-file；v2 砍 Linux/macOS serve_install）

| 文件 | 改动 |
|---|---|
| `internal/store/dpapi_windows.go`（新）| CryptProtectData/UnprotectData syscall（纯 stdlib `syscall.NewLazyDLL`，`//go:build windows`）+ LocalFree |
| `internal/store/masterkey_windows.go`（新）| `DpapiKeyProvider`（DPAPI user-scope + 文件，原子写 temp+rename，icacls ACL）|
| `internal/store/masterkey_file.go`（新）| `FileKeyProvider`（明文 fallback，全平台；Windows 不走主路径）|
| `internal/store/masterkey.go`（改）| 迁移 helper（检测旧 keychain slot，master key + cache DEK 两条）|
| `internal/store/*_test.go`（新/改）| DPAPI/File/迁移测试 |
| `internal/cli/keychain_windows.go`（新）| `var keychain = DpapiKeyProvider{}` |
| `internal/cli/keychain_unix.go`（新）| `var keychain = KeyringKeyProvider{}` |
| `internal/cli/unlock.go`（改）| 删 line 14 的 keychain init（移到 build-tag 文件）；加 master key + cache DEK 迁移逻辑 |
| `internal/vault/vault.go`（改）| `resolveMasterKey` 三级 + **错误分支硬失败语义**（解密失败不降级明文）|
| `internal/cli/serve.go`（改）| 加 `install`/`uninstall`/`status` 子命令入口（Windows 实现）|
| `internal/cli/serve_install_windows.go`（新）| Task Scheduler schtasks XML（at-startup + RestartOnFailure + 日志重定向）+ COM API 注册（避免 /RP 命令行暴露密码）|
| ~~`serve_install_linux.go`~~ / ~~`serve_install_darwin.go`~~ | **v2 砍**（defer 专门 plan，§3.4）|
| `internal/cli/export.go` / `import.go`（改）| `--passphrase-file` flag |
| `docs/backup-restore.md`（改）| 升级 Runbook（停旧进程 → 交互式 unlock 迁移 → serve install）+ DPAPI/master.key 说明（master.key ≠ 备份）+ admin 重置密码迁移 |
| `docs/multi-machine.md`（改）| serve 部署章节改 Windows Task Scheduler at-startup（配合 Plan 13 UNC 路径模式）|

## 9. 未来工作（显式 deferred）

- **Linux/macOS daemon session keychain 验证**（§3.3，本次不验证）。
- **Linux/macOS serve install**（systemd --user / launchd LaunchAgent）——v2 从本 plan 砍出（§3.4），defer 到专门 plan（含 linger 权限、D-Bus session、LaunchAgent vs LaunchDaemon pi #9 的平台细节）。
- **kardianos/service 替换 Task Scheduler**（若 review/实测发现 Task Scheduler 密码存储/可靠性问题，尽管 COM API 路径已规避密码暴露）。
- **serverInfo.version 注入**（Plan 9 land-later，独立修）。
- **machine-scope DPAPI 选项**（若未来要支持多用户共享 vault；v2 spike 实测 user-scope 三 session 全通，machine-scope 无优势，仅多用户场景才需）。
- **serve 的 admin endpoint**（远程 owner 管理 vault，本次 owner 仍 ssh 到 serve 机本地管理）。

## 10. 落地前 checklist（v2 同步评审项）

- [ ] DPAPI user-scope round-trip + LocalFree（Windows 单测，spike §12 已实证）
- [ ] DpapiKeyProvider Set（**原子写 temp+rename**）/ Get / Delete + **icacls ACL**（非 0600）
- [ ] FileKeyProvider fallback（全平台单测）
- [ ] keychain seam build-tag 分流（Windows=DPAPI / Unix=keychain）
- [ ] resolveMasterKey 三级 + **错误分支硬失败**（解密失败不降级明文）
- [ ] v0.2.0 迁移：master key + **cache DEK** 两条（旧 slot 读出 → DPAPI 文件；读不出 → 清晰提示）
- [ ] 升级 Runbook（停旧进程 → 交互式 unlock 迁移 → serve install）写进成功标准 + 文档
- [ ] serve install/uninstall/status（**仅 Windows Task Scheduler**：at-startup + RestartOnFailure + 日志重定向 + COM API 注册避免 /RP）
- [ ] serve status 区分"进程活"vs"vault 已解锁"
- [ ] export/import `--passphrase-file`
- [ ] 跨平台编译（GOOS windows/linux/darwin —— Linux/macOS 仍编译，只是 serve install 不实现）
- [ ] **端到端真机**（NUC10：停旧 → 交互 unlock 迁移 → serve install → 重启 → 自起 → 笔记本连 → exec_command）
- [ ] 文档（backup-restore 升级 Runbook + master.key≠备份 + admin 重置密码；multi-machine Windows 部署）

## 11. 参考

- E2E 14 个 finding：`.omc/state/e2e-2026-08-12-{phase0,phase1-2,summary-and-plan14-seed}.md`（plan-13-nas-backup worktree；已 gitignore）
- FINDING 9 根因复现：NUC10 `wincred-test.exe`（sshd session 报 `ERROR_NO_SUCH_LOGON_SESSION 1312`）vs 本机 RDP session（正常）
- **DPAPI spike**（推翻共识 A，§12）：user-scope DPAPI 在 RDP/sshd/Task-Scheduler 三 session 全通 + 跨 session 解密成功
- 交叉评审：`.xcheck/20260812-141013-plan14/`（codex/opencode/pi 三家 SUGGEST_CHANGES，已 gitignore）
- Plan 10（serve 模式）：`internal/cli/serve.go`、`internal/mcpserver/serve.go`
- Plan 11（export/import）：`internal/vaultio/`、`internal/store/export.go`
- Plan 12（cache）：`internal/cli/cache.go`（cache DEK 也走 DPAPI-file，**在本 plan 范围内**，§4）
- Plan 13（backup）：`internal/cli/backup.go`、`docs/backup-restore.md`

## 12. 附录：DPAPI spike 结果（2026-08-12，推翻评审共识 A）

**背景**：交叉评审三家（codex/opencode/pi）独立指出"user-scope DPAPI 在 sshd/Task-Scheduler 非交互 session 的可用性未验证"（共识 A，最高优先），codex 给机制推断"LSA 无明文密码 → 解不开 DPAPI Master Key"。要求实现前先 spike。

**spike 程序**：~100 行 Go，纯 stdlib `syscall.NewLazyDLL("crypt32.dll")`，命令行接 `<scope:user|machine> <op:roundtrip|protect|unprotect>`，`defer LocalFree`。

**测试矩阵**（NUC10, user=allan716）：

| Session | user-scope | machine-scope |
|---|---|---|
| RDP 交互式（本机基线） | ✅ roundtrip ok | ✅ roundtrip ok |
| **sshd network logon**（`SESSIONNAME=(empty)`，FINDING 9 那类） | ✅ roundtrip + protect + unprotect 全 ok | ✅ 全 ok |
| **Task Scheduler batch logon**（`/RU allan716`，生产 serve 跑的 session） | ✅ unprotect ok | ✅ unprotect ok |
| **跨 session**（sshd protect → Task Scheduler unprotect，模拟 owner 写/serve 读） | ✅ **ok** | ✅ ok |

**结论**：
1. **共识 A 的担忧被推翻** —— user-scope DPAPI 在 sshd/Task-Scheduler session 都正常，跨 session 能解。codex 的机制推断在这个环境不成立（DPAPI Master Key 解密比 Credential Manager 宽容——可能用 cached logon creds 或 pre-loaded MK，不依赖 Credential Manager 那种交互式 logon session 的 credential store）。
2. **DPAPI 与 Credential Manager 行为不同** —— 同样"绑 logon session"，但 wincred CredWrite 在 sshd 报 1312，DPAPI 正常。两者底层 API 要求不同（Credential Manager 显式要 logon session 的 credential store；DPAPI 要 Master Key，后者在用户 profile 里已存）。
3. **spec §3.2 的 user-scope 选择成立** —— 实测可用 + 安全性严格更强（对同机其他用户保密），machine-scope 无优势（§3.2 决策理由硬化）。
4. **SSH CLI 管理（codex #1）也化解** —— sshd session DPAPI 能用，SSH 跑 `servers add`/`unlock` 没问题（FINDING 9 的 unlock 失败是 Credential Manager 读 v0.2.0 keychain，不是 DPAPI）。
5. **实现路径被实证** —— `crypt32.dll` syscall + LocalFree 跑通，spec §5.3 的"自己 syscall"可行。

spike 程序源码：`Temp/sshmgr-e2e/dpapi-spike/main.go`（一次性，未提交；可按此重跑）。

