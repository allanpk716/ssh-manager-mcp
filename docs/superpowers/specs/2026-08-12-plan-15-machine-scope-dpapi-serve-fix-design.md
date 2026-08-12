# Plan 15 — machine-scope DPAPI + serve install 修复 — Design Spec

> **修订**：Plan 14（`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`）的 §3.2（user-scope→machine-scope）、§5.3（DPAPI flag）、§5.8（serve install 对象 API）、§6（威胁模型）、§7.2（真机集成测试）。
> **依据**：`docs/superpowers/specs/2026-08-12-plan-14-nuc10-e2e-findings.md`（Plan 14 §7.3 NUC10 真机验收暴露的 FINDING B/C/D/E/F）。
> **目标**：让 `ssh-manager serve install` → boot 自起 → serve 可用 这条链在 Windows 生产环境真正跑通。

---

## 1. 背景：Plan 14 §7.3 验收暴露什么

Plan 14 的两个核心交付物在 NUC10 真机上都不能直接可用：

- **FINDING B（架构缺陷）**：user-scope DPAPI 的 master.key，在"RDP 交互 session 生成 → boot/Password-logon session 读"的**跨 logon session** 场景失败（`CryptUnprotectData: Key not valid for use in specified state`）。boot 自起的 serve（必须 LogonType=Password）永远读不出 master.key。Plan 14 spec §12 spike 的"user-scope 三 session 全通"结论基于不完整的 roundtrip 测试，被推翻。
- **FINDING C（实现 bug）**：`serve install` 命令有 3 个叠加 bug（stdin/$input 拿不到多行 XML、UTF-16 声明 vs UTF-8 字节、`Register-ScheduledTask -Xml` 序列化失败），从未在真机可用过。
- **FINDING D**：RestartOnFailure 没持久化（注册后 Count=0）。
- **FINDING E**：`serve status` 简体中文本地化误报（`计划任务状态:` 不匹配 `Status:`/`任务状态:`）+ process-running 误报。
- **FINDING F**：`serve status` 的 vault-ok 扫描基于陈旧 serve.log（boot 失败时 log 不增长，扫到的是上次成功的历史标记）。

**根因**：Plan 14 spec §7.2 的 `SSHMGR_SERVE_INSTALL=1` gated 集成测试**从未在真机或 CI 跑过**（默认跳过），单测只覆盖 `buildServeTaskXML` 纯函数字符串，所以 serve install 的 PowerShell/CIM bug + DPAPI 跨 session 缺陷全藏着。

Plan 14 的 vault 修复链路（export/import/unlock 迁移）已验证可用；DPAPI syscall 可行；安全绳机制可靠。**只是 user-scope 选型错了 + serve install 实现裸奔**。Plan 15 修这两块。

---

## 2. 目标

1. **machine-scope DPAPI**：master.key + cache-dek.key 改用 `CRYPTPROTECT_LOCAL_MACHINE`，让任何 session（boot Service、sshd、Password-logon、RDP）都能解。
2. **serve install 能工作**：换对象 API 注册，修三个 XML bug + RestartOnFailure 持久化。
3. **serve status 准确**：修本地化误报 + process 误报 + 陈旧 log 误报。
4. **真机集成测试进 CI**：§7.2 gate 在 CI windows-latest 真跑（注册→起→status→uninstall），不再裸奔。
5. **迁移路径**：现有 user-scope master.key（NUC10 的 key C）→ machine-scope 重 protect，C 值不变，store.db 数据不动。
6. **NUC10 §7.3 重验**：boot 自起 + 跨重启 DPAPI + 笔记本 exec 全通过。

---

## 3. 关键决策（brainstorm + 用户拍板）

### 3.1 合并一个 Plan 15 全修

machine-scope（解 FINDING B）+ serve install 修复（解 FINDING C/D/E/F）+ 真机集成测试，**一个 plan**。两者都改才能让 `serve install → boot 自起 → serve 可用` 链跑通；拆开任一个都留断链。代价：plan 较大（~9-10 task），但同根因、同文件、同真机验收，割裂反而错。

### 3.2 master.key = machine-scope DPAPI + 文件（推翻 Plan 14 §3.2）

