# Linux 采集夹具（固定命令集 linux-v1）

由主会话在 2026-11-09 用 sshmgr 在真实服务器上运行固定命令集原文录得；在本仓库里，命令集原文存放在 `internal/softwareinventory/linux-v1.sh`，Go 构建时以文件嵌入方式随二进制发布。处理只有三项：去掉 `ss` 输出行尾的空白（命令集已同步加上同样的 `sed`），把内网地址换成文档地址（100.64.0.77、10.0.0.47），把家目录用户名换成 `user`。其余为原样输出。

| 文件 | 来源 | 用途 |
|---|---|---|
| `ubuntu2404-full.txt` | Ubuntu 24.04，root 用户，有 docker 与 NVIDIA 驱动 | 完整夹具：监听端口带进程名与进程编号，可执行文件可反查 dpkg 包（也有反查不到的，例如 conda 里的 python） |
| `ubuntu2204-no-process-names.txt` | Ubuntu 22.04，普通用户 | 权限不足：大部分监听行没有 `users:(...)`，只能看到自己进程 |
| `ubuntu2404-truncated.txt` | 由完整夹具在系统包一节中途截断 | 某节被截断：没有该节的 STATUS 行，也没有 END 行 |

输出协议：
- `SSHMGR-INVENTORY BEGIN <命令集版本>` 开头，`SSHMGR-INVENTORY END` 结尾。
- 每节以 `SSHMGR-INVENTORY SECTION <节名>` 开头，以 `SSHMGR-INVENTORY STATUS <节名> <退出码>` 结束；工具不存在时整节只有一行 `SSHMGR-INVENTORY ABSENT <节名>`（表示「没有」，不算不完整）。
- 节名：os、packages、listen、exe（进程编号<TAB>可执行文件）、exe-owner（可执行文件<TAB>所属 dpkg 包，空表示反查不到）、container-images（仓库<TAB>标签<TAB>摘要<TAB>镜像编号）、containers、firmware（键<TAB>值）、gpu-driver。
- 缺 STATUS 行、退出码非 0、或缺 END 行的节，标为不完整。
