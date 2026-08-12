# Plan 15 — machine-scope DPAPI + serve install 修复 — Design Spec (v2)

> **修订**：Plan 14（`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`）的 §3.2（user-scope→machine-scope）、§5.3（DPAPI flag）、§5.8（serve install 对象 API）、§6（威胁模型）、§7.2（真机集成测试）。Plan 14 正文**不改写**（保留审计轨迹），仅在其顶部加"Superseded by Plan 15"横幅。
> **依据**：`docs/superpowers/specs/2026-08-12-plan-14-nuc10-e2e-findings.md`（Plan 14 §7.3 NUC10 真机验收暴露的 FINDING B/C/D/E/F）。
> **v2 依据**：4 家异构 xcheck（codex/opencode/pi/kimi，全部 SUGGEST_CHANGES）+ 主会话核实（读代码 + spike）。`.xcheck/20260812-210000/SUMMARY.md` §5（读代码）/ §6（spike）。v2 采纳**全部经核实成立**的主张；2 条高危误报被 spike 推翻（pi #2 双 trigger、pi #3 ACL 继承——见 §5.4 多实例契约 / §5.2 ACL 契约）。
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
2. **serve install 能工作**：换对象 API 注册，修三个 XML bug + RestartOnFailure 尽力修复（spike 3 确认 PS 5.1 对象 API 不持久化，R1/R2 二选一或降级，见 §5.4）。
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

**user-scope → machine-scope 决策理由**（Plan 14 §3.2 推翻；v2 spike 厘清根因）：
- Plan 14 §12 spike 测的是**同 session roundtrip**，没测**跨 logon session**（RDP 生成 → Password-logon 读）。生产路径（boot 自起）正是跨 logon session，user-scope 失败。
- **v2 spike 2b 厘清 FINDING B 根因**：user-scope DPAPI 在 sshd session **内** roundtrip（protect+unprotect）= ok；失败只在**跨 logon session**（RDP 生成 master.key，sshd/Password-logon 读）。根因不是 scope flag、不是 session 类型本身，是**跨 logon session 时 user-scope DPAPI Master Key 不可用**（用户 profile/MK 未加载）。machine-scope 用 `DPAPI_SYSTEM` LSA secret（不依赖用户 profile）→ 跨 session 可解。这也彻底厘清 Plan 14 §12 spike 为何"假阳"：测同 session roundtrip（对），推断跨 session（错）。
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
    // machine=true 时传 flagMachine。
    // 注意：v2 spike 2 实测——CryptUnprotectData 的 flag **不强制 scope 隔离**：
    // machine-protected blob 用 flags=0（user 模式）也能解，反之亦然（DPAPI blob 内嵌
    // scope 元数据，解密时从 blob 自身判断用哪把 key）。所以这里的 machine bool 参数
    // 只是"优先尝试的 scope"，不是权威约束——见 §5.2 Get 的双 scope尝试。
}
```

**v2 修正（spike 2 实证）**：protect/unprotect 的 scope **不必一致**（跨 scope 能解）。"关键约束"那句 v1 写错了（codex #6/pi #7，已 spike 确认）。迁移时仍先 user-scope unprotect 旧 blob、再 machine-scope protect 新 blob（写新 blob 用 machine scope），但这只是流程，不是"必须"——即使 flag 不匹配也能解。

### 5.2 `DpapiKeyProvider` 改 machine-scope（`internal/store/masterkey_windows.go`，改）

```go
func (p DpapiKeyProvider) Get() ([]byte, error) {
    blob, err := os.ReadFile(path)
    // ...
    // machine-scope unprotect（主路径）
    mk, err := dpapiUnprotect(blob, true)
    if err == nil { return mk, nil }
    // 兼容旧 user-scope blob（迁移窗口期）：user-scope fallback unprotect。
    // spike 2 已证 scope flag 不强制隔离，但双 scope 尝试仍保证"无论旧 blob 是哪个
    // scope 都能读出"，迁移探测（§5.3）单独判断要不要迁。
    mk2, err2 := dpapiUnprotect(blob, false)
    if err2 == nil { return mk2, nil }
    return nil, err  // 两个 scope 都失败，返回 machine-scope 的错误
}
```

**v2 设计（共识 D）**：Get **不返回 scope**（避免改 KeyProvider 接口签名，影响所有 provider + 所有调用方）。迁移探测由 `migrateDpapiScope`（§5.3）单独读 blob + 判断 scope —— KeyProvider 接口不变，迁移逻辑收敛到一处。Get 的双 scope fallback 是**迁移窗口期的临时容错**（几个 release 后，旧 user-scope blob 清零，fallback 可删——§9 land-later 记）。

Set 一律 machine-scope：

```go
func (p DpapiKeyProvider) Set(mk []byte) error {
    // ... ensureDirACL（不变，ACL 仍是 allan716 only）
    blob, err := dpapiProtect(mk, true)  // machine-scope
    // ... 原子写 temp+rename
}
```

ACL 不变（`icacls /inheritance:r /grant:r allan716:(OI)(CI)F`）——machine-scope 削弱了"对同机其他用户保密"，ACL 是兜底防线，必须保留。

**v2 ACL 契约（pi #3 防御性，核实见 SUMMARY §5）**：现有 `masterkey_windows.go:82` 的 `os.CreateTemp(dir, ...)` 已正确（`dir` = protectedDir，temp 继承 allan716-only ACL，rename 后保留）。但 machine-scope 下 ACL 是唯一防线，**spec 钉死契约**：Set 的 temp 文件**必须建在 protectedDir 内**（`os.CreateTemp(protectedDir, ...)`），**严禁**用 `os.TempDir()` 或任何系统临时目录（那些继承宽 ACL，rename 后保留宽 ACL → machine-scope 下全库失守）。单测断言：Set 后 master.key 的 ACL 只含 allan716 + SYSTEM，无 Everyone/Users。

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
    //        - y → 用 machine-scope 重 protect mk，原子写回 master.key（ACL 契约见 §5.2）
    //        - N → 报 "master.key 是旧 user-scope，serve 在非交互 session 读不出；建议迁移或重新 unlock"
    //      - 失败 = 两个 scope 都读不出（损坏/admin 重置）→ 报错（不静默降级）
    // 2. cache-dek.key 同理（如果存在）—— 复用 Plan 14 v0.2.0 迁移的"master 拒绝则 dek 跳过"
    //    一致性策略（migrateSources/confirmMigrate，一次确认管两个 key）
}
```

