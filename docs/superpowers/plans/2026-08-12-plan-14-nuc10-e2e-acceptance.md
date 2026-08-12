# Plan 14 NUC10 真机 E2E 验收计划（含回退）

> 不是代码实现 plan，是 **§7.3 端到端验收的操作 runbook + 回退预案**。
> 对应 spec：`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md` §7.3 / §10。
> master 已在 `a00f5c0`（Plan 14 全部上线）。本计划验证它真的能在 NUC10 生产环境跑起来。

**目标**：在 NUC10（真实 vault + 真实目标机）端到端验证 Plan 14 —— DPAPI master key 迁移 + serve 常驻 + 重启自起 + 笔记本 agent 远程 exec。收集过程数据。出问题时能回到部署前状态。

---

## 0. 范围 / 假设 / 不触碰清单

### 0.1 已知事实（来自 spec / docs / 之前 E2E）

| 项 | 值 | 来源 |
|---|---|---|
| NUC10 IP | `192.168.100.235` | spec §7.3 |
| NUC10 用户 | `allan716` | docs / spec |
| NUC10 OS | Win10 19045（`wmic` 可用） | docs backup-restore.md |
| 现状版本 | v0.2.0（master key 在 Credential Manager keychain） | 之前 E2E FINDING 9 |
| store.db 路径 | `%AppData%\ssh-manager\store.db` | docs |
| 新 master.key 路径 | `%AppData%\ssh-manager\master.key` | spec §5.4 |
| 新 cache-dek.key 路径 | 同目录 `cache-dek.key` | spec §5.7 |
| serve.log | `%LocalAppData%\ssh-manager\serve.log` | spec §5.8 |
| Task 名 | `ssh-manager-serve` | spec §5.8 |
| 监听端口 | `7878` | docs |
| 测试目标机 | `1660Super01` → 期望 `hostname` = `DESKTOP-UP1MHGT` | spec §7.3 |
| 其他受影响服务 | DocuFiller UpdateHub 等（重启影响） | 之前对话 |

### 0.2 不触碰清单（零风险保证）

- **NUC10 其他服务**（DocuFiller UpdateHub 等）：只有 reboot 会影响它们，reboot 时机由用户选（§6）。本计划不做任何影响其他服务的操作。
- **笔记本的本地 vault**（若有）：笔记本侧只改 `.mcp.json`（且先备份），不碰笔记本的 `store.db`。
- **生产 vault 的数据内容**：export 是只读；迁移是 in-place 但有 export 兜底（见 §2）。
- **v0.2.0 二进制**：不覆盖，改名为 `.v0.2.0.bak` 保留，可换回。

### 0.3 依赖的外部条件（执行前确认）

- [ ] NUC10 当前可 SSH（recon 用）+ 可 RDP/本地控制台（迁移 + serve install 用）。
- [ ] allan716 的 Windows 密码已知（serve install 的 Get-Credential 要弹窗输）。
- [ ] 笔记本能连通 NUC10:7878（VLAN 内）。
- [ ] 1660Super01 在线、凭据在 vault 里、profile 授权正确。

---

## 1. 风险模型（什么会坏 + 爆炸半径）

