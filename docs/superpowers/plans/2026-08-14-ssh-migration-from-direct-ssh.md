# SSH 迁移执行计划:从直接 SSH 迁到 ssh-manager MCP

> **For agentic workers:** 本计划是**运维迁移 runbook**(在两台真机上操作 + 删除凭据),不是写代码。用 checkbox (`- [ ]`) 跟踪。**没有 TDD 循环**(无法给"删私钥文件"写失败测试);每个删除/变更步骤都有"验证"步骤替代。**任何删除动作前必须先列清单给用户确认(§2.2 铁律)**。
>
> REQUIRED SUB-SKILL: 执行时用 superpowers:executing-plans(批量执行 + checkpoint),不适合 subagent-driven(跨机运维无并行任务,且每步需用户确认)。

**Goal:** 把笔记本上直接交给 AI agent 的 SSH 凭据全部收回,改走 ssh-manager MCP(NUC10 权威 broker);清理 `~/.ssh`(删 10 个服务器 Host + 10 个私钥);两端升级到 v0.4.0(auto-TLS)。

**Architecture:** 方案 A(保持现状拓扑):NUC10 继续权威 broker + 笔记本 client。复用 Plan 16 §7.3 已验收的部署形态。全局时序 = spec §4.6 的 10 个阶段(备份→补机→验证→发版→升级→离线验证→删除审批→清理→iron rule 验证)。

**Tech Stack:** ssh-manager-mcp v0.4.0(auto-TLS,c42024c 之后 master HEAD `7de4fb1`)、GoReleaser 发版、Windows Service(kardianos)、Windows Task Scheduler(cache-refresh)。

## Global Constraints(逐字来自 spec)

- **删除审批铁律(spec §2.2)**:任何删除(私钥/config/config.bak/known_hosts)执行前,必须先输出完整删除清单(路径+说明+为何删+是否已备份/入 vault),经用户确认才动手。阶段 6.5 是硬 checkpoint。
- **升级顺序铁律(spec §4.2)**:先升笔记本二进制+pin → 最后才重启 NUC10 serve。升 serve 瞬间变 TLS-only,旧明文 client 断。
- **cert-info 在 v0.3.1 不存在(spec §4.3)**:用 staging 的 v0.4.0 二进制跑 cert-info,不靠运行中的 v0.3.1。
- **192.168.8.121 定删不入 vault(spec §2.1)**:删除清单 10 个服务器 Host,不灌 vault。
- **群晖同步到自己服务器=安全(spec §2.1)**:明文备份不必暂停 SynologyDrive 同步。
- **GitLab 私钥保留(spec §2 边界2)**:`192.168.200.46` GitLab over ssh 依赖 `C:/WorkSpace/` 下 20 个 repo,私钥必须留。
- **私钥跨机传输禁止明文(spec §5.2)**:NUC10 还是 v0.3.1 明文 serve 时,禁止走 HTTP 远程 `servers add`(=内网明文传私钥)。优先 RDP 手粘。
- **拓扑机器**:权威 broker = NUC10(192.168.100.235);client = 笔记本(DESKTOP-UGO0KBL)。vault 现有 7 台(1660Super01/02、3090x2、4090x2、DocuFiller-UpdateHub、NUC10、procurement-recog),project `e2e-agent` / profile `e2e-profile`,cache-token `laptop`。

**前置事实(本计划假设为真,执行前由用户/环境核对)**:
- master HEAD `7de4fb1` 含完整 auto-TLS(已 `git log` 确认)。
- NUC10 + 笔记本当前跑 v0.3.1(无 TLS)。
- 笔记本 `~/.ssh/` 有 10 个 `id_*` 私钥 + config 12 个 Host(2 git + 10 服务器)。

---

## 文件 / 状态结构(本计划"动"什么)

