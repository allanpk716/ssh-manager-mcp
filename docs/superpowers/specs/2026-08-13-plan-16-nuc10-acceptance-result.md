# Plan 16 §7.3 NUC10 真机验收结果

> **2026-08-13 修复状态更新**：F1（store.db ACL 缺用户 ACE）已修 — `b7e3ac5`（HardenACL 只在首次创建时跑，serve 重开不重写）。F2 已修 — `5e1ec15`，**且 F2 根因修正**：不是"CLI 并发写 audit 撞锁"，而是 SQLite WAL 边车（`-shm`/`-wal`）的 ACL 不对（LocalSystem 创建时继承上层 ProgramData 的 Admins-read-only DACL，CLI 经 Administrators 写 `-shm` 失败）；`hardenWALSidecars` 在每次 store.Open 时 HardenACL 边车文件，真机验证 serve 跑着时 CLI `servers ls` 返回 7（不再 readonly）。F3 已订正 — spec §1.1 加 F3 注记（sshd 实测能解 DPAPI，读不出的是 serve/Service session）。F1/F2/F3 全部闭环。详见下文各 finding 段的"修复"小节。

**日期**：2026-08-13
**验收对象**：Plan 16（fixed path + FileKeyProvider），二进制 `v0.3.0-rc-acceptance`，master `585cc24`
**方式**：全自动化 SSH（用户未 RDP，未手动操作）— export→import 绕开 DPAPI RDP 墙

---

## ✅ 核心命题验证通过

**Plan 16 的核心赌注**：boot 自起的 serve（跑在 LocalSystem 账户的 Windows Service）能读 `C:\ProgramData\ssh-manager\master.key.plain`（纯文件读，无 DPAPI）。

**reboot 后铁证**（2026-08-13 20:25:58 重启，20:27 验证）：

| 信号 | 值 |
|---|---|
| service | Running（StartMode=Auto, StartName=LocalSystem）|
| process | running |
| http | responding (401/200 = auth working) |
| **vault** | **ok** ★（boot 后 LocalSystem 读 master.key.plain 成功 + 解密 vault）|
| overall | HEALTHY |
| 7878 | LISTENING |

**对比 Plan 14/15 同关卡**：两者都在 boot/serve 读 master.key 时死（`Key not valid for use in specified state`，DPAPI 跨 session 读不出）。Plan 16 换"明文文件 + 固定路径 + ACL" → 纯文件读，零 session 依赖，可预测可测可自动化。

## §7.2 通过标准 — 全满足

| spec §7.2 标准 | 结果 |
|---|---|
| reboot 后 vault: ok（不是 LOCKED）| ✅ |
| serve 自起（process running）| ✅ |
| 7878 listening | ✅ |
| machine-scope master.key 跨重启可读 | ✅（且已改为明文文件，不再依赖 DPAPI）|

---

## 验收执行过程（全自动化）

| Phase | 方式 | 结果 |
|---|---|---|
| 1 部署 Plan 16 exe | SSH scp + 备份旧 exe | ✅（备份 .plan15-bak）|
| 2 数据迁移 | SSH 跑：旧 exe `export .sme`（DPAPI 解出）→ Plan 16 exe `import .sme`（明文 key E 重加密）| ✅ 7 servers / 7 credentials，N/N 自检 |
| 2 关键发现 | SSH session **能解** machine-scope DPAPI（`servers ls` 通）— 推翻了 spec §1.1 "sshd 读不出"的部分假设；但 export→import 更稳，绕开所有 DPAPI 不确定性 |
| 3 serve install | SSH 非管理员（allan716 在 Administrators）→ kardianos Windows Service | ✅ 不需管理员，不需 `--task-user`，不需密码（Plan 15 的密码坑消失）|
| 3 ACL 硬化 | master.key.plain ACL = `D:PAI(SYSTEM+Admins FullControl + allan716 RW)`，继承禁用，无 broad groups | ✅ 与 T6 单测 SDDL 完全一致 |
| 4 reboot + 验证 | `shutdown /r /t 30` 远程触发，等 150s SSH 回来 | ✅ vault: ok（见上）|

**用户零手动**：全程 SSH 自动化，唯一中断是 reboot 等待（2-3 min）。Phase 5（agent exec）需 project token，未测（见下）。

---

## ⚠️ 发现的问题（不阻塞 v0.3.0 发版，需 follow-up）

### F1（重要，T10 回归）：store.db ACL 缺当前用户 ACE

`C:\ProgramData\ssh-manager\store.db` 的 ACL = `D:PAI(A;;FA;;;SY)(A;;FA;;;BA)` — 只有 SYSTEM + Administrators，**没有 allan716（当前用户）ACE**。

对比 master.key.plain = `D:PAI(...)(A;;0x13019f;;;S-1-5-21-...-1000)`（有当前用户 ACE）。

