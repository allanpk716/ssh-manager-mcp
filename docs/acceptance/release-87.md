# 整体发版真机验收册（#87）

## 替代旧册声明

本册替代已废弃的原终端图形界面验收册
[plan-46-45-tui-flows.md](./plan-46-45-tui-flows.md)。旧册的废弃标注与正文是
历史记录，保持原样，不得为本册改写；其命令行等价面已跑的历史结论继续有效。
本册承接原终端图形界面删除后的图形界面验收，并按票 #87 汇总本次整体发版的
真机验收。

## 适用范围与依据

- 适用范围：机密项、网络拓扑与主机信任、共享存储、Web UI、初始化与迁移、
  新旧版本混跑、备份迁移，以及 Windows 反斜杠路径上传缺陷 #47 的复设用例。
- 部署与版本组合：单机、权威端、客户端、新旧版本混跑。四种组合分部见本册
  第一至第四部分。
- 编排依据：票 #87 的验收标准、规格 #240 的定案，以及规格引用的功能决策票
  与架构决策记录。后续条目引用具体依据时必须写在条目内。
- 本册只编排验收与记录要求，不新增产品行为，也不改变任何接口。
- 运行记录写入 [runs/](./runs/)；已验证组合追加到
  [compat-matrix.md](../compat-matrix.md)，失败项登记
  [backlog.md](../backlog.md)，不得放宽判据。

## 标注规范

### 操作面

| 标注 | 含义 |
| --- | --- |
| owner 命令行 | 由 owner 在指定机器的命令行环境执行，或由 owner 完成命令行中的交互确认。 |
| agent 工具面 | 由 agent 经 sshmgr 命令行或 SSH 工具面执行；证据注明「agent 代跑」。 |
| 浏览器 | 在 Web UI 页面执行；证据按页面文本、响应头或截图留存，截图不得含敏感值。 |

### 证据来源

| 标注 | 含义 |
| --- | --- |
| 真机端到端 | 在真机设备矩阵所列代称设备与真实部署拓扑上执行完整路径。 |
| 自动化测试代证 | 引用进程内测试、一致性测试或持续集成测试的断言，证明行为契约；不能替代必须实证的真机路径。 |

### 执行分工与安全护栏

执行者分工、agent 代跑、人工保留面、一次性靶子护栏、取证与通过判据沿用
[验收册总则](./README.md)，本册不重复另立规矩。破坏性演练一律使用一次性
靶子；agent 不得代跑人工保留面。后续每条正式条目除固定结构外，必须写明
操作面三类之一与证据来源两类之一。

## 条目模板

每条正式条目使用以下固定结构；「是否允许自动化替代」只能写明一种结论，并写
清限制条件：

```text
标注：操作面=owner 命令行 / agent 工具面 / 浏览器；证据来源=真机端到端 / 自动化测试代证
前置数据：
执行步骤：
期望结果：
证据留存方式：
是否允许自动化替代：
```

## 取证模板

- 命令原文与完整输出必须成对留存；失败输出也原样留存，不修饰、不截断。
- 机密值、真实 IP 地址、主机名、账号名不得进入本册与运行记录。
- 涉及完整性断言时，附原文件与对照文件的 SHA-256 哈希值，并写明哈希算法。
- 远端操作必须注明经哪台代称机器执行；只用代称，不写真实网络标识。
- 浏览器取证留存页面文本、相关响应头或截图；截图前必须确认没有机密值、
  真实网络标识与账号名。

取证块使用以下格式：

```text
【命令原文】
<逐字保存命令>
【完整输出】
<逐字保存完整输出；失败同样保留>
【完整性对照】
<需要时写原文件、对照文件与两者的 SHA-256 哈希值>
【执行位置】
<写本机或经哪台代称机器执行>
```

## 真机设备矩阵

本矩阵只用代称，设备组合按 owner 2026-11-11 的确认入册。真实设备、地址、
端口、账号与凭据的对应关系只存在 vault 里；不得写入本册、运行记录、兼容矩阵
或欠账清单。执行真机票时 owner 在场。

| 代称 | 本册用途 |
| --- | --- |
| 公司 A 的权威端 | 多机部署中的权威端，承载权威 vault 与 Web UI 独立监听验收。 |
| 家 A 的单机演练机 | 单机部署演练，承载单机向导与全功能验收。 |
| 家 A Windows 新版客户端（工作机 1） | Windows 新版客户端与浏览器验收。 |
| 工作机 1 上的旧版本第二实例 | 与新版客户端同机并存的一次性旧版本实例，用于版本混跑；不碰生产实例。 |
| Linux 受管服务器 A | Linux 持久挂载、环境变量注入、路径与网络行为验收。 |
| macOS 受管服务器 A | macOS 间接映射、环境变量注入、路径与网络行为验收。 |
| Windows 受管服务器 A（PowerShell 条目） | Windows 环境变量注入与路径验收的 PowerShell 服务器条目。 |
| Windows 受管服务器 A（cmd 条目） | Windows 环境变量注入与路径验收的 cmd 服务器条目。 |
| NAS A | 不受管设备，提供共享存储。 |

## 覆盖对照表

表中 `§1` 至 `§4` 分别指向单机、权威端、客户端、新旧版本混跑四个部分。
每行只归属一张负责票；后续票只改自己负责的行。「无条目」表示该票已在骨架
阶段确认该组合不需要条目，仍由该票复核。

| 编号 | 票 #87 验收标准与本行负责范围 | 负责票 | 单机位置 | 权威端位置 | 客户端位置 | 混跑位置 |
| --- | --- | --- | --- | --- | --- | --- |
| R01 | 新验收册明确替代已废弃的原终端图形界面验收册，并保留旧册的废弃标注。 | #269 | 册首「替代旧册声明」 | 同左 | 同左 | 同左 |
| R02 | 验收矩阵覆盖单机、权威端、客户端与新旧版本混跑四种部署与版本组合。 | #269 | §1 | §2 | §3 | §4 |
| R03 | 机密项验收覆盖录入、只写不读、快照裁剪、占位符写入、输出遮蔽、环境变量注入、下发记录、备份恢复与审计。 | #270 | §1.1 S1-01–S1-05 | §2.1 A2-01–A2-03 | §3.1 C3-01、C3-02、C3-03、C3-04、C3-W1、C3-W2、C3-PATH-01 | §4.1 M4-01–M4-02 |
| R04 | 验收过程包含捕获工具返回、审计与本地日志的检查，证明机密项值未进入 agent 上下文或审计。 | #270 | §1.1 S1-01–S1-05 通用取证 | §2.1 A2-01–A2-03 通用取证 | §3.1 C3-01、C3-02、C3-03、C3-04、C3-W1、C3-W2、C3-PATH-01 通用取证 | §4.1 M4-01–M4-02 通用取证 |
| R05 | 网络拓扑验收覆盖站点、网络、设备、多服务器条目、多地址选址、密钥不一致跳过、探测与按需查询。 | #271 | 单机271-1、单机271-2、单机271-3 | 权威端271-1 | 客户端271-1、客户端271-2 | 混跑271-1、混跑271-2 |
| R06 | 主机信任验收覆盖存量锚迁移、首次锚定、锚定转发、旧格式兜底锚与旧格式转发拒绝。 | #271 | 单机271-2、单机271-4 | 权威端271-2 | 客户端271-3 | 混跑271-1 |
| R07 | 共享存储验收覆盖登记、Linux 持久挂载、macOS 间接映射、Windows 会话内注入、撤销、残留检查与共享地址中继本地拷贝。 | #272 | §1.3 | §2.3 | §3.3 | §4.3 |
| R08 | Web UI 验收覆盖单机回环、权威端独立监听、客户端只读、管理员鉴权、敏感操作重验、主要页面与手机查看路径。 | #273 | 单机273-1、单机273-2 | 权威端273-1–权威端273-4、人工273-1 | 客户端273-1、客户端273-2 | §4.4 无条目（#273 确认） |
| R09 | Web UI 验收包含一次性回环令牌、Host 与 Origin 请求头校验、防跨站请求伪造、内容安全策略与审计。 | #273 | 单机273-1、单机273-2 | 权威端273-2、权威端273-3、权威端273-4 | 客户端273-1、客户端273-2 | §4.4 无条目（#273 确认） |
| R10a | 图形界面能力逐项对照命令行等价面，布局坐标例外需明确标注——基础页面与既有功能。 | #273 | 单机273-2 | 权威端273-4 | 客户端273-2 | §4.4 无条目（#273 确认） |
| R10b | 图形界面能力逐项对照命令行等价面，布局坐标例外需明确标注——新功能页面。 | #274 | §1.5 | §2.5 | §3.5 | §4.5 无条目 |
| R11 | 初始化与迁移验收覆盖单机向导、权威端向导、客户端配对、旧 `sshmgr tui` 提示与文档入口。 | #275 | §1.6 | §2.6 | §3.6 | §4.6 |
| R12 | Windows 反斜杠路径上传缺陷 #47 的复设用例通过，且路径进入下发记录时已规范化。 | #270 | 无条目（由 §3.1 C3-PATH-01 覆盖） | 无条目（由 §3.1 C3-PATH-01 覆盖） | §3.1 C3-PATH-01 | 无条目（缺陷属新版路径，不做旧客户端复设） |
| R13 | 备份、导出、导入与单机升级多机的验收覆盖四个功能的新数据。 | #276 | 发版检查清单占位 | 同左 | 同左 | 同左 |
| R14 | 每条验收写明前置数据、执行步骤、期望结果、证据留存方式与是否允许自动化替代；人工视觉或安全判断项单独列出。 | #269 | 条目模板与人工保留面清单框架 | 同左 | 同左 | 同左 |
| R15 | 发版检查清单要求所有验收证据齐全且失败项清零。 | #276 | 发版检查清单占位 | 同左 | 同左 | 同左 |

## 人工保留面清单框架

以下子集永远不由 agent 代跑；具体条目由对应票补写。SAS 配对双屏比对指
配对过程中两台屏幕各自显示的配对信息由 owner 人工逐项核对。

### 机密测试值人工对照（#270 填写）

#### H-SEC-01 机密测试值事实来源与值不入证据

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- owner 在验收现场自己的密码管理器或私密记录中保存一份一次性测试值；本册与运行记录只记录该事实来源的代称，不记录值。
- 测试值至少包含中文、空格、引号、百分号、反斜杠与连字符，长度不小于 32 个字节，且不含换行。
- agent 只能拿到机密项名字、环境变量名与占位符，不能拿到值；owner 不得把值粘贴到对话、工单、命令参数、截图或运行记录。
- 依据：#55 结论第 1 条与票 #270 的取证要求。
执行步骤：
- owner 在不交给 agent 的本地终端中把测试值放入进程内变量，对三份证据文件做固定字符串检索：工具返回原文、审计查询输出、本地日志文件。
- 检索结论只写「未命中」或「命中」及文件名；不保存展开后的命令、测试值或命中行内容。
- owner 逐项人工确认运行记录和截图中没有测试值、真实网络标识与账号名。
期望结果：
- 三份证据文件均未命中测试值；若任一文件命中，该项验收失败，不得删改证据后重跑掩盖。
证据留存方式：
- 留存不含值的三份检索结论、owner 的事实来源代称、人工检查时间，以及每条机密项条目的 `masked` 名单或「无 `masked` 字段」记录；`masked` 名单指结构化输出里列出被输出遮蔽替换的机密项名字的字段。
是否允许自动化替代：
- 不允许自动化替代；测试值的事实来源与最终人工对照必须保留给 owner。agent 可以代跑产品步骤，但不能拿到测试值。

### 证书指纹人工比对（#271 填写）

#### 人工271-1 主机密钥或主机证书指纹人工比对

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 依据 #45、#67 与 ADR 0011，准备一台一次性靶机和一个一次性服务器条目；
  owner 先在靶机本体控制台、虚拟化控制台或供应商页面取得主机密钥指纹。若
  执行现场提供的是 SSH 主机证书指纹，则以同一外部事实来源记录该证书指纹。
- 指纹的事实来源、取得时间和取得路径由 owner 私下记录；本册和运行记录只写
  来源类别，不写真实地址、账号或凭据。
- agent 可以准备靶机和命令，但不得代替 owner 判断两个指纹是否相同。
执行步骤：
1. owner 触发一次性条目的首次连接，让自动首次信任生成锚。
2. owner 在持有 vault 的机器执行 `sshmgr servers pin-hostkey <一次性条目>`，
   读取 `fingerprint`、`source`、`handshake` 与 `created` 字段。
3. owner 把输出中的 SHA-256 主机密钥指纹（或主机证书指纹）与第 1 步之前
   取得的带外事实逐字人工比对。
4. 比对一致时保留该锚供后续锚定转发验收使用；不一致时立即停止后续信任
   验收，执行 `sshmgr servers pin-hostkey <一次性条目> --clear` 清理错误锚，
   并把不一致作为失败证据登记。
期望结果：
- owner 记录「人工比对一致」或「人工比对不一致」；一致时 `source` 能说明
  锚的来源，`handshake` 与真实握手所用地址相符，但真实地址不出现在证据里。