| 位置 | 机器 | 动作 |
|---|---|---|
| `C:\Users\allan716\SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\` | 笔记本 | **新建**(备份目标,阶段0) |
| `~/.ssh/{config,config.bak,id_*,known_hosts,known_hosts.old}` | 笔记本 | **阶段7-8 删除/清空**(经审批) |
| NUC10 vault(store.db) | NUC10 | **补加** ml_hub/ai_runner(阶段1) |
| NUC10 `C:\ProgramData\ssh-manager\ssh-manager.exe` | NUC10 | **替换** v0.3.1→v0.4.0(阶段4) |
| NUC10 `serve cert-info` 输出指纹 | NUC10 | **生成**(阶段4) |
| 笔记本 `~/bin/ssh-manager.exe` | 笔记本 | **替换** v0.3.1→v0.4.0(阶段4) |
| 笔记本 cache-pull.cmd(定时任务 wrapper) | 笔记本 | **重发** 带 pin 设备码(阶段4-5) |
| 笔记本顶层 `.mcp.json` 的 `ssh` 条目 | 笔记本 | **改** 在线 HTTP→离线 `mcp --cache`(阶段5) |
| GitHub Release v0.4.0 | GitHub | **发版**(阶段3) |

---

## 阶段 0:备份 ~/.ssh

**Files:** 无(纯文件操作)
**目标:** 在动任何东西前,先全量备份 `~/.ssh/` 到 SynologyDrive。

- [ ] **0.1 创建备份目录**

```bash
mkdir -p "/c/Users/allan716/SynologyDrive/ServerKey/ssh-dot-ssh-backup-2026-08-14"
ls -d "/c/Users/allan716/SynologyDrive/ServerKey/ssh-dot-ssh-backup-2026-08-14"
```
Expected: 目录路径回显,无报错。

- [ ] **0.2 镜像 ~/.ssh 全部内容到备份目录**

```bash
cp -av ~/.ssh/. "/c/Users/allan716/SynologyDrive/ServerKey/ssh-dot-ssh-backup-2026-08-14/"
```
Expected: 列出复制的文件(config、config.bak、id_*(含.pub)、known_hosts、known_hosts.old),无报错。

- [ ] **0.3 验证备份完整且可读**

```bash
echo "===源===" && ls ~/.ssh/ | wc -l
echo "===备份===" && ls "/c/Users/allan716/SynologyDrive/ServerKey/ssh-dot-ssh-backup-2026-08-14/" | wc -l
echo "===抽检一个私钥能读===" && head -1 "/c/Users/allan716/SynologyDrive/ServerKey/ssh-dot-ssh-backup-2026-08-14/id_nuc10"
```
Expected: 源/备份文件数一致(约 21);私钥首行是 `-----BEGIN OPENSSH PRIVATE KEY-----`。

- [ ] **0.4 确认未触碰 ServerKey 现有文件**

```bash
ls "/c/Users/allan716/SynologyDrive/ServerKey/" | head -20
```
Expected: 只有新增的 `ssh-dot-ssh-backup-2026-08-14/` 子目录,github/gitlab/update-hub 等现有文件原样在。

> ✅ 阶段 0 checkpoint:备份可读 + 文件数一致 + 未碰 ServerKey 现有文件。**备份不暂停 SynologyDrive 同步**(spec §2.1 拍板:同步到自己服务器=安全)。

---

## 阶段 1:补加 ml_hub / ai_runner 到 NUC10 vault

**Files:** NUC10 的 store.db(通过 `ssh-manager servers add`)
**目标:** 把 config 有但 vault 缺的 2 台(ml_hub/ai_runner)灌进 NUC10 vault 并验证可连。
**Interfaces:** 产出入 vault 的 server 名(ml_hub/ai_runner),阶段 2/5 依赖。
**⚠️ 私钥跨机传输(spec §5.2)**:NUC10 还是 v0.3.1 明文 serve,禁止 HTTP 远程 add。优先路径 a(RDP 上 NUC10 手粘)。

- [ ] **1.1 先做实地核对(spec §5.1/5.3,执行前查清,不能凭空)**

在笔记本上查(只读,不删):
```bash
echo "===172.18.200.47 是否同机(ml_hub 40101 vs update-hub 30000)===" 
echo "ml_hub:    172.18.200.47:40101 allan"
echo "update-hub:172.18.200.47:30000 Administrator"
echo "(阶段1.4 上去后用 hostname 核对)"
echo ""
echo "===vault 现有 DocuFiller-UpdateHub vs config update-hub(坐实同一性)==="
```
需用户/执行者确认:DocuFiller-UpdateHub 的 host:port:user 是否 = 172.18.200.47:30000:Administrator。
需核对:`id_ed25519`(默认键)归属——grep ~/.ssh/config 无指向则疑废弃;`id_4090x2.deprecated` 已标废弃。
**输出一张核对表给用户**,用户确认后再 1.2。

- [ ] **1.2 私钥跨机传输路径(用户拍板:路径 a — RDP 手粘)**

✅ **已定路径 a**:用户 RDP 上 NUC10,在 NUC10 上跑 `ssh-manager servers add` 交互,**手动粘贴**私钥内容(私钥不落 NUC10 临时文件,最安全)。
> 192.18.8.121(spec §2.1)用户定**删除不入 vault**,本阶段跳过它。
> `id_ed25519`(阶段1.1 核对 = NUC10 账号默认 fallback key,vault 已有专用 id_nuc10)——**用户拍板:定删,不入 vault**。

- [ ] **1.3 在 NUC10 上 add ml_hub / ai_runner(路径 a)**

RDP 上 NUC10。先看现有 7 台 + 核对 add 的 flag:
```
ssh-manager servers ls                       # 看现有 7 台名(确认 1660Super01/02,3090x2,4090x2,DocuFiller-UpdateHub,NUC10,procurement-recog)
ssh-manager servers add --help               # 核对 flag 名(--name/--host/--user/--port/--key 或 --password)
```
然后 add 两台(flag 名以 --help 为准,下面是预期形):
```
ssh-manager servers add --name ml_hub --host 172.18.200.47 --user allan --port 40101 --key "<粘贴 ~/.ssh/id_ml_hub 内容>"
ssh-manager servers add --name ai_runner --host 192.168.100.201 --user urit_ai --port 22 --key "<粘贴 ~/.ssh/id_ai_runner_201 内容>"
```
> 私钥内容从笔记本 `~/.ssh/id_ml_hub` / `~/.ssh/id_ai_runner_201` 取(已备份)。粘贴完整含 BEGIN/END 行。**不落 NUC10 临时文件。**

- [ ] **1.4 grant 到现有 e2e-profile**

```
ssh-manager profiles grant e2e-profile ml_hub
ssh-manager profiles grant e2e-profile ai_runner
ssh-manager servers ls
```
Expected: `servers ls` 列出 9 台(原 7 + ml_hub + ai_runner);顺带核对 172.18.200.47 两台 hostname 是否同机。

- [ ] **1.5 验证 ml_hub / ai_runner 可连(via MCP)**

用现有 project token 跑在线 MCP(NUC10 v0.3.1 明文,本机直连):
```
ssh-manager mcp --token <e2e-agent project token>
```
在 MCP 里 `exec_command` 对 ml_hub 跑 `hostname`、对 ai_runner 跑 `hostname`。
Expected: 两台都返回真实 hostname,exit 0。
> 若失败:核对私钥/密码/端口;**不要**删本地私钥(本地仍在)。

> ✅ 阶段 1 checkpoint:vault 9 台 + ml_hub/ai_runner MCP 可连。阶段 2 继续全量验证。

---

## 阶段 2:全量验证 vault 所有目标机 MCP 可连

**目标:** 删本地私钥前,确认 vault 全量 9 台都能通过 MCP 连上(铁律:全过才继续)。
**L5 特例(spec §5.3):** 3090x2 无本地私钥,验证失败无 ~/.ssh 兜底,靠密码重试或备份还原。

- [ ] **2.1 逐台 MCP exec_command 验证**

对 vault 全量 9 台各跑一个无害命令(`hostname` 或 `whoami`):1660super01/02、3090x2、4090x2、DocuFiller-UpdateHub、NUC10、procurement-recog、ml_hub、ai_runner。
Expected: 9/9 exit 0,返回真实 hostname/用户。

- [ ] **2.2 记录验证结果矩阵**

输出一张表(机器 | 命令 | 返回 | exit码),给用户看。
Expected: 9 行全 ✅。**有任何一台 ✗,停在阶段 2,不进阶段 3。** 修复后重验。

> ✅ 阶段 2 checkpoint:vault 9 台 9/9 MCP 可连。此时笔记本仍 v0.3.1 + ~/.ssh 私钥仍在,有回退。

---

## 阶段 3:发版 v0.4.0

**Files:** GitHub Release(tag 触发 GoReleaser,Plan 9 既定流程)
**目标:** 把 master 上的 auto-TLS 发成可下载的 v0.4.0。

- [ ] **3.1 确认 master HEAD 含完整 auto-TLS**

```bash
git log --oneline -1 && git log v0.3.1..master --oneline | wc -l
```
Expected: HEAD `7de4fb1`;master 比 v0.3.1 多 ~25 个提交(含 auto-TLS)。

- [ ] **3.2 打 tag 并推送(触发 release.yml)**

```bash
git tag v0.4.0 && git push origin v0.4.0
```
Expected: tag 推送成功;GitHub Actions release.yml 开始跑(~2min)。

- [ ] **3.3 等 Release 构建完成并下载 windows-amd64**

浏览器/gh 看 Release v0.4.0 构建绿,下载 `ssh-manager_0.4.0_windows_amd64.zip` 到笔记本,解压出 `ssh-manager.exe`。
Expected: 拿到 v0.4.0 的 windows exe。

- [ ] **3.4 Release notes 写破坏性变更**

在 GitHub Release v0.4.0 描述里写明:无 pin 的 `cache pull` 现 hard-fail,需配 pin(`serve cert-info` 取指纹)或 `--allow-plaintext`。
Expected: Release 描述含此警告。

> ✅ 阶段 3 checkpoint:v0.4.0 Release 绿,windows exe 到手。

---

## 阶段 4:升级(铁律:先笔记本+pin,最后 NUC10 serve)

### 阶段 4a:先升笔记本二进制

- [ ] **4a.1 备份笔记本旧 v0.3.1 exe(spec S5)**

```bash
cp -av ~/bin/ssh-manager.exe ~/bin/ssh-manager-v0.3.1-backup.exe
ls -la ~/bin/ssh-manager*.exe
```
Expected: 两个 exe(v0.4.0 新下载的 + v0.3.1-backup)。

- [ ] **4a.2 停旧 cache-refresh 定时任务(它跑无 pin 旧码,升级后 hard-fail)**

```bash
schtasks //Query //TN ssh-manager-cache-refresh 2>/dev/null || echo "(查任务名;可能需 PowerShell)"
```
用 PowerShell 禁用任务:`Disable-ScheduledTask -TaskName ssh-manager-cache-refresh`(确切的 task 名以 3.2 查到为准)。
Expected: 任务已禁用。

- [ ] **4a.3 替换笔记本 exe 为 v0.4.0**

```bash
cp ssh-manager.exe ~/bin/ssh-manager.exe   # v0.4.0 覆盖
~/bin/ssh-manager.exe version   # 或 --version,确认 v0.4.0
```
Expected: 版本输出 v0.4.0。

### 阶段 4b:NUC10 取指纹(用 staging v0.4.0,不碰运行中的 v0.3.1 serve)

> ⚠️ **cert-info 排序铁律(spec §4.3/M1)**:NUC10 当前 v0.3.1 无 `cert-info` 子命令。把 v0.4.0 exe 放 staging 路径跑,运行中的 v0.3.1 serve 进程不受影响。

- [ ] **4b.1 把 v0.4.0 exe 上传到 NUC10 staging 路径**

```bash
scp ssh-manager.exe allan716@192.168.100.235:"C:/ProgramData/ssh-manager/ssh-manager-v0.4.0.exe"
```
(或 RDP 上去 copy。)Expected: 文件在 NUC10 staging 路径。

- [ ] **4b.2 用 staging v0.4.0 跑 cert-info,生成证书 + 打印指纹**

NUC10 上:
```powershell
& C:\ProgramData\ssh-manager\ssh-manager-v0.4.0.exe serve cert-info
```
Expected: 打印 `fp=sha256:abcd1234...`(或 `Server fingerprint (serve cert SPKI): sha256:...`)。**记下这个指纹,4c/5 要用。**
> 幂等:证书已存在只读不写;此刻明文 serve 仍在内存跑(旧 v0.3.1 进程不受影响)。

- [ ] **4b.3 重发带指纹的设备码(spec §4.3[4] + S3 吊销旧码)**

NUC10 上:
```
ssh-manager cache-tokens add --name laptop
```
(用 staging v0.4.0 或旧 v0.3.1,cache-tokens add 在两者都可用。)
Expected: 输出 `<设备码>:<指纹>`(形态 A)或 `<设备码>` + 单独指纹行。**记下新设备码+指纹组合。**
然后吊销旧明文码(阶段1.1 核对:当前活跃旧码 = `REDACTED-REVOKED-cache-token`,嵌在 cache-pull.cmd 里):
```
ssh-manager cache-tokens revoke laptop
```
> revoke 后旧码 `5vYN79Ly…` 即失效;30min 定时任务(4a.2 已禁)即便跑也是 401。新带 pin 码在 5.1/5.2 注入。

### 阶段 4c:最后才重启 NUC10 serve 变 TLS

- [ ] **4c.1 备份 NUC10 旧 v0.3.1 exe(spec S5)**

NUC10 上:
```powershell
Copy-Item C:\ProgramData\ssh-manager\ssh-manager.exe C:\ProgramData\ssh-manager\ssh-manager-v0.3.1-backup.exe
```
Expected: 旧 exe 已备份。

- [ ] **4c.2 替换正式 exe + 重启 serve(强制 TLS)**

```powershell
Stop-Service ssh-manager-serve
Copy-Item C:\ProgramData\ssh-manager\ssh-manager-v0.4.0.exe C:\ProgramData\ssh-manager\ssh-manager.exe -Force
Start-Service ssh-manager-serve
Get-Service ssh-manager-serve
```
Expected: 服务 Running;`serve cert-info` 仍是 4b.2 那个指纹。

- [ ] **4c.3 验证 serve 强制 TLS(明文 http 现在应失败)**

笔记本:
```bash
curl -k https://192.168.100.235:7878/ 2>&1 | head -3   # TLS 通(401 鉴权=预期)
curl http://192.168.100.235:7878/ 2>&1 | head -3         # 明文应失败/空
```
Expected: https 返 401(鉴权层,说明 TLS 通);http 失败(serve 已 TLS-only)。

> ✅ 阶段 4 checkpoint:笔记本 v0.4.0 + NUC10 serve 强制 TLS + 指纹在手 + 新带 pin 设备码 + 旧码吊销。

---

## 阶段 5:笔记本 cache pull(TLS+pin)+ 切离线主路径

**目标:** 笔记本用新设备码+pin 走 TLS 拉全量 cache;`.mcp.json` 切离线 `mcp --cache`。

- [ ] **5.1 cache pull 走 TLS+pinning**

```bash
~/bin/ssh-manager.exe cache pull \
  --url https://192.168.100.235:7878 \
  --token '<4b.3 新设备码>:<4b.2 指纹>'
