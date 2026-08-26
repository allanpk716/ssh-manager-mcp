# client ↔ serve 版本兼容矩阵

> 维护规则：每次发版后追加一行「已验证组合」。破坏性变更必须在此登记 + 给迁移顺序。

## 已验证组合

| client 版本 | serve 版本 | 在线（HTTP MCP） | 离线（cache pull / mcp --cache） | 验证日期 |
|---|---|---|---|---|
| v0.10.0 | v0.10.0 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；NUC10 exe 改经 GitHub release 直连下载——`upload_file` 自硬 per-file cap（1 MiB）后 17 MB exe 已传不动，本版起部署通道改道；zip/exe sha256 双核对 `e54a8b11…`/`aca1f4c9…`；升级按 Plan 39 runbook：换二进制 → `cache-tokens bind laptop-v040 e2e-profile` → 重启 serve；**Plan 39 裁剪实测：re-pull 后 cache 11→10 srv / 12→10 creds，`cache status` scope=e2e-profile，gitlab-urit 离线零出现（探针 grep=0）**；离线探针 serverInfo 0.10.0 + 10 工具，exec_background/exec_output/exec_stop/upload_content 新面全在） | ✅（doctor 双端 0 WARN 0 FAIL；NUC10 解密探针 11 srv/12 creds 整库（服务端不裁）、serve cert 指纹 `c69b2560…` 未变零重 pin、serve HEALTHY；**Plan 37 到龄自废已在笔记本启用 `SSHMGR_CACHE_MAX_OFFLINE=24h`**——pull 侧 wrapper 与 load 侧 MCP env 双路径，服务器 Date 锚已建立（anchored pull exit=0），离线加载实测不破） | 2026-08-26 |
| v0.9.0 | v0.9.0 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；exe 两端 sha256 `c1cec2ab…b8fb` 核对；**v0.9 掩码实测：在线 list_servers 9/9 `"hidden"`；connect_error 文本 `ssh dial: dial tcp [REDACTED]: connectex: …` 无 host/IP 单前缀；expose 两态翻转恰 1 台明文**） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11、cert 指纹未变；serve HEALTHY @0.9.0（serverInfo.version 确认）；**cache pull 后离线两态一致（快照携带 expose_host 实证：开启台离线亦明文，全关后 9/9 `"hidden"`）**；验证后 port/expose 复位=全默认掩码稳态） | 2026-08-21 |
| v0.8.10 | v0.8.10 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；exe 两端 sha256 `9e32f67b…f91d` 核对） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11、serve cert 指纹未变；serve HEALTHY @0.8.10；v0.8.8/0.8.9 守卫生产回归：`projects enable/rotate` 对 revoked 行 `phase5-e2e` 双双拒绝） | 2026-08-21 |
| v0.8.9 | v0.8.9 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11；serve HEALTHY @0.8.9；v0.8.9 修复生产实测：`projects rotate` 对 revoked 行 `phase5-e2e` 拒绝且零 token 输出） | 2026-08-20 |
| v0.8.8 | v0.8.8 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11；serve HEALTHY @0.8.8；v0.8.8 漏洞修复生产实测：`projects enable/disable` 对 revoked 行 `phase5-e2e` 双双拒绝） | 2026-08-20 |
| v0.8.1 | v0.8.1 | ✅（NUC10 权威 broker + 笔记本；发版后按铁律 client 先 serve 后） | ✅（cache 健康；doctor 双端 0 WARN 0 FAIL 含解密探针 9/10；owner ssh echo 冒烟过） | 2026-08-17 |
| v0.8.0 | v0.8.0 | ✅（NUC10 权威 broker + 笔记本；发版后按铁律 client 先 serve 后） | ✅（cache 9 servers/10 creds；owner ssh 三连 smoke：echo=exit 0 / 远端非零=CLI 非零+stderr 报码 / 无命令=显式报错） | 2026-08-17 |
| v0.7.3 | v0.7.3 | ✅（NUC10 权威 broker + 笔记本） | ✅（9/9 服务器） | 2026-08-16 |

（v0.10.0 为当前生产双端；v0.8.2–v0.8.8 曾部署双端但未逐版登记本表（v0.8.8 行除外）；更早历史组合未逐一回归，旧版本请先看下方破坏性变更。）

## 已知破坏性变更

