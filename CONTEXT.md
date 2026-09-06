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
落在**接收端**目标同目录的续传唯一事实源:源文件指纹(size+mtime)、各 Chunk 哈希与完成位、根哈希。broker 任务丢失后靠它自愈续传。
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