```
Expected: `pulled N servers / M credentials into cache.bin`(N=9)。验证 TLS+指纹钉死生效(无 pin 会 hard-fail,有错指纹会 mismatch 报错)。

- [ ] **5.2 重发 cache-refresh 定时任务(wrapper 嵌新带 pin 设备码)**

阶段1.1 核对现状:`%LOCALAPPDATA%\ssh-manager\cache-pull.cmd`(419B)当前嵌旧明文码 `5vYN79Ly…`(无 pin)+ `http://`。本步改它:
把 cache-pull.cmd 里 `SSHMGR_CACHE_URL` 改 `https://192.168.100.235:7878`,`SSHMGR_CACHE_TOKEN` 改成 `<4b.3 新设备码>:<4b.2 指纹>`(形态 A)。然后:
```powershell
Enable-ScheduledTask -TaskName ssh-manager-cache-refresh
# 手动跑一次验证
schtasks //Run //TN ssh-manager-cache-refresh
tail "%LOCALAPPDATA%\ssh-manager\cache-pull-task.log"
```
Expected: 任务跑 exit=0;cache-pull.cmd 内嵌带 pin 码 + https URL。

- [ ] **5.3 改 `~/.claude.json` 的 ssh MCP 条目为离线主路径(阶段1.1 核对修正:实际配置在 claude.json 非 .mcp.json)**