| 起始版本 | 变更 | 影响 | 迁移 |
|---|---|---|---|
| v0.4.0（实证：commit `d48523a`，2026-08-13「RunServe auto-generates cert + forces TLS when none given」） | serve 默认 TLS-only + 自签证书 + SPKI pin；无 pin 客户端默认拒连 | 旧明文 client 无法拉快照/连 MCP | 先升全部工作机 binary + 配 pin，**最后**重启 serve（README「migration order」） |
| v0.7.0 | `tui --mode broker` 移除（自动判定覆盖） | 脚本里写死 `--mode broker` 的调用报错 | 改 `ssh-manager tui` |
| v0.9.0 | `list_servers` host 默认掩码为 `"hidden"`（per-server `expose_host` opt-in）；工具错误文本清洗（不含 host/IP/host:port） | 依赖 host 明文的 agent 流程当场断；v0.9 serve + 未升级 client 的离线模式仍回明文；旧 binary 导入新快照丢 `expose_host=true` 偏好（fail-safe：折回掩码） | 顺序按铁律 client 先、serve 后（技术上无硬约束；该顺序服务于「掩码尽快全生效」——在线随 serve 升级即刻生效、离线需 client 升级）。**依赖 host 明文的流程唯一补救 = 升级前 `ssh-manager servers edit <name> --expose-host`** |
| v0.10.0 | **Plan 39 授权收紧**：`/snapshot` 从整库 dump 改为按设备码绑定 profile 裁剪；`cache-tokens add` 必填 `--profile`；存量未绑码拉取被拒 **403**（非 401，不触发本地销毁，cache 不毁但不拉新）。同批：Plan 35 隧道契约变更（revoke/disable → ≤15s 拆除，`forward_port.listen_host` 白名单）；`upload_file` 超 1 MiB 硬拒（此前超限截断/误报） | 未绑存量码的定时拉取开始失败（403 文案给 bind 指引）；旧整库 cache.bin 在 re-pull 前保留原样（不受影响但也不再刷新授权边界）；混合部署期 `tunnels ls/kill` 覆盖不完整 | 升级顺序：**先 bind 后重启 serve**（见下方铁律），重启后各工作机 re-pull 一次即原子覆盖旧整库 cache.bin；`upload_file` 大文件改走目标机直连 release 下载或分块 |
| v0.10.1 | **Plan 41 批 1：sudo wrapper 整体提权**——`sudo=true` 的远端包装从 `sudo -S -p '' -- <cmd>` 改为 `BASH_ENV= LC_ALL=C sudo -S -p '' -- bash -c '<单引号转义 cmd>'`：**整条命令（所有 `;`/`&&`/`\|` 段）以 root 执行**（v0.10.0 及之前仅第一段提权、后续段以登录用户静默执行——正确性缺陷）。同批行为变化：变量/tilde 由 root bash 扩展（`$USER`→root 通用；`$HOME`/`$PATH` 部署依赖）、`$0`→`bash`、内层不读 `~/.bashrc`、sudo 诊断恒 C locale、内层命令 locale 在 env_keep 保留的部署下为 C；**前置条件**：sudoers 须为通用命令授权形态（command-specific allowlist 部署下 `sudo bash` 被拒；NOEXEC 部署下外部命令部分被拦——均不静默） | 依赖"复合命令后续段以普通用户执行"这一缺陷形态的流程（不应存在）失效；依赖 `$HOME`/`.bashrc` 环境或 locale 输出格式的复合命令需自查 | 无迁移动作：cache 模式随客户端、http 直连随 serve，双端各自升级各自生效，无协议层不一致。单元回归见 `internal/sshbroker` 批 1 矩阵（复合全程提权 / 转义 round-trip / 注入不逃逸 / exit 传播 / 错密码 fail-loud / 后台特权同测） |

## 升级顺序铁律

**先升所有 client（工作机 binary + cache pin），最后重启 serve。** serve 一旦升级到 TLS-only 版本即刻拒绝旧明文 client——顺序反了会把整条缓存链打断。token / snapshot 格式 / tool schema 目前无跨版本不兼容记录；出现时在此登记。

**Plan 39 追加（设备码绑 profile）**：升 serve 机时，替换二进制后**先 `cache-tokens bind` 全部存量未绑码，再重启 serve**——未绑码在 Plan 39 serve 上拉取被拒（403，本地缓存不毁但不拉新）；重启后各工作机 re-pull 一次，旧整库 cache.bin 即被裁剪快照原子覆盖。

**Plan 37 追加（到龄自废启用）**：`SSHMGR_CACHE_MAX_OFFLINE` 必须同时设在**拉取路径**（定时刷新 wrapper、`mcp --cache` 会话内 lazy pull）与**加载路径**（`mcp --cache` 进程 env）——服务器 Date 锚只在带该 env 的拉取进程里记录（`clientops.go` §2.2-2.4）。只设加载侧时，每次无 env 的定时拉取会把 `server_anchored` 重写为 false，加载侧随即可拒载（fail-closed）。

## 相关文档

- [multi-machine.md](./multi-machine.md) —— serve 部署与 TLS 迁移 runbook
- [agent-access.md](./agent-access.md) —— token 生命周期与断连语义（四层）
