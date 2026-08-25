# 威胁模型（Threat Model）

> 本篇记录 ssh-manager-mcp **当前**（Plan 16，v0.3.0+）的威胁模型等级、适用前提、残留风险与未来升级路径。设计来源：[Plan 16 §1.3 / §6](./superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md)。
>
> 一句话：**L1+** ——固定路径裸文件 + 文件 ACL。保护"同机非特权进程意外读到凭据"，**不**保护 admin/root 或离线拷盘。这是用户在 Plan 16 grilling 中显式签字接受的降级（从 Plan 1-15 的 L2）。

---

## 1. 当前等级：L1+

| 维度 | 说明 |
|---|---|
| master.key 形态 | **裸明文文件**（`master.key.plain`）+ 文件 ACL/权限位 |
| 保护 | 同机**非特权进程**意外读（Windows ACL 移除 `Users`/`Authenticated Users`/`Everyone`，禁用继承；Unix `0600`/`0700`） |
| **不**保护 | admin/root 直接读 `master.key` 明文 → 解开 vault 全部 SSH 私钥；离线拷盘 |

**与 Plan 1-15（L2）的对比**（见 [Plan 16 §1.3](./superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md)）：

| | 旧（Plan 1-15，L2） | 新（Plan 16，L1+） |
|---|---|---|
| 等级 | L2（agent 永不碰凭据 + 用户态密钥保护） | **L1+**（固定路径裸文件 + ACL） |
| 防"同机另一用户/非登录态进程读凭据" | ✅ DPAPI/keyring 绑用户态 | ❌ admin/root 可读明文 master.key → 解开 vault 全部 SSH 私钥明文 |
| 防"离线拷盘" | ✅ keychain 不落盘 | ❌ master.key 裸文件，拷盘即得 |
| 适用前提 | 多用户 / 不可信机器 | **单用户、机器可信**（用户明确接受） |

> **降级根因**（不是"选错了 scope"，是"整个用户态密钥模型与部署形态不匹配"）：Plan 14（user-scope DPAPI）和 Plan 15（machine-scope DPAPI）在 NUC10 真机 §7.3 验收中**两次撞墙**——"服务自起、跨 logon session、headless"形态下，DPAPI 跨 session 不可解（`Key not valid for use in specified state`）。详见 [Plan 16 §1.1](./superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md)。

### 1.1 传输层（serve ↔ 工作机，独立于上面的 at-rest 模型）

`serve`↔`cache pull` 同步链路的传输加密**与 L1+ at-rest 模型是两套独立的密码学**：

