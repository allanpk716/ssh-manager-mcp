# Plan 20 设计：~/.ssh/config 批量导入 + 卫生票 + 安全加固（spec v2 = rev1）

> 日期：2026-08-15。v1 五轮 grilling 拍板；v2 吸收四家异构评审闭环（C1 查证 6/6 证实 + C2 实验 4/4 成立 + E1 顺带证实 + 三项设计决策用户拍板）。
> 三条 stream 相互独立的原则**修订**：存在两处显式先后序（A1→B2、C0↔B1 串行），见「依赖与并行」。

## 背景与目标

Plan 17-19 合并后核心功能链（单机 vault → broker → 多机缓存 → TUI → 向导/clear）无缺口。本轮收三件事：

1. **Stream C（新功能）**：`~/.ssh/config` 批量导入 + 无凭据 server 模型支持（评审证实的承重墙前置任务）。
2. **Stream A（卫生票）**：三个 Plan 终审 triage 的 deferred minors（ledger 权威清单）。
3. **Stream B（安全加固）**：仓库级老账——孤儿凭据、token argv 可见性、版本号真值、half-close、HostKeyAlgorithms。

## Stream C：`~/.ssh/config` 批量导入

### C0（新任务，C2/C3 前置）无凭据 server 模型支持

现状（已核实）：`store.go:309` `credential_id TEXT NOT NULL REFERENCES credentials(id)` + FK 开启；`AuthMethod` 仅 password/private_key；CLI add 强制凭据二选一；TUI `submitServer:282` 同。无 IdentityFile 的 config 条目（常见形态）当前插不进库。

- **schema 迁移**：`credential_id` 改 nullable（`auth_method` 新增 `none` 空串语义：CredentialID=="" ⇔ AuthMethod==""）。迁移路径沿用既有 ALTER 机制；`scanServer`/export/import 快照路径同改。
- **行为定义**：无凭据 server 可以存在、可被 list/grant（元数据对 agent 可见）；`exec_command` 等用凭据的操作返回明确错误「该服务器尚未配置凭据，用 servers edit 补」——**不 panic 不空凭据尝试连接**。
- **CLI**：`servers add` 放开为凭据可选（与 TUI 对齐，加/不加减一即可）；`servers edit --password/--key` 补凭据路径保持现状即可挂上。
- **TUI**：`submitServer` add 模式去掉「凭据必填」硬检查。
- **⚠ 判定的地基**：「无凭据」从此可判定（CredentialID==""）。

### C1 `internal/importer` 包（纯逻辑，无 store 依赖）

- 解析：`github.com/kevinburke/ssh_config` v1.6.0（MIT，已验证行为，见下）。对每个**字面量** Host 别名（含 `Host a b c` 多名块逐名展开）求值。
- **IdentityFile 用 `GetAll`**（实验 E2 证实）：返回全列表含 `Host *` 块继承、顺序=文件序，与真 ssh IdentityFile 累积语义一致；`Get` 只回首个，不可用。
- 产出结构：`[]ImportCandidate{Name, Host, Port int, User, KeyPaths []string}` + `[]Skipped{Alias, Reason}`。
- 过滤规则：
  - 别名含 `*` `?` `!` 等模式字符 → 跳过（wildcard-pattern）。
  - `HostName` 缺省 → 别名本身当主机名（ssh 原语义）；`Port` 缺省 → 22；**`User` 缺省 → 调用方回填当前 OS 用户名**（ssh 原语义；存 `""` 会导致 x/crypto/ssh 握手必败——G2）。
  - `Port` 非数字 → 该条进报告（bad-port），不导入。
- **路径解析（E5 证实 + 决策②）**：`~` 展开（`~/...` 与 `~user/...`）；**相对路径相对 config 文件所在目录解析**——这是对真 ssh（连接时按进程 CWD）的**有意偏离**，文档与报告明标；解析后不存在 → 无 key 处理（同无 IdentityFile）。
- **多密钥（决策③）**：KeyPaths 按序取**第一个存在且可读**的文件为该机凭据；其余路径进报告备注（`extra-key-paths`）。
- **Match 块警示（E1 证实）**：库把 Match 参数当 Host 模式正则匹配（误求值，非忽略），且 `isMatch` 未导出、API 无法过滤。importer **原文预扫** `Match` 关键字行（大小写不敏感），命中即在报告顶部打警示横幅「config 含 Match 块，继承值可能与真 ssh 不一致」。
- 冲突判定：
  - config 内部：多别名解析到同 `host:port:user` → 取第一个，其余进报告（internal-duplicate）。
  - vault 侧冲突由调用方对照 store 判定（skip-existing）。

### C2 CLI：`ssh-manager servers import`

