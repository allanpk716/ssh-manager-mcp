# client ↔ serve 版本兼容矩阵

> 维护规则：每次发版后追加一行「已验证组合」。破坏性变更必须在此登记 + 给迁移顺序。

## 已验证组合

<!-- v0.11.0（Plan 40 多实例第一批 + P0 锚修复）：下两行为发版前预填的行为矩阵（spec §5）——v0.11.0 发版双端验证后回写验证日期与实测细节，删除本注释。 -->

| client 版本 | serve 版本 | 在线（HTTP MCP） | 离线（cache pull / mcp --cache） | 验证日期 |
|---|---|---|---|---|
| v0.11.0 | v0.11.0 | ✅（在线面不变；`/snapshot` 新增响应头 `X-Sshmgr-Device-Name`——老 client 忽略，新 client 用于实例路由与 `--instance` 强一致校验） | ✅ 全功能（Plan 40 第一批：多实例 CLI 闭环——`cache pull/status --instance`、`mcp --cache --instance`、`instances/<name>/` 目录 + per-instance DEK `cache-dek-<name>.key`、`cache.config.json` MAX_OFFLINE 持久化 + `pull --max-offline`、默认实例身份门禁三分支（存量空 `device_name` 补记——零迁移）、`clear` 清实例树 + DEK 变体、roles 认命名实例机；**P0 锚修复**：pinned pull 恒记服务器锚——无 env 的 pull / TUI `[s]` 同步不再抹 `server_anchored`（v0.10 部署实测的"同步致 mcp --cache 瘫痪"bug 根治）。**边界如实**：首次 enroll 无 flag 仍落默认目录（自动归位第二批）、TUI client 页仅默认实例、doctor 不感知命名实例） | 待发版回写 |
| v0.11.0 | v0.10.0 | ✅（MCP 工具面无变化；老 serve 不下发 `X-Sshmgr-Device-Name` 头，在线行为同 v0.10.0×v0.10.0） | ⚠️ 受限（多实例不可用，前两项均带提示文案——**`--instance` 拒**：报错 "`--instance requires a Plan-40 serve`"（提示升级 serve 或去掉 flag 用默认槽）；**默认实例身份门禁不生效**：响应头缺失 → 门禁跳过 + WARNING 提示升级后生效（老 serve 拓扑异码覆盖敞口维持现状级，非新增敞口）；**无自动归位**：首次 enroll 无 flag 仍落默认目录（与 v0.11×v0.11 同——自动归位本批即未实现，第二批）；无 flag 的 pull/mcp/status 走默认目录，行为与 v0.10.0 一致（零迁移）——除 `cache status` 无 `--instance` 时输出为列表视图（默认槽一行 + 每实例一行，rev3 §2.6；对老 serve 拓扑通常只有默认槽一行） | 待发版回写 |
| v0.10.0 | v0.10.0 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；NUC10 exe 改经 GitHub release 直连下载——`upload_file` 自硬 per-file cap（1 MiB）后 17 MB exe 已传不动，本版起部署通道改道；zip/exe sha256 双核对 `e54a8b11…`/`aca1f4c9…`；升级按 Plan 39 runbook：换二进制 → `cache-tokens bind laptop-v040 e2e-profile` → 重启 serve；**Plan 39 裁剪实测：re-pull 后 cache 11→10 srv / 12→10 creds，`cache status` scope=e2e-profile，gitlab-urit 离线零出现（探针 grep=0）**；离线探针 serverInfo 0.10.0 + 10 工具，exec_background/exec_output/exec_stop/upload_content 新面全在） | ✅（doctor 双端 0 WARN 0 FAIL；NUC10 解密探针 11 srv/12 creds 整库（服务端不裁）、serve cert 指纹 `c69b2560…` 未变零重 pin、serve HEALTHY；**Plan 37 到龄自废已在笔记本启用 `SSHMGR_CACHE_MAX_OFFLINE=24h`**——pull 侧 wrapper 与 load 侧 MCP env 双路径，服务器 Date 锚已建立（anchored pull exit=0），离线加载实测不破） | 2026-08-26 |
| v0.9.0 | v0.9.0 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；exe 两端 sha256 `c1cec2ab…b8fb` 核对；**v0.9 掩码实测：在线 list_servers 9/9 `"hidden"`；connect_error 文本 `ssh dial: dial tcp [REDACTED]: connectex: …` 无 host/IP 单前缀；expose 两态翻转恰 1 台明文**） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11、cert 指纹未变；serve HEALTHY @0.9.0（serverInfo.version 确认）；**cache pull 后离线两态一致（快照携带 expose_host 实证：开启台离线亦明文，全关后 9/9 `"hidden"`）**；验证后 port/expose 复位=全默认掩码稳态） | 2026-08-21 |
| v0.8.10 | v0.8.10 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；exe 两端 sha256 `9e32f67b…f91d` 核对） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11、serve cert 指纹未变；serve HEALTHY @0.8.10；v0.8.8/0.8.9 守卫生产回归：`projects enable/rotate` 对 revoked 行 `phase5-e2e` 双双拒绝） | 2026-08-21 |
| v0.8.9 | v0.8.9 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11；serve HEALTHY @0.8.9；v0.8.9 修复生产实测：`projects rotate` 对 revoked 行 `phase5-e2e` 拒绝且零 token 输出） | 2026-08-20 |
| v0.8.8 | v0.8.8 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11；serve HEALTHY @0.8.8；v0.8.8 漏洞修复生产实测：`projects enable/disable` 对 revoked 行 `phase5-e2e` 双双拒绝） | 2026-08-20 |
| v0.8.1 | v0.8.1 | ✅（NUC10 权威 broker + 笔记本；发版后按铁律 client 先 serve 后） | ✅（cache 健康；doctor 双端 0 WARN 0 FAIL 含解密探针 9/10；owner ssh echo 冒烟过） | 2026-08-17 |
| v0.8.0 | v0.8.0 | ✅（NUC10 权威 broker + 笔记本；发版后按铁律 client 先 serve 后） | ✅（cache 9 servers/10 creds；owner ssh 三连 smoke：echo=exit 0 / 远端非零=CLI 非零+stderr 报码 / 无命令=显式报错） | 2026-08-17 |
| v0.7.3 | v0.7.3 | ✅（NUC10 权威 broker + 笔记本） | ✅（9/9 服务器） | 2026-08-16 |