- **默认强制 TLS**：`serve` 无 `--tls-cert` 时首次启动**自签**一张 ed25519 证书（落 `serve-cert.pem`/`serve-key.pem`，ACL 与 `master.key.plain` 同级），从此监听 TLS。`/snapshot`（整库凭据 dump）与 MCP JSON-RPC 全程密文 + 前向保密。
- **无 pin 默认 hard-fail（修订，xcheck 共识 C）**：客户端没拿到指纹（env/`--pin`/token 内嵌三处都无）→ **默认拒连**（不再静默明文回退 —— 那是 fail-open 隐患）。明文拉取需显式 `--allow-plaintext` opt-in（仅调试/连旧明文 serve）。有 pin 但 URL 非 https / pin 格式非法 也 hard-fail（防 pin 静默失效 / 防打错别字降级）。serve 证书误删（marker 仍在）→ serve 拒启动（防静默重生新 key 致全客户端 bricked）。
- **指纹钉死（SPKI TOFU）**：客户端（`cache pull`）钉死 serve 证书公钥的 SPKI 指纹（`sha256:...`）——`InsecureSkipVerify: true` 跳过对自签证书不可能的 CA 链验证 + `VerifyConnection` 回调做**常量时间** SPKI 比对作为唯一信任锚（HPKP/Tailscale 模式）。**首次连接即校验，零 MITM 窗口**（指纹在 enroll 时随设备码交付，非首次盲连）。
- **信任根**：信任来自"enroll 时人工/流程交接的指纹"，不来自任何 CA。serve 重生 key（重装/迁移）→ 用 `serve cert-info` 拿新指纹重新交接。
- **⚠️ 前提：enroll 渠道本身必须可信**。"零 MITM 窗口"是对**首次连接之后的每次握手**而言——指纹一旦正确到达工作机，后续 MITM 都被挡。但指纹和设备码是 `cache-tokens add` **一起打印到 stdout** 的，所以两者同等依赖操作者把这条输出传到工作机的那条渠道（本地 console / 你信任的 SSH 会话 / 带外通道）。**若该渠道本身正被 MITM，指纹和设备码同时被换，pin 形同虚设**。这是任何"交付即信任"(TOFU-by-delivery) 方案的固有约束：首次 enroll 不要在被 MITM 的渠道上做。
- **`cache.auth.json`（新增 artifact）**：工作机持久化的拉取凭据（url + 设备码 + 归一后 pin；0600，Windows 加 DACL）。设备码授予**拉取未来快照**的权力——比本机已有的 cache.bin（过去快照 + cache-dek.key）多出的正是这个增量。处置：机器失窃 → 立即 `cache-tokens revoke`（断"拉新" + 该机**回连即销毁**本地 cache，见 §3.6）；serve 证书轮换 → 手动带新 `--pin` 重拉一次覆盖旧 pin。自动路径永不 `--allow-plaintext`。

