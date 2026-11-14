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
| 权威端275-1 | 失败→第二轮通过 | 2.6 | 第一轮：默认 ED25519 证书浏览器打不开（欠账第 36 条）。第二轮（rc7）：全条重跑通过，新默认证书 ECDSA P-256 且 Chromium/Firefox 159 可打开；既有 ED25519 证书未被替换 |
| 权威端270-1 | 通过 | 2.1 | 审计与值检索通过 |
| 权威端270-2 | 通过 | 2.1 | 首轮误授权已撤销并重跑 |
| 权威端270-3 | 通过 | 2.1 | 级联删除与预览通过 |
| 权威端271-1 | 通过 | 2.2 | 四类分组与只读边界通过 |
| 权威端271-2 | 通过（附欠账第 38 条） | 2.2 | 手写客户端角色文件后转发落库通过 |
| 权威端272-1 | 通过 | 2.3 | 版本联动与变更提示通过 |
| 权威端272-2 | 移交下一轮 | 2.3 | 需要 macOS 靶机 |
| 权威端272-3 | 失败（第 39 条已修复，新欠账第 46 条） | 2.3 | 第二轮（rc7）：幂等挂载不改写且只以网络名提示 owner（第 39 条修复）；但 `shares reselect --yes` 切换路径在 Linux 上恒失败（新欠账第 46 条）；--force 路径与幂等重选通过 |
| 权威端273-1 | 通过 | 2.4 | 浏览器用 WebKit 代跑 |
| 权威端273-2 | 通过（两子判据移交） | 2.4 | 重验成功回跳与敏感入口扫描未完成 |
| 权威端273-3 | 通过 | 2.4 | 421/403 与响应头通过 |
| 权威端273-4 | 通过 | 2.4 | 第二轮（rc7）：update 等价面补证（--check 干跑 + 本地包拒绝同版本重装）；附带发现 --check 干跑后误入重启分支（欠账第 48 条） |
| 权威端274-1 | 通过 | 2.5 | 第二轮（rc7）：位置移动「布局已保存。」子项补证通过（容器节点拖动 + POST 204 + 刷新保持） |
| 权威端274-2 | 第二轮通过 | 2.5 | 第二轮（rc7）：全条通过——重验拦截与回跳保留字段、虚拟地址首锚、同设备第二条目复制同端口锚、命令行等价面 |
| 权威端274-3 | 移交下一轮 | 2.5 | 需要 macOS 靶机；桌面目验移交 owner |
| 权威端274-4 | 第二轮通过 | 2.5 | 第二轮（rc7）：全条通过——四组与同设备提示、重验闸、profile 免重验、删除预览与级联、条目 B 命令行对照 |
| 权威端276-1 | 第二轮通过 | 2.7 | 第二轮（rc7）：备份/导出/双恢复 vault/服务切换/重新配对/恢复后复核全部完成；恢复后的权威端与客户端按 Brief 保留；期间复现欠账第 45 条一次（按提示恢复） |
| 客户端270-1 | 通过 | 3 | secrets add --instance 旗标缺失见备注 |
| 客户端270-2 | 通过 | 3 | 连接前整体拒绝通过 |
| 客户端270-3 | 通过 | 3 | 输出遮蔽通过 |
| 客户端270-4 | 通过 | 3 | 替换审计明细随收尾轮归档 |
| 客户端270-Windows-1 | 通过 | 3 | 环境变量与进程命令行通过 |
| 客户端270-Windows-2 | 通过 | 3 | 第二轮（rc7）：cmd 进程命令行检查补证通过，未再超时 |
| 客户端270-路径-1 | 通过 | 3 | 反斜杠/正斜杠路径均正确写入 |
| 客户端271-1 | 通过（附欠账第 38 条） | 3 | 本地结果边界与写拒绝通过 |
| 客户端271-2 | 通过 | 3 | 第二轮（rc7）：connection_notes（仅网络名+序号）、错误靶机 preauth 日志、实际尝试顺序全部补证 |
| 客户端271-3 | 通过（与权威端271-2 同路径） | 3 | 第二次连接摘要随收尾轮补查 |
| 客户端272-1 | 通过 | 3 | 现场检测只读通过 |
| 客户端272-2 | 失败（第 41 条已修复，新欠账第 47 条） | 3 | 第二轮（rc7）：exec_context 空名单输出 [] 且结构校验通过（第 41 条修复）；但 Windows 探测脚本本身恒失败、盘符预览不可用（新欠账第 47 条）；注入/暂停/恢复/审计通过 |
| 客户端272-3 | 第二轮部分通过（新欠账第 49、50 条） | 3 | 第二轮（rc7）：共享翻译/Linux 本地拷贝/Windows robocopy/双共享拒绝/停止留半成品通过；共享为目标端的写入全部失败（49）、停止后重跑挂起（50） |
| 客户端273-1 | 移交下一轮 | 3 | 客户端 Web UI 未及执行 |
| 客户端273-2 | 移交下一轮 | 3 | 同上 |
| 客户端274-1 | 移交下一轮 | 3 | 同上 |
| 客户端274-2 | 移交下一轮 | 3 | 同上 |
| 客户端275-1 | 第二轮通过（含欠账第 38 条复核） | 3 | 第二轮（rc7）：向导指引/进程退出/角色不变/配对+SAS/二次只读界面/清理全部通过；pair 自动写 role.json |

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

【第二轮（rc7）执行记录】通过。浏览器为 WebKit（同前：既有 ED25519 证书）；owner 不在场，由 agent 代跑。
一次性前置（CLI 创建）：同设备条目 A/B（一次性 ed25519 私钥登录演练机 2）、一次性设备（虚拟局域网成员）、
普通机密项、两个共用同一挂载密码机密项的一次性共享（登记用一次性共享名，本条不做真实挂载）、一次性
profile、两条真实下发记录（经 Linux 一次性客户端以占位符写入条目 A/B）、同设备同端口锚（TOFU 首连写入）。

