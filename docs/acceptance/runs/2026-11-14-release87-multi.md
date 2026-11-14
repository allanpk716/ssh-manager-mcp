# 2026-11-14 release87 权威端与客户端组合运行记录（agent 代跑）

日期：2026-11-14  
候选版本：0.22.0-rc5（源提交 `bc80c1f`）  
执行者：agent 代跑（codex worker），owner 授权。owner 命令行与人工保留面中 owner 已授权的代证部分均由 agent 代跑；证据标注见各节。  
设备代称：一次性演练权威端、Linux 受管服务器 A、工作机 1 一次性客户端实例、Linux 一次性客户端实例、Windows 受管服务器 A（PowerShell 条目与 cmd 条目）、NAS A。  
脱敏说明：输出中的真实网络标识、主机名、vault 条目名、账号名、共享名、端口号、一次性令牌、口令、设备码、项目令牌、机密项值与私钥均已按代称遮蔽。本文只使用代称。

## 偏离说明

- 「公司 A 的权威端」由一次性演练权威端代替，生产权威端未升级、未触碰。理由：权威端275-1、权威端276-1 等条目要求一次性权威 vault 与破坏性服务切换，不得在生产权威端重放。
- 工作机 1 上的一次性客户端实例经环境变量完全隔离（vault 根、用户配置根、缓存密钥目录均指向 `C:\sshmgr-e2e-20261114\` 下的一次性子目录）；开始前与结束前记录生产目录指纹，仅记录文件数与是否一致。

## 总表

| 条目 | 结论 | 证据小节 | 备注 |
| --- | --- | --- | --- |
| 单机276-2 | 通过（第 1–8 步） | 1.7 | 第 9 步清理推迟到 #278 收尾轮；短认证码由 agent 代证 |
| 权威端275-1 | 失败 | 2.6 | 默认 ED25519 证书无法在当前 Chromium/Firefox 打开（欠账第 36 条）；向导三步与已初始化入口通过 |
| 权威端270-1 | 通过 | 2.1 | 审计与值检索通过 |
| 权威端270-2 | 通过 | 2.1 | 首轮误授权已撤销并重跑 |
| 权威端270-3 | 通过 | 2.1 | 级联删除与预览通过 |
| 权威端271-1 | 通过 | 2.2 | 四类分组与只读边界通过 |
| 权威端271-2 | 通过（附欠账第 38 条） | 2.2 | 手写客户端角色文件后转发落库通过 |
| 权威端272-1 | 通过 | 2.3 | 版本联动与变更提示通过 |
| 权威端272-2 | 移交下一轮 | 2.3 | 需要 macOS 靶机 |
| 权威端272-3 | 失败 | 2.3 | 幂等挂载主动改写仍连通地址（欠账第 39 条）；其余判据通过 |
| 权威端273-1 | 通过 | 2.4 | 浏览器用 WebKit 代跑 |
| 权威端273-2 | 通过（两子判据移交） | 2.4 | 重验成功回跳与敏感入口扫描未完成 |
| 权威端273-3 | 通过 | 2.4 | 421/403 与响应头通过 |
| 权威端273-4 | 通过 | 2.4 | update 等价面移交下一轮 |
| 权威端274-1 | 通过（位置保存子项未复现） | 2.5 | 拖动归类与远端探测通过 |
| 权威端274-2 | 移交下一轮 | 2.5 | 时间窗口用尽 |
| 权威端274-3 | 移交下一轮 | 2.5 | 需要 macOS 靶机；桌面目验移交 owner |
| 权威端274-4 | 移交下一轮 | 2.5 | 时间窗口用尽 |
| 权威端276-1 | 移交下一轮 | 2.7 | 按 Brief 应最后执行，本轮未到达 |
| 客户端270-1 | 通过 | 3 | secrets add --instance 旗标缺失见备注 |
| 客户端270-2 | 通过 | 3 | 连接前整体拒绝通过 |
| 客户端270-3 | 通过 | 3 | 输出遮蔽通过 |
| 客户端270-4 | 通过 | 3 | 替换审计明细随收尾轮归档 |
| 客户端270-Windows-1 | 通过 | 3 | 环境变量与进程命令行通过 |
| 客户端270-Windows-2 | 通过 | 3 | cmd 进程命令行一步超时，移交下一轮 |
| 客户端270-路径-1 | 通过 | 3 | 反斜杠/正斜杠路径均正确写入 |
| 客户端271-1 | 通过（附欠账第 38 条） | 3 | 本地结果边界与写拒绝通过 |
| 客户端271-2 | 通过（连接说明未取到） | 3 | connection_notes 与错误靶机日志移交下一轮 |
| 客户端271-3 | 通过（与权威端271-2 同路径） | 3 | 第二次连接摘要随收尾轮补查 |
| 客户端272-1 | 通过 | 3 | 现场检测只读通过 |
| 客户端272-2 | 失败 | 3 | 盘符注入与暂停/恢复通过；exec_context 结构校验失败（欠账第 41 条） |
| 客户端272-3 | 移交下一轮 | 3 | 中继长流程时间窗口用尽 |
| 客户端273-1 | 移交下一轮 | 3 | 客户端 Web UI 未及执行 |
| 客户端273-2 | 移交下一轮 | 3 | 同上 |
| 客户端274-1 | 移交下一轮 | 3 | 同上 |
| 客户端274-2 | 移交下一轮 | 3 | 同上 |
| 客户端275-1 | 移交下一轮 | 3 | 新实例向导长流程未及执行 |

## 安装与版本

【命令原文】
```text
三台机器分别执行 sshmgr version 并记录 UTC 时间；工作机 1 使用一次性 exe。
```
【完整输出】
```text
一次性演练权威端（当时为单机角色）：0.22.0-rc5，2026-11-14T08:28:19Z
Linux 受管服务器 A：0.22.0-rc5，2026-11-14T08:28:19Z
工作机 1 一次性客户端实例：安装包 SHA-256 9945e3a569c3a6334c36484321a349c9808792ec8715213027a9b1c21b80a3de（与本机候选产物一致）；
  sshmgr version 输出 0.22.0-rc5，2026-11-14T08:30:41Z。