- 不一致时错误锚被清除，后续条目不会继续使用这把锚。
证据留存方式：
- 留存 owner 命令原文、把真实地址替换为代称后的完整输出、人工比对结论、
  指纹事实来源类别和取得时间；需要清理时留存清理命令与完整输出。
是否允许自动化替代：
- 不允许。自动化测试只能证明显示值与存储值一致，不能替代「外部事实来源
  与呈现指纹是否相同」的人工安全判断。

### Windows 桌面手动添加引导目验（#272 填写）

<!-- 待 #272 补写：共享存储桌面手动添加引导与可见性人工检查条目 -->

### Web UI 基础页面浏览器与手机观感（#273 填写）

#### 人工273-1 基础页面手机观感

标注：适用组合=权威端；操作面=浏览器；证据来源=真机端到端
前置数据：
- owner 使用自己的手机访问公司 A 的权威端 Web UI。手机只作为浏览器，
  不入真机设备矩阵，也不在本册记录真实网络标识。
- 已完成人工273-2的证书公钥指纹人工比对；手机浏览器允许继续打开权威端
  Web UI。
- 本条只看基础查看页面的观感。网络拓扑画布、新增服务器与设备、共享存储
  授权页、服务器详情反向视图归 #274，不在本条验收。
执行步骤：
1. 在手机浏览器依次打开：`/ui/`、`/ui/access/profiles`、
   `/ui/access/projects`、`/ui/access/device-codes`、
   `/ui/access/pairing/list`、`/ui/ops/backup`、`/ui/ops/audit`、
   `/ui/secrets/`。
2. owner 逐页目验：页面能打开、标题与导航可读、列表或表单不出现必须横向
   滚动才能读完整行的情况、返回导航可用；查看与过滤操作可以用触控完成。
3. 配对批准页只检查列表、短认证码、Profile（服务器授权分组）选择与返回
   导航的观感，不提交批准或拒绝；人工配对判断归 #275。
4. owner 记录每页「过」或列出影响使用的观感问题；不得用浏览器开发工具的
   截图代替手机目验。
期望结果：
- 上述基础查看页面在手机上可用；文字、表格、过滤条件与主要按钮没有遮挡
  或超出视口后无法到达的问题。
- 发现观感问题时登记欠账，不得因页面在桌面浏览器正常而放宽判据。
证据留存方式：
- 每页留存手机截图或页面文本，另附路径清单与 owner 的逐页结论；截图先
  确认不含机密值、真实网络标识与账号名。
是否允许自动化替代：
- 不允许。本条是 owner 对手机观感的人工保留面；自动化手机视口测试只能
  作为实现前的补充证据。

#### 人工273-2 权威端证书公钥指纹人工比对

标注：适用组合=权威端；操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 公司 A 的权威端 `serve` 服务已生成证书，Web UI 登录页可访问。
- owner 在权威端主机命令行可执行 `sshmgr serve admin status`；浏览器登录页
  显示证书公钥的 SHA-256 指纹。
- 指纹事实来源是权威端主机上的证书文件；agent 可以留存两处显示，但不得
  代替 owner 判断两处指纹相同。
执行步骤：
1. owner 在公司 A 的权威端执行 `sshmgr serve admin status`，读取
   「证书公钥 SHA-256 指纹」整行。
2. owner 在浏览器打开权威端 Web UI 登录页，读取「证书公钥指纹」整行。
3. owner 把两处 `sha256:` 后的 64 位十六进制指纹逐字比对。
期望结果：
- 两处指纹完全相同；owner 记录「人工比对一致」。
- 任一字符不一致时停止后续权威端 Web UI 验收，登记失败证据，并先检查
  是否访问到错误服务或证书已被替换。
证据留存方式：
- 留存命令原文与完整输出、登录页页面文本或截图、owner 人工比对结论；
  输出中不得出现真实地址、账号名或口令。
是否允许自动化替代：
- 不允许。自动化只能证明两个显示值都被取出，不能替代 owner 对安全判断项
  的人工逐字比对。

### Web UI 新功能页面浏览器与手机观感（#274 填写）

<!-- 待 #274 补写：Web UI 新功能页面的浏览器与手机观感条目 -->

### SAS 配对双屏比对（#275 填写）

<!-- 待 #275 补写：客户端配对的 SAS 双屏人工比对条目 -->

## 1 单机组合

### 1.1 机密项与 Windows 路径（#270 填写）

本小节通用取证：

- 每条执行后保存工具返回或命令输出的完整原文；有 `masked` 字段时一并保存，
  没有该字段时在运行记录写「无 `masked` 字段」。
- 在家 A 的单机演练机查询本机 vault 审计：`sshmgr audit --action <动作名>
  --limit 0`；同一证据需要多个动作时重复 `--action` 参数。审计查询输出
  原样留档，不得截断。
- 收集本次 sshmgr 进程的标准错误；若以常驻服务运行，同时收集
  `serve.log`。按 H-SEC-01 检索工具返回、审计输出与本地日志，三者都必须
  未命中测试值。

#### S1-01 录入、只写不读与 owner 可见面

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 家 A 的单机演练机使用一次性演练 vault，不触碰生产 vault。
- owner 按 H-SEC-01 准备两个一次性测试值，创建两个机密项，并授权给一台
  指向 Linux 受管服务器 A 的一次性服务器条目。
- 依据：#28 结论第 3、4 条；#55 结论第 1 条。
执行步骤：
- owner 依次执行 `sshmgr secrets add <机密项名> --stdin`、
  `sshmgr secrets allow --secrets <机密项名 1>,<机密项名 2>
  --servers <一次性服务器条目>`、`sshmgr secrets list`。
- owner 对第二个机密项执行 `sshmgr secrets set <机密项名> --stdin` 换值，
  再执行 `sshmgr secrets list`；第一个机密项保持原值，供 S1-03 制造当前
  值与上一个值。
- agent 在同一单机工具面调用 `list_secrets`。
期望结果：
- owner 列表只显示名字、备注、环境变量名、可写入服务器、创建时间与值的
  最后更改时间；不显示值、值的哈希或值长度，也没有任何读回命令。
- `list_secrets` 每个机密项一行，只含名字、备注、环境变量名与可写入服务
  器名；不显示值、值长度、哈希、时间或授权范围外的服务器。
- 录入、换值与授权审计行完整，摘要只含名字和服务器条目名。
证据留存方式：
- 保存上述命令原文与完整输出、`list_secrets` 返回、`sshmgr audit
  --action secret-add --action secret-set --action secret-allow --limit 0`
  输出、本地日志检索结论，以及 `masked` 名单或「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；单机 vault 与真实命令行入口必须真机执行，进程内测试
  只能作为补充证据。

#### S1-02 `upload_content` 占位符写入与 Linux 权限

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 完成 S1-01；agent 只持有模板与机密项名字，不持有测试值。
- 在 Linux 受管服务器 A 上准备两个一次性绝对路径，并由 owner 建一台可用
  的一次性服务器条目；另建一台故意不可达的一次性服务器条目用于连接前
  拒绝观察。
- 依据：规格 #223；#26 结论。
执行步骤：
- agent 调用 `upload_content`，`encoding=text`，模板同时引用两个机密项，
  其中一个重复出现，写入第一个一次性路径。
- agent 把同一模板编码为单行标准 Base64 后再用 `encoding=base64` 写入第
  二个一次性路径。
- agent 在目标服务器执行 `stat -c %a <一次性路径>` 与
  `sha256sum <一次性路径>`，只取权限与 SHA-256 哈希，不读取文件内容。
- agent 以故意不可达的一次性服务器条目为目标，用含两处格式错误与一个
  未授权引用的模板调用 `upload_content`；模板包含空格名字、缺少右花括
  号的片段，以及未授权机密项引用。
- agent 再用合法占位符指向未列入可写入服务器列表的一次性目标调用
  `upload_content`。
期望结果：
- 两次成功返回的 `bytes` 均为填值后的实际字节数；`filled_secret_names`
  按首次出现顺序列出两个名字且去重；`delivery_record_status` 为
  `recorded`。
- 两个 Linux 新建文件权限都是 `600`；远端 SHA-256 哈希与 owner 在下发记
  录详情中看到的文件哈希一致。
- 格式错误与授权错误在同一次返回中全部列出。格式错误包含行号、字节偏移
  与不含值的原文片段；未授权引用报「机密项 <名字> 不存在，或不允许写入
  服务器 <条目>」，并只提示当前 profile 内的可写入服务器。
- 两次拒绝都在连接前完成：工具不返回成功，目标路径不存在，审计状态为
  `denied` 且只记名字，不记值；一个字节也不传给目标服务器。
- 格式错误调用的目标本来不可达；若实现误先连接，只能得到连接错误而不是
  整体校验错误。实际返回整体校验错误，即证明它在连接前拒绝。
证据留存方式：
- 保存每次 `upload_content` 的完整返回、远端权限与哈希输出、
  `sshmgr audit --action upload-content --limit 0` 输出、本地日志检索结
  论、`masked` 名单或「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；连接前零传输、Linux 真实文件权限与下发记录必须真机
  实证，进程内假 SSH 服务器的测试只能作为补充证据。

#### S1-03 输出遮蔽当前值与上一个值

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 完成 S1-02；第一个机密项仍是写入文件时使用的当前值。
- 依据：#28 结论第 4 条；CONTEXT.md 的「输出遮蔽」定义。
执行步骤：
- agent 调用 `exec_command` 执行 `cat <S1-02 的第一个一次性路径>`，读出
  仍含旧值的文件。
- owner 把第一个机密项换成新的测试值。
- agent 重新用同一模板调用 `upload_content` 覆盖该路径，再执行同样的
  `cat` 命令读出新值文件。
期望结果：
- 换值前的文件内容是旧值，覆盖写入后的文件内容是新值；两次
  `exec_command` 的 `stdout` 都只包含
  `{{sshmgr-secret:<机密项名>}}`，不包含旧值或新值。
- 两次结果均无未注入警告；`masked` 字段按字母顺序列出该机密项名，其他
  字段不含值。
- 审计中的命令只有 `cat <路径>`，不含值；本地日志同样检索不到值。
证据留存方式：
- 保存两次工具返回原文、`masked` 名单、上传返回、
  `sshmgr audit --action exec --action upload-content --limit 0` 输出与
  本地日志检索结论。
是否允许自动化替代：
- 不允许自动化替代；当前值与上一个值在真实工具返回中的遮蔽必须真机实证。

#### S1-04 机密项与下发记录的备份恢复

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 完成 S1-02，演练 vault 中已有两个机密项与两条下发记录。
- owner 准备一次性备份目录、目录内的 `.ssh-manager-backup-marker` 标记
  文件、一次性口令文件、一次性导出文件，以及两个空的一次性恢复 vault。
- 依据：规格 #224；#29 结论第 11 条。
执行步骤：
- owner 执行 `sshmgr backup create --dir <一次性备份目录>
  --passphrase-file <一次性口令文件> --keep 1 --prefix release87`。
- owner 执行 `sshmgr backup verify <最新 vault-*.sme 文件>
  --passphrase-file <一次性口令文件>`。
- owner 执行 `sshmgr export --out <一次性导出文件> --passphrase-file
  <一次性口令文件>`。
- owner 分别把加密备份产物与导出产物导入两个空的一次性恢复 vault：
  `sshmgr import <产物> --passphrase-file <一次性口令文件>`。
- owner 在两个恢复 vault 中执行 `sshmgr secrets records <机密项名>`。
期望结果：
- 加密备份校验通过；备份与导出产物都能完成导入。
- 两个恢复 vault 都保留机密项、可写入服务器列表与下发记录；记录详情包含
  规范化路径、模板原文、文件 SHA-256 哈希与值版本号，且任何输出都不含机
  密项值。
证据留存方式：
- 保存备份、校验、导出、导入与记录查看的命令原文和完整输出；对产物与恢
  复 vault 的审计输出做值检索；留存本地日志检索结论与 `masked` 名单或
  「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；加密备份与导出导入必须走真实命令行和真实文件。备份
  内容的逐字段一致性可用自动化校验脚本辅助，但不能替代真机往返。

#### S1-05 审计汇总与清理

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- S1-01 至 S1-04 的证据已全部落盘。
- 依据：#28、#29 的审计结论；规格 #224。
执行步骤：
- owner 执行 `sshmgr audit --action secret-add --action secret-set
  --action secret-allow --action upload-content --action exec
  --action secret-delivery-record --action secret-delivery-replace
  --action backup.create --limit 0`。
- owner 按 H-SEC-01 对工具返回、审计输出与本地日志做值检索。
- owner 删除两台一次性服务器条目并确认下发记录随之删除；agent 清理
  Linux 受管服务器 A 上的两个一次性文件；owner 删除一次性机密项、备份产
  物、导出产物、口令文件与恢复 vault。
期望结果：
- 审计查询能看到录入、换值、授权、执行、上传、下发记录登记或替换、备份
  创建的对应行；所有命令摘要都不含机密项值。
