# Web UI 教程 · 联机版（serve 多机共享）

> **读者**：权威端（常驻 broker 的服务器机）和工作机（客户端）两边的操作者。
> 架构 / runbook 深水区（TLS 迁移、证书轮换、export/import）在
> [multi-machine.md](./multi-machine.md)——本篇只讲 Web UI 与配对怎么点。
> 单机场景看 [web-ui-single-machine.md](./web-ui-single-machine.md)；命令行视角的速通看
> [quickstart-multi-machine.md](./quickstart-multi-machine.md)。

## 全景图

```
 ┌──权威端（你在这台操作：初始化向导 + 正式 Web UI）──────────────┐
 │  sshmgr ui（未初始化时）                              │
 │   └─ 向导三步：建 vault → 设管理员口令 → 安装 serve       │
 │  serve 常驻 0.0.0.0:7878（vault + /snapshot + /pair）    │
 │   └─ 独立 Web UI 监听（默认端口 7879，管理员口令登录）      │
 │       服务器 / Profile / 项目令牌 / 设备码 / 配对批准页      │
 └──────────────────────────────┬────────────────────────┘
                        pair 入网 │ 批准（对照工作机屏幕短认证码）
                                ▼
 ┌──工作机（你在这台操作：pair 入网 + 客户端 Web UI）──────────────┐
 │  sshmgr pair --instance <名>                           │
 │   ├─ LAN 发现 serve（或 --url 直指）→ 屏显三件套             │
 │   │   <名> @ <url> SAS <6位>（等 owner 批准）               │
 │   ├─ 批准后自动：凭据下发 → 首拉 cache.bin（只读快照；         │
 │   │   真空机自动归位进 instances/<name>/）                   │
 │   └─ 产物 pair.<名>.mcp.json 抄进 agent 配置                  │
 │  sshmgr ui → 客户端页面：连接状态 / 立即同步 / 切换实例（只读）  │
 │   （零远程写；写操作一律到权威端 Web UI）                      │
 └─────────────────────────────────────────────────────────┘
```

两台机、两把密钥、两道独立的闸门（与 multi-machine.md 一致）：**project token**
驱动 agent 的 MCP 工具调用（进 `.mcp.json`）；**设备码**只授权拉取 `/snapshot`
缓存。两者永不互通，一台设备码被吊销不影响任何 project token。

> 工作机入网只有一条路径：命令行 `sshmgr pair`（工作机的初始化向导选「客户端」
> 时只显示这条命令的写法）。批准在权威端 Web UI 的配对页；连接配置、设备码、
> 指纹全部由配对自动交付；写操作只在权威端。

## 权威端走查

### 首次初始化（三步向导）

空机第一次运行 `sshmgr ui` 进入初始化向导，第一步选**权威端**（多机部署里常驻
权威 vault，其余机器经配对入网）。向导按「建 vault → 设置管理员口令 → 安装 serve」
三步走，步骤条标明当前进度，可中断续配：

1. **建 vault**——创建本机 vault 并生成随机主密钥文件。命令行等价面：
   `sshmgr init --role server`（输出主密钥文件位置与备份提醒；对已有 vault 的
   单机运行它就是「升级为权威端」，vault 内容不动）。
2. **设置管理员口令**——输入两遍，规则与命令行一致（不能为空、两次输入必须
   一致）。命令行等价面：`sshmgr serve admin set`。
3. **安装 serve**——页面按本机操作系统显示**完整提权命令**（含 sshmgr 可执行
   文件绝对路径）和「复制命令」按钮，向导不替你提权，把命令贴到终端执行：

   | 操作系统 | 命令 |
   |---|---|
   | Linux / macOS | `sudo <sshmgr 绝对路径> serve install --addr 0.0.0.0:7878` |
   | Windows | 在管理员 PowerShell 中运行：`& "<sshmgr 绝对路径>" serve install --addr 0.0.0.0:7878` |

   页面每 2 秒自动检查 serve；检测到运行后进入**完成页**，列出正式 Web UI 的全部
   地址（虚拟局域网地址在前，`127.0.0.1` 注明「仅本机」；未找到虚拟局域网地址时
   提示可用设置项 `serve.ui_listen` 指定）。完成页出现后 `sshmgr ui` 很快退出，
   终端同时打印这些地址。

**保护与续配（如实）**：

- serve 已在运行时，向导对每个写 vault 的步骤（建 vault、设管理员口令）都会先重新
  探测 serve，探测到运行就**拒绝写入**，页面提示「serve 已在运行，请到正式 Web UI
  操作，或用 `sshmgr serve admin set`」——不会出现两个进程同时改 vault。
- 中途关掉浏览器是安全暂停：再次运行 `sshmgr ui` 从还没完成的那一步继续
  （权威端按「vault 是否可打开 → 管理员口令是否已设」实时判断入口步骤）。
- serve 已运行、但还没设管理员口令的权威端，运行 `sshmgr ui` 不进向导，终端直接
  提示用 `sshmgr serve admin set` 设口令并打印 Web UI 地址；登录页也会给出同一句
  提示。