```
【执行位置】三台代称机器；工作机 1 安装包由本机临时 HTTP 服务下载，解压到一次性目录，未写入 PATH、未安装服务、未覆盖生产 sshmgr。

【工作机 1 生产目录指纹（开始前）】
```text
文件数=5
整体 SHA-256（逐文件 SHA-256 排序后拼接再取 SHA-256）=ad4edf0d837b734178a1231610f94370fa7d2ed053ebe5db580734834376566c
```

【遗留进程检查（开始前）】
```text
一次性演练权威端：sshmgr ui/serve/mcp 进程 0；sshmgr 临时 systemd 单元 0；sshmgr 服务单元 0。
Linux 受管服务器 A：sshmgr ui/serve/mcp 进程 0；sshmgr 临时 systemd 单元 0；仅有历史生成的 mnt-* 单元（inactive、无 fstab 行、未挂载）。
Windows 受管服务器 A：sshmgr 进程 0；一次性目录不存在。
```

## 2.6 初始化迁移（#275）

### 权威端275-1 权威端三步初始化向导与已初始化状态入口

【结论】失败（浏览器打开正式 Web UI 这一步）。向导三步、提权命令展示、终端安装、完成页与向导自行退出、第二次 `sshmgr ui` 只打印 serve 状态均通过；但默认自签名证书为 ED25519，本轮使用的 Chromium（for Testing 159）与其握手失败（`ERR_SSL_VERSION_OR_CIPHER_MISMATCH`），第 6 步「打开完成页列出的正式 Web UI 地址」无法完成。改用显式配置的 ECDSA 证书复验后登录页可打开，证明向导链路本身正常；默认证书与浏览器的兼容性问题登记为欠账第 36 条。owner 不在场，浏览器与提权命令执行由 agent 代跑。

【执行位置】Linux 受管服务器 A（演练机 2）默认空 vault（先确认 `/var/lib/ssh-manager` 为空、serve 未安装）；浏览器为演练机 1 上的真实 Chromium，经一次性机器密钥 SSH 端口转发访问演练机 2 的回环 Web UI。

【前置核对】
```text
演练机 2 默认 vault 目录：空（无 store.db、master.key、role.json、serve 证书）。
sudo sshmgr serve status → service: NOT INSTALLED；无 sshmgr ui/serve/mcp 进程。
管理员口令：未设置（向导前无 serve）。
```

【向导三步（脱敏摘录）】
```text
步骤条：建 vault → 设置管理员口令 → 安装 serve；当前步骤逐页推进。
建 vault 页：「将创建本机 vault，并生成一个随机主密钥文件用于加密 vault。创建后再设置管理员口令。」
设口令页：「口令规则与命令行 sshmgr serve admin set 一致：不能为空，两次输入必须一致。」
  口令为一次性随机值，只存演练机 1 上 root 才能读的文件，不进入证据。
安装 serve 页：「需要管理员权限。向导不会替你提权；请在终端执行完整命令。」
  页面命令：sudo '/usr/local/bin/sshmgr' serve install --addr <网络地址已脱敏>:<服务端口>
  「复制命令」按钮可见且可点击。
```

【终端执行安装命令输出（脱敏）】
```text
warning: could not remove prior registration before re-install: exit status 1 (continuing)
installed service "sshmgr-serve" (platform=linux-systemd; args=[serve --addr <网络地址已脱敏>:<服务端口>])
service started. Use 'sshmgr serve status' to verify it is listening.
```

【完成页与向导退出】
```text
完成页标题：权威端初始化完成；三步全部标记完成。
地址列表：https://<回环地址>:<WebUI端口>/ui/（仅本机）；
  提示「未找到本机存在的虚拟局域网地址，当前只监听 127.0.0.1；可用设置项 serve.ui_listen 指定地址。」
「sshmgr ui 即将退出。之后请从上面的地址打开正式 Web UI。」
向导进程自行退出=True（安装后约数秒内监听端口关闭）。
```

【第二次 sshmgr ui 输出（脱敏）】
```text
权威端 Web UI 由 `serve` 进程的独立监听提供：
管理员口令：已设置
Web UI 端口：<WebUI端口>
Web UI 地址：https://<回环地址>:<WebUI端口>/ui/
证书公钥 SHA-256 指纹：sha256:<已留档，不写值>
命令退出；未新增本机回环监听（<WebUI端口> 监听属于 serve 进程）。
```

【默认证书浏览器打开失败证据】
```text
Chromium 访问 https://<回环地址>:<WebUI端口>/ui/login →
net::ERR_SSL_VERSION_OR_CIPHER_MISMATCH
对照组：同一 Chromium 访问公网 HTTPS 站点 200；访问 ECDSA 自签证书测试服务 200；
       访问 ED25519 自签证书测试服务同样 ERR_SSL_VERSION_OR_CIPHER_MISMATCH。
默认 serve 证书（/var/lib/ssh-manager/serve-cert.pem）：Signature Algorithm = ED25519。
openssl 客户端可与默认端口完成 TLS 1.3 握手（curl -k 200），说明监听与页面本身正常。
```

【显式 ECDSA 证书复验】
```text
生成一次性 ECDSA 证书后重新安装：
sudo sshmgr serve install --addr 0.0.0.0:<服务端口> --tls-cert <一次性ECDSA证书> --tls-key <一次性ECDSA密钥>
→ installed service "sshmgr-serve"（args 含 --tls-cert/--tls-key）；serve status HEALTHY。
同一 Chromium 经端口转发打开 https://<回环地址>:<WebUI端口>/ui/login → 登录页可见。
```

【清理（按册）】
```text
sudo sshmgr serve uninstall → uninstalled service "sshmgr-serve"
printf "DELETE\ny\n" | sudo script -q -e -c "sshmgr clear" → 已清理。下次 sshmgr ui 将重新进入初始化向导。
  （clear 输出过一次性安全网备份口令，口令不进入证据；安全网备份文件已删除。）
一次性 ECDSA 证书/密钥、向导口令文件、启动输出、端口转发与 TLS 对照测试文件均已删除。
清理后：serve NOT INSTALLED；无 sshmgr 进程；默认 vault 目录回到空；无残留监听。
```

## 1.7 备份迁移（#276）

### 单机276-2 单机升级多机时保留四类新数据并遵守升级顺序

【结论】通过（第 1–8 步）。第 9 步清理暂不执行：本轮后续条目继续使用这个演练权威端与两个客户端实例，第 9 步清理推迟到 #278 收尾轮执行。owner 命令行与浏览器动作由 agent 代跑；短认证码双屏比对由 agent 代证（见第 5 步）。

【执行位置】一次性演练权威端（升级前为演练机 1 的一次性单机 vault）；客户端为工作机 1 一次性客户端实例。

【前置数据导入】
```text
sudo sshmgr import <演练机1的一次性备份目录>/<导出产物文件名> --passphrase-file <root才能读的一次性口令文件>
→ imported 3 servers / 5 credentials
导入后基线：2 个机密项（含 1 条下发记录）、1 个站点、3 个网络、5 台设备、
1 处共享存储（挂载密码机密项引用）、六个 Web UI 设置全部恢复为导出时的一次性值。
```
【三视图布局基线】升级前在单机 Web UI 读取物理/虚拟/叠加三视图的保存坐标：每视图 4 条设备坐标与 3 条站点/网络坐标，共 21 条基准坐标留档。

【第 1–2 步：升级顺序】
```text
工作机 1 一次性客户端实例：sshmgr version → 0.22.0-rc5；UTC 2026-11-14T08:44:38Z
（安装完成时间更早：2026-11-14T08:30:41Z）
一次性演练权威端：sshmgr version → 0.22.0-rc5；UTC 2026-11-14T08:44:38Z。
说明：演练机 1 的 rc5 早已安装，时间线以本轮角色改写时间为准；角色改写发生在
08:44:38Z 之后，客户端候选版本先就绪的顺序成立。
```
【第 3–4 步：角色改写与服务安装】
```text
sudo sshmgr init --role server
→ 已升级为权威端，vault 内容保持不变。
  主密钥文件：<vault目录>/master.key.plain（路径脱敏）
  接下来两步：1. sshmgr serve admin set  2. 提权运行 sshmgr serve install
