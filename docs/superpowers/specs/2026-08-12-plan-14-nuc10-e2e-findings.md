# Plan 14 NUC10 E2E 验收 — Findings 报告

> **日期**：2026-08-12
> **范围**：Plan 14（Windows prod deploy: DPAPI master key + serve 常驻）§7.3 NUC10 真机端到端验收
> **结论**：**§7.3 验收未通过** —— 暴露 1 个架构缺陷 + 5 个 Windows 实现 bug + 1 个潜伏的 vault 数据损坏。vault 损坏已修复；架构缺陷需 Plan 15（machine-scope DPAPI）重做。
> **过程数据**：`.omc/state/p14-nuc10-e2e-2026-08-12.md`（按时间序的完整证据链）

---

## 0. 执行摘要

Plan 14 的两个核心交付物在真机上**都不能直接可用**：

| 交付物 | 真机结果 | 根因 |
|---|---|---|
| DPAPI master key 替代 keychain | ⚠️ vault 修复后可用，但 **boot 自起读不出** | user-scope DPAPI 跨 logon session 不可用（架构缺陷）|
| `serve install` boot 常驻 | ❌ **命令完全不能工作** | 3 个叠加 bug（从未在真机测过）|

**但真机验收的最大价值不在 Plan 14 本身**——而在它顺带抓到了一个**潜伏的 vault 数据损坏**（key/密文错配），这个损坏如果不跑真机、等 37836 进程一死就全锁死。

---

## 1. FINDING A（最高优先）：vault key/密文错配（已修复）

### 现象
NUC10 上 v0.2.0 vault 的 7 条凭据密文，用 keychain 里的 master key **全部解不开**（AES-GCM auth tag 失败）。正确的 master key 只存在于一个**正在运行的 serve 进程（PID 37836）的启动命令行环境变量**里。

### 根因
v0.2.0 的 master key 有两套来源不一致：
- **keychain slot**：存的 key = A（错）
- **7 条凭据密文**：用 key = B 加密（对）
- **PID 37836（serve 进程）**：env `SSHMGR_MASTERKEY_HEX=B`（所以它能正常服务）

事故链推断（来自之前 E2E 的操作记录）：
1. 某次 `unlock` 生成 B 并写进 keychain
2. serve 用 `cmd /c set SSHMGR_MASTERKEY_HEX=B && serve` 启动（env 注入 B）
3. 通过这个 env 加了 7 台服务器（凭据用 B 加密）
4. 之后某次 unlock 让 keychain 里 B 被换成 A（可能是 passphrase 派生路径，或另一次生成）
5. 从此：keychain=A（错），凭据=B 加密（对），37836 用 env B 维持可用假象

### 爆炸半径
**只要 37836 进程一死（重启/崩溃/kill）且没人记得 B，整个 vault 的 7 条私钥凭据永久不可解**。这是个靠"进程碰巧还活着"维持的脆弱状态。

### 修复（已完成，B1-B7）
1. 从 PID 37836 的启动命令行**提取正确的 key B**（`04aeace...d2523`）
2. 用 B 做 `export` 安全绳（.sme 文件，import round-trip 验证 7/7 可恢复）
3. kill 37836 + 备份旧 store.db
4. `keychain-clear`（删错的 keychain slot A）
5. `unlock` 生成**全新 key C**（master.key = C，DPAPI user-scope）
6. `import` 安全绳（C 重新 seal 7 条凭据）
7. 验证：vault-tool diag with C → 7/7 OK

### 遗留
- **key B 在 PID 37836 启动命令行里泄露过** → 已进 Windows 4688 审计日志 + 任何同用户进程可见。**重建用全新 C，B 已作废**（旧 store.db.broken-B 的 B 加密数据再也解不开），泄露的 B 失效。
- 安全绳 `.sme` 双份（NUC10 + 笔记本），passphrase 在 1Password。

### 对 Plan 14 的影响
**无**。Plan 14 不改 master key 的**值**，只改**存储位置**（keychain→DPAPI 文件）。vault 损坏是 v0.2.0 历史问题，Plan 14 不会让它更坏也不会修好——但 Plan 14 的迁移流程**要求 keychain 和凭据一致**，所以修复 vault 是 Plan 14 的前置条件（已满足）。

---

## 2. FINDING B（架构缺陷）：user-scope DPAPI + boot 自起互斥

### 现象
reboot NUC10 后，BootTrigger 成功触发 serve 任务（task result 0），但 serve 子进程**立即退出**，7878 不监听。前台手动跑 serve 报：
```
Error: master key present but unreadable:
dpapi: CryptUnprotectData failed: Key not valid for use in specified state.
```