| 风险 | 触发场景 | 爆炸半径 | 可逆性 |
|---|---|---|---|
| **迁移失败导致 vault orphan** | DPAPI 写了但旧 keychain slot 已删 / 迁移中途崩 | vault 读不出（master key 丢） | ✅ 用 §2 的 export 在新 master key 上 `import` 重建（token hash 保留，agent 不用改配置） |
| **DPAPI 在真实 serve session 读不出 master.key**（spike 说能，但 spike ≠ 生产） | Task Scheduler 起的 serve 进程 CryptUnprotectData 失败 | serve 起不来 / vault LOCKED | ✅ `serve uninstall` + 用 export 重建；spike 已在三 session 验证过，概率低 |
| **reboot 后 serve 不自起** | Task Scheduler at-startup 触发器没生效 / 密码过期 | serve 不跑 | ✅ **不危及 vault**——手动 `schtasks /Run` 起来；reboot 只测自起，不碰 master.key/store.db |
| **reboot 后 DPAPI master.key 读不出**（跨重启未测，残余风险） | DPAPI Master Key 重启后未加载 | vault LOCKED | ⚠ Phase D 第一道关卡就查这个；失败 → 用 export 重建 |
| **v0.2.0 进程持有 store.db 句柄** | 迁移前没停干净 → 撞锁 | 迁移 / 启动失败 | ✅ kill 所有 v0.2.0 ssh-manager 进程（FINDING 5） |
| **密码过期导致任务起不来** | allan716 账户密码策略过期 | serve 任务凭据失效 | ✅ 部署前禁密码过期（`wmic ... PasswordExpires=False`） |
| **笔记本 .mcp.json 改坏** | 编辑出错 | 笔记本 agent 连不上 | ✅ 改前备份 .mcp.json |
| **reboot 影响 NUC10 其他服务** | 重启本身 | DocuFiller UpdateHub 等短暂中断 | ⚠ 不可逆（不能 un-reboot）→ 用户选时机；但 serve 自起验证不依赖其他服务 |

**两条贯穿性结论**：
1. **万能恢复介质 = 部署前的 export 文件**（§2）。它在 v0.2.0 的交互式 session 里生成，口令加密，与 master key 机制完全解耦。有它在手，最坏情况（vault orphan）= fresh `unlock` + `import` 重建，project token hash 保留，agent 配置不动。
2. **reboot 不危及 vault**。reboot 唯一测的是"Task Scheduler 自起"；master.key + store.db 不被重启触碰。reboot 后 serve 没自起 → 手动起 + 查 log，不需要回退。

---

## 2. Phase B1 安全绳：部署前 export（THE critical phase）

**这是整条链的可逆性基石，必须在任何写操作之前做。**

### 2.1 为什么必须在交互式 session

export 要读 master key 来解密 vault。v0.2.0 的 master key 在 Credential Manager keychain → **sshd session 读 keychain 报 1312**（FINDING 9）→ export 在 SSH 里跑会失败。所以 export 必须在 **RDP / 本地控制台** 跑（keychain 在交互式 logon session 可读）。

> 例外：若 allan716 手头有当初 `unlock` 打印的 `SSHMGR_MASTERKEY_HEX`，可用 `SSHMGR_MASTERKEY_HEX=<hex> ssh-manager export ...` 在 SSH 里跑（env var fallback 绕过 keychain）。但默认走交互式 session 最稳。

### 2.2 export 命令

在 NUC10 交互式 session（RDP/本地）：

```powershell
# v0.2.0 二进制（部署新版之前）
ssh-manager export --out C:\Users\allan716\ssh-manager-pre-p14.v0.2.0.sme
# 提示输口令两次——用强随机口令，存进 1Password / 密码管理器
```

或带 passphrase 文件（避免 TTY prompt，便于脚本化）：

```powershell
# 生成强口令到 0600 文件
$rng = [byte[]]::new(32); [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($rng)
$pass = [Convert]::ToBase64String($rng)
Set-Content -Path C:\Users\allan716\.ssh-manager\export.pass -Value $pass -Encoding UTF8 -NoNewline
icacls C:\Users\allan716\.ssh-manager\export.pass /inheritance:r /grant:r "allan716:F"

ssh-manager export --out C:\Users\allan716\ssh-manager-pre-p14.v0.2.0.sme `
  --passphrase-file C:\Users\allan716\.ssh-manager\export.pass
```

### 2.3 export 校验（必做，否则安全绳不算就位）

```powershell
# 验证文件可被 import 读回（用一个临时空 store，不碰生产 vault）
# 方法：拷一份 export 到测试目录 + 临时 SSHMGR_STORE，import 后 servers ls
$env:SSHMGR_STORE = "$env:TEMP\verify-p14-store.db"
del $env:TEMP\verify-p14-store.db -ErrorAction SilentlyContinue
ssh-manager unlock                    # 临时空 vault，新 master key
ssh-manager import C:\Users\allan716\ssh-manager-pre-p14.v0.2.0.sme `
  --passphrase-file C:\Users\allan716\.ssh-manager\export.pass
ssh-manager servers ls                # 应看到所有生产 server
del $env:TEMP\verify-p14-store.db
Remove-Item Env:\SSHMGR_STORE
```