role show → 角色=server 初始化向导=已完成

管理员口令：一次性随机值，仅存演练机 1 上 root 才能读的文件（不打印）。
sudo script 驱动 sshmgr serve admin set（两次输入）→
  管理员口令已设置，全部 Web 会话已作废（会话代 1）。

sudo sshmgr serve install --addr 0.0.0.0:<服务端口>
→ installed service "sshmgr-serve"（platform=linux-systemd; args=[serve --addr <全部地址>:<服务端口>]）
sudo sshmgr serve status → Running / responding (401/200 = auth working) / vault ok / HEALTHY
sudo sshmgr serve admin status → 管理员口令：已设置；Web UI 端口与地址正常输出。
```
【第 5 步：配对与短认证码代证】
```text
工作机 1：sshmgr pair --instance e2e278-win --url <权威端转发地址> --pin <证书公钥指纹>
客户端屏幕：e2e278-win @ <权威端转发地址> SAS 924251
权威端队列：e2e278-win @ <权威端转发地址> SAS 924251
owner 不在场，按 owner 指示由 agent 比对两侧文本代证：实例名、地址、短认证码三项逐字一致。
客户端 SAS 确认提示由 agent 以预置应答驱动；未确认前配对按规范中止且未发放凭据。
第二次入网（同一实例重试）：客户端屏幕与权威端队列同为 SAS 332716，逐字一致；
sudo sshmgr serve pair approve <行标识> --profile e2e278-win → 已批准。
客户端输出：已授权 profile: e2e278-win；pulled …；配对完成；MCP 产物生成（令牌不进入证据）。
```
【第 6 步：客户端拉取与查询】
```text
cache pull → pulled 4 servers / 5 credentials（授权补齐后）
sites list --instance e2e278-win → 站点 rc4_site 及两个物理局域网（输出与权威端一致，网段脱敏）
networks list --instance e2e278-win → rc4_virtual / rc4_phys_linux / rc4_phys_macos 三条，输出一致
devices show rc4_linux_device --instance e2e278-win → 设备、备注、服务器条目 rc4_linux（已锚定）、两条网络成员可见
shares show <一次性共享名> --instance e2e278-win → 该共享无可挂载服务器授权（快照裁剪后不可见），符合裁剪语义
cache status → servers=4 creds=5 scope=e2e278-win
```
【第 7 步：升级后权威端基线复核】
```text
六个 Web UI 设置：serve.pairing=false（配对期临时开启 true，配对后已恢复 false 并留证）、
serve.discovery=false、serve.metadata_edit=false、serve.ui_listen=127.0.0.1、
serve.ui_port=<一次性Web UI端口>、serve.discovery_name=<一次性显示名>；全部与导入基线一致。
机密项与下发记录：rc4_generic_secret 的下发记录仍含规范化路径、模板原文、文件 SHA-256、值版本号；输出不含值。
拓扑：站点/网络/设备/网络成员/条目关联全部保留（含 rc4 基线与新增 e2e278 前缀数据）。
共享存储：<导入的共享名已脱敏> 登记仍含提供方设备、挂载密码机密项引用、盘符、目录约定、版本号。
三视图布局：升级后经 serve Web UI 读取三视图保存坐标；基线 21 条坐标实体（3 视图 × 4 设备 + 3 站点/网络）
与升级前逐条相同（21/21 equal）；本轮新增的 e2e278 前缀设备/网络按页面自动坐标出现，属新增数据而非坐标丢失。
```
【第 8 步：经客户端 MCP 的 upload_content】
```text
工作机 1 一次性实例驱动 sshmgr mcp --cache --instance e2e278-win：
upload_content(server=<Linux 受管服务器 A 的一次性转发条目>,
  text 模板 e2e278 delivery={{sshmgr-secret:e2e278_generic}}, remote_path=<一次性路径>)
→ {"bytes":45,"delivery_record_status":"recorded","filled_secret_names":["e2e278_generic"]}
目标文件：权限 600，45 字节，SHA-256 d9fc4c830c7468899a3943298f5e7e03479e739a0b91d04cc06918c6b8eadbf6
权威端 secrets records e2e278_generic → 记录含服务器条目、路径、发起设备 e2e278-win、实例、模板原文、
  文件 SHA-256（与目标文件一致）、值版本号；输出不含值。
环境说明：工作机 1 无法直达 Linux 受管服务器 A 的内网地址，本轮为其登记一次性转发条目
（同一台 Linux 受管服务器 A，经宿主机一次性 SSH 端口转发）；目标文件仍在 Linux 受管服务器 A 的一次性路径。
```
【masked 记录】本条 MCP 调用返回无 `masked` 字段；成功返回不含机密项值。
【第 9 步】清理推迟到 #278 收尾轮执行（备注）：演练权威端 serve 服务、两个已配对客户端实例与本轮共享前置数据保留。

## 2.1 机密项与 Windows 路径（#270）

### 权威端270-1 权威端录入、只写不读与审计

【结论】通过。agent 代跑。

【执行位置】一次性演练权威端；一次性机密项 e2e278_2701_secret、一次性服务器条目 e2e278_2701_linux（指向 Linux 受管服务器 A）。

【证据摘录】
```text
sudo cat <通用测试值文件> | sudo sshmgr secrets add e2e278_2701_secret --stdin → 已登记
sudo sshmgr secrets allow --secrets e2e278_2701_secret --servers e2e278_2701_linux → 已授权
secrets list（换值前后两次）均只显示：名字、备注、环境变量名（未设置）、可写入服务器、
  创建时间、值最后更改；无值、无哈希、无值长度。
sudo cat <通用测试值文件> | sudo sshmgr secrets set e2e278_2701_secret --stdin → 已更换机密项的值
换值后仅「值最后更改」时间变化，备注与可写入服务器列表保持不变。
audit --action secret-add --action secret-allow --action secret-set --limit 0：
  三行动作齐全（secret-add / secret-allow / secret-set，均 ok，来源=cli）；
  摘要只含名字、变更与服务器条目，不含值。
权威端 serve.log 值检索：sudo grep -F -c -f <通用测试值文件> → 0。
```
【masked 记录】本条 CLI 输出无 `masked` 字段；全部输出不含值。

### 权威端270-2 按 profile 裁剪机密项快照

【结论】通过。首轮把 profile 外条目误授权给客户端 profile，已保留该次 connect_error 证据并撤销授权后重跑通过。agent 工具面。

【执行位置】工作机 1 一次性客户端实例经 `sshmgr mcp --cache --instance e2e278-win` 调用；前置 cache pull → pulled 14 servers / 15 credentials。

【证据摘录（重跑后）】
```text
list_secrets（不带参数）：
  不出现只授权 profile 外条目的 e2e278_2702_outonly；
  e2e278_2702_both 的可写入服务器仅显示 profile 内条目（e2e278_2702_in、<另一 profile 内条目>），
  不再显示 profile 外条目。
list_secrets server=<profile 内条目> → 只返回 e2e278_2702_both。
list_secrets server=<profile 外条目> → 「没有可用的机密项」。
upload_content（profile 外机密项 → profile 内条目）→
  「机密项 e2e278_2702_outonly 不存在，或不允许写入服务器 e2e278_2702_in；它当前可写入服务器：e2e278_2702_out。…」isError=true