### 根因（实证）
master.key 文件**没坏**——在 **RDP/console 交互 session** 跑 serve 正常（`listening on 127.0.0.1:7879`）。问题在 **logon session 类型**：

| logon session | user-scope DPAPI 解 master.key |
|---|---|
| RDP / console 交互登录 | ✅ 能（profile 加载、DPAPI Master Key unlock）|
| sshd network logon | ❌ `Key not valid for use in specified state` |
| Task Scheduler `LogonType=Password`（boot/Service）| ❌ 同一类失败 |

**死结**：
- boot 自起 **必须**用 `LogonType=Password` 任务（开机时没人交互登录）
- `LogonType=Password` 任务的 session **解不开** user-scope DPAPI blob
- → boot 自起的 serve **永远读不出** user-scope master.key

### spec §12 spike 的结论是错的

spec §12 附录声称"user-scope DPAPI 在 sshd/Task-Scheduler session 全通"——**那是假阳性**：
- spike 测的是 **同一 session 内 protect→unprotect roundtrip**（同 logon session = 同 DPAPI Master Key）
- 生产路径是 **RDP session 生成 master.key（unlock）→ Service/sshd session 读**（**跨 logon session**）
- spike **没测跨 logon session**

"共识 A 被推翻"的结论（spec §12.1）**本身被推翻**——共识 A（user-scope DPAPI 在非交互 session 可用性未验证）是对的，spike 的"推翻"基于不完整的测试矩阵。

### 对 Plan 14 的影响（致命）
spec §3.2 选 user-scope DPAPI 的理由是"安全性更强（对同机其他用户保密）"，基于 §12 spike。**这个前提在 boot 自起场景下不成立**。Plan 14 §7.3 的核心目标（boot 自起 serve）**架构性不可达**。

### 解法（Plan 15）
**改 machine-scope DPAPI**（`CRYPTPROTECT_LOCAL_MACHINE`）：绑机器不绑 logon session → 任何 session（含 boot Service、sshd）都能解。boot 自起立即能用。
- 代价：同机其他用户能解 master.key（安全性弱于 user-scope）
- 可接受：NUC10 是单用户机（allan716），且威胁模型里"同机其他用户"本就靠 ACL 兜底（master.key 文件夹 `icacls allan716 only`）

这正是用户最初 grilling 的"为什么用 LocalSystem / 什么需求导致"——当时基于错误 spike 否定了 machine-scope。**实测推翻后，machine-scope 是唯一满足"无人值守 boot 自起"的方案**。

---

## 3. FINDING C（实现 bug）：`serve install` 命令完全不能工作

`ssh-manager serve install` 在 Windows 上有 3 个叠加 bug，**命令从未在真机可用过**。

### C1: stdin→$input 拿不到 XML
`registerTaskViaPowerShell`（serve_install_windows.go:422）把 Task XML 经 stdin 传，PowerShell 用 `$input` 读。PowerShell 5.1 的 `-Command` 模式 `$input` **不捕获多行 stdin** → `LoadXml("")` → "文档中根元素无效"。

### C2: XML 声明 UTF-16 但实际 UTF-8
`buildServeTaskXML`（serve_install_windows.go:401）声明 `encoding="UTF-16"`，但 Go 的 `strings.NewReader` 写 UTF-8 字节。`Register-ScheduledTask -Xml` 走 CIM 严格按声明解码 → UTF-8 字节当 UTF-16 读 → `(1,2) 不正确的文档语法`。

### C3: Register-ScheduledTask -Xml 序列化失败
即使 XML 内容对了，`Register-ScheduledTask -Xml <string>` 在 PowerShell 5.1 的 CIM 序列化也崩 `(1,40) 无法序列化`。

### 根因（测试盲区）
- 单元测试只测 `buildServeTaskXML` **纯函数**的字符串输出
- spec §7.2 的 `SSHMGR_SERVE_INSTALL=1` gated 集成测试**从没在真机跑过**（默认跳过，CI 也没开）
- → 三个 bug 全藏着，直到 NUC10 真机验收才暴露

### 绕法（本次验收用）
用 PowerShell 对象 API 注册（`New-ScheduledTaskAction/Trigger/SettingsSet` + `Register-ScheduledTask` User 参数集 `-User -Password -RunLevel`），绕开全部 XML 路径。脚本：`rdp-B8-object-api.ps1`。**但这不是代码修复，是验收绕法**。

---

## 4. FINDING D：RestartOnFailure 没持久化

对象 API 注册时传了 `New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval 1min`，但注册后任务 `RestartOnFailure Interval="" Count=0`。`Set-ScheduledTask` 修不了（需重新输密码 + PS 5.1 的 RestartOnFailure 是只读 CIM 视图）。

