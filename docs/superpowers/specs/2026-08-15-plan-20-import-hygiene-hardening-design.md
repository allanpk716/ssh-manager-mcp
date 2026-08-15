# Plan 20 设计：~/.ssh/config 批量导入 + 卫生票 + 安全加固（spec v1）

> 日期：2026-08-15。来源：用户 grilling 五轮拍板（凭据策略 / 解析范围 / 冲突策略 / 入口形态 / 补全引导）。
> 三条 stream 相互独立可并行；本 spec 一次覆盖，实施 plan 按任务拆。

## 背景与目标

Plan 17-19 合并后核心功能链（单机 vault → broker → 多机缓存 → TUI → 向导/clear）无缺口。本轮收三件事：

1. **Stream C（新功能）**：`~/.ssh/config` 批量导入——录入服务器清单的最后一块拼图（此前只能逐台 `servers add` / TUI 表单）。
2. **Stream A（卫生票）**：三个 Plan 终审 triage 的 deferred minors（ledger 权威清单）。
3. **Stream B（安全加固）**：仓库级老账——孤儿凭据、token argv 可见性、版本号真值、half-close、HostKeyAlgorithms。

## Stream C：`~/.ssh/config` 批量导入

### C1 `internal/importer` 包（纯逻辑，无 IO）

- 解析：`github.com/kevinburke/ssh_config`（MIT）读整份文件 → 对每个**字面量** Host 别名（含 `Host a b c` 多名块逐名展开）求值 `HostName / User / Port / IdentityFile`。**继承语义交给库**：`Host *` 块的默认值、first-obtained-wins 等暗坑不由本项目重造。
- 过滤规则：
  - 别名含 `*` `?` `!` 等模式字符 → 跳过（进报告，原因 wildcard-pattern）。
  - `HostName` 缺省 → 用别名本身当主机名（ssh 原语义）；`Port` 缺省 → 22；`User` 缺省 → 空。
- 冲突判定：
  - **config 内部**：多个别名解析到同 `host:port:user` → 只取第一个，其余进报告（原因 internal-duplicate）。
  - **vault 侧**冲突（同名 server 或同 `host:port:user` 已存在）由调用方判定（C1 不依赖 store，返回候选集；C2/C3 拿候选对照 store 后标记 skip-existing）。
- 产出：`[]ImportCandidate{Name, Host, Port int, User, KeyPath string}` + `[]Skipped{Alias, Reason string}`。表驱动测试全覆盖（含 `Host *` 继承、多名块、内部去重、`~` 相对 IdentityFile 路径）。

### C2 CLI：`ssh-manager servers import`

- flags：`--file`（缺省 `~/.ssh/config`，`~` 展开）｜`--dry-run`（打印将导入/跳过表，不落库）｜`--profile <name>`（导入后 grant 全部成功项；profile 不存在=报错，不隐式建）。
- 每条候选：
  - vault 冲突（同名 / 同 `host:port:user`）→ 跳过，报告列 `skip-existing`。
  - `IdentityFile` 存在 → `readKeyFile` 读**内容**进 vault（等价 `servers add --key`）；`~` 与相对路径按 ssh 语义展开（相对 `~/.ssh/`）。
  - 无 `IdentityFile` → 导入为无凭据（模型已支持；后续 `servers edit --password` 补）。
  - **passphrase 加密的私钥**：config 不携带口令 → 照常导入，报告标 ⚠（连接会失败，需 edit 补 passphrase）。
- 事务边界：逐条独立导入；单条失败不回滚已成功项（报告如实列），重跑幂等（靠 skip-existing）。
- CLI **不做逐台问答**（脚本化优先）：报告每台列出空的结构化字段，末尾提示「进 TUI 逐台补全或 `servers edit`」。
- 只读 `~/.ssh/config` 与私钥文件，绝不写回；导入成功后原私钥文件仍在盘上（报告末尾一行提醒双份存在，是否删由用户决定）。

### C3 TUI：servers 页 `[i]` 导入 + 逐台补全循环

流程：`[i]` → 文件路径表单（预填缺省路径）→ 候选多选（huh multiselect，默认全选，vault 冲突项直接不出现）→ **先全部静默入库** → 立即进入**逐台补全循环**：

- 每台一张 huh 表单，字段：`role / description / location / hardware / services / caveats / sudo 密码（选填）` 七栏 + 无密钥条目**追加一栏密码**（有密钥的不出现）。表单顶部只读显示 name/host/user。
- 表单字段构造与既有 edit 表单**同源**（同一组 field 构造函数，不新造第二套定义）。
- **Esc = 跳过本台**（数据已在库，标 ⚠，进下一台）；**q = 结束循环**（剩余全部标 ⚠）。
- ⚠ 判定：无凭据 **或** role 为空。servers 列表显示 ⚠ 标记；随时 `e`（既有 edit）补全。
- 结束后结果页：导入 N / 跳过 M（原因分组）/ 待补 K（⚠）。