【第 1 步：条目 A 详情四组与同设备提示（rc7）】
```text
四组齐全：机密项 / 共享存储 / profile 授权 / 下发记录；
同设备提示区出现「同设备其他服务器条目带来的…」；
共享存储组按挂载密码机密项归组：两个一次性共享显示「同一密码机密项：…」，
挂载密码机密项不出现在普通机密项组（复检=False）；
下发记录组只读（无编辑/删除入口），显示路径、机密项名、写入时间、发起设备与客户端实例，不含值。
```
【第 2 步：授权变更与重验证（rc7）】
```text
机密项授权变更（移除普通机密项对条目 A 的写入）：
  预览 → 确认 → 未重验时被拦截：303 跳转 /ui/reverify?next=…（预览状态保留在跳转链接的 resume 参数中，
  不含任何机密项值）→ 重验证 → 回到预览 → 确认应用成功。
共享存储授权变更（移除一组共享对条目 A 的可挂载）：预览 → 确认 → 应用成功（重验证后新鲜状态）。
profile 授权变更（移除条目 A 的一次性 profile）：再等待 5 分零 5 秒后直接提交 →
  未被重验证拦截，直接进入确认并应用 → profile 组不需要重验 ✓。
```
【第 3 步：删除条目 A（rc7）】
```text
删除预览包含：服务器条目与标识符、所属设备（设备保留）、主机密钥锚（要删除 1 个）、
失去这台服务器的 profile、不再可挂载的共享存储、移出可写入服务器列表的机密项、
会随之删除的凭据（登录凭据-私钥）、会随之删除的下发记录（1 条，含路径与机密项名）；
一次「确认删除」后条目 A 删除，列表不再出现。
```
【第 4–5 步：条目 B 与命令行等价（rc7）】
```text
条目 B 详情：同设备提示区不再显示来自条目 A 的共享与下发文件（含条目 A 名=False）；
四组仍完整呈现，B 自己的下发记录只读可见。
命令行：sshmgr servers show e2e278_r2_2744_b 与页面事实一致（连接、shell、凭据类型、分组）；
sshmgr secrets records --server e2e278_r2_2744_b 列出 B 的下发记录（路径/机密项/时间/发起设备/实例），
与页面分组一致；sshmgr servers rm e2e278_r2_2744_b 删除 B 成功。
```
【清理】一次性条目 A/B、设备、两个一次性共享、两个一次性机密项与一次性 profile 已全部删除；
真实共享与既有对象未动。

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

【第二轮（rc7）复核结论】通过（第一轮的失败判据全部翻绿；欠账第 36 条复核通过）。
执行位置与第一轮相同：演练机 2 默认空 vault（复核前再次确认 vault 目录为空、serve 未安装、无 sshmgr 进程）；
浏览器为演练机 1 上的真实 Chromium 与 Firefox，经一次性机器密钥 SSH 端口转发访问演练机 2 回环。
owner 不在场，向导、提权命令执行与浏览器动作按 owner 授权由 agent 代跑。

【第二轮证据（rc7）】
```text
向导三步与第一轮一致：步骤条「建 vault → 设置管理员口令 → 安装 serve」；
建 vault 页与设口令页文案一致；口令为一次性随机值（仅演练机 1 root 可读，复核后已删除）。
安装页完整提权命令（脱敏）：sudo '/usr/local/bin/sshmgr' serve install --addr <网络地址已脱敏>:<服务端口>；
  「复制命令」按钮可见可点击；向导本身不提权。
agent 在演练机 2 终端执行安装命令 → installed service "sshmgr-serve"（args=[serve --addr <网络地址已脱敏>:<服务端口>]）；
  service started。
完成页：标题「权威端初始化完成」，三步全部完成；地址列表仅回环地址（页面提示未找到本机虚拟局域网地址）；
  「sshmgr ui 即将退出」；向导进程随后自行退出=True。
serve status → Running / responding (401/200 = auth working) / vault ok / HEALTHY。
第二次 sshmgr ui → 只打印「权威端 Web UI 由 serve 进程的独立监听提供」+ 管理员口令已设置 +
  Web UI 端口/地址/证书指纹，命令随即退出；ss 仅见 serve 进程的监听，无新增回环初始化监听。
```
【第 36 条复核证据一：新默认证书算法与浏览器】
```text
新生成默认 serve 证书（serve 自动生成，未显式配置）：
  openssl x509 -noout -text → Signature Algorithm: ecdsa-with-SHA256；
  Public Key Algorithm: id-ecPublicKey；ASN1 OID: prime256v1；NIST CURVE: P-256。
Chromium 159.0.8154.6 打开正式 Web UI 登录页（经端口转发、访问默认证书）→ TLS 握手成功，登录页可见
  （第一轮同一页面报 ERR_SSL_VERSION_OR_CIPHER_MISMATCH）。
Firefox 159.0 打开同一地址 → TLS 握手成功，登录页可见（第一轮报 SSL_ERROR_UNKNOWN）。
登录页显示的证书公钥 SHA-256 指纹与 serve admin status 输出逐字一致（agent 代证，指纹值不进证据）。
```
【第 36 条复核证据二：既有 ED25519 证书不被替换】
```text
演练权威端（演练机 1）升级到 rc7 并重启 sshmgr-serve 后：
/var/lib/ssh-manager/serve-cert.pem 仍为 Signature Algorithm=ED25519，文件时间戳保持升级前的值，未被重写；
serve status HEALTHY。即：已存在的 serve 证书在 rc7 下原样沿用，不会被替换成 ECDSA。
```
【第二轮清理】
```text
演练机 2：serve uninstall → uninstalled service "sshmgr-serve"；
  printf "DELETE
y
" | sudo script … "sshmgr clear" → 已清理。下次 sshmgr ui 将重新进入初始化向导。
清理后：serve NOT INSTALLED；无 sshmgr ui/serve/mcp 进程；默认 vault 目录回到空；无残留监听；
  一次性向导口令文件、启动输出、端口转发与脚本 URL 文件均已删除。
（附带观察，范围外登记见报告：清理后无任何 sshmgr 进程时，`serve status` 在演练机 2 上也显示
  process: running，与欠账第 43 条在 Windows 上登记的现象同类。）
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

【第二轮（rc7）复核结论】失败（新欠账第 46 条：`shares reselect --yes` 的地址切换路径在 Linux 上失败）；第 39 条本身（幂等挂载不改写 + owner 切换提示）复核通过。owner 不在场，命令行由 agent 代跑。

【第二轮（rc7）证据】
```text
初始状态：共享 <NAS 共享名> 版本号 12；靶机（Linux 受管服务器 A）已按第一轮末状态挂载。
把靶机设备移出提供方物理局域网后执行 shares mount →
  update-success；版本号 12；实际网络 e2e278_virtual（第 1 个地址）——初始虚拟局域网挂载就位。
把提供方设备的物理局域网成员地址改为一个仍可达的更优物理地址后（见下方测试辅助说明），再次
  shares mount →（欠账第 39 条复核点）
  「共享存储 <NAS 共享名> 在服务器 e2e278_linux_a 上无需改动；版本号：13
    实际网络：e2e278_virtual（第 1 个地址）
    提示：存在更优物理局域网地址（网络 e2e278_nas_net）；如需切换请执行 sshmgr shares reselect … --yes」
  ✓ 未改写仍连通地址；✓ 提示只含网络名（e2e278_nas_net），不含地址；✓ 版本号未因地址选择变化
  （13 为提供方成员登记变更联动产生，与地址选择无关）。
shares reselect <NAS 共享名> e2e278_linux_a --yes（两次）→
  Error: 卸载共享存储挂载点失败，已保留旧配置（两次均失败，审计 result=unmount-failed）。
诊断：产品的 fstab 管理行自带 x-systemd.automount；reselect 固定命令先 systemctl 停 automount 单元
  （该操作在 Linux 上会连同挂载一起卸下），随后又要求 umount 挂载点必须成功——此时挂载点已不处于
  挂载状态，umount 退出码 32 → 固定命令 exit 85 → 整条 reselect 报错。手工按同顺序复现：
  停 automount 后 mount 表清空，umount 返回「not mounted」。失败后 fstab 与凭据文件确实保留，
  但挂载被留在未挂载状态。
