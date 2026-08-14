# 从直接 SSH 迁移到 ssh-manager MCP（清理 ~/.ssh + 两端升级 v0.4.0）

> **日期**:2026-08-14 · **状态**:设计稿 v2(经 xcheck 异构评审 codex/kimi/opencode/pi 四家 + 主会话核实硬事实后修订) · **类型**:运维迁移(非代码功能开发)
> **一句话**:把笔记本上"直接交给 AI agent 的 SSH 凭据"全部收回,agent 的 SSH 操作路径只走 ssh-manager MCP(NUC10 权威 broker);`~/.ssh` 彻底清空(仅保留 GitLab git 私钥例外);两端升级到 v0.4.0(auto-TLS)。
>
> **v2 修订来源**:本轮 xcheck 抓到 3 个**硬事实错误**(主会话已逐条核实):① GitLab 依赖的仓库 `sw_dst` 不存在,真实依赖 = `C:/WorkSpace/` 下 20 个 repo;② 活 config 有缩进 Host `192.168.8.121` 被 `^Host` grep 漏掉(spec 盘点漏一台);③ 清空 known_hosts 会打断保留的 GitLab 直连。详见 `.xcheck/20260814-094215/SUMMARY.md`。

---

## 1. 背景与动机

### 1.1 现状

ssh-manager-mcp 项目已开发到 v0.3.1,多机 serve 架构(路线乙)于 2026-08-13 端到端验过并两端生产部署:

- **NUC10(192.168.100.235)**:权威 broker,`ssh-manager-serve` Windows Service(LocalSystem/Automatic),`serve --addr 0.0.0.0:7878`,**无 TLS**,vault 7 台真数据。
- **笔记本(DESKTOP-UGO0KBL)**:client,顶层 `.mcp.json` ssh 指向 NUC10 serve(**在线 HTTP 模式**),离线 cache.bin 7/7 + 30min 定时刷新。
- master 上 **auto-TLS 特性已合并(c42024c)但未发版**(v0.3.1 之后,无 tag/无 release)。

### 1.2 问题

用户之前把服务器 SSH 信息直接交给 AI agent(写进 `~/.ssh/`,agent 能 `ssh <别名>`/读私钥直接连)。本次目标:**全部收回**,改走本项目 MCP,落实项目的核心 iron rule——**agent 的 SSH 操作路径永不触碰凭据**,凭据只存在于加密 vault。

