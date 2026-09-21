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