**通过标准**：`servers ls` 列出全部生产服务器。**没通过 = 安全绳断了，禁止继续后续 phase。**

### 2.4 export 文件保管

- 拷一份到 **笔记本**（或 NAS 离线目录）——NUC10 出事时本地文件可能也读不到。
- 口令存 1Password。
- 验收完成后（Phase F）按需清理或转正为定期备份。

---

## 3. 执行阶段总览

```
Phase A (SSH, 只读)     Recon —— 盘点 NUC10 现状，零改动
   ↓
Phase B (RDP/本地, 写)   变更窗口 —— 所有写操作在这一轮交互式 session
   B1 export 安全绳
   B2 kill v0.2.0 进程
   B3 部署新版二进制（改名旧版）
   B4 unlock 迁移（keychain → DPAPI）
   B5 迁移验证（master.key 在 / keychain slot 删 / vault 可读）
   B6 禁密码过期 + serve install
   B7 ★ 重启前关卡：serve status 全绿
   ↓
Phase C (reboot)         用户触发重启（影响其他服务，用户选时机）
   ↓
Phase D (SSH, 只读)      重启后自起验证 —— serve status / vault 解锁
   ↓
Phase E (笔记本)         客户端连 + 真实目标机 exec_command
   ↓
Phase F                  收尾：定稿 or 回退
```

每个 phase 有**通过标准**（可量化）+ **失败回退**（指向 §8 矩阵）。

---

## 4. Phase A：Recon（SSH 只读，零改动）

**目的**：把 §0.1 的"已知事实"对照 NUC10 实际状态核实一遍，记录基线。**只读，不改任何东西。**

### 4.1 Recon 命令清单（全只读）

```bash
# 通过 SSH 到 allan716@192.168.100.235 跑（或 RDP 里跑）
# 1. 现有 ssh-manager 进程（v0.2.0 持有 store.db 句柄的元凶）
tasklist | findstr ssh-manager

# 2. 二进制位置 + 版本
where ssh-manager
ssh-manager --version 2>&1 || echo "no --version flag"

# 3. vault 路径 + 大小 + mtime
dir "%AppData%\ssh-manager\store.db"

# 4. 新版产物是否已存在（不该有）
dir "%AppData%\ssh-manager\master.key" 2>nul && echo "master.key EXISTS (unexpected)"
dir "%AppData%\ssh-manager\cache-dek.key" 2>nul && echo "cache-dek.key EXISTS (unexpected)"

# 5. 现有 Task Scheduler 任务（不该有 ssh-manager-serve）
schtasks /Query /TN ssh-manager-serve 2>nul && echo "task EXISTS (unexpected)" || echo "task absent (ok)"

# 6. keychain slot 现状（v0.2.0 master key + cache-dek）
#    用 vault/cmdkey 或 ssh-manager 自带的 status 命令查
ssh-manager status 2>&1 || ssh-manager cache status 2>&1

# 7. 账户密码过期策略
wmic UserAccount where Name='allan716' get PasswordExpires,Name

# 8. 端口 7878 是否已被占
netstat -ano | findstr :7878

# 9. 其他服务基线（reboot 影响评估用）
tasklist | findstr -i "DocuFiller UpdateHub"
```

### 4.2 通过标准

- 所有"unexpected"项为 absent（NUC10 是干净的 v0.2.0 状态，无 P14 残留）。
- 记录到 recon 报告：进程 PID 列表、二进制路径、store.db 大小/mtime、密码过期状态、7878 占用情况、其他服务列表。

### 4.3 失败处理

- 发现 `master.key` 已存在 / `ssh-manager-serve` task 已存在 → 说明 NUC10 不是干净 v0.2.0（可能之前试装过 P14）。**停下，把现状报给用户拍板**，不要继续。
- 密码 `PasswordExpires=True` → 记下，Phase B6 第一步禁掉。

