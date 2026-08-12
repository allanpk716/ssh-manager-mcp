# Plan 14 — Windows 生产部署（DPAPI master key + serve 常驻）— Design Spec

**Date:** 2026-08-12
**Status:** Design — pending implementation plan
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
- user-scope DPAPI 绑 `allan716` 的 SID + 密码 hash → **只有 `allan716`（owner 或 serve）能解**，同机其他用户进程解不开
- ACL：文件 0600 + 文件夹 ACL（仅 `allan716`）

**安全模型（诚实）**：
- ✓ master key 对同机其他用户保密（user-scope DPAPI 绑账户）
- ✓ master key 对 agent 保密（agent 进程无 mk，走 broker）
- ⚠ master key 对 `allan716` 跑的**任意进程**不保密（任何 `allan716` 进程能 `CryptUnprotectData` 解 master.key）—— 这与 v0.2.0 keychain 等级相同（keychain 也对同用户进程不设防），**不是 regression**
- ⚠ `allan716` 密码改了 → 旧 DPAPI 密文解不开（DPAPI 绑密码 hash）→ 必须在改密码前迁移，或重设 vault

### 3.3 Linux/macOS：保持 keychain + 加 FileKeyProvider fallback（不写迁移）

- Linux secret-service / macOS keychain（go-keyring）**在 daemon session 理论上无 Windows 那种 logon-session 问题**——本次不验证（留后续），保持 `KeyringKeyProvider` 不变。
- 加 `FileKeyProvider`（0600 明文文件，全平台）作为**无 keychain 环境 fallback**（CI / 容器 / 无 secret-service 的 headless Linux）。Windows 不用它（DPAPI 优先）。
- **不写 Linux/macOS 的 v0.2.0 迁移**（v0.2.0 的 keychain slot 迁移逻辑在所有平台一致，但实测只在 Windows 跑）。

### 3.4 常驻机制用 **Task Scheduler**（不用 kardianos/service）

- Windows：Task Scheduler `at-startup`（schtasks XML，以 `allan716` 身份，需存密码）。
- Linux：systemd `--user` unit（或 system unit 指定用户）。
- macOS：launchd LaunchAgent。
- `serve install/uninstall/status` 子命令生成原生 scheduler 配置 + 注册。
- **理由**：无新 Go 依赖（kardianos/service 要 go get）；跨平台"原生 scheduler"模式统一；贴合 P13 文档已写的 schtasks 模式；Windows Service 配用户账户也要密码，Task Scheduler 不更差。
- ⚠ **kardianos/service 是备选**（若 review 认为 Task Scheduler 的密码存储/可靠性不可接受，切到 kardianos/service——它 Windows 也支持配置用户账户，但同样要密码）。

### 3.5 DPAPI **自己 syscall**（不用第三方库）

- `golang.org/x/sys/windows` 无 `CryptProtectData`/`CryptUnprotectData` 封装 → 自己封 `crypt32.dll` 两个调用（~30 行，含 `DATA_BLOB` 结构）。
- **理由**：无新依赖（项目铁律：依赖最小）；DPAPI 调用简单（两个 syscall + 一个结构体）；第三方库（`AdRoll/go-dpapi` 等）都只是这层的薄包装。

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
mk = CryptUnprotectData(blob)   // user-scope，绑当前用户 SID
return mk
```
**Set**：
```
blob = CryptProtectData(mk)     // user-scope
ensureDir(%AppData%\ssh-manager\) with ACL (仅 allan716)
os.WriteFile(path, blob, 0600)
```
**Delete**：`os.Remove(path)`（忽略 not-found）。

**ACL**：文件夹 `C:\Users\allan716\AppData\Roaming\ssh-manager\` —— 用 `icacls` 设 inheritance off + allan716 FullControl（Windows 不程序强制 0700，文档 + install 时设）。

### 5.3 DPAPI syscall（`dpapi_windows.go`）

```go
//go:build windows

package store

import "golang.org/x/sys/windows"

type dataBlob struct {
	cbData uint32
	pbData *byte
}

// CryptProtectData / CryptUnprotectData via crypt32.dll
// 不加 CRYPTPROTECT_LOCAL_MACHINE → user-scope（绑当前用户）
```
（实现细节：`windows.NewLazySystemDLL("crypt32.dll")` + `Proc` 调用，DATA_BLOB 用 `windows.NewLazySystemDLL` 的指针参数。~30 行。Go 侧 byte slice ↔ DATA_BLOB 的转换是唯一易错点，单测 round-trip 覆盖。）

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

### 5.6 `resolveMasterKey` 顺序（`vault/vault.go`，改）

```
1. SSHMGR_MASTERKEY_HEX env（dev/脚本）→ hex decode 返回
2. 平台 KeyProvider（keychain seam）Get：
   - Windows: DpapiKeyProvider
   - Unix: KeyringKeyProvider
   成功 → 返回
   ErrNotFound → 继续下一步