**v2 关键修复（codex #1，读代码核实 SUMMARY §5）**：**unlock 触发迁移必须加新钩子**。当前 `unlock.go:68-73` 流程是 `keychain.Get() → err==nil → printMasterKey → return`，**Get 成功后直接返回，没有 post-Get 钩子**；现有 `firstRunMigrator` 只在 `ErrNotFound` 触发（unlock.go:95）。而双 scope Get 让旧 user-scope blob 返回 `err==nil` → 永远到不了 `firstRunMigrator` → **迁移不可达**。

修法：在 `unlock.go` 加 `postGetMigrator` package var（类比现有 `firstRunMigrator`），在 **Get 成功后、printMasterKey 前**调用；`migrate_windows.go` 的 `init()` 里赋值 `postGetMigrator = migrateDpapiScope`（Unix builds 留 nil，同 `firstRunMigrator` 模式）。**`unlock.go` 必须列进 §8 触点表**（v1 漏了）。

UX（opencode #6）：只在 master.key 实际是 user-scope blob 时提示（migrateDpapiScope 内部判断），machine-scope blob 直接返回 nil 不打扰；无需"已拒绝"状态文件（拒绝 = master.key 仍 user-scope，下次 unlock 仍会探测到，但 confirmMigrate 的 [y/N] 默认 N 不阻塞，可接受）。

**约束**（同 Plan 14 v0.2.0 迁移）：必须交互 session（RDP/本地）。sshd/非交互 session 读不出 user-scope 旧 blob → 提示"在交互 session 重跑"，**不静默生成新 key**（避免 orphan vault）。

**v2（kimi #7）**：boot 自起的 serve 先于任何交互 unlock 运行，读旧 user-scope blob 失败时，serve 启动失败路径**检测到 user-scope blob 则在 serve.log 写明确迁移指引**（"run 'ssh-manager unlock' in an interactive session to migrate"），不止是"master key unreadable"。

### 5.4 serve install 对象 API（`internal/cli/serve_install_windows.go`，重写）