> **措辞收敛(xcheck S1)**:本方案达成的是"agent 的 **SSH 命令/工具路径** 不再能拿到/使用凭据(断掉 `ssh <别名>`、`~/.ssh` 私钥)",而非"凭据在物理上对 agent 进程彻底不可见"。原因见 §9 风险表:为容灾做的 `~/.ssh` 明文备份会落在 `SynologyDrive\ServerKey\` 下,而 agent 进程与本机用户同账户,**理论上可读该备份**(这是用户知情接受的 L1+ 残留,同 ServerKey 现状)。真正的 L2"凭据物理不可见"需把备份移出 agent 可达路径或加密——属未来加固,非本次范围。

### 1.3 迁移前的散落地图(已盘点 + xcheck 核实修正)

> **盘点方法说明(xcheck M3 修正)**:Host 清单用 `grep -iE "^\s*Host "`(含缩进),不要用 `grep "^Host "`——活 config 里 `192.168.8.121` 是**两空格缩进**写法,会被 `^Host` 漏掉。

**A. `~/.ssh/config` 的 Host(12 个):**

| Host | HostName | User | Port | IdentityFile | 类别 |
|---|---|---|---|---|---|
| 1660super01 | 192.168.100.146 | US | 22 | ~/.ssh/id_1660super01_146 | 服务器(GPU) |
| 1660super02 | 192.168.100.236 | US | 22 | ~/.ssh/id_1660super02_236 | 服务器(GPU) |
| 3090x2 | 192.168.200.120 | urit_ai | — | (无 IdentityFile,疑密码认证) | 服务器(GPU) |
| 4090x2 | 172.18.200.81 | root | — | ~/.ssh/id_ed25519_4090srv | 服务器(GPU) |
| nuc10 | 192.168.100.235 | allan716 | 22 | ~/.ssh/id_nuc10 | 服务器(broker 本身) |
| ml_hub | 172.18.200.47 | allan | 40101 | ~/.ssh/id_ml_hub | 服务器 |
| ai_runner | 192.168.100.201 | urit_ai | 22 | ~/.ssh/id_ai_runner_201 | 服务器 |
| procurement-recog | 172.18.200.46 | Administrator | 22 | ~/.ssh/id_procurement_recog_46 | 服务器 |
| update-hub | 172.18.200.47 | Administrator | 30000 | **SynologyDrive/...** | 服务器 |
| **192.168.8.121** | 192.168.8.121 | rag | — | **(无 IdentityFile,缩进写法,疑密码认证)** | **服务器(xcheck 补,待用户定归属)** |
| github.com | github.com | git | — | SynologyDrive/... | **git(留)** |
| 192.168.200.46 | — | git | 53802 | SynologyDrive/... | **git(GitLab,留)** |

> **待用户决策的归属项**:① `192.168.8.121`(rag,known_hosts 有 3 条=真在用,无私钥):纳入 vault / 删除 / 保留?② `3090x2` 与 `192.168.8.121` 均无私钥文件,若纳入 vault 需用密码认证,且**无 `~/.ssh` 私钥可作验证失败兜底**(见 §3.2 L5 特例)。

**B. `~/.ssh/` 私钥文件(10 个,均明文):**
`id_1660super01_146`(+.pub)、`id_1660super02_236`(+.pub)、`id_4090x2.deprecated`、`id_ai_runner_201`(+.pub)、`id_ed25519`(默认,归属待核对)、`id_ed25519_4090srv`、`id_ml_hub`、`id_nuc10`、`id_procurement_recog_46`(+.pub)。

**C. 其他:**
- `~/.ssh/config.bak`(384B,**仅含 github + GitLab 两段** = 引用 SynologyDrive 私钥路径,泄露面较小但仍建议删)
- `~/.ssh/known_hosts` + `known_hosts.old`(目标机指纹,泄露面;MCP broker 自己有 host_keys 表做 TOFU,不依赖它。⚠️ 但 `192.168.200.46` GitLab 直连 ssh **依赖**它,清空前需处理,见 §3.1)
- `~/.ssh/` 之外:**`C:\Users\allan716\SynologyDrive\ServerKey\`**(用户冷备份,含 github/gitlab/update-hub 私钥)—— **本次完全不碰**

**D. GitLab over ssh 的真实依赖面(xcheck M2 核实修正):**
`url.https://github.com/.insteadof git@github.com:` → github 已走 https,**不依赖 ssh 私钥** ✅。
GitLab(`ssh://git@192.168.200.46:53802/...`)的依赖**远超原 spec 误写的单个 sw_dst repo**——主会话扫 `C:/WorkSpace/` 实测 **20 个 repo** 依赖此 GitLab over ssh(分散在 ca_things/5、dotnet/2、ic_encode_system/6、ml-things/1、opencv/1、PythonThings/5)。其中 `C:/WorkSpace/agent/sw_dst` **已不存在**。→ **保留 GitLab 私钥的必要性非常强**(清掉会断 20 个工作 repo 的 push/pull)。

---

## 2. 已确认的边界(决策记录)

经 brainstorming 多轮确认,本次迁移的硬边界:

1. **彻底清空 `~/.ssh`**,全走 MCP(iron rule 最纯粹形态)。
2. **唯一例外**:GitLab 私钥(`192.168.200.46` / GitLab over ssh)保留——清空后 `C:/WorkSpace/` 下 **20 个** GitLab repo 的 push/pull 会断(实测依赖面),用户选择保留这一个。
3. **`~/.ssh/config` 精确边界**:删 **9 个**服务器 Host(xcheck L1 修正:11 − 2 git = 9,原写"8 个"是误数),留 `github.com` + `192.168.200.46`(GitLab)两个 git Host。⚠️ `192.168.8.121`(rag,缩进 Host)是否删除**待用户定**(见 §1.3 待决项),定删后则删 10 个。
4. **目标机全量纳入**:现有 vault 7 台 + `ml_hub` + `ai_runner` + 可能的 `192.168.8.121` + 查漏补缺。
5. **版本基线 v0.4.0(auto-TLS)**:GoReleaser 发版后两端升级。
6. **现有 NUC10 vault 7 台保留**,只补加新机(不抹重建,不碰真数据)。
7. **拓扑 = 方案 A**:NUC10 继续权威 broker + 笔记本 client(复用 Plan 16 §7.3 已验收拓扑,最小改动)。
8. **`SynologyDrive\ServerKey\` 完全不碰**(用户冷备份,不读/不删/不纳入流程)。
9. **备份先于删除**:`~/.ssh/` 要删内容先镜像备份到 `SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\`(明文,与 ServerKey 现有习惯一致,符合用户接受的 L1+ 威胁模型)。⚠️ xcheck S6:备份前须检查 SynologyDrive Cloud Sync 状态(备份会把当前**只在本地**的 9+ 服务器私钥一并推上云,暴露面**净增大**而非等同级),若同步开启则暂停或换不同步路径。

### 2.1 待用户拍板项(xcheck 后新增的决策点)

| 项 | 说明 | 影响 |
|---|---|---|
| `192.168.8.121`(rag)归属 | 缩进 Host,known_hosts 3 条=真在用,无私钥(疑密码) | 纳入 vault / 删除 / 显式保留 三选一 |
| NUC10 vault 灾备源确认 | 删光本地私钥前,NUC10 vault 是唯一权威源 | 需确认 NUC10 有 Plan-13 NAS 备份或显式把 ~/.ssh 明文备份当 DR 源(S4) |
| SynologyDrive 云同步开关 | 明文备份是否会被云同步 | 备份前检查,决定是否暂停同步(S6) |

---

## 3. 清理策略(精确清单 + 安全顺序)

### 3.1 要删除的内容

**A. `~/.ssh/config` 删 9 个服务器 Host 段(xcheck L1 修正,实际 9 个非 8):**
`1660super01`、`1660super02`、`3090x2`、`4090x2`、`nuc10`、`ml_hub`、`ai_runner`、`procurement-recog`、`update-hub`。
> `update-hub` 的 IdentityFile 指向 SynologyDrive —— **只删 config 这段文字引用**,SynologyDrive 文件不动。
> 若 §2.1 用户决定 `192.168.8.121` 也删,则此处为 10 个。

**B. `~/.ssh/` 私钥文件(迁进 vault + 验证可连后删):**
全部 10 个 `id_*` 私钥(+.pub)。归属待核对的(`id_ed25519` 默认键 / `id_4090x2.deprecated` 旧键)在补加阶段核对清楚再决定入 vault 还是直接弃。

**C. 保留 / 处理其余项(xcheck L3 修正:原"留下的"标题下混了删除项,此处拆清):**
- **保留**:`config` 只剩 `github.com` + `192.168.200.46`(GitLab)两段。
- **删**:`config.bak`(含 SynologyDrive 私钥路径引用,虽仅 2 段仍删)。
- **谨慎处理(不是简单清空,xcheck M5)**:`known_hosts` + `known_hosts.old`。
  - `192.168.200.46` GitLab 直连 ssh **依赖** known_hosts(有 2 条)。直接清空会让保留的 GitLab 首次 push 撞 "authenticity can't be established",非交互场景失败。
  - **正确做法二选一**:① 清空 known_hosts 但**单独保留** `192.168.200.46` 那两行;② 或清空后对 GitLab 设 `StrictHostKeyChecking accept-new`,首连自动接受;③ 或先 `ssh-keyscan -p 53802 192.168.200.46 >> ~/.ssh/known_hosts` 预置。
  - 其余目标机指纹可清(broker 用自己的 host_keys 表做 TOFU,不依赖 known_hosts)。

### 3.2 安全顺序(铁律:先验证可连,再删本地)

> ⚠️ **§3 清理 与 §4 升级 是同一条主线的两段,不是独立模块(xcheck M4)**。完整全局时序见 §4.6。本节只列清理段内的相对顺序。两者的拼接顺序:补机器 → cache pull → 升级 v0.4.0 TLS → 切离线模式 → 验离线 → 删私钥。

```
[0] 备份:~/.ssh/ 全量镜像 → SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\
    (备份前先检查 SynologyDrive 云同步状态,见 §3.3)
