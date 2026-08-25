# client ↔ serve 版本兼容矩阵

> 维护规则：每次发版后追加一行「已验证组合」。破坏性变更必须在此登记 + 给迁移顺序。

## 已验证组合

<!-- v0.10.0/0.11.0（Plan 33 upload_content）：工具面纯增量——1 新工具（upload_content：内联内容上传，text/base64，解码后 ≤8 MiB，`SSHMGR_UPLOAD_CONTENT_MAX` env seam）+ serve HTTP 请求体上限（MaxBytesReader，cap+cap/3+64 KiB 同源联动），无破坏性变更。v0.10.0 未发版——并入 v0.10.0 行还是开 v0.11.0 行留 owner 发版时拍板。占位:发版后回写,记得删除本注释 -->
<!-- v0.10.0/0.11.0（Plan 34 离线 cache 切断失效）：纯增量，无新 env、无新响应头——`cache-tokens revoke` 语义增强（断拉新 + 该机回连即销毁本地 cache 四件：DEK/auth/bin→quarantine/meta；触发条件 = pinned-401）；服务端 401 附 reason（revoked/unknown，纯可观测性，客户端判定不依赖）；`cache-tokens revoke` CLI 输出附两 token 提示行。跨版本口径：销毁在 client 侧执行——**旧 client 对新 serve 维持旧语义**（只断拉新、不销毁本地 cache，401 静默失败），新 client 对旧 serve 销毁语义即生效（不依赖任何新响应头）。与上条 Plan 33 同批发版回写，占位注释届时删除。 -->
<!-- v0.10.0/0.11.0（Plan 32 后台任务三件套）：纯增量——3 新工具（exec_background / exec_output / exec_stop）+ ExecOutput 新字段 effective_timeout_seconds，无破坏性变更；发版后双端实测回写本表。 -->
<!-- v0.10.0/0.11.0（Plan 35 tunnels 硬化）：契约变更——revoke/disable → 已建立隧道 **≤15s 拆除**（一个控制 tick；前提 = store 健康 + 控制循环存活，store 持续故障降级为 ≤~2min 有界关闭；进程 hang 不在 DB kill 域，应急 = 重启/杀进程；此前契约 = 隧道不受 revoke 影响、无急停）；`forward_port` 新参数 `listen_host`（非环回需 owner `serve bind` 白名单预批，`bind rm` 后存量 ≤15s 收缩）；vault 新表 ×3（forward_bind_hosts / tunnel_orders / tunnel_registry）；新 CLI `serve bind add/rm/ls` + `tunnels ls` / `kill <id>` / `kill --project`；`forward` audit 行 Command 追加 ` id=<tunnelID>` 单列（格式变更，消费方注意）。**kill/ls 域完整性要求全部可写 broker（serve + 在线 stdio）都升级**——混合部署期旧版进程的隧道不进 registry、不受 kill 单/级联管辖，`tunnels ls` 覆盖不完整。与 Plan 32/33/34 同批发版回写，占位注释届时删除；版本行归属 owner 发版拍板。 -->