⚠️ 核对发现:笔记本的 ssh MCP 配置在 **`~/.claude.json`**(不是 `.mcp.json`),当前指向 `http://192.168.100.235:7878/`(明文 serve)。本步改它。

备份后改 `~/.claude.json` 里的 `mcpServers.ssh` 条目为:
```json
"ssh": {
  "command": "C:\\Users\\allan716\\bin\\ssh-manager.exe",
  "args": ["mcp", "--cache", "--token", "<e2e-agent project token>"]
}
```
(claude.json 是大 JSON,用脚本/jq 精确改这一条,不要手改整个文件。原在线 HTTP 配置备份到 `~/.claude.json.ssh-http-backup-2026-08-14.json`。)
Expected: `~/.claude.json` 的 ssh 条目是 stdio `mcp --cache`,非 http。
> 重启 Claude Code 生效。command 用绝对路径避免 PATH 问题。

- [ ] **5.4 cache status 验证全量**

```bash
~/bin/ssh-manager.exe cache status
```
Expected: 9 servers / 对应 credentials,`N/N`(spec L2:不再写 N/7)。

> ✅ 阶段 5 checkpoint:cache.bin 9/9 走 TLS+pin + `.mcp.json` 离线主路径 + 定时任务带 pin。

---

## 阶段 6:离线验证(`mcp --cache` 全量可连)