shares reselect <NAS 共享名> e2e278_linux_a --force --yes（诊断性复核，属产品文档行为）→
  已重选地址；网络：e2e278_nas_net；版本号：13（地址切换本身不加版本号 ✓）。
再次 shares reselect --yes → 「当前地址已是最优可达地址，未做改动。」 ✓ 幂等。
切换后 /etc/fstab 管理行已改写：地址更换、凭据文件被替换、x-sshmgr-version=13；
  经新地址路径读写测试（写入→读回→删除测试文件）成功。
审计：share-mount（no-change）与 share-reselect（error×2 / force success / best-address-unchanged）
  行均在，摘要只含共享名、服务器名、网络名、版本号与结果，不含地址、账号或密码。
shares residue <NAS 共享名> → e2e278_linux_a：无残留；其余一次性条目按各自原因「未检查」。
```
【测试辅助说明（偏离登记）】真实提供方（NAS A）只有一个可达 SMB 地址，而第一轮登记中物理与虚拟
成员地址字符串相同，无法构造「当前地址可达且与更优物理地址不同」的册中场景。本轮在演练权威端上
临时起了一个一次性透明 TCP 转发（把演练权威端地址的 445 端口转发到 NAS A 的 445 端口），把提供方
物理局域网成员地址临时指向该转发路径，形成第二个可达且字符串不同的物理候选地址；取证完成后已停止
转发进程、把提供方地址登记与靶机挂载恢复为真实 NAS 地址路径（现场探测已挂载、版本号 14）。数据
全程仍真实流向 NAS A，无其他机器被触碰。
【清理说明】按本轮 Brief，共享登记、共享挂载与拓扑登记保留到 #278 收尾轮（#279 仍需使用）；
本轮已执行 residue 复查并留证。临时转发进程与临时登记已全部恢复。

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
【第二轮（rc7）：`sshmgr update` 等价面补证】
```text
sshmgr update --check（演练权威端，GitHub 在线干跑）→
  update base: https://api.github.com
  当前版本: 0.22.0-rc7；最新版本: v0.21.1；资产: sshmgr_0.21.1_linux_amd64.tar.gz
  干跑:未下载、未替换、未重启任何东西
  （随后命令误入服务重启确认分支并打印「⚠ 重启将断开活动隧道…」「替换已成功,但服务重启未完成(待手工)」，
   退出码 3——见下方附带发现；二进制哈希与时间戳复核确认实际零改动。）
sshmgr update --file <rc7 本地包> --sha256 <与候选产物一致> --yes →
  Error: 试运行失败：已装该版本(0.22.0-rc7);--file 提供的包与当前版本相同（退出码 1，零改动）
  复核：/usr/local/bin/sshmgr 哈希未变，仍为 0.22.0-rc7；serve status HEALTHY。
结论：命令行等价面存在（当前/最新/资产/更新源 + 本地包校验 + 相同版本拒绝）；
真实替换路径未重放——GitHub 最新为正式版，重放会把演练权威端降级且打断后续条目，
与第一轮同样的安全边界（该路径已由单机组合早期轮次的真实升级覆盖）。
```
【附带发现（新欠账第 48 条）】`sshmgr update --check` 在装有 serve 服务的权威端上，干跑输出正确后
没有提前返回，误入第 10 步服务重启分支：非交互终端下打印「替换已成功,但服务重启未完成(待手工)」
并退出码 3。实际二进制未被替换（哈希/时间戳不变）。root 与非 root 运行均复现。
候选源码核对：internal/cli/update.go 的 RunE 在 updateService.Run 返回后无条件进入服务分支，
未区分 CheckOnly 结果。

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

【第二轮（rc7）：位置移动「布局已保存」子项补证】通过。浏览器仍为 WebKit：演练权威端的 serve 证书是
第一轮之前生成的既有 ED25519 证书，按欠账第 36 条的修复语义 rc7 不会替换它，Chromium 打不开该证书，
故按 Brief 指示继续用 WebKit（Linux 平台 TLS 栈可握手）。owner 不在场，画布操作由 agent 代跑。
```text
登录后打开 /ui/topology/ 物理视图；鼠标拖动网络分组框（e2e278_nas_net）位移后松开 →
  [data-topology-status] 状态文本=「布局已保存。」；POST /ui/api/topology/layout 返回 204。
刷新页面并切回物理视图 → 画布正常重绘（18 个节点保持），布局按保存值呈现。
```
第一轮未复现的原因复核：第一轮脚本拖动对象与按住时长不同，未触发拖动结束的保存提交；
本轮对容器节点做真实鼠标拖动（多步移动 + 松开）后提示稳定出现。该提示为持续状态文本，
非瞬时提示，保存失败时会显示错误文案。
【等价命令与审计】topology.layout.save 审计与命令行探测等价面已在第一轮覆盖；布局坐标无命令行
等价命令的例外维持权威端273-4 登记。

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

【第二轮（rc7）补证】通过。cmd 进程命令行检查本次未再超时：
```text
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter 'name=''cmd.exe''' | Select-Object ProcessId,CommandLine"
→ 返回 cmd.exe 进程命令行：C:\Windows\system32\cmd.exe /d /s /c "powershell -NoProfile -Command Get-CimInstance …"
命令行参数中无测试值；工具返回无 masked 名单（未因命令行泄漏出现机密项名）；调用耗时约 10 秒，正常返回。
```

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

【第二轮（rc7）补证】通过（第一轮移交的 connection_notes 与错误靶机日志已补齐）。Linux 一次性客户端实例
对一次性多地址条目（物理局域网第 1 地址指向演练权威端的辅助地址=错误主机密钥靶机，虚拟局域网地址指向真正
目标）执行只读命令。前置：把客户端设备临时加入物理局域网（共用登记，地址为本机另一辅助地址；第一轮末已
按册移除），拉取快照后执行；取证后已再次移除该成员登记。
```text
exec_command → exit_code=0，stdout=用户命令输出；
connection_notes=["e2e278_phys 第1个地址呈现了不同的主机密钥，已跳过"]
  → 连接说明只用网络名+序号指称被跳过地址，不含真实地址 ✓
错误靶机（演练权威端 sshd）本机日志：Connection closed by <客户端辅助地址已脱敏> port <端口已脱敏> [preauth]
  → 连接在认证请求前关闭，未收到登录凭据 ✓
客户端本地 probe-results.json（按观察时间排序）：
  1) e2e278_phys <内部地址已脱敏> ok=False detail=主机密钥不同
  2) e2e278_virtual <内部地址已脱敏> ok=True
  → 实际尝试顺序：物理局域网错误地址在前、可用虚拟地址在后 ✓（成功地址缓存中的旧记录已按册先清除）
