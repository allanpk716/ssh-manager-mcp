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

（执行中逐条追加）

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