- 已完成初始化的权威端，运行 `sshmgr ui` 只打印 Web UI 地址与状态，**不另起监听**。

### 正式页面（serve 独立监听）

从完成页给出的地址（形如 `https://<虚拟局域网地址>:7879/ui/`）打开，**管理员口令**
登录。页面与单机版同一套（服务器 / Profile / 项目令牌 / 机密项 / 运维 / 审计），
另加多机专属的两个页面。敏感写操作（签发项目令牌、调整授权、签发设备码等）在会话
超过 5 分钟未验证口令时会先跳「重新验证管理员口令」页，验证后回到原表单——这是
权威端页面的额外保护，单机回环页面没有它。

### 配对批准页（多机专属）

导航「配对」进入待批准队列。每行显示：`name @ 客户端声明的目标地址 · 来源地址 ·
profile 提示 · 剩余秒数 · ⚠ 标记`。点「批准」进入批准表单：

- 核对**客户端屏幕上的短认证码**与批准行的短认证码**逐位一致**，勾选确认框；
- 选择「批准到 profile」（该工作机拉到的快照就是这个 profile 授权的服务器）；
- 行带 ⚠「目标地址异常」（客户端声明的连接地址不属于权威端本机地址集合——疑似
  中继 / 假发现 / 拿错地址）时，常规批准被拒绝，必须在「目标地址异常时的覆盖文字」
  里输入 `OVERRIDE` 显式覆盖；
- 「拒绝」随时可用；已处理 / 已过期的行不能再操作。

命令行等价面：`sshmgr serve pair ls` / `approve <名> --profile <名>`（异常地址加
`--allow-foreign-url`）/ `reject`。

### 设备码页（多机专属，手工路径）

pair 时代日常设备码由批准自动签发；手工签发留给 CI / 存量迁移场景。导航「设备码」：
表单填设备名、选 Profile →「签发」→ **一次性结果页**（设备码 + 权威端证书公钥指纹 +
`cache pull` 示例命令，只显示这一次）。表格列出全部设备码（令牌前缀 / 状态 / Profile /
最近拉取 / 最近客户端版本），行内「撤销」。命令行等价面：`sshmgr cache-tokens add
--name <名> --profile <名>` / `cache-tokens revoke <名>` / `cache-tokens ls`。

## 工作机走查

**前提：权威端已完成上面的初始化**（serve 在跑，你知道它的地址或同网段可自动发现）。

### 入网：命令行 `sshmgr pair` 一条龙

```bash
sshmgr pair --instance laptop
```

1. **发现 / 连接**——默认 LAN 广播发现权威端（同网段即中）；拿不到 offer（防火墙
   挡 UDP / 跨网段）按提示 `--url https://<权威端>:7878` 直指，**显式 `--url` 不带
   `--pin` 默认拒连**（无锚通道需 `--allow-tofu` 显式接受，仅限受控环境）。
2. **三件套屏显**——`laptop @ https://192.0.2.5:7878 SAS 482913`。**停在批准等待**
   （10 分钟窗口）；此时去权威端批准。
3. **批准**（权威端 Web UI 配对页，见上）——对照**工作机屏幕的短认证码**与批准行
   逐位一致后选 profile 批准。
4. **自动收尾**——批准后 120 秒内工作机自动完成：凭据解密下发 → 先落盘
   （`cache.auth.json` + `cache.config.json` + 产物 `pair.laptop.mcp.json`，0600）
   → 首拉 cache.bin。终端只显示 `<project-token>` 占位符——真值只在产物文件里。
5. **配 agent**——产物片段抄进 `.mcp.json`（`mcp --cache` + env token 形态；
   `--write-mcp <路径>` 可让 pair 直落目标路径），重启 Claude Code 即用。

想看指引而不想翻文档：工作机运行 `sshmgr ui`，初始化向导选「客户端」，页面显示
配对命令写法（`--instance` 必填；`--url` 直指权威端地址；`--pin` 校验证书公钥指纹；
`--write-mcp` 把配对产物另存到指定路径）与「到权威端 Web UI 的配对页批准」——页面
只展示指引，随后 `sshmgr ui` 退出，不写 `role.json`。

### 客户端页面（`sshmgr ui`，只读）

配对完成后运行 `sshmgr ui`（多实例机器加 `--instance <名>` 选择缓存实例）进入客户端
页面：

- **服务器**：列表与详情**只读**——查看快照里的服务器与其元数据；页面固定提示
  「这里只读，修改需到权威端 Web UI 去做」。
- **机密项**：只显示名字与备注（值永不在客户端展示）。
- **运维 → 客户端**：连接状态（实例 / 权威端地址 / 证书公钥指纹 / Profile / 服务器数 /
  本地快照拉取时间 / 快照修订号 / 推荐版本）、「立即同步」（手动拉取，失败保留旧
  缓存）、实例表（快照年龄 / 材料齐全度 / 行内「切换」——只对材料齐全的实例开放）。