- flags：`--file`（缺省 `~/.ssh/config`）｜`--dry-run`｜`--profile <name>`。**顺序语义**：`--profile` 在**导入前**预检存在（fail-fast，不存在=报错不建）；`--dry-run` 与 `--profile` 并存时**只打印将 grant 的清单，不 grant**。
- 每条候选：
  - vault 冲突（同名 / 同 `host:port:user`）→ 跳过（skip-existing）。
  - 有可用 key → 读**内容**进 vault；**passphrase 加密私钥照常导入**，报告标 ⚠（需补口令——C3 补全表单有栏）。
  - 无可用 key → 无凭据导入（C0 模型），报告列 `needs-credential`。
- **单条原子（G6）**：新增 store 事务 API `AddServerWithCredentials(srv, cred *Credential, sudo *Credential) (id string, err error)`——credential(s) 与 server 行同事务写入，失败零残留。CLI/TUI 导入与 `servers add` 全部改走此 API（旧 SetCredential+AddServer 两步路径保留给零散调用但不再新增使用）。
- **批内密钥去重（G3，决策）**：导入过程中维护 `map[sha256(key内容)]credentialID`——同一 key 文件被多台引用（`Host *` 常态）只铸一份凭据行，多台共享引用。**连带契约**：凭据可共享 ⇒ B1 的删除/换凭据/gc 必须查两列引用并做「他处引用」守卫。
- 事务边界：逐条独立（一条失败不回滚别条），重跑幂等（skip-existing）。报告每台列空结构化字段，末尾提示进 TUI 补全或 `servers edit`。
- 只读 `~/.ssh/config` 与私钥文件；报告末尾提醒原私钥仍在盘上（双份存在）。

### C3 TUI：servers 页 `[i]` 导入 + 逐台补全循环

- **前置子任务**：把 `newServerForm`（forms.go:26-53）的三组 inline 字段抽成可组合构造器——`identityFields(d, editable)`、`credentialFields(d, editing)`、`structuredFields(d)`（E3 证实改动面 ~30 行零行为变更）；add/edit 表单改用构造器组装（行为等价，回归测试锁定）。
- 流程：`[i]` → 文件路径表单（预填缺省）→ 候选多选（vault 冲突项不出现）→ **先全部静默入库**（逐条原子 + 批内去重，单条失败如实进结果页不中断）→ 逐台补全循环：
  - 每台一张 huh 表单：`structuredFields` 七栏（role/description/location/hardware/services/caveats/sudo 选填）+ **条件栏**：无凭据条目加密码栏；**加密 key 无口令条目加 passphrase 栏**（G10——补全循环里就能救回，不必事后 edit）。表单顶部只读显示 name/host/user。
  - Esc = 跳过本台（标 ⚠ 进下一台）；q = 结束循环（剩余标 ⚠）。
  - **⚠ 判定（扩展）**：无凭据 **或** key 缺口令（加密私钥且 Passphrase 空）**或** role 空。
- **中断恢复（G7）**：servers 列表 **⚠ 置顶排序** + `!` 键切换「只看 ⚠」过滤；结果页明示「K 台待补：列表按 ! 过滤后逐台 e 补全」。
- 结束后结果页：导入 N / 跳过 M（原因分组）/ 待补 K（⚠）。

### C4 文档

getting-started（Step 2 加「批量导入」旁门）、managing-servers（import 专节 + 补全流程 + Match 警示 + 相对路径偏离说明）、README 功能表加一行。concepts.md 不动。

## Stream A：卫生票（4 任务）

| 任务 | 内容 |
|---|---|
| A1 死代码/孪生清理 | 删 `DetectMode/DetectModeWith`（**在 `internal/tui/mode.go`（package tui），非 roles 包**——评审笔误更正）；删 `client.timeout`、`syncCmd` 生产死代码；`stripEmbeddedPin` 双点孪生 → 导出 `clientops.SplitTokenPin` 统一（**B2 依赖此导出，先行**）；`go mod tidy`；TUI dispatch 表驱动测试（T6 欠账） |
| A2 错误分类/呈现 | `classifyPullError` 401 裸子串 → 精确匹配 `server returned 401`；**负例用 `4010`/`1401`**（hex 无空格/字母撞不上精确串，裸子串时代的老负例失效——评审证实）；UNIQUE 违规本地化；serve URL untrimmed |
| A3 向导/clear 残角 | wizard `saveErr` 在 vaultErr 视图可见；role.json 两处并存显式测试；GrantServers 失败留孤儿空 profile → grant 前预检全部 server 存在（fail-fast） |
| A4 cert-before-mint + isTTY + 文档 | CLI `cache-tokens add` + TUI tokenview 一起把证书加载挪到 mint 前；`tui < NUL` 挂起修复（GetConsoleMode 判真终端）；文档三处措辞（multi-machine:536、「每 30 分钟」→逐调用懒查、spawn-pull 缺文件分支） |

## Stream B：仓库级安全加固（4 任务）