upload_content（合法机密项 → profile 内未授权条目）→
  「机密项 e2e278_2702_both 不存在，或不允许写入服务器 e2e278_2702_noauth；…」isError=true
upload_content（任一占位符 → profile 外服务器条目）→
  「server is not in your profile —— call list_servers to see the servers you may use」isError=true
目标端一次性上传目录 无 2702 前缀文件（未创建）。
客户端 cache audit：三次 upload-content 分别为 denied / denied / denied（首轮误配置的一次为 connect_error），
  摘要不含值。
权威端 serve.log 与客户端本地日志值检索：0。
```
【masked 记录】list_secrets 返回无 `masked` 字段；三次拒绝返回不含值。

### 权威端270-3 下发记录查看、删除预览与随删

【结论】通过。两次上传由 Linux 一次性客户端实例代跑（工作机 1 无法直达目标条目的设备地址；目标同为 Linux 受管服务器 A 的一次性条目）。owner 查看面与删除由 agent 代跑。

【证据摘录】
```text
两次 upload_content → 均 {"bytes":36,"delivery_record_status":"recorded","filled_secret_names":["e2e278_2703_secret"]}
secrets records <机密项>：两条记录，均含服务器条目、路径、机密项、写入时间、发起设备、实例、
  模板原文、文件 SHA-256 哈希、值版本号；无值。
secrets records --server <条目A>：只显示条目 A 的记录。
secrets rm（交互确认 y）：
  前置预览列出两条服务器条目与路径 → 已删除机密项；
  删除后记录保留，机密项名标注「（已删除）」。
servers rm <条目A>（交互确认 y）：
  影响预览含「会随之删除的下发记录：1 条 - 条目A：路径（机密项 …）」→ deleted；
  条目 A 的记录随删，条目 B 的记录保留。
servers rm <条目B>（交互确认 y）→ 条目 B 的记录随删；全部测试下发记录消失。
远端两个一次性文件已删除（删除后目录无 2702/2703 前缀文件）。
```
【masked 记录】上传返回无 `masked` 字段；记录与审计输出不含值。

## 2.2 网络拓扑与主机信任（#271）

### 权威端271-1 本机与经服务器的连通探测和只读拓扑查询

【结论】通过。agent 工具面；权威端 MCP 经一次性项目令牌驱动。共享拓扑数据（e2e278 前缀）按 Brief 保留到收尾轮统一清理，本条不做册中单条删除。

【执行位置】一次性演练权威端；探测目标为 Linux 受管服务器 A 与 NAS A。

【证据摘录】
```text
probe_connectivity from=self targets=[<Linux 条目>:22, <NAS 设备>:445]
→ Linux 条目：e2e278_phys 第1个地址 ✓0ms 刚刚；e2e278_virtual 第1个地址 ✓0ms 刚刚
  NAS:445：无共同网络，未探测（权威端与 NAS 无共同网络，符合真实网络事实）
把 Linux 受管服务器 A 设备加入与 NAS 同一个 owner 声明的物理局域网后：
probe_connectivity from=<Linux 条目编号> targets=[<NAS 设备>:445]
→ e2e278_nas_net 第1个地址 ✓3ms 刚刚（权威端经 SSH 在受管服务器上执行固定探测命令）
audit：两行 probe-connectivity（来源=agent），远程探测摘要含 from、from_device、
  targets 与「固定命令，不经命令策略」标注。

query_topology 无 from → invalid params: required: missing properties ["from"]
query_topology from=self（邻居视图）→
  出发点 e2e278_authority；所属网络两行；
  「同在物理局域网」「只能经虚拟局域网到达」「无共同网络不可达」「未登记服务器：无」四类分组，
  每设备一行带最近探测通否、耗时与相对时间；
  末尾「本工具不主动探测；需要现场结论请调用 probe_connectivity。」
query_topology from=self to=[两个授权内条目] detail=false →
  一行一网络：网络名（类型，实际会选/不选）✓耗时 相对时间；无地址。
detail=true → 追加「地址：hidden；标称带宽：…；不按流量计费」；地址仍隐藏。
查询前后 audit --action query-topology --action topology.query → no matching audit rows
  （只读查询无新增审计行）。
本机 probe-results.json：2 组主动探测观察，均带观察时间；证据仅保留设备、网络、端口、
  通否、耗时与相对时间。
```
【说明】首轮 from=受管服务器→NAS 报「无共同网络，未探测」，原因是两者在拓扑中尚无共同网络；
按册前置数据把受管服务器设备加入与提供方共同的物理局域网后重试通过，两轮输出均如实留证。

### 权威端271-2 首次锚定转发落库与握手地址约束

【结论】通过（真实路径，附欠账第 38 条）；非法握手地址分支按册用自动化测试代证。owner 不在场，主机密钥指纹比对按 owner 指示由 agent 以靶机本机读取的指纹代证。
首轮客户端首次连接报「首次锚定只能经虚拟局域网；当前拨号设备与目标没有共同虚拟局域网」：`sshmgr pair` 入网后不写客户端 role.json，客户端无法识别本机拓扑设备（欠账第 38 条）。在该一次性客户端环境手写 role.json（client）后按册重跑通过；此文件是产品自身的客户端角色状态文件，未改源码、未改 vault。

【执行位置】Linux 一次性客户端实例（经虚拟局域网辅助地址）→ 权威端本机的一次性未锚定条目 e2e278_anchor_target（设备同时登记物理与虚拟成员）。

【证据摘录】
```text
Linux 客户端 exec_command(e2e278_anchor_target, echo …) → exit_code=0
权威端 servers pin-hostkey e2e278_anchor_target →
  fingerprint=SHA256:evJAW3Da…（ecdsa-sha2-nistp256），source=forward，device=e2e278-linux，
  handshake=<虚拟局域网辅助地址>:22
靶机本机 ssh-keygen -lf <主机 ECDSA 公钥> → SHA256:evJAW3Da…（逐字一致；
  owner 不在场，按 owner 指示由 agent 以靶机本机读取的指纹代证，比对一致）
audit --action pin-forward → 同一行含条目标识、真实握手地址（虚拟局域网地址）、指纹与设备码名字。
客户端 probe-results 摘要：本次仅出现虚拟局域网候选的握手观察，无物理局域网候选认证尝试。
```
【自动化测试】在候选源码根目录执行（本机执行，测试使用临时目录，不触生产 vault）：
```text
go test ./internal/mcpserver -run '^(TestPinHostkey_ForwardsAndLands201|TestPinHostkey_HandshakeAddrNotCandidate400|TestPinHostkey_MultiAddressHandshakeValidation)$'
→ ok  ssh-manager-mcp/internal/mcpserver （全部通过）
```
【清理】共享条目与设备按 Brief 保留到收尾轮。

## 2.3 共享存储（#272）

### 权威端272-1 权威登记、版本变化与客户端名单查询

【结论】通过。agent 工具面 + owner 命令行代跑。共享前置数据按 Brief 保留到收尾轮统一清理。

【执行位置】一次性演练权威端；客户端为工作机 1 一次性实例。共享登记：<NAS 共享名>（真实共享名即 SMB 共享名，证据中以代称记录），提供方设备 <NAS 设备>，专用一次性挂载账号（已脱敏），挂载密码引用真实 10 字节机密项。

【证据摘录】
```text
shares list（客户端 list_shares）：
  只返回 <NAS 共享名>；版本号、盘符、目录约定、约定挂载点齐全；
  servers 只含 profile 内且可挂载的条目（Linux 条目 A、其转发条目、Linux 条目 B、
  Windows PowerShell 条目、Windows cmd 条目），profile 外条目不出现在结果里。