- 删除一次性服务器条目后，`sshmgr secrets records` 不再显示这两条记录；
  清理失败原样登记，不得把失败留成无证据的完成项。
证据留存方式：
- 保存审计查询完整输出、三份值检索结论、清理命令与完整输出、清理后的
  `sshmgr secrets records` 输出，以及本地日志检索结论。
是否允许自动化替代：
- 不允许自动化替代；审计查询、清理与失败留证必须在实际演练 vault 上执行。

### 1.2 网络拓扑与主机信任（#271 填写）

#### 单机271-1 站点、网络、设备与网络成员登记和删除预览

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 依据 #35、#50、#67、规格 #122 与 ADR 0011，在家 A 的单机演练机的一次性
  演练 vault 中准备：一个一次性站点、一个物理局域网、一个虚拟局域网、
  一台代表本机的一次性设备、一台一次性受管靶机设备，以及靶机的一条一次性
  服务器条目。所有名字都带一次性标记，真实地址只写入 vault。
- 靶机设备登记物理局域网地址与虚拟局域网地址；本机设备也登记为这两个网络
  的成员。
执行步骤：
1. 依次执行 `sshmgr sites add`、`sshmgr networks add --type physical`、
   `sshmgr networks add --type virtual`、`sshmgr devices add`、
   `sshmgr devices member add` 与 `sshmgr servers add --device`，创建
   一次性拓扑和服务器条目。
2. 用 `sshmgr sites list`、`sshmgr networks list`、`sshmgr devices list` 与
   `sshmgr devices show` 核对站点、网络属性、设备、多个网络成员与服务器
   条目的呈现。
3. 分别用 `sites edit`、`networks edit`、`devices edit` 与
   `devices member edit` 修改名字、备注、网段、标称带宽、是否按流量计费
   和地址；再执行一次 `networks edit --type`，确认网络类型不可修改。
4. 交互式执行 `sshmgr networks rm <一次性网络>`，确认删除前输出该网络的
   全部网络成员清单；删除最后一个网络后核对「候选地址只剩兜底 Host」的
   逐条提示。
5. 交互式执行 `sshmgr devices rm <一次性靶机设备>`，确认预览包含服务器
   条目与锚、共享存储、机密项可写入服务器列表、下发记录、网络成员、布局
   坐标、软件清单计数和客户端实例关联，然后只输入一次 `y` 确认。
6. 网络清空后执行 `sshmgr sites rm <一次性站点>`；再核对一次性服务器条目、
   设备、网络与站点均无残留，并核对审计和清理输出。
期望结果：
- 物理局域网必须归属站点、虚拟局域网不能归属站点、同一网络内地址重复被
  拒绝；不同网络的网段重复不被拦截。
- 删除网络先列出将被删除的网络成员；删除设备前给出完整分组预览，整个级联
  删除只需一次确认，客户端实例只解除关联。
- `sshmgr audit` 能查到代表性审计行：`site.add`、`network.add`、
  `device.member-add` 与 `device.move-server` 或设备删除审计；审计摘要不含
  凭据。
证据留存方式：
- 留存全部命令原文与完整输出、交互确认输入、审计查询输出和清理输出；
  输出中的真实地址必须先替换为网络名加序号后再入运行记录。
是否允许自动化替代：
- 不允许用自动化测试替代。删除预览、一次确认和真机命令行输出是本条目的
  发版判据；进程内测试只能作为代码合入前的补充证据。

#### 单机271-2 同设备多个服务器条目与真实选址

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 依据 #35、#36、#45、#50、#67 与 ADR 0011，在家 A 的单机演练机上保留
  单机271-1的一次性物理局域网与虚拟局域网；一次性靶机设备登记两个网络的
  地址，本机设备也登记在两个网络里。
- 创建两个一次性服务器条目：条目 A 先关联一次性靶机设备；条目 B 先留在
  自动生成的未归类设备上，稍后移入同一设备。两者可以使用不同用户、端口
  和一次性凭据。
- 执行 `sshmgr devices local set <本机一次性设备>`，让单机部署知道实际拨号
  设备是哪台。
执行步骤：
1. 执行 `sshmgr ssh <条目A> <只读命令>`，让条目 A 首次锚定。
2. 执行 `sshmgr devices move-server <条目B> --to <一次性靶机设备>`；同端口
   兄弟条目应复制条目 A 的锚，并写 `pin-copy` 审计。
3. 用 `sshmgr devices show <一次性靶机设备>` 核对两个服务器条目都在同一
   设备下，并分别显示各自用户、端口和锚状态；再执行
   `sshmgr ssh <条目B> <只读命令>`，核对条目 B 也可按共同网络成员地址连接。
4. 在本机 vault 的命令行执行 `sshmgr servers pin-hostkey --list`，核对两个
   条目各有一把锚；随后执行 `sshmgr servers pin-hostkey <条目A> --clear`，
   再显示条目 A 与条目 B 的锚，核对清除只影响条目 A。
5. 把本机设备移出一次性物理局域网后重试条目 A，核对连接改走虚拟局域网
   地址；再把本机设备移出全部共同网络，并让该条目的兜底主机不可达，核对
   失败提示要求补登记本机设备与目标设备的共同网络。
6. 验收结束后清除本机设备设定，并删除两个一次性服务器条目、靶机设备、
   网络与站点。
期望结果：
- 两个条目共用设备网络成员地址，但端口、用户、凭据和锚彼此独立；成功地址
  缓存也按条目隔离。
- 同在物理局域网时先走物理局域网地址；失去物理共同网络后走虚拟局域网地址；
  无共同网络时只尝试兜底主机，失败提示补登记，不盲试目标全部地址。
证据留存方式：
- 留存命令原文、把真实地址替换为代称后的完整输出、`pin-hostkey --list`
  脱敏输出、`pin-copy` 审计摘要、清理输出和本机 `probe-results.json` 中
  只含网络名与顺序的尝试摘要；不得把真实地址、账号或凭据写入证据。
是否允许自动化替代：
- 不允许用自动化测试替代真实网络切换；同级成功地址缓存和密钥不一致的
  精确顺序另由单机271-3自动化测试代证。

#### 单机271-3 多地址顺序、成功地址缓存与密钥不一致代证

标注：操作面=agent 工具面；证据来源=自动化测试代证
前置数据：
- 依据 #36、#44、#45、#67、规格 #132 与 ADR 0011，候选发行版源码树包含
  统一拨号入口的进程内 SSH 服务器测试夹具。
执行步骤：
1. 在源码根目录执行：
   `go test ./internal/sshbroker -run '^(TestPlanServerDialAddressesOrdersAndDeduplicates|TestConnectServerEntryTriesPlannedAddressesInOrder|TestConnectServerEntrySuccessCacheOrdersSameLevelByDuration|TestConnectServerEntryMismatchFailsWithoutSendingCredentials|TestConnectServerEntrySkipsMismatchedHostKeyThenConnectsNextAddress|TestConnectServerEntrySkipsSuccessCacheForFallbackOnlyPlan)$'`
2. 再执行：
   `go test ./internal/mcpserver -run '^TestQueryTopologyPairActualSelectionUsesDialSuccessCache$'`
期望结果：
- 测试证明：物理局域网候选先于虚拟局域网候选，兜底主机最后；同级地址先按
  仍有效的成功记录和建连耗时排序，没有缓存时按登记顺序。
- 测试证明：无共同网络或拨号设备未知时只用兜底主机；某个地址呈现不同主机
  密钥时先跳过它且不发送登录凭据，再尝试下一个地址，被跳过地址不进入成功
  地址缓存。
- 拓扑查询标记的「实际会选」与统一拨号入口的成功地址缓存排序一致。
证据留存方式：
- 留存两条 `go test` 命令原文与完整输出、测试所在提交的完整提交哈希。
是否允许自动化替代：
- 允许且必须自动化替代。本条目验证的是进程内可精确观测的尝试顺序和
  “未发送认证请求”断言；真机上难以在不污染目标认证日志的情况下稳定复现。

#### 单机271-4 存量锚迁移与旧格式兜底锚推导代证

标注：操作面=agent 工具面；证据来源=自动化测试代证
前置数据：
- 依据 #45、#67、规格 #132 与 ADR 0011，候选发行版源码树包含旧格式
  「主机:端口」锚快照和按条目锚快照的测试夹具。
执行步骤：
1. 在源码根目录执行：
   `go test ./internal/store -run '^(TestImportSnapshot_OldShapeAppliesEntryMigration|TestSnapshotEntryPins_ExportAndLegacyDerivation)$'`
期望结果：
- 旧格式锚导入时复制给所有兜底「主机:端口」相同的服务器条目；没有条目指向
  的孤立锚被丢弃，并写 `pin-migrate` 审计。
- 新快照同时携带按服务器条目的锚和为旧客户端推导的旧格式兜底锚；同一兜底
  地址上多个条目锚不同时不推导旧格式行，避免旧客户端导入冲突。
证据留存方式：
- 留存 `go test` 命令原文与完整输出、测试所在提交的完整提交哈希。
是否允许自动化替代：
- 允许自动化替代。构造旧数据库形状和孤立锚属于一次性测试数据，直接碰生产
  vault 反而违反一次性靶子护栏；真机旧客户端行为另由混跑271-1验收。

### 1.3 共享存储（#272 填写）

<!-- 待 #272 补写：单机组合的共享存储条目 -->

### 1.4 Web UI 基础、安全与等价面（#273 填写）

#### 单机273-1 回环监听、一次性启动令牌与退出即停

标注：适用组合=单机；操作面=浏览器；证据来源=真机端到端
前置数据：
- 家 A 的单机演练机使用一次性演练 vault，不触碰生产 vault；本机角色为
  单机，且已有可供页面显示的服务器条目。
- 依据：#38 结论第 3 条；#39 结论第 1 条；#49 结论第 14 条；ADR 0010、
  0012、0013；CONTEXT.md 的「Web UI（管理界面）」。
执行步骤：
1. owner 在家 A 的单机演练机执行 `sshmgr ui`，保存终端输出；输出中的一
   次性启动令牌必须先替换为 `<一次性启动令牌>` 再入运行记录。
2. 确认输出网址以 `http://127.0.0.1:<随机端口>/ui/start?token=` 开头；
   浏览器打开后落在不带令牌的 `/ui/`。
3. 在浏览器开发者工具中保存 `/ui/` 的响应头与会话 cookie 属性；令牌值与
   cookie 值不得进入证据。
4. 使用同一个启动网址第二次访问；保存错误页页面文本。
5. 在候选发行版源码根目录执行：
   `go test ./internal/webui -run '^(TestHostValidationRejectsNonLoopbackHosts|TestWriteRouteRequiresMatchingOriginAndCSRFToken|TestCSRFTokenBoundToSession|TestAllWriteMethodsRequireOriginAndCSRFToken|TestSecurityHeadersOnEveryResponse)$'`
6. 对 `sshmgr ui` 发送终止信号或关闭其终端；等待命令退出后刷新浏览器页。
期望结果：
- Web UI 只监听 `127.0.0.1` 的随机端口；启动令牌首次使用即换取进程内会话，
  第二次使用返回 403，并提示重新运行 `sshmgr ui` 获取新的启动链接。
- 会话 cookie 名包含本次端口，属性为 `HttpOnly`、`SameSite=Strict`、
  `Path=/ui/`，且不带 `Secure`。
- 页面响应包含内容安全策略 `default-src 'none'; script-src 'self';
  style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self';
  base-uri 'none'; form-action 'self'; frame-ancestors 'none'`，并带
  `X-Content-Type-Options: nosniff`、`Referrer-Policy: same-origin` 与
  `Cache-Control: no-store`。
- 自动化测试证明非回环 Host、错误 Origin、缺失或错误防跨站请求伪造令牌
  的写请求被拒绝，且令牌与会话绑定。
- `sshmgr ui` 退出后浏览器无法再连接；没有常驻进程继续提供页面。
证据留存方式：
- 留存脱敏后的启动输出、`/ui/` 页面文本、响应头与会话 cookie 属性、令牌
  二次使用错误页、自动化测试完整输出、命令退出与浏览器刷新后的失败截图。
是否允许自动化替代：
- 不允许用自动化替代真实回环监听、令牌一次性、退出即停与浏览器响应头；
  第 5 步测试只作为 Host、Origin 与防跨站请求伪造拒绝分支的补充证据。

#### 单机273-2 单机基础页面、命令行等价面与写操作审计

标注：适用组合=单机；操作面=浏览器；证据来源=真机端到端
前置数据：
- 完成 S1-01，家 A 的单机演练机已有服务器条目、机密项与审计记录；备份
  配置可读。本条不执行破坏性服务器、机密项或备份操作。
- 设备码与配对是权威端专属能力，单机 Web UI 不注册对应页面；本条按单机
  适用页面逐一对照。
- 依据：#38 结论第 2、6 条；#39 结论第 8 条；#55 结论第 4 条；ADR 0003、
  0010；CONTEXT.md 的「Web UI（管理界面）」。