**目标:** 删本地私钥前,确认离线只读缓存也能驱动 MCP 连全部目标机(iron rule:在线/离线 agent 表面一致)。

- [ ] **6.1 离线 mcp --cache list_servers**

```bash
~/bin/ssh-manager.exe mcp --cache --token <e2e-agent project token>
```
在 MCP 里 `list_servers`。
Expected: 列出全量 9 台。

- [ ] **6.2 离线 exec 逐台验证**

对 9 台各 `exec_command hostname`(走本地 cache.bin + 笔记本侧 SSH 直拨目标机)。
Expected: 9/9 exit 0。

- [ ] **6.3 重启 Claude Code 确认走离线 MCP**

重启 Claude Code → agent 用 `~/.claude.json` 的 ssh(现在是 `mcp --cache`)。
Expected: agent 能 list_servers + exec_command,不碰 NUC10 serve(离线)。

> ✅ 阶段 6 checkpoint:离线全量可连,agent 重启即用离线 MCP。**此时可进删除阶段。**

---

## 阶段 6.5:★ 删除审批 checkpoint(spec §2.2 硬铁律)

**目标:** 删任何东西前,输出完整删除清单给用户确认。**用户未确认前,阶段 7-8 不得执行。**

- [ ] **6.5.1 生成并输出完整删除清单**