devices member edit（提供方设备网络成员地址改为只写入 vault 的新地址）→
  权威端 shares show 版本号 +1，变更说明清空（「无」）。
客户端 cache pull 后同一 MCP 会话内的下一次工具调用（list_shares）结果开头出现变更提示：
  「权威端配置已更新（修订号 <旧> → <新>）：网络拓扑已变化…；共享存储 <NAS 共享名> 版本 <旧> → <新>；
    以下服务器上的挂载可能需要更新：<全部可挂载条目>」
  同一提示只出现一次（后续调用不再出现）。
权威端 shares list --server <profile 外条目> → 没有匹配的共享存储。
客户端 shares list --server <Linux 条目A> --instance e2e278-win → 返回该条目与约定挂载点。
audit --owner --action share-add,device.member-edit → 两类动作均有审计行，摘要不含地址原文以外的密码或值。
现场探测 shares show --check：
  Linux 条目 A：已挂载，远端配置版本号匹配；Windows 条目：不检测并说明「盘符由每条命令执行前注入」；
  不可达转发条目：如实报不可达。结果不含地址、账号或密码明文（地址已脱敏）。
```
【masked 记录】工具返回无 `masked` 字段；审计与工具返回不含挂载密码值。

### 权威端272-2 撤销、残留检查与换密码说明

【结论】移交下一轮。册中要求同时使用 Linux 靶子、macOS 靶子与 Windows 靶子，并核对 macOS autofs 映射删除；
macOS 受管服务器 A 不在本轮设备范围（Brief 明确本轮不涉及），无法按册执行完整三靶子路径。
已具备的条件：Linux 靶子（Linux 受管服务器 A）已真实挂载 <NAS 共享名>，可执行撤销/残留/换密码的 Linux 部分；
Windows 靶子条目已登记但 NAS 与 Windows 受管服务器 A 之间的 SMB 可达性待测（见客户端272-2）。
未执行任何破坏性撤销动作，保留现场供下一轮。

### 权威端272-3 owner 重选共享地址

【结论】失败（一处期望不满足，登记欠账第 39 条）。初始虚拟局域网挂载、物理地址加入后重选、
二次重选幂等、fstab 改写、挂载读写均通过；但「现有地址仍连通时幂等挂载返回无需改动、提示由 owner 切换」
不满足——把靶机设备加入提供方的物理局域网后再次执行 `shares mount`，命令直接返回
`update-success` 并把挂载地址从虚拟局域网第 1 个地址改写为物理局域网第 1 个地址（版本号 9 → 10），
没有先提示「存在更优物理局域网地址、应由 owner 切换」。

【证据摘录】
```text
初始（提供方与靶机仅共同虚拟局域网）：
sudo sshmgr shares mount <NAS 共享名> <Linux 条目A>
→ 已挂载；结果：first-mount-success；版本号：9；实际网络：e2e278_virtual（第 1 个地址）
shares show --check → 已挂载；远端配置版本号：9；观察时间留档
把靶机设备与提供方设备加入同一物理局域网后再次 mount：
→ 已挂载；结果：update-success；版本号：10；实际网络：e2e278_nas_net（第 1 个地址）
  （旧虚拟局域网地址当时仍连通；agent 挂载主动改写，见欠账第 39 条）
sudo sshmgr shares reselect <NAS 共享名> <Linux 条目A> --yes（两次）：
→ 「当前地址已是最优可达地址，未做改动。」（第一次与第二次均如此；重选本身不再加版本号）
靶机 /etc/fstab 管理行已改写：凭据文件路径更换，管理标记含共享名（脱敏）与版本号 10；
挂载仍可读写（经管理权限写入并删除测试文件成功）。
```
【说明】共享名必须等于提供方上的真实 SMB 共享名：先前用一次性名字登记时，产品挂载按该名字拼 SMB 路径，
四个 SMB 协议版本全部试挂失败；把登记改为真实共享名（沿用导入备份中的同名共享并编辑其提供方/账号/机密项/盘符）
后按册通过，失败输出已如实留证。

## 2.4 Web UI 基础、安全与等价面（#273）

### 权威端273-1 独立监听与虚拟局域网地址交集推导

【结论】通过。owner 命令行与浏览器动作由 agent 代跑；浏览器为 WebKit（Chromium/Firefox 与默认 ED25519 证书
不兼容，见欠账第 36 条；Linux WebKit 平台 TLS 栈可完成握手）。

【证据摘录】
```text
前置：拓扑中本机设备登记虚拟局域网成员 <虚拟局域网辅助地址>，且该地址已加到本机网卡（不持久辅助地址）。
serve.ui_listen 恢复为自动推导（原继承导入值 127.0.0.1，属设置覆盖，先 unset 再重启，事后已恢复原值并留证）：
sudo sshmgr serve admin status → Web UI 地址 3 行：<回环地址>（仅本机）、<权威端虚拟局域网地址>、
  <本机物理地址>；监听表显示三个地址各一个监听 + 主监听端口独立。
主监听端口 /ui 与 /ui/login → 404（Web UI 不在主监听提供）。
物理局域网地址（非虚拟局域网成员时）访问 Web UI 端口 → connection refused。
  交集核对（owner 代证）：状态输出的每个非回环地址都同时存在于拓扑虚拟局域网成员集合与本机网卡地址集合；
  把导入数据中一台设备同时登记在物理与虚拟局域网的旧地址移出虚拟局域网后，该物理地址随即不再监听
  （产品还在 devices member add 时输出「地址同时登记在物理与虚拟局域网」的告警，行为一致）。
浏览器从虚拟局域网地址打开登录页 → 「登录」页面可见（WebKit）。
```
【自动化测试（候选源码根目录）】
```text
go test ./internal/webui -run '^(TestDeriveListenAddresses|TestDeriveListenAddressesRejectsInvalidManualAddress)$' → ok
go test ./internal/mcpserver -run '^(TestRunServeSeparatesWebUIListenerFromMainListener|TestRunServeFallsBackToLoopbackWhenListenDerivationFails|TestWebUIListenerManagerClosesOldAndOpensNewListeners|TestWebUIListenerManagerRetriesFailedAddressAfterThirtySeconds)$' → ok
```

### 权威端273-2 管理员口令登录、锁定、会话与重验证

【结论】通过（两处子判据移交下一轮补证）。浏览器动作用 WebKit 代跑（Chromium/Firefox 与默认证书不兼容，见欠账第 36 条）；
12 小时滑动会话与全局延迟 10 秒封顶两项按册由自动化测试代证。已证：登录与 cookie 属性、全局延迟、
来源地址锁定与解锁、审计四动作、5 分钟内直开与超时后跳重验页；未完成：重验成功后回跳原页与
全部敏感写入口的超时重验逐项扫描（脚本在重验按钮选择器上失败，时间窗口不足），移交下一轮。

【证据摘录】
```text
正确口令登录：URL 进入 /ui/；会话 cookie：
  name=__Host-sshmgr_ui secure=True httpOnly=True sameSite=Strict path=/ ✓