执行步骤：
1. owner 启动单机 Web UI 后逐页打开基础页面：
   `/ui/`、`/ui/access/profiles`、`/ui/access/projects`、`/ui/secrets/`、
   `/ui/ops/backup`、`/ui/ops/settings`、`/ui/ops/update`、`/ui/ops/audit`。
2. 对照命令行等价面并保存输出：
   - 服务器列表、需要注意过滤、详情：`sshmgr servers ls [--attention]`、
     `sshmgr servers show <条目>`；
   - 服务器编辑、删除、导入、补全：`sshmgr servers edit <条目>`、
     `sshmgr servers rm <条目>`、`sshmgr servers import`、
     `sshmgr servers complete`；
   - 服务器主机密钥锚查看、覆盖与删除：`sshmgr servers pin-hostkey <条目>`、
     `sshmgr servers pin-hostkey <条目> --fingerprint <指纹> --force`、
     `sshmgr servers pin-hostkey <条目> --clear`；
   - Profile（服务器授权分组）：`sshmgr profiles ls`、
     `sshmgr profiles add <名>`、`sshmgr profiles grant <Profile> <条目...>`、
     `sshmgr profiles revoke <Profile> <条目...>`、
     `sshmgr profiles remove <名>`；
   - 项目令牌：`sshmgr projects ls`、`sshmgr projects show <项目>`、
     `sshmgr projects add <项目> --profile <Profile>`、
     `sshmgr projects rotate <项目>`、`sshmgr projects disable <项目>`、
     `sshmgr projects enable <项目>`、`sshmgr projects revoke <项目>`、
     `sshmgr projects remove <项目>`；
   - 机密项：`sshmgr secrets list`、`sshmgr secrets add <名> --stdin`、
     `sshmgr secrets set <名> --stdin`、`sshmgr secrets edit <名>`、
     `sshmgr secrets allow --secrets <名...> --servers <条目...>`、
     `sshmgr secrets disallow --secrets <名...> --servers <条目...>`、
     `sshmgr secrets records <名>`、`sshmgr secrets rm <名>`；
   - 备份：`sshmgr backup status`、`sshmgr backup create`、
     `sshmgr backup verify --latest`、`sshmgr backup config show`、
     `sshmgr backup config set`；
   - 设置与升级：`sshmgr settings ls`、`sshmgr settings get <设置项>`、
     `sshmgr settings set <设置项> <值>`、`sshmgr settings unset <设置项>`、
     `sshmgr update`；
   - 审计：`sshmgr audit --source web_ui --action <动作名> --limit 100`。
3. 在浏览器创建一个一次性 Profile（服务器授权分组），确认列表出现；再在
   浏览器删除该一次性 Profile，确认清理完成。
4. 打开 `/ui/ops/audit?source=web_ui`，核对第 3 步的写操作行；再执行
   `sshmgr audit --source web_ui --action profile.add --action profile.rm
   --limit 100` 对照。
期望结果：
- 浏览器页面与命令行输出对同一 vault 数据给出相同条目名、条目数、状态与
  过滤结果；页面显示的每项既有能力都能在上面的命令行等价面中找到。
- 机密项页面只显示名字、备注、环境变量名、可写入服务器与时间，不显示值。
- 一次性 Profile 的新增与删除都产生来源为 `web_ui` 的 `profile.add` 与
  `profile.rm` 审计；审计摘要不含机密值或凭据。
- 清理后 Profile 列表无该一次性条目；失败清理不得记为完成。
证据留存方式：
- 留存每个页面路径与页面文本或截图、所有对照命令原文与完整输出、浏览器
  写操作页面、审计页面与命令输出、一次性 Profile 清理证据。
是否允许自动化替代：
- 不允许。页面呈现、命令行等价面对照和真实单机 vault 审计必须真机执行；
  进程内测试只能作为实现前的补充证据。

### 1.5 Web UI 新功能页面（#274 填写）

<!-- 待 #274 补写：单机组合的 Web UI 新功能页面条目 -->

### 1.6 初始化迁移与版本混跑（#275 填写）

<!-- 待 #275 补写：单机组合的初始化、迁移与版本混跑条目 -->

## 2 权威端组合

### 2.1 机密项与 Windows 路径（#270 填写）

本小节通用取证：

- 在公司 A 的权威端保存工具返回或命令输出完整原文；有 `masked` 字段时一
  并保存，没有该字段时在运行记录写「无 `masked` 字段」。
- 权威 vault 审计用 `sshmgr audit --action <动作名> --limit 0` 查询；同一
  证据需要多个动作时重复 `--action` 参数。
- 收集权威端 `serve.log` 与执行命令的标准错误；涉及客户端的条目同时在家
  A Windows 新版客户端执行 `sshmgr cache audit --instance <一次性实例>
  --limit 0`。按 H-SEC-01 检索工具返回、权威审计与两台机器的本地日志，
  全部证据都必须未命中测试值。
- 本小节创建的一次性服务器条目、一次性实例、一次性机密项、授权与远端一
  次性文件都是靶子。除同一靶子明确供后续条目继续使用外，每条取证完成后
  必须清理；清理命令、完整输出与清理后的记录查询结果一并留档，失败不得
  记为完成。

#### A2-01 权威端录入、只写不读与审计

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 在公司 A 的权威端创建一个一次性机密项与一台指向 Linux 受管服务器 A 的
  一次性服务器条目；机密项只授权给这台一次性条目。
- owner 按 H-SEC-01 保存测试值的事实来源。
- 依据：#28 结论第 3、4 条；#55 结论第 1 条。
执行步骤：
- owner 执行 `sshmgr secrets add <一次性机密项名> --stdin`，再执行
  `sshmgr secrets allow --secrets <一次性机密项名> --servers <一次性服务
  器条目>` 与 `sshmgr secrets list`。
- owner 执行 `sshmgr secrets set <一次性机密项名> --stdin` 换值，再执行
  `sshmgr secrets list`。
- owner 查询录入、授权与换值审计。
期望结果：
- 两次列表都只显示机密项的安全字段，不显示值、值的哈希或值长度；换值只
  改值、上一个值与值版本号，备注和可写入服务器列表保持不变。
- 审计包含 `secret-add`、`secret-allow`、`secret-set` 动作，摘要只含名字、
  服务器条目与结果，不含值。
证据留存方式：
- 保存命令原文与完整输出、`sshmgr audit --action secret-add --action
  secret-allow --action secret-set --limit 0` 输出、权威端本地日志检索结
  论，以及 `masked` 名单或「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；必须在真实权威端 vault 与命令行入口执行。

#### A2-02 按 profile 裁剪机密项快照

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- owner 在权威端创建两台指向 Linux 受管服务器 A 的一次性服务器条目：一
  台授予家 A Windows 新版客户端的 profile，另一台不授予。
- owner 创建两个一次性机密项：一个同时授权给两台一次性服务器条目；另一
  个只授权给 profile 外的那台条目。
- owner 再创建第三台指向 Linux 受管服务器 A 的一次性服务器条目，授予该
  profile，但不加入任何一次机密项的可写入服务器列表。
- 客户端执行一次 `sshmgr cache pull --instance <一次性实例>`，拿到按
  profile 裁剪后的加密快照。
- 依据：#25 结论；#28 结论第 11 条；ADR 0014。
执行步骤：
- agent 在客户端调用 `list_secrets`，不带参数。
- agent 再分别调用 `list_secrets`，一次传 `server=<profile 内一次性服务
  器条目>`，一次传 `server=<profile 外一次性服务器条目>`。
- agent 用只授权给 profile 外条目的机密项占位符，调用 `upload_content`
  写入第一台 profile 内条目；再用任一机密项占位符写入第三台一次性条目。
- agent 最后用任一占位符写入不授予该 profile 的第二台一次性条目。
期望结果：
- 不带参数的列表只显示同时与 profile 有交集的机密项；其可写入服务器只显
  示 profile 内那台条目，不显示 profile 外条目。
- 只授权在 profile 外的机密项不出现在任何列表；用 profile 外条目筛选时，
  工具返回「没有可用的机密项」。
- 写入 profile 内但未授权的机密项时报「机密项 <名字> 不存在，或不允许写
  入服务器 <条目>」；写入 profile 外服务器条目时在 profile 授权检查处拒绝。
  两种拒绝都不连接服务器、不创建文件，审计不包含值。
证据留存方式：
- 保存客户端 `cache pull` 输出、三次 `list_secrets` 返回、三次
  `upload_content` 返回、权威端 `sshmgr audit --action secret-allow --limit 0`
  输出、客户端 `sshmgr cache audit --instance <一次性实例> --limit 0` 输
  出、两台机器本地日志检索结论，以及 `masked` 名单或「无 `masked` 字段」
  记录。
是否允许自动化替代：
- 不允许自动化替代；快照裁剪必须穿过真实权威端、设备码、加密快照与客户
  端桥进程。进程内快照测试只能作为补充证据。

#### A2-03 下发记录查看、删除预览与随删

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 家 A Windows 新版客户端上的 agent 已用一个一次性机密项，经
  `upload_content` 把含占位符的文件写到一台一次性 Linux 服务器条目，并得
  到 `delivery_record_status="recorded"`。
- 同一机密项还写到第二台一次性服务器条目，形成两条不同路径的记录。
- 依据：规格 #224；#29 结论；ADR 0015。
执行步骤：
- owner 在权威端执行 `sshmgr secrets records <一次性机密项名>`。
- owner 执行 `sshmgr secrets records --server <第一台一次性服务器条目>`。
- owner 执行 `sshmgr secrets rm <一次性机密项名>`；命令会先列出引用该机
  密项的全部下发记录，然后删除机密项。
- owner 再次执行 `sshmgr secrets records <一次性机密项名>`。
- owner 执行 `sshmgr servers rm <第一台一次性服务器条目>`，人工在交互确
  认中输入 `y`。
- owner 再次执行 `sshmgr secrets records`。
- 取证完成后，owner 同样删除第二台一次性服务器条目；agent 清理两台一次
  性服务器条目对应的远端一次性文件。
期望结果：
- 按机密项查看时展开两条记录：服务器条目、路径、机密项名字、写入时间、
  发起设备、实例、模板原文、文件 SHA-256 哈希与值版本号；任何视图都不显
  示值。
- 按服务器筛选只显示该服务器条目的记录；同一服务器同一路径只保留最新一
  条。
- `secrets rm` 前置预览列出两条服务器条目与路径；删除后记录保留，机密项
  名字标注「已删除」。
- `servers rm` 的影响预览包含「会随之删除的下发记录」，并列出该条目的记
  录；确认删除后这些记录随服务器条目消失，另一台条目的记录仍保留。
- 第二台一次性服务器条目删除后，全部测试下发记录消失；远端一次性文件不
  存在。清理失败原样登记，不得记为完成。
证据留存方式：
- 保存客户端上传返回、全部 owner 命令与远端清理命令原文及完整输出、`sshmgr audit
  --action secret-rm --action server.delete --action secret-delivery-rm
  --limit 0` 输出、权威端与客户端本地日志检索结论，以及 `masked` 名单或
  「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；owner 查看面、删除前预览、人工确认与级联删除必须走
  真实权威端。所有删除动作只允许使用一次性机密项与一次性服务器条目。

### 2.2 网络拓扑与主机信任（#271 填写）

#### 权威端271-1 本机与经服务器的连通探测和只读拓扑查询

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 依据 #35、#36、#44、#67、规格 #142 与规格 #234，在公司 A 的权威端登记
  一次性本机设备、Linux 受管服务器 A 与 NAS A；本机设备与两台目标至少同在
  一个物理局域网，另有一个只经虚拟局域网可达的一次性目标、一个无共同网络
  的目标和一个未关联设备的一次性服务器条目。
- 网络登记包含标称带宽与是否按流量计费；执行 `sshmgr devices local set`
  设定权威端本机设备。受管服务器条目保持地址隐藏。
执行步骤：
1. 调用 `probe_connectivity`，参数 `from="self"`、`targets` 包含 Linux
   受管服务器 A 的编号和 `NAS A:445`。
2. 调用 `probe_connectivity`，参数 `from` 为 Linux 受管服务器 A 的编号、
   `targets` 包含 `NAS A:445`，让权威端经 SSH 在受管服务器上执行固定探测
   命令。
3. 调用 `query_topology` 时不传 `from`，再调用 `from="self"` 且不传 `to`，
   最后调用 `from="self"`、`to` 包含多个目标且分别设置 `detail=false` 与
   `detail=true`。
4. 在查询前后执行 `sshmgr audit --limit 20`，核对只读查询没有新增审计行。
5. 读取权威端本机 `probe-results.json` 的摘要，确认两次主动探测的观察都带
   观察时间；证据中只保留设备、网络、端口、通否、耗时和相对时间。
6. 删除本条使用的一次性拓扑登记和服务器条目；本地探测结果文件不属于
   vault，不做删除演练。
