# 从直接 SSH 迁移到 ssh-manager MCP（清理 ~/.ssh + 两端升级 v0.4.0）

> **日期**:2026-08-14 · **状态**:设计稿(待 review) · **类型**:运维迁移(非代码功能开发)
> **一句话**:把笔记本上"直接交给 AI agent 的 SSH 凭据"全部收回,agent 只能通过 ssh-manager MCP(NUC10 权威 broker)操作目标机;`~/.ssh` 彻底清空(仅保留 GitLab 一个 git 私钥例外);两端升级到 v0.4.0(auto-TLS)。

---

## 1. 背景与动机

### 1.1 现状

ssh-manager-mcp 项目已开发到 v0.3.1,多机 serve 架构(路线乙)于 2026-08-13 端到端验过并两端生产部署:

- **NUC10(192.168.100.235)**:权威 broker,`ssh-manager-serve` Windows Service(LocalSystem/Automatic),`serve --addr 0.0.0.0:7878`,**无 TLS**,vault 7 台真数据。
- **笔记本(DESKTOP-UGO0KBL)**:client,顶层 `.mcp.json` ssh 指向 NUC10 serve(**在线 HTTP 模式**),离线 cache.bin 7/7 + 30min 定时刷新。
- master 上 **auto-TLS 特性已合并(c42024c)但未发版**(v0.3.1 之后,无 tag/无 release)。

### 1.2 问题

用户之前把服务器 SSH 信息直接交给 AI agent(写进 `~/.ssh/`,agent 能 `ssh <别名>`/读私钥直接连)。本次目标:**全部收回**,改走本项目 MCP,落实项目的核心 iron rule——agent 永不触碰凭据,凭据只存在于加密 vault。

### 1.3 迁移前的散落地图(已盘点)

**A. `~/.ssh/config` 的 Host(11 个):**

| Host | HostName | User | Port | IdentityFile | 类别 |
|---|---|---|---|---|---|
| 1660super01 | 192.168.100.146 | US | 22 | ~/.ssh/id_1660super01_146 | 服务器(GPU) |
| 1660super02 | 192.168.100.236 | US | 22 | ~/.ssh/id_1660super02_236 | 服务器(GPU) |
| 3090x2 | 192.168.200.120 | urit_ai | — | (无 IdentityFile) | 服务器(GPU) |
| 4090x2 | 172.18.200.81 | root | — | ~/.ssh/id_ed25519_4090srv | 服务器(GPU) |
| nuc10 | 192.168.100.235 | allan716 | 22 | ~/.ssh/id_nuc10 | 服务器(broker 本身) |
| ml_hub | 172.18.200.47 | allan | 40101 | ~/.ssh/id_ml_hub | 服务器 |
| ai_runner | 192.168.100.201 | urit_ai | 22 | ~/.ssh/id_ai_runner_201 | 服务器 |
| procurement-recog | 172.18.200.46 | Administrator | 22 | ~/.ssh/id_procurement_recog_46 | 服务器 |
| update-hub | 172.18.200.47 | Administrator | 30000 | **SynologyDrive/...** | 服务器 |
| github.com | github.com | git | — | SynologyDrive/... | **git(留)** |
| 192.168.200.46 | — | git | 53802 | SynologyDrive/... | **git(GitLab,留)** |

**B. `~/.ssh/` 私钥文件(10 个,均明文):**
`id_1660super01_146`(+.pub)、`id_1660super02_236`(+.pub)、`id_4090x2.deprecated`、`id_ai_runner_201`(+.pub)、`id_ed25519`(默认,归属待核对)、`id_ed25519_4090srv`、`id_ml_hub`、`id_nuc10`、`id_procurement_recog_46`(+.pub)。