删 `buildServeTaskXML` + `registerTaskViaPowerShell`。新 `registerTaskViaPowerShell`：

```go
func registerTaskViaPowerShell(in taskInputs) error {
    // 构造 PowerShell 脚本（对象 API），Go 把参数（exe path/addr/user/log path）
**v2 重写（共识 A spike + 共识 C spike + codex #5 TLS + pi #2 spike + opencode #9 引号声明）**：

删 `buildServeTaskXML` + `registerTaskViaPowerShell`。新 `registerTask` 用对象 API，**密码经 Go 读后 stdin 传 PowerShell**（不弹 Get-Credential）：

```go
func registerTask(in taskInputs) error {
    // 1. Go 侧读密码：复用 unlock 的 readPassphrase（golang.org/x/term，TTY 无 echo）。
    //    这绕开 PowerShell Get-Credential / ConvertTo-SecureString 在无头/非交互环境的
    //    整体脆弱性（spike 1：-NonInteractive 下 Get-Credential/ConvertTo-SecureString 的
    //    Microsoft.PowerShell.Security 模块加载/TypeData 不稳）。CI 模式下密码走 env
    //    SSHMGR_SERVE_INSTALL_PASSWORD（非空则跳过 TTY 读，用 env 值）。
    password, err := readServeInstallPassword()  // env 优先，否则 TTY
    if err != nil { return err }

    // 2. PowerShell 脚本：密码 + 参数都经 stdin 传（不进 argv/4688）。
    //    TLS flags（codex #5）：--tls-cert/--tls-key 若非空，拼进 actionArg。
    const ps = `$ErrorActionPreference='Stop'
$lines = [string]::Join("` + "\n" + `", $input)  # stdin: exe|addr|user|logPath|logDir|tlsCert|tlsKey|password
$p = $lines -split "`n"
$exe=$p[0]; $addr=$p[1]; $user=$p[2]; $logPath=$p[3]; $logDir=$p[4]; $tlsCert=$p[5]; $tlsKey=$p[6]; $password=$p[7]
$tlsArg = ''
if ($tlsCert -ne '' -and $tlsKey -ne '') { $tlsArg = ' --tls-cert "' + $tlsCert + '" --tls-key "' + $tlsKey + '"' }
$actionArg = '/C if not exist "' + $logDir + '" mkdir "' + $logDir + '" & "' + $exe + '" serve --addr "' + $addr + '"' + $tlsArg + ' >> "' + $logPath + '" 2>&1'
$action = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument $actionArg
$trigBoot = New-ScheduledTaskTrigger -AtStartup
$trigLogon = New-ScheduledTaskTrigger -AtLogOn -User $user
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -DontStopIfGoingOnBatteries -AllowStartIfOnBatteries
# 注意：-RestartCount/-RestartInterval 在 New-ScheduledTaskSettingsSet (PS 5.1) 不持久化
# (spike 3 实测：注册后 Count=0)。见下方 RestartOnFailure 修复。
$sec = ConvertTo-SecureString $password -AsPlainText -Force
$cred = New-Object System.Management.Automation.PSCredential($user, $sec)
Register-ScheduledTask -TaskName 'ssh-manager-serve' -Action $action -Trigger @($trigBoot,$trigLogon) -Settings $settings -RunLevel Limited -User $user -Password $password -Force | Out-Null
Write-Output "REGISTERED"
`
    // ConvertTo-SecureString 仍在 PowerShell 内（但密码来自 Go stdin，不依赖 Get-Credential
    // 弹窗）。如果 CI 环境 ConvertTo-SecureString 也脆弱，备选：Go 侧 SecureString 构造
    // (DPAPI) 传 PowerShell —— 但先按 stdin + ConvertTo-SecureString 走，CI 真跑验证。
    cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", ps)  // 不用 -NonInteractive（Go stdin 喂密码，不靠 PS prompt）
    cmd.Stdin = strings.NewReader(strings.Join([]string{exe, addr, user, logPath, logDir, tlsCert, tlsKey, password}, "\n"))
    // ... CombinedOutput, 检查 "REGISTERED"
}
```

**参数传递（v2）**：exe/addr/user/log/tls/密码**全部经 stdin**（不进 argv/4688，不靠 PowerShell prompt）。这是共识 A 的解法——Go 读密码（TTY 或 env），PowerShell 只做 Register，凭据处理的环境脆弱性被绕开。

**RestartOnFailure（FINDING D，spike 3 实证不持久化）**：对象 API `-RestartCount/-RestartInterval` 在 PS 5.1 被静默忽略（spike 3：注册后 `Count=0`）。**v2 二选一**（实现时定，spike 已确认现状）：
- **方案 R1（推荐）**：Register 后用 CIM 直接设 —— `Set-ScheduledTask -TaskName X | %{ $_.Settings.RestartOnFailure.Interval='PT1M'; $_.Settings.RestartOnFailure.Count=3; Set-ScheduledTask -InputObject $_ }`（注意 FINDING D 原文说 PS 5.1 RestartOnFailure 是只读 CIM 视图，Set 可能也修不了——实现时实测；若 Set 也不行，走 R2）。
- **方案 R2**：仅 RestartOnFailure 这一字段保留 XML 路径 —— XML schema 的 `<RestartOnFailure Interval="PT1M" Count="3">` 是 Task Scheduler 原生支持、能解析的（Plan 14 C1-C3 bug 在 XML 的 stdin/UTF-16/-Xml 链，不在 RestartOnFailure 字段本身）。对象 API 注册主任务，再用 `schtasks /Change /XML` 或 Register 时混用。

**RestartOnFailure 契约降级（共识 C）**：`Count==3` 从 §10 硬契约改为**目标 + 实证契约**——实现时若 R1/R2 都不行，降级 best-effort（文档标注"RestartOnFailure 在 PS 5.1 对象 API 不可靠，崩溃恢复靠 Boot trigger 间接兜底"+ §10 去掉硬勾选）。serve 是稳定长驻进程，RestartOnFailure 只管运行中崩溃，不是 §7.3 核心（Boot trigger 自起 + 跨重启 DPAPI 才是）。

**多实例契约（pi #2，spike 4 推翻 pi 主张但加防御）**：spike 4 实测 Task Scheduler **默认 `MultipleInstances=IgnoreNew`**（任务在跑时新触发被忽略）—— Boot 起了 serve 后 RDP Logon trigger 触发不会起第二个 serve，7878 不冲突。**pi #2 双 trigger 双起的担心不成立**。但 v2 在 `New-ScheduledTaskSettingsSet` **显式 `-MultipleInstances IgnoreNew`**（防未来默认值改变），spec 钉死这个契约。

**引号坑声明（opencode #9）**：actionArg 仍手拼 `--addr` 进 cmd.exe /C 串。`--addr` 由 `serve install` 的 flag 传入（owner 控制，非外部输入），不含 `"`；spec **明确声明"输入受控、不防引号注入"**。若未来 addr 变外部输入，改用 `[Diagnostics.ProcessStartInfo]` 参数数组。

**v2（kimi #7 / codex #2 连锁）serve install precheck 加 machine-scope 验证**：现有 `serve_install_windows.go:130` precheck 只 `keychain.Get()` 验"能读出"（codex #2 误放行）。v2 precheck 额外读 master.key blob 判断 scope：若仍是 user-scope（迁移未完成），**拒绝 install**，提示"run 'ssh-manager unlock' in an interactive session to migrate to machine-scope first"。否则 user-scope 残留 → install 注册 → boot Password-logon 读不出 → FINDING B 复发（codex #1+#2 连锁）。

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

**process running 误报（v2 opencode #7）**：`serveProcessRunning()` **保持纯进程存在检查**（不并入端口监听——否则 (b) process-alive 和 (c) HTTP-alive 两个独立信号合并，模糊 status 四路设计初衷，代码注释明确"process-alive ≠ vault-unlocked"）。只修"匹配残留进程名"的宽进：要求进程名**精确等于** `ssh-manager`（去掉 `.exe` 子串宽匹配）。进程存在 ≠ 端口监听，但那正是 (c) HTTP probe 的职责——两路有意分开。

**F（陈旧 log，v2 共识 E 改进）**：`vaultUnlockedFromLog` 加时间戳检查，但**配合 serve 心跳**避免"健康但空闲误判 unknown"：
- **serve 加 heartbeat**：serve 进程每 ~1 分钟写一行心跳到 serve.log（如 `heartbeat: still listening on 0.0.0.0:7878 at <ts>`）。这让健康但无请求的 serve log 仍新鲜。
- `vaultUnlockedFromLog` 时间戳检查：log mtime > 5min 判 stale（心跳是 1min，5min 阈值给 4 次心跳冗余）。
- **staleness 仅作降级提示，不当否定**：log stale 时报 `vault: unknown (log stale >5min; current state unknown)`，但 (a) task + (b) process + (c) HTTP 三路若全绿，overall 仍可判 HEALTHY（log 只作辅助）。这避免"必然过期的信号废了第四路"（共识 E）。

```go
func vaultUnlockedFromLog() (unlocked bool, note string) {
    info, _ := os.Stat(logPath)
    if time.Since(info.ModTime()) > 5*time.Minute {
        return false, " (log stale >5min; current state unknown)"  // 仅降级提示
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
- **密码变更（v2 修正 kimi #2 事实错误）**：machine-scope 用 `DPAPI_SYSTEM` LSA secret（机器级，**不依赖用户 DPAPI Master Key**）。**所以 admin 强制重置用户密码不影响 machine-scope master.key 解密**（与 user-scope 相反——user-scope admin reset 会断）。v1 写"machine-scope 同样依赖用户 Master Key, admin reset 会断"是**事实错误**，v2 改：machine-scope 对密码重置**免疫**，代价是"本机任何能读到文件的用户上下文（SYSTEM/admin/同 ACL 用户）可解"。
- **master.key ≠ 备份（不变）**：machine-scope 也绑机器，换机/重装即废物（machine key 不跟随）。灾备仍是 Plan 11 export 信封 + Plan 13 NAS 备份。
- **Task Scheduler 密码（v2 改）**：密码经 Go `readPassphrase`（TTY）或 env `SSHMGR_SERVE_INSTALL_PASSWORD`（CI）读，stdin 传 PowerShell 的 `Register-ScheduledTask -Password`。**不用 Get-Credential**（避开无头/非交互环境的脆弱性，spike 1）。不进 argv/4688。
- **L2 模型 / iron rule 不变**。

---

## 7. 测试

### 7.1 单元测试
- **`dpapi_windows_test.go`**（build windows）：machine-scope roundtrip（`dpapiProtect(p,true)` → `dpapiUnprotect(b,true)`）；user-scope roundtrip（`machine=false`）；**v2 spike 2 修正断言**：machine protect + user unprotect **互通**（不失败——DPAPI blob 内嵌 scope 元数据，解密从 blob 判断用哪把 key，flag 不强制隔离）。单测断言"跨 scope 能解"（反映 spike 2 实测），而非 v1 的"必失败"。
- **`masterkey_windows_test.go`**：DpapiKeyProvider Set(machine) → Get 成功；**Get 的 user-scope fallback**（构造旧 user-scope blob，Get 能读出）；**v2 ACL 契约（pi #3）**：Set 后 master.key 的 ACL 只含 allan716 + SYSTEM，无 Everyone/Users（断言 temp 在 protectedDir 内继承正确 ACL）。
- **迁移测试**（`unlock_windows_test.go`）：构造旧 user-scope master.key → unlock 触发 → machine-scope 重 protect；拒绝迁移 → 正确提示；sshd/非交互 session 读不出旧 user-scope → 提示重跑（不生成新 key）。
- **serve install 参数构造测试**：测传给 PowerShell 的 action 命令行、trigger（Boot+Logon）、RunLevel=LeastPrivilege（对应 -RunLevel Limited）、RestartCount=3（契约断言）。

### 7.2 集成测试（CI windows-latest）—— **根因修复，必须真跑**
- **install → status → uninstall round-trip**：
  0. **v2 共识 B vault seed（CI 前置，v1 漏）**：CI job 内非交互初始化 vault —— `SSHMGR_MASTERKEY_HEX=<fixed-test-key> ssh-manager unlock`（env 优先，生成空 store.db + machine-scope master.key）→ `ssh-manager servers add ...` 放 1 台测试 server + 凭据。否则全新 runner 上 step 5 "vault ok" 永远验不成。
  1. `ssh-manager serve install`（CI 通过 `SSHMGR_SERVE_INSTALL_PASSWORD` env 提供账户密码 —— **v2 共识 A 解法**：Go readServeInstallPassword 读 env，stdin 传 PowerShell，不弹 Get-Credential）。CI 里 `net user sshmgrci <password> /add` + `net user sshmgrci /passwordreq:no`（不过期）+ `secedit` 授 SeBatchLogonRight（pi #1 补充）。
  2. 验 task 注册（`Get-ScheduledTask.State == Ready`）
  3. 验 `Settings.MultipleInstances == IgnoreNew`（v2 pi #2 防御契约）
  4. `schtasks /Run` → 等 serve 起 → HTTP `localhost:7878` 返回 401（鉴权工作）
  5. **验 vault ok**（machine-scope master.key 在 serve 进程能解 store.db）。**v2 opencode #2 断言降级**：这只证"machine-scope 在 task session 可解"，**不证"user-scope 是 bug"**（CI 同用户同机刚 machine-scope 加密，trivially 通过）。FINDING B 的真正闭环（跨 logon session）只能靠 §7.3 NUC10 reboot 手动验。**CI 测试定位为"machine-scope 回归测试 + serve install 注册路径验证"，不是 FINDING B 验证**。
  6. `serve status` 四路（task/process/http/vault），`overall: HEALTHY` 或 `DEGRADED` 但四路语义正确（log stale 降级不算 fail）
  7. `serve uninstall` → task 删除 + 进程停
- **CI workflow**：`.github/workflows/serve-install-windows.yml`，`on` push/PR 改 `internal/cli/serve_install*.go` 或 `internal/store/dpapi*.go`/`masterkey*.go` 时跑；windows-latest runner。**不再 gated** —— FINDING C 根因（gate 从没跑过）的直接修复。账户密码 + SeBatchLogonRight 都在 job 内 `net user`/`secedit` 动态建（GitHub-hosted runner 每次全新，无持久账户）。
- **reboot 自起留手动 runbook**（CI 不能 reboot）：docs 里写 NUC10 真机 reboot 验证步骤（Plan 14 §7.3 同款），作为 release 前的人工 checklist 项。**FINDING B 的闭环验证在这里**（跨重启 DPAPI 可解），不在 CI。

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
| `internal/store/dpapi_windows.go`（改）| `dpapiProtect`/`dpapiUnprotect` 加 `machine bool` 参数，按 scope 选 flag（spike 2 确认 flag 不强制隔离，仅"优先尝试的 scope"）|
| `internal/store/masterkey_windows.go`（改）| `DpapiKeyProvider.Set` 用 machine-scope + **ACL 契约**（temp 必须在 protectedDir，pi #3）；`Get` 双 scope 尝试（machine 主 + user fallback，临时容错）|
| `internal/cli/unlock.go`（改，**v2 codex #1 新增触点**）| 加 `postGetMigrator` package var（类比 `firstRunMigrator`），在 **Get 成功后、printMasterKey 前**调用；否则迁移不可达（v1 硬伤）|
| `internal/cli/migrate_windows.go`（改）| 新增 `migrateDpapiScope`（user-scope → machine-scope 重 protect，复用 v0.2.0 迁移的 confirmMigrate 一致性策略）；`init()` 赋值 `postGetMigrator = migrateDpapiScope` |
| `internal/cli/serve_install_windows.go`（重写）| 删 `buildServeTaskXML` + `registerTaskViaPowerShell` 的 XML 链；对象 API 注册（密码 Go readPassphrase→stdin→Register-ScheduledTask -Password，共识 A）；**precheck 加 machine-scope 验证**（codex #2）；RestartOnFailure R1/R2（共识 C）；status 用 Get-ScheduledTask/Info（E）；process 精确匹配不并入 HTTP（E opencode #7）；vault-ok 陈旧检查 + serve heartbeat（F 共识 E）；MultipleInstances=IgnoreNew 防御契约（pi #2）；TLS flags 保留（codex #5）|
| `internal/cli/serve_install_windows_test.go`（改）| 单测从 XML 字符串改成对象 API 参数契约；RestartOnFailure 持久化断言改为目标-非硬契约（共识 C）；MultipleInstances=IgnoreNew 断言 |
| `internal/cli/serve.go`（改）| serve 进程加 ~1min heartbeat 写 serve.log（共识 E）|
| `internal/store/*_test.go`（改）| DPAPI machine-scope roundtrip + **跨 scope 互通**（spike 2 断言改）；DpapiKeyProvider 双 scope Get + ACL 契约（pi #3）；迁移测试 |
| `.github/workflows/serve-install-windows.yml`（新）| CI windows-latest 真 run §7.2 集成测试（含 vault seed step 0 + net user 建账户 + env 密码）|
| `docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`（**v2 opencode #8 改**）| **正文不改写**（保留审计轨迹）。仅在文档**顶部加 "⚠ Superseded by Plan 15 (machine-scope DPAPI + serve install fix); see docs/superpowers/specs/2026-08-12-plan-15-...md" 横幅**。Plan 14 的 §3.2/§5.3/§5.8/§6/§7.2 原文留着，读者看横幅跳 Plan 15 |
| `docs/backup-restore.md` + `docs/multi-machine.md`（改）| machine-scope 威胁模型（对密码重置免疫 kimi #2）+ ACL 兜底 + user→machine 迁移 runbook |

---

## 9. 未来工作（显式 deferred）

- **Linux/macOS serve install**（systemd --user / launchd）——仍 deferred（Plan 14 §3.4）。
- **reboot 自起的 CI 自动验证**——需要能 reboot 的 CI 环境（Windows container / self-hosted runner），本次手动 runbook。
- **多用户共享 vault**——machine-scope 已支持（任何同机用户能解 + ACL 放权），但 per-user ACL 策略未做，留专门 plan。
- **serverInfo.version 硬编码**（Plan 9 land-later）。

---

## 10. 落地前 checklist（v2）

- [ ] DPAPI machine-scope roundtrip + **跨 scope 互通**（spike 2 断言）
- [ ] DpapiKeyProvider machine-scope Set / Get（+ user-scope fallback）+ **ACL 契约**（temp 在 protectedDir，pi #3）
- [ ] **unlock.go postGetMigrator 钩子**（codex #1）+ migrateDpapiScope（交互 session，不 orphan）
- [ ] cache-dek 也改 machine-scope（复用 confirmMigrate 一致性）
- [ ] serve install 对象 API（删 XML 链）+ **密码 Go readPassphrase→stdin→Register-ScheduledTask -Password**（共识 A，不用 Get-Credential）+ 参数经 stdin 不进 argv
- [ ] **serve install precheck 验 machine-scope**（codex #2，拒绝 user-scope 残留）
- [ ] **RestartOnFailure**：R1（CIM Set）或 R2（XML 字段）二选一 + **契约降级为目标非硬契约**（共识 C，spike 3 确认对象 API 不持久化）
- [ ] **MultipleInstances=IgnoreNew 显式契约**（pi #2 spike 4 防御）
- [ ] **TLS flags 在对象 API 保留**（codex #5）
- [ ] serve status 用 Get-ScheduledTask/Info（本地化修复）+ process 精确匹配（不并入 HTTP，opencode #7）+ vault-ok 陈旧检查
- [ ] **serve heartbeat ~1min 写 log**（共识 E）
- [ ] CI serve-install-windows.yml 真 run §7.2（不再 gated）+ **vault seed step 0**（共识 B）+ **net user 建账户 + env 密码 + SeBatchLogonRight**（共识 A/pi #1）
- [ ] Plan 14 spec **顶部加 Superseded 横幅**（opencode #8，正文不改）
- [ ] docs 升级 runbook（user→machine 迁移）+ ACL 兜底 + **machine-scope 对密码重置免疫**（kimi #2）
- [ ] NUC10 §7.3 重做（unlock 迁移 → serve install → reboot → 自起 → 笔记本 exec）—— **FINDING B 闭环验证在这里，不在 CI**

---

## 11. 参考

- Plan 14 spec：`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`
- Plan 14 §7.3 NUC10 E2E findings：`docs/superpowers/specs/2026-08-12-plan-14-nuc10-e2e-findings.md`（FINDING B/C/D/E/F）
- 过程数据：`.omc/state/p14-nuc10-e2e-2026-08-12.md`（gitignore，按时间序证据链）
- DPAPI spike（Plan 14 §12，结论被推翻）：user-scope roundtrip 假阳，跨 logon session 失败
- 验收时对象 API 绕法脚本：`rdp-B8-object-api.ps1`（gitignore，Plan 15 正式化的基础）