输出一张表,每条:`路径 + 说明 + 为何删 + 是否已备份/入 vault`:

| 路径 | 说明 | 为何删 | 已备份/入vault |
|---|---|---|---|
| `~/.ssh/id_1660super01_146`(+.pub) | 1660super01 私钥 | 已入 vault | ✅ 阶段0备份 + 阶段1.4 vault |
| `~/.ssh/id_1660super02_236`(+.pub) | 1660super02 | 已入 vault | ✅ |
| `~/.ssh/id_4090x2.deprecated` | 旧 4090x2 键 | vault 有更新凭据 | ✅ |
| `~/.ssh/id_ai_runner_201`(+.pub) | ai_runner | 已入 vault(阶段1) | ✅ |
| `~/.ssh/id_ed25519`(+.pub) | NUC10 账号默认 fallback key | 阶段1.1 核对 = `allan716@Nuc10`,vault 已有专用 id_nuc10;**用户拍板定删** | ✅ |
| `~/.ssh/id_ed25519_4090srv` | 4090x2 | 已入 vault | ✅ |
| `~/.ssh/id_ml_hub` | ml_hub | 已入 vault(阶段1) | ✅ |
| `~/.ssh/id_nuc10` | nuc10 | 已入 vault | ✅ |
| `~/.ssh/id_procurement_recog_46`(+.pub) | procurement-recog | 已入 vault | ✅ |
| `~/.ssh/config` 的 10 个服务器 Host 段 | 别名 | 走 MCP 不再需要 | ✅ 备份 |
| `~/.ssh/config.bak` | 旧 config | 含 SynologyDrive 私钥路径引用 | ✅ |
| `~/.ssh/known_hosts` 的非 GitLab 行 | 目标机指纹 | broker 用自己 host_keys 表 | ✅(GitLab 两行保留) |

- [ ] **6.5.2 等用户明确确认("可以删"等)**

**用户未确认 → 停在这里,不进阶段 7。** 这是用户硬约束(spec §2.2)。

> ✅ 阶段 6.5 checkpoint:删除清单已输出 + 用户确认。

---

## 阶段 7:删 ~/.ssh 私钥文件

**目标:** 用户确认后,删 10 个私钥(+.pub)。
**前置:** 阶段 6.5 用户已确认。

- [ ] **7.1 删 10 个私钥(+.pub)**

