# client ↔ serve 版本兼容矩阵

> 维护规则：每次发版后追加一行「已验证组合」。破坏性变更必须在此登记 + 给迁移顺序。

## 已验证组合

| client 版本 | serve 版本 | 在线（HTTP MCP） | 离线（cache pull / mcp --cache） | 验证日期 |
|---|---|---|---|---|
| v0.7.3 | v0.7.3 | ✅（NUC10 权威 broker + 笔记本） | ✅（9/9 服务器） | 2026-08-16 |

（v0.7.3 为当前生产双端；历史组合未逐一回归，旧版本请先看下方破坏性变更。）

## 已知破坏性变更

| 起始版本 | 变更 | 影响 | 迁移 |
|---|---|---|---|
| v0.4.0（实证：commit `d48523a`，2026-08-13「RunServe auto-generates cert + forces TLS when none given」） | serve 默认 TLS-only + 自签证书 + SPKI pin；无 pin 客户端默认拒连 | 旧明文 client 无法拉快照/连 MCP | 先升全部工作机 binary + 配 pin，**最后**重启 serve（README「migration order」） |
| v0.7.0 | `tui --mode broker` 移除（自动判定覆盖） | 脚本里写死 `--mode broker` 的调用报错 | 改 `ssh-manager tui` |

## 升级顺序铁律

**先升所有 client（工作机 binary + cache pin），最后重启 serve。** serve 一旦升级到 TLS-only 版本即刻拒绝旧明文 client——顺序反了会把整条缓存链打断。token / snapshot 格式 / tool schema 目前无跨版本不兼容记录；出现时在此登记。

## 相关文档

- [multi-machine.md](./multi-machine.md) —— serve 部署与 TLS 迁移 runbook
- [agent-access.md](./agent-access.md) —— token 生命周期与断连语义（四层）
