# Web UI 教程 · 单机版（sshmgr ui）

> **读者**：拿到 sshmgr 单机版、想用浏览器页面操作（不想记命令行命令）的人。
> 与 [quickstart-single-machine.md](./quickstart-single-machine.md)（命令行速通）殊途同归——同一套
> vault 操作的两个入口，每个页面动作都有命令行等价面。概念模型图解见 [concepts.md](./concepts.md)。

## 1. 启动

```bash
sshmgr ui
```

- 终端打印「Web UI 已启动：http://127.0.0.1:<随机端口>/ui/start?token=<一次性启动令牌>」并自动打开系统浏览器；打不开时把网址手动复制到浏览器。
- 页面**只监听本机回环地址 `127.0.0.1`**，命令退出（`Ctrl+C` 或终端关闭）即停，不新增常驻进程。
- 空机器第一次运行自动进入**初始化向导**；已初始化的机器直接进入单机正式页面。
- 同一套操作用命令行怎么做，见 [quickstart-single-machine.md](./quickstart-single-machine.md)。

## 2. 首次初始化向导走查（可中断续配）

向导第一步是「选择本机角色」，三个选项各附一句适用场景：

- **单机**——一台机器自持 vault，适合只在当前电脑上使用 sshmgr；
- **权威端**——多机部署里常驻权威 vault，其余机器经配对入网（走查见 [web-ui-multi-machine.md](./web-ui-multi-machine.md)）；
- **客户端**——只持本地只读快照，修改数据到权威端 Web UI 做（只显示配对指引，见联机版教程）。

单机场景点「选择单机」：

1. **确认页**说明将创建本机 vault，并用一个随机主密钥文件加密；
2. **创建 vault**——等价于「选角色 + 建 vault」的命令行一条龙 `sshmgr init --role standalone`（生成 32 字节随机主密钥写入固定路径裸文件 + 初始化加密库 + 写 `role.json`）。若本机已有**锁定**的 vault，向导不会覆盖它，而是报错引导先处理钥匙问题（`sshmgr unlock` / `sshmgr doctor`）；
3. **主密钥文件位置页**：显示主密钥文件路径与「**请备份此文件。**丢失它后将无法打开这个 vault」——**当场备份**；
4. 点「进入单机正式页面」——同一个进程直接切换到正式页面处理器，浏览器跳到首页，**不需要重新取启动令牌**。

**中断续配**：任何时刻关掉浏览器都是安全暂停——已写入 vault 的数据都在；再次运行 `sshmgr ui` 从「创建 vault」步骤继续。纯命令行机器直接 `sshmgr init --role standalone`。

## 3. 首页与下一步引导

初始化刚完成时，首页（服务器列表页顶部）会出现**下一步引导卡片**，只列还没完成的项，逐项链接到对应页面：

| 引导项 | 链接到 | 命令行等价面 |
|---|---|---|
| 录服务器（或导入现有 SSH 配置） | 「导入 SSH 配置」页 | `sshmgr servers add ...` / `sshmgr servers import` |
| 建 Profile 并授权 | 「访问 → Profile」页 | `sshmgr profiles add` + `sshmgr profiles grant` |
| 建项目并取令牌 | 「访问 → 项目令牌」页 | `sshmgr projects add` |

三项都完成后卡片消失（已有服务器但缺 profile 时只显示缺的两项，依此类推）。

> 注意：**Web UI 的服务器页没有「手工新增单台服务器」的表单**——录新机器走「导入 SSH 配置」（见下）；不在 `~/.ssh/config` 里的新机器用命令行 `sshmgr servers add --name ... --host ... --user ... --password ... / --key ...` 录入后，刷新页面即可见。

## 4. 页面参考（单机）