- 文件：`%AppData%\ssh-manager\master.key`（路径不变）
- 内容：`CryptProtectData` **加** `CRYPTPROTECT_LOCAL_MACHINE(0x1)` 加密的 32 字节 mk
- machine-scope 绑**机器**，不绑用户 SID / logon session → **任何 session 能解**
- 代价：同机其他用户进程也能解 → 靠 **ACL 兜底**（master.key 文件夹 `icacls /inheritance:r /grant:r allan716:(OI)(CI)F` only，其他用户读不到文件 → 即使理论上能解也读不到）

**user-scope → machine-scope 决策理由**（Plan 14 §3.2 推翻）：
- Plan 14 §12 spike 测的是**同 session roundtrip**，没测**跨 logon session**（RDP 生成 → Password-logon 读）。生产路径（boot 自起）正是跨 logon session，user-scope 失败。
- machine-scope 在"boot 自起必须 LogonType=Password、该 session 解不开 user-scope"的死结下，是唯一满足"无人值守 boot 自起"的方案。
- 安全性弱于 user-scope（对同机其他用户不保密），但 NUC10 单用户 + ACL 兜底，可接受。未来若多用户共享 vault，machine-scope 反而更合适。

### 3.3 cache DEK 同改 machine-scope

Plan 12 的 cache DEK（Windows 用 `DpapiKeyProvider`，user-scope）同步改 machine-scope。理由：cache 工作机的 scheduled cache-pull task 也是 Password-logon session → 同样撞 FINDING B。一致改，避免 cache 场景重踏同一坑。cache-dek.key 文件路径不变。

### 3.4 serve install 换对象 API（推翻 Plan 14 §5.8 XML 链）

删 `buildServeTaskXML` + `registerTaskViaPowerShell`，改用 PowerShell 对象 API（`New-ScheduledTaskAction/Trigger/SettingsSet` + `Register-ScheduledTask` User 参数集）。让 PowerShell 自己把对象序列化成正确 XML 给 CIM，绕开 Go 生成 XML 的全部坑（编码、序列化、本地化）。

### 3.5 迁移策略：重 protect C（不重新生成）

unlock 检测到旧 user-scope master.key（交互 session 能读出 C）→ 用 machine-scope 重 protect C 写回。C 值不变，store.db 凭据不动（它们绑 C，与 master.key 的存储 scope 无关）。比"重新生成 D + import 安全绳"轻量，且 C 没问题（只是存储 scope 错了）。

### 3.6 集成测试：CI 验注册+起+status，reboot 留手动

CI（GitHub Actions windows-latest）跑 `serve install` → 验 task 注册 + `schtasks /Run` 起 serve + HTTP 401 + vault ok + `serve status` 四路 + `serve uninstall`。能抓 C/D/E 类 bug。**reboot 自起（BootTrigger）留手动 runbook**（CI runner 不能 reboot）。这是 FINDING C 根因（gate 从没跑过）的直接修复。

### 3.7 不变的部分

- DPAPI 自己 syscall（Plan 14 §3.5，不加第三方库）——只加 `flagMachine` 参数
- serve 以用户账户（allan716）跑（Plan 14 §3.1）——不变
- Linux/macOS 保持 keychain + FileProvider（Plan 14 §3.3）——不变，serve install 仍 Windows only
- Task Scheduler（非 kardianos/service，Plan 14 §3.4）——不变
- L2 安全模型 / iron rule / broker 工具集——全不动

---

## 4. 非目标

- **不做 Linux/macOS serve install**（仍 deferred）。
- **不做 reboot 自起的 CI 自动验证**（CI 不能 reboot）。
- **不修 serverInfo.version 硬编码**（Plan 9 land-later，无关）。
- **不换 kardianos/service**（对象 API + Task Scheduler 够用）。
- **不改 master key 生命周期**（unlock 生成 + 持久化模型不变，只换 DPAPI scope + 加 user→machine 迁移分支）。
- **不改 serve HTTP/MCP 协议、不改 broker 工具集**。

---

## 5. 设计

### 5.1 DPAPI syscall 加 machine flag（`internal/store/dpapi_windows.go`，改）

`dpapiProtect`/`dpapiUnprotect` 加 `machine bool` 参数：