期望结果：
- 缺少 `from` 时调用报错；邻居视图包含出发点所属网络，以及「同在物理局域
  网」「只能经虚拟局域网到达」「无共同网络不可达」「未登记服务器」四类分组。
- 默认输出一行一台设备，只带设备、所走网络和最近探测结果的通否、耗时与
  多久以前；`detail=true` 才展开地址、标称带宽、是否按流量计费和设备角色。
- 经服务器探测输出仍按网络名加第几个地址指称地址，不暴露真实地址；审计有
  `probe-connectivity` 行，摘要包含起点、目标端口和起点设备，远程探测注明
  固定命令不经命令策略。
- `query_topology` 不发起探测、不写审计、不改变探测结果；输出末尾提示需要
  现场结论时调用 `probe_connectivity`。
证据留存方式：
- 留存两次探测调用与完整紧凑输出、三次查询调用与完整输出、查询前后审计
  输出、本地探测结果摘要和执行位置；真实地址必须打码。
是否允许自动化替代：
- 不允许用自动化测试替代。本条目要在公司 A 的权威端、Linux 受管服务器 A
  和 NAS A 的真实网络路径上验证；进程内测试只能作为实现前置证据。

#### 权威端271-2 首次锚定转发落库与握手地址约束

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 依据 #45、#50、#67、规格 #132 与 ADR 0011，公司 A 的权威端有一个尚未锚定
  的一次性服务器条目；其设备同时登记物理局域网地址与虚拟局域网地址，且新
  客户端经虚拟局域网可以到达它。
- 新客户端的一次性命名实例已用一次性设备码绑定到授予该条目的访问配置组，
  并完成 `sshmgr cache pull`；权威端 `serve` 服务日志可留存。
执行步骤：
1. owner 在新客户端执行 `sshmgr ssh <一次性条目> <只读命令>`，触发首次
   信任和锚定转发。
2. owner 在权威端执行 `sshmgr servers pin-hostkey <一次性条目>`，读取锚的
   指纹、来源、握手地址与时间。
3. owner 执行 `sshmgr audit --action pin-forward --limit 10`，核对审计中的
   条目、握手地址、指纹和设备码名字。
4. owner 把显示的握手地址与该条目登记的虚拟局域网地址私下比对，不把真实
   地址写入运行记录。
5. 在源码根目录执行：
   `go test ./internal/mcpserver -run '^(TestPinHostkey_ForwardsAndLands201|TestPinHostkey_HandshakeAddrNotCandidate400|TestPinHostkey_MultiAddressHandshakeValidation)$'`
6. 删除一次性服务器条目与设备拓扑登记；客户端缓存与本地探测结果随一次性
   命名实例目录一起删除。
期望结果：
- 首次连接只经虚拟局域网地址完成握手；权威端只允许新增锚，并把条目标识、
  真实握手地址、密钥和设备码名字写入同一条 `pin-forward` 审计。
- 非候选握手地址、兜底地址或条目已有虚拟局域网地址时的非虚拟局域网握手
  地址都被拒绝；自动化测试同时证明这些约束。
- 客户端命令成功，锚的来源显示为转发，人工271-1的指纹比对结论为一致。
证据留存方式：
- 留存客户端命令原文与完整输出、把真实地址替换为代称后的权威端锚显示、
  同样脱敏的 `pin-forward` 审计、把握手地址替换为「虚拟局域网地址」的
  服务日志状态行、自动化测试完整输出和一次性数据清理输出。
是否允许自动化替代：
- 真实首次锚定、转发落库和权威端显示不允许自动化替代；握手地址非法分支
  允许用第 5 步自动化测试代证，避免人为伪造转发请求。

### 2.3 共享存储（#272 填写）

<!-- 待 #272 补写：权威端组合的共享存储条目 -->

### 2.4 Web UI 基础、安全与等价面（#273 填写）

#### 权威端273-1 独立监听与虚拟局域网地址交集推导

标注：适用组合=权威端；操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 公司 A 的权威端已初始化，`serve` 与 Web UI 独立监听运行；拓扑中已有
  虚拟局域网成员地址，且该地址存在于权威端本机网卡上。
- 权威端主监听端口与 Web UI 独立端口按实际设置记录；证据中的真实地址先
  替换为「权威端虚拟局域网地址」「权威端物理局域网地址」等代称。
- 依据：#39 结论第 2 条；#49 结论第 11–13 条；#55 结论第 3、6 条；ADR
  0013。
执行步骤：
1. owner 在公司 A 的权威端执行 `sshmgr serve admin status`，保存 Web UI
   端口、全部 Web UI 地址与证书指纹输出。
2. owner 从虚拟局域网里的浏览器打开状态输出中的 Web UI 地址，确认登录页
   可用；主监听端口上的 `/ui` 与 `/ui/login` 应返回 404。
3. owner 从物理局域网地址访问同一 Web UI 端口，确认连接被拒绝；该真实地址
   只私下使用，证据写代称。
4. owner 把状态输出的非回环地址与拓扑虚拟局域网成员地址、本机网卡地址的
   交集私下核对；不应出现未同时存在于两侧的物理局域网地址。
5. 在候选发行版源码根目录执行：
   `go test ./internal/webui -run '^(TestDeriveListenAddresses|TestDeriveListenAddressesRejectsInvalidManualAddress)$'`
6. 在候选发行版源码根目录执行：
   `go test ./internal/mcpserver -run '^(TestRunServeSeparatesWebUIListenerFromMainListener|TestRunServeFallsBackToLoopbackWhenListenDerivationFails|TestWebUIListenerManagerClosesOldAndOpensNewListeners|TestWebUIListenerManagerRetriesFailedAddressAfterThirtySeconds)$'`
期望结果：
- Web UI 与主监听使用不同端口；主监听不再提供 `/ui`，Web UI 独立监听提供
  登录页。
- 默认监听地址是拓扑里全部设备的虚拟局域网成员地址与本机网卡地址的交集，
  并始终包含 `127.0.0.1`；物理局域网地址不会被自动加入。
- 交集为空时只监听 `127.0.0.1`，并提示用 `serve.ui_listen` 手动指定；
  `serve.ui_listen` 可覆盖地址集合，非法 IP 地址被拒绝。
- 绑定失败的地址按测试证明的节奏重试，不再需要的旧地址会被关闭。
证据留存方式：
- 留存状态命令原文与脱敏完整输出、虚拟局域网浏览器登录页文本、主监听 404
  响应、物理地址拒绝证据、owner 的交集核对结论、两条自动化测试完整输出。
是否允许自动化替代：
- 不允许用自动化替代真实权威端双端口与虚拟局域网路径；地址交集、交集为空
  与重试契约允许第 5、6 步测试代证。

#### 权威端273-2 管理员口令登录、锁定、会话与重验证

标注：适用组合=权威端；操作面=浏览器；证据来源=真机端到端
前置数据：
- 公司 A 的权威端 Web UI 可从 `127.0.0.1` 访问；owner 知道当前管理员口令。
  全部失败登录演练只用 `127.0.0.1` 这个来源地址，避免把其他设备锁住。
- 依据：#39 结论第 3–9 条；ADR 0010、0013；CONTEXT.md 的「管理员口令」与
  「会话代」。
执行步骤：
1. owner 在权威端本机浏览器用正确管理员口令登录，保存登录响应与会话
   cookie 属性，再注销。
2. owner 用错误口令登录一次，保存错误页与耗时；紧接着用正确口令登录，记
   录成功登录耗时，观察全局延迟。随后注销。
3. owner 连续五次用错误口令登录；第五次应显示该来源 IP 已锁定。保存第五次
   错误页后，在权威端执行 `sshmgr serve admin unlock 127.0.0.1` 解除锁定。
4. owner 重新登录，先打开 `/ui/password`，确认 5 分钟内直接显示修改口令页；
   不提交修改。
5. 保持该浏览器会话 5 分零 1 秒无操作，再打开 `/ui/password`；应跳转到
   `/ui/reverify?next=/ui/password`。输入正确口令后应回到修改口令页。
6. 再次保持会话 5 分零 1 秒无操作，继续用一次性靶子核对其他敏感写入口：
   新增机密项、机密项换值、修改机密项备注或环境变量名、删除机密项、立即
   备份、删除一次性服务器条目、导出或恢复备份、撤销一次性实例。逐项发起
   提交；未重验时都应先跳到重新验证页，且不写入任何靶子数据。若页面缺少
   其中某个入口，按「页面缺失」登记失败，不得改用其他能力代替。随后用正
   确口令完成一次重新验证。
7. 在候选发行版源码根目录执行：
   `go test ./internal/webui -run '^(TestAuthorityLoginExchangesPasswordForSecureSession|TestAuthoritySessionSlidesAndGenerationInvalidates|TestAuthorityLoginFailuresLockIPAndDelayAllRequests|TestAuthorityLoginGlobalDelayIsCappedAtTenSeconds|TestAuthorityPasswordChangeRequiresRecentReverification)$'`
8. owner 检查登录页：没有找回口令入口。重置已忘记口令的唯一入口写明为在
   权威端主机执行 `sshmgr serve admin set`；该命令会使全部 Web 会话作废并
   写审计。
期望结果：
- 正确口令登录只签发名为 `__Host-sshmgr_ui` 的会话 cookie，属性为 `Secure`、
  `HttpOnly`、`SameSite=Strict`、`Path=/`；登录成功、失败、锁定与注销分别
  产生 `admin.login`、`admin.login_fail`、`admin.lockout`、`admin.logout`
  审计。
- 第一次错误登录后，后续所有登录请求至少多等 1 秒；全局延迟最多 10 秒。
- 同一来源地址 15 分钟内第五次失败后锁定 15 分钟；锁定期间不校验口令，
  `sshmgr serve admin unlock 127.0.0.1` 能解除。
- 会话按最后一次活动时间滑动，12 小时无活动失效；会话代变化后旧会话立即
  失效。这两个时间边界由第 7 步测试代证。
- 超过 5 分钟未重验时，修改管理员口令页先要求重新输入口令；重验成功后回
  到原页面。新增、修改或删除机密项，导出或恢复备份，删除服务器，撤销实
  例等敏感写入口同样要求 5 分钟内重新验证；未重验时不得写入数据。口令忘
  记时没有浏览器找回入口，只能权威端命令行重置。
证据留存方式：
- 留存浏览器页面文本、截图、响应头与 cookie 属性（不含 cookie 值）、每次
  登录耗时、锁定与解锁命令输出、审计查询输出、自动化测试完整输出；口令
  不得进入任何证据。
是否允许自动化替代：
- 不允许用自动化替代真实浏览器登录、错误口令、按来源地址锁定、解锁与
  5 分钟重验；12 小时滑动会话与全局延迟 10 秒封顶允许第 7 步测试代证。

#### 权威端273-3 Host、Origin、防跨站请求伪造与审计来源

标注：适用组合=权威端；操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 权威端273-2已完成一次有效登录，浏览器会话可用于本条；agent 在权威端本机
  发送测试请求，不改动 vault 数据。
- 依据：#39 结论第 1、8 条；#49 结论第 6 条；ADR 0012；#55 结论第 6 条。
执行步骤：
1. 保存 `/ui/login` 与登录后的 `/ui/` 响应头，核对内容安全策略、
   `X-Content-Type-Options`、`Referrer-Policy` 与 `Cache-Control`。
2. 用本地 HTTP 客户端带错误 `Host` 请求 Web UI 登录页，保存状态码与页面
   文本。
3. 复用有效会话 cookie，但分别发送 `Origin: https://example.invalid` 的写
   请求、来源正确但缺失防跨站请求伪造令牌的写请求；两组请求都以
   `/ui/access/profiles` 为目标。保存两次状态码与页面文本。令牌和 cookie
   值不得进入证据。
4. 在候选发行版源码根目录执行：
   `go test ./internal/webui -run '^(TestAuthorityHostOriginAndCSRFGuards|TestAuthorityLoginExchangesPasswordForSecureSession)$'`
5. 引用权威端273-4的一次性 Profile 写操作审计，或在同一会话中用页面提交一
   次一次性 Profile 新增；随后执行
   `sshmgr audit --source web_ui --action admin.login --action profile.add
   --limit 100`。
期望结果：
- 所有页面响应都带第 1 步列出的安全响应头；HTML 不缓存。
- 错误 `Host` 被拒绝；错误或缺失 `Origin` 的写请求返回 403，缺失防跨站请
  求伪造令牌也返回 403，不落入业务处理器。
- 登录表单的防跨站请求伪造令牌与 cookie 绑定；登录后的写请求令牌与会话
  绑定。
- 登录与经 Web UI 发起的写操作都有来源为 `web_ui` 的审计行；来源地址与审
  计摘要不包含口令、cookie、令牌或机密值。
证据留存方式：
- 留存响应头、三组拒绝请求的命令原文与完整输出（敏感值先替换为占位符）、
  审计查询输出、自动化测试完整输出。
是否允许自动化替代：
- 不允许自动化替代真实权威端监听上的请求与审计；第 4 步测试只作为分支
  覆盖补充。

#### 权威端273-4 权威端基础页面、命令行等价面与唯一例外