query_topology from=self to=<条目标识符> →
  「目标：<设备> [<条目>]；只能经虚拟局域网到达：e2e278_virtual；
   e2e278_virtual（虚拟局域网，实际会选） ✓21ms 4分钟前」
  → 查询只给登记分组与最近观察，未把被跳过的物理地址伪装成连通 ✓
```

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

【第二轮（rc7）复核结论】失败（新欠账第 47 条：`exec_context` 在 Windows 服务器上运行 POSIX 探测脚本，恒失败，盘符预览不可用）；欠账第 41 条本身复核通过（空注入名单输出 `[]`，结构校验通过）；盘符注入、认证失败暂停与恢复、本地审计全部通过。owner 不在场，机密项换值与恢复由 agent 代跑（值不进入证据）。

【第二步：盘符注入（rc7）】
```text
PowerShell Get-ChildItem X:\ → exit_code=0；injected_network_drives=[{drive:"X",share:<NAS 共享名>}]；
  stdout 列出共享目录内容（内容已脱敏）；无未注入警告。
cmd dir X:\ → exit_code=0；同样 injected_network_drives=[{drive:"X",share:<NAS 共享名>}]；目录行可见。
PowerShell 进程命令行检查：返回 PowerShell 进程完整命令行，含 sshmgr 注入包装；
  环境变量值经标准输入帧读入进程内变量后写入环境，命令行参数中无任何机密项值。
cmd 进程命令行检查（同时补证客户端270-Windows-2 第 2 步）：返回 cmd.exe 的
  /d /s /c "powershell -NoProfile -Command Get-CimInstance Win32_Process -Filter name=''cmd.exe'' …"
  完整命令行；参数中无测试值；两次工具调用均未超时（第一轮两次超时未取回的问题未复现）。