（v0.10.0 为当前生产双端；v0.11.0 两行为发版前预填（未实测），发版后回写；v0.8.2–v0.8.8 曾部署双端但未逐版登记本表（v0.8.8 行除外）；更早历史组合未逐一回归，旧版本请先看下方破坏性变更。）

## 已知破坏性变更

| 起始版本 | 变更 | 影响 | 迁移 |
|---|---|---|---|
| v0.4.0（实证：commit `d48523a`，2026-08-13「RunServe auto-generates cert + forces TLS when none given」） | serve 默认 TLS-only + 自签证书 + SPKI pin；无 pin 客户端默认拒连 | 旧明文 client 无法拉快照/连 MCP | 先升全部工作机 binary + 配 pin，**最后**重启 serve（README「migration order」） |
| v0.7.0 | `tui --mode broker` 移除（自动判定覆盖） | 脚本里写死 `--mode broker` 的调用报错 | 改 `ssh-manager tui` |
| v0.9.0 | `list_servers` host 默认掩码为 `"hidden"`（per-server `expose_host` opt-in）；工具错误文本清洗（不含 host/IP/host:port） | 依赖 host 明文的 agent 流程当场断；v0.9 serve + 未升级 client 的离线模式仍回明文；旧 binary 导入新快照丢 `expose_host=true` 偏好（fail-safe：折回掩码） | 顺序按铁律 client 先、serve 后（技术上无硬约束；该顺序服务于「掩码尽快全生效」——在线随 serve 升级即刻生效、离线需 client 升级）。**依赖 host 明文的流程唯一补救 = 升级前 `ssh-manager servers edit <name> --expose-host`** |
| v0.10.0 | **Plan 39 授权收紧**：`/snapshot` 从整库 dump 改为按设备码绑定 profile 裁剪；`cache-tokens add` 必填 `--profile`；存量未绑码拉取被拒 **403**（非 401，不触发本地销毁，cache 不毁但不拉新）。同批：Plan 35 隧道契约变更（revoke/disable → ≤15s 拆除，`forward_port.listen_host` 白名单）；`upload_file` 超 1 MiB 硬拒（此前超限截断/误报） | 未绑存量码的定时拉取开始失败（403 文案给 bind 指引）；旧整库 cache.bin 在 re-pull 前保留原样（不受影响但也不再刷新授权边界）；混合部署期 `tunnels ls/kill` 覆盖不完整 | 升级顺序：**先 bind 后重启 serve**（见下方铁律），重启后各工作机 re-pull 一次即原子覆盖旧整库 cache.bin；`upload_file` 大文件改走目标机直连 release 下载或分块 |
| v0.10.1 | **Plan 41 批 1：sudo wrapper 整体提权**——`sudo=true` 的远端包装从 `sudo -S -p '' -- <cmd>` 改为 `BASH_ENV= LC_ALL=C sudo -S -p '' -- bash -c '<单引号转义 cmd>'`：**整条命令（所有 `;`/`&&`/`\|` 段）以 root 执行**（v0.10.0 及之前仅第一段提权、后续段以登录用户静默执行——正确性缺陷）。同批行为变化：变量/tilde 由 root bash 扩展（`$USER`→root 通用；`$HOME`/`$PATH` 部署依赖）、`$0`→`bash`、内层不读 `~/.bashrc`、sudo 诊断恒 C locale、内层命令 locale 在 env_keep 保留的部署下为 C；**前置条件**：sudoers 须为通用命令授权形态（command-specific allowlist 部署下 `sudo bash` 被拒；NOEXEC 部署下外部命令部分被拦——均不静默） | 依赖"复合命令后续段以普通用户执行"这一缺陷形态的流程（不应存在）失效；依赖 `$HOME`/`.bashrc` 环境或 locale 输出格式的复合命令需自查 | 无迁移动作：cache 模式随客户端、http 直连随 serve，双端各自升级各自生效，无协议层不一致。单元回归见 `internal/sshbroker` 批 1 矩阵（复合全程提权 / 转义 round-trip / 注入不逃逸 / exit 传播 / 错密码 fail-loud / 后台特权同测） |
| v0.11.0 | **Plan 40 多实例第一批 + P0 锚修复**：① serve 启动对存量 **active** 设备码 name 做 fail-closed 检测——**casefold 碰撞**（如 `agentA` vs `AGENTA`）或**白名单外非法 name**（自由文本时代遗留：空格/路径形态/超长/首段 DOS 保留名）任一 → **serve 拒绝启动**（错误逐行列出 anomaly + 修复指引；revoked 行不参与检测——revoke 即修复通道）；② 新发码 name 走白名单 + casefold **全历史（含 revoked）唯一**；③ `--instance` 与 `SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK` env **互斥报错**；④ `DoPull` 记锚条件从 `maxOffline>0` 改为 `pin!=""`（P0——无 env 的 pull/TUI 同步不再抹 `server_anchored`）；⑤ MAX_OFFLINE 新增 `cache.config.json` 读取源（env > file > 关）；⑥ `/snapshot` 新增响应头 `X-Sshmgr-Device-Name`（老 client 忽略） | 库中存在 active name 碰撞/非法的 serve **升级后起不来**（fail-closed 非静默——这是有意拦截：碰撞/非法 name 即将成为客户端目录名）；其余变更面向新功能面，无 flag 的存量用法零迁移不受影响；`cache status` 无 `--instance` 输出为列表视图（rev3 §2.6） | 存量 name 异常的修复 = `cache-tokens revoke <异常名>` 后 `cache-tokens add --name <合法新名> --profile <profile>` 重发（客户端该实例需重新 enroll；revoke 是修复通道、永不校验 name）；升级顺序铁律不变（client 先 serve 后）；双端 ≥v0.11.0 后 spec §1.3 过渡期纪律（恢复只跑带 env 的 pull、禁 TUI `[s]`/裸 pull）作废 |