```bash
cd ~/.ssh
rm -v id_1660super01_146 id_1660super01_146.pub \
       id_1660super02_236 id_1660super02_236.pub \
       id_4090x2.deprecated \
       id_ai_runner_201 id_ai_runner_201.pub \
       id_ed25519 id_ed25519.pub \
       id_ed25519_4090srv \
       id_ml_hub \
       id_nuc10 \
       id_procurement_recog_46 id_procurement_recog_46.pub
```
> `id_ed25519` 删不删取决于阶段 1.1 归属核对结论:若确认废弃则删;若归属某台且需留作他用,从清单移除。执行者据 6.5.1 清单实际勾选项删。
Expected: rm -v 逐个回显已删;`ls id_*` 应为空(或只剩核对后保留的)。

- [ ] **7.2 验证 ~/.ssh 私钥已清**

```bash
ls ~/.ssh/id_* 2>/dev/null || echo "✅ 无 id_* 私钥"
```
Expected: `✅ 无 id_* 私钥`(或只剩 6.5.1 明确保留的)。

- [ ] **7.3 验证 vault 仍全量可连(删后回归)**

笔记本 MCP(在线或离线)`list_servers` + 对 2 台抽 `exec_command`。
Expected: 仍 9 台,可连。**确认删私钥没影响 MCP 路径。**

> ✅ 阶段 7 checkpoint:私钥已删 + MCP 仍可连。回退点:阶段0备份可 copy 回任意私钥。

---

## 阶段 8:删 config 服务器 Host + config.bak + 处理 known_hosts

**目标:** 清 config 的 10 个服务器 Host 段 + 删 config.bak + known_hosts 留 GitLab 两行。

- [ ] **8.1 备份 config 当前状态(再保一层)**

```bash
cp -av ~/.ssh/config ~/.ssh/config.pre-cleanup.bak
```
Expected: 备份生成(此备份阶段 8.3 一并删,或留作短期回退)。

- [ ] **8.2 删 config 的 10 个服务器 Host 段**

用编辑器或 sed 精确删除这 10 段:`1660super01`、`1660super02`、`3090x2`、`4090x2`、`nuc10`、`ml_hub`、`ai_runner`、`procurement-recog`、`update-hub`、`192.168.8.121`(含缩进那段)。
保留:`github.com` + `192.168.200.46` 两段。
验证:
```bash
grep -iE "^\s*Host " ~/.ssh/config
```
Expected: 只剩 `Host github.com` 和 `Host 192.168.200.46` 两行。

- [ ] **8.3 删 config.bak / config.pre-cleanup.bak**

```bash
rm -v ~/.ssh/config.bak ~/.ssh/config.pre-cleanup.bak
```
Expected: 两个 .bak 已删(含 SynologyDrive 私钥路径引用的泄露面清除)。

- [ ] **8.4 known_hosts 处理(留 GitLab 两行,spec §3.1C/M5)**

三选一(阶段 6.5.1 清单里定):
- 方案①:删 known_hosts 除 `192.168.200.46` 外所有行:
  ```bash
  grep "192.168.200.46" ~/.ssh/known_hosts > ~/.ssh/known_hosts.new
  mv ~/.ssh/known_hosts.new ~/.ssh/known_hosts
  rm -f ~/.ssh/known_hosts.old
  ```
- 方案②:对 GitLab 设 `StrictHostKeyChecking accept-new`(写进 config 的 GitLab 段)。
- 方案③:`ssh-keyscan -p 53802 192.168.200.46 >> ~/.ssh/known_hosts` 预置后清其余。
Expected(方案①): `grep -c . ~/.ssh/known_hosts` = 2(只剩 GitLab 两行)。

> ✅ 阶段 8 checkpoint:config 只剩 2 git Host + config.bak 删 + known_hosts 只剩 GitLab。回退点:阶段0备份。

---

## 阶段 9:iron rule 验证 + GitLab 回归

**目标:** 证明迁移达成——agent 直连 ssh 失败,MCP 可用,GitLab 仍可用。

- [ ] **9.1 iron rule:agent 直连 ssh <server> 必须失败**

```bash
ssh nuc10 hostname 2>&1 | head -3        # 应失败(无 config 别名 / 无私钥)
ssh 1660super01 hostname 2>&1 | head -3   # 应失败
ssh -i ~/.ssh/id_nuc10 allan716@192.168.100.235 hostname 2>&1 | head -3  # 应失败(私钥已删)
```
Expected: 都失败(如 `Could not resolve hostname` / `no such identity` / `Permission denied (publickey)`)。