```
【第一步：exec_context（rc7）——欠账第 41 条复核与新失败】
```text
对 PowerShell 条目与 cmd 条目分别调用 exec_context：
  工具返回通过结构校验：injected_environment_names=[]、uninjected_environment_variables=[]、
    planned_network_drives=[]、uninjected_network_drives=[]（第 41 条：空数组字段输出 []，不再是 null）✓
  但 isError=true：exec_context probe failed (exit 1)，stderr 为 PowerShell 解析错误——
    探测脚本使用 POSIX 语法（[ -t 0 ] && echo tty || echo no-tty、cat /proc/self/uid_map、
    /proc/self/attr/current、$$/$PPID、sudo bash -c），PowerShell 5.1 不认 [、&&、|| 等语法；
    cmd 条目同样失败。即：exec_context 的上下文探测脚本没有 Windows 分支，Windows 服务器上恒失败，
    planned_network_drives 无法列出共享名与盘符（工具描述声称 Windows 端只做盘符预览、不探测连接，
    但实现仍先跑 POSIX 探测，失败后把已算出的盘符预览丢弃）→ 登记欠账第 47 条。
  候选源码核对（internal/mcpserver/context.go）：buildCtxCommand 仅生成 POSIX 命令，无 shell 分支；
    Windows 分支只在盘符预览数据结构上存在。该缺陷在 rc5 被第 41 条的结构校验失败掩盖。
```
【第四至五步：认证失败暂停与恢复（rc7）】
```text
owner 面（agent 代跑）把一次性挂载密码换成错误值（值不进入证据）→ 客户端快照自动同步
  （工具结果开头出现权威端配置已更新提示：机密项值已更改、共享版本号 14→15）。
第一次命令（Write-Output r2-authfail-1）：盘符 X 仍注入成功（Windows 复用了本机已有的
  到 NAS 的已认证 SMB 会话，属操作系统行为），用户命令执行成功。
第二次命令（r2-authfail-2）：输出「共享 <NAS 共享名> 的网络盘符 X：未注入：共享密码认证失败，
  已暂停注入，请同步或检查机密项」；用户命令本身仍执行成功（不被映射失败阻断）。
owner 面（agent 代跑）把正确密码写回机密项 → 新会话自动同步（共享版本号 15→16）。
紧接的第一次命令仍短暂报认证失败暂停（与上一次失败间隔不足数秒，Windows 侧旧会话/凭据
  状态尚未释放，属瞬时现象，已如实记录）；数秒后的两次重试命令：盘符 X 全部重新注入成功，
  命令正常执行 → 暂停自动解除、盘符恢复满足判据。
```
【第六步：客户端本地 cache-audit.log（rc7）】
```text
本轮各条命令的审计行：command 字段只含 agent 原命令原文；injected_share_names 只含
<NAS 共享名>；无地址、账号或密码；未注入警告不进入命令文本。
```
【清理说明】共享授权与两条 Windows 条目按 Brief 保留到收尾轮；本轮未删除共享登记与机密项。

### 客户端272-3 共享地址中继本地拷贝、流式回退与重跑

【结论】移交下一轮。原因：条目组合要求两条 Linux 条目都已实际挂载且需大文件中继、
中断重跑与哈希比对的长流程；本轮时间窗口已用尽。已具备条件：Linux 条目 A 已挂载、
Linux 条目 B 设备已登记虚拟局域网成员、Windows 条目与共享授权齐备、客户端可达权威端。

【第二轮（rc7）执行记录】部分通过（两处产品缺陷登记欠账第 49、50 条；一处环境限制如实登记）。
执行实例按册用工作机 1 一次性实例；因网络事实（工作机 1 到不了 Linux 受管服务器 A 的内网段），
涉及 Linux 端点的中继由 Linux 一次性客户端实例代替执行（同册条目、同真实共享与靶机），
Windows 端点仍由工作机 1 执行。一次性源文件 20 MiB（SHA-256 14578de7…8f761），共享内一次性子目录与
本地源文件均为一次性对象。

【第 1–2 步：共享地址翻译与 Linux 本地拷贝（rc7）】
```text
query_topology from=<Linux条目A> to=<共享地址> →
  服务器：e2e278_linux_a；共享存储：<NAS 共享名>（提供方设备 <NAS 设备>）；可挂载：是；
  实际路径：/mnt/sshmgr/<共享名>/<一次性子路径>/r2-relay-src.bin ✓
exec_command 命令文本中的共享地址原样出现在 stdout，未被改写 ✓
relay_file from_share_address=<共享源> to_server_id=<Linux条目A> →
  mode="local_copy"；restart_after_interruption=true；bytes_total=20971520；space_check=ok；
  任务输出含固定 cp 命令（cp -- '<挂载点源>' '<目标>.sshmgr-partial'）与
  「本地拷贝模式：中断、停止或失败后不能续传，重跑会从文件开头覆盖旧半成品」提示；
  完成后目标真名出现、半成品消失；目标端 SHA-256 与源一致 ✓（14578de7…8f761）
```
【第 3 步：反向本地拷贝（服务器→共享）——失败（欠账第 49 条）】
```text
relay_file from_server_id=<Linux条目A> from_path=<普通路径> to_share_address=<共享目标> →
  mode="local_copy" 正确判定、固定 cp 命令正确生成；但目标端共享目录按产品自身挂载映射为
  root 所有（uid 未映射），relay 明确不提权，SSH 用户无法在挂载点创建文件 → 拷贝未完成，
  共享内无目标文件（读取共享正常，写入失败）。
```
【第 4 步：Windows 盘符本地拷贝（rc7）】通过。
```text
工作机 1 实例 relay_file from_share_address=<共享源> to_server_id=<Windows PowerShell 条目>
  to_path=<盘符根下一次性目标> → mode="local_copy"；restart_after_interruption=true；
  任务输出含固定 robocopy 命令（目标为 <目标>.sshmgr-partial）与本地拷贝模式提示；
  完成后共享内目标文件 20971520 字节，SHA-256 与源一致 ✓（14578de7…8f761）。
```
【第 5 步：流式回退与 via 覆盖（rc7）】
```text
执行实例本地文件 → to_share_address（不传 via）→ mode="streaming_relay"；
  share_endpoint_server_id=<Linux条目A>（自动选择物理局域网优先且已实际挂载的端点）；
  任务输出「流式转发：共享地址由服务器 e2e278_linux_a…暴露；中断、停止或失败后按目标端块清单续传」。
带 via=<Linux条目B> 重跑 → share_endpoint_server_id=<Linux条目B>（指定虚拟局域网端点覆盖自动选择）✓
  但流式任务在共享目标端写空块清单失败：permission denied（同欠账第 49 条：共享挂载 root 所有、
  非提权 SSH 用户不可写；该步在当前产品挂载模型下无法完成）。
```
【第 6 步：两端都是共享地址（rc7）】通过。
```text
relay_file 同时传 from_share_address 与 to_share_address →
  「源端和目标端不能都是共享地址；请先用 query_topology 查询实际路径，再在可挂载共享的服务器上用
   exec_command 拷贝」（与册中期望文案逐字一致）
```
【第 7 步：停止与重跑（rc7）——部分失败（欠账第 50 条）】
```text
重新发起同一 Linux 本地拷贝后立即 exec_stop(task_id) → 目标端留下
  <目标>.sshmgr-partial（2097152 字节），无块清单 ✓；
重跑同参数任务 → 任务输出再次给出「重跑会从文件开头覆盖旧半成品」与固定 cp 命令；
  但重跑任务在 3 分钟内未完成：exec_output 一直 status=running，目标端半成品未增长、
  系统上也没有 cp 进程（共享读取此刻正常——直接 dd 读源文件成功）。
  判定：停止后的同目标重跑存在挂起缺陷，登记欠账第 50 条；
  「从头覆盖旧半成品、完成后真名出现」的完成语义已由第 2 步首次完成的拷贝与哈希比对证明。
```
【哈希与清理说明】源/目标 SHA-256 均为 14578de7…8f761（比对一致的两处：Linux 本地拷贝、Windows robocopy）。
共享内一次性文件、普通服务器文件与半成品随收尾清理；本轮登记的欠账见第 49、50 条。

### 客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI

【结论】移交下一轮。原因：客户端 Web UI 需在演练机 2 的一次性客户端实例上启动回环监听并经端口转发
用浏览器取证；本轮时间窗口已用尽。已具备条件：一次性客户端实例已配对并可拉取快照、
演练机 1 的浏览器可用（客户端回环为纯 HTTP，不涉及证书问题）。
册中要求工作机 1 的条目由 Linux 一次性客户端代替的原因：工作机 1 禁止打开浏览器与触碰桌面，
客户端 Web UI 只监听回环，需经端口转发访问。

### 客户端275-1 客户端角色的配对指引与退出

【结论】移交下一轮。原因：需要一个全新一次性实例的向导浏览、配对与清理长流程；
本轮两个已配对实例与共享数据需保留，时间窗口已用尽。

【第二轮（rc7）执行记录】通过（含欠账第 38 条复核）。执行位置：演练机 2 上全新的一次性未配对客户端环境
（vault/用户配置/缓存密钥目录均指向一次性子目录，实例名 e2e278-r2-new）；浏览器为演练机 1 上的真实
Chromium，经一次性机器密钥端口转发访问演练机 2 回环（客户端向导为纯 HTTP，无证书问题）。
owner 不在场，向导、配对、批准与短认证码比对均由 agent 代跑（双侧 SAS 经脚本逐字比对，值不进入证据）。

【第 1–3 步：客户端分支指引与进程退出（rc7）】
```text
未配对环境 sshmgr ui → 选择本机角色页可见「客户端」选项 → 选择客户端 →
「客户端入网」页全文：只列出
  sshmgr pair --instance <名字>
  --url <权威端地址>：不使用自动发现，直接指定权威端地址。
  --pin <证书公钥指纹>：校验权威端证书，防止连接被冒充。
  --write-mcp <文件路径>：把配对生成的 MCP 配置文件另存到指定路径。
  配对发起后，到权威端 Web UI 的配对页批准；本页不执行配对，也不会写入任何文件。
  （页面无配对表单）
页面完整显示后 sshmgr ui 进程自行退出=True。
角色状态复核：sshmgr role show → 角色=尚未初始化；初始化向导=未完成；
  一次性环境内无 role.json；默认用户配置目录也未生成 role.json。
```
【第 4 步：配对与短认证码（rc7）】
```text
前置：owner 面（agent 代跑）把 serve.pairing 打开（第一轮末按册恢复为 false；产品设置约 5 秒内生效，
  期间一次 404 属预期，重启 serve 后确认），批准完成后已恢复 false 并留证。
客户端按页面指引发起 sshmgr pair --instance e2e278-r2-new --url <权威端地址> --pin <证书公钥指纹>
  → 客户端屏幕显示 e2e278-r2-new @ <权威端地址> SAS <短认证码已脱敏>；
权威端队列同名单同地址同 SAS；owner 不在场，按 owner 指示由 agent 比对两侧文本：
  实例名、地址、短认证码三项逐字一致（SAS-MATCH=True，值不进入证据）。
权威端 Web UI 配对页：选择一次性 profile、勾选「已与客户端屏幕上的短认证码逐位比对一致」、批准
  → 状态「已批准，等待客户端完成」→ 客户端侧完成：
  已授权 profile: e2e278-r2-new；pulled 1 servers / 1 credentials；配对完成；MCP 产物生成（令牌不进入证据）。
```
【欠账第 38 条复核（rc7）】通过。
```text
配对完成后：sshmgr role show → 角色=client；初始化向导=已完成；
  一次性环境的用户配置目录下 role.json 已由 pair 写入（第一轮两台客户端均不写，需手写后才可用）。
首次锚定复核：新实例对一条全新的一次性未锚定拓扑设备条目（设备仅登记虚拟局域网成员）执行只读命令
  → exit_code=0，stdout 正常返回；未出现第一轮的「首次锚定只能经虚拟局域网…没有共同虚拟局域网」拒绝；
  权威端 servers pin-hostkey 显示该条目锚已生成（指纹与靶机本机读取一致，握手地址为虚拟成员地址）。
（第一轮手写的两个一次性环境 role.json 按 Brief 保留不动，未受影响。）
```
【第 5 步：二次进入只读界面（rc7）】
```text
sshmgr ui --instance e2e278-r2-new → 直接进入客户端只读页面：
  「这里只读，修改需到权威端 Web UI 去做。」；服务器列表只显示该 profile 授权的条目。
```
【第 6 步清理（按册）】
```text
客户端：sshmgr cache instances rm e2e278-r2-new --yes → 实例槽删除（提示写在槽外的 .mcp.json 副本不随 rm 清理）；
  一次性环境目录已逐文件删除并移除空目录。
权威端：cache-tokens revoke e2e278-r2-new（status=revoked）→ projects revoke pair-e2e278-r2-new →
  profiles remove e2e278-r2-new；一次性条目与设备已删除；serve.pairing 已恢复 false。
两个既有已配对实例（e2e278-win、e2e278-linux）与共享前置数据未受影响。
```

## 2.5 Web UI 新功能页面（#274）

### 权威端274-2 新增服务器、凭据重验与首次锚定

【结论】移交下一轮。原因：需在浏览器完成带凭据表单、五分钟重验回跳保留字段、连接测试轮询、
同设备第二条目与等价命令对照的长流程；本轮 WebKit 会话已用于 273-2/273-3/273-4/274-1，
时间窗口用尽。已具备条件：权威端 Web UI 可用（WebKit）、一次性 SSH 靶机与带外指纹事实齐备。

【第二轮（rc7）执行记录】通过。浏览器为 WebKit（原因同权威端274-1：演练权威端沿用既有 ED25519 证书）；
owner 不在场，表单、重验证与提权外的全部动作由 agent 代跑。一次性靶机：演练机 2 经一次性 ed25519 私钥登录，
设备只登记虚拟局域网成员（不填站点与物理局域网），首次锚定只能走虚拟局域网地址；带外主机密钥指纹事实
从靶机本机 ssh-keygen 读取（agent 代证）。

【第 1–2 步：带凭据表单与重验证（rc7）】
```text
登录后等待 5 分零 5 秒使重验证状态过期 → 打开新增服务器表单，页面显示需要重新验证管理员口令的提示。
填写：名称/用户/端口（22）/shell=auto/登录凭据=粘贴一次性私钥/新设备+虚拟局域网成员；兜底主机留空；
勾选一次性 profile。第一次提交 → 303 跳转 /ui/reverify?next=…/servers/new?resume=1（未写入条目）。
重验证页输入管理员口令 → 验证并继续 → 回到表单：提示「已保留表单状态，请检查后再次提交。」；
名称/用户/设备名字段保留；私钥内容不回显（暂存凭据，不进跳转链接）。
再次提交 → 已保存，连接测试完成。
```
【第 3 步：结果页（rc7）】
```text
状态=已验证；主机密钥指纹=SHA256:D0A1uAGCdjsCwQj0eCbsvnhA76KZGL5XWOu2ofrJwbk
锚定结果=首次连接已记录主机密钥锚（真实握手地址 <虚拟局域网辅助地址>:22）
  → 首次锚定只经虚拟局域网地址 ✓；握手地址为登记的虚拟成员地址 ✓
shell 测试=shell 类型已保存为 posix（依据：echo $PSVersionTable 在 POSIX shell 中无输出）
profile 授权=e2e278_r2_2742 ✓
带外比对（agent 代证）：结果页指纹与靶机本机 ssh-keygen -lf <ECDSA 主机公钥> 输出逐字一致。
```
【第 4 步：同设备第二条目（rc7）】
```text
拓扑页未归类托盘中打开设备详情 → 「在此设备上新增服务器条目」入口 → 表单预选已有设备；
第二条目不填兜底主机，粘贴同一私钥并勾选 profile → 已保存，连接测试完成；
结果页：「已按现有主机密钥锚完成校验（真实握手地址 <虚拟地址>:22）」+
  「已从服务器条目 e2e278_r2_2742_srv 复制同端口主机密钥锚。」→ 沿用设备网络成员并复制同端口锚 ✓；
同设备两台条目在拓扑上合并为一个节点（未归类托盘单节点，带条目数量角标）。
```
【第 5 步：命令行等价面与审计（rc7）】
```text
devices add / devices member add（网络+地址）→ 成功；
servers add --name e2e278_r2_2742_cli --user <一次性用户> --port 22 --device <设备> --key <一次性私钥> → added server …；
profiles grant e2e278_r2_2742 e2e278_r2_2742_cli → granted 1 server(s)；
servers probe-shell e2e278_r2_2742_srv → real shell=posix — matches the annotation（含判定依据）；
servers pin-hostkey e2e278_r2_2742_srv → fingerprint/key-type=ecdsa-sha2-nistp256/source=tofu/handshake=<虚拟地址>:22。
审计：server.add（web_ui 与 cli）、device.member-add、profile.grant 行均在，摘要只含名称/用户/端口/网络名，
不含私钥、口令或凭据值。
```
【清理】两条一次性条目、两个一次性设备与一次性 profile 已删除（server.delete / device.rm / profile.rm
审计留痕，锚随删除标注）；一次性私钥与 authorized_keys 行暂保留到本轮收尾统一清理。

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

【第二轮（rc7）执行记录】通过（按本轮 Brief：恢复后的权威端继续运行、两个客户端重新配对后保留，
册中第 8 步清理推迟到收尾轮；另复现欠账第 45 条一次，见下）。owner 不在场，命令行、浏览器与
短认证码比对均由 agent 代跑。

【第 1–3 步：备份、导出与两个空恢复 vault（rc7）】
```text
backup create --dir <root 专属一次性目录> --passphrase-file <一次性口令文件> --keep 1 --prefix release87
  → wrote release87-20261114-144833.sme（目录先按产品要求写入 marker 文件）
backup verify <最新加密备份> --passphrase-file … → ok（解密且 JSON 结构有效）
export --out <导出文件> --passphrase-file … → exported 15 servers / 18 credentials
两个空恢复 vault（SSHMGR_STORE/SSHMGR_FILEKEY_PATH 指向 root 专属一次性目录，各自 init 生成主密钥后）：
  恢复 vault 1 ← 加密备份导入 → imported 15 servers / 18 credentials
  恢复 vault 2 ← 导出产物导入 → imported 15 servers / 18 credentials
两恢复 vault 复核一致：15 个服务器条目、54 行机密项清单、共享 <NAS 共享名> 版本 16、9 行设置；
源权威端三视图布局基线（每视图 11 设备坐标 + 7 容器坐标）已留档。
（过程事实：向全新 vault 导入前需先 init 生成主密钥——直接对未初始化目录导入会报 vault locked，属产品
  既有语义，重试后通过。）
```
【第 4–5 步：服务切换与恢复后权威端（rc7）】
```text
源权威端 serve status（HEALTHY）留证 → serve uninstall → 已注销；
源 vault 全量快照保留到 root 专属一次性目录（供收尾轮比对）；默认 vault 目录清空后：
  init --role server → 已初始化为权威端（新主密钥）；
  import <导出产物> --passphrase-file … → imported 15 servers / 18 credentials；
  serve admin set（新一次性随机口令，仅 root 可读文件，不打印）→ 已设置（会话代 1）；
  serve install --addr 0.0.0.0:<服务端口> → 服务已安装并启动；
  serve status → Running / responding / vault ok / HEALTHY；serve admin status → 口令已设置。
恢复时新生成的默认 serve 证书为 ECDSA P-256（与欠账第 36 条一致，恢复路径复证）。
```
【第 6 步：设备码不随备份携带与重新配对（rc7）】
```text
恢复后 cache-tokens ls 为空（旧设备码未随备份/导出复活）✓；serve.pairing 临时打开（约 5 秒生效）。
Linux 一次性客户端 pair --force → SAS 双屏比对（agent 代证，逐字一致）→ CLI 批准到原 profile；
Windows 一次性客户端经宿主机转发地址 pair --force → 批准时按产品安全闸加 --allow-foreign-url
  （客户端声明的转发地址 ≠ 权威端本机地址，产品要求显式确认，属预期行为）。
第一次 finish 阶段两侧客户端均报 HTTP 500 finish failed——复现欠账第 45 条：导出产物携带的
  pair-<实例名> 项目令牌被恢复后的权威端判为「不是配对生成」，同名重配对被拒。
按产品提示恢复：owner 面（agent 代跑）revoke+remove 两个 pair-* 项目 → 两客户端重新 pair --force →
  批准 → 配对完成：Linux pulled 4 servers / 5 credentials；Windows pulled 12 servers / 14 credentials；
  cache-tokens ls 两条新设备码 active（client_version=0.22.0-rc7），队列清空；serve.pairing 恢复 false。
```
【第 7 步：恢复后复核（rc7）】
```text
设置：settings ls 仍为 9 行（六个 Web UI 相关设置随导出恢复）；共享：shares list 仍为
  <NAS 共享名> 版本 16、5 台可挂载；服务器条目 15 个；拓扑三视图布局：33/34 条坐标逐字一致
  （11+11+11 条设备坐标全等；7+7+7 条容器坐标中仅一个站点容器 y 值 409.5→410，半像素读回舍入差，
  三视图同源同差，判为页面读回舍入而非恢复缺陷）。
机密项写入与下发记录：重新配对的 Linux 客户端以占位符模板 upload_content → 38 字节 recorded；
  恢复后权威端 secrets records 列出该新记录（路径与模板原文可见，值不出现）✓。
```
【保留（按本轮 Brief）】恢复后的权威端 serve 服务与 vault 继续运行；两个客户端实例重新配对后保留；
备份/导出产物与两个恢复 vault 快照、源 vault 快照随收尾清理（root 专属目录）。

## 欠账复核（第二轮 rc7）

### 欠账第 36 条：默认 serve 证书算法与浏览器兼容

【复核结论】通过。两处判据均实证：
```text
1) 新生成的默认 serve 证书（演练机 2 一次性 vault，向导自动生成、未显式配置）为
   ECDSA P-256：openssl x509 -noout -text → id-ecPublicKey / prime256v1 / NIST CURVE: P-256 /
   ecdsa-with-SHA256；Chromium 159 与 Firefox 159 经端口转发访问正式 Web UI 登录页均握手成功、
   页面可见（第一轮同一浏览器对旧默认 ED25519 证书分别报 ERR_SSL_VERSION_OR_CIPHER_MISMATCH /
   SSL_ERROR_UNKNOWN）。证据小节：权威端275-1 / 第二轮（rc7）。