### C4 文档

getting-started（Step 2 加「批量导入」旁门）、managing-servers（import 专节 + 补全流程）、README 功能表加一行。concepts.md 不动（导入只是录入的另一种姿势，不新增概念）。

## Stream A：卫生票（4 任务，来源 ledger per-task 条目）

| 任务 | 内容 |
|---|---|
| A1 死代码/孪生清理 | 删 `roles.DetectMode/DetectModeWith`（仅测试引用）；删 `client.timeout`、`syncCmd` 生产死代码；`stripEmbeddedPin` 双点孪生 → 导出 `clientops.SplitTokenPin` 统一；`go mod tidy`（lipgloss //indirect）；TUI dispatch 表驱动测试（T6 欠账） |
| A2 错误分类/呈现 | `classifyPullError` 401 子串匹配 → 精确匹配 `server returned 401`（防指纹 hex 误撞）；UNIQUE 违规错误本地化（撞名时人话报错）；serve URL untrimmed（Plan 18 T7 M1） |
| A3 向导/clear 残角 | wizard `saveErr` 在 vaultErr 视图可见；role.json 两处并存（vault 角色 + 用户目录）加显式测试；GrantServers 失败重试留孤儿空 profile → grant 前预检全部 server 存在（fail-fast） |
| A4 cert-before-mint + isTTY + 文档 | CLI `cache-tokens add` + TUI tokenview **一起**把证书加载挪到 mint 前（防孤儿设备码）；`tui < NUL` 挂起修复（isTTY 被 NUL 字符设备骗过 → GetConsoleMode 判真终端）；文档三处措辞（multi-machine:536 auto-pull 过度承诺、「每 30 分钟」→逐调用懒查、spawn-pull 缺文件分支补记） |

## Stream B：仓库级安全加固（4 任务）

| 任务 | 内容 |
|---|---|
| B1 孤儿凭据级联 | `DeleteServer` 事务内级联删 `credential_id` + `sudo_credential_id`；`servers edit` 换凭据时同事务删旧凭据（若无他处引用）；新命令 `ssh-manager gc` 枚举+清除历史存量无引用凭据行（dry-run 先行） |
| B2 token env 通道 | `mcp` 支持 `SSHMGR_TOKEN` 环境变量（flag `--token` 优先级更高）；`.mcp.json` 走 `env` 字段 → token 不出现在进程 argv（/proc、ps 可见性归零）；文档同步（agent-access/getting-started Step 5） |
| B3 serverInfo.version 真值 | MCP initialize 响应硬编码 v0.1.0 → 接 goreleaser ldflags 注入的版本号（与 `ssh-manager version` 同源同值） |
| B4 half-close + 旋钮 | `tunnel.go handle` 双向 copy 先完一侧时 `CloseWrite` 传播 EOF（落地代码注释里已画好的方案）；新全局环境旋钮 `SSHMGR_SSH_HOST_KEY_ALGORITHMS`（连老机器需 ssh-rsa 时显式开启，零 schema 变更；per-server 字段不做，真有需求再立） |

## 依赖与并行

- 三条 stream 无相互依赖，任务级并行安全（C2 复用 `readKeyFile`，B1 动 `DeleteServer` 事务——不同文件）。
- 发版：整批进 **v0.7.0**（release note 增补 import 功能段 + token env 通道段）。

## 边界与不做

- 不做 Include 递归、ProxyJump 链标注、known_hosts 导入（明确排除，ledger 记录）。
- 不做 per-server HostKeyAlgorithms 字段（schema 变更，需求未证）。
- 不做 CLI 逐台问答补全（脚本化优先，补全引导是 TUI 专属体验）。
- 不动 ~/.ssh/config 与私钥文件（只读）。
- 自动化测试不替代 OWNER 真机演练（向导 Esc 重入 / clear 真跑 / TUI 交互冒烟仍属 v0.7.0 发版前手工项，与本批开发并行不阻塞合并）。

## 验证策略

- C1 表驱动：wildcard 过滤、`Host *` 继承、多名块、内部 host:port:user 去重、IdentityFile `~`/相对路径展开、HostName 缺省=别名。
- C2 冒烟：dry-run 不落库、幂等重跑、vault 冲突跳过、passphrase 加密键 ⚠ 标注、`--profile` grant、`--profile` 不存在报错。
- C3 表单流：Esc 跳过标 ⚠、q 结束、⚠ 判定（无凭据/role 空）、字段同源（edit 表单构造复用）。
- A/B 各任务带回归测试（401 精确匹配负例=含 401 子串的指纹 hex；级联删除 FK 断言；gc dry-run 无副作用）。
- 全量 `go build ./... && go vet ./... && go test ./...`。