详见 [multi-machine.md 的自动 TLS 迁移 Runbook](./multi-machine.md#自动-tls-迁移-runbook从旧版明文--外部证书升级) 与设计 spec `docs/superpowers/specs/2026-08-13-serve-auto-tls-fingerprint-design.md`。

---

## 2. 适用前提（必须全部成立）

L1+ 模型**仅在以下前提成立时**安全。任一不成立，**不要**部署本方案（先升级，见 §4）：

- **M1：单用户机器**（或所有用户互信）。同机没有不可信的低权用户/服务。
- **M2：机器物理 + 管理面可信**。admin/root 不被恶意方获取；机器不离开你的物理管控（防"离线拷盘"靠的是物理 + 启动锁，不是密码学）。
- **M3：不需要防"同机另一低权用户/服务"读凭据**。也就是说：本机任何一个能拿到 admin/root 或物理接触的实体，本来就是你信任的。

**典型适用场景**：家用 VLAN 中的 NUC / 软路由 / 单 owner 工作站——一台机器一个主人，物理可控，admin 即本人。

**典型不适用场景**：多用户共享服务器、托管在他人可物理接触的机房、容器/VM 镜像可能被外部分析。这些请走 §4 升级路径，或不要部署本工具的常驻 service 形态（改用 stdio + 走 agent）。

---

## 3. 残留风险（接受）

L1+ 模型下**未消除**的威胁：

- **R1：admin/root compromise → 全 vault 明文。**
  admin/root 可直接读 `master.key.plain` → 解开 `store.db` 全部 SSH 私钥。本模型**不防 admin/root**——这是与 L2 的核心差距。
- **R2：物理 / 离线盘访问 → master.key + store.db 可被拷走离线解。**
  `master.key.plain` 是裸明文文件；任何人能拿到磁盘（拆机、启动盘、镜像备份外泄），就能离线解开全部 vault。**靠物理 + 启动锁兜底**，不靠密码学。
- **R3：service 账户 compromise → 同 R1。**
  serve 在 Windows 上以 `LocalSystem`、在 Linux/macOS 上以 root 跑（kardianos 默认）。service 账户能读 master.key——它 compromise = admin compromise = R1。

> **缓释**：Windows 上 `serve install` 会对 vault 目录做 defense-in-depth 的 ACL 加固（`SYSTEM` + `Administrators` + 当前用户，移除 `Users`/`Authenticated Users`/`Everyone`，禁用继承）。这只挡"同机非特权进程意外读"——**不改变** R1/R2/R3。

---

## 3.5 agent 可见性承诺边界（v0.9 起）

「接口级不暴露」的准确边界（Plan 31，backlog #12）：

- **承诺**：ssh-manager 的 MCP 接口默认不披露 vault 内 host:port 与凭据——`list_servers` 默认回 `"hidden"`（owner 可按服务器 `expose_host=true` 显式放开，host:port 组合口径——孤立端口号不构成披露）；工具错误文本不含 host / IP 字面量 / host:port 组合。
- **后台任务三件套（v0.10，Plan 32）不新增披露面**：任务表是 broker 进程内状态（与隧道同类），无任何持久化；`sudo` 密码仅在启动时瞬时传递给会话内核、不进任务记录；failed 态回给 agent 的错误文本过与连接错误同一条地址清洗链。
- **`forward_port` 的 `listen_host` 越权拒绝文本不披露白名单内容**（Plan 35）：`listen_host` 是 agent 自己提供的输入（host 不由 broker 回填）；拒绝只说「不在 owner 预批白名单」，不枚举表内条目——不构成新披露面。
- **不算违约**：agent 在服务器上主动执行 `ip addr` / `hostname` 等命令探出的地址。
- **明确不防的运行时逃逸**：agent 调用本机 ssh-manager owner CLI（`projects show` / `servers ls` 明文打印 `user@host:port`）；agent 读到离线 client 上的 cache.bin（整仓快照，设计上含全部 host 明文）——这两类属「agent 宿主已被完全信任」范畴，见 [backlog.md](./backlog.md) 的 (d) 类定级。
- **明确不做**：运行时级隐藏（命令过滤 / 输出脱敏 / 网络盲化）与服务器出网管控（backlog 不做清单）。

---

## 3.6 cache 吊销的切断语义（Plan 34）

威胁 (b) 类（设备失窃/被控 + 设备码已 revoke）下"切断失效"的兑现路径（[Plan 34](./superpowers/specs/2026-08-24-plan-34-cache-invalidation-design.md.rev4.md) 起；此前 revoke 只断"拉新"、对已落盘快照零作用）：

- **revoke + 回连即销毁**：`cache-tokens revoke` 后，该设备下一次自动（≤30min lazy cadence）或手动 `cache pull` 收到 **pinned 401** → 本地 cache 侧**四件销毁**（DEK / `cache.auth.json` / `cache.bin`→隔离 / `cache.meta.json`）→ 此后 spawn 报明确归因错误、无凭据不再自动拉取。信任锚：pinningTransport 上的 401 = 通过 SPKI 指纹验证后的**权威服务器明确拒绝**；明文 pull 的 401（HTTP 劫持可伪造）、网络错误 / TLS 失败 / 非 401 状态码**永不触发**销毁。
- **永离线残余 = 轮换服务器凭据（唯一根治）**：失窃机持有"密文 + 解密钥 + 二进制"三件，**永不离线**的机器上没有任何服务端机制能远程废掉本地解密能力——销毁要"回连"才兑现。根治只有轮换该机接触过的服务器凭据（`servers edit --password/--key`）。
- **fail-closed 代价（接受）**：pinned 401 **不区分** revoked / unknown（401 reason 字段 revoked/unknown 纯可观测性，供 owner 日志排查，客户端判定不依赖），也**不区分新码打错**——非攻击场景（服务端数据丢失/重建、换码手滑/用过期码）同样触发销毁。恢复 = 用正确码重新 `cache pull`（全量重建）。安全优先的取舍。
- **失窃响应口径**：cache token 与该设备上的 project token **都要 revoke**（`cache-tokens revoke` 的 CLI 输出附此提示）。销毁清单**不含** project token——`.claude.json` 是用户自己的 agent 配置，客户端程序不改写；revoke cache token ≠ 切断该机的 project token。

---

## 3.7 隧道面的 (b) 类约束现状（Plan 35）

威胁 (b) 类（agent 被劫持后开隧道打内网）在隧道面上现在有三道约束（[Plan 35](./superpowers/specs/2026-08-25-plan-35-tunnels-hardening-design.md.rev4.md) 起；此前 revoke 后已建立的隧道继续转发、且无 owner 急停——本节即该缺口的闭合记录）：

- **急停已存在**：revoke/disable 级联 ≤~15s（一个控制 tick）拆隧道；owner `tunnels kill <id>` / `tunnels kill --project` 随时拆；store 持续故障时降级为 ≤~2min **有界关闭**（不存在「无限期暴露」）；进程级 hang 不在 DB kill 保障域——应急 = 重启/杀进程（隧道随进程死）。
- **非环回 bind 必须白名单预批**：`forward_port` 的 `listen_host` 缺省环回；非环回 IP 需 owner `serve bind add` 预批（IP 字面量 only——hostname / 网段 / 通配一律拒，gate 读失败 fail-closed 拒）；撤回后存量 ≤~15s 收缩——被劫持 agent 无法自行把隧道 bind 到 VLAN 面扩大攻击面。
- **离线 cache 客户端的隧道不在 kill/ls 域**：白名单表不进离线快照（离线恒 loopback-only，机制性 fail-closed）、隧道不进 `tunnel_registry`——其拆法 = §3.6 的回连销毁 + 本机杀进程。

---

## 4. 升级路径（L1+ → L2，未来若需）

如果将来部署到多用户 / 不可信机器 / 物理不可控环境，三个升级选项（**当前均未实现**，留作未来工作）：

- **U1：重新引入 keyring（Unix）/ DPAPI（Win）作为可选 KeyProvider。**
  **前置条件**：必须先解决"跨 session 服务自起"问题——这正是 Plan 14/15 两次撞墙的坑。可能需要：Windows Service + `LoadUserProfile`、或 passphrase-at-boot、或专用 service 账户 + 用户态密钥库。**没有解之前，U1 不能用。**
- **U2：passphrase 派生 DEK（`store.DeriveFromPassphrase` 已存在）。**
  serve 启动时提示输入 passphrase → Argon2id 派生 master key。**冲突点**：这与"boot 自起、headless"冲突——除非 passphrase 也落盘（→ 退化回 L1+，且 passphrase 明文进 service 配置，比裸文件更糟）。**仅在"放弃 boot 自起、每次手动起 serve"的部署形态下可用**。
- **U3：外部 KMS / HSM / Vault Transit。**
  架构级改动——master key 不落本机，每次解密走远程 KMS。引入网络依赖、KMS 自身可用性、新信任根。最重但最强。

### 结论

**L2 在"服务自起 + headless"形态下本质困难**：

- passphrase 要人输 → 与 boot 自起冲突。
- 用户态密钥（DPAPI/keyring）跨 session 不可靠 → Plan 14/15 已实证两次。
- 外部 KMS → 引入新依赖，超出"单机工具"范畴。

**L1+ 是该部署形态下的合理工程选择**——接受 R1/R2/R3，换"boot 自起可预测、可测、跨平台一致"。前提是 §2 的三条全部成立。

---

## 5. `SSHMGR_MASTERKEY_HEX` 环境变量（⚠️ 仅供测试）

`SSHMGR_MASTERKEY_HEX` 是 `resolveMasterKey` 的一个 tier（`internal/vault/vault.go`）：**测试、脚本、临时迁移**用它把 master key 注入到 ssh-manager 进程。

> **⚠️ 不要用于生产 boot 自起。**
> 如果把 `SSHMGR_MASTERKEY_HEX` 写进 service 配置（Windows Service 的注册表环境 / Linux systemd 的 `EnvironmentFile` / macOS launchd 的 `EnvironmentVariables`），**明文 master key 会落进 service 配置文件**——比 0600+ACL 的 `master.key.plain` **更糟**（service 配置常进版本控制、备份、监控采集；权限模型与 vault 目录不一致）。
>
> 生产路径**只能**走 `FileKeyProvider`（裸文件 + ACL）——`serve install` 默认如此，无需在 service 配置里写任何 key。

---

## 6. 资源封顶（1 MiB 输出/传输基线 + upload_content 8 MiB 独立口径）

agent 工具面的资源封顶是防 DoS / 防上下文膨胀的基线：`exec_command` 输出
**每通道 1 MiB**（前缀截断 + truncated）、`download_file` 内容 **1 MiB**、
`upload_file` **单文件 1 MiB**（传输前拒绝）。**`upload_content`（Plan 33；
版本行待发版回写）采用独立的 8 MiB（解码后字节）上限**，理由：上传的是**已在 agent
上下文里的内容**（内联 JSON 入参）——不新增读取面、不再膨胀上下文，与
download 方向相反（download 封顶防的是大文件全文灌进上下文），1 MiB 那套
理由不适用于上行。

- **上限 env seam 可调且 fail-closed**：`SSHMGR_UPLOAD_CONTENT_MAX`（缺省
  8 MiB），env 不可解析 / 非正 / **大于 1 GiB** → 进程拒绝启动（stdio 的
  `NewServerFromSource` 与 serve 的 `NewServeRunner` 走同一解析函数、两读点
  均取启动时快照，运行期改 env 不热生效）。
- **body limit 随 env 同源联动放大（登记）**：serve HTTP 请求体上限 =
  `cap + cap/3 + 64 KiB`，与内容上限**同源**——owner 调大该 env 时单请求
  内存占用上界一起放大。1 GiB 硬上限同时是这条联动的护栏。
- **serve 请求体收口（`http.MaxBytesReader`）= 传输层加固**：SDK 原生对
  请求体是无上限 `io.ReadAll`（持有效 token 者 POST 巨 body 占内存的 DoS
  面）；中间件在 token 门之外层收口——Content-Length 诚实超限直接 413，
  谎报/无 Content-Length（chunked）由 MaxBytesReader 兜底为错误响应（攻击
  者拿不到工具执行）。stdio 不加 cap（对端是本机进程，非网络面）。
- **已知边界（登记，owner 2026-08-24 拍板接受）——text 转义早拒**：text
  模式 JSON 转义的**线上平均膨胀 >4/3 即可触发 413 早拒**，不是只有极端
  形态才中（8 MiB cap 下全 2× 转义 >~5.6 MiB、控制字符 6× >~1.8 MiB 即中；
  ~48 MiB 只是 6× 全覆盖的形态上界），被 413 的内容解码后可能 ≤ cap。真实
  场景（配置/脚本，膨胀通常 <1.1）几乎不命中。**不为该边界放大上限**（6× =
  把单请求 DoS 面放大到 ~48 MiB，得不偿失）；agent 侧指引：极端转义/二进制
  内容走 base64（[agent-tools.md](./agent-tools.md) upload_content 节）。
- **残余风险（登记，owner 拍板接受）——并发聚合内存无全局上界**：
  MaxBytesReader 是**单请求体**粒度；已认证客户端可并发发送多个近上限请求，
  并发聚合内存无全局上界。接受理由：serve 对所有工具响应本就是同款无界
  并发（既有环境属性，非本特性新增）；并发主体（token → project → agent）
  受 owner token 生命周期管控；加全局 semaphore/内存约束是新机制，超
  Plan 33 scope（纯增量口径）。

---

## 相关文档

- [Plan 16 设计 spec §6](./superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md)——本篇来源。
- [getting-started.md](./getting-started.md)——路径表、master.key 形态、service 安装。
- [multi-machine.md](./multi-machine.md)——serve 常驻、cache DEK 介质。
- [backup-restore.md](./backup-restore.md)——便携备份 / 灾难恢复（export/import + NAS）。