2) 已存在的 ED25519 证书不被替换：演练权威端升级到 rc7 并重启 sshmgr-serve 后，既有
   serve-cert.pem 仍为 ED25519、文件时间戳未变，serve HEALTHY。证据小节：同上。
```

### 欠账第 37 条：客户端 `servers show <条目> --instance <实例>`

【复核结论】通过。两个已配对客户端实例上分别执行：
```text
Linux 一次性客户端实例：sshmgr servers show e2e278_linux_a --instance e2e278-linux → 正常输出
  条目名、连接（地址已脱敏）、shell 类型、凭据类型、机密项与共享存储分组、
  「仅显示本客户端可用的部分，修改请到权威端。」；退出码 0（第一轮报 invalid --mode）。
工作机 1 一次性实例（rc7 exe）：sshmgr servers show e2e278_win_ps --instance e2e278-win 与
  sshmgr servers show e2e278_win_cmd --instance e2e278-win → 均正常输出，退出码 0；
  PowerShell 条目还列出同设备其他条目带来的共享存储（只读事实提示）。
```

### 欠账第 38 条：`sshmgr pair` 写客户端 role.json

【复核结论】（待客户端275-1 新实例执行后填入）

### 欠账第 38 条：`sshmgr pair` 写客户端 role.json

【复核结论】通过（客户端275-1 第二轮，新实例 e2e278-r2-new）。
```text
配对完成 → sshmgr role show：角色=client；初始化向导=已完成；role.json 位于一次性环境用户配置目录
（pair 自动写入，无需手写）。新实例对全新未锚定拓扑设备条目首次执行命令成功（exit 0），
未再出现「没有共同虚拟局域网」拒绝；权威端锚记录生成（握手地址为虚拟成员地址）。
证据小节：客户端275-1 / 第二轮（rc7）。
```

### 欠账第 39 条：幂等挂载不改写仍连通地址并提示 owner 切换

【复核结论】通过（权威端272-3 第二轮）。核心输出：
```text
现有地址仍连通且存在更优物理地址时再次 shares mount →
「无需改动；版本号：13 / 实际网络：e2e278_virtual（第 1 个地址）/
  提示：存在更优物理局域网地址（网络 e2e278_nas_net）；如需切换请执行 sshmgr shares reselect … --yes」