3. FileKeyProvider Get（fallback）：
   成功 → 返回
   ErrNotFound → "vault locked: run `ssh-manager unlock`"
```

### 5.7 v0.2.0 迁移（Windows only；`unlock` 首次运行）

**仅 Windows**：v0.2.0 在所有平台都用 `KeyringKeyProvider`（keychain）存 master key，但只有 **Windows** 上 keychain 在 sshd/Service session 读不出（FINDING 9 的 1312）→ Windows 迁移有真实价值（把能读出的旧 slot 迁到 DPAPI 文件）。Linux/macOS 的 v0.2.0 keychain slot 若存在，新版本仍用 `KeyringKeyProvider`（§3.3 保持不变，新旧存储介质相同）→ **无需迁移**。

`unlock`（Windows）当 `DpapiKeyProvider.Get()` 返回 `ErrNotFound`（DPAPI 文件不存在）时，**首次运行逻辑**：
1. 检测旧 keychain slot：`KeyringKeyProvider{Service:"ssh-manager",User:"master-key"}.Get()`。
2. 读出成功（交互式 session，wincred 正常）→ 提示"检测到 v0.2.0 master key，迁移到 DPAPI 文件？[y/N]" → `DpapiKeyProvider.Set(mk)` + `KeyringKeyProvider.Delete()`。
3. 读出失败但**非 ErrNotFound**（sshd session 报 `1312`）→ 捕获，提示：
   > "检测到可能的 v0.2.0 keychain master key 但当前会话读不出（sshd/非交互 session 的 Windows Credential Manager 限制）。请在**交互式会话**（本地终端/RDP）重跑 `unlock` 迁移，或重设 vault（见 docs/backup-restore.md）。"
4. 旧 slot `ErrNotFound`（干净环境）→ 正常 first-run generate + `DpapiKeyProvider.Set`。

**UX 边界**：迁移**必须**在交互式 session（owner 本地终端）跑；serve/Service 上下文不跑迁移（serve 启动时 mk 不存在 = 报"locked"，不自动迁移）。

### 5.8 `serve install/uninstall/status`

**Windows**（`serve_install_windows.go`）—— Task Scheduler：
- `serve install [--addr 0.0.0.0:7878]`：
  1. 确认 master.key 存在（DpapiKeyProvider.Get 不报 ErrNotFound）；不存在 → 提示先 `unlock`（交互式）。
  2. 生成 schtasks XML（at-startup，以 `allan716` 身份，RunLevel Highest）。
  3. 问 `allan716` 密码（交互 prompt，Task Scheduler 要求）。
  4. `schtasks /Create /XML ... /RU allan716 /RP <密码>`。
  5. 立即 `schtasks /Run` 启动一次（验证）。
- `serve uninstall`：`schtasks /Delete /TN ssh-manager-serve`。
- `serve status`：`schtasks /Query` + 检测 serve 进程 + curl localhost:7878。

**Linux**（`serve_install_linux.go`）—— systemd `--user`：
- `serve install`：生成 `~/.config/systemd/user/ssh-manager-serve.service` + `loginctl enable-linger <user>`（让 user service 开机自起）+ `systemctl --user enable/start`。
- uninstall/status 对应。

**macOS**（`serve_install_darwin.go`）—— launchd LaunchAgent：
- `serve install`：生成 `~/Library/LaunchAgents/com.ssh-manager.serve.plist` + `launchctl load`。
- uninstall/status 对应。

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

## 6. 安全考虑（写进文档，诚实）

- **DPAPI user-scope 的边界**：master key 对 `allan716` 的任意进程不保密（同 keychain 等级）；对同机其他用户、对 agent 保密。**不是 regression**。
- **`allan716` 密码变更**：DPAPI 密文绑密码 hash → 改密码后 master.key 解不开。**文档**：改密码前先迁移（`unlock --migrate` 或交互式重跑），或重设 vault（从 backup 恢复）。
- **Task Scheduler 存密码**：`schtasks /RP <密码>` 把 `allan716` 密码存进 Task Scheduler（系统级加密，但本质是凭据存储）。**文档**：或改用"自动登录 + At logon"触发器（不存密码但需自动登录）。
- **master.key 文件 ACL**：install 时 `icacls` 设文件夹仅 `allan716`。
- **恢复流程脚本化**：`--passphrase-file` 让 import 可无人值守——但要确保 passphrase 文件本身受控（0600 + 不进 git + 恢复后删）。
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

## 8. 实现触点（file-by-file）

| 文件 | 改动 |
|---|---|
| `internal/store/dpapi_windows.go`（新）| CryptProtectData/UnprotectData syscall（`//go:build windows`）|
| `internal/store/masterkey_windows.go`（新）| `DpapiKeyProvider`（DPAPI user-scope + 文件）|
| `internal/store/masterkey_file.go`（新）| `FileKeyProvider`（0600 明文 fallback，全平台）|
| `internal/store/masterkey.go`（改）| 迁移 helper（检测旧 keychain slot）|
| `internal/store/*_test.go`（新/改）| DPAPI/File/迁移测试 |
| `internal/cli/keychain_windows.go`（新）| `var keychain = DpapiKeyProvider{}` |
| `internal/cli/keychain_unix.go`（新）| `var keychain = KeyringKeyProvider{}` |
| `internal/cli/unlock.go`（改）| 删 line 14 的 keychain init（移到 build-tag 文件）；加迁移逻辑 |
| `internal/vault/vault.go`（改）| `resolveMasterKey` 加 FileProvider fallback |
| `internal/cli/serve.go`（改）| 加 `install`/`uninstall`/`status` 子命令入口 |
| `internal/cli/serve_install_windows.go`（新）| Task Scheduler schtasks XML + 注册 |
| `internal/cli/serve_install_linux.go`（新）| systemd --user unit |
| `internal/cli/serve_install_darwin.go`（新）| launchd LaunchAgent |
| `internal/cli/export.go` / `import.go`（改）| `--passphrase-file` flag |
| `docs/backup-restore.md`（改）| 升级路径 + DPAPI/master.key 说明 + 密码变更迁移 + serve install 文档 |
| `docs/multi-machine.md`（改）| serve 部署章节改 Task Scheduler at-startup（配合 Plan 13 UNC 路径模式）|