```go
//go:build windows
package store

func dpapiProtect(plain []byte, machine bool) ([]byte, error) {
    // ... 同 Plan 14，但 flags 按 machine 选
    flags := uintptr(0)
    if machine {
        flags = flagMachine // 0x1 = CRYPTPROTECT_LOCAL_MACHINE
    }
    // CryptProtectData(..., flags, ...)
}

func dpapiUnprotect(blob []byte, machine bool) ([]byte, error) {
    // 同理：machine=true 时传 flagMachine
    // 注意：CryptUnprotectData 的 flag 也必须匹配 protect 时的 scope
    //       ——user-scope 加密的 blob 用 machine flag 解会失败，反之亦然
}
```

**关键约束**：protect 和 unprotect 的 scope 必须一致。迁移时要先 user-scope unprotect 旧 blob、再 machine-scope protect 新 blob。

### 5.2 `DpapiKeyProvider` 改 machine-scope（`internal/store/masterkey_windows.go`，改）

```go
func (p DpapiKeyProvider) Get() ([]byte, error) {
    blob, err := os.ReadFile(path)
    // ...
    // machine-scope unprotect（主路径）
    mk, err := dpapiUnprotect(blob, true)
    if err == nil { return mk, nil }
    // 兼容旧 user-scope blob（迁移路径用，§5.5）：user-scope fallback unprotect
    mk2, err2 := dpapiUnprotect(blob, false)
    if err2 == nil { return mk2, nil }  // 旧 user-scope blob，调用方决定是否迁移
    return nil, err  // 两个 scope 都失败，返回 machine-scope 的错误
}
```

**注意**：Get 的双 scope 尝试让"读旧 user-scope master.key"在迁移窗口期仍可工作。Set 一律 machine-scope：

```go
func (p DpapiKeyProvider) Set(mk []byte) error {
    // ... ensureDirACL（不变，ACL 仍是 allan716 only）
    blob, err := dpapiProtect(mk, true)  // machine-scope
    // ... 原子写 temp+rename
}
```

ACL 不变（`icacls /inheritance:r /grant:r allan716:(OI)(CI)F`）——machine-scope 削弱了"对同机其他用户保密"，ACL 是兜底防线，必须保留。

### 5.3 user-scope → machine-scope 迁移（`internal/cli/migrate_windows.go`，改）

新增迁移分支（与 Plan 14 的 v0.2.0 keychain→DPAPI 迁移同级，但这次是 DPAPI scope 迁移）：

```go
// migrateDpapiScope 检测 master.key 是否是旧 user-scope blob，是则提示重 protect 为 machine-scope。
// 必须交互 session（user-scope 旧 blob 只在交互 session 能解）。
func migrateDpapiScope(w io.Writer) error {
    // 1. 读 master.key blob，尝试 machine-scope unprotect
    //    - 成功 = 已经是 machine-scope，无需迁移，返回 nil
    //    - 失败 → 尝试 user-scope unprotect
    //      - 成功 = 旧 user-scope blob → 提示 "migrate to machine-scope? [y/N]"
    //        - y → 用 machine-scope 重 protect mk，原子写回 master.key
    //        - N → 报 "master.key 是旧 user-scope，serve 在非交互 session 读不出；建议迁移或重新 unlock"
    //      - 失败 = 两个 scope 都读不出（损坏/admin 重置）→ 报错（不静默降级）
    // 2. cache-dek.key 同理（如果存在）
}
```

unlock 触发：unlock 在 keychain seam（DpapiKeyProvider）Get 成功后，调 `migrateDpapiScope`（如果迁移分支返回"已迁移"，继续；"拒绝/失败"按情况报）。

**约束**（同 Plan 14 v0.2.0 迁移）：必须交互 session（RDP/本地）。sshd/非交互 session 读不出 user-scope 旧 blob → 提示"在交互 session 重跑"，**不静默生成新 key**（避免 orphan vault）。

### 5.4 serve install 对象 API（`internal/cli/serve_install_windows.go`，重写）

删 `buildServeTaskXML` + `registerTaskViaPowerShell`。新 `registerTaskViaPowerShell`：

```go
func registerTaskViaPowerShell(in taskInputs) error {
    // 构造 PowerShell 脚本（对象 API），Go 把参数（exe path/addr/user/log path）
    // 作为 stdin 或 -ArgumentList 传入（不走 XML）
    const ps = `$ErrorActionPreference='Stop'