**C. 其他:**
- `~/.ssh/config.bak`(384B,旧 config 完整副本 = 泄露面)
- `~/.ssh/known_hosts` + `known_hosts.old`(目标机指纹,泄露面;MCP broker 自己有 host_keys 表做 TOFU,不依赖它)
- `~/.ssh/` 之外:**`C:\Users\allan716\SynologyDrive\ServerKey\`**(用户冷备份,含 github/gitlab/update-hub 私钥)—— **本次完全不碰**

**D. 全局 git 配置:**
`url.https://github.com/.insteadof git@github.com:` → github 已走 https,**不依赖 ssh 私钥** ✅。但 `C:/WorkSpace/agent/sw_dst` 的 remote 是 `ssh://git@192.168.200.46:53802/...`(GitLab over ssh)→ 依赖 GitLab 私钥。

---

## 2. 已确认的边界(决策记录)

经 brainstorming 多轮确认,本次迁移的硬边界:

1. **彻底清空 `~/.ssh`**,全走 MCP(iron rule 最纯粹形态)。
2. **唯一例外**:GitLab 私钥(`192.168.200.46` / sw_dst repo)保留——清空后该 repo push/pull 会断,用户选择保留这一个。
3. **`~/.ssh/config` 精确边界**:删 **8 个服务器 Host**,留 `github.com` + `192.168.200.46`(GitLab)两个 git Host。
4. **目标机全量纳入**:现有 vault 7 台 + `ml_hub` + `ai_runner` + 查漏补缺。
5. **版本基线 v0.4.0(auto-TLS)**:GoReleaser 发版后两端升级。
6. **现有 NUC10 vault 7 台保留**,只补加新机(不抹重建,不碰真数据)。
7. **拓扑 = 方案 A**:NUC10 继续权威 broker + 笔记本 client(复用 Plan 16 §7.3 已验收拓扑,最小改动)。
8. **`SynologyDrive\ServerKey\` 完全不碰**(用户冷备份,不读/不删/不纳入流程)。
9. **备份先于删除**:`~/.ssh/` 要删内容先镜像备份到 `SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\`(明文,与 ServerKey 现有习惯一致,符合用户接受的 L1+ 威胁模型)。

---

## 3. 清理策略(精确清单 + 安全顺序)

### 3.1 要删除的内容

**A. `~/.ssh/config` 删 8 个服务器 Host 段:**
`1660super01`、`1660super02`、`3090x2`、`4090x2`、`nuc10`、`ml_hub`、`ai_runner`、`procurement-recog`、`update-hub`。
> `update-hub` 的 IdentityFile 指向 SynologyDrive —— **只删 config 这段文字引用**,SynologyDrive 文件不动。

**B. `~/.ssh/` 私钥文件(迁进 vault + 验证可连后删):**
全部 10 个 `id_*` 私钥(+.pub)。归属待核对的(`id_ed25519` 默认键 / `id_4090x2.deprecated` 旧键)在补加阶段核对清楚再决定入 vault 还是直接弃。

**C. 留下的:**
- `config`:只剩 `github.com` + `192.168.200.46` 两段。
- `config.bak`:删(泄露面)。
- `known_hosts` + `known_hosts.old`:**清空**(MCP broker 不依赖,是泄露面)。

### 3.2 安全顺序(铁律:先验证可连,再删本地)

```
[0] 备份:~/.ssh/ 全量镜像 → SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\
[1] 补加 ml_hub/ai_runner/(查漏)私钥+主机信息灌进 NUC10 vault(servers add + grant)
[2] 每台目标机用 MCP exec_command 验证可连成功 ✅(全量通过才继续)
[3] 笔记本 cache pull 拉到全量 cache.bin,离线 mcp --cache 也验过
[4] 此时本地私钥已有 vault 替代,才开始删 ~/.ssh 私钥文件
[5] 删 config 服务器 Host 段
[6] 删 config.bak / 清空 known_hosts
[7] 验证 iron rule:agent 直连 ssh <server> 必须失败
```

**关键安全点:第 [2] 步全量验证通过前,绝不动 `~/.ssh`。** 万一某台机器私钥有问题,本地仍在,可回退。

### 3.3 备份细节

- 目标目录:`C:\Users\allan716\SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\`(新建子文件夹,**不触碰 ServerKey 现有任何文件**)。
- 备份内容:`~/.ssh/` 的完整镜像(config + config.bak + 全部 id_* + .pub + known_hosts + known_hosts.old)。
- 格式:明文(用户选择,与 ServerKey 现有 github/gitlab 明文私钥习惯一致;威胁模型 L1+ 已接受同机/离线拷盘风险)。
- 备份在 step [0] 完成、并确认备份可读后,才进入 step [4] 删除。

---

## 4. 部署 / 升级流程(发版 v0.4.0 + 两端升级)

### 4.1 发版 v0.4.0

走 Plan 9 既定的 GoReleaser 流程(tag 触发 GitHub Actions):
```
[1] 确认 master HEAD(7de4fb1)包含完整 auto-TLS(c42024c + xcheck 修复 53afc5d/3dfc69e)
[2] git tag v0.4.0 && git push origin v0.4.0
[3] release.yml 自动出 Release(6 平台 assets + checksums,~2min)
[4] 下载 windows-amd64 archive 备两端升级用
```
**Release notes 必须写明破坏性变更**:无 pin 的 `cache pull` 现 hard-fail,需配 pin 或 `--allow-plaintext`。

### 4.2 升级顺序铁律(spec §4.1)

> ⚠️ **先升全部工作机(笔记本)+ 配 pin → 最后才升 NUC10 serve。**
> 升 serve 瞬间其变 TLS-only,旧明文 client 直连会断(`malformed HTTP response`)。"不中断"仅在此协调前提下成立。

正确序列:
```
① 升级【笔记本】二进制 → v0.4.0,并先把 pin 备好
② NUC10 上(明文 serve 仍跑)跑:ssh-manager serve cert-info → 打印 fp=sha256:abcd...
   (幂等:证书已存在只读不写;明文部署此刻生成自签证书文件 + 打印指纹)
