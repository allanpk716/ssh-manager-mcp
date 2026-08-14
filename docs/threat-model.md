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

## 相关文档

- [Plan 16 设计 spec §6](./superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md)——本篇来源。
- [getting-started.md](./getting-started.md)——路径表、master.key 形态、service 安装。
- [multi-machine.md](./multi-machine.md)——serve 常驻、cache DEK 介质。
- [backup-restore.md](./backup-restore.md)——便携备份 / 灾难恢复（export/import + NAS）。