| 页面 | 能做什么 | 命令行等价面 |
|---|---|---|
| **服务器**（首页） | 列表与详情；「只看需要注意」过滤（无凭据、未填角色、缺私钥口令等 ⚠ 项置顶）；「导入 SSH 配置」；「逐台补全需要注意的服务器」；行内「编辑 / 删除」 | `servers ls` / `servers import` / `servers edit` / `servers rm` |
| **访问 → Profile** | 「新增 profile」表单；行内「调整授权」（勾选服务器保存）；「删除」（被项目/设备码引用时拒绝并点名） | `profiles add` / `profiles grant` / `profiles revoke` / `profiles rm` |
| **访问 → 项目令牌** | 「签发新项目令牌」（名称 + 绑定 profile）→ **token 一次性展示页**（令牌与 `.mcp.json` 片段各带「复制」按钮）；行内「轮换 / 停用 / 重新启用 / 吊销（勾选确认）/ 删除（仅已吊销）」 | `projects add` / `rotate` / `disable` / `enable` / `revoke` / `delete` |
| **机密项** | 列表（只显示名字与备注）；新增、查看值、可写服务器矩阵、删除 | `secrets` 命令组 |
| **运维** | 备份（状态 / 立即备份 / 校验最新备份 / 编辑备份配置）、设置、升级；**升级为权威端**入口（见下） | `backup` / `settings` / `update` / `sshmgr init --role server` |
| **审计** | 按条件查询 vault 审计日志 | `sshmgr audit` |

单机没有「配对」「设备码」页面——那是多机权威端的能力（命令行 `pair` / `cache-tokens` 同理只在多机使用）。

### 导入 SSH 配置（四段流程）

服务器页点「导入 SSH 配置」：

1. **路径表单**：预填默认 SSH 配置路径，提交「查看候选」；
2. **候选页**：全部预先勾选，可增减；与 vault 冲突的（同名 / 同 host:port:user）已自动排除并说明原因；可选「授权 profile」一并 grant；config 含 `Match` 块时提示继承值可能与真实 ssh 不一致；
3. **执行导入**：同名密钥只建一份凭据、加密私钥带「缺密钥口令」⚠ 原样入库；
4. **逐台补全循环**（「补全 n/N」）：结构化字段 + sudo 密码，外加至多一个条件凭据——无凭据机器出现「密码（现在设置）」、缺口令机器出现「密钥口令（补全加密私钥）」；「跳过这一台」保留 ⚠ 之后处理、「结束补全」退出循环、「保存并继续」进下一台。

命令行等价面：`sshmgr servers import --dry-run` / `--profile <名>`（补全用 `servers edit`；逐台补全循环是 Web UI 的引导形态）。

### 编辑服务器

行内「编辑」打开同一张表单：连接信息（名称/主机地址/端口/用户/shell 类型）、服务器元数据（说明/位置/硬件/服务/角色/注意事项）、标签与主机可见性、凭据（下拉选「保持不变 / 改用密码 / 粘贴私钥 / 只补私钥口令 / 清除凭据」+ 密码 / 私钥内容 / 私钥口令 / sudo 密码，全程掩码不回显）。保存带 `revision` 乐观锁，与别的会话互踩时报冲突。命令行等价面：`sshmgr servers edit <名> ...`。

### 删除服务器

行内「删除」先显示**影响预览**（设备保留、主机密钥锚、失去这台服务器的 profile 等），一次确认后删除。命令行等价面：`sshmgr servers rm <名>`。

## 5. 典型任务

### 给第二个 agent 发令牌（项目令牌页）

「访问 → 项目令牌」→ 填项目名称、选要绑定的 Profile →「签发」→ **token 一次性展示页**：

- 「项目令牌」与「.mcp.json 片段」各带「复制」按钮，片段里 token 已代入、抄完即用（单机形态：`args: ["mcp"]` + `env SSHMGR_TOKEN`）；
- 页面固定提示：令牌只显示这一次，离开本页后不能再查看；不要把含令牌的文件提交进 git。
- 丢失的唯一出路是行内「轮换」（旧 token 立即失效，重新发一把）。

每个项目一把独立 token，给第二个 agent 就是再签发一个（可绑同一 profile——授权范围相同，身份/吊销独立）。命令行等价面：`sshmgr projects add <名> --profile <名>`。

### 换 token（轮换）