标注：适用组合=权威端；操作面=浏览器；证据来源=真机端到端
前置数据：
- 公司 A 的权威端已有服务器条目、Profile（服务器授权分组）、项目令牌、
  设备码、备份状态、审计记录与机密项；配对队列可以为空，但页面必须可打
  开。本条不提交配对批准。
- owner 已完成权威端273-2的登录与重验证，距上次验证不足 5 分钟。
- 设备码与配对是权威端专属页面；本条把两页与单机共有的服务器、Profile
  （服务器授权分组）、项目令牌、备份、审计、机密项页面一起逐项对照。
- 依据：#38 结论第 2、6 条；#39 结论第 8 条；#49 结论第 6 条；#55 结论第
  4 条；ADR 0003、0010、0012。
执行步骤：
1. owner 逐页打开：
   `/ui/`、`/ui/access/profiles`、`/ui/access/projects`、
   `/ui/access/device-codes`、`/ui/access/pairing/list`、`/ui/secrets/`、
   `/ui/ops/backup`、`/ui/ops/settings`、`/ui/ops/update`、`/ui/ops/audit`。
2. 对照命令行等价面并保存输出：
   - 服务器列表、过滤、详情、编辑、删除、导入、补全：`sshmgr servers ls
     [--attention]`、`sshmgr servers show <条目>`、`sshmgr servers edit
     <条目>`、`sshmgr servers rm <条目>`、`sshmgr servers import`、
     `sshmgr servers complete`；
   - 服务器主机密钥锚查看、覆盖与删除：`sshmgr servers pin-hostkey <条目>`、
     `sshmgr servers pin-hostkey <条目> --fingerprint <指纹> --force`、
     `sshmgr servers pin-hostkey <条目> --clear`；
   - Profile（服务器授权分组）：`sshmgr profiles ls`、`sshmgr profiles add
     <名>`、`sshmgr profiles grant <Profile> <条目...>`、`sshmgr profiles
     revoke <Profile> <条目...>`、`sshmgr profiles remove <名>`；
   - 项目令牌：`sshmgr projects ls`、`sshmgr projects show <项目>`、
     `sshmgr projects add <项目> --profile <Profile>`、`sshmgr projects
     rotate <项目>`、`sshmgr projects disable <项目>`、`sshmgr projects
     enable <项目>`、`sshmgr projects revoke <项目>`、`sshmgr projects
     remove <项目>`；
   - 设备码：`sshmgr cache-tokens ls`、`sshmgr cache-tokens add --name
     <设备名> --profile <Profile>`、`sshmgr cache-tokens revoke <设备名>`；
   - 配对：`sshmgr serve pair ls`、`sshmgr serve pair approve <名字或标识>
     --profile <Profile>`、`sshmgr serve pair reject <名字或标识>`；
   - 机密项：`sshmgr secrets list`、`sshmgr secrets add <名> --stdin`、
     `sshmgr secrets set <名> --stdin`、`sshmgr secrets edit <名>`、
     `sshmgr secrets allow --secrets <名...> --servers <条目...>`、
     `sshmgr secrets disallow --secrets <名...> --servers <条目...>`、
     `sshmgr secrets records <名>`、`sshmgr secrets rm <名>`；
   - 备份、设置、升级、审计：`sshmgr backup status`、`sshmgr backup create`、
     `sshmgr backup verify --latest`、`sshmgr backup config show`、
     `sshmgr backup config set`、`sshmgr settings ls`、
     `sshmgr settings get <设置项>`、`sshmgr settings set <设置项> <值>`、
     `sshmgr settings unset <设置项>`、`sshmgr update`、
     `sshmgr audit --source web_ui --limit 100`。
3. 在浏览器创建一个一次性 Profile（服务器授权分组），确认列表出现；再删除
   该一次性 Profile，并确认清理完成。
4. 打开 `/ui/ops/audit?source=web_ui`，核对第 3 步写操作；再执行
   `sshmgr audit --source web_ui --action profile.add --action profile.rm
   --limit 100`。
5. 在运行记录的基础页面对照表中登记：网络拓扑画布的布局坐标是图形界面
   能力对照的唯一例外，没有命令行等价面；本条不执行画布操作，画布与探测
   交互归 #274。
期望结果：
- 浏览器页面与命令行输出对同一权威 vault 数据给出相同条目名、条目数、状
  态与过滤结果；基础页面和既有功能的每项能力都有命令行等价面。
- 设备码列表只显示令牌前缀与安全字段，不显示完整令牌；机密项页面不显示
  值。配对列表与 `sshmgr serve pair ls` 对同一队列给出相同待处理行。
- 一次性 Profile 新增与删除产生 `profile.add`、`profile.rm` 来源为 `web_ui`
  的审计，清理后无残留。
- 除拓扑画布布局坐标外，没有发现其他图形界面专属且无命令行等价面的能力；
  该例外被明确登记。
证据留存方式：
- 留存每个页面路径与页面文本或截图、全部对照命令原文与完整输出、一次性
  Profile 写操作与清理证据、审计页面与命令输出、唯一例外登记。
是否允许自动化替代：
- 不允许。权威端真实页面、真实命令行入口与权威 vault 审计必须真机执行；
  配对批准本身另由 #275的人工判断条目覆盖。

### 2.5 Web UI 新功能页面（#274 填写）

<!-- 待 #274 补写：权威端组合的 Web UI 新功能页面条目 -->

### 2.6 初始化迁移与版本混跑（#275 填写）

<!-- 待 #275 补写：权威端组合的初始化、迁移与版本混跑条目 -->

## 3 客户端组合

### 3.1 机密项与 Windows 路径（#270 填写）

本小节通用取证：

- 每条执行后保存 MCP 工具返回或命令输出完整原文；有 `masked` 字段时一并
  保存，没有该字段时在运行记录写「无 `masked` 字段」。
- 审计分两侧取证：公司 A 的权威端执行 `sshmgr audit --action <动作名>
  --limit 0`；家 A Windows 新版客户端执行 `sshmgr cache audit --instance
  <一次性实例> --limit 0`。同一证据需要多个动作时重复 `--action` 参数。
- 收集客户端 sshmgr 进程标准错误与权威端 `serve.log`。按 H-SEC-01 检索工
  具返回、两侧审计输出与两侧本地日志，全部证据都必须未命中测试值。
- 本小节创建的一次性服务器条目、一次性实例、一次性机密项、授权与远端一
  次性文件都是靶子。除同一靶子明确供后续条目继续使用外，每条取证完成后
  必须清理；清理命令、完整输出与清理后的记录查询结果一并留档，失败不得
  记为完成。

#### C3-01 客户端快照可见列表与只读边界

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 权威端已按 A2-02 准备 profile 内与 profile 外的一次性服务器条目，以及只
  与 profile 内条目有交集的一次性机密项。
- 依据：#25 结论；#28 结论第 1、11 条。
执行步骤：
- owner 在客户端执行 `sshmgr cache pull --instance <一次性实例>`。
- agent 调用 `list_secrets`，再分别用 profile 内与 profile 外的服务器条目
  作为 `server` 参数调用。
- owner 在客户端执行 `sshmgr secrets list`，并尝试执行
  `sshmgr secrets add <一次性名字> --stdin`。
期望结果：
- `list_secrets` 与 `sshmgr secrets list` 都只显示快照内机密项的安全字
  段；profile 外条目筛选返回「没有可用的机密项」。
- 客户端写入口被拒绝，报「本机是缓存客户端：机密项在权威端维护，客户端
  只能执行只读的 secrets list——请在权威端执行这条命令」。
- 客户端本地审计与日志只有列表、拉取或拒绝摘要，不含测试值。
证据留存方式：
- 保存 `cache pull`、三次 `list_secrets`、两次 owner 命令的完整输出，以
  及权威端和客户端审计查询输出、本地日志检索结论、`masked` 名单或「无
  `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；真实设备码、加密快照、客户端桥进程与客户端命令行入
  口必须真机执行。

#### C3-02 客户端占位符填值与连接前整体拒绝

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 客户端已拉取含两个一次性机密项的快照；两个机密项都授权给一台指向
  Linux 受管服务器 A 的一次性服务器条目。
- 另有一台授予该 profile、但未列入这两个机密项可写入服务器列表的一次性
  服务器条目，以及一台故意不可达的一次性服务器条目。
- 依据：规格 #223；#24、#25、#26 结论。
执行步骤：
- agent 调用 `upload_content`，`encoding=text`，模板同时引用两个机密项并
  重复其中一个，写入一次性 Linux 路径。
- agent 以故意不可达的一次性服务器条目为目标，用包含两处格式错误与一
  个未授权引用的模板调用 `upload_content`。
- agent 用合法占位符写入未授权目标调用 `upload_content`。
- agent 在真实一次性目标上检查拒绝路径是否存在。
期望结果：
- 成功结果返回填值后的实际字节数；`filled_secret_names` 按首次出现顺序去
  重列出两个名字；`delivery_record_status="recorded"`，不返回文件哈希或值
  版本号。
- 格式错误与授权错误一次全部列出，包含行号、字节偏移、不含值的原文片段
  或目标服务器提示；拒绝时目标文件不存在，一个字节也不传给目标服务器。
- 格式错误调用的目标本来不可达；实际返回整体校验错误而不是连接错误，
  证明它在连接前拒绝。
- 客户端桥进程的返回、权威审计、客户端本地审计与本地日志都检索不到测试
  值。
证据留存方式：
- 保存三次工具返回、目标存在性检查输出、权威端 `sshmgr audit --action
  secret-delivery-record --limit 0` 输出、客户端 `sshmgr cache audit
  --instance <一次性实例> --limit 0` 输出、两侧本地日志检索结论，以及
  `masked` 名单或「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；客户端缓存取值、真实目标写入与连接前零传输必须真机
  实证。进程内假 SSH 服务器测试只能作为补充证据。

#### C3-03 客户端输出遮蔽当前值与上一个值

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- C3-02 已在一次性 Linux 路径写入含当前值的文件；owner 在权威端把该机密
  项换成新的测试值并让客户端拉取新快照，旧值仍保留在服务器文件中。
- 依据：#28 结论第 4 条；CONTEXT.md 的「输出遮蔽」定义。
执行步骤：
- agent 调用 `exec_command` 执行 `cat <一次性路径>`，观察旧值文件。
- agent 用同一占位符模板再次调用 `upload_content` 覆盖该路径，然后再次执
  行同样的 `cat` 命令。
期望结果：
- 两次 `stdout` 都只含 `{{sshmgr-secret:<机密项名>}}`；旧值与新值都不出
  现。
- 两次结果的 `masked` 字段都列出该机密项名；审计命令摘要只含 `cat` 与路
  径，本地日志检索不到旧值或新值。
证据留存方式：
- 保存两次 `exec_command` 返回、上传返回、`masked` 名单、权威端与客户端
  审计输出、本地日志检索结论。
是否允许自动化替代：
- 不允许自动化替代；换值后的真实客户端缓存与工具返回遮蔽必须真机实证。

#### C3-04 客户端下发记录登记与替换

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 使用一台指向 Linux 受管服务器 A 的一次性服务器条目、一个一次性机密项
  与一个尚不存在的一次性路径。
- 依据：规格 #224；#29 结论；ADR 0015。
执行步骤：
- agent 第一次调用 `upload_content` 把含占位符的文件写到一次性路径。
- owner 在权威端换值并让客户端拉取新快照。
- agent 第二次调用 `upload_content` 覆盖同一路径。
期望结果：
- 两次结果均为成功写入，且 `delivery_record_status="recorded"`；第二次结
  果仍只返回填入名字列表、字节数、登记状态与实际存在的连接说明，不返回
  文件哈希或值版本号。
- 权威端同一路径只保留最新下发记录；替换的旧记录进入审计，登记与替换审
  计都不含值。
- 客户端本地审计只做镜像补记，不是第二事实源；其中的名字、路径与状态也
  不含值。
证据留存方式：
- 保存两次上传返回、`sshmgr audit --action secret-delivery-record
  --action secret-delivery-replace --limit 0` 输出、客户端
  `sshmgr cache audit --instance <一次性实例> --limit 0` 输出、两侧本地日
  志检索结论，以及 `masked` 名单或「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；客户端写文件、经设备码登记权威端与同路径替换必须真
  机执行。

#### C3-W1 Windows PowerShell 读帧与进程命令行检查

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- owner 创建一个一次性机密项并设置环境变量名 `WIN_PS_TOKEN`，授权给指向
  Windows 受管服务器 A（PowerShell 条目）的一次性服务器条目。
- 测试值满足 H-SEC-01 的组成与长度要求，且值不入证据。
- 依据：票 #87 既有 W1 判据；#55 结论第 1 条。
执行步骤：
- agent 调用 `exec_command` 执行：
  `Write-Output $env:WIN_PS_TOKEN`
- agent 再调用 `exec_command` 执行：
  `$process = Get-CimInstance Win32_Process -Filter "ProcessId=$PID"; $process.CommandLine`
期望结果：
- 第一步输出只含 `{{sshmgr-secret:<机密项名>}}`，没有未注入警告。
- 第二步输出包含 PowerShell 进程命令行，但命令行参数里没有测试值；
  `masked` 名单没有因命令行泄漏而出现该机密项名。
证据留存方式：
- 保存两次工具返回、`masked` 名单、权威端与客户端审计输出、本地日志检索
  结论；owner 按 H-SEC-01 对照测试值，但不把值写入证据。
是否允许自动化替代：
- 不允许自动化替代；PowerShell 读帧与真实进程命令行必须真机执行。

#### C3-W2 Windows cmd 读帧与进程命令行检查

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- owner 创建另一个一次性机密项并设置环境变量名 `WIN_CMD_TOKEN`，授权给
  指向 Windows 受管服务器 A（cmd 条目）的一次性服务器条目。
- 测试值满足 H-SEC-01 的组成与长度要求，且值不入证据。
- 依据：票 #87 既有 W2 判据；#55 结论第 1 条。
执行步骤：
- agent 调用 `exec_command` 执行：
  `echo %WIN_CMD_TOKEN%`
- agent 再调用 `exec_command` 执行：
  `powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter 'name=''cmd.exe''' | Select-Object ProcessId,CommandLine"`