## 升级顺序铁律

**先升所有 client（工作机 binary + cache pin），最后重启 serve。** serve 一旦升级到 TLS-only 版本即刻拒绝旧明文 client——顺序反了会把整条缓存链打断。token / snapshot 格式 / tool schema 目前无跨版本不兼容记录；出现时在此登记。

**Plan 39 追加（设备码绑 profile）**：升 serve 机时，替换二进制后**先 `cache-tokens bind` 全部存量未绑码，再重启 serve**——未绑码在 Plan 39 serve 上拉取被拒（403，本地缓存不毁但不拉新）；重启后各工作机 re-pull 一次，旧整库 cache.bin 即被裁剪快照原子覆盖。

**Plan 37 追加（到龄自废启用）**：`SSHMGR_CACHE_MAX_OFFLINE` 必须同时设在**拉取路径**（定时刷新 wrapper、`mcp --cache` 会话内 lazy pull）与**加载路径**（`mcp --cache` 进程 env）——服务器 Date 锚只在带该 env 的拉取进程里记录（`clientops.go` §2.2-2.4）。只设加载侧时，每次无 env 的定时拉取会把 `server_anchored` 重写为 false，加载侧随即可拒载（fail-closed）。

## 相关文档

- [multi-machine.md](./multi-machine.md) —— serve 部署与 TLS 迁移 runbook
- [agent-access.md](./agent-access.md) —— token 生命周期与断连语义（四层）