行内「轮换」→ 新 token 以同款一次性页面展示。把新片段更新进 agent 的 `.mcp.json` 即完成换发；怀疑 token 泄露时这是标准处置。命令行等价面：`sshmgr projects rotate <名>`。

### 升级为权威端（运维页）

单机用了一段时间想改成多机：**运维 → 升级为权威端**——页面说明升级后的变化（之后需要设管理员口令、提权安装 serve，正式 Web UI 改由 serve 提供），勾选「我确认现在把本机升级为权威端」提交。已有服务器、Profile、项目令牌与机密项保持不变，vault 数据不迁移也不清空。成功页提示「`sshmgr ui` 即将退出，再次运行它进入权威端初始化向导」，随后进程退出。命令行等价面：`sshmgr init --role server`（之后按提示 `sshmgr serve admin set` + 提权 `sshmgr serve install`）。后续走查见 [web-ui-multi-machine.md](./web-ui-multi-machine.md)。

### 备份

「运维 → 备份」页：状态区（备份目录、最新备份与新鲜度、最近校验、计划任务只读探测）、「立即备份」、「校验最新备份」、备份配置表单（备份目录 / 保留份数 / 口令文件，保存前统一校验）。命令行等价面：`sshmgr backup create` / `backup verify` / `backup config`（详见 [backup-restore.md](./backup-restore.md)）。

## 6. 排错

| 症状 | 处置 |
|---|---|
| 浏览器没自动打开 | 终端里有完整网址（带一次性启动令牌），手动复制到浏览器打开即可 |
| 网址提示令牌失效 | 一次性启动令牌只用于换取会话：重新运行 `sshmgr ui` 取新网址 |
| 向导报「本机 vault 已存在但锁定或不可读」 | 按提示处理钥匙 / 权限问题（`sshmgr unlock`、`sshmgr doctor`），回到向导重试；向导不会覆盖既有 vault |
| 向导中途关掉了浏览器 | 什么都不用做：重跑 `sshmgr ui` 自动从「创建 vault」续配 |
| 想录入一台不在 `~/.ssh/config` 里的新服务器 | 用命令行 `sshmgr servers add`（Web UI 服务器页提供导入 / 编辑 / 补全，不提供单台手工新增表单） |
| 编辑表单填错想放弃 | 直接「取消」返回详情页（不提交） |
| 页面报「读取服务器列表失败」等错误 | 多为本机 vault 打不开：终端跑 `sshmgr doctor` 按行处理 |

## 7. 安全面

- **只监听本机回环地址**：页面（含初始化向导）不暴露给局域网；入口网址带一次性启动令牌，并沿用 Host 检查、来源检查与 CSRF 校验。
- **凭据输入全程掩码**：密码 / 私钥内容 / 私钥口令 / sudo 密码一律密文显示；已设凭据只显示「已设置」，输入新值即更换，从不回显旧值。
- **token / 设备码一次性展示**：离开页面后不可再查（库里只存哈希，明文无法恢复），丢失的唯一出路是轮换/重发。
- `.mcp.json` 含 token，**不要提交进 git**（发 token 的每一页都带这条提示）。
- 疑似机密扫描只给提示，不回显、不拦截——但根子上的纪律是：结构化备注里**本来就不该放机密**。

## 相关文档

- 命令行视角的完整流程与排错：[getting-started.md](./getting-started.md)
- token 生命周期（轮换 / 吊销 / 设备码）：[agent-access.md](./agent-access.md)
- 服务器增删改查与批量导入：[managing-servers.md](./managing-servers.md)
- 概念模型图解（vault / profile / project / 设备码谁是谁的）：[concepts.md](./concepts.md)
- 联机版 Web UI 教程（权威端 + 客户端页面）：[web-ui-multi-machine.md](./web-ui-multi-machine.md)
- 给 AI agent 的工具手册（可贴进 CLAUDE.md 的规则模板）：[agent-tools.md](./agent-tools.md)
- 多机部署（serve + 设备码的用武之地）：[quickstart-multi-machine.md](./quickstart-multi-machine.md)、[multi-machine.md](./multi-machine.md)