| 任务 | 内容 |
|---|---|
| B1 孤儿凭据级联（**与 C0 同改 store，串行**） | `DeleteServer` 事务内级联删其凭据，**带「他处引用」守卫**（凭据被共享——C2 批内去重后是常态——时只解除本机引用不删行，守卫必须查 `credential_id` **和** `sudo_credential_id` 两列，两路径守卫一致否则 FK 硬失败）；`servers edit`（CLI）与 TUI `submitServer` 换凭据**同修**（同事务删旧凭据，仅当无他处引用）；新命令 `ssh-manager gc`：默认 dry-run、`--apply` 显式执行，SQL 限定只删 credentials 中**两列均无引用**的行，断言 host_keys/cache_tokens 零影响 |
| B2 token env 通道（**消费 A1 的 SplitTokenPin，后行**） | `mcp` 支持 `SSHMGR_TOKEN`（**与 `--token` 同名同义同解析路径**——决策①：非 cache 模式收 project token；`--cache` 模式收设备码可内嵌 pin，走同一 SplitTokenPin；flag 优先于 env）；去掉 `MarkFlagRequired("token")`；**改 `projects.go:234` 的 `.mcp.json` 生成器**为 env 形态（`"env":{"SSHMGR_TOKEN":"..."}`，args 不再带 token）；测试含「带 pin 形态打非 cache 端点认证失败」「与 SSHMGR_SERVE_PIN 并存两道闸不互认」；措辞降级为「消除 argv/ps 暴露面」（env 仍可被同用户/root 经 /proc/PID/environ 读）；文档同步含 **PUBLIC 仓库 `.mcp.json` gitignore 强制警告** |
| B3 serverInfo.version 真值 | **新 shared 包 `internal/buildinfo`**（E4 证实 cli→mcpserver 单向、直接读必循环 import）；goreleaser ldflags 目标改指该包；`ssh-manager version` 与 MCP initialize 同源同值 |
| B4 half-close + 旋钮 | `tunnel.go handle` 双向 copy：**按方向区分** local→remote EOF 后对 remote `CloseWrite`，反向同理，等两个 copy 都结束才关双连接（不是第一个 done 就关）；半关闭集成测试（echo 一侧 shutdown 写端，断言另一侧仍能收完）；`SSHMGR_SSH_HOST_KEY_ALGORITHMS` 旋钮：解析非法值 fail-closed（报错不静默）、空值=不动默认，文档含「**可能需清该机 host key 重 TOFU**」（服务端可能改呈不同算法的主机键） |

## 依赖与并行（v2 修订）

- **显式先后序两处**：A1（export SplitTokenPin）→ B2；C0（schema 迁移）↔ B1（同改 store）**串行不并行**（C2 的事务 API 与 B1 的级联守卫共用同一套 store 事务层，合并实现）。
- 其余任务并行安全（C3 表单拆分子任务触碰 TUI 测试，独立于 A/B）。
- 发版：整批进 **v0.7.0**。

## 边界与不做

- 不做 Include 递归的**额外**处理（库原生支持，Get/GetAll 自动跟随——不主动拒绝也不额外展开报告）；不做 ProxyJump 链标注、known_hosts 导入。
- 不做 per-server HostKeyAlgorithms 字段（schema 变更，需求未证）。
- 不做 CLI 逐台问答补全（TUI 专属体验）。
- 不动 ~/.ssh/config 与私钥文件（只读）。
- 相对 IdentityFile 相对 config 目录解析 = 有意偏离 ssh CWD 语义，文档明标（不做 CWD 模拟）。
- OWNER 真机演练（向导 Esc 重入 / clear 真跑 / TUI 冒烟）仍属发版前手工项，不阻塞合并。

## 验证策略

- C0：无凭据 server 增删改查/list/grant/快照导出导入全通；exec 返回结构化错误不 panic；迁移前后库兼容。
- C1 表驱动：wildcard 过滤、`Host *` 继承（fixture 已有实证）、多名块、内部 host:port:user 去重、`~`/相对路径展开、多 key 取首个可读、Port 非数字进报告、Match 横幅。
- C2 冒烟：dry-run 不落库不 grant、幂等重跑、vault 冲突跳过、**批内同 key 只铸一份凭据**（hash 断言）、单条失败零残留（事务回滚断言）、`--profile` 不存在 fail-fast、加密 key ⚠。
- C3：表单构造器拆分行为等价回归；Esc/q/⚠ 判定三态（无凭据/缺口令/role 空）；`!` 过滤键；条件栏（密码/passphrase）出现规则。
- A/B 各带回归测试（401 精确匹配负例 `4010`/`1401`；gc 两列判定 + 共享凭据不误删 + host_keys/cache_tokens 零影响；env/flag 同路径解析；buildinfo 注入）。
- 全量 `go build ./... && go vet ./... && go test ./...`。