$actionArg = '/C if not exist "' + $logDir + '" mkdir "' + $logDir + '" & "' + $exe + '" serve --addr "' + $addr + '" >> "' + $logPath + '" 2>&1'
$action = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument $actionArg
$trigBoot = New-ScheduledTaskTrigger -AtStartup
$trigLogon = New-ScheduledTaskTrigger -AtLogOn -User $user
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -DontStopIfGoingOnBatteries -AllowStartIfOnBatteries
$cred = Get-Credential -UserName $user -Message '...'
Register-ScheduledTask -TaskName 'ssh-manager-serve' -Action $action -Trigger @($trigBoot,$trigLogon) -Settings $settings -RunLevel Limited -User $cred.UserName -Password $cred.GetNetworkCredential().Password -Force | Out-Null
Write-Output "REGISTERED"
`
    // 参数（exe/addr/user/logPath/logDir）作为环境变量或 stdin 传给 PowerShell，
    // 不进 argv（避免 4688 + 避免引号坑）
    cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
    // 传参：cmd.Env 追加 SSHMGR_INSTALL_EXE / ADDR / USER / LOGPATH 等，
    //       PowerShell 用 $env:SSHMGR_INSTALL_EXE 读
    // ... CombinedOutput, 检查 "REGISTERED"
}
```

**参数传递**：用环境变量（`$env:SSHMGR_INSTALL_*`）而非 argv——既避免 4688 暴露路径，又避免引号/空格坑。密码仍走 `Get-Credential`（PowerShell 进程内，不进 argv）。

**RestartOnFailure（FINDING D）**：实现时**实证** `-RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)` 是否持久化（验收时 Count=0，可能是参数组合冲突）。若对象 API 参数仍不持久化，注册后用 `Set-ScheduledTask` 补 `RestartOnFailure.Interval='PT1M' Count=3`（CIM 对象，需重新输密码——或在同一 PowerShell 脚本内 Register 后立即 Set，复用 `$cred`）。**契约：注册后 `Get-ScheduledTask` 的 `Settings.RestartOnFailure.Count==3`，CI 断言此**。

### 5.5 serve status 修复（E + F，`internal/cli/serve_install_windows.go`）

**E（本地化）**：`schtasksQuery` 删掉 `Status:`/`任务状态:` 文本前缀解析。改用：
```go
// 用 Get-ScheduledTask / Get-ScheduledTaskInfo（PowerShell），返回结构化数据
// State: Ready/Running/Disabled（枚举，不本地化）
// LastTaskResult: 整数（0=ok, 267009=running, 267011=not-run-yet）
func taskState(taskName string) (state string, lastResult uint32, err error) {
    // powershell -Command "Get-ScheduledTask -TN X | Select State; Get-ScheduledTaskInfo -TN X | Select LastTaskResult"
    // 解析 stdout（State 是英文枚举，跨语言一致）
}
```

**process running 误报**：`serveProcessRunning()` 改用 `Get-Process ssh-manager` 精确匹配 + 验证 PID 对应的进程确实监听 7878（netstat 关联），避免"匹配到残留进程名"误报。

**F（陈旧 log）**：`vaultUnlockedFromLog` 加时间戳检查：
```go
func vaultUnlockedFromLog() (unlocked bool, note string) {
    info, _ := os.Stat(logPath)
    if time.Since(info.ModTime()) > 5*time.Minute {
        return false, " (log stale >5min; current state unknown)"
    }
    // ... 原有 marker 扫描
}
```

### 5.6 文件布局（不变）

```
%AppData%\ssh-manager\master.key       (machine-scope DPAPI, key C)
%AppData%\ssh-manager\cache-dek.key    (machine-scope DPAPI，如存在)
%AppData%\ssh-manager\store.db          (不变)
%LocalAppData%\ssh-manager\serve.log    (不变)
```

---

## 6. 安全考虑（推翻性修订 Plan 14 §6）