- [ ] **9.2 MCP 路径仍可用(对照)**

笔记本 MCP(离线)`exec_command nuc10 hostname`。
Expected: 返回真实 hostname(如 `DESKTOP-...`/NUC10 名),exit 0。**证明命令路径已强制走 MCP。**

- [ ] **9.3 GitLab over ssh 仍可用(spec §6-7,真实 ca_things repo)**

```bash
cd /c/WorkSpace/ca_things/SW_System_BioChem_Develop
git fetch 2>&1 | head -5
```
Expected: fetch 成功(GitLab 私钥保留 + known_hosts 有 GitLab 两行,无未知主机提示)。
> 不再用不存在的 sw_dst(spec M2 修正)。

- [ ] **9.4 github 仍可用**

```bash
cd /c/WorkSpace/agent/ssh-manager-mcp && git fetch 2>&1 | head -3
```
Expected: fetch 成功(走 https,不依赖 ssh)。

- [ ] **9.5 全量目标机 MCP 可连(最终回归)**

对 vault 全量 9 台各 `exec_command hostname`(在线或离线)。
Expected: 9/9 exit 0。

> ✅ 阶段 9 checkpoint(=迁移完成):iron rule 生效(agent 直连失败)+ MCP 全量可用 + GitLab/github 仍可用。

---

## 阶段 10:收尾

- [ ] **10.1 更新项目 memory**

把本次迁移结果写进 `~/.claude/projects/.../memory/ssh-manager-mcp-project.md`:
- 两端升级到 v0.4.0(auto-TLS 强制 TLS + pin)。
- 笔记本 `~/.ssh` 清空(10 Host + 10 私钥删),agent 走离线 MCP。
- GitLab 私钥保留例外(ca_things 20 repo 依赖)。
- 旧明文设备码已吊销,新带 pin 设备码 `laptop` 在用。

- [ ] **10.2 push secret scan(本次接触的活 secret)**

按 [[ssh-manager-mcp-push-secret-scan]]:扫描本次会话接触的活 secret(新 v0.4.0 设备码、project token),确保零明文进 git。memory 里**不写活设备码明文**(只写"已发新码,旧码已吊销")。

- [ ] **10.3 提交计划执行记录(可选)**

把执行过程中发现的事实(如 172.18.200.47 同机结论、id_ed25519 归属)补进 spec 或 plan 的核对表。

---

## 回滚(spec §8)

| 阶段 | 回滚动作 |
|---|---|
| 升级(4-5) | 笔记本 exe 退回 `~/bin/ssh-manager-v0.3.1-backup.exe`;NUC10 退回 `ssh-manager-v0.3.1-backup.exe`;**重发无 pin 纯设备码**(v0.3.1 cache pull 不认 `:pin` 后缀);`.mcp.json` 退回在线 HTTP |
| 清理(7-8) | 从 `SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\` 全量 copy 回 `~/.ssh/`(明文备份,直接还原) |
| vault(1) | 补加的 ml_hub/ai_runner `servers delete` 或 `edit`(现有 7 台未动) |
| break-glass(serve 宕机) | 从 SynologyDrive 备份还原 NUC10 单台私钥 → 手动 ssh;或 NUC10 物理/RDP 控制台。**执行前确认 SynologyDrive 备份盘不依赖 NUC10** |

---

## Self-Review 结论(对照 spec)

- **spec 覆盖**:§1.3 散落地图→阶段0备份+阶段7-8清理;§2 边界→Global Constraints;§2.2 删除铁律→阶段6.5;§3 清理→阶段7-8;§4 升级→阶段3-4;§4.6 全局时序→10 阶段一一对应;§5 补机→阶段1;§6 验证→阶段9;§7 memory→阶段10.1;§8 回滚→回滚表。**全覆盖。**
- **无占位符**:所有命令含实际路径/flag;设备码/指纹/project token 标 `<...>` 因它们是执行时才生成的活 secret(不能写死),执行时从 4b.2/4b.3 取。
- **类型/命名一致**:`cache pull --token --pin`、`serve cert-info`、`cache-tokens add/revoke`、`servers add/grant/ls` 全部对照源码确认(serve.go:95/106, cache_tokens.go:22/80, cache.go:228-229)。