| client 版本 | serve 版本 | 在线（HTTP MCP） | 离线（cache pull / mcp --cache） | 验证日期 |
|---|---|---|---|---|
| v0.9.0 | v0.9.0 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；exe 两端 sha256 `c1cec2ab…b8fb` 核对；**v0.9 掩码实测：在线 list_servers 9/9 `"hidden"`；connect_error 文本 `ssh dial: dial tcp [REDACTED]: connectex: …` 无 host/IP 单前缀；expose 两态翻转恰 1 台明文**） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11、cert 指纹未变；serve HEALTHY @0.9.0（serverInfo.version 确认）；**cache pull 后离线两态一致（快照携带 expose_host 实证：开启台离线亦明文，全关后 9/9 `"hidden"`）**；验证后 port/expose 复位=全默认掩码稳态） | 2026-08-21 |
| v0.8.10 | v0.8.10 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve；exe 两端 sha256 `9e32f67b…f91d` 核对） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11、serve cert 指纹未变；serve HEALTHY @0.8.10；v0.8.8/0.8.9 守卫生产回归：`projects enable/rotate` 对 revoked 行 `phase5-e2e` 双双拒绝） | 2026-08-21 |
| v0.8.9 | v0.8.9 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11；serve HEALTHY @0.8.9；v0.8.9 修复生产实测：`projects rotate` 对 revoked 行 `phase5-e2e` 拒绝且零 token 输出） | 2026-08-20 |
| v0.8.8 | v0.8.8 | ✅（NUC10 权威 broker + 笔记本；client 先 serve 后，schtasks 独立任务重启 serve） | ✅（doctor 双端 0 WARN 0 FAIL，NUC10 解密探针 10/11；serve HEALTHY @0.8.8；v0.8.8 漏洞修复生产实测：`projects enable/disable` 对 revoked 行 `phase5-e2e` 双双拒绝） | 2026-08-20 |
| v0.8.1 | v0.8.1 | ✅（NUC10 权威 broker + 笔记本；发版后按铁律 client 先 serve 后） | ✅（cache 健康；doctor 双端 0 WARN 0 FAIL 含解密探针 9/10；owner ssh echo 冒烟过） | 2026-08-17 |
| v0.8.0 | v0.8.0 | ✅（NUC10 权威 broker + 笔记本；发版后按铁律 client 先 serve 后） | ✅（cache 9 servers/10 creds；owner ssh 三连 smoke：echo=exit 0 / 远端非零=CLI 非零+stderr 报码 / 无命令=显式报错） | 2026-08-17 |
| v0.7.3 | v0.7.3 | ✅（NUC10 权威 broker + 笔记本） | ✅（9/9 服务器） | 2026-08-16 |

（v0.8.10 为当前生产双端；v0.8.2–v0.8.8 曾部署双端但未逐版登记本表（v0.8.8 行除外）；更早历史组合未逐一回归，旧版本请先看下方破坏性变更。）

## 已知破坏性变更

| 起始版本 | 变更 | 影响 | 迁移 |
|---|---|---|---|
| v0.4.0（实证：commit `d48523a`，2026-08-13「RunServe auto-generates cert + forces TLS when none given」） | serve 默认 TLS-only + 自签证书 + SPKI pin；无 pin 客户端默认拒连 | 旧明文 client 无法拉快照/连 MCP | 先升全部工作机 binary + 配 pin，**最后**重启 serve（README「migration order」） |
| v0.7.0 | `tui --mode broker` 移除（自动判定覆盖） | 脚本里写死 `--mode broker` 的调用报错 | 改 `ssh-manager tui` |
| v0.9.0 | `list_servers` host 默认掩码为 `"hidden"`（per-server `expose_host` opt-in）；工具错误文本清洗（不含 host/IP/host:port） | 依赖 host 明文的 agent 流程当场断；v0.9 serve + 未升级 client 的离线模式仍回明文；旧 binary 导入新快照丢 `expose_host=true` 偏好（fail-safe：折回掩码） | 顺序按铁律 client 先、serve 后（技术上无硬约束；该顺序服务于「掩码尽快全生效」——在线随 serve 升级即刻生效、离线需 client 升级）。**依赖 host 明文的流程唯一补救 = 升级前 `ssh-manager servers edit <name> --expose-host`** |

## 升级顺序铁律

**先升所有 client（工作机 binary + cache pin），最后重启 serve。** serve 一旦升级到 TLS-only 版本即刻拒绝旧明文 client——顺序反了会把整条缓存链打断。token / snapshot 格式 / tool schema 目前无跨版本不兼容记录；出现时在此登记。

## 相关文档

- [multi-machine.md](./multi-machine.md) —— serve 部署与 TLS 迁移 runbook
- [agent-access.md](./agent-access.md) —— token 生命周期与断连语义（四层）