[1] 补加 ml_hub/ai_runner/(192.168.8.121 若定纳入)私钥或密码+主机信息灌进 NUC10 vault(servers add + grant)
    ⚠️ 私钥跨机传输路径见 §5.2(xcheck S2)
[2] 每台目标机用 MCP exec_command 验证可连成功 ✅(全量通过才继续)
    ⚠️ L5 特例:3090x2 / 192.168.8.121 无本地私钥,验证失败时无 ~/.ssh key 兜底,只能靠密码重试或 SynologyDrive 备份还原
[3] 笔记本 cache pull 拉到全量 cache.bin,离线 mcp --cache 也验过
[4] 此时本地私钥已有 vault 替代,才开始删 ~/.ssh 私钥文件
[5] 删 config 服务器 Host 段(9 个,或 10 个含 192.168.8.121)
[6] 删 config.bak;known_hosts 按§3.1C 三选一处理(保留 GitLab 那两行)
[7] 验证 iron rule:agent 直连 ssh <server> 必须失败
```

**关键安全点:第 [2] 步全量验证通过前,绝不动 `~/.ssh`。** 万一某台机器私钥有问题,本地仍在,可回退。

### 3.3 备份细节

- 目标目录:`C:\Users\allan716\SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\`(新建子文件夹,**不触碰 ServerKey 现有任何文件**)。
- 备份内容:`~/.ssh/` 的完整镜像(config + config.bak + 全部 id_* + .pub + known_hosts + known_hosts.old)。
- 格式:明文(用户选择,与 ServerKey 现有 github/gitlab 明文私钥习惯一致;威胁模型 L1+ 已接受同机/离线拷盘风险)。
- 备份在 step [0] 完成、并确认备份可读后,才进入 step [4] 删除。
- **⚠️ 备份前必做(xcheck S6):检查 SynologyDrive Cloud Sync 进程状态**。当前 9+ 服务器私钥**只在笔记本本地**;备份把它们复制进云同步目录后,暴露面**净增大**(严格大于,非等同级 ServerKey 现状)。若同步开启:暂停同步、或把备份放同步范围外。此风险单列在 §9。

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
① 升级【笔记本】二进制 → v0.4.0(xcheck L9:此处不涉及 pin,pin 来自 ②)
② NUC10 上(明文 serve 仍跑)取指纹(见 §4.3 重排后的 cert-info 步骤)→ fp=sha256:abcd...
   (幂等:证书已存在只读不写;明文部署此刻生成自签证书文件 + 打印指纹)
③ 笔记本注入 SSHMGR_SERVE_PIN=sha256:abcd...(或重发带指纹的设备码 <设备码>:<指纹>)
④【最后】重启 NUC10 serve → 监听强制 TLS(用 ② 的证书)
⑤ 笔记本下次 cache pull → 走 TLS + pinning 成功 → 升级完成
```

### 4.3 NUC10 升级(权威 broker)

> ⚠️ **cert-info 步骤重排(xcheck M1,must-fix)**:`cert-info` 是 auto-TLS 子命令(commit `3ab3103`),**不在 v0.3.1 内**。NUC10 当前跑的是 v0.3.1,直接跑 `ssh-manager serve cert-info` 会 `unknown command`。且 Windows 下运行中 Service 的 exe 被锁不能直接覆盖。**利用"替换磁盘 exe 不影响内存中已运行服务进程"特性**,正确顺序:

```
[1] 笔记本端先就绪(见 4.2 ①③)——NUC10 serve 暂不动
[2] 把 v0.4.0 的 ssh-manager.exe 放到 NUC10 一个 staging 路径(如 C:\ProgramData\ssh-manager\ssh-manager-v0.4.0.exe)
    (旧 v0.3.1 serve 进程仍在内存跑,不受影响)
[3] 用 staging 的 v0.4.0 二进制跑 cert-info,生成/确认证书 + 打印指纹:
       & C:\ProgramData\ssh-manager\ssh-manager-v0.4.0.exe serve cert-info  → fp=sha256:abcd...
    (幂等,只写证书文件 + 打印指纹;旧 serve 进程仍不受影响)
[4] cache-tokens add 重发带指纹的设备码(形态 A: <设备码>:<指纹>)
    ⚠️ xcheck S3:重发后须吊销旧明文设备码(cache-tokens revoke <旧码>),否则 pinning 收益打折
[5] 备份旧 v0.3.1 exe(xcheck S5:回滚要用),替换正式路径 exe 为 v0.4.0
[6] 重启 serve install(Windows Service)→ 自起正常,强制 TLS
[7] vault 7 台保留不动(master.key.plain 沿用,不重生成)
```
> NUC10 已有 vault 7 台真数据,升级**只换二进制 + 重发带 pin 设备码 + 吊销旧码**,不碰 vault 内容。

### 4.4 笔记本升级(client)+ .mcp.json 主路径切换

```
[1] 停旧定时任务 ssh-manager-cache-refresh(跑的是无 pin 旧码,升级后 hard-fail)
[2] 备份旧 v0.3.1 exe(xcheck S5:回滚要用),替换 ssh-manager.exe → v0.4.0(~/bin/)
[3] 注入 SSHMGR_SERVE_PIN=<§4.3[3]的指纹>(或用重发的 <设备码>:<指纹>)
[4] cache pull --url https://192.168.100.235:7878 --token '<新设备码>:<指纹>'
    → 验证 TLS + 指纹钉死生效,拉到 cache.bin
[5] 重发定时任务,wrapper 嵌新设备码(带 pin)
[6] 顶层 .mcp.json 的 ssh 条目改为主路径离线模式:
    { "command": "ssh-manager", "args": ["mcp", "--cache", "--token", "<项目token>"] }
    (原在线 HTTP 配置 {type:http,url:...} 退役或留作备用)
[7] cache status 验证 N/N(xcheck L2:原写"N/7"是旧 7 台残留,应为全量 N 台)
```

### 4.5 关键设计结论(auto-TLS 查证,§3.4)

- **auto-TLS 只管 `cache pull` ↔ `/snapshot` 同步链路**;pin 校验只在 `cache pull` 的 `pinningTransport`。
- **路线乙下 agent 日常主路径 = 离线 `mcp --cache`(stdio,零跨网络)**,不碰 serve,**无"在线 HTTP MCP 配 pin"问题**。
- **在线 HTTP MCP 是可选非主路径**,auto-TLS spec §2.3 明确不为它加 bridge。
- 因此笔记本 `.mcp.json` 切到 `mcp --cache` 离线主路径(quickstart 推荐),**在线 HTTP 配置退役**。
- agent 日常操作(exec/scp/forward)全在离线只读范畴内可用;加改删服务器才需在线模式。

### 4.6 全局执行时序(§3 清理 + §4 升级 合并,xcheck M4 must-fix)