③ 笔记本注入 SSHMGR_SERVE_PIN=sha256:abcd...(或重发带指纹的设备码 <设备码>:<指纹>)
④【最后】重启 NUC10 serve → 监听强制 TLS(用 ② 的证书)
⑤ 笔记本下次 cache pull → 走 TLS + pinning 成功 → 升级完成
```

### 4.3 NUC10 升级(权威 broker)

```
[1] 笔记本端先就绪(见 4.2 ①③)——NUC10 serve 暂不动
[2] 跑 ssh-manager serve cert-info → 生成/确认证书 + 打印指纹
[3] cache-tokens add 重发带指纹的设备码(形态 A: <设备码>:<指纹>)
[4] 替换 ssh-manager.exe → v0.4.0
[5] 重启 serve install(Windows Service)→ 自起正常,强制 TLS
[6] vault 7 台保留不动(master.key.plain 沿用,不重生成)
```
> NUC10 已有 vault 7 台真数据,升级**只换二进制 + 重发带 pin 设备码**,不碰 vault 内容。

### 4.4 笔记本升级(client)+ .mcp.json 主路径切换

```
[1] 停旧定时任务 ssh-manager-cache-refresh(跑的是无 pin 旧码,升级后 hard-fail)
[2] 替换 ssh-manager.exe → v0.4.0(~/bin/)
[3] 注入 SSHMGR_SERVE_PIN=<②的指纹>(或用重发的 <设备码>:<指纹>)
[4] cache pull --url https://192.168.100.235:7878 --token '<新设备码>:<指纹>'
    → 验证 TLS + 指纹钉死生效,拉到 cache.bin
[5] 重发定时任务,wrapper 嵌新设备码(带 pin)
[6] 顶层 .mcp.json 的 ssh 条目改为主路径离线模式:
    { "command": "ssh-manager", "args": ["mcp", "--cache", "--token", "<项目token>"] }
    (原在线 HTTP 配置 {type:http,url:...} 退役或留作备用)