- **DPAPI machine-scope 的边界（修订）**：master.key 对**同机任何用户的进程**不保密（machine-scope 绑机器不绑 SID）——**这是 user-scope→machine-scope 的安全性 regression**，但靠 ACL 兜底（master.key 文件夹 `icacls allan716:(OI)(CI)F` only，其他用户读不到文件）。NUC10 单用户机适用；未来多用户机要重评。
- **威胁模型更新**：master.key 的防线从"user-scope DPAPI（对其他用户保密）+ ACL"降级为"**ACL 独力承担**"。ACL 必须 100% 可靠（`ensureDirACL` 每次写 master.key 都无条件 `icacls /inheritance:r /grant:r`，防外部进程松绑——Plan 14 已实现，Plan 15 保留）。
- **密码变更（不变）**：只有 admin 强制重置密码才让 master.key 解不开（machine-scope 同样依赖用户 DPAPI Master Key，admin reset 会断）；用户自行改密码无影响。
- **master.key ≠ 备份（不变）**：machine-scope 也绑机器，换机/重装即废物。灾备仍是 Plan 11 export 信封 + Plan 13 NAS 备份。
- **Task Scheduler 密码（不变）**：Get-Credential + 对象 API Register-ScheduledTask，不进 argv/4688。
- **L2 模型 / iron rule 不变**。

---

## 7. 测试

### 7.1 单元测试
- **`dpapi_windows_test.go`**（build windows）：machine-scope roundtrip（`dpapiProtect(p,true)` → `dpapiUnprotect(b,true)`）；user-scope roundtrip（`machine=false`）；**scope 隔离**（machine protect + user unprotect 必失败，反之亦然）。
- **`masterkey_windows_test.go`**：DpapiKeyProvider Set(machine) → Get 成功；**Get 的 user-scope fallback**（构造旧 user-scope blob，Get 能读出）；ACL（文件夹 allan716 only）。
- **迁移测试**（`unlock_windows_test.go`）：构造旧 user-scope master.key → unlock 触发 → machine-scope 重 protect；拒绝迁移 → 正确提示；sshd/非交互 session 读不出旧 user-scope → 提示重跑（不生成新 key）。
- **serve install 参数构造测试**：测传给 PowerShell 的 action 命令行、trigger（Boot+Logon）、RunLevel=LeastPrivilege（对应 -RunLevel Limited）、RestartCount=3（契约断言）。

### 7.2 集成测试（CI windows-latest）—— **根因修复，必须真跑**
- **install → status → uninstall round-trip**：
  1. `ssh-manager serve install`（CI 通过 env 提供 Windows 账户密码，或用 CI runner 的服务账户——实现时选定）
  2. 验 task 注册（`Get-ScheduledTask.State == Ready`）
  3. 验 `Settings.RestartOnFailure.Count == 3`（FINDING D 契约）
  4. `schtasks /Run` → 等 serve 起 → HTTP `localhost:7878` 返回 401（鉴权工作）
  5. **验 vault ok**（machine-scope master.key 在 serve 进程能解 store.db）—— 这是 FINDING B 的 CI 级验证
  6. `serve status` 四路全绿（task/process/http/vault），`overall: HEALTHY`
  7. `serve uninstall` → task 删除 + 进程停
- **CI workflow**：`.github/workflows/serve-install-windows.yml`，`on` push/PR 改 `internal/cli/serve_install*.go` 或 `internal/store/dpapi*.go`/`masterkey*.go` 时跑；windows-latest runner。**这个集成测试不再 gated（不再默认跳过）——它是 FINDING C 根因的直接修复，必须在 CI 真跑。** 密码通过 GitHub secret 注入（`SSHMGR_SERVE_INSTALL_PASSWORD`），CI runner 的本地账户配不过期密码。
- **reboot 自起留手动 runbook**（CI 不能 reboot）：docs 里写 NUC10 真机 reboot 验证步骤（Plan 14 §7.3 同款），作为 release 前的人工 checklist 项。

### 7.3 端到端（NUC10 真机，Plan 14 §7.3 重做）
**部署 Plan 15 新版到 NUC10**（已修复的 vault key C 还在，user-scope master.key 待迁移）：
1. NUC10 部署新版 ssh-manager.exe。
2. NUC10 交互式（RDP）跑 `unlock` → 触发 user→machine 迁移（重 protect C）。
3. `serve install` → 对象 API 注册（这次能工作）。
4. **reboot NUC10** → BootTrigger 自起 serve → `serve status` 全绿 + `vault: ok`（machine-scope 跨重启可解，FINDING B 修复验证）。
5. 笔记本 MCP 连 `http://192.168.100.235:7878` → `exec_command` 在 1660Super01 跑 `hostname` → 返回 `DESKTOP-UP1MHGT`。
6. 清理。