两条主线有交叉依赖,必须钉死成单一可执行时序(不靠推断):
```
阶段 0  备份:~/.ssh/ 全量镜像(先查 SynologyDrive 云同步,§3.3)
阶段 1  补加新机:ml_hub/ai_runner/(192.168.8.121 若定)私钥或密码灌进 NUC10 vault(§5.2 跨机传输)
阶段 2  全量验证:每台目标机 MCP exec_command 可连 ✅(全过才继续)
        ┄┄ 此时笔记本仍是 v0.3.1 + 明文 serve,~/.ssh 私钥仍在,有回退 ┄┄
阶段 3  发版 v0.4.0 + 下载(§4.1)
阶段 4  升级顺序(铁律 §4.2):先笔记本二进制+pin → NUC10 cert-info(§4.3 重排)→ 重发带 pin 设备码+吊销旧码 → 最后重启 serve 强制 TLS
阶段 5  笔记本 cache pull 走 TLS+pin 成功;切 .mcp.json 离线主路径;cache status N/N
阶段 6  离线验证:mcp --cache list_servers = 全量;离线 exec 可连
阶段 7  删 ~/.ssh 私钥文件(此时 vault 已是权威源,本地有备份)
阶段 8  删 config 服务器 Host 段;删 config.bak;known_hosts 按 §3.1C 处理(留 GitLab 两行)
阶段 9  iron rule 验证:agent 直连 ssh <server> 必须失败;GitLab push/pull 仍正常
```
> **关键**:清理(删 ~/.ssh)放在升级 + 离线验证**之后**(阶段 7-9),确保删之前 vault 已就位且 TLS 已通。回滚点:每个阶段独立(见 §8)。
> **S4 提醒**:阶段 7 删光本地副本前,先确认 NUC10 vault 有灾备(Plan-13 NAS 或显式把 ~/.ssh 明文备份当 DR 源),否则 NUC10 盘损 = 全凭据失联。

---

## 5. 补加新机到 NUC10 vault

### 5.1 现有 vs 目标差异

vault 已有 7 台(memory):`1660Super01`、`1660Super02`、`3090x2`、`4090x2`、`DocuFiller-UpdateHub`(≈ update-hub)、`NUC10`、`procurement-recog`。

`~/.ssh/config` 有但 vault 可能缺的:**`ml_hub`(172.18.200.47:40101, allan)**、**`ai_runner`(192.168.100.201:22, urit_ai)**、可能的 **`192.168.8.121`(rag,待用户定)**。

> 台数说明:现有 7 台 + 待补 ml_hub/ai_runner ≈ 9 台,但 `DocuFiller-UpdateHub` 是否等于 config 的 `update-hub`、有无重复/已废弃,以实现阶段实际核对为准(本 spec 不写死最终台数)。
> **核对项(xcheck L6/L7)**:① `172.18.200.47` 双机——ml_hub(40101/allan)与 update-hub/DocuFiller-UpdateHub(30000/Administrator)同 IP 不同端口,核对是否同一物理机(避免重复计数/凭据重叠)。② 坐实 `DocuFiller-UpdateHub == update-hub`(host:port:user 一致),否则删 config 的 update-hub 引用后会失联。

### 5.2 补加流程(含私钥跨机传输路径,xcheck S2)

> ⚠️ ml_hub/ai_runner 的私钥在**笔记本** `~/.ssh/`,vault 在 **NUC10**——这是全方案凭据最敏感的跨机流动环节,必须写清传输路径,不能走未升级前的明文 HTTP serve。

```
[1] 确定传输路径(三选一,优先级从高到低):
    a) 人坐到 NUC10 前(RDP/console),把私钥内容手动粘贴到 servers add 交互(私钥不落 NUC10 磁盘临时文件);
    b) 或用已升级 TLS 后的 serve 在线模式 servers add(私钥走 TLS+pinning,不走明文);
    c) 或临时 scp 私钥到 NUC10 → servers add → 立即 shred 临时文件。
    ❌ 禁止:在 NUC10 还是 v0.3.1 明文 serve 时走 HTTP 远程 servers add(= 内网明文传私钥)。
[2] 在 NUC10 用 ssh-manager servers add 录入 ml_hub / ai_runner(host/user/port + 凭据)
[3] ssh-manager profiles grant <profile> ml_hub / ai_runner(纳入现有 e2e-profile)
[4] 用 MCP exec_command 验证两台可连 ✅
[5] 笔记本 cache pull 拉全量(N 台)
[6] 跨机传输用的任何临时私钥文件 shred 清除(路径 a 无临时文件;c 必清)
```