[7] cache status 验证 N/7
```

### 4.5 关键设计结论(auto-TLS 查证,§3.4)

- **auto-TLS 只管 `cache pull` ↔ `/snapshot` 同步链路**;pin 校验只在 `cache pull` 的 `pinningTransport`。
- **路线乙下 agent 日常主路径 = 离线 `mcp --cache`(stdio,零跨网络)**,不碰 serve,**无"在线 HTTP MCP 配 pin"问题**。
- **在线 HTTP MCP 是可选非主路径**,auto-TLS spec §2.3 明确不为它加 bridge。
- 因此笔记本 `.mcp.json` 切到 `mcp --cache` 离线主路径(quickstart 推荐),**在线 HTTP 配置退役**。
- agent 日常操作(exec/scp/forward)全在离线只读范畴内可用;加改删服务器才需在线模式。

---

## 5. 补加新机到 NUC10 vault

### 5.1 现有 vs 目标差异

vault 已有 7 台(memory):`1660Super01`、`1660Super02`、`3090x2`、`4090x2`、`DocuFiller-UpdateHub`(≈ update-hub)、`NUC10`、`procurement-recog`。

`~/.ssh/config` 有但 vault 可能缺的:**`ml_hub`(172.18.200.47:40101, allan)**、**`ai_runner`(192.168.100.201:22, urit_ai)**。

> 台数说明:现有 7 台 + 待补 ml_hub/ai_runner ≈ 9 台,但 `DocuFiller-UpdateHub` 是否等于 config 的 `update-hub`、有无重复/已废弃,以实现阶段实际核对为准(本 spec 不写死最终台数)。

### 5.2 补加流程

```
[1] 在 NUC10 用 ssh-manager servers add 录入 ml_hub / ai_runner
    (host/user/port + 从 ~/.ssh 对应私钥或密码取凭据)