---

## 5. Phase B：变更窗口（RDP / 本地控制台，一轮交互式 session）

**所有写操作集中在此。必须在交互式 session（不是 SSH）——迁移 + export + serve install 都依赖 keychain 可读 / Get-Credential 弹窗。**

> 建议整个 Phase B 在**同一个 RDP 窗口**里顺序跑，不要跨 session——避免 keychain 读取状态不一致。

### 5.1 B1：export 安全绳

见 §2.2-2.4。**B1 没通过校验（§2.3 servers ls 不全）→ 禁止继续 B2 及之后。**

### 5.2 B2：停所有 v0.2.0 进程

```powershell
# 列出 + 确认 + kill（FINDING 5：v0.2.0 持有 store.db 句柄）
Get-Process ssh-manager -ErrorAction SilentlyContinue | Format-Table Id,StartTime,Path
# 人工核对：都是 v0.2.0 的 mcp / serve / cli，没有误伤
Get-Process ssh-manager -ErrorAction SilentlyContinue | Stop-Process -Force
# 确认清空
Get-Process ssh-manager -ErrorAction SilentlyContinue
# 确认 store.db 句柄释放（能 move 即释放）
move "$env:APPDATA\ssh-manager\store.db" "$env:APPDATA\ssh-manager\store.db.probe" 
move "$env:APPDATA\ssh-manager\store.db.probe" "$env:APPDATA\ssh-manager\store.db"
```

通过标准：进程清空 + store.db 可来回 move（句柄释放）。

### 5.3 B3：部署新版二进制（可逆）

```powershell
# 记录旧二进制路径
$old = (Get-Command ssh-manager).Source            # e.g. C:\Users\allan716\go\bin\ssh-manager.exe
# 备份旧版（可换回）
move $old "$old.v0.2.0.bak"
# 放新版（从 master a00f5c0 编译，或 release）—— 路径按实际
copy <new-ssh-manager.exe> $old
# 验证新版可执行 + 版本
ssh-manager --version 2>&1
ssh-manager serve status 2>&1 | findstr "not yet"   # 占位检查不崩
```

通过标准：新版 `--version` 跑通（或至少 `serve status` 不因二进制本身崩）。

**回退**：若新版二进制启动就崩（编译问题等）—— `move "$old.v0.2.0.bak" $old` 换回，Phase B 中止。vault 还没动，无损失。

### 5.4 B4：unlock 迁移（keychain → DPAPI）

```powershell
ssh-manager unlock
# 预期提示（spec §5.6）：
#   "检测到 v0.2.0 keychain master key，迁移到 DPAPI 文件？[y/N]"
#   同时迁移 cache-dek（Plan 12 slot → cache-dek.key）
# 输 y
```

通过标准：程序打印迁移成功 + master.key 生成。

### 5.5 B5：迁移验证（4 项硬检查，全过才算迁移成功）

```powershell
# 1. master.key 存在 + 非空 + ACL 正确
Get-Item "$env:APPDATA\ssh-manager\master.key" | Select Length,LastWriteTime
icacls "$env:APPDATA\ssh-manager\master.key"
# 期望：Length > 0；ACL 只有 allan716 + SYSTEM（无 Everyone）

# 2. cache-dek.key 存在
Get-Item "$env:APPDATA\ssh-manager\cache-dek.key"

# 3. 旧 keychain slot 已删（v0.2.0 master key slot + cache-dek slot）
#    用 vault 的 status / 或 keychain 枚举确认 slot 没了
ssh-manager status 2>&1 | findstr -i "keychain migrate"
# （具体命令按 vault 暴露的 introspection；若没有，跳过此项，靠 4 兜底）

# 4. vault 可读（终极验证：master.key 真能解开 store.db）
ssh-manager servers ls
# 期望：列出全部生产 server（数量 == export 时记录的）
```

通过标准：4 项全过，尤其 **#4 servers ls 列出全部 server**。