未改写地址；提示只含网络名不含地址；版本号未因地址选择变化。证据小节：权威端272-3 / 第二轮（rc7）。
（同轮发现切换路径新缺陷，登记欠账第 46 条，与第 39 条判据分开判定。）
```

### 欠账第 40 条：预发布版本客户端能力识别

【复核结论】通过。两个客户端均以 0.22.0-rc7 拉取快照后，权威端 `sshmgr cache-tokens ls`：
```text
e2e278-linux … status=active … client_version=0.22.0-rc7
e2e278-win   … status=active … client_version=0.22.0-rc7
两行均不再出现「旧版客户端：不支持机密项、网络拓扑、共享存储」标注（第一轮 rc5 客户端被标旧版）。
```

### 欠账第 41 条：`exec_context` 空数组字段输出 `[]`

【复核结论】通过（按本条登记范围）。对 PowerShell 条目与 cmd 条目调用 exec_context：
```text
工具结果通过结构校验；injected_environment_names=[]、uninjected_environment_variables=[]、
planned_network_drives=[]、uninjected_network_drives=[]（空名单为空数组，不再是 null）。
第一轮的 validating /properties/injected_environment_names: type "null", want "array" 不再出现。
证据小节：客户端272-2 / 第二轮（rc7）。
（同轮暴露下一层缺陷：Windows 上探测脚本本身恒失败，登记欠账第 47 条，与第 41 条分开判定。）
```

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

## 第二轮（rc7）安装与版本

【第二轮执行说明】owner 不在场，按调用方授权由 agent 继续代跑；本轮候选版本 0.22.0-rc7（源提交 `b8a0e0a`），
补跑第一轮移交条目并复核欠账第 36–41 条。第一轮结论与证据全部保留；本轮证据均以「第二轮（rc7）」小节追加。

【产物哈希核对（本机候选产物目录）】
```text
linux_amd64.tar.gz  SHA-256 17cf19512a84dea317230b237b363844ce2d8caee995341233a6e4440cf4fe49 = 与 Brief 给定值一致
darwin_arm64.tar.gz SHA-256 ab114b9711e743c877a6b76e60a34ae23dd4815a268587272295f7b87d689c20 = 与 Brief 给定值一致
windows_amd64.zip   SHA-256 e788c6ed74605ad3c7e1710c61f7f8ec33eedc04ddc96afb837083496540f4e9 = 与 Brief 给定值一致
```

【升级前版本（第二轮开始）】
```text
一次性演练权威端：0.22.0-rc5；serve 服务 Running / responding / vault ok / HEALTHY
Linux 受管服务器 A：0.22.0-rc5；无 sshmgr ui/serve/mcp 进程、无 sshmgr 服务单元
工作机 1 一次性客户端实例：0.22.0-rc5（bin 目录下的一次性 exe）
Windows 受管服务器 A：无 sshmgr 进程；一次性目录仍在（内含第一轮 work 子目录）
```

【升级记录（2026-11-14，经本机临时 HTTP 服务分发）】
```text
一次性演练权威端：下载 linux_amd64.tar.gz → sha256sum -c 输出 OK；
  旧二进制备份为 /usr/local/bin/sshmgr.rc5；替换为 rc7；重启 sshmgr-serve 后
  serve status：Running / responding (401/200 = auth working) / vault ok / HEALTHY；sshmgr version → 0.22.0-rc7
  升级重启后既有 serve 证书仍为 ED25519（文件时间戳未变），未被替换（欠账第 36 条第二判据的证据之一）