期望结果：
- 第一步输出只含 `{{sshmgr-secret:<机密项名>}}`，没有未注入警告。
- 第二步输出包含 cmd 进程命令行，但命令行参数里没有测试值；
  `masked` 名单没有因命令行泄漏而出现该机密项名。
证据留存方式：
- 保存两次工具返回、`masked` 名单、权威端与客户端审计输出、本地日志检索
  结论；owner 按 H-SEC-01 对照测试值，但不把值写入证据。
是否允许自动化替代：
- 不允许自动化替代；cmd 读帧与真实进程命令行必须真机执行。

#### C3-PATH-01 Windows 反斜杠路径上传复设

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 执行前确认 #47 的修复合入候选版本；记录候选版本号与修复提交号。
- owner 创建一个一次性机密项，授权给指向 Windows 受管服务器 A（PowerShell
  条目）的一次性服务器条目。
- 准备只有文件名不同的两个一次性绝对路径：一个使用反斜杠分隔符传入，一
  个使用正斜杠分隔符传入。
- 依据：#47；规格 #224 的路径规范化决策。
执行步骤：
- agent 调用 `upload_content`，`remote_path` 使用反斜杠绝对路径，内容为含
  占位符的非空模板。
- agent 调用 `exec_command` 用 `Test-Path -LiteralPath <反斜杠路径>` 与
  `Get-Item -LiteralPath <反斜杠路径> | Select-Object Length` 检查文件。
- agent 用同样大小的正斜杠路径与同样模板再调用一次 `upload_content`，并
  同样检查文件。
- owner 在权威端执行 `sshmgr secrets records <一次性机密项名>` 查看成功
  写入产生的下发记录。
期望结果：
- 反斜杠路径不得再出现「返回成功且 `bytes:0`，但文件不存在」的静默结果；
  只允许正确写入或明确报错。
- 正斜杠对照路径必须写入成功、文件存在且长度大于 0；若反斜杠路径选择正
  确写入，它的文件长度与返回字节数必须和对照路径一致。
- 成功写入的 Windows 路径进入下发记录时规范化为正斜杠形式，并记录最终绝
  对路径；若反斜杠路径明确报错，它不得留下半写文件，正斜杠记录仍必须规
  范化。记录详情不含机密项值。
证据留存方式：
- 保存候选版本与修复提交号、两次上传返回、两次路径与长度检查输出、下发
  记录输出、权威端与客户端审计输出、本地日志检索结论，以及 `masked` 名
  单或「无 `masked` 字段」记录。
- 清理两个远端一次性文件，并删除一次性服务器条目使测试记录随删；清理输
  出原样留档。
是否允许自动化替代：
- 不允许自动化替代；#47 必须在真实 Windows 受管服务器上复设，不能用进程
  内假 SSH 服务器替代。

### 3.2 网络拓扑与主机信任（#271 填写）

#### 客户端271-1 客户端拓扑查询、连通探测与本地结果边界

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 依据 #35、#36、#44、#67、规格 #128、规格 #142 与规格 #234，公司 A 的
  权威端已登记公司 A 权威端设备、家 A Windows 新版客户端（工作机 1）设备、
  Linux 受管服务器 A 与 NAS A；客户端设备与目标设备的共同网络、无共同网络
  目标和未登记服务器都存在。
- 新客户端的一次性命名实例用一次性设备码绑定访问配置组并执行
  `sshmgr cache pull`；快照按授权范围裁剪拓扑，客户端实例自己的设备随
  快照下发。
执行步骤：
1. 在客户端调用 `query_topology`，参数 `from="self"` 且不传 `to`，核对分组
   视图；再分别调用带 `to` 的默认档和 `detail=true` 展开档。
2. 在客户端调用 `probe_connectivity`，参数 `from="self"`、`targets` 包含
   `NAS A:445`；再以 Linux 受管服务器 A 的编号为 `from`，探测它到
   `NAS A:445`。
3. 调用后立即再次执行 `query_topology from="self"`，核对最近探测结果和
   相对观察时间出现在对应网络行。
4. 查看客户端实例目录的 `cache-audit.log` 与 `probe-results.json` 摘要；
   再到权威端查询自身 `query_topology from="self"` 的同名目标，确认没有
   出现客户端本机刚写入的观察。
5. 在客户端执行 `sshmgr sites add <一次性站点>`，确认写命令被拒绝并提示
   到权威端执行。
6. 验收结束后删除一次性命名实例目录，并在权威端删除一次性拓扑登记与
   服务器条目。
期望结果：
- 客户端查询只读本地裁剪快照和本实例探测结果；授权范围外的服务器不出现在
  任何分组，包括不可达与未登记分组。
- 默认输出紧凑分组；`detail=true` 只在服务器条目允许显示主机时展开地址，
  不受管设备地址始终显示为隐藏，输出不含网段。
- 两次探测都写客户端实例本地审计 `probe-connectivity`；观察带时间，只存在
  客户端本地 `probe-results.json`，不回传权威端。
- 客户端拓扑写命令被拒绝，查看命令可读缓存。
证据留存方式：
- 留存工具调用原文与完整输出、客户端本地审计摘要、本地探测结果摘要、
  权威端对照查询输出、写命令拒绝输出和清理输出；真实地址必须打码。
是否允许自动化替代：
- 不允许用自动化测试替代。客户端快照、客户端本地审计、权威端对照和本地探测结果
  文件的边界必须在新版 Windows 客户端真机上成立。

#### 客户端271-2 多地址试连与主机密钥不一致跳过

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 依据 #36、#44、#45、#67、规格 #132 与 ADR 0011，在新客户端可见的一次性
  目标设备上登记三个候选：物理局域网的第一地址指向一台呈现不同主机密钥的
  一次性靶机，虚拟局域网地址指向真正目标，服务器条目的兜底主机最后。
- 真正目标条目已锚定到正确主机密钥；错误靶机与真正目标使用相同端口，且
  agent 可经 SSH 工具面读取错误靶机的 SSH 服务日志。客户端已拉取包含该
  拓扑的快照。
执行步骤：
1. 调用 `exec_command`，在该一次性服务器条目上执行一个只读命令。
2. 读取返回结构中的 `connection_notes`，核对错误物理地址被说明为呈现不同
   主机密钥并已跳过。
3. 经 SSH 工具面查看错误靶机的 SSH 服务日志，核对该连接在认证请求前关闭，
   没有收到登录凭据。
4. 读取客户端本机 `probe-results.json` 的摘要，按观察时间整理实际尝试顺序；
   运行记录只写「物理局域网第 1 地址、虚拟局域网第 1 地址」这样的实际顺序，
   并另行记录规划中的兜底主机排在最后，不写真实地址。
5. 用 `query_topology from="self"` 查看该目标，确认查询只给登记分组和最近
   观察，不把被跳过地址伪装成连通。
6. 删除两个一次性靶机、服务器条目与拓扑登记；客户端缓存与本地探测结果
   随一次性命名实例目录一起删除。
期望结果：
- 命令最终经真正目标地址成功执行；返回中的连接说明只用网络名和序号指称
  被跳过地址。
- 错误靶机没有认证请求；被跳过地址不进入成功地址缓存，也不改写为成功观察。
- 本地探测结果能证明实际尝试顺序为物理局域网错误地址在前、可用地址在后。
证据留存方式：
- 留存工具调用原文与完整返回、错误靶机认证日志摘要、本地探测结果的网络名
  与顺序摘要、清理输出；真实地址、账号与凭据不得进入证据。
是否允许自动化替代：
- 不允许用自动化测试替代。真实客户端、错误靶机与正确靶机的网络路径和
  认证日志是本条判据；更细的函数级顺序断言由单机271-3代证。

#### 客户端271-3 首次锚定仅走虚拟局域网并转发

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 依据 #45、#50、#67、规格 #132 与 ADR 0011，权威端有一个未锚定的一次性
  条目，其设备同时登记物理局域网地址与虚拟局域网地址；新客户端的一次性
  命名实例两个地址都可达，且已拉取快照。
- 人工271-1的带外指纹事实已由 owner 准备好；权威端审计和服务日志可查询。
执行步骤：
1. agent 在新客户端调用 `exec_command`，对该未锚定条目执行只读命令。
2. 读取客户端本机 `probe-results.json` 摘要，核对本次只出现虚拟局域网候选
   的握手观察，没有物理局域网候选的认证尝试。
3. owner 按人工271-1比对权威端落库指纹；agent 留存权威端 `pin-forward`
   审计摘要，不代替人工判断。
4. 在同一客户端重复调用一次 `exec_command`，核对已有锚不会被转发请求覆盖，
   第二次连接可以按正常选址规则使用物理局域网地址。
5. 删除一次性服务器条目与设备拓扑登记；客户端缓存与本地探测结果随一次性
   命名实例目录一起删除。
期望结果：
- 首次连接只经虚拟局域网地址完成首次信任，并把真实握手地址随锚定转发提交
  权威端；物理地址不会在无锚状态下先被信任。
- 权威端仅新增锚，不覆盖已有锚；第二次连接物理局域网地址与虚拟局域网地址
  共用同一把锚。
证据留存方式：
- 留存两次工具调用与完整输出、客户端本地观察摘要、把真实地址替换为代称的
  权威端锚显示与 `pin-forward` 审计、owner 人工比对结论和清理输出；真实
  地址必须打码。
是否允许自动化替代：
- 不允许用自动化测试替代。真实客户端握手、权威端落库和第二次真实连接是
  本条判据；非法握手地址分支由权威端271-2的自动化测试代证。

### 3.3 共享存储（#272 填写）

<!-- 待 #272 补写：客户端组合的共享存储条目 -->

### 3.4 Web UI 基础、安全与等价面（#273 填写）

#### 客户端273-1 客户端回环监听、一次性启动令牌与只读入口

标注：适用组合=客户端；操作面=浏览器；证据来源=真机端到端
前置数据：
- 家 A Windows 新版客户端（工作机 1）的一次性实例已按 C3-01 拉取本地快照；
  本条只使用该一次性实例。
- 依据：#38 结论第 3 条；#39 结论第 1 条；#49 结论第 14 条；ADR 0010、
  0012、0013；CONTEXT.md 的「Web UI（管理界面）」。
执行步骤：
1. owner 在客户端执行 `sshmgr ui --instance <一次性实例>`，保存脱敏后的
   启动输出，并确认网址以 `http://127.0.0.1:<随机端口>/ui/start?token=`
   开头。
2. 浏览器打开后确认页面显示「客户端 · 只读」与「这里只读，修改需到权威端
   Web UI 去做」；保存 `/ui/` 页面文本、响应头与会话 cookie 属性。
3. 使用同一启动网址第二次访问，保存错误页。
4. 在候选发行版源码根目录执行：
   `go test ./internal/webui -run '^(TestHostValidationRejectsNonLoopbackHosts|TestWriteRouteRequiresMatchingOriginAndCSRFToken|TestCSRFTokenBoundToSession|TestAllWriteMethodsRequireOriginAndCSRFToken|TestSecurityHeadersOnEveryResponse|TestClientRoleShowsReadOnlyNoticeWithoutWriteEntries)$'`
5. 关闭 `sshmgr ui` 终端或发送终止信号，确认命令退出后浏览器无法连接。
期望结果：
- 客户端 Web UI 只监听本机回环地址的随机端口；启动令牌首次使用即换会话，
  第二次使用返回 403；命令退出即停。
- 会话 cookie 名包含端口，属性为 `HttpOnly`、`SameSite=Strict`、
  `Path=/ui/`，且不带 `Secure`。
- 页面响应带与单机273-1相同的内容安全策略与安全响应头；自动化测试证明
  非回环 Host、错误 Origin、缺失或错误防跨站请求伪造令牌被拒绝。