- **运维 → 本机审计**：查询本机实例的审计边车。
- **运维 → 升级**：升级本机二进制（权威端的重启服务入口只在权威端页面）。

命令行等价面：`sshmgr cache pull [--instance <名>]`（同步）、`sshmgr cache status` /
`cache instances ls`（状态与实例）、`sshmgr cache audit --instance <名>`、
`sshmgr update`。实例删除仍是命令行专属：`sshmgr cache instances rm <名>`（默认槽
不可删，整机清空用 `sshmgr clear`）。

## 排错

| 症状 | 处置 |
|---|---|
| pair 报「指纹失配」/ TLS 错 | **失配 ≠ 泄露**：意味着对端证书公钥变了——可能是权威端重装/迁移重签了证书（正常），也可能是中间人（异常）。权威端跑 `sshmgr serve cert-info` 拿新指纹，`pair --pin <新指纹>` 重新入网。重签证书的全量交接 runbook 见 [multi-machine.md](./multi-machine.md) |
| pair 吃 ⚠「目标地址异常」 | 工作机声明的连接地址不属于权威端机的地址集/主机名——疑似中继/假发现/拿错了地址。核对后重跑；确属故意（受控中继）才在批准表单显式输入覆盖文字（命令行 `--allow-foreign-url`） |
| `pair --url` 被拒「refusing TOFU」 | 直指又不带 `--pin` 是**默认拒绝**（默认安全）。带上权威端指纹 `--pin sha256:...`；`--allow-tofu` 只留给受控环境的无锚通道 |
| 向导安装页一直「正在等待 serve 启动」 | 按安装页的命令核对：7878 端口防火墙是否放行（TCP + UDP 都要）；`sshmgr serve status` 查四项信号（service/process/http/vault）；服务可能仍在启动，稍候——页面每 2 秒自动重查 |
| 向导报「serve 已在运行，请到正式 Web UI 操作」 | 这是写入保护：vault 已有常驻写者。到正式 Web UI 操作，或按提示用 `sshmgr serve admin set`；不要在向导里继续写 |
| 配对失败「device name in use」（419） | 实例名的旧设备码还在权威端上且**在用**（已拉取过）——同名重配在 enroll 一步即被拒，旧槽材料完好无损。owner 在权威端 `sshmgr cache-tokens revoke <名>` 吊销旧码后，工作机重跑 `sshmgr pair --force` 即可 |
| 配对/重配在 finish 后失败（写盘 / 首拉 / 传输中断） | 错误文案统一带**双路径恢复指引**（如实——客户端无法可靠分辨权威端是否已提交）：直接重跑 `sshmgr pair --force`；若重跑报设备名占用（419），请 owner 在权威端执行 `sshmgr cache-tokens revoke <实例名>` 后再重跑 |
| 同步失败但页面还有数据 | 失败保留旧缓存——这是特性不是 bug。修好网络/serve 后再「立即同步」（命令行 `cache pull`） |
| 缓存多久算旧 / 怎么自动保鲜 | TTL 由 `.mcp.json` 的 `--cache-max-age` 控制（默认 30m；0=关闭自动拉取）。`mcp --cache` 进程内 spawn 惰性拉取 + 会话内懒检查 + 热加载，无需操作系统定时器；细节见 [multi-machine.md](./multi-machine.md#离线只读缓存plan-12) |
| 吊销了 token/设备码，agent 还在跑 | 三路径：设备码吊销 → 该机下次 pull 即隔离销毁（在线 ≤30min）；project token 吊销 → 下次保鲜的新快照已无该 project；永离线设备 → `max_offline` 到期拒载。已建立的隧道 revoke/disable 后 ~15s 内级联拆除（`tunnels kill` 可急停）——完整语义见 [agent-access.md](./agent-access.md#project-生命周期轮换--暂停--恢复--吊销) |
| 权威端初始化中途退出了 | 什么都不用做，重跑 `sshmgr ui` 从未完成的步骤继续 |
| 工作机想改连别的权威端 | 重新 `pair`（`--force` 换码 / 换 `--instance` 换槽）。连接参数一律由配对交付，没有手工连接编辑表单 |

## 相关文档

- 架构 / 运营深水区（pair 协议、TLS 迁移、证书轮换 runbook、export/import、备份）：
  [multi-machine.md](./multi-machine.md)
- 单机版 Web UI 教程（初始化向导、页面参考、导入 SSH 配置）：
  [web-ui-single-machine.md](./web-ui-single-machine.md)
- 命令行视角的多机速通（pair 版）：[quickstart-multi-machine.md](./quickstart-multi-machine.md)
- token / 设备码生命周期与吊销三路径：[agent-access.md](./agent-access.md) / [agent-tools.md](./agent-tools.md)
- 概念模型图解（vault / profile / project / 设备码谁是谁的）：[concepts.md](./concepts.md)
- client↔serve 版本兼容矩阵（升级任何一端前先看）：[compat-matrix.md](./compat-matrix.md)