### 5.3 归属待核对(实现 plan 阶段处理)

- `~/.ssh/id_ed25519`(默认键,无明确指向)→ 核对对应哪台机或是否废弃。
- `~/.ssh/id_4090x2.deprecated`(已标 .deprecated)→ 核对 vault 现有 4090x2 凭据是否已是最新,此旧键可直接弃。
- 核对 vault 7 台现有凭据是否与 `~/.ssh/config` 当前值一致(用户选"保留现有 7 台",但若发现 config 与 vault 有偏差,记录差异让用户决定)。
- **3090x2 / 192.168.8.121 无私钥文件(xcheck L5)**:确认其认证方式(密码?默认 key?),若纳入 vault 用密码;验证失败时无 ~/.ssh 兜底,恢复路径 = 密码重试或 SynologyDrive 备份还原。

---

## 6. 验证(成功标准)

迁移完成的可验证标准:

1. ✅ **`~/.ssh/` 清理**:`config` 仅剩 `github.com` + `192.168.200.46` 两段;无私钥文件(L4:GitLab/GitHub 私钥本就在 SynologyDrive,清理后 `~/.ssh` 应**无任何**私钥,不存在"保留项"私钥);`config.bak` 删;`known_hosts` 除保留的 GitLab 两行外清空。
2. ✅ **备份就位**:`SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\` 含完整镜像且可读。
3. ✅ **两端 v0.4.0**:NUC10 serve 强制 TLS(用 staging v0.4.0 跑 cert-info 出指纹);笔记本 `cache pull` 走 pinning 成功。
4. ✅ **全量目标机 MCP 可连**:笔记本 Claude Code 通过 MCP 对**每台**目标机(vault 全量 N 台)`exec_command <验证命令>` 成功(不只抽 1 台)。
5. ✅ **离线 cache 全量**:笔记本 `mcp --cache` list_servers = 全量 N 台。
6. ✅ **iron rule 生效(命令路径)**:删除后 agent 直连 `ssh <server别名>` **必须失败**(无 config/无私钥)。⚠️ 见 §9:这不等于凭据物理不可见(备份仍在 agent 可读路径)。
7. ✅ **GitLab 仍可用**(xcheck M2 修正):对**真实存在的** `C:/WorkSpace/ca_things/` 下某 repo(如 `SW_System_BioChem_Develop`)跑 `git fetch/pull`(ssh://192.168.200.46:53802)成功。⚠️ 不再用不存在的 sw_dst。
8. ✅ **github 仍可用**:常规 git push/pull 走 https 正常。
9. ✅ **GitLab host-key TOFU 已处理**(xcheck M5):首次 GitLab push 不撞未知主机提示(known_hosts 保留 GitLab 两行 / 或 accept-new / 或 ssh-keyscan 预置)。
10. ✅ **vault 灾备就绪**(xcheck S4):NUC10 vault 有 Plan-13 NAS 备份,或显式确认 ~/.ssh 明文备份为 DR 源。

---

## 7. memory / 转录清理(审计项)

用户诉求含"清理之前记录的 SSH 信息"。核心对象是 `~/.ssh`(已覆盖 §3)。另需审计:

- **memory(`~/.claude/projects/.../memory/`)**:已 grep,无活凭据明文(无 token/设备码/master key 明文);仅有部署 IP(NUC10 192.168.100.235 = broker 地址,非凭据;192.168.200.120 已在 2026-08-10 public scrub 替换)。**结论:memory 无需改,保留架构描述**。
- **转录(`~/.claude.json` / 项目转录)**:可能含历史会话的临时凭据引用。属会话快照,删 `~/.ssh` 不影响。**本次不主动清理历史转录**(量大且为历史快照);若发现活凭据明文(能用的活 token/私钥)才单独脱敏。
- push 前按 [[ssh-manager-mcp-push-secret-scan]] 原则:secret scan 覆盖本次会话接触的所有活 secret(含新生成的 v0.4.0 设备码),活 secret 明文零容忍进 git。

---

## 8. 回滚计划

每个阶段都有独立回滚点(因 step [0] 已全量备份 + 升级前已备份旧 exe):

- **升级阶段回滚(xcheck S5 + L8)**:
  - serve/笔记本二进制退回**已备份的** v0.3.1 exe(替换前已备份,见 §4.3[5]/§4.4[2])。
  - `.mcp.json` 退回在线 HTTP 配置;cache-pull.cmd 退回无 pin 版。
  - **⚠️ 设备码格式回滚(xcheck L8)**:退回 v0.3.1 serve 时,须 `cache-tokens` 重发**无 pin 的纯设备码**(v0.3.1 的 cache pull 不认 `:pin` 后缀,带 pin 码会解析失败)。
  - auto-TLS cert 文件可留(下次升 v0.4.0 复用)。
- **清理阶段回滚**:`SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\` 全量还原 `~/.ssh/`(明文备份,直接 copy 回)。
- **vault 回滚**:补加的 ml_hub/ai_runner 若有误,`servers edit`/删除即可(现有 7 台未动,无数据丢失风险)。
- **break-glass(xcheck S7,codex/kimi)**:迁移后 NUC10 本身是被管目标机,若 serve 宕机 + 私钥已删 = 无路径上去。break-glass = 从 `SynologyDrive\ServerKey\ssh-dot-ssh-backup-2026-08-14\` 还原 NUC10 单台私钥 → 手动 ssh 上去运维;或走 NUC10 物理/RDP 控制台。**此路径须在执行前确认 SynologyDrive 备份可离线访问**(若 NUC10 宕机导致 SynologyDrive 同步盘不可达,备份也读不到 → 额外确认备份所在盘不依赖 NUC10)。

---

## 9. 风险

| 风险 | 严重度 | 缓解 |
|---|---|---|
| **iron rule 物理层未达成**(xcheck S1,kimi/opencode) | 高(认知) | 明文备份在 `SynologyDrive\ServerKey\`(agent 同账户可读),agent 命令路径断了但物理可读。**这是用户知情接受的 L1+ 残留**,非 bug。§1.2 已收敛措辞为"命令路径不碰凭据";真正的 L2 物理不可见需移盘/加密,属未来加固。 |
| **明文备份使云同步暴露面净增大**(xcheck S6) | 高 | 备份把当前只在本地的 9+ 服务器私钥推上云。§3.3 备份前必查 SynologyDrive Cloud Sync 状态,开启则暂停/换路径。 |
| 删私钥后发现某台 vault 凭据不对,连不上 | 中 | §3.2 铁律:全量 exec_command 验证通过才删;备份兜底 |
| auto-TLS 升级顺序错(先升 serve 致 client 断) | 中 | §4.2 铁律:先工作机+pin,最后 serve |
| **cert-info 在 v0.3.1 不存在**(xcheck M1) | 高(已修) | §4.3 已重排:用 staging v0.4.0 跑 cert-info,不靠运行中的 v0.3.1 |
| **私钥跨机明文传输**(xcheck S2) | 中 | §5.2 路径 a 优先(RDP 手粘,不落临时文件);禁止 v0.3.1 明文 serve 远程 add |
| **3090x2/192.168.8.121 无本地 key 兜底**(xcheck L5) | 中 | §5.3 标注特例,验证失败靠密码重试或备份还原 |
| GitLab push 断(私钥误删) | 中 | §2 边界 2 保留 GitLab 私钥;§6-7 对真实 ca_things repo 验证 |
| **NUC10 broker 单点,盘损=全凭据失联**(xcheck S4) | 中 | §6-10 验证 vault 有 Plan-13 NAS 备份;删光本地前确认 DR 源 |
| **break-glass 路径依赖 NUC10 可达**(xcheck S7) | 中 | §8 执行前确认 SynologyDrive 备份盘不依赖 NUC10 |
| 旧明文设备码未吊销 | 中 | §4.3[4] 重发后 revoke 旧码 |
| 归属不明的私钥(id_ed25519 等)误删 | 低 | §5.3 归属核对先于删除,不明则暂留 |

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