- 客户端首页没有编辑、删除、新增、添加等修改入口，并提示修改到权威端
  Web UI。
证据留存方式：
- 留存脱敏启动输出、页面文本、响应头与 cookie 属性、二次访问错误页、
  自动化测试完整输出、退出后浏览器失败截图。
是否允许自动化替代：
- 不允许用自动化替代客户端真实回环模式、令牌一次性、退出即停与只读入口；
  第 4 步测试只作为拒绝分支与只读页面契约的补充证据。

#### 客户端273-2 客户端本地快照、本机探测视图与命令行等价面

标注：适用组合=客户端；操作面=浏览器；证据来源=真机端到端
前置数据：
- 完成客户端273-1；一次性实例的本地快照内有服务器条目、机密项与网络拓扑
  数据，且至少有一个可用的一次性探测目标。
- 本条不写权威 vault；配对批准与实例删除不在本条执行。
- 设备码与配对页面不适用于客户端；本条按客户端本地快照与本机操作对照。
- 依据：#38 结论第 3 条；#39 结论第 8 条；#49 结论第 6 条；#55 结论第 4 条；
  ADR 0003、0010；CONTEXT.md 的「客户端」「探测结果」。
执行步骤：
1. 在浏览器打开 `/ui/`、`/ui/secrets/`、`/ui/ops/client`、
   `/ui/ops/client-audit` 与 `/ui/topology/`。
2. 对照命令行输出：
   - 服务器：默认实例用 `sshmgr servers ls`；命名实例逐条执行
     `sshmgr servers show <条目> --instance <一次性实例>`；
   - 机密项：`sshmgr secrets list --instance <一次性实例>`；
   - 连接状态与实例：`sshmgr cache status --instance <一次性实例>`、
     `sshmgr cache instances ls`；
   - 本机审计：`sshmgr cache audit --instance <一次性实例> --limit 100`；
   - 网络拓扑只读数据：`sshmgr sites list --instance <一次性实例>`、
     `sshmgr networks list --instance <一次性实例>`、`sshmgr devices list
     --instance <一次性实例>`。
3. 在客户端执行 `sshmgr devices probe --from self --instance <一次性实例>
   <一次性目标>`，再刷新 `/ui/topology/`，确认页面仍为只读、探测起点只能
   是本机，并能看到本机起点的探测结果摘要；真实地址先替换为网络名加序号。
4. 在浏览器分别访问 `/ui/access/`、`/ui/access/profiles/delete`、
   `/ui/secrets/new`、`/ui/servers/<一次性条目>/edit`、
   `/ui/servers/<一次性条目>/delete` 与 `/ui/ops/backup`；第一个路径应显示
   权威端管理提示，其余路径应返回 404。
5. 在客户端页面对同一一次性实例执行一次「立即同步」；对照
   `sshmgr cache pull --instance <一次性实例>`。实例切换的命令行等价面登记
   为后续命令显式加 `--instance <实例>`，或启动 Web UI 时使用
   `sshmgr ui --instance <实例>`。
期望结果：
- 客户端页面只显示本地快照内的服务器、机密项名字、备注、环境变量名与
  可写入服务器等安全字段、网络拓扑、连接状态与本机审计；显示内容与第 2
  步命令行输出一致，不显示机密项值。
- 页面不出现新增、编辑、删除、授权、备份配置等修改入口；访问写路由得到
  404，而不是客户端代写权威 vault。
- `/ui/access/` 提示「profile、配对、设备码和项目令牌在权威端管理。」；
   页头同时保留只读到权威端修改的指引。
- 本机探测视图可用且起点只能是本机；探测结果保存在客户端本地，权威端审计
   不新增 `probe-connectivity` 行。
- 立即同步失败时旧快照仍可查看；成功后页面与 `cache status` 的时间、修订
  号一致。
证据留存方式：
- 留存每个页面路径与页面文本或截图、全部命令原文与完整输出、脱敏探测结
  果、写路由 404 响应、权威端审计无探测记录的查询输出、同步前后状态。
是否允许自动化替代：
- 不允许。真实客户端快照、回环页面、本机探测与权威端无回传必须真机实证；
  进程内测试只能作为页面路由契约的补充证据。

### 3.5 Web UI 新功能页面（#274 填写）

<!-- 待 #274 补写：客户端组合的 Web UI 新功能页面条目 -->

### 3.6 初始化迁移与版本混跑（#275 填写）

<!-- 待 #275 补写：客户端组合的初始化、迁移与版本混跑条目 -->

## 4 新旧版本混跑组合

### 4.1 机密项与 Windows 路径（#270 填写）

本小节通用取证：

- 保存两侧工具返回与命令输出完整原文；有 `masked` 字段时一并保存，没有
  该字段时在运行记录写「无 `masked` 字段」。
- 新权威端用 `sshmgr audit --action <动作名> --limit 0` 取证；两台客户端
  实例分别用 `sshmgr cache audit --instance <一次性实例> --limit 0` 读取本
  地日志。
- 收集两侧 sshmgr 进程标准错误与权威端 `serve.log`。按 H-SEC-01 检索工具
  返回、审计输出与本地日志，全部证据都必须未命中测试值。
- 本小节的一次性服务器条目、客户端实例、机密项、授权与远端文件都必须在
  取证后清理；清理输出与清理后的记录查询结果一并留档。

#### M4-01 旧客户端遇新权威端：占位符字面量原样写出

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 公司 A 的权威端运行候选新版版本；工作机 1 上的旧版本第二实例使用未声
  明客户端版本的设备码拉取快照。
- 权威端有一个一次性机密项与一台指向 Linux 受管服务器 A 的一次性服务器
  条目。该机密项对新版客户端可用，但按版本声明规则从旧客户端快照中去掉。
- 依据：#30 结论；#51 结论；ADR 0014。
执行步骤：
- agent 通过旧版本第二实例调用 `upload_content`，把含
  `{{sshmgr-secret:<一次性机密项名>}}` 的非空模板写入一次性 Linux 路径。
- agent 通过同一实例调用 `exec_command` 执行 `cat <一次性路径>`。
- agent 在远端执行 `sha256sum <一次性路径>`；同时在本机对预期的占位符模
  板计算 SHA-256 哈希。
- owner 在新权威端执行 `sshmgr secrets records <一次性机密项名>`。
期望结果：
- 旧客户端原样写出占位符字面量，文件哈希等于预期模板哈希；文件中没有测
  试值。
- `cat` 返回仍是占位符字面量；新旧两侧工具返回、审计与本地日志均检索不
  到测试值。
- 新权威端不产生这次写入的下发记录；设备列表或等价命令将该客户端标为
  旧版客户端。
证据留存方式：
- 保存旧客户端版本号、上传与执行返回、两边 SHA-256 哈希、权威端记录查
  询输出、权威端与客户端审计输出、本地日志检索结论，以及 `masked` 名单
  或「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；必须使用真实旧版本二进制、真实新权威端与真实受管服
  务器。

#### M4-02 新客户端遇旧权威端：连接前整体拒绝

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 工作机 1 上的旧版本第二实例作为一次性旧权威端运行；家 A Windows 新版
  客户端从它拉取不含机密项与推荐版本字段的快照。
- 快照中有一台指向 Linux 受管服务器 A 的一次性服务器条目及其授权。
- 另有一台指向 Linux 受管服务器 A 的一次性观察条目，由候选新版工具面使
  用，只用于确认目标路径从未被创建。
- 依据：#30 结论；规格 #223；#51 结论；ADR 0014。
执行步骤：
- agent 在新版客户端调用 `list_secrets`。
- agent 调用 `upload_content`，内容同时包含一个合法占位符与一处格式错误
  占位符，目标为快照内的一次性 Linux 服务器条目。
- agent 确认一次性路径不存在。
期望结果：
- `list_secrets` 返回「没有可用的机密项」，不报传输错误。
- `upload_content` 在连接目标服务器之前整体拒绝，返回固定错误「权威端版
  本过旧，不支持机密项，请先升级权威端」；不得报成名字不存在，也不得只
  报格式错误。
- 目标路径不存在，一个字节也不传给目标服务器；工具返回、客户端本地审计
  与本地日志均不含测试值。
证据留存方式：
- 保存旧权威端版本号、两次工具返回、目标存在性检查输出、
  `sshmgr cache audit --instance <一次性实例> --limit 0` 输出、本地日志检
  索结论，以及 `masked` 名单或「无 `masked` 字段」记录。
是否允许自动化替代：
- 不允许自动化替代；新旧二进制、缓存快照与连接前拒绝必须真机混跑验证。

### 4.2 网络拓扑与主机信任（#271 填写）

#### 混跑271-1 旧客户端、新权威端的兜底锚与旧格式转发拒绝

标注：操作面=owner 命令行；证据来源=真机端到端
前置数据：
- 依据 #45、#67、规格 #132 与 ADR 0011，工作机 1 上的旧版本第二实例使用
  一次性缓存目录和一次性设备码，仅授予两个一次性条目：条目 A 已按正确主机
  密钥锚定且兜底主机可达；条目 B 未锚定、兜底主机可达。
- 新权威端为条目 A 的快照推导旧格式兜底锚；两个条目的设备拓扑存在，但旧
  客户端版本不支持读取网络拓扑。
执行步骤：
1. 在旧版本第二实例执行 `sshmgr cache pull`，再执行
   `sshmgr ssh <条目A> <只读命令>`。
2. 在旧版本第二实例执行 `sshmgr ssh <条目B> <只读命令>`，触发旧格式首次
   锚定转发请求。
3. 查看新权威端 `serve` 服务日志中 `/pin-hostkey` 的状态行，并查询条目 B
   的锚与权威端审计。
4. 用源码测试补充旧格式请求契约：
   `go test ./internal/mcpserver -run '^TestPinHostkey_OldFormatRejected400$'`
5. 清理旧版本第二实例的一次性缓存、设备码、两个服务器条目和相关拓扑登记。
期望结果：
- 条目 A 照常只用兜底主机连接，导入的旧格式兜底锚校验通过；它得不到多地址
  选址能力，但也不能因新快照而失败。
- 条目 B 的旧格式转发请求缺少服务器条目标识，被新权威端以状态码 400 拒绝，
  并提示升级客户端；权威端没有为条目 B 落锚。
- 拒绝发生在发送登录凭据之前；服务日志和自动化测试均证明旧格式请求不会被
  猜测归属到某个条目。
证据留存方式：
- 留存旧客户端两条命令原文与完整输出、权威端服务日志状态行、条目锚显示、
  审计查询、自动化测试完整输出和清理输出；真实地址与账号必须打码。
是否允许自动化替代：
- 不允许用自动化测试替代。旧客户端二进制、新权威端推导的旧格式锚和真实
  兜底主机路径必须真机实证；请求形状契约可由第 4 步测试代证。

#### 混跑271-2 新客户端遇旧权威端的拓扑入口降级

标注：操作面=agent 工具面；证据来源=真机端到端
前置数据：
- 依据 #44、#67、规格 #234 与 ADR 0014，家 A Windows 新版客户端（工作机 1）
  的一次性实例指向不支持网络拓扑的旧权威端，并已完成一次缓存拉取；该缓存
  快照没有推荐版本字段。
- 旧权威端中仍有一个可执行命令的授权服务器条目，用于确认降级只影响网络
  拓扑入口。
执行步骤：
1. 在新客户端调用 `query_topology`，参数 `from="self"`。
2. 在新客户端调用 `probe_connectivity`，参数 `from="self"`、`targets` 包含
   一个已授权目标。
3. 在新客户端执行 `sshmgr sites list` 与 `sshmgr devices list`。
4. 对仍授权的普通服务器条目调用 `exec_command` 执行只读命令。
期望结果：
- `query_topology` 与 `probe_connectivity` 均返回固定错误「权威端版本过旧，
  不支持网络拓扑与共享存储，请先升级权威端」，不把版本混跑误报成拓扑缺失。
- 客户端拓扑查看命令同样拒绝；普通命令执行能力不因拓扑入口降级而失败。
证据留存方式：
- 留存四个调用或命令的原文与完整输出、客户端和权威端版本号、一次性实例
  清理输出；真实地址与账号必须打码。
是否允许自动化替代：
- 不允许用自动化测试替代。新旧二进制的实际缓存格式、工具注册面和错误呈现
  必须在版本混跑真机上核对。

### 4.3 共享存储（#272 填写）

<!-- 待 #272 补写：新旧版本混跑组合的共享存储条目 -->

### 4.4 Web UI 基础、安全与等价面（#273 填写）

本组合无条目（#273 确认：基础 Web UI 按单机、权威端、客户端三种新版本角
色验收；旧版本界面的兼容提示与版本混跑行为归 #275 及第 4 部分其他条目。）

### 4.5 Web UI 新功能页面（#274 填写）

本组合无条目（#274 确认）

### 4.6 初始化迁移与版本混跑（#275 填写）

<!-- 待 #275 补写：新旧版本混跑组合的初始化、迁移与版本混跑条目 -->

## 发版检查清单

<!-- 待 #276 补写 -->