### 7.4 No-regression
- `go test ./...` green（含 build-tag 测，Windows + Linux 都跑 gofmt/vet）。
- `gofmt -l .` 干净。
- 跨平台编译 `GOOS=windows/linux/darwin go build ./...` 都过。

---

## 8. 实现触点（file-by-file）

| 文件 | 改动 |
|---|---|
| `internal/store/dpapi_windows.go`（改）| `dpapiProtect`/`dpapiUnprotect` 加 `machine bool` 参数，按 scope 选 flag |
| `internal/store/masterkey_windows.go`（改）| `DpapiKeyProvider.Set` 用 machine-scope；`Get` 双 scope 尝试（machine 主 + user fallback）|
| `internal/cli/migrate_windows.go`（改）| 新增 `migrateDpapiScope`（user-scope → machine-scope 重 protect）；unlock 触发 |
| `internal/cli/serve_install_windows.go`（重写）| 删 `buildServeTaskXML` + `registerTaskViaPowerShell` 的 XML 链；对象 API 注册；修 RestartOnFailure（D）；status 用 Get-ScheduledTask/Info（E）；process 精确匹配（E）；vault-ok 加陈旧检查（F）|
| `internal/cli/serve_install_windows_test.go`（改）| 单测从 XML 字符串改成对象 API 参数契约；RestartOnFailure 持久化断言 |
| `internal/store/*_test.go`（改）| DPAPI machine-scope roundtrip + scope 隔离；DpapiKeyProvider 双 scope Get；迁移测试 |
| `.github/workflows/serve-install-windows.yml`（新）| CI windows-latest 真 run §7.2 集成测试 |
| `docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`（改）| §3.2/§5.3/§5.8/§6/§7.2 推翻性修订，指向 Plan 15 spec |
| `docs/backup-restore.md` + `docs/multi-machine.md`（改）| machine-scope 威胁模型 + ACL 兜底 + user→machine 迁移 runbook |

---

## 9. 未来工作（显式 deferred）

- **Linux/macOS serve install**（systemd --user / launchd）——仍 deferred（Plan 14 §3.4）。
- **reboot 自起的 CI 自动验证**——需要能 reboot 的 CI 环境（Windows container / self-hosted runner），本次手动 runbook。
- **多用户共享 vault**——machine-scope 已支持（任何同机用户能解 + ACL 放权），但 per-user ACL 策略未做，留专门 plan。
- **serverInfo.version 硬编码**（Plan 9 land-later）。

---

## 10. 落地前 checklist

- [ ] DPAPI machine-scope roundtrip + scope 隔离（单测）
- [ ] DpapiKeyProvider machine-scope Set / Get（+ user-scope fallback）+ ACL 仍 allan716 only
- [ ] user-scope → machine-scope 迁移（migrateDpapiScope，交互 session，不 orphan）
- [ ] cache-dek 也改 machine-scope
- [ ] serve install 对象 API（删 XML 链）+ 参数经 env 不进 argv + 密码 Get-Credential
- [ ] RestartOnFailure 持久化（注册后 Count==3 契约）
- [ ] serve status 用 Get-ScheduledTask/Info（本地化修复）+ process 精确匹配 + vault-ok 陈旧检查
- [ ] CI serve-install-windows.yml 真 run §7.2（不再 gated，密码走 GitHub secret）
- [ ] spec §3.2/§5.3/§5.8/§6/§7.2 推翻性修订 + 威胁模型更新
- [ ] docs 升级 runbook（user→machine 迁移）+ ACL 兜底说明
- [ ] NUC10 §7.3 重做（unlock 迁移 → serve install → reboot → 自起 → 笔记本 exec）

---

## 11. 参考

- Plan 14 spec：`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`
- Plan 14 §7.3 NUC10 E2E findings：`docs/superpowers/specs/2026-08-12-plan-14-nuc10-e2e-findings.md`（FINDING B/C/D/E/F）
- 过程数据：`.omc/state/p14-nuc10-e2e-2026-08-12.md`（gitignore，按时间序证据链）
- DPAPI spike（Plan 14 §12，结论被推翻）：user-scope roundtrip 假阳，跨 logon session 失败
- 验收时对象 API 绕法脚本：`rdp-B8-object-api.ps1`（gitignore，Plan 15 正式化的基础）