根因：T10 fix 在 `store.Open` 里调 `HardenACL`，但 HardenACL 取"当前用户"的逻辑在 store.Open 路径（import 进程）里没拿到 allan716，导致 store.db 的 DACL 少了用户 ACE。

影响：
- allan716 在 Administrators 组，通过组成员身份有 FullControl — 所以**功能上仍可读写**（serve 跑得好好的）
- 但**不符合 spec §5.2 承诺**（"master.key + store.db + cache-dek.key 同 ACL"）
- 若未来 allan716 不在 Administrators（多用户机），CLI 会读不了 store.db

修法：`store.Open` 调 HardenACL 时确保取到当前用户 token 加 ACE（同 FileKeyProvider.Set 的做法）。

### F2（中，预存非 Plan 16 引入）：CLI 并发写 audit 撞 SQLite 锁

serve 跑着时（持有 store.db WAL 写锁），本地 CLI 跑 `projects ls` / `serve stop` 等会**写 audit** → 第二个进程尝试写 → `attempt to write a readonly database (8)`。

- `serve status`（只读，不写 audit）→ 通
- 任何写 audit 的 CLI → 撞锁

这是 SQLite WAL 多进程并发的固有行为（Plan 14/15 也有），不是 Plan 16 引入。但影响"serve 跑着时本地 CLI 管理 vault"的体验。

修法（可选）：CLI 检测到 serve 在跑时，audit 走 sidecar（offline-cache 已有的机制）或跳过 audit 写；或文档说明"serve 跑时用 MCP 接口而非本地 CLI"。

### F3（观察）：machine-scope DPAPI 在 sshd session 实际可解

spec §1.1 触发事实写 "machine-scope DPAPI 在 sshd session 读不出"。但本次验收用旧 Plan 15 exe 在 SSH 跑 `servers ls` **成功**（7/7 列出），说明 sshd session **能解** machine-scope DPAPI。

回忆 Plan 15 §7.3 失败的是 **serve 前台进程**读（B3 关卡），不是 `servers ls`。可能：
- sshd session 与 serve（Task Scheduler /Run 的 Password-logon session）DPAPI 上下文不同
- 或当时是 transient 状态

这不改变 Plan 16 方向（boot Service session 的 DPAPI 行为仍不可预测），但说明 spec §1.1 的诊断可更精确（"serve/Service session 读不出"而非"sshd 读不出"）。文档可订正。

---

## v0.3.0 发版判定

**spec §7.2 通过标准全满足 → 可发 v0.3.0。**

F1/F2/F3 均不阻塞发版（F1 功能不影响因 Administrators 兜底；F2 预存；F3 文档订正）。建议：
1. **先发 v0.3.0**（Plan 16 + §7.3 通过）
2. follow-up 修 F1（store.db ACL）— 这是 T10 真回归，应尽快修
3. follow-up 评估 F2（CLI 并发 audit）
4. 文档订正 F3（§1.1 sshd vs serve session 措辞）

## Phase 5（agent exec）未测

需 project token（serve 跑着时 CLI 拿不到，F2）。可后续测：停 serve → CLI 拿 token → 起回 serve → 笔记本 .mcp.json 配 token → MCP exec_command 在 1660Super01 跑 `hostname`。非发版阻塞（spec §7.2 通过标准已满足）。

---

## 2026-08-13 更新：Phase 5 端到端已测 — 通过

F2 修复后（`5e1ec15`，serve 跑着时 CLI 能读），Phase 5 全自动化完成：

1. **拿 token**：`projects add phase5-e2e --profile e2e-profile`（serve 跑着，CLI 经 F2 fix 读 WAL 成功）→ 拿完整 project token
2. **MCP handshake**（笔记本 curl NUC10 serve，HTTP Streamable MCP）：`initialize` → `Mcp-Session-Id` → `notifications/initialized`（202）→ `tools/list`（拿到 exec_command / list_servers / download_file / upload_file / forward_port / close_port）
3. **exec_command**：`tools/call exec_command {server_id: 1660Super01, command: hostname}` → **`exit_code:0, stdout: DESKTOP-UP1MHGT`**

**完整端到端链路打通**：笔记本 → HTTP MCP → NUC10 serve（reboot 后 LocalSystem 自起）→ SSH → 1660Super01 执行。全程 SSH 自动化，用户零手动（含 Phase 4 reboot 用 `shutdown /r` 远程触发 + sleep 等）。

测后清理：`phase5-e2e` project 已 revoke（token 失效，401 确认）；本地 token 残留扫描 clean（误报已排查：grep 管道 exit code 假阳性）。

**§7.3 五个 Phase 全过**（部署 / 迁移 / serve install / reboot / agent exec）。