**失败回退（§8 矩阵 R2）**：
- master.key 生成了但 servers ls 读不出 → DPAPI 在该 session 解不开。**别删任何东西**。用 §2 的 export 在新 master key 上 import 重建（详见 §8）。

### 5.6 B6：禁密码过期 + serve install

```powershell
# 6a. 禁密码过期（单 owner 本地账户；Win10 19045 wmic 可用）
wmic UserAccount where Name='allan716' set PasswordExpires=False
# 核实
wmic UserAccount where Name='allan716' get PasswordExpires,Name

# 6b. serve install（弹 Get-Credential 输 allan716 密码）
ssh-manager serve install --addr 0.0.0.0:7878
# 若已有 TLS 证书：
# ssh-manager serve install --addr 0.0.0.0:7878 --tls-cert cert.pem --tls-key key.pem
```

通过标准：任务注册成功 + 立即 `/Run` 验证启动 + serve.log 生成。

### 5.7 B7：★ 重启前关卡（必须全绿才进 Phase C）

```powershell
ssh-manager serve status
# 期望四路：
#   task:      registered (Running, last result 0)
#   process:   running
#   http:      responding (401/200 = auth working)
#   vault:     ok
#   overall:   HEALTHY
```

**通过标准：overall = HEALTHY，尤其 `vault: ok`（不是 LOCKED）。**

任何一路不绿 → **不重启**，先排查（§8 矩阵 R4/R5）。reboot 不会修好 B7 的问题，只会复现它。

---

## 6. Phase C：Reboot（用户触发）

**这是唯一影响 NUC10 其他服务的步骤。用户选时机。**

- 关机前最后看一眼 B7 的 serve status（截图存档）。
- `shutdown /r /t 0`（或开始菜单重启）。
- 等 NUC10 起来 + 网络就绪（~2-5 min）。

**不可逆性说明**：reboot 本身不能 undo，但 vault 不受影响（master.key + store.db 不被重启触碰）。reboot 失败模式只有"serve 没自起"，可手动起，不需要回退。

---

## 7. Phase D：重启后自起验证（SSH 只读）

NUC10 起来后，SSH 进去查：

```bash
# 1. serve 自起了没
ssh-manager serve status
# 期望：和 B7 一样 HEALTHY

# 2. 若 status 报 process not running，手动起一次看 log
schtasks /Run /TN ssh-manager-serve
sleep 5
tail -50 "$LOCALAPPDATA/ssh-manager/serve.log"     # 或用 Get-Content -Tail 50

# 3. HTTP 活 + 鉴权工作（401 = 鉴权闸在）
curl -s -o /dev/null -w "%{http_code}" http://192.168.100.235:7878/
# 期望：401（无 token）或 200（带 token）
```

通过标准：serve status HEALTHY + vault: ok（不是 LOCKED）。

**关键残余风险检查**：`vault: ok` 这一行是跨重启 DPAPI 可用性的实证（spike 测过跨 session 但没测跨重启）。若 `vault: LOCKED` → 见 §8 矩阵 R6。

---

## 8. 失败场景 → 回退矩阵（cheat-sheet）

> **第一原则**：有 §2 的 export 在手，最坏情况都能重建。下面的"回退"多数是"更快的小回退"，终极兜底永远是 R0。