[2] ssh-manager profiles grant <profile> ml_hub / ai_runner(纳入现有 e2e-profile)
[3] 用 MCP exec_command 验证两台可连 ✅
[4] 笔记本 cache pull 拉全量(N 台)
```

### 5.3 归属待核对(实现 plan 阶段处理)

- `~/.ssh/id_ed25519`(默认键,无明确指向)→ 核对对应哪台机或是否废弃。
- `~/.ssh/id_4090x2.deprecated`(已标 .deprecated)→ 核对 vault 现有 4090x2 凭据是否已是最新,此旧键可直接弃。
- 核对 vault 7 台现有凭据是否与 `~/.ssh/config` 当前值一致(用户选"保留现有 7 台",但若发现 config 与 vault 有偏差,记录差异让用户决定)。

---

## 6. 验证(成功标准)

迁移完成的可验证标准:

1. ✅ **`~/.ssh/` 清理**:`config` 仅剩 `github.com` + `192.168.200.46` 两段;无私钥文件(除保留项);`config.bak` 删;`known_hosts` 空。
2. ✅ **备份就位**:`SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\` 含完整镜像且可读。
3. ✅ **两端 v0.4.0**:NUC10 serve 强制 TLS(`serve cert-info` 出指纹);笔记本 `cache pull` 走 pinning 成功。
4. ✅ **全量目标机 MCP 可连**:笔记本 Claude Code 通过 MCP 对**每台**目标机(vault 全量 N 台)`exec_command <验证命令>` 成功(不只抽 1 台)。
5. ✅ **离线 cache 全量**:笔记本 `mcp --cache` list_servers = 全量 N 台。
6. ✅ **iron rule 生效**:删除后 agent 直连 `ssh <server别名>` **必须失败**(无 config/无私钥)。
7. ✅ **GitLab 仍可用**:`C:/WorkSpace/agent/sw_dst` 的 `git push/pull`(ssh://192.168.200.46:53802)仍正常(私钥保留)。
8. ✅ **github 仍可用**:常规 git push/pull 走 https 正常。

---

## 7. memory / 转录清理(审计项)

用户诉求含"清理之前记录的 SSH 信息"。核心对象是 `~/.ssh`(已覆盖 §3)。另需审计:

- **memory(`~/.claude/projects/.../memory/`)**:已 grep,无活凭据明文(无 token/设备码/master key 明文);仅有部署 IP(NUC10 192.168.100.235 = broker 地址,非凭据;192.168.200.120 已在 2026-08-10 public scrub 替换)。**结论:memory 无需改,保留架构描述**。
- **转录(`~/.claude.json` / 项目转录)**:可能含历史会话的临时凭据引用。属会话快照,删 `~/.ssh` 不影响。**本次不主动清理历史转录**(量大且为历史快照);若发现活凭据明文(能用的活 token/私钥)才单独脱敏。
- push 前按 [[ssh-manager-mcp-push-secret-scan]] 原则:secret scan 覆盖本次会话接触的所有活 secret(含新生成的 v0.4.0 设备码),活 secret 明文零容忍进 git。

---

## 8. 回滚计划

每个阶段都有独立回滚点(因 step [0] 已全量备份):

- **升级阶段回滚**:serve/笔记本二进制退回 v0.3.1;`.mcp.json` 退回在线 HTTP 配置;cache-pull.cmd 退回无 pin 版。auto-TLS cert 文件可留(下次升 v0.4.0 复用)。
- **清理阶段回滚**:`SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\` 全量还原 `~/.ssh/`(明文备份,直接 copy 回)。
- **vault 回滚**:补加的 ml_hub/ai_runner 若有误,`servers edit`/删除即可(现有 7 台未动,无数据丢失风险)。

---

## 9. 风险

| 风险 | 缓解 |
|---|---|
| 删私钥后发现某台 vault 凭据不对,连不上 | §3.2 铁律:全量 exec_command 验证通过才删;备份兜底 |
| auto-TLS 升级顺序错(先升 serve 致 client 断) | §4.2 铁律:先工作机+pin,最后 serve |
| 明文备份落 SynologyDrive 被云同步外泄 | 用户知情选择(与 ServerKey 现有习惯一致,L1+ 模型);若 SynologyDrive 同步开启,风险面=已有 github/gitlab 明文私钥等同级 |
| sw_dst GitLab push 断(GitLab 私钥误删) | §2 边界 2 明确保留 GitLab 私钥;验证标准 §6-7 含 sw_dst push 测试 |
| ml_hub/ai_runner 私钥不在 `~/.ssh/` 而在别处 | 补加阶段核对私钥实际位置(config 指向 ~/.ssh/id_ml_hub/id_ai_runner_201,已在) |
| 归属不明的私钥(id_ed25519 等)误删 | §5.3 归属核对先于删除,不明则暂留 |

---

## 10. 不在范围(YAGNI)

- ❌ 改 vault 已有 7 台数据(用户选保留)。
- ❌ 触碰 `SynologyDrive\ServerKey\` 任何现有文件。
- ❌ 在线 HTTP MCP 模式的 TLS bridge(路线乙非主路径,spec §2.3 已砍)。
- ❌ 历史转录主动清理(会话快照,量大;仅活凭据明文才单独处理)。
- ❌ 笔记本变权威 broker(方案 B 已否决)。
- ❌ master.key.plain 轮换(沿用现有,无泄露迹象)。

---

## 11. 相关文档

- auto-TLS spec:`docs/superpowers/specs/2026-08-13-serve-auto-tls-fingerprint-design.md`(升级顺序铁律 §4.1、pin 语义 §3.3、错误矩阵 §4.2)
- 多机快速上手:`docs/quickstart-multi-machine.md`(主路径 `mcp --cache` 配置、定时器模板)
- 多机详尽:`docs/multi-machine.md`
- 备份/恢复:`docs/backup-restore.md`