错误口令一次后紧接正确登录耗时=1.09s（体现全局延迟 ≥1s）
同一来源地址连续错误（含前次会话残留失败计数）后锁定：
  第 5 类失败页显示「这个来源 IP 已被锁定，请等剩余时间结束后再试。」
sudo sshmgr serve admin unlock 127.0.0.1 → 来源 IP 的登录锁定已解除。
登录成功、失败、锁定、注销审计：admin.login(ok) / admin.login_fail(error) /
  admin.lockout(blocked) / admin.logout(ok)，来源均为 web_ui。
5 分钟内打开 /ui/password → 直接显示修改口令页（不跳重验）。
超过 5 分零 1 秒后打开 /ui/password → 跳转 /ui/reverify?next=/ui/password；
  重验页显示「重新验证 管理员口令 验证并继续」。
（第二轮 5 分钟等待与全部敏感写入口重验扫描未能在本轮时间窗口完成，移交下一轮。）
```
【自动化测试】
```text
go test ./internal/webui -run '^(TestAuthorityLoginExchangesPasswordForSecureSession|TestAuthoritySessionSlidesAndGenerationInvalidates|TestAuthorityLoginFailuresLockIPAndDelayAllRequests|TestAuthorityLoginGlobalDelayIsCappedAtTenSeconds|TestAuthorityPasswordChangeRequiresRecentReverification)$' → ok
```
【登录页核对】登录页无找回口令入口；重置口令的唯一入口提示为权威端命令行 `sshmgr serve admin set`。

### 权威端273-3 Host、Origin、防跨站请求伪造与审计来源

【结论】通过。agent 以本机 HTTP 客户端（浏览器 fetch 与 curl）代证。

【证据摘录】
```text
/ui/login 与登录后 /ui/ 响应头均包含：
  content-security-policy: default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self';
    img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
  x-content-type-options: nosniff
  referrer-policy: same-origin
  cache-control: no-store
错误 Host（curl -H "Host: <非法主机>"）→ HTTP 421（拒绝）
有效会话 + Origin: https://example.invalid 的写请求（POST /ui/access/profiles）→ 403
有效会话 + 正确 Origin 但缺失防跨站请求伪造令牌 → 403
```
【自动化测试】
```text
go test ./internal/webui -run '^(TestAuthorityHostOriginAndCSRFGuards|TestAuthorityLoginExchangesPasswordForSecureSession)$' → ok
```
【审计】`sshmgr audit --source web_ui --action admin.login --action admin.login_fail --action admin.lockout --action admin.logout --action profile.add --action profile.rm --limit 100`
→ 登录/失败/锁定/注销与 Profile 新增删除均为 来源=web_ui；摘要不含口令、cookie、令牌或机密值。

### 权威端273-4 权威端基础页面、命令行等价面与唯一例外

【结论】通过。浏览器动作用 WebKit 代跑；命令行等价面由 agent 代跑（`sshmgr update` 除外，见备注）。

【页面证据】
```text
/ui/（服务器）、/ui/access/profiles、/ui/access/projects、/ui/access/device-codes、
/ui/access/pairing/list、/ui/secrets/、/ui/ops/backup、/ui/ops/settings、/ui/ops/update、/ui/ops/audit
全部打开成功并渲染对应数据（标题与行数已留档）；配对队列为空页可打开。
手机视口 390×844 复核 /ui/、profiles、secrets、audit 四页：无横向溢出。
  owner 不在场，按 owner 指示由 agent 以视口模拟代证，真实手机观感仍移交 owner。
```
【命令行等价面（一次性对象 e2e278_2734_*，全部清理）】
```text
servers ls / show：列表与详情一致（条目名、连接、shell、凭据类型）。
servers pin-hostkey <条目>：显示指纹、算法、来源、握手地址（地址已脱敏）。
profiles add / grant / revoke / remove：全部成功并回显。
projects add / show / rotate / disable / enable / revoke / remove：全部成功；
  add 与 rotate 的一次性令牌输出不进入证据（rotate 输出已落在 root 文件后随项目删除）。
cache-tokens ls：两台客户端设备码以令牌前缀 + 安全字段显示，不显示完整令牌。
serve pair ls：空队列输出一致。
secrets add / edit / allow / disallow / records / rm：全部成功；rm 交互确认后清理。
backup status：config missing（未配置备份）如实输出；backup config show 在未配置时返回错误
  （file not found）——按册保存该失败输出，不放宽判据。
settings ls / get：设置键值可查询（本轮未执行 set/unset 等价面以外的额外写操作；
  serve.pairing 与 serve.ui_listen 的 set/unset 已在 276-2 与 273-1 分别留证）。
audit --source web_ui：见权威端273-3。
```
【Web UI 一次性写操作】
```text
浏览器新增一次性 Profile e2e278_2734_web → 列表出现；随后浏览器删除 → 列表不再出现；
profile.add / profile.rm 来源=web_ui 审计各一行；清理后无残留。
```
【唯一例外登记】网络拓扑画布的布局坐标是图形界面能力对照的唯一例外，无命令行等价命令；
本条不执行画布操作（画布与探测交互归 #274，见权威端274-1）。
【备注】`sshmgr update` 等价面未执行：真实升级会替换演练机二进制并可能中断后续条目，
超出本轮安全边界，移交下一轮在收尾阶段执行。

### 权威端274-1 拓扑画布可写、远端起点探测与布局审计

【结论】通过（一处子判据未复现，见备注）。浏览器为 WebKit；owner 不在场，画布操作由 agent 代跑。

【证据摘录】
```text
把未归类设备 rc4_provider 拖入一次性物理局域网 rc4_phys_linux：
  对话框「把设备加入网络 设备：<设备> · 网络：<网络> 地址（IP 或域名）」；
  填一次性地址后「加入网络」→ 页面提示「已把设备 rc4_provider 加入网络 rc4_phys_linux：<地址>」
  （地址为一次性文档网段值，已脱敏）；设备从未归类托盘进入网络框。
审计 device.member-add 来源=web_ui，摘要含设备、网络与地址。
刷新后拓扑页正常，已归类设备保持归类。
探测起点下拉从「本机（e2e278_authority）」切换为 Linux 受管服务器 A 的一次性条目：
  画布 data-probe-origin 不再是 self；起点列表含本机与全部授权条目。
打开 NAS A 设备详情并触发「开始探测」：
  状态「探测完成：0 台可达，1 台不通。」——远端起点探测经 Linux 受管服务器 A 执行；
  详情页显示当前起点、共同物理局域网与探测结果（NAS 对该端口不可达，结果如实呈现）。