## 9. 未来工作（显式 deferred）

- **Linux/macOS daemon session keychain 验证**（§3.3，本次不验证）。
- **kardianos/service 替换 Task Scheduler**（若 review/实测发现 Task Scheduler 密码存储/可靠性问题）。
- **serverInfo.version 注入**（Plan 9 land-later，独立修）。
- **machine-scope DPAPI 选项**（若未来要支持多用户共享 vault）。
- **serve 的 admin endpoint**（远程 owner 管理 vault，本次 owner 仍 ssh 到 serve 机本地管理）。

## 10. 落地前 checklist

- [ ] DPAPI user-scope round-trip（Windows 单测）
- [ ] DpapiKeyProvider Set/Get/Delete + ACL（Windows 单测）
- [ ] FileKeyProvider fallback（全平台单测）
- [ ] keychain seam build-tag 分流（Windows=DPAPI / Unix=keychain）
- [ ] resolveMasterKey 三级顺序（env → 平台 → File fallback）
- [ ] v0.2.0 迁移（旧 slot 读出 → 新存储；读不出 → 清晰提示）
- [ ] serve install/uninstall/status（Windows Task Scheduler + Linux systemd + macOS launchd）
- [ ] export/import `--passphrase-file`
- [ ] 跨平台编译（GOOS windows/linux/darwin）
- [ ] **端到端真机**（NUC10 install → 重启 → 自起 → 笔记本连 → exec_command）
- [ ] 文档（backup-restore 升级路径 + multi-machine 部署）

## 11. 参考

- E2E 14 个 finding：`.omc/state/e2e-2026-08-12-{phase0,phase1-2,summary-and-plan14-seed}.md`（在 plan-13-nas-backup worktree）
- FINDING 9 根因复现：NUC10 `wincred-test.exe`（sshd session 报 `ERROR_NO_SUCH_LOGON_SESSION 1312`）vs 本机 RDP session（正常）
- Plan 10（serve 模式）：`internal/cli/serve.go`、`internal/mcpserver/serve.go`
- Plan 11（export/import）：`internal/vaultio/`、`internal/store/export.go`
- Plan 12（cache）：`internal/cli/cache.go`（cache DEK 也用 keychain，FINDING 9 同样影响它 → cache DEK 也走 DPAPI-file，**在本 plan 范围内**，见 §4 范围说明）
- Plan 13（backup）：`internal/cli/backup.go`、`docs/backup-restore.md`