| ID | 场景 | 现象 | 回退动作 |
|---|---|---|---|
| **R0** | 终极兜底（vault orphan / master key 丢 / 任何读不出） | `servers ls` 空 / `vault: LOCKED` / master.key 解不开 | ① 确认 export 文件在（笔记本 + NUC10 双份）② `move master.key master.key.bad` / `del master.key` ③ `ssh-manager unlock`（生成全新 master key）④ `ssh-manager import <export.sme> --passphrase-file <pass>` ⑤ `servers ls` 核对数量。token hash 保留，agent 配置不动。 |
| R1 | B2 kill 后 store.db 仍锁住 | move 探测失败 | 还有隐藏 v0.2.0 进程；`tasklist /v | findstr ssh-manager` + handle.exe 找句柄；全 kill 后重试。不进 B3。 |
| R2 | B5 迁移后 servers ls 读不出 | master.key 在但解不开 | 别删 master.key；先 R0 重建。然后查为什么 DPAPI 解不开（icacls? DPAPI Master Key 状态?）—— 这是 P14 的真 bug，记 finding。 |
| R3 | B3 新二进制启动崩 | `ssh-manager --version` 异常退出 | `move ssh-manager.exe.v0.2.0.bak ssh-manager.exe` 换回；vault 没动；中止 Phase B 报 bug。 |
| R4 | B7 serve status http 不响应 | process 在但 http 超时 | 查 serve.log；可能端口冲突（recon #8 应已查）或 TLS 配置；`serve uninstall` → 修参 → 重 install。 |
| R5 | B7 serve status vault: LOCKED | 进程在但 master.key 在 serve session 解不开 | 这是 FINDING 10 / 共识 A 的生产复现（spike 说不会，但实证优先）。`serve uninstall`；先 R0 确认 vault 本体可重建；记 finding（可能要回 §9 machine-scope 或 kardianos/service）。 |
| R6 | Phase D 重启后 vault: LOCKED | 跨重启 DPAPI 不可用（残余风险） | 手动 `schtasks /Run` + 看 log；若仍 LOCKED → R0 重建；记 finding（跨重启 DPAPI Master Key 加载问题）。 |
| R7 | Phase D 重启后 serve 不自起 | process not running | `schtasks /Run /TN ssh-manager-serve` 手动起；查 Task Scheduler "Last Result" + serve.log；常见 = 密码过期（B6 没禁成功）或触发器配置。vault 不受影响。 |
| R8 | Phase E 笔记本连不上 | MCP 连接超时 / 401 / 403 | ① 网络层：`curl http://192.168.100.235:7878/`（期望 401）② 401 = 鉴权闸在，查 token ③ 403 = session mismatch，查 project token 对不对 ④ 还原笔记本 .mcp.json 备份。 |
| R9 | Phase E exec_command 失败 | 连上但 SSH 到目标机失败 | vault 侧没问题（能连 serve = vault ok）；查 1660Super01 是否在线 / 凭据对不对 / profile 授权。换一台目标机试。 |
| R10 | 整体想放弃 / 全部回退到 v0.2.0 | 任何阶段 | ① `ssh-manager serve uninstall`（若有 task）② `del master.key` / `del cache-dek.key` ③ `move ssh-manager.exe.v0.2.0.bak ssh-manager.exe` ④ `ssh-manager unlock`（v0.2.0 会重新写 keychain slot—— 但旧 slot 被迁移删了，**这一步需要 export 重建**）→ 走 R0 import。**注意：迁移删了 keychain slot 后，v0.2.0 不能直接换回用**，必须 R0 重建。 |

### 8.1 abort 标准（何时停止一切）

**立即 abort 并走 R0 重建**：
- 任何 phase 出现 `vault: LOCKED` 且手动重试无效。
- `servers ls` 数量少于 export 时记录的数量（数据丢失嫌疑）。
- master.key 反复生成 / 解密失败。
- DPAPI 在 serve session 确认不可用（R5 实证）。

**abort 后**：NUC10 vault 在新 master key 上从 export 重建；serve 暂不装（等 P14 bug 修）；用户决定是否换回 stdio 模式。**数据零丢失**（export 兜底）。

---

## 9. Phase E：笔记本客户端 + 真实目标机 exec

### 9.1 发 / 确认 project token

在 NUC10（SSH 或 RDP 都行，vault 已迁完，DPAPI 在 sshd 也能读——spike 实证）：

```bash
# 若已有 project token（从 export 保留）—— 直接用，不用重发
# 若要新发：
ssh-manager projects ls                    # 看现有 project
# ssh-manager projects add <name> --profile <profile>   # 打印一次性 token
```

### 9.2 笔记本 .mcp.json（先备份）

```powershell
# 备份现有 .mcp.json
copy C:\Users\allan716\.claude.json C:\Users\allan716\.claude.json.pre-p14-e2e.bak
# 或备份项目级 .mcp.json（按实际位置）
```

改 `.mcp.json`（VLAN 内，HTTP；若有 TLS 用 https）：