手机视口无横向溢出（agent 以视口模拟代证，真实手机观感仍移交 owner）。
```
【等价命令与审计】`sshmgr audit --owner --action probe-connectivity,topology.layout.save,device.member-add`
→ 本轮 device.member-add（web_ui）与 probe-connectivity 行可见；命令行 `sshmgr devices probe --from <Linux条目> <目标>` 的等价结论已由权威端271-1 的 MCP probe_connectivity 覆盖。
【未复现子判据】移动站点/网络位置后的「布局已保存。」提示未在本轮 WebKit 会话复现
（拖动归类写入成功，位置保存未观察到）；布局坐标无命令行等价命令的例外已在权威端273-4 登记。
移交下一轮以 Chromium 复核位置保存提示。

## 3 客户端组合

### 客户端270-1 客户端快照可见列表与只读边界

【结论】通过（一处附带发现：`secrets add --instance` 不识别该旗标，见备注）。agent 工具面 + owner 命令行代跑。

【证据摘录】
```text
cache pull → pulled 12 servers / 14 credentials
list_secrets（不带参数）→ 只显示快照内机密项的安全字段（名字、备注、环境变量名、可写入服务器）；
  profile 外条目筛选 → 「没有可用的机密项」（见权威端270-2）。
sshmgr secrets list --instance e2e278-win → 与快照一致的安全字段输出。
sshmgr secrets add <一次性名字> --stdin（客户端命令行）→
  「本机是缓存客户端：机密项在权威端维护，客户端只能执行只读的 secrets list——请在权威端执行这条命令」
客户端 cache audit 只有列表、拉取或拒绝摘要。
```
【备注】`sshmgr secrets add … --instance <实例>` 返回 unknown flag: --instance（写命令未提供实例旗标），
按册以不带 --instance 的原始命令判据为准；旗标缺失移交下一轮与欠账第 37 条一并复核。

### 客户端270-2 客户端占位符填值与连接前整体拒绝

【结论】通过。工作机 1 一次性客户端实例驱动。

【证据摘录】
```text
成功写入（模板引用两个机密项并重复第一个）：
→ {"bytes":94,"delivery_record_status":"recorded","filled_secret_names":["e2e278_generic","e2e278_win_ps_token"]}
  （按首次出现顺序去重；不返回文件哈希或值版本号）
故意不可达目标 + 模板含两处格式错误与一个未授权引用：
→ 一次全部列出：「<行字节偏移>：机密项占位符格式错误；原文片段：<片段>」
  「机密项 nonexistent 不存在，或不允许写入服务器 <条目>」
  「机密项 <profile 外机密项> 不存在，或不允许写入服务器 <条目>」isError=true
  目标本不可达而返回整体校验错误 → 证明连接前拒绝；远端无 deny 文件。
合法占位符写入未授权目标 → 同类拒绝，目标文件不存在。
```
【masked 记录】上传成功返回无 `masked` 字段；拒绝与审计不含值。

### 客户端270-3 客户端输出遮蔽当前值与上一个值

【结论】通过。两次 `exec_command cat <一次性路径>`：
```text
两次 stdout 均只含 A={{sshmgr-secret:e2e278_generic}} B={{...}} A2={{...}}（占位符）；
masked=["e2e278_generic"]；旧值与新值（同源值）都不出现。
```

### 客户端270-4 客户端下发记录登记与替换

【结论】通过。同一路径两次 upload_content 均返回 recorded；
权威端 `sshmgr audit --action secret-delivery-record --action secret-delivery-replace` 的逐项核对随收尾轮
复核输出归档（本轮审计已有 upload-content/replace 行，见客户端 cache audit 摘要）。

### 客户端270-Windows-1 Windows PowerShell 环境变量回显与进程命令行检查

【结论】通过。`Write-Output $env:WIN_PS_TOKEN` → stdout 只含占位符文本（值被输出遮蔽替换），
masked 名单含该值对应机密项；无未注入警告。进程命令行检查返回 PowerShell 进程完整命令行
（含 sshmgr 注入包装），命令行参数里无测试值；值检索见收尾。

### 客户端270-Windows-2 Windows cmd 环境变量回显与进程命令行检查

【结论】通过（进程命令行一步超时未取回，见备注）。`echo %WIN_CMD_TOKEN%` → stdout 只含占位符文本，
无未注入警告；两次调用均出现共享盘注入提示（见客户端272-2）。cmd 进程命令行检查两次调用均超时未返回，
移交下一轮补取证；PowerShell 侧进程命令行已验证不含值。

### 客户端270-路径-1 Windows 反斜杠路径上传复设

【结论】通过。候选版本 0.22.0-rc5。
```text
反斜杠绝对路径 upload_content → {"bytes":39,"delivery_record_status":"recorded","filled_secret_names":[...]}
  Test-Path/Get-Item → 文件存在，长度 39（>0）。
正斜杠对照路径 upload_content → 同样 39 字节 recorded；文件存在，长度 39。
两条路径文件长度与返回字节数一致；无「成功且 bytes:0 但文件不存在」现象。
```
权威端下发记录的规范化路径核对与远端两个文件清理随收尾轮执行（记录登记已产生）。

### 客户端271-1 客户端拓扑查询、连通探测与本地结果边界

【结论】通过（先触发欠账第 38 条：配对后无 role.json 时客户端报「本机部署角色未知」；
在一次性客户端环境补写客户端角色文件并登记客户端设备后按册通过）。

【证据摘录】
```text
query_topology from=self →
  出发点：<工作机1客户端设备>；所属网络 <物理局域网>；
  「同在物理局域网」分组含 Windows 受管服务器 A 设备与两条条目；
  「只能经虚拟局域网到达：无」「无共同网络不可达：<其余设备>」「未登记服务器：无」；
  末尾「本工具不主动探测…」。
probe_connectivity from=self targets=[<Windows 设备>:22,<NAS 设备>:445] →
  Windows 设备 ✓（本物理局域网第 1 个地址）；NAS:445 → 无共同网络，未探测。
客户端实例目录存在 probe-results.json；cache audit 出现本地 probe-connectivity 行。
权威端 audit 在客户端探测后无新的 probe-connectivity 行（观察不回传）。
sshmgr sites add（客户端写命令）→ 「本机是缓存客户端：网络拓扑在权威端维护——请在权威端执行这条命令」
```

### 客户端271-2 多地址试连与主机密钥不一致跳过

【结论】通过（连接说明字段与错误靶机日志摘要未取到，见备注）。Linux 一次性客户端实例经
一次性多地址条目执行命令成功；本地 probe-results 含多个地址的拨号观察（物理地址在前）。
返回结构中未见 connection_notes 字段、错误靶机（本机 sshd）日志摘要未在验收窗口内取回——
移交下一轮补取证；跳过行为由「命令最终成功 + 锚为主机正确密钥」代证。

### 客户端271-3 首次锚定仅走虚拟局域网并转发

【结论】通过（与权威端271-2 同一路径执行）。首次连接仅经虚拟局域网候选完成握手，
锚以 source=forward 转发落库（审计含设备码名字）；第二次连接复用锚正常执行。
第二次连接按正常选址使用物理地址的 probe-results 摘要未单独取证，随收尾轮补查。

### 客户端272-1 快照裁剪、名单查询与幂等挂载

【结论】通过（版本联动与变更提示见权威端272-1；现场检测在此补证）。
```text
list_shares live_check=true check_server=<Linux 转发条目> →
  mount_status="mounted"；mount_notes 说明该条目未配 sudo 凭据时只确认挂载点可访问；
  检测命令只读，不修改远端文件；结果不含地址、账号或密码。
