# ssh-manager-mcp

L2-broker SSH credential vault: 凭据只活在权威 broker 的加密 vault,一切远程操作(执行/转发/传输)经 broker 代理,agent 永远接触不到凭据。

## Language

### 传输

**Relay(中继)**:
服务器→服务器经 broker 转发的分块流式传输;源也可以是 broker 本机盘。文件字节只走 broker 内存,绝不进 agent 上下文,也绝不落 broker 盘。
_Avoid_: transfer(过泛,与 Transfer Task 撞)、copy、scp

**Chunk(块)**:
Relay 的固定大小传输与断点单位;每块独立校验(sha256),重跑只补缺失块。
_Avoid_: block、part、segment、分片

**Manifest(块清单)**:
落在**接收端**目标同目录的续传唯一事实源:源文件指纹(size+mtime)、各 Chunk 哈希与完成位。broker 任务丢失后靠它自愈续传;根哈希/全文件摘要完成时推导,不存于其中。
_Avoid_: checkpoint、resume file、断点文件

**Partial File(半成品文件)**:
传输进行中接收端以 `<target>.sshmgr-partial` 名义存在的目标文件;全部 Chunk 完成并根校验通过后才 rename 成真名。真名即"传完"的可见保证。
_Avoid_: temp file、临时文件

**Transfer Task(传输任务)**:
一次 Relay 在 broker 后台任务表中的条目(复用 Plan 32 任务模型,经 exec_output 轮询进度)。
_Avoid_: job、transfer(裸用)

**Upload(上传)**:
broker 本机盘 → 服务器的同步小文件通道(单文件 ≤1 MiB);大文件一律走 Relay。
_Avoid_: push(预留给批 2 的 CLI 命令名)

**Download(下载)**:
服务器 → agent 上下文的内容回传(1 MiB 前缀截断);大文件进 agent 上下文是有意抑制的反模式。
_Avoid_: fetch、read file

### 主机信任

**锚定(Pin)**:
首次信任一台目标服务器的主机密钥并将该密钥记录进权威 vault 的动作;此后连接若呈现不同密钥即拒绝(视为可能的中间人攻击)。锚的归属键是「主机:端口」,跟随服务器条目的地址而非条目本身——条目改地址后旧锚不随之迁移。
_Avoid_: trust(过泛)、register(与服务器录入撞)、verify(是校验不是记录)

**锚定转发(Pin Forwarding)**:
自动锚定路径:持有网络路径的缓存客户端与目标完成握手,新学到的主机密钥作为受审计的变更送权威 vault 落库。握手发生在有通路的一侧,落库发生在权威一侧。仅当该「主机:端口」尚无任何锚时成立,已有锚则拒绝;权威不可达时按只读语义直接失败,不排队、不留本地状态。
_Avoid_: write-back(实现视角)、mutation forwarding(过泛)

**带外锚定(Out-of-band Pinning)**:
手动锚定路径:owner 经带外渠道(设备屏幕显示、人工核对等)取得目标主机密钥指纹后,人工登记进权威 vault。不依赖登记机器与目标之间有任何网络连通。
_Avoid_: manual pin(不完整,未体现带外取指纹这一关键)、keyscan import(只是其中一种输入形态)

### 服务器元数据

**服务器元数据(Server Metadata)**:
服务器条目的六个描述性字段:Role(用途)/Services(部署了什么)/Location(在哪)/Hardware(硬件配置)/Caveats(注意事项)/Description(备注);各为 ≤4 KiB 自由文本,经 list_servers 全量给 agent 读。连接信息(地址/端口/用户/凭据)与标签(Tags)不属于它——三类东西的修改权限边界不同。
_Avoid_: 服务器信息(过泛,混入连接信息)、notes(只是其中一字段)

**受审计转发写(Audited Forwarded Write)**:
客户端机器经设备码认证发起、权威 broker 在单个事务里落库并写审计行的远程写通道族;broker 不可达直接失败,不排队不留本地状态,本地只以窄写口镜像补记(镜像不是第二事实源)。现有两员:锚定转发(仅可新增)与元数据编辑(带 revision 乐观锁的部分更新)。
_Avoid_: write-back(实现视角)、远程修改(未体现「审计+事务」这两个护栏)

**元数据编辑(Metadata Edit)**:
受审计转发写的一员:客户端把服务器元数据的部分字段变更(字段缺席=保持原值,显式空串=清空)连同 expected_revision(乐观锁令牌,取自 list_servers)发给 broker;revision 不匹配回 409 并附六字段现值,客户端一跳内合并重试。owner 端 CLI/TUI 编辑与它共用同一把 revision 锁。
_Avoid_: 元数据同步(是写不是同步)、远程表单(实现形态)

### 备份

**备份(Backup)**:
权威 vault 全量快照封成的可移植文件;加密形态(.sme,口令加密)与 export 产物同格式,明文形态(JSON)仅限满足部署硬约束的环境。恢复统一走 import。
_Avoid_: 快照备份(与离线缓存快照混淆)、NAS 备份(只是一种部署形态,不是独立概念)

**备份配置(Backup Config)**:
程序内备份的唯一事实源:备份目录、保留份数、口令文件路径三要素;调度时刻不属它管。
_Avoid_: 备份策略(时序归外部调度器,这个词会让配置文件的职责越界)

**口令文件(Passphrase File)**:
无人值守备份与恢复时读备份口令的文件;活在 vault 目录(与 master key 同级保护),必须有离机副本。
_Avoid_: 密钥文件(与 master.key.plain 混淆)

### 验收

**验收册(Acceptance Playbook)**:
仓库内可重跑的真机验收手册:每项由前置条件、步骤、取证要求、通过判据构成;按版本登记,证据回写兼容矩阵与欠账清单。
_Avoid_: checklist(过泛)、测试计划(与单元测试混淆)

**Agent 代跑**:
由 AI agent 经命令行或 SSH 工具面执行的验收,及其证据的来源标注——区别于 owner 人工执行。跳过人工信任动作必须走显式无人值守接口,并在证据中注明。
_Avoid_: 自动验收(掩盖「谁执行」这一来源信息)

**人工保留面**:
永远不由 agent 代跑的验收子集:人工信任动作(SAS 双屏比对)、感官判断(真终端观感)、物理操作(断网、断电)。
_Avoid_: 手工测试(过泛,不含「永久保留给 owner」的承诺)