```json
{
  "mcpServers": {
    "ssh": {
      "type": "http",
      "url": "http://192.168.100.235:7878/",
      "headers": { "Authorization": "Bearer <project-token>" }
    }
  }
}
```

重启 Claude Code → 笔记本 agent 连 NUC10 serve。

### 9.3 真实目标机 exec（spec §7.3 最后一关）

在笔记本的 Claude Code 里，通过 MCP 调 `tools/call exec_command`：

- server = `1660Super01`
- command = `hostname`
- **期望返回 `DESKTOP-UP1MHGT`**（spec §7.3 的硬标准）

通过标准：返回 `DESKTOP-UP1MHGT`（或 1660Super01 实际 hostname）。

### 9.4 失败回退

- 连不上 → R8。
- 连上但 exec 失败 → R9。
- 改回笔记本：`copy .claude.json.pre-p14-e2e.bak .claude.json` + 重启 Claude Code。

---

## 10. Phase F：收尾

### 10.1 验收通过（全绿）

- [ ] 收集过程数据：每个 phase 的 serve status 输出 / serve.log 摘要 / recon 报告 / 迁移时间。
- [ ] export 安全绳文件转正为定期备份（或纳入 Plan 13 NAS 备份周期）。
- [ ] 清理临时文件：`export.pass`（passphrase 文件，0600 但用完应删）、`store.db.probe`、`verify-p14-store.db`。
- [ ] 保留 `ssh-manager.exe.v0.2.0.bak` 一段时间（确认稳定后删）。
- [ ] 更新 memory：Plan 14 §7.3 NUC10 E2E 通过，记录实测数据（跨重启 DPAPI 是否 ok 等残余风险结论）。

### 10.2 验收未通过（发现 P14 真 bug）

- 走 §8 abort → R0 重建 NUC10 vault。
- serve 不装（或装了但标记已知不可用）。
- 把 finding 写成新 plan（如 R5/R6 的 DPAPI 生产不可用 → 回 spec §9 machine-scope / kardianos/service）。
- NUC10 暂回 stdio 模式或 v0.2.0（经 R0 重建后）。
- **数据零丢失**。

---

## 11. 过程数据收集清单（验收产出）

每个 phase 记录（追加到 `.omc/state/p14-nuc10-e2e-<date>.md`，已 gitignore）：

| Phase | 记录什么 |
|---|---|
| A | recon 报告全文（进程 / 路径 / 密码过期 / 7878 占用 / 其他服务） |
| B1 | export 文件 sha256 + size + servers ls 数量（校验用） |
| B2 | kill 的 PID 列表 + store.db 句柄释放确认 |
| B3 | 新旧二进制路径 + 版本 |
| B4 | unlock 迁移提示原文 + 是否成功 |
| B5 | master.key ACL / cache-dek.key / servers ls 数量（== B1?） |
| B6 | PasswordExpires 核实 + serve install 输出 |
| B7 | serve status 四路输出（截图/文本） |
| C | reboot 时间 + NUC10 回来时间 |
| D | 重启后 serve status（== B7?）+ curl 401/200 |
| E | project token 来源（保留/新发）+ exec_command 返回值 |

**核心对照**：B1 的 `servers ls` 数量 == B5 == D（vault 自始至终数据完整）。

---

## 12. 执行方式建议

- **串行单 session 执行**（推荐）：我和用户一起，phase by phase，每个关卡用户确认。reboot 时机用户定。适合"收集过程数据 + 出问题立即决策"。
- **不并行**：Phase B 写操作不可并行（keychain / store.db 单写者）。
- **subagent 不适用**：这是生产环境操作 + 物理重启，不是代码任务，需要人的判断关卡。

**执行前需用户确认**：
1. 时机（reboot 影响其他服务，选维护窗口）。
2. 是否有 `SSHMGR_MASTERKEY_HEX`（决定 export 走交互式还是 SSH）。
3. 1660Super01 在线 + 凭据就绪。
4. 授权我 SSH 到 NUC10 做 Phase A recon（只读）。