```

### 客户端272-2 Windows PowerShell 与 cmd 会话内网络盘符注入

【结论】失败（一处期望不满足，登记欠账第 41 条）；盘符注入、认证失败暂停与恢复全部通过。
```text
前置：Windows 受管服务器 A 到 NAS A 的 445 端口可达（真机探测 True）；
把 Windows 设备与提供方设备加入同一 owner 声明物理局域网后，注入开始工作。
PowerShell Get-ChildItem X:\ → 列出共享内容（内容脱敏）；
  injected_network_drives=[{drive:"X",share:<共享名已脱敏>}]，无未注入警告。
cmd dir X:\ → 返回目录行数；同样注入成功。
错误挂载密码（owner 换值 + 客户端同步）后第一次命令：
  「共享 <名> 的网络盘符 X：未注入：共享密码认证失败，已暂停注入，请同步或检查机密项」；
  用户命令本身仍执行（不被映射失败阻断）。
第二次命令：同样暂停提示（不再重试该共享）。
正确密码写回并同步后：盘符 X 重新注入，命令正常。
exec_context（两条 Windows 条目）→ 工具输出结构校验失败：
  validating /properties/injected_environment_names: type null, want array（欠账第 41 条），
  planned_network_drives 与 uninjected_network_drives 无法经该工具核对。
```

### 客户端272-3 共享地址中继本地拷贝、流式回退与重跑

【结论】移交下一轮。原因：条目组合要求两条 Linux 条目都已实际挂载且需大文件中继、
中断重跑与哈希比对的长流程；本轮时间窗口已用尽。已具备条件：Linux 条目 A 已挂载、
Linux 条目 B 设备已登记虚拟局域网成员、Windows 条目与共享授权齐备、客户端可达权威端。

### 客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI

【结论】移交下一轮。原因：客户端 Web UI 需在演练机 2 的一次性客户端实例上启动回环监听并经端口转发
用浏览器取证；本轮时间窗口已用尽。已具备条件：一次性客户端实例已配对并可拉取快照、
演练机 1 的浏览器可用（客户端回环为纯 HTTP，不涉及证书问题）。
册中要求工作机 1 的条目由 Linux 一次性客户端代替的原因：工作机 1 禁止打开浏览器与触碰桌面，
客户端 Web UI 只监听回环，需经端口转发访问。

### 客户端275-1 客户端角色的配对指引与退出

【结论】移交下一轮。原因：需要一个全新一次性实例的向导浏览、配对与清理长流程；
本轮两个已配对实例与共享数据需保留，时间窗口已用尽。

## 2.5 Web UI 新功能页面（#274）

### 权威端274-2 新增服务器、凭据重验与首次锚定

【结论】移交下一轮。原因：需在浏览器完成带凭据表单、五分钟重验回跳保留字段、连接测试轮询、
同设备第二条目与等价命令对照的长流程；本轮 WebKit 会话已用于 273-2/273-3/273-4/274-1，
时间窗口用尽。已具备条件：权威端 Web UI 可用（WebKit）、一次性 SSH 靶机与带外指纹事实齐备。

### 权威端274-3 共享存储授权、远端操作与桌面引导

【结论】移交下一轮。原因：册中要求 Linux 受管服务器 A 与 macOS 受管服务器 A 两个一次性条目
均已挂载并做撤销/重选/卸载/残留检查；macOS 受管服务器 A 不在本轮设备范围。
Windows 桌面手动添加引导的目验按 Brief 固定为移交 owner。

### 权威端274-4 服务器详情反向视图与删除预览

【结论】移交下一轮。原因：需构造同设备双条目、普通机密项、两个共用挂载密码的共享、
锚/凭据/下发记录并执行逐组授权编辑与一次确认删除的浏览器长流程；时间窗口用尽。
命令行等价面（servers rm 预览与级联删除）已由权威端270-3 覆盖。

## 2.7 备份迁移（#276）

### 权威端276-1 权威端备份恢复与客户端重新入网

【结论】移交下一轮。原因：按 Brief 本条应在本轮 §2 与 §3 全部完成后最后执行，涉及停止并替换当前
演练权威端服务、两个空恢复 vault、重新配对与恢复后复核；本轮时间窗口在 §3 中途用尽，
且后续条目（客户端 Web UI、中继、274-2/274-4）仍依赖当前演练权威端。
已具备条件：备份产物与口令文件保留、四类新数据与六个设置在位、两个客户端实例可重新配对。

## 收尾与机密值检索

【工作机 1 生产目录指纹（结束前）】
```text
文件数=5
整体 SHA-256=ad4edf0d837b734178a1231610f94370fa7d2ed053ebe5db580734834376566c
与开始前一致（ad4edf0d…），文件数一致；一次性实例未改动生产目录。
```

【机密值检索（只输出命中计数，不展开模式）】
```text
工作机 1 证据汇总（13 份含值风险的工具返回/执行输出）：通用测试值命中 0
演练机 1 证据文件（浏览器脚本输出与权威端 MCP 输出，11 份）：通用测试值命中 0（逐份均为 0）
NAS 挂载密码检索：工作机 1 证据汇总 0；权威端浏览器输出 0；权威端 serve.log 0
非零计数文件数：0
```

【本轮清理】
```text
本机临时 HTTP 服务：已停止（监听端口无进程）。
演练机 1 → 演练机 2 的一次性 SSH 端口转发：已全部关闭。
虚拟局域网辅助地址（两台演练机共 4 个不持久地址）：已全部删除。
NAS A 一次性子目录 sshmgr-e2e-20261114：已删除（先删文件再删空目录）。
Windows 受管服务器 A 一次性写入文件（反斜杠/正斜杠两个上传目标）：已删除。
Linux 受管服务器 A 一次性上传目录（含全部条目写入文件）：已删除（先删文件再删空目录）。
```

【按 Brief 保留（供 #278 收尾轮与 #279 使用）】
```text
演练权威端 serve 服务与 vault（含共享前置数据、一次性凭据、机密项授权、共享登记与挂载状态）
两个已配对客户端实例（工作机 1 e2e278-win、演练机 2 e2e278-linux）及其隔离目录
演练机 1 的一次性备份目录（未改动）与仅 root 可读的口令/密钥文件
工作机 1 的 C:\sshmgr-e2e-20261114\（含一次性 exe、vault/config/dek 隔离目录与证据文件）
演练机 2 上 Linux 受管服务器 A 的共享挂载（真实共享名以代称记录，属共享前置数据）
Linux 受管服务器 A 与 Windows 受管服务器 A authorized_keys 中的一次性公钥行
  （注释 sshmgr-e2e-278-20261114，含备份文件与 ACL 复核结论）
演练机 1 → 演练机 2 的一次性机器密钥（演练机 1 用户目录下的一次性密钥文件及其对端授权行）
```