Linux 受管服务器 A：同样下载并核对 SHA-256 OK，备份 sshmgr.rc5 后替换；sshmgr version → 0.22.0-rc7
工作机 1：下载 windows_amd64.zip（SHA-256 核对一致），解压到 C:\sshmgr-e2e-20261114\bin-rc7\；
  bin-rc7\sshmgr.exe version → 0.22.0-rc7；不进 PATH，不覆盖 bin\ 下 rc5 exe，不触碰生产 sshmgr
```

【第二轮开始前检查】
```text
三台机器遗留一次性 Web UI / serve / mcp 进程：0（一次性演练权威端仅有应保留的 sshmgr-serve 服务）
Windows 受管服务器 A：无 sshmgr 进程
工作机 1 生产目录指纹（文件数=5；逐文件 SHA-256 按路径排序拼接后再取 SHA-256，
  与第一轮方法同族）=9dc1c658a28e00ff081e3f7a6d6df455e587556638f1211c211c1809d7c7bb95
  （与第一轮报告值不同：工作机 1 上 owner 的生产 sshmgr 进程一直运行，生产目录属活目录；
   本轮只以同一方法比对开始前/结束前指纹）
```

## 第二轮（rc7）收尾与机密值检索

【工作机 1 生产目录指纹（第二轮结束前）】
```text
文件数=5
整体 SHA-256=9dc1c658a28e00ff081e3f7a6d6df455e587556638f1211c211c1809d7c7bb95
与第二轮开始前完全一致（9dc1c658…），文件数一致；一次性实例未改动生产目录。
（与第一轮报告值不同的说明见「第二轮（rc7）安装与版本」：工作机 1 生产 sshmgr 进程持续运行，
 生产目录属活目录；本轮自比对一致。）
```

【机密值检索（只输出命中计数，不展开模式；检索后临时拷贝即删）】
```text
工作机 1 第二轮证据汇总（r2-* 工具输出，45 份）：通用测试值命中 0；NAS 挂载密码命中 0
演练权威端证据文件（第二轮浏览器脚本输出汇总）：通用测试值命中 0；NAS 挂载密码命中 0
恢复后权威端 serve.log：通用测试值命中 0；NAS 挂载密码命中 0
非零计数文件数：0
```

【第二轮清理】
```text
本机临时 HTTP 服务：已停止（端口无监听）。
演练机 1/演练机 2 虚拟局域网辅助地址（本轮共 4 个不持久地址）：已全部删除。
NAS A 一次性子目录（含中继源/目标/半成品文件）：已逐文件删除并移除空目录。
演练机 2 /tmp 本轮一次性文件（中继源/目标/半成品、上传目标、脚本输出）：已删除。
演练权威端 /tmp 与 root 专属一次性目录（备份目录、导出产物、口令文件、源 vault 快照、两个恢复 vault、
 一次性 known_hosts、临时转发脚本）：已删除；仅保留恢复后权威端的管理员口令文件与一次性登录密钥
 （authorized_keys 行按 Brief 保留，见下）。
工作机 1：中继本地源文件与指纹临时文件已删除；r2-* 证据文件保留在一次性目录内（属 Brief 保留范围）。
```

【按 Brief 保留（供 #278 收尾轮与 #279 使用）】
```text
恢复后的演练权威端 serve 服务与 vault（276-1 恢复出来的那个，继续运行；管理员口令在演练权威端
 root 专属文件 e2e278-r2-admin2，未打印）
两个已重新配对的客户端实例（e2e278-win、e2e278-linux，均 rc7、cache-tokens active）
共享前置数据（共享登记版本 16、演练机 2 挂载、拓扑登记）
工作机 1 的 C:\sshmgr-e2e-20261114\（含 rc5/rc7 两份一次性 exe、隔离目录与全部证据文件）
演练机 2 的 Linux 一次性客户端实例目录
两台受管服务器 authorized_keys 中的一次性公钥行（第一轮注释与第二轮注释 e2e278-r2-20261114）
演练权威端 root 专属的一次性登录密钥（e2e278-r2-key，与上一行配套）
第一轮保留的旧管理员口令文件、一次性备份目录与 root 专属密钥文件（未动）
```

【第二轮脱敏 grep】
```text
grep -nE '<第一轮 Brief 中的模式>' 多机运行记录 + 单机运行记录 + 欠账清单
唯一命中：欠账清单第 17 条历史存量行（含默认端口数字样，第一轮已登记的既有内容）。
本轮全部新增内容（多机运行记录、欠账清单 diff）零命中；发现并修正过一处新增内容中的
一次性用户名后复验为零。
```