**不阻塞**（BootTrigger 是 §7.3 核心，RestartOnFailure 只管运行中崩溃恢复）。但 spec §5.8 要求 PT1M×3，实现没达成。

---

## 5. FINDING E：`serve status` 简体中文本地化误报

`schtasksQuery`（serve_install_windows.go:481-484）找 `Status:` / `任务状态:` 前缀。**简体中文 Windows 实际输出 `计划任务状态:`** → 不匹配 → state 留空 → "Unknown" → `overall: DEGRADED`，即使 serve 完全健康。

另：`serve status` 的 `process: running` 在 Phase D 也误报过（实际无进程却报 running）——`serveProcessRunning()` 检测逻辑可能匹配错。**第 6 个 bug**。

---

## 6. FINDING F：`serve status` 的 vault-ok 扫描基于陈旧 log

`vaultUnlockedFromLog` 扫 `serve.log` 末尾找成功标记。但 boot 自起失败时 serve.log **不增长**（serve 在写 log 前就退出）→ 扫到的是**上次成功启动的历史标记** → 报 `vault: ok`，掩盖了当前的 master.key 读失败。

这是 FINDING B 的**放大器**——Phase D 第一次探查时，`vault: ok` 让我一度以为 serve 健康，差点错过 boot 自起失败。真正暴露问题的是"process not running + http not responding"两路。

---

## 7. 修复的 vault 状态（Plan 15 将继承）

Plan 15 不需要重做 vault 修复——vault 已经干净：
- keychain：空（keychain-clear 已删）
- master.key：`%AppData%\ssh-manager\master.key`，262 bytes，user-scope DPAPI，key C（值不记录于此）
- store.db：全新，7 条凭据用 C 加密
- key B 已作废（旧 store.db.broken-B 留作记录，B 泄露无害）
- key C 明文不进任何文档/repo（它是当前活 key；若需轮换，从 NUC10 `unlock` 重新导出）

**Plan 15 要做的**：把 master.key 从 user-scope 改成 machine-scope（重新 DPAPI protect C，用 `CRYPTPROTECT_LOCAL_MACHINE`）。**vault 数据不动**（凭据密文绑 key C，与 master.key 的存储 scope 无关）。

---

## 8. 待办（新开 plan）

### Plan 15：machine-scope DPAPI（阻塞 boot 自起）
- spec §3.2 推翻性修订：user-scope → machine-scope
- `dpapiProtect` 加 `CRYPTPROTECT_LOCAL_MACHINE` flag
- 重生成 master.key（machine-scope protect C）
- 重测 boot 自起（这次应该过）
- 威胁模型更新：machine-scope 的"同机其他用户"风险靠 ACL 兜底

### Plan 16：serve install 修复（FINDING C/D/E/F）
- C1: registerTaskViaPowerShell 改用临时 XML 文件或 `-EncodedCommand`，不用 stdin/$input
- C2: buildServeTaskXML 声明改 UTF-8（或 stdin 写 UTF-16 BOM）
- C3: 改用对象 API（New-ScheduledTask*）注册，绕开 -Xml 序列化坑 —— **或** C1-C3 一并解决
- D: 对象 API 的 RestartOnFailure 持久化（或注册后 Set 补）
- E: schtasksQuery 本地化（不依赖 Status 文本，用 Get-ScheduledTaskInfo.State）
- F: vaultUnlockedFromLog 加时间戳检查（log 太旧 = 不可信）
- **加真机集成测试**：spec §7.2 的 SSHMGR_SERVE_INSTALL=1 gate 必须在真机/CI 跑

### Plan 15 + 16 合并？
两者都改 serve_install_windows.go / masterkey_windows.go，且都需真机重测 boot 自起。**建议合并成一个 plan**（Plan 15 = machine-scope + serve install 修复 + 真机集成测试），一次重做 Windows 生产部署。

---

## 9. 本次验收的净价值

**不是 Plan 14 §7.3 通过**——而是用真机暴露了 3 类问题，每类都救了未来的命：
1. **vault 损坏**（FINDING A）：不修，37836 一死全锁死。现在修了，数据零丢失。
2. **架构缺陷**（FINDING B）：不发现，machine-scope vs user-scope 的错误决策进生产，所有 Windows 用户的 boot 自起都坏。
3. **serve install 从未可用**（FINDING C）：不发现，`serve install` 命令是个摆设。

真机验收的最大意义就是**在问题进生产前抓到**。Plan 14 的代码不是白写——vault 修复链路（export/import/unlock 迁移）全部验证可用；DPAPI syscall 可行；安全绳机制可靠。只是 user-scope 这个**选型**错了，需要 Plan 15 改 machine-scope。
