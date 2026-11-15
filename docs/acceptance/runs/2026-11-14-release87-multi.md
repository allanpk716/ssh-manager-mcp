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
| 权威端272-2 | 未执行（owner 处置：已知缺口） | 2.3 | 缺一次性 macOS 受管靶机；2026-11-15 owner 明确处置为本版已知缺口，登记欠账第 51 条，带着放行 |
| 权威端272-2（第六轮 macOS 补跑） | 失败 | 权威端272-2、权威端274-3 macOS 补跑 | macOS 撤销/换密码/清理与 Windows 停止注入通过；residue 只余撤销标记时未按册输出「仅有撤销标记」（第 56 条），且 unmount 无法清理仅剩标记（第 57 条） |
| 权威端272-2（第七轮 rc12 复跑） | 通过 | 权威端272-2 复跑（第七轮 rc12，一次性 macOS 虚拟机） | macOS 与 Linux 撤销、换密码说明、只余撤销标记识别、标记清理、Windows 停止注入与审计均按册通过；欠账第 56、57 条复核通过 |
| 权威端272-3 | 失败→第三轮通过 | 2.3 | 第二轮（rc7）：幂等挂载不改写且只以网络名提示 owner；非 force 重选失败登记第 46 条。第三轮（rc8）：旧挂载行幂等补属主映射且地址不变（第 49 条），非 force 重选切换、读写与二次幂等全部通过（第 46 条） |
| 权威端273-1 | 通过 | 2.4 | 浏览器用 WebKit 代跑 |
| 权威端273-2 | 通过（两子判据移交） | 2.4 | 重验成功回跳与敏感入口扫描未完成 |
| 权威端273-3 | 通过 | 2.4 | 421/403 与响应头通过 |
| 权威端273-4 | 通过（第 48 条第三轮复核通过） | 2.4 | 第二轮（rc7）：update 等价面补证并发现干跑误入重启分支（第 48 条）。第三轮（rc8）：root 与非 root `update --check` 均退出码 0、无误导输出且二进制哈希不变 |
| 权威端274-1 | 通过 | 2.5 | 第二轮（rc7）：位置移动「布局已保存。」子项补证通过（容器节点拖动 + POST 204 + 刷新保持） |
| 权威端274-2 | 第二轮通过 | 2.5 | 第二轮（rc7）：全条通过——重验拦截与回跳保留字段、虚拟地址首锚、同设备第二条目复制同端口锚、命令行等价面 |
| 权威端274-3 | 未执行（owner 处置：已知缺口） | 2.5 | 缺一次性 macOS 受管靶机；2026-11-15 owner 明确处置为本版已知缺口，登记欠账第 51 条；桌面目验由人工272-1 承担 |
| 权威端274-3（第六轮 macOS 补跑） | 通过 | 权威端272-2、权威端274-3 macOS 补跑 | 真实 macOS 授权移除、撤销清理、残留检查、Linux 重选/卸载、桌面引导与命令行等价面通过；浏览器重验证子步分两遍补齐 |
| 权威端274-4 | 第二轮通过 | 2.5 | 第二轮（rc7）：全条通过——四组与同设备提示、重验闸、profile 免重验、删除预览与级联、条目 B 命令行对照 |
| 权威端276-1 | 第二轮通过 | 2.7 | 第二轮（rc7）：备份/导出/双恢复 vault/服务切换/重新配对/恢复后复核全部完成；恢复后的权威端与客户端按 Brief 保留；期间复现欠账第 45 条一次（按提示恢复） |
| 人工272-1 | 部分通过（owner 授权 agent 代跑） | 人工272-1 小节 | 页面引导与无密码入口通过；桌面手动映射被 Windows 错误 1219 拦下（owner 桌面会话已用其他账号连着同一台 NAS），未断开 owner 现有连接，读写与断开两步未完成；owner 未亲眼目验 2026-11-15 owner 处置：桌面映射剩余部分记为本版已知缺口（欠账第 52 条） |
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
| 客户端272-2 | 失败→第三轮通过 | 3 | 第二轮（rc7）：结构修复通过但 Windows POSIX 探测恒失败登记第 47 条。第三轮（rc8）：PowerShell/cmd `exec_context` 均正常返回盘符预览，数组字段为 [] 且盘符注入成功 |
| 客户端272-3 | 第二轮部分通过→第三轮通过 | 3 | 第二轮共享目标写入失败（49）、停止后重跑挂起（50）。第三轮（rc8）：反向本地拷贝与流式写入共享均完成且 SHA-256 一致；停止后同参数重跑约 2 秒完成且哈希一致 |
| 客户端273-1 | 第三轮通过 | 3 | 回环随机端口、一次性启动令牌、安全头/cookie、二次访问 403、只读入口、自动化测试与退出即停全部通过 |
| 客户端273-2 | 第三轮通过 | 3 | 五页面、命令行等价、本机探测、写路由 404、成功/失败同步与旧快照保留、权威端无回传全部通过 |
| 客户端274-1 | 第三轮通过 | 3 | 三视图、平移缩放、同设备合并角标、本机可达/不可达探测、写路由 404、手机视口与权威端无回传全部通过 |
| 客户端274-2 | 第三轮通过 | 3 | 新增入口与写路由缺失、反向视图值裁剪、共享只读、命令行等价全部通过 |
| 客户端275-1 | 第二轮通过（含欠账第 38 条复核） | 3 | 第二轮（rc7）：向导指引/进程退出/角色不变/配对+SAS/二次只读界面/清理全部通过；pair 自动写 role.json |
| 混跑270-1 | 通过 | 第四部分 | 旧客户端遇新权威端，占位符字面量原样写出 |
| 混跑270-2 | 通过 | 第四部分 | 新客户端遇旧权威端，连接前整体拒绝且目标未建文件 |
| 混跑271-1 | 通过 | 第四部分 | 旧客户端、新权威端的兜底锚与旧格式转发拒绝 |
| 混跑271-2 | 通过（附欠账第 55 条） | 第四部分 | 新客户端遇旧权威端拓扑入口降级；新格式锚请求旧权威端解析失败如实登记 |
| 混跑272-1 | 通过（附欠账第 54 条） | 第四部分 | 旧客户端遇新权威端无挂载工具且无盘符注入；多算法主机误报已登记 |
| 混跑272-2 | 通过 | 第四部分 | 新客户端遇旧权威端共享入口降级且不建任务 |
| 混跑275-1 | 通过（附欠账第 53 条） | 第四部分 | 旧客户端快照兼容总览通过；旧客户端与新默认证书配对失败已登记 |
| 混跑275-2 | 通过 | 第四部分 | 权威端 Web UI 对旧客户端显示完整旧版能力标签 |
| 混跑275-3 | 通过 | 第四部分 | 新客户端遇旧权威端降级、快照回退与通用工具全部通过 |
| 混跑275-4 | 通过 | 第四部分 | 客户端本机 Web UI 两个页面均提示先升权威端，推荐版本显示“—” |
| 混跑275-5 | 通过 | 第四部分 | cache status、doctor、update check 三个本机出口提示正确，测试通过 |
| 欠账第 52 条（rc10 复核） | 通过 | 欠账复核（第五轮 rc10） | 桌面手动添加引导注意事项包含 Windows 1219 与 `net use \\<地址> /delete` 处理办法 |
| 欠账第 53 条（rc10 复核） | 通过 | 欠账复核（第五轮 rc10） | v0.21.1 客户端在登记阶段收到升级/设备码指引，不进入待批准列表；rc10 新客户端配对不受影响 |
| 欠账第 55 条（rc10 复核） | 通过 | 欠账复核（第五轮 rc10） | rc10 新客户端首连遇旧权威端 400 后自动退回旧格式落锚并连接成功；刷新后走已锚定路径 |
| serve status（rc10 复核） | 通过 | 欠账复核（第五轮 rc10） | Windows 与 Linux 均不把非 serve 的 sshmgr 进程误报为 serve；真实 serve 显示 running |
| 权威端273-2（第九轮 rc13 复跑） | 通过；册外补充发现第 61 条 | 权威端273-2 复跑（第九轮 rc13） | 真实 Chromium 完整复跑通过；第 58、60 条复核通过；共享存储卸载守卫有效但重验自动回跳丢失目标服务器参数 |

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

【第三轮（rc8）执行记录】通过（第 49 条的幂等补属主映射与第 46 条的非 force 重选切换均复核通过）。
owner 不在场，owner 命令行由 agent 代跑。本轮开始时靶机上的持久挂载行仍是 rc7 写入的版本 14，
缺少数字用户标识与用户组标识映射。

【第 49 条：旧挂载行幂等补映射（rc8）】
```text
靶机事实：共享挂载点在产品 fstab 管理行中地址为真实 NAS 地址，x-sshmgr-version=14；
  旧行无 uid=/gid= 选项；实际 CIFS 挂载显示 uid=0、gid=0、noforceuid、noforcegid；
  挂载点与既有样例文件属主均为 root。服务器条目的 SSH 登录用户数字用户标识=1000、用户组标识=1000。
权威端幂等执行 shares mount <NAS 共享名> <Linux 条目A> →
  已挂载共享存储 <NAS 共享名>；结果：update-success；版本号：16；
  实际网络：e2e278_nas_net（第 1 个地址）；提示：已补充文件属主映射；退出码 0。
复核：fstab 仍指向原真实 NAS 地址（地址未变），新增 uid=1000、gid=1000，
  x-sshmgr-version=16；实际挂载为 uid=1000、forceuid、gid=1000、forcegid。
  挂载点与既有样例文件属主变为 SSH 登录用户（1000:1000）。
  以该 SSH 登录用户直接写入共享 → 文件属主 1000:1000，可读回，SHA-256 留档后删除成功。
```

【第 46 条：非 force 重选切换（rc8）】
```text
测试辅助：沿用第二轮方法，临时启动一次性透明 TCP 转发，把提供方设备的物理局域网成员地址临时指向
  该转发路径；虚拟局域网成员仍指向真实 NAS 地址，形成「当前虚拟地址仍连通、且存在不同且更优的
  物理候选地址」场景。
临时改动物理成员后执行 shares mount <NAS 共享名> <Linux 条目A> →
  「无需改动；版本号：17；实际网络：e2e278_virtual（第 1 个地址）
   提示：存在更优物理局域网地址（网络 e2e278_nas_net）；如需切换请执行 shares reselect … --yes」
  ✓ 仍连通地址未被改写，提示只含网络名。
执行 shares reselect <NAS 共享名> <Linux 条目A> --yes（不加 --force）→
  「已在服务器上重选地址；网络：e2e278_nas_net；版本号：17」；退出码 0 ✓
  不再出现「卸载共享存储挂载点失败」。
切换后：fstab 地址换为临时转发路径、uid/gid 映射保留、版本号 17；实际 CIFS 挂载处于挂载状态；
  以 SSH 登录用户写入/读回/删除测试文件成功。
第二次执行 shares reselect … --yes →
  「当前地址已是最优可达地址，未做改动。」；退出码 0 ✓
```

【第三轮恢复与清理（rc8）】
```text
提供方物理局域网成员地址已恢复为真实 NAS 地址；再次 shares reselect --yes 成功切回原地址，
  版本号联动为 18；现场探测 Linux 条目 A：已挂载、远端配置版本号 18。
以 SSH 登录用户对原地址路径读写测试成功后删除测试文件；一次性透明 TCP 转发与临时文件停止/删除，
  拓扑登记恢复为第三轮开始前的双成员同真实 NAS 地址状态。
```

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

【第三轮（rc8）：第 48 条复核】通过。owner 不在场，命令行由 agent 代跑。
```text
二进制 SHA-256（执行前）=9b64cbea…dd41。
root 执行 sshmgr update --check →
  update base: https://api.github.com
  当前版本: 0.22.0-rc8；最新版本: v0.21.1；资产: sshmgr_0.21.1_linux_amd64.tar.gz
  干跑:未下载、未替换、未重启任何东西
  退出码 0。
非 root 执行同一命令 → 先输出权限导致的角色判定警告（按产品语义），随后同样输出当前/最新/资产
  与「干跑:未下载、未替换、未重启任何东西」；退出码 0。
两次输出均不再出现 rc7 的「⚠ 重启将断开活动隧道…」或「替换已成功,但服务重启未完成(待手工)」；
  上文「未重启任何东西」为否定式干跑结果，不是重启提示。
二进制 SHA-256（执行后）=9b64cbea…dd41，与执行前一致；serve 服务保持 HEALTHY。
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

【第三轮（rc8）执行记录】通过（第 47 条复核通过）。owner 不在场，工具调用由 agent 代跑。
执行前一次性实例先用 rc8 拉取权威端快照（服务器 12、凭据 14），`mcp` 握手报告版本 0.22.0-rc8。

【exec_context（rc8）】
```text
PowerShell 条目 → JSON-RPC 正常 result（无 isError 字段，即 false），退出到调用方无异常；
  structuredContent.planned_network_drives=[{drive:"X",share:<NAS 共享名>,
    address_plan:"执行时探测"}]；
  injected_environment_names=[两条一次性环境变量名]；uninjected_environment_variables=[]；
  uninjected_network_drives=[]。
cmd 条目 → 同样正常 result；planned_network_drives=[{drive:"X",share:<NAS 共享名>,
    address_plan:"执行时探测"}]；uninjected_environment_variables=[]；
  uninjected_network_drives=[]。
核对：不再出现 rc7 的 PowerShell 解析错误、`exec_context probe failed` 或 `server is not in your profile`
（本轮第一次误用条目名代替真实服务器标识时出现该提示；改用 list_servers 返回的标识后不再出现，
该误用操作未触达目标机）。
```

【盘符注入复证（rc8）】
```text
PowerShell 条目执行只输出共享目录计数的命令 → exit_code=0；
  injected_network_drives=[{drive:"X",share:<NAS 共享名>}]；stdout 仅含计数。
cmd 条目执行只输出目录行计数的命令 → exit_code=0；
  injected_network_drives=[{drive:"X",share:<NAS 共享名>}]；stdout 仅含计数。
两条命令均未把目录内容写入证据；认证失败暂停/恢复沿用第二轮结论，rc8 未改动该路径。
```

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

【第三轮（rc8）执行记录】通过（第 49 条的第 3、5 步与第 50 条第 7 步均复核通过）。
执行实例仍按网络事实由 Linux 一次性客户端代替工作机 1 执行 Linux 端点；一次性源文件 20 MiB。
开始时共享目标端目录已按第 49 条修复映射为 SSH 登录用户 1000:1000。owner 不在场，工具调用由 agent 代跑。

【第 3 步：反向本地拷贝写入共享（rc8）】
```text
relay_file from=<Linux 条目A 普通路径> to_share_address=<共享目标> →
  mode=local_copy；restart_after_interruption=true；bytes_total=20971520；space_check=ok。
同一 MCP 会话内轮询到终态：status=done；输出含固定 cp 到 <目标>.sshmgr-partial 与 mv 到真名。
目标真名出现、半成品消失；目标属主=SSH 登录用户（1000:1000）、大小 20971520；
  目标 SHA-256=6f608ea1…f9aa，与源一致 ✓。
```

【第 5 步：流式回退写入共享目标（rc8）】
```text
relay_file from=<执行实例本地文件路径> to_share_address=<共享目标> →
  mode=streaming_relay；自动选择已实际挂载的 Linux 条目 A 作为共享端点。
任务输出：1 个块、20 MiB；chunk 1/1 ok；file_sha256=sha256:6f608ea1…f9aa；
  relay done 后半成品重命名为目标真名；status=done。
目标属主=SSH 登录用户（1000:1000）、大小 20971520；目标 SHA-256 与源一致 ✓；
  半成品与块清单均不存在。
```

【第 7 步：停止后同参数重跑（rc8，第 50 条真机取证）】
```text
第一次任务（任务号 1）：发起共享源 → Linux 本地目标的 local_copy 后立即 exec_stop →
  exec_stop 返回 status=running（停止已触发）；随后 exec_output 终态 status=stopped；
  输出保留「重跑会从文件开头覆盖旧半成品」与固定 cp 命令。
第二次同参数任务（任务号 2）：同一 MCP 会话内发起 → 第一次轮询 status=running；
  约 2 秒后第二次轮询 status=done；输出含 cp 与 mv，真名出现、半成品消失。
最终目标大小 20971520，SHA-256=6f608ea1…f9aa，与共享源逐字节一致 ✓。
补充对照：另发起一个新目标并立即停止，任务进入 stopped；此刻无 cp 进程（进程检查只匹配到
  取证 shell 自身，未计入），该目标没有留下半成品或真名。
```

【第 50 条结论（rc8）】不再复现挂起。rc7 时共享文件在目标 Linux 主机上呈现为 root 所有，
强杀读取/写入链路后同目标重跑 3 分钟无进展；rc8 已把共享挂载映射到 SSH 登录用户，目标端非提权
写入与重命名正常，停止后的同参数重跑约 2 秒完成且哈希一致。按 Brief 的「若不再挂起」分支，
未继续展开不可中断睡眠、内核 CIFS 统计、内核日志、换目标/删半成品/重挂载等逐项取证。

【取证偏离说明】第一轮取证脚本曾在任务仍 running 时过早结束 MCP 进程，导致一个全尺寸半成品未由该次
任务重命名；该半成品已删除后按正确姿势（保持同一 MCP 会话轮询到终态）重跑，不作为产品判定证据。

### 客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI

【结论】移交下一轮。原因：客户端 Web UI 需在演练机 2 的一次性客户端实例上启动回环监听并经端口转发
用浏览器取证；本轮时间窗口已用尽。已具备条件：一次性客户端实例已配对并可拉取快照、
演练机 1 的浏览器可用（客户端回环为纯 HTTP，不涉及证书问题）。
册中要求工作机 1 的条目由 Linux 一次性客户端代替的原因：工作机 1 禁止打开浏览器与触碰桌面，
客户端 Web UI 只监听回环，需经端口转发访问。

【第三轮（rc8）：客户端273-1 回环监听、一次性启动令牌与只读入口】通过（退出即停复核见本节末尾）。
owner 不在场，按授权由 agent 在演练机 1 的真实 Chromium 中代跑；工作机 1 桌面未触碰。
```text
启动输出：网址以 http://127.0.0.1:<随机端口>/ui/start?token= 开头 ✓；仅监听本机回环随机端口 ✓。
首次打开启动网址：HTTP 303 换会话后落到 /ui/，最终页面 200；
  页面显示「客户端 · 只读」与「这里只读，修改需到权威端 Web UI 去做。」✓
  首页无新增、编辑、删除、添加等修改入口。
/ui/ 响应头：内容安全策略包含 default-src 'none' 与 script/style/connect/img/font 'self'、
  base-uri 'none'、form-action 'self'、frame-ancestors 'none'；nosniff、same-origin、no-store 均在。
会话 cookie：名称包含监听端口；HttpOnly=True；SameSite=Strict；Path=/ui/；Secure=False ✓。
同一启动网址第二次访问：HTTP 403，页面「启动链接无效 / 启动链接无效或已使用…重新运行 sshmgr ui」✓。
自动化测试：
go test ./internal/webui -run '^(TestHostValidationRejectsNonLoopbackHosts|TestWriteRouteRequiresMatchingOriginAndCSRFToken|TestCSRFTokenBoundToSession|TestAllWriteMethodsRequireOriginAndCSRFToken|TestSecurityHeadersOnEveryResponse|TestClientRoleShowsReadOnlyNoticeWithoutWriteEntries)$'
→ ok
```
【退出即停（rc8）】四条客户端 Web UI 取证完成后向 UI 进程发送终止信号；进程退出，回环监听消失，
一次性 SSH 端口转发关闭。Chromium 再访问原回环地址 → `net::ERR_CONNECTION_REFUSED`，截图留档 ✓。
【取证辅助偏离】演练机 2 无桌面浏览器，`sshmgr ui` 自动调用 `xdg-open` 失败后会按产品语义退出；
本轮在一次性客户端目录放置一个只睡眠的一次性 `xdg-open` 占位脚本，使 Web UI 进程保持运行。
该脚本不读取参数、不打开浏览器、不改系统 PATH 持久配置，结束后删除。

【第三轮（rc8）：客户端273-2 本地快照、本机探测视图与命令行等价面】通过。owner 不在场，浏览器与
命令行由 agent 代跑。
```text
页面：/ui/、/ui/secrets/、/ui/ops/client、/ui/ops/client-audit、/ui/topology/ 全部 200；
  均保留只读/到权威端修改提示，不显示机密项值。
命令行等价：对 4 个可见服务器条目逐条 servers show --instance、secrets list、cache status、
  cache instances ls、cache audit --limit 100、sites/networks/devices list 共 12 组命令全部退出码 0；
  页面可见对象与命令行本地快照一致。（servers ls 为默认实例命令；本条使用命名实例，产品该子命令
  不带 --instance，故按册中命名实例分支逐条 show。）
本机探测：devices probe --from self <一次性可达设备> → 物理候选 0ms 可达；另一虚拟候选连接超时；
  刷新 /ui/topology/ 后能看到本机起点观察摘要，页面仍只读。
写路由：/ui/access/ 200 且提示 profile、配对、设备码和项目令牌在权威端管理；
  profiles/delete、secrets/new、服务器 edit/delete、ops/backup 均 404。
立即同步：成功路径按钮可见、同步成功；cache status 仍为 4 台服务器 / 5 条凭据。
  失败路径短暂停止演练权威端 serve 后点击同步，页面出现失败信息；返回 /ui/ 时旧快照服务器仍可见；
  随后重启 serve，状态恢复 HEALTHY。
权威端审计对照：probe-connectivity 查询前后均 41 行，一次性 Linux 客户端命中 0 → 客户端观察未回传。
```
【前置数据偏离】既有 Linux 一次性客户端设备码未关联拓扑设备，导致 `--from self` 首次报「没有记录本机设备」。
按 owner 授权在演练权威端把该客户端实例临时关联到 Linux 条目 A 对应的设备，并经缓存客户端自动拉取后重试；
该改动只为本组客户端 Web UI 条目服务，收尾时解除并复核（见第三轮收尾）。

【第三轮（rc8）：客户端274-1 拓扑只读、本机探测与无编辑入口】通过。owner 不在场，真实 Chromium
桌面/手机视口均由 agent 在演练机 1 代跑；真实手机观感仍移交 owner。
```text
只读入口：页面显示「拓扑为只读视图；网络成员的修改请到权威端 Web UI 做。」；
  未归类托盘无「新增」链接；页面无布局保存/重置入口；data-editable 为 false 或未注入 ✓。
三视图：物理/虚拟/叠加按钮均点击成功，画布 data-view 依次切换 ✓。
画布交互：鼠标滚轮缩放后 Cytoscape 画布缩放值变化；拖动画布后平移值变化，交互无错误 ✓。
节点与合并：本地裁剪快照显示站点、网络、设备与坐标；同设备 Linux 条目 A 与一次性条目 B
  在拓扑中合并为一个节点，节点角标 ×2 ✓；点节点可打开设备详情，详情内有本机起点探测表单。
本机探测（UI）：起点固定为本机；对可达目标点击「开始探测」后任务 state=done，
  汇总「目标：✓2ms·不一致」（物理可达，另一候选与既有观察不一致）✓。
  对一次性不可达目标触发后汇总为「不通·不一致」，详情行显示共同物理网络候选连接超时 ✓。
命令行等价：devices list --unassigned 与 devices probe --from self <可达目标> <不可达目标>
  均退出码 0；输出包含可达与连接超时，同读本地快照/本机观察。
写路由：携带同一回环会话 cookie、同源 Origin 与页面防跨站请求伪造令牌 POST
  /ui/api/topology/membership/add → HTTP 404 ✓（未注入写来源，客户端无拓扑写路由）。
手机视口 390×844：无横向溢出，三视图按钮可见，只读提示保留 ✓。
权威端审计对照：probe-connectivity 仍为 41 行，一次性 Linux 客户端命中 0 → 客户端观察未回传。
```
【274-1 一次性前置】为构造「同设备多条目合并」与「共同网络但不可达」的判据，临时新增：
同设备第二条目（复用一次性登录私钥与主机密钥锚）、一台仅在共同物理网络中登记不可达测试地址的设备；
并把既有 Linux 客户端实例临时关联到本机设备。三者仅授权给该一次性 Linux profile，第三轮收尾删除/恢复。
【274-1 一次性实例清理判据】另用一个全新的一次性命名客户端实例完成配对并拉取 4 台服务器 / 5 条凭据，
打开其客户端拓扑页确认只读提示；携带该实例回环会话与防跨站请求伪造令牌 POST 拓扑写路由 → 404。
随后停止其 Web UI，设备码 revoke、pair 项目 revoke+remove，实例 8 个文件逐个删除、8 个空目录逐层 rmdir，
一次性端口转发与日志删除。这满足册中第 5 步「删除一次性命名实例目录」；上文保留的 e2e278-linux 仍按 Brief
继续供 #279 使用。

【第三轮（rc8）：客户端274-2 新增入口缺失与反向视图裁剪】通过。owner 不在场，浏览器与命令行由 agent 代跑。
```text
服务器列表：条目 A 与同设备条目 B 可见；无「新增服务器」「导入 SSH」，页头无「添加」入口 ✓。
直接访问 /ui/servers/new 与 /ui/devices/new → 均 HTTP 404 ✓。
条目 A 详情：机密项组与共享存储组可见；显示「仅显示本客户端可用的部分，修改请到权威端。」；
  主机密钥/锚信息只读可见；普通机密项显示名字与备注，不显示环境变量名、值、哈希、值长度或值版本号 ✓。
  无 profile 授权组、下发记录组、同设备下发文件提示、预览/编辑/删除入口 ✓；
  同设备其他条目带来的共享存储仅作为只读事实提示可见。
共享列表/详情：只显示本客户端可用共享；按钮与链接控件仅有导航、筛选、详情、返回，没有新增、编辑、删除、
  挂载、卸载、撤销、重选、残留检查或桌面手动添加引导 ✓；详情显示挂载密码机密项的名字但无值，
  值长度/哈希/值版本等标记不存在 ✓。
命令行等价：servers show 条目 A、shares list、shares show <共享名> 三组均退出码 0；
  与页面同样只读本地裁剪快照，不显示地址原文以外的账号值、密码值或机密项值。
授权范围外对象：未出现在客户端页面与命令行输出（以既有 profile 外一次性对象名核对）。
```

【第三轮仍移交】
```text
权威端272-2、权威端274-3：仍缺一次性 macOS 受管靶机；生产 macOS 机器按 Brief 禁止使用，未执行。
人工272-1：需要真实 Windows 桌面交互与目验，继续移交 owner。
```
（2026-11-15 补记：人工272-1 已由 agent 按 owner 授权代跑，结论见下一节。）

### 人工272-1 Windows 桌面手动添加引导与盘符可见性目验（agent 代跑）

【结论】部分通过。owner 于 2026-11-15 明确授权由 agent 代跑本人工保留面；owner 未亲眼目验。
期望结果第 1、2 条通过；第 3 条只完成「映射前桌面看不到盘符」，手动映射被 Windows 拦下，读写与断开两步未完成。
执行位置：演练权威端（0.22.0-rc8）回环 Web UI，由演练机 1 上的 Chromium 无界面浏览器打开；桌面部分在工作机 1
owner 登录会话（会话 1，当时处于断开状态）中以「仅交互式」一次性计划任务运行。

【前置数据调整】
```text
首次「显示引导」时，工作机 1 对应拓扑设备与共享存储提供方设备没有共同网络，页面如实显示
「这台电脑与共享存储提供方设备没有共同网络，因此不给出推荐地址。」且不显示映射步骤（行为合理）。
册中前置要求两者同在一个物理局域网，因此在演练权威端把工作机 1 对应设备加入 NAS A 所在的物理局域网
（devices member add，演练数据，生产未触碰），再次「显示引导」。
```

【页面核对（脱敏）】
```text
推荐地址：<网络地址已脱敏>，所属网络=NAS A 所在网络（物理局域网）；附说明「地址按这台电脑与共享存储提供方设备的
  共同网络推算；有共同物理局域网时优先使用物理局域网地址。」 ✓
固定盘符 X:、访问路径 \\<网络地址已脱敏>\<NAS共享名>、目录约定 temp / datasets 两条访问路径、专用挂载账号用户名 ✓
Windows 手动映射三步（映射网络驱动器 → 盘符 X: 与文件夹 → 输入账号与共享密码）✓
等价命令：net use X: \\<网络地址已脱敏>\<NAS共享名> /user:<挂载账号> * /persistent:yes ✓
「命令里的 * 表示运行时输入密码；sshmgr 不显示、复制或下载共享密码。」 ✓
注意事项：笔记本更换网络后映射可能失效；请输入自己保存在密码管理器里的共享密码，本页面不显示、复制或下载
  共享密码，也不生成含密码的脚本 ✓
全部控件：按钮仅「注销」「注销全部会话」「显示引导」；输入框仅两个隐藏的 csrf_token；下拉框 1 个；文本框 0；
  密码输入框 / 显示密码 / 复制密码 / 下载密码 / 下载脚本控件数量均为 0 ✓
页面 HTML 中 NAS 密码命中计数 = 0 ✓
```

【桌面映射（工作机 1 owner 登录会话）】
```text
映射前：盘符 X 不可见；net use 无 X:；凭据管理器中该 NAS 地址已有 1 条 owner 自己的凭据（本次不改动）。
映射：按页面推荐地址与专用挂载账号映射 X:（密码经只有 owner 可读的临时文件传入脚本进程，读取后立即删除，
  未出现在任何进程命令行）→ 失败：
  「不允许一个用户使用一个以上用户名与服务器或共享资源的多重连接。中断与此服务器或共享资源的所有连接，然后再试一次。」
  （Windows 错误 1219：owner 桌面会话已用其他账号连接同一 NAS 地址。）
处置：不断开 owner 现有连接（主力工作机桌面不得改动），读写与断开两步未执行。
结束状态：盘符 X 不可见；net use 无 X:；该 NAS 地址凭据条数仍为 1（与映射前一致）。
```

【清理与指纹】
```text
一次性计划任务 3 个（含上一次尝试遗留）、脚本与密码临时文件已逐个删除；演练机 1、演练机 2 上本条目
传递密码失败留下的临时文件（均为空或已删除）与监听进程已清理。
工作机 1 生产目录指纹：映射前后一致（5 个文件）。
机密值检索：worker 运行日志、页面文本命中计数均为 0。
```

【移交】
```text
期望结果第 3 条的手动映射、读写与断开，需要 owner 在桌面上先断开与该 NAS 的现有连接后亲自执行；
或在没有连接过该 NAS 的另一台 Windows 桌面上执行。页面在 1219 场景下没有提示，已登记欠账第 52 条。
```


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

## 第三轮（rc8）安装与版本

【第三轮执行说明】owner 不在场，按调用方授权由 agent 继续代跑；本轮候选版本 0.22.0-rc8（源提交
`930fd33`），补跑客户端 Web UI 四条并复核第 44–49 条欠账修复、为第 50 条取证。第一、二轮结论与证据全部保留。

【产物哈希核对（本机候选产物目录）】
```text
linux_amd64.tar.gz  SHA-256 9889c0851e328fb15326eafd5878ebb0a78c6f8365ae48acded6d1a32d04ea88 = 与 Brief 给定值一致
darwin_arm64.tar.gz SHA-256 210c452f91e2e76cc6cd388f9b004cc1b394d26ce042ba697a3600543a9e147b = 与 Brief 给定值一致
windows_amd64.zip   SHA-256 a1a27df9e9210996ca9f1f297200eb1c66a21d35d37222c060a5dcad70e56ecd = 与 Brief 给定值一致
```

【升级记录（2026-11-15，经本机临时 HTTP 服务分发）】
```text
一次性演练权威端：下载 linux_amd64.tar.gz 并核对 SHA-256 OK；旧二进制备份为 sshmgr.rc7；
  替换为 rc8 后重启 serve 服务；sshmgr version → 0.22.0-rc8；
  serve status：Running / responding / vault ok / HEALTHY。
Linux 受管服务器 A：下载并核对同一 Linux 产物 SHA-256 OK；旧二进制备份为 sshmgr.rc7；
  替换后 sshmgr version → 0.22.0-rc8。既有共享挂载保留。
工作机 1：下载 windows_amd64.zip，SHA-256 与 Brief 给定值一致，解压到一次性目录 bin-rc8 下；
  bin-rc8\sshmgr.exe version → 0.22.0-rc8。不进 PATH，不覆盖旧候选目录，不触碰生产 sshmgr。
```

【第三轮开始前检查】
```text
三台代称机器版本：一次性演练权威端 / Linux 受管服务器 A / 工作机 1 一次性实例均为 0.22.0-rc8。
一次性演练权威端遗留 Web UI / 缓存客户端进程：0（仅保留应运行的 serve 服务）。
Linux 受管服务器 A 遗留 Web UI / serve / 缓存客户端进程：0；共享挂载仍在。
Windows 受管服务器 A：无 sshmgr 进程。
工作机 1 生产目录指纹（文件数=5；逐文件 SHA-256 按路径排序拼接后再取 SHA-256）=
1be8f02f12488a0c2a440bc62a9265227d440e5a88df8cb2fbd68f5b4319c94d
```

## 欠账复核（第三轮 rc8）

### 欠账第 44 条：`serve cert-info` 反映服务实际证书

【复核结论】通过（默认证书与自定义证书两处判据均实证）。owner 不在场，证书生成、服务安装与
指纹比对由 agent 代跑；指纹证据只留前后缀，不写完整值。

【演练权威端默认证书】
```text
sshmgr serve cert-info → 证书路径=vault 根目录默认 serve 证书；
  证书公钥 SHA-256 指纹=sha256:c87e65f0…2c7d。
openssl s_client 连接本机 serve 服务，取服务端证书公钥并按 DER 编码计算 SHA-256 →
  c87e65f0…2c7d；两者逐字节一致 ✓。服务证书公钥算法为 ECDSA P-256。
```

【演练机 2 一次性自定义证书对照】
```text
一次性 vault 前置：默认 vault 目录为空、serve 服务未安装、无 sshmgr 进程。
openssl 生成一次性 ECDSA P-256 自签证书/私钥 → 以 serve install --tls-cert/--tls-key
  安装一次性 serve 服务；服务 Running / responding / vault ok / HEALTHY。
自定义证书文件公钥 SHA-256=bde824cf…327c。
openssl s_client 从服务端取实际证书并计算公钥 SHA-256=bde824cf…327c。
sshmgr serve cert-info → 证书路径=<一次性自定义证书路径>；
  fingerprint=sha256:bde824cf…327c；并显示「服务使用自定义证书：<一次性自定义证书路径>」✓。
```

【一次性对照清理】
```text
serve uninstall 成功；sshmgr clear 确认删除成功；一次性证书、私钥、管理员口令文件与 clear 输出
  已删除；clear 生成的加密安全网备份已删除。复核：默认 vault 目录为空、sshmgr-serve 为 inactive /
  not-found、无 sshmgr 进程、无服务端口监听。
```

### 欠账第 45 条：同名非配对生成项目的配对错误回传

【复核结论】通过。owner 不在场，owner 命令行与短认证码比对由 agent 代跑；一次性设备名与项目名
均以代称记录，项目令牌只落入 root 专属临时文件，未进入证据。

【冲突构造与第一次 finish】
```text
命令行允许创建 pair- 前缀的普通项目：projects add pair-<一次性设备名> --profile <既有 Linux profile>
  → status=active。该项目不是配对功能生成，未带配对生成标记。
新的一次性客户端实例向演练权威端 pair（带服务证书公钥指纹锚定）；权威端审批面与客户端日志中的
  六位短认证码由 agent 程序化提取并逐字比对一致后批准。
finish 阶段客户端不再出现 HTTP 500，退出时显示：
  「项目 pair-<一次性设备名> 已被占用，且不是配对功能生成的项目，不能自动接管。
   请 owner 在权威端依次执行 sshmgr projects revoke pair-<一次性设备名> 和
   sshmgr projects remove pair-<一次性设备名>，处理后重新配对」
✓ 消息包含项目名；✓ 包含 revoke / remove 两条可执行命令；✓ 为中文可操作错误，不是笼统 500。
```

【按提示恢复并重新配对】
```text
依次执行消息中的 projects revoke pair-<一次性设备名> 与 projects remove pair-<一次性设备名>
  → revoked / deleted。
同一一次性客户端实例重新 pair；再次由 agent 比对两侧短认证码并批准 →
  已授权 profile；pulled 4 servers / 5 credentials；配对完成；
  role show：角色=client、初始化向导=已完成；cache status 显示快照 4 台服务器 / 5 条凭据。
权威端新生成的 pair-<一次性设备名> 项目 status=active；cache-tokens 中一次性设备码 active，
  client_version=0.22.0-rc8。
```

【一次性对象清理】
```text
cache-tokens revoke <一次性设备名> → status=revoked（该命令面无硬删除；revoked 行为终态，不可再拉取）。
projects revoke + projects remove pair-<一次性设备名> 完成；serve.pairing 恢复 false。
一次性客户端实例 7 个文件逐个删除后 7 个空目录逐层 rmdir；配对日志与进程号文件删除。
复核：项目默认列表无该 pair 项目；两个既有已配对客户端仍 active；演练权威端 serve HEALTHY。
```

【取证方法偏离】第一次客户端进程按交互式姿势启动，因自动化驱动未向「确认两侧 SAS 一致？」发送
`y` 而按产品语义中止；随后 agent 先程序化比对两侧短认证码，再用 `SSHMGR_PAIR_ASSUME_SAS=1`
重跑无人值守配对。该环境变量只跳过终端确认，不替代上述比对；第一次中止请求未发放凭据。

### 欠账第 46 条：Linux 非 force 共享地址重选

【复核结论】通过。证据小节：「权威端272-3 / 第三轮（rc8）」。
```text
存在更优且仍可达的物理候选地址时，`shares reselect --yes`（不加 --force）成功从仍连通的虚拟地址切换；
  退出码 0，不再出现「卸载共享存储挂载点失败」。切换后挂载状态可读写，uid/gid 映射保留；
  第二次重选返回「当前地址已是最优可达地址，未做改动」。临时转发与拓扑登记已恢复。
```

### 欠账第 47 条：Windows `exec_context` 盘符预览

【复核结论】通过。证据小节：「客户端272-2 / 第三轮（rc8）」。
```text
PowerShell 与 cmd 条目均不再下发 POSIX 探测；exec_context 正常返回 structuredContent，
  planned_network_drives 列出 <NAS 共享名> 与固定盘符 X，address_plan=执行时探测；
  空数组字段为 [] 而非 null；实际 PowerShell/cmd 命令盘符注入均成功。
```

### 欠账第 48 条：装有 serve 时 `update --check`

【复核结论】通过。证据小节：「权威端273-4 / 第三轮（rc8）：第 48 条复核」。
```text
root 与非 root 均输出干跑结果并以退出码 0 结束；无误导性「重启」确认或「替换已成功」输出；
  二进制 SHA-256 前后一致，serve 保持 HEALTHY。
```

### 欠账第 49 条：共享作为目标端的 Linux 属主映射与写入

【复核结论】通过。证据小节：「权威端272-3 / 第三轮（rc8）」与「客户端272-3 / 第三轮（rc8）」。
```text
rc7 旧 fstab 行幂等补 uid=1000/gid=1000，真实 NAS 地址不变并提示「已补充文件属主映射」；
  挂载点与文件属主为 SSH 登录用户。反向本地拷贝与流式回退写入共享均完成，目标 SHA-256 与源一致。
```

### 欠账第 50 条：停止本地拷贝后同参数重跑

【真机取证结论】不再复现（第 49 条属主映射修复后现象消失）。证据小节：
「客户端272-3 / 第三轮（rc8）」。
```text
停止任务终态 stopped；同参数重跑约 2 秒 done，半成品被覆盖并重命名为真名；
  最终 20 MiB 文件 SHA-256 与共享源一致。按 Brief 的「若不再挂起」分支，未展开内核 CIFS 级取证。
```

## 第三轮（rc8）收尾与机密值检索

【工作机 1 生产目录指纹（第三轮结束前）】
```text
文件数=5
整体 SHA-256=1be8f02f12488a0c2a440bc62a9265227d440e5a88df8cb2fbd68f5b4319c94d
与第三轮开始前完全一致，文件数一致；一次性 rc8 实例未改动生产目录。
```

【第三轮清理】
```text
本机临时 HTTP 服务：已停止，端口无监听。
一次性透明 TCP 转发：已停止；演练权威端 445 端口无监听；提供方拓扑成员地址与挂载地址已恢复。
客户端 Web UI：进程已停止，回环监听消失；演练机 1 → 演练机 2 的一次性 SSH 端口转发已关闭；
  一次性 xdg-open 占位脚本删除。
一次性自定义 serve 对照（演练机 2）：serve uninstall + clear 完成；证书/私钥/口令文件、clear 输出与
  加密安全网备份删除；默认 vault 目录为空，服务 inactive/not-found，无端口监听。
一次性配对冲突实例/项目/设备码：项目已 revoke+remove；设备码已 revoke（产品无硬删除，终态 revoked）；
  客户端实例文件逐个删除、空目录逐层 rmdir；serve.pairing 恢复 false。
客户端274-1 专用一次性命名实例：设备码与 pair 项目 revoke 后，实例文件逐个删除、空目录逐层 rmdir；
  其 Web UI、xdg-open 占位脚本、端口转发与日志均清理。
客户端274-1 一次性同设备条目与不可达设备：已删除；Linux 客户端实例与本机设备的临时关联已解除；
  Linux 客户端最后重新拉取快照（4 台服务器 / 5 条凭据）。
NAS A 本轮一次性子目录与两份 20 MiB 文件：已逐文件删除并移除空目录。
演练机 2 /tmp 与一次性客户端目录中的本轮中继源/目标/半成品、驱动脚本、调用文件、UI 日志：已删除。
演练权威端 /tmp 与浏览器目录中的本轮脚本、输出、审计临时拷贝、证据汇总：已删除。
Windows 受管服务器 A：本轮未写入；无 sshmgr 进程。
```

【按 Brief 保留（供 #278 收尾轮与 #279 使用）】
```text
演练权威端 rc8 serve 服务与恢复出的 vault（HEALTHY；共享前置数据保留，现场版本号 18）。
两个既有已配对客户端实例（e2e278-linux、e2e278-win；cache-tokens 均 active，client_version=0.22.0-rc8）。
一次性配对冲突设备码的 revoked 审计行（产品命令面无硬删除；不可再拉取，不影响两个 active 客户端）。
客户端274-1 专用一次性设备码的 revoked 审计行（同上，不可再拉取）。
演练机 2 的共享挂载与真实 NAS 地址、uid/gid 映射；两台受管机 authorized_keys 中的一次性公钥行。
工作机 1 的 C:\sshmgr-e2e-20261114\（含 rc5/rc7/rc8 三份一次性 exe、隔离目录与既有证据文件）。
```

【第三轮机密值检索（只输出命中计数）】
```text
工作机 1 本轮 7 份工具输出汇总：通用测试值命中 0；NAS 挂载密码命中 0。
演练机 2 本轮中继/客户端证据汇总：通用测试值命中 0；NAS 挂载密码命中 0。
演练权威端浏览器取证文件（逐份）：通用测试值命中 0；NAS 挂载密码命中 0。
本多机运行记录与欠账清单当前内容：通用测试值命中 0；NAS 挂载密码命中 0。
本轮 serve 日志切片：通用测试值命中 0；NAS 挂载密码命中 0。
非零计数文件数：0。
```

【第三轮脱敏 grep】
```text
grep -nE '<第一轮 Brief 中的模式>' 多机运行记录 + 单机运行记录 + 欠账清单
唯一命中：欠账清单第 17 条历史存量行（含默认端口数字样，第一轮已登记的既有内容）。
本轮新增 diff 逐行复检：0 命中。
```

## 第四轮（rc9）安装与版本

【命令原文】
```text
本机核对候选产物 SHA-256；本机临时 HTTP 服务分发 Linux 安装包（演练机经 SSH 会话分块传输，哈希两端核对）；
  Windows 安装包由工作机 1 经本机临时 HTTP 服务下载。
两台演练机：备份 /usr/local/bin/sshmgr 为 sshmgr.rc8 → 解压 rc9 → install 替换；
  演练权威端重启 sshmgr-serve 并查健康。
工作机 1：下载 rc9 安装包（SHA-256 核对）→ 解压到 C:\sshmgr-e2e-20261114\bin-rc9\，不进 PATH、不碰生产。
```
【完整输出】
```text
候选产物 SHA-256（与本机 dist 目录一致）：
  linux_amd64.tar.gz  57041fe4d91b0058b406daca713676705384f46503ea0e1db5eb691d3ad7a64a ✓
  windows_amd64.zip   71774e2fd51c5f661b98937a3c030d8114eef05a809f7eb413aebbe39aee91f5 ✓
演练权威端：/usr/local/bin/sshmgr SHA-256 a9da65d3…5dfde39c，sshmgr.rc8 备份 SHA-256 9b64cbea…8f1dd41；
  0.22.0-rc9，2026-11-15T02:20:41Z 安装；serve status：service Running / process running /
  http responding (401/200) / vault ok / overall HEALTHY；2026-11-15T02:33:06Z 复核仍 HEALTHY。
Linux 受管服务器 A：/usr/local/bin/sshmgr SHA-256 a9da65d3…5dfde39c；0.22.0-rc9，2026-11-15T02:21:52Z。
工作机 1：bin-rc9\sshmgr.exe version=0.22.0-rc9；exe SHA-256 88e0a1d44b4804381faf0a9231c400f52f447564b8bbea4fce5927aa2d8f5660；
  安装包核对后删除；未写 PATH、未装服务、未触碰生产 sshmgr。
升级前三台机器版本：演练权威端 0.22.0-rc8（2026-11-15T02:09:13Z，serve active）、
  Linux 受管服务器 A 0.22.0-rc8（2026-11-15T02:09:13Z）、工作机 1 bin-rc8 0.22.0-rc8（2026-11-15T02:10:55Z）。
```
【执行位置】三台代称机器；本机只做产物哈希核对、临时 HTTP 分发与 SSH 会话内传输。

【工作机 1 生产目录指纹（第四轮开始前）】
```text
文件数=5
整体 SHA-256（逐文件 SHA-256 按路径排序后换行拼接再取 SHA-256）=e0ba15ad06c569c698f1ea5a888cff4f69670ec8a4d9ab7b9f31763de6b2ca9c
```

【遗留一次性进程检查（第四轮开始前）】
```text
演练权威端 / Linux 受管服务器 A：无 sshmgr ui、无临时 systemd 单元、无一次性 mcp 进程
  （pgrep 仅命中查询命令自身）；演练权威端仅保留应保留的 sshmgr-serve 服务。
工作机 1：全部 sshmgr 进程均来自生产路径（用户目录下生产 exe）；无 e2e 一次性实例进程。
```

## 欠账复核（第四轮 rc9）

### 欠账第 42 条：版本提示不再销毁超龄缓存；离线超龄隔离文案不再称服务器拒绝

【结论】通过。owner 不在场，按 owner 授权由 agent 在演练机 2 新建一次性客户端实例代跑复核。

【源码口径（复核前读取）】
```text
internal/clientops/expiry.go / clientops.go：离线上限来源为环境变量 SSHMGR_CACHE_MAX_OFFLINE，
  或实例目录内 cache.config.json 的 max_offline（语法一致，>=1h 才合法，环境变量优先）；
  只读加载 LoadCacheSnapshotReadOnlyFor 只报错不销毁；破坏性加载 LoadCacheSnapshotFor 在
  超龄且二次复核仍超龄时才隔离；隔离前缀由 quarantine.go 按触发源区分（offline limit / server rejection）。
```

【前置构造（演练机 2 一次性实例，不动 e2e278-linux）】
```text
新实例 e2e279-exp42（独立 vault/config/dek 根），配对到演练权威端：
  owner 不在场，agent 比对两侧短认证码（实例名、地址、短认证码逐字一致，SAS-MATCH=True，值不入证据），
  权威端批准一次性 profile（0 台服务器授权）；配对完成后拉取 0 servers / 0 credentials。
构造超龄：cache.config.json max_offline=1h；cache.meta.json pulled_at 回拨 7200 秒（ServerAnchored 保持 true）；
  未改系统时间。此时该隔离配置根下只有这一个命名实例（版本提示的实例推导会选中它）。
```

【命令原文】
```text
sshmgr --help
sshmgr help
sshmgr serve status
sshmgr definitely-not-a-subcommand        （必然失败的命令）
sshmgr cache status --instance e2e279-exp42   （确实加载缓存）
每条后检查：cache.auth.json、cache.bin、解密钥文件是否仍在；是否出现 quarantine/ 目录。
```

【完整输出（关键行）】
```text
after-setup      device-code=1 cache-bin=1 dek=1 quarantine-dir=0
--help        exit=0（输出为帮助文本）      → after-help         device-code=1 cache-bin=1 dek=1 quarantine-dir=0
help          exit=0（输出为帮助文本）      → after-help2        device-code=1 cache-bin=1 dek=1 quarantine-dir=0
serve status  exit=0：service NOT INSTALLED / process not running / http not responding / vault LOCKED / overall DEGRADED
                                      → after-serve-status device-code=1 cache-bin=1 dek=1 quarantine-dir=0
失败命令      exit=1：unknown command "definitely-not-a-subcommand" for "sshmgr"
                                      → after-failing-cmd  device-code=1 cache-bin=1 dek=1 quarantine-dir=0
cache status --instance e2e279-exp42  exit=1：
  cache QUARANTINED because the offline limit was exceeded (snapshot expired (offline beyond SSHMGR_CACHE_MAX_OFFLINE)): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code
  Error: cache quarantined because the offline limit was exceeded — cache snapshot expired (offline 2h4m42s > cap 1h0m0s) — snapshot destroyed; run cache pull to re-enroll
                                      → after-cache-load   device-code=0 cache-bin=0 dek=0 quarantine-dir=1
quarantine/manifest.json：{"state":"done","reason":"snapshot expired (offline beyond SSHMGR_CACHE_MAX_OFFLINE)",…,"steps":{"auth":"ok","bin":"ok","dek":"ok","meta":"ok"}}
```

【判据核对】
```text
--help、help 子命令、serve status、失败命令：四条之后设备码、缓存、解密钥全部仍在，未隔离 ✓
真正加载缓存的 cache status：此时才隔离 ✓
隔离文案含「cache QUARANTINED because the offline limit was exceeded」，全文不含 server rejection 字样 ✓
隔离执行：设备码与解密钥删除、cache.bin 移入 quarantine/、manifest 原因正确 ✓
```

【清理】
```text
演练机 2：一次性实例全部文件逐个删除（含 quarantine 内两份），空目录逐层移除，临时脚本与输出删除；
  ls 确认一次性根目录不存在（instance-cleanup-done）。
演练权威端：cache-tokens revoke e2e279-exp42（status=revoked）、projects revoke pair-e2e279-exp42、
  profiles remove e2e279_exp42、serve.pairing 恢复 false；profiles ls 中该一次性 profile 计数 0
  （cache-tokens ls 保留 revoked 终态审计行，为产品既定行为，不可再拉取）。
两个既有已配对实例与共享前置数据未受影响。
```
【执行位置】演练机 2（一次性实例）+ 演练权威端（配对批准与清理）。

### 欠账第 43 条：serve status 进程探测排除自身，服务未安装时显示 process: not running

【结论】通过（工作机 1 字面输出受生产 sshmgr 进程干扰，详见下文，另以无 sshmgr 进程的
Windows 受管服务器 A 做清洁对照）。owner 不在场，agent 代跑。

【命令原文】
```text
工作机 1：tasklist /FI "IMAGENAME eq sshmgr.exe" /FO CSV /NH（对照取证）；
  隔离环境变量下 C:\sshmgr-e2e-20261114\bin-rc9\sshmgr.exe serve status
Linux 受管服务器 A（未装 serve）：sshmgr serve status
演练权威端（装有 serve）：sudo sshmgr serve status
Windows 受管服务器 A（无任何 sshmgr 进程的清洁对照）：一次性 bin-rc9 exe（隔离环境变量）serve status
```

【完整输出】
```text
工作机 1（服务未安装；取证时已存在 19 个生产 sshmgr.exe 进程，PID 清单略）：
  service: NOT INSTALLED / process: running / http: not responding / vault: LOCKED / overall: DEGRADED，exit=0
  serve status 退出后 sshmgr.exe 进程数=19（与其运行前一致；「running」来自真实存在的生产进程，不是把自身算入）
Linux 受管服务器 A（未装 serve）：
  service: NOT INSTALLED / process: not running / http: not responding / vault: LOCKED / overall: DEGRADED，exit=0
演练权威端（装有 serve）：
  service: Running / process: running / http: responding (401/200 = auth working) / vault: ok / overall: HEALTHY，exit=0
Windows 受管服务器 A（清洁对照，tasklist 无任何 sshmgr.exe）：
  service: NOT INSTALLED / process: not running / http: not responding / vault: LOCKED / overall: DEGRADED，exit=0
```

【判据核对】
```text
Linux（/proc）服务未安装 → process: not running ✓（自身进程号被排除）
Windows（tasklist）在无其他 sshmgr.exe 的机器上服务未安装 → process: not running ✓（自身进程号被排除）
装有 serve 的演练权威端 → process: running ✓
工作机 1 输出 process: running 的解释：该机同时运行 19 个生产 sshmgr.exe（MCP 桥），
  探测按映像名如实报告真实进程；这些进程属于生产，按禁区不得停止。rc6 缺陷场景
  （机器上无任何 sshmgr 进程却显示 running，即把 serve status 自身算入）已由清洁对照证不复现。
```

【清理】
```text
Windows 受管服务器 A：一次性 bin-rc9\sshmgr.exe 删除；顺带删除该机一次性目录中第一轮遗留的两份
  路径取证文件，逐层删除空目录，root-exists=False。
```
【执行位置】工作机 1、Linux 受管服务器 A、演练权威端、Windows 受管服务器 A。

## 第四部分 新旧版本混跑（rc9 对 v0.21.1）

【两端口径】
```text
旧客户端：工作机 1 一次性 v0.21.1 实例 e2e279-old（APPDATA 重定向 + 独立 store/解密钥根，未触碰生产目录）。
新权威端：演练权威端 0.22.0-rc9。
新客户端：工作机 1 一次性 rc9 实例（bin-rc9，独立 vault/config/dek 根）。
旧权威端：一次性 v0.21.1 vault+serve（放置机器与偏离原因见「混跑270-2」小节开头）。
旧客户端取码方式：演练权威端 cache-tokens add 发放一次性设备码（owner 面，agent 代跑），凭码 cache pull；
  佐证：cache-tokens ls 显示该码绑定一次性 profile 且客户端版本列为旧版标签。
```

【混跑发现（配对路径）】
```text
旧客户端 v0.21.1 用 SAS 配对（pair --url --pin）向 rc9 权威端注册时，客户端报
  Error: pairing enroll: bad sig (err=<nil> len=71)。
原因核对：演练权威端现行 serve 证书为 ECDSA P-256（第一册第 36 条修复后的新默认证书，权威端276-1
  恢复后的 vault 重新生成了证书）；v0.21.1 客户端源码硬性要求 enroll 响应签名为 ed25519 原文签名
  （internal/clientops/pairsession.go 校验签名长度 64 与证书公钥类型），收到 ECDSA 签名即失败。
影响：ECDSA 证书的新装权威端无法用配对面接纳 v0.21.1 客户端；owner 发放设备码路径可用（本轮已实证）。
已按欠账登记（第 53 条），是否修复由 owner 裁决；不影响本条其余判据（快照拉取与执行均走设备码路径）。
```

### 混跑270-1 旧客户端遇新权威端：占位符字面量原样写出

【结论】通过。agent 工具面代跑（owner 授权）；真实旧版本二进制 + 真实新权威端 + 真实受管服务器。

【前置数据】
```text
演练权威端一次性机密项 e2e279_mix_secret（通用测试值，只经 root 文件管道录入，未打印）；
  可写入服务器列表=既有一次性 Linux 转发条目（Linux 受管服务器 A 经宿主机转发口）。
旧客户端 e2e279-old 已凭一次性设备码拉取快照（5 servers / 5 credentials）；
  工具清单（v0.21.1）：close_port / download_file / exec_background / exec_command / exec_context /
  exec_output / exec_stop / forward_port / list_servers / relay_file / reload_self /
  update_server_metadata / upload_content / upload_file（无 list_secrets / list_shares / mount_share /
  query_topology / probe_connectivity）。
```

【命令原文】
```text
旧客户端经 mcp --cache --instance e2e279-old：
  upload_content {server_id=<Linux 转发条目标识符>, remote_path=/home/<Linux 受管服务器 A 用户>/e2e279-mix/mix270-1.txt,
    content="mixed270 A={{sshmgr-secret:e2e279_mix_secret}}\n"}
  exec_command {同条目, command=cat /home/<Linux 受管服务器 A 用户>/e2e279-mix/mix270-1.txt}
远端（Linux 受管服务器 A）：sha256sum /home/<Linux 受管服务器 A 用户>/e2e279-mix/mix270-1.txt
本机：对同一模板文本计算 SHA-256。
权威端：sshmgr secrets records e2e279_mix_secret；sshmgr cache-tokens ls。
```

【完整输出（关键行）】
```text
upload_content → {"bytes":47}（无 masked 字段，无值版本号，无文件哈希字段）
cat → exit_code=0，stdout="mixed270 A={{sshmgr-secret:e2e279_mix_secret}}\n"，stdout_bytes=47
远端 SHA-256 = 00ceb7e66d8fd67a117d178bbfcca10ad378269dcab252e56dddd6cab00b4a49
本机预期模板 SHA-256 = 00ceb7e66d8fd67a117d178bbfcca10ad378269dcab252e56dddd6cab00b4a49 → 一致 ✓
权威端 secrets records e2e279_mix_secret →「没有符合条件的下发记录。」（本次字面量写出不产生下发记录）✓
cache-tokens ls → e2e279-old status=active … client_version=旧版客户端:不支持机密项、网络拓扑、共享存储 ✓
旧客户端本地 cache-audit.log → upload-content 与 exec 行；命令字段只含原文与路径；无值，无 masked 字段 ✓
机密值检索（证据汇总拷回演练权威端后 grep -c -F -f）：通用测试值 0；NAS 挂载密码 0 ✓
```
【执行位置】工作机 1（旧客户端 v0.21.1 一次性实例）→ Linux 受管服务器 A（经转发条目）；权威端命令在演练权威端。

### 混跑271-1 旧客户端、新权威端的兜底锚与旧格式转发拒绝

【结论】通过。agent 代跑（owner 授权）。真实旧版本二进制、真实新权威端、真实兜底主机路径。

【前置数据】
```text
条目 A=e2e279_linux_a（Linux 受管服务器 A 经宿主机转发口，专用一次性私钥）：
  先由一次性 rc9 实例（e2e279-anch）首次连接，锚以 source=forward 落库
  （fingerprint=SHA256:D0A1…Jwbk，key-type=ecdsa-sha2-nistp256，handshake=兜底主机:端口）。
条目 B=e2e279_linux_b：未锚定、兜底主机可达。
  （前置调整如实记录：条目 B 最初与条目 A 同兜底地址，旧客户端按 host:port 复用了 A 的锚并直连成功；
  为构造「未锚定的不同目标」，把条目 B 改指向演练权威端 SSH 的转发地址（可达、快照内无该地址锚），
  并把一次性公钥按册内做法追加到该机 authorized_keys，行尾带一次性注释。）
旧客户端 e2e279-old 重新 cache pull（5 servers / 5 credentials）。
```

【命令原文】
```text
旧客户端经 mcp --cache --instance e2e279-old：
  exec_command {条目 A, echo entryA-ok; uname -s}
  exec_command {条目 B, echo entryB-at-authority-machine}
旧客户端命令行（册中原文形态）：sshmgr ssh <条目A> "echo via-owner-ssh"
权威端：journalctl -u sshmgr-serve 中 /pin-hostkey 状态行；servers pin-hostkey <条目B>；sshd 日志 preauth 行。
源码测试：go test ./internal/mcpserver -run '^TestPinHostkey_OldFormatRejected400$'
```

【完整输出（关键行）】
```text
条目 A → exit_code=0，stdout="entryA-ok\nLinux\n"（只用兜底主机连接，旧格式兜底锚校验通过）✓
条目 B → isError=true：
  ssh dial: ssh: handshake failed: pin forward for [REDACTED]: broker returned 400 —
  old-format pin forwarding request: no server entry id — upgrade the client, then retry ✓
权威端服务日志：pin-hostkey entry="" handshake="" -> 400 (device "-") ✓（旧格式请求缺少条目标识，未被猜测归属）
条目 B 锚查询：no pin for e2e279_linux_b (unanchored) ✓（权威端没有为条目 B 落锚）
演练权威端 sshd 日志：Connection closed by <来源地址已脱敏> port <端口已脱敏> [preauth] ✓（登录凭据未发送）
源码测试 → ok  ssh-manager-mcp/internal/mcpserver ✓
旧客户端命令行 sshmgr ssh <条目A> … → exit=1，「vault locked: run `sshmgr unlock`」：
  v0.21.1 的 `ssh` 子命令属 owner 权威库路径，不读客户端缓存（源码核对 internal/cli/ssh.go 直开 vault）；
  旧客户端的缓存执行面是 `mcp --cache` 工具面，本条判据以工具面实证，命令行形态如实记录。
```
【执行位置】工作机 1（旧客户端）→ Linux 受管服务器 A（条目 A）与演练权威端 SSH 转发地址（条目 B 兜底靶）；
服务日志与锚查询在演练权威端；源码测试在本仓源码根目录（候选提交）。

### 混跑272-1 旧客户端遇新权威端的无挂载与无盘符注入

【结论】通过。agent 工具面代跑；Windows 相关判据在工作机 1 上取证。

【前置数据与一处如实记录的调整】
```text
共享前置的 PowerShell / cmd 两条一次性 Windows 条目授权给旧客户端的一次性 profile；
  演练权威端另登记一次性共享 e2e279_dl（盘符 Y，挂载密码引用一次性机密项 e2e279_nas_pass，
  该机密项可写入服务器=这两条 Windows 条目；共享提供方设备与地址沿用既有一次性登记）。
前置调整：旧客户端 v0.21.1 对多算法主机按默认顺序协商（先 ECDSA），而共享条目锚为 ED25519 指纹，
  首次试连报 host key mismatch（可能中间人）。为完成本条判据，把两条共享条目的锚以 --force 临时
  改为同一主机的 ECDSA 指纹（带 key-type，源码带外核对），旧客户端重新拉取后可连；本条完成后
  已按原指纹恢复 ED25519 锚并留证。该现象是旧客户端不按锚算法收窄协商的已知旧版限制，
  已一并写入欠账第 54 条。
```

【命令原文】
```text
旧客户端：cache pull；tools/list 枚举；
  exec_command {PowerShell 条目, Write-Output ps-ok}
  exec_command {PowerShell 条目, Get-ChildItem Y:\e2e279}
  exec_command {cmd 条目, dir Y:\e2e279}
  （list_shares / mount_share：工具清单中不存在，无法调用——如实记录）
```

【完整输出（关键行）】
```text
cache pull → pulled 5 servers / 5 credentials ✓（新权威端快照携带共享段，旧客户端拉取不失败）
工具清单（v0.21.1，见混跑270-1）：无 list_shares、无 mount_share、无 query_topology ✓
PowerShell 只读命令 → exit_code=0，stdout="ps-ok" ✓；返回无 injected_network_drives 字段、
  无共享注入提示、无映射警告 ✓（普通 SSH 命令仍走兜底主机）
Get-ChildItem Y:\e2e279 → exit_code=1，DriveNotFoundException「找不到驱动器…Y…不存在」✓（盘符未注入，按系统正常报错）
dir Y:\e2e279（cmd 条目）→ exit_code=1，同样报告驱动器 Y 不存在 ✓
旧客户端本地审计：只有 exec 行，无共享名、无地址、无账号、无密码 ✓；全程未向命令送入共享存储密码 ✓
```
【执行位置】工作机 1（旧客户端 v0.21.1 一次性实例）→ Windows 受管服务器 A（PowerShell 与 cmd 条目）。

### 混跑275-1 旧客户端遇新权威端的快照兼容总览

【结论】通过。agent 工具面代跑；真实旧版本二进制对真实新权威端完成拉取与执行。

【两端版本与功能起始支持版本表核对】
```text
旧客户端=0.21.1（工作机 1 一次性实例，安装包 SHA-256 与 v0.21.1 正式发布 checksums.txt 核对一致）；
新权威端=0.22.0-rc9（源提交 b0ecdc2 构建产物，SHA-256 核对一致）。
功能起始支持版本表（源码 internal/clientfeatures 集中配置）：机密项 / 网络拓扑 / 共享存储三项的
  起始支持版本均为 v0.22.0；v0.21.1 低于三项起始版本 → 旧版标签应列全部三项，与实测标签逐字一致 ✓。
```

【执行与判据（含被引用条目汇总）】
```text
旧客户端 cache pull → pulled 5 servers / 5 credentials（重复多次，含共享段与拓扑段在场时均不失败）✓
普通命令：一次性 Linux 转发条目 exec_command → exit_code=0，stdout="mix2751-ok\n/home/<Linux 用户>\n"（走兜底主机）✓
权威端 cache-tokens ls：未声明版本设备码显示「旧版客户端:不支持机密项、网络拓扑、共享存储」（完整标签）✓
快照裁剪：旧客户端快照内无 e2e279_mix_secret / e2e279_nas_pass（list 工具不存在且写入只出字面量，
  见混跑270-1）；共享存储其余字段照常下发（cache pull 不失败），挂载密码随机密项消失 ✓
占位符字面量 / 无挂载工具 / Windows 无网络盘符注入 / 旧格式兜底锚与 400 拒绝：
  分别由 混跑270-1 / 混跑272-1 / 混跑271-1 按册完成 ✓（证据编号见各小节）
源码测试：
  go test ./internal/store -run '^(TestExportSnapshotForProfile_ScopesSecrets|TestExportSnapshotForProfileScopesSharedStorages)$' → ok ✓
  go test ./internal/cli -run '^TestCacheVersionE2E_SnapshotCarriesRecommendedVersion$' → ok ✓
```
【执行位置】工作机 1（旧客户端）与演练权威端；源码测试在候选源码根目录（本仓工作树，源提交与候选一致）。

### 混跑275-2 权威端 Web UI 的旧客户端标签

【结论】通过。owner 不在场，agent 以真实浏览器页面文本代证（演练权威端本机无界面浏览器打开权威端 Web UI，
管理员口令经 root 专属文件直接读入脚本，未进入证据）。

【命令原文】
```text
浏览器（agent 代跑）：登录权威端 Web UI → 打开 /ui/access/device-codes → 保存页面文本；
  用 rc9 对照实例 e2e279-anch 拉取一次 → 刷新；再用 v0.21.1 旧客户端拉取一次 → 刷新。
对照设备码与旧设备码均绑定一次性 profile（e2e279_old）。
```

【完整输出（页面相关行）】
```text
旧客户端拉取后（初始与最终状态一致）：
  e2e279-old  <设备码前缀已脱敏>…  活跃  e2e279_old  2026-11-15 03:34:32  旧版客户端:不支持机密项、网络拓扑、共享存储
  → 不是「未声明」，也不只提示机密项；完整标签 ✓
rc9 对照实例拉取后：
  e2e279-anch  <设备码前缀已脱敏>…  活跃  e2e279_old  2026-11-15 03:34:18  0.22.0-rc9
  → 只显示版本号，不带旧版标签 ✓；旧客户端随后再拉取，其行立即回到未声明状态与完整旧版标签 ✓
页面不显示完整设备码（列表仅显示前缀 + 省略号）✓
（过程补充：rc9 对照实例的一次手工 cache pull 误用项目令牌导致设备码被拒并隔离，属操作失误；
  按 owner 面流程撤销旧码、重新发放设备码并重拉成功，未用作产品判据，如实记录。）
```
【执行位置】演练权威端（浏览器与页面）；两次拉取分别由工作机 1 上的 rc9 一次性实例与 v0.21.1 旧客户端执行。

### 混跑270-2 新客户端遇旧权威端：连接前整体拒绝

【结论】通过。agent 工具面代跑。

【旧权威端放置与偏离说明】
```text
旧权威端 = 演练权威端所在机器上的一次性 v0.21.1 vault + 前台 serve（独立 store/证书/日志/解密钥根，
  全部经环境变量重定向到一次性目录；服务端口沿用宿主机既有转发口；v0.21.1 安装包 SHA-256 与
  正式发布 checksums.txt 核对一致；exe SHA-256 e29a99b7…c047f201）。
偏离册的原因：册与 Brief 默认把一次性旧权威端放在工作机 1。核对 v0.21.1 源码：Windows 版 vault 根
  硬编码 C:\ProgramData，完整隔离需要 6 个测试级环境变量接缝（store/主密钥/证书/私钥/日志/解密钥目录），
  任一路径遗漏都会写入工作机 1 的生产目录；且演练机 2（Brief 给出的备选）除 SSH 转发口外对工作机 1
  不可达。为不冒生产写入风险、又保持新客户端仍按册在工作机 1 上取证，选择在演练权威端机器上起一次性
  v0.21.1 serve（期间 rc9 serve 临时停止，结束后恢复并复核健康），复用宿主机既有 HTTP 转发口。
```

【命令原文】
```text
工作机 1 一次性 rc9 实例 e2e279-new（已配对旧权威端并拉取 1 servers / 1 credentials）：
  list_secrets {}
  upload_content {server=<旧权威端一次性 Linux 条目>, remote_path=/home/<Linux 受管服务器 A 用户>/e2e279-mix/mix2702.txt,
    content="A={{sshmgr-secret:e2e279_never}} B={{sshmgr-secret:bad-name}}\n"}（一合法 + 一格式错误占位符）
Linux 受管服务器 A：检查目标路径是否存在。
```

【完整输出（关键行）】
```text
配对与首拉（rc9 客户端 → v0.21.1 权威端）：SAS 双侧 agent 比对一致（SAS-MATCH=True，值不入证据）；
  pulled 1 servers / 1 credentials；拉取结束即出现「客户端比权威端新，建议先升级权威端」。
list_secrets → 「没有可用的机密项」（不报传输错误；结果前附「客户端比权威端新…」提示）✓
upload_content → isError=true，固定错误「权威端版本过旧，不支持机密项，请先升级权威端」
  （不是名字不存在，也不是只报格式错误；返回 bytes=0）✓
目标路径检查 → TARGET-NOT-CREATED（一个字节都没传给目标服务器）✓
MCP 会话标准错误：snapshot poll: authority has no snapshot version endpoint; stopping background polling
  （修订号接口 404 → 停止轮询的证据，另见混跑275-3）
客户端 cache audit --instance e2e279-new --limit 0：
  upload-content denied … secrets=e2e279_never,bad-name; delivery-record=（空）→ 本地审计有拒绝行、无值 ✓
本条无机密项值进入任何输出（旧权威端快照本不含机密项）；无 masked 字段 ✓
```
【执行位置】工作机 1（rc9 一次性实例）→ Linux 受管服务器 A（经转发条目，未实际连接）；旧权威端在演练权威端机器。

### 混跑271-2 新客户端遇旧权威端的拓扑入口降级

【结论】通过。agent 工具面代跑。

【命令原文】
```text
rc9 一次性实例 e2e279-new（快照来自 v0.21.1 旧权威端，1 台授权 Linux 条目）：
  query_topology {from="self"}
  probe_connectivity {from="self", targets=[<授权条目标识符>:22]}
  sshmgr sites list --instance e2e279-new；sshmgr devices list --instance e2e279-new
  exec_command {授权条目, echo normal-exec-ok; uname -s}
```

【完整输出（关键行）】
```text
query_topology from=self → 「权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端」（固定错误）✓
probe_connectivity from=self targets=[<授权条目>:22] → 同一固定错误，不误报拓扑缺失 ✓
sites list / devices list（命令行）→ 同一固定错误，退出码 1 ✓
exec_command（普通命令）→ exit_code=0，stdout="normal-exec-ok\nLinux\n" ✓（拓扑入口降级不影响普通执行）
MCP 会话标准错误同步出现：snapshot poll: authority has no snapshot version endpoint; stopping background polling
前置如实记录：旧权威端一次性条目最初未锚定时，rc9 客户端首连会按新格式转发锚请求，
  v0.21.1 权威端无法解析（400 unparseable key_blob）。按真实升级场景在旧权威端用 owner 面
  `servers pin-hostkey --fingerprint` 带外落锚后重拉，普通命令即恢复——该差异是旧权威端
  不识别新格式转发请求所致，与拓扑入口降级判据无关，已并入欠账第 55 条登记。
（操作失误补记：一次手工 cache pull 误用项目令牌导致设备码被拒并隔离，属操作失误；按 owner 面
  流程撤销旧码、重新发放设备码并重拉成功，未用作产品判据。）
```
【执行位置】工作机 1（rc9 一次性实例）→ Linux 受管服务器 A（经转发条目）；旧权威端在演练权威端机器。

### 混跑272-2 新客户端遇旧权威端的共享入口降级

【结论】通过。agent 工具面代跑。

【命令原文】
```text
list_shares {}；mount_share {server_id=<授权条目>, share=e2e279_dl}（共享地址仅用于入口报错检查）；
query_topology {from=<授权条目>, to=[smb://e2e279_dl]}；
relay_file {from_share_address=smb://e2e279_dl/mix2722.txt, to_server_id=<授权条目>, to_path=/home/<Linux 受管服务器 A 用户>/e2e279-mix/mix2722-relay.txt}；
exec_context {server_id=<授权条目>}；exec_command {同条目, echo exec-without-drive}；
命令行：sshmgr shares list / shares show e2e279_dl / update --check（均带 --instance e2e279-new）。
```

【完整输出（关键行）】
```text
list_shares → 「权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端」✓
mount_share → 同一固定错误（isError=true，mount_point 为空）✓
共享地址翻译的 query_topology → 同一固定错误 ✓
共享地址端点的 relay_file → 同一固定错误（isError=true，task_id 为空，未建任务）✓
exec_context → planned_network_drives=[]，network_drive_preview_note=
  「权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端」✓（盘符预览说明权威端过旧）
exec_command → exit_code=0，stdout="exec-without-drive\n"，返回无 injected_network_drives 字段 ✓（不注入盘符）
命令行 shares list / shares show → 同一固定错误，退出码 1，不误报「共享不存在」✓
update --check → 「检查失败：无法确定推荐版本，请先升级权威端，或用 `--version` 指定」，退出码 1
  → 不把缺少推荐版本当成正常最新状态，也不默认升级客户端 ✓
```
【执行位置】工作机 1（rc9 一次性实例）；目标端点检查在 Linux 受管服务器 A（未实际连接/未建任务）。

### 混跑275-3 新客户端遇旧权威端的降级、快照刷新回退与通用工具

【结论】通过。agent 工具面代跑；通用工具全部经兜底主机工作。

【命令原文】
```text
记录两端版本；确认快照无推荐版本字段、修订号查询接口 404。
同一 MCP 会话（mcp --cache --instance e2e279-new --cache-max-age 1m）：启动后等待 65 秒使快照过期，
  依次三次 list_servers；保存三次返回与标准错误。
通用工具（一次性靶子，执行后清理）：exec_command、exec_background、exec_output、exec_stop、
  upload_file、download_file、forward_port、close_port、relay_file、list_servers。
命令行补口：networks list / devices show / devices probe（均 --instance e2e279-new）。
源码测试：TestMaybeLazyPull_Version404FallsBackToFullPull；
  TestSnapshotPollE2E_OldAuthorityStopsPolling；TestCacheVersionE2E_ToolHintComparesVersions。
```

【完整输出（关键行）】
```text
两端版本：客户端 0.22.0-rc9；旧权威端 0.21.1（快照无 recommended_version 字段——列表输出与提示均证）。
修订号查询 404：MCP 会话标准错误「snapshot poll: authority has no snapshot version endpoint;
  stopping background polling」（v0.21.1 服务端本身不记录该查询，如实说明；停止轮询由客户端行与测试代证）。
快照过期会话：等待 65 秒后三次 list_servers 全部正常返回同一台授权条目；
  标准错误出现两次「pulled 1 servers / 1 credentials」——到期后回退为全量拉取并继续使用 ✓
  （404 → 全量拉取回退由 TestMaybeLazyPull_Version404FallsBackToFullPull 代证 → ok）。
通用工具：
  exec_command → exit_code=0（stdout=normal-exec-ok）✓
  upload_file（26 字节一次性文件）→ {"bytes":26,"files":1}；远端 SHA-256=81b2c319…F7F8D7 与本地一致 ✓
  download_file → {"bytes":26,"content":"generic-tools-payload-2753"} ✓
  exec_background（sleep 30）→ status=running，返回任务标识 ✓
  exec_output → status=running，stdout/stderr 计数为 0（任务尚在睡）✓
  exec_stop → {"status":"running"}（停止已发起，任务按产品语义稍后转 stopped）✓
  forward_port → listen_host=127.0.0.1，本地端口与隧道标识返回 ✓
  close_port → 「closed」✓
  relay_file（同一条目两端常规路径，流式中继）→ task_id 返回；完成后目标
    /home/<Linux 受管服务器 A 用户>/e2e279-gen/upload-relay.txt SHA-256=81b2c319…F7F8D7 与源一致 ✓
  list_servers → 正常返回 ✓
命令行补口：networks list / devices show / devices probe → 均为固定错误
  「权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端」，退出码 1 ✓
源码测试 → 三组全部 ok ✓
（会话提示：工具返回附「客户端比权威端新，建议先升级权威端」，首次出现后同一会话不重复刷屏。）
```
【清理】Linux 受管服务器 A 一次性目录与两份 26 字节文件已删除（gen-files-clean）；
工作机 1 侧上传源/下载副本文件随收尾删除。
【执行位置】工作机 1（rc9 一次性实例）→ Linux 受管服务器 A；旧权威端在演练权威端机器；源码测试在候选源码根目录。


### 混跑275-4 新客户端本机 Web UI 的先升权威端提示

【结论】通过。owner 不在场，按 owner 授权由 agent 以本机 HTTP 页面文本代证；未打开工作机 1 桌面浏览器。

【现场盘点与本条前置】
```text
旧权威端仍在演练权威端机器上运行：一次性 v0.21.1 serve 进程监听原服务端口；rc9 正式服务此时处于停止状态。
新版一次性客户端实例 e2e279-new 仍在工作机 1，cache status 的 source 指向该旧权威端（真实地址不入证据）。
本地快照无推荐版本：本条客户端状态页“推荐版本”实际显示“—”。
客户端二进制输出 0.22.0-rc9；源码版本解析层接受该预发布号，且它不是开发构建默认号 dev，
  因此不会被“开发构建不提示”分支挡掉。若最终发布认为“正式版本号”只能是去掉 rc 后缀的稳定号，
  以最终发布产物重跑；本条按候选产物实际版本号如实记录。
```

【命令原文】
```text
C:\sshmgr-e2e-20261114\bin-rc9\sshmgr.exe ui --instance e2e279-new
```

【代证方式】
```text
工作机 1 上由 PowerShell 启动上述一次性 exe，并把子进程 PATH 限制到一次性目录，避免 ui 命令调用系统
浏览器打开 owner 桌面。随后在同一台机器上用 curl 访问回环启动网址换取会话，再抓取首页与
/ui/ops/client 页面文本；启动令牌、会话 cookie、真实权威端地址、证书公钥指纹与跨站请求伪造令牌
均只留在进程内存或临时 cookie 文件中，未写入证据。抓取后关闭 ui 子进程标准输入，让命令自然退出。
```

【完整输出（脱敏后的页面文本）】
```text
ui-started=True
startup-exchange=303; curl 会话 cookie=1
home-status=200
home-version-hint=客户端比权威端新，建议先升级权威端
home-readonly-notice=这里只读，修改需到权威端 Web UI 去做。
client-status-page=200
client-recommended-version=—
client-status-version-hint=客户端比权威端新，建议先升级权威端
ui-exit=0
after-close=connection-refused-or-listener-closed
```

【判据核对】
```text
客户端首页与客户端状态页均显示“客户端比权威端新，建议先升级权威端” ✓
推荐版本为空时显示“—”，不是正常同版，也不是本地缓存损坏 ✓
命令关闭后回环页面连接失败，页面不可访问 ✓
启动令牌、cookie、设备码、项目令牌与真实地址未进入证据 ✓
```
【执行位置】工作机 1（rc9 一次性实例）；旧权威端在演练权威端机器。

### 混跑275-5 新客户端命令行与 doctor 的先升权威端提示

【结论】通过。owner 命令行面由 agent 按授权代跑；三个本机出口真实执行，源码测试全部通过。

【命令原文】
```text
C:\sshmgr-e2e-20261114\bin-rc9\sshmgr.exe cache status --instance e2e279-new
C:\sshmgr-e2e-20261114\bin-rc9\sshmgr.exe doctor
C:\sshmgr-e2e-20261114\bin-rc9\sshmgr.exe update --check --instance e2e279-new
```

【完整输出（真实地址已替换为代称）】
```text
=== cache status ===
cache:    <一次性客户端根>\config279\ssh-manager\instances\e2e279-new\cache.bin
age:      11m7s
servers:  1
creds:    1
scope:    e2e279_oa_prof
device:    e2e279-new
source:   <旧权威端真实地址>
exit=0
标准错误：客户端比权威端新，建议先升级权威端

=== doctor ===
sshmgr doctor (0.22.0-rc9)
env:  INFO  SSHMGR_* env overrides in effect: SSHMGR_CACHE_DEK_DIR (values not shown)
role:  PASS  role=client setup_complete=true
store:  INFO  store.db absent on a client machine — cache-only is normal
masterkey:  INFO  master.key absent on a client machine — no local vault to unlock
vault-open:  INFO  skipped — store.db/master.key not both present
store-sidecar:  INFO  skipped — no store.db on this machine
serve-cert:  INFO  serve not in use (no serve cert and no init marker)
serve-svc:  INFO  serve service not installed (serve not in use)
client-cache:  INFO  no default-instance cache.bin — 1 named instance(s) hold the offline cache (diagnosed below)
client-versions:  INFO  客户端版本 0.22.0-rc9；推荐版本 权威端过旧，未提供推荐版本；客户端比权威端新，建议先升级权威端
client-cache[e2e279-new]:  PASS  instance e2e279-new: cache.bin present (age 11m0s)
update-gen:  INFO  no on-disk generation signal (no sshmgr update has replaced this binary yet)
overall: 0 WARN, 0 FAIL
exit=0
标准错误：（空）

=== update --check ===
update base: https://api.github.com
exit=1
Error: 检查失败：无法确定推荐版本，请先升级权威端，或用 `--version` 指定
Usage:
  sshmgr update [flags]

Flags:
      --check             dry run: report current/latest/asset/update base, change nothing
      --file string       install from a local archive (.zip/.tar.gz/.tgz) instead of GitHub discovery
  -h, --help              help for update
      --instance string   read the recommended version from this named cache instance (default slot when omitted)
      --no-verify         skip file integrity check (explicit trust in this file only)
      --sha256 string     expected sha256 of --file (explicit trust in this file only)
      --version string    install this release tag (e.g. v0.13.0) instead of latest; downgrade = rollback channel
      --yes               assume yes at confirmation points (required on non-TTY stdin)

检查失败：无法确定推荐版本，请先升级权威端，或用 `--version` 指定
```

【doctor 隔离说明】
```text
doctor 在同一 PowerShell 进程中沿用一次性 vault/config/dek 环境变量；输出只发现一次性命名实例
e2e279-new，未读取或改动工作机 1 生产目录。
```

【源码测试】
```text
命令：go test ./internal/cli -run '^(TestRootCommandWritesNewerClientHintForOldAuthority|TestDoctorShowsOldAuthorityRecommendedVersion|TestClientWebUIShowsNewerClientHintForOldAuthority)$'
Go 工具链：go1.25.8 darwin/arm64（与 Brief 给定版本一致）
源码口径：本仓库当前提交只比候选源提交 b0ecdc2 多文档提交，源码相同；测试在仓库根目录执行。
输出：ok  	ssh-manager-mcp/internal/cli	0.598s，退出码 0
```

【判据核对】
```text
cache status 标准错误末尾出现“客户端比权威端新，建议先升级权威端” ✓
doctor 显示“权威端过旧，未提供推荐版本”并给出同一升级顺序提示 ✓
update --check 退出码 1，未把缺少推荐版本当作正常最新状态，未下载或升级客户端 ✓
四个版本提示出口由混跑275-3、本条与混跑275-4共同覆盖 ✓
```
【执行位置】工作机 1（rc9 一次性实例）；源码测试在候选源码根目录。

## 第四部分 收尾清理

【结论】全部清理完成；应保留对象仍在，演练权威端 rc9 服务恢复为 HEALTHY，工作机 1 生产目录指纹前后一致。

【旧权威端停止与 rc9 服务恢复】
```text
old-authority-stop pid-present
old-authority-stopped
rc9-service=active
service:   Running
process:   running
http:      responding (401/200 = auth working)
vault:     ok
overall:   HEALTHY
0.22.0-rc9
```

【演练权威端机器清理】
```text
一次性 v0.21.1 vault 中的 8 个文件逐个删除：
  .serve-cert-initialized、master.key.plain、serve-cert.pem、serve-key.pem、
  serve-stdout.log、serve.log、serve.pid、store.db；
随后逐层移除 config、dek 与一次性 vault 根目录。
旧版本 Linux 二进制 /usr/local/bin/sshmgr-v0211 删除。
旧版本下载目录为空，确认不存在。
root 专属一次性密钥、项目令牌文本与设备码文本 4 个文件删除；/tmp 下 2 个一次性脚本删除。
清理后：旧权威端根目录不存在；旧版本二进制不存在；一次性泄漏检查目录不存在；rc9 服务 active。
主 vault 中两个一次性拓扑设备 e2e279_linux_a / e2e279_linux_b 删除，删除预览均为“无关联条目、
  无共享、无机密项、无网络成员”，产品返回已删除。
profiles、servers、secrets、shares、networks、devices、projects 列表查询 e2e279 命中 0。
cache-tokens 仅保留产品不可删除的 revoked 审计行（e2e279-anch、e2e279-exp42、e2e279-old，
  均无 profile、不可再拉取；旧权威端自己的 e2e279-new 设备码随一次性 vault 删除）。
```

【工作机 1 清理（每个文件单独删除，不用递归删除）】
```text
删除对象：bin-v0211、appdata-v0211、dek-v0211、v0211-store、vault279、config279、dek279、work\r4。
实际在场：bin-v0211 2 个文件；vault279 0 个文件；config279 6 个文件；
  dek279 1 个文件；work\r4 57 个文件；其余旧客户端目录在前一轮已清空。
共删除 66 个文件。清单如下（<一次性根目录>=C:\sshmgr-e2e-20261114）：
  bin-v0211\README.md、bin-v0211\sshmgr.exe；
  config279\ssh-manager\instances\e2e279-new\cache.auth.json、cache.bin、cache.meta.json、
    cache-audit.log、software-inventory-collect.lock，config279\ssh-manager\role.json；
  dek279\cache-dek-e2e279-new.key；
  work\r4\anchor-a.lines、anchor-a-out.txt、curl-body.txt、curl-head.txt、fwd-driver.ps1、
    fwd-seq-out.txt、gen-driver.ps1、gen-seq-out.txt、gen-upload-src.txt、ls-anch-out.txt、
    mcpdrive.ps1、mcp-driver-r4.ps1、mix2701-cat.txt、mix2701-old.txt、mix2702-new.txt、
    mix2711-entryb.txt、mix2711-old.txt、mix2712b-new.txt、mix2712c-new.txt、mix2712-new.txt、
    mix2721-old.txt、mix2721-old2.txt、mix2722-new.txt、mix2722-relay.txt、mix2751-old.txt、
    mix2753-expire.txt、mix2753-gen1.txt、mix2753-gen2.txt、mix2754-client.html、
    mix2754-home.html、mix2754-ui-evidence.txt、mix2755-cache.err、mix2755-cache.out、
    mix2755-cache-clean.err、mix2755-cache-clean.out、mix2755-clean.cmd、mix2755-doctor.err、
    mix2755-doctor.out、mix2755-doctor-clean.err、mix2755-doctor-clean.out、mix2755-out.txt、
    mix2755-update.err、mix2755-update.out、mix2755-update-clean.err、mix2755-update-clean.out、
    mixnew-ls.txt、new-mcp-driver.ps1、new-proj-token.txt、new-tools.txt、old-mcp-driver.ps1、
    old-tools.txt、pair-anch.err、pair-anch.out、pair-new.err、pair-new.out、
    pair-old.err、pair-old.out。
随后按最深目录优先移除 instances\e2e279-new、instances、ssh-manager 与各一次性根目录。
清理后上述 8 个对象全部不存在；根目录内已无 e2e279 或 v0211 命名路径；
bin-v0211 / bin-rc9 一次性 sshmgr.exe 进程数 0。
```

【其他机器与本机清理】
```text
Linux 受管服务器 A：e2e279 命名文件/目录 0；一次性 sshmgr ui/serve/mcp 进程 0；
  e2e279 授权行已在前一轮清理，本轮复核计数 0；e2e278-linux 隔离客户端根目录保留。
NAS A：既有共享挂载仍可查看，sshmgr-e2e-20261114 一次性目录命中 0。
Windows 受管服务器 A：一次性根目录不存在；sshmgr 进程数 0。
本机：v0.21.1 下载校验目录、临时 HTTP 分发目录与 12 个旧版本/辅助文件逐个删除；
  分发辅助根目录不存在；任务指定的本机临时 HTTP 端口无监听；python3 -m http.server 进程数 0。
泄漏检查临时副本：演练权威端机器上 68 个文件逐个删除，extract 与检查根目录逐层移除；
  本机压缩包与 21 个分块文件删除。
```

【清理后保留状态】
```text
演练权威端：rc9 serve 服务 active / HEALTHY，版本 0.22.0-rc9。
Linux 受管服务器 A：rc9 版本；e2e278-linux 一次性客户端实例与隔离目录保留；无遗留进程。
工作机 1：C:\sshmgr-e2e-20261114\ 保留（含 bin-rc9）；e2e278-win 实例与 vault/config/dek 保留；
  该目录中早期轮次保留的 bin、bin-rc7、bin-rc8、manual272、work、三份历史安装包未动。
演练权威端共享前置数据、NAS 挂载与既有 e2e278 设备码、项目令牌未清理。
两个应保留客户端实例的最终权威端仍在演练权威端 rc9 服务上。
```

【工作机 1 生产目录指纹（收尾后）】
```text
文件数=5
整体 SHA-256=ad4edf0d837b734178a1231610f94370fa7d2ed053ebe5db580734834376566c
与本记录开头指纹一致；本轮所有工作机 1 操作未改动生产目录。
```

【机密值命中计数（只看计数，值不入证据）】
```text
检索范围：本运行记录、docs/backlog.md、#279 上一轮运行目录、#279 本轮收尾运行目录；
  归档展开后共 46 个文件表示（含 macOS 归档元数据副本）。
通用机密项测试值：非零文件 0，总命中 0。
NAS 一次性账号密码：非零文件 0，总命中 0。
检索命令在演练权威端机器上以 root 只读模式执行；临时归档与分块用后逐文件删除。
```

【脱敏 grep】
```text
仓库证据：本运行记录命中 0。docs/backlog.md 保留 1 条历史欠账第 17 条原文，
  其中写出了默认服务端口号，为既有历史内容，非本轮新增，未改动。
两个运行目录为私有输入与执行日志，Brief 本身按要求包含真实访问标识；本轮快照合计命中
  1113 行、15 个文件。这不计入公开运行记录脱敏结论；两个机密测试值在这些文件中命中均为 0。
```

## 第五轮（rc10）安装与版本

【结论】演练环境升级到 0.22.0-rc10（源提交 `e4c322d`），产物 SHA-256 全部核对一致；演练权威端服务重启后健康，两个保留客户端实例完成一次缓存拉取与只读执行冒烟。

【升级与版本】
```text
本机产物核对：
  linux_amd64.tar.gz  11a3c8b672d21ba83bf874922b00868ed7b22ba82fdaf80d83dde348b84d522b
  darwin_arm64.tar.gz 9d122ac7408eae0008950c49e5f5916f2f25454f99f696d5ffc9fc3bb09b771d
  windows_amd64.zip   9671a9fac6b0129be3d4f05c7faedc508bdd95fb87e8e667e38ff0e406c6b90d

演练权威端：
  升级前 0.22.0-rc9 → 升级后 0.22.0-rc10；
  /usr/local/bin/sshmgr 先备份为 /usr/local/bin/sshmgr.rc9，再替换；
  systemctl restart sshmgr-serve 后：
    service:   Running
    process:   running
    http:      responding (401/200 = auth working)
    vault:     ok
    overall:   HEALTHY

Linux 受管服务器 A：
  升级前 0.22.0-rc9 → 升级后 0.22.0-rc10；
  /usr/local/bin/sshmgr 先备份为 /usr/local/bin/sshmgr.rc9，再替换；该机未安装 serve 服务。

工作机 1：
  rc10 压缩包下载到 C:\sshmgr-e2e-20261114\，SHA-256 与上表一致；
  解压到 C:\sshmgr-e2e-20261114\bin-rc10\，未进 PATH，未触碰生产 sshmgr；
  C:\sshmgr-e2e-20261114\bin-rc10\sshmgr.exe version → 0.22.0-rc10。
```

【保留实例缓存拉取与冒烟】
```text
Linux 一次性客户端实例 e2e278-linux：
  cache pull --instance e2e278-linux → pulled 4 servers / 5 credentials
  cache status → age 0s；servers 4；credentials 5；source=<演练权威端地址>
  MCP serverInfo.version → 0.22.0-rc10
  exec_command（Linux 受管服务器 A 条目，只读）：
    exit_code=0；stdout="rc10-linux-cache-smoke\nLinux\n"；stderr_bytes=0

Windows 一次性客户端实例 e2e278-win：
  cache pull --instance e2e278-win → pulled 12 servers / 14 credentials
  cache status → age 0s；servers 12；credentials 14；source=<演练权威端转发地址>
  MCP serverInfo.version → 0.22.0-rc10
  exec_command（Windows 受管服务器 A PowerShell 条目，只读）：
    exit_code=0；stdout 首行 "rc10-windows-cache-smoke"；stderr_bytes=0
```

【安装过程如实记录】Linux 两台第一次下载命令把压缩包落在用户主目录而不是计划临时路径；未解压、未替换二进制。随后对该文件重新核对 SHA-256 后完成安装，并逐个删除压缩包与解出的临时二进制，不留安装残留。

## 欠账复核（第五轮 rc10）

### 欠账第 53 条：旧客户端配对 ECDSA 权威端

【结论】通过。v0.21.1 旧客户端在登记阶段即被 rc10 权威端拒绝，错误不再落到签名验证的 `bad sig`；申请不进入待批准列表；声明版本的 rc10 新客户端对同一权威端可完成完整配对。

【前置与设置】
```text
演练权威端：0.22.0-rc10；serve 证书公钥算法 id-ecPublicKey（ECDSA）。
旧客户端：工作机 1 一次性 v0.21.1 实例 e2e279-old10（独立 APPDATA、store、缓存解密钥目录；
  安装包 SHA-256 与 v0.21.1 checksums.txt 一致）。
本轮开始时权威端沿用上一轮清理后的 serve.pairing=false，首次真实 pair 得到 HTTP 404；
  这不是本欠账的产品结果。为执行配对判据，临时把 serve.pairing 设为 true，空正文探针返回
  HTTP 400「id must be 32 bytes hex」，证明 /pair/enroll 面已打开；本条结束后已恢复 false。
```

【旧客户端真实执行】
```text
命令：v0.21.1 pair --instance e2e279-old10 --url <演练权威端转发地址> --pin <服务证书指纹>
exit=1
Error: pairing enroll refused: HTTP 400 此权威端使用 ECDSA 证书，需要 0.22.0 或更新的客户端才能配对；
  请升级客户端，或请 owner 在权威端执行 `sshmgr cache-tokens add` 发放设备码后用 `sshmgr cache p…
（v0.21.1 客户端将响应正文裁剪到 200 字符；未出现 `bad sig`。）

权威端无版本请求头补充探针：
  HTTP 400
  此权威端使用 ECDSA 证书，需要 0.22.0 或更新的客户端才能配对；请升级客户端，
  或请 owner 在权威端执行 `sshmgr cache-tokens add` 发放设备码后用 `sshmgr cache pull` 入网

待批准列表：sudo sshmgr serve pair ls → (no pending pairing requests)
权威端 serve 日志：
  sshmgr serve: pairing enroll refused: client did not declare a version and
  the serve certificate is not ed25519
```

【新客户端对照与清理】
```text
新客户端：Linux 受管服务器 A 上一次性 rc10 实例 e2e279-new10。
pair 输出：SAS 六位短认证码已生成；owner 面 serve pair approve 绑定既有 Linux profile。
结果：配对完成；pulled 4 servers / 5 credentials；
  cache status → device=e2e279-new10，scope=既有 Linux profile，source=<演练权威端地址>；
  cache-tokens ls → client_version=0.22.0-rc10，status=active。
只读冒烟：MCP serverInfo.version=0.22.0-rc10；exec_command → exit_code=0，
  stdout="rc10-new-pair-smoke\nLinux\n"，stderr_bytes=0。
撤销：cache-tokens revoke e2e279-new10 → status=revoked；待批准列表为空。
清理：该一次性实例 11 个文件逐个删除，空目录逐层移除，路径不存在；无遗留 pair 进程。
```

### 欠账第 55 条：新客户端遇旧权威端的未锚定条目首连

【结论】通过。rc10 客户端对 v0.21.1 旧权威端快照中的未锚定条目首次连接成功；新格式锚转发被旧权威端拒绝后自动退回旧格式，锚以 `source=forward` 落库；刷新快照后的再次连接不再发起锚转发。

【一次性旧权威端】
```text
放置机器：Linux 受管服务器 A（该机无生产 sshmgr；独立 vault/config/dek/store 与独立端口）。
版本：v0.21.1；Linux 产物 SHA-256 与正式 release checksums.txt 一致；
  一次性二进制路径 /usr/local/bin/sshmgr-v0211。
serve：/usr/local/bin/sshmgr-v0211 serve --addr 0.0.0.0:<旧权威端独立端口>；
  进程监听该端口；/snapshot 未认证探针 HTTP 401；证书为 ED25519。
服务器条目：e2e279_oa55_linux，指向演练权威端（本轮作为可连受管靶机），专用一次性私钥；
  登记后 servers pin-hostkey --list 对该条目无输出 → 未锚定，未带外落锚。
新客户端：同一台 Linux 受管服务器 A 上的一次性 rc10 实例 e2e279-new55，
  以旧权威端一次性设备码 cache pull 入网；pulled 1 servers / 1 credentials；
  cache status → device=e2e279-new55，scope=一次性 profile，source=<旧权威端地址>。
```

【首次连接与落锚】
```text
exec_command（未锚定条目，只读）：
  MCP serverInfo.version=0.22.0-rc10
  exit_code=0；stdout="rc10-first-unanchored-ok\nLinux\n"；stderr_bytes=0
  标准错误只有旧权威端版本提示、快照版本端点缺失提示与软件清单降级提示；
  无 unparseable key_blob，无连接失败。

旧权威端 serve 输出：
  sshmgr serve: pin-hostkey "":0 -> 400 (device "-")
  sshmgr serve: pin-hostkey "<演练权威端地址>":22 -> 201 (device "e2e279-new55")
解释：第一行是 rc10 新格式请求被 v0.21.1 按旧格式解析失败；第二行是自动退回的
  host/port/key_blob 旧格式请求落锚成功。

落锚核对：
  servers pin-hostkey --list →
  <演练权威端地址>:22 fp=SHA256:<主机密钥指纹> (format=blob, source=forward,
  device=e2e279-new55, created=<时间>)

客户端一次性实例内检索 unparseable key_blob → 命中 0。
```

【再次连接】
```text
未刷新快照的立即复连：exit_code=0；旧权威端先返回一次 400，再用旧格式返回 409；
  连接仍成功。这说明旧快照中的锚需刷新后才进入本地已锚定路径。
重新 cache pull → pulled 1 servers / 1 credentials；age 0s。
刷新后第三次只读连接：
  exit_code=0；stdout="rc10-anchored-after-pull-ok\nLinux\n"；stderr_bytes=0。
旧权威端 pin-hostkey 日志行数保持 4 行，未新增任何锚转发请求 → 已走本地锚定路径。
```

### serve status 进程探测

【结论】通过。Windows 只按 `sshmgr-serve` 服务进程判断；Linux 只把命令行中真正带有 `serve` 子命令的 sshmgr 进程认作 serve，`mcp` 干扰进程与一次性 `sshmgr-v0211` serve 均不误报。

【工作机 1】
```text
只读进程盘点：Win32_Process name=sshmgr.exe → count=18；
  18 个进程的可执行文件路径均为工作机生产 sshmgr 路径；未停止、未重启、未改动。
服务查询：sc query sshmgr-serve → 1060 指定的服务未安装。
因此本机一次性 rc10 客户端路径的期望值为 process: not running。

C:\sshmgr-e2e-20261114\bin-rc10\sshmgr.exe serve status（隔离 vault/config/dek 环境变量）：
  service:   NOT INSTALLED
  process:   not running
  http:      not responding
  vault:     LOCKED (master.key not found — run `sshmgr unlock`)
  overall:   DEGRADED (see above)
  status-exit=0
判据：18 个生产 sshmgr.exe 进程在场时仍不误报 serve 进程 ✓。
```

【演练权威端（装有 serve）】
```text
systemctl：active / running；MainPID 在场。
/proc/<MainPID>/cmdline：/usr/local/bin/sshmgr serve --addr 0.0.0.0:<服务端口>
serve status：
  service:   Running
  process:   running
  http:      responding (401/200 = auth working)
  vault:     ok
  overall:   HEALTHY
```

【Linux 受管服务器 A（未装 serve，带干扰进程）】
```text
系统服务 sshmgr-serve：inactive。
干扰进程 /proc/<进程号>/cmdline：
  /usr/local/bin/sshmgr mcp --cache --instance e2e279-new55
第 55 条一次性旧权威端仍在运行：
  进程名 comm=sshmgr-v0211
  /proc/<进程号>/cmdline=/usr/local/bin/sshmgr-v0211 serve --addr 0.0.0.0:<旧权威端独立端口>

serve status：
  service:   NOT INSTALLED
  process:   not running
  http:      not responding
  vault:     LOCKED (open /var/lib/ssh-manager/master.key.plain: permission denied)
  overall:   DEGRADED (see above)
判据：mcp 进程不误报 ✓；一次性文件名 sshmgr-v0211 的 serve 未被认作本产品
  /usr/local/bin/sshmgr serve，按实际命令行不误报 ✓。DEGRADED 来自该机无权威 vault
  且服务未安装，不影响本条进程探测判据。
```

### 欠账第 52 条：桌面手动添加引导的 Windows 1219 提示

【结论】通过。演练权威端 rc10 Web UI 的「桌面手动添加共享存储」引导注意事项已包含 Windows 错误 1219 的原因与 `net use \\<地址> /delete` 处理办法；未在工作机 1 桌面做任何实际映射。

【代证方式】
```text
owner 不在场，按授权由 agent 代证。未打开工作机 1 桌面浏览器；在演练权威端本机用无界面
Chromium 打开回环 Web UI，管理员口令只从 root 专属文件读入脚本，不进入证据。
页面路径：共享存储详情 → 打开桌面手动添加引导 → 选择工作机 1 对应拓扑设备 → 显示引导。
```

【页面注意事项（脱敏）】
```text
注意事项
笔记本更换网络后，映射可能失效；换回同一网络或重新按引导映射。
这台电脑已用其他账号连接过该共享存储所在设备时，Windows 会报错 1219
（不允许同一用户用多个用户名连接同一服务器）。请先执行
net use \\<地址> /delete 断开那些连接，或改用该设备的其他地址。

1219-present=True
net-use-delete-present=True
```

### 第五轮收尾清理、保留状态与指纹

【清理结论】本轮新建的一次性客户端、旧权威端、旧版本二进制、设备码、干扰进程、一次性授权公钥、本机临时 HTTP 服务与临时脚本均已清理；应保留对象仍在。

【设备码与进程】
```text
e2e279-new10：cache-tokens revoke → status=revoked；一次性实例已删除。
e2e279-new55：在旧权威端 cache-tokens revoke → status=revoked；旧权威端随后停止并删除。
干扰用 mcp 进程与启动壳进程：按记录的进程号停止；清理后无匹配进程。
旧权威端 serve 进程：按记录的进程号停止；独立端口不再监听。
演练权威端 pairing 开关：恢复为 false；serve pair ls → (no pending pairing requests)。
```

【逐文件清理】
```text
Linux 受管服务器 A：
  e2e279-oa55 一次性旧权威端 13 个文件逐个删除：
    .serve-cert-initialized、id_ed25519、id_ed25519.pub、master.key.plain、serve-cert.pem、
    serve-key.pem、serve-stdout.log、serve.pid、store.db、work/device-code.txt、
    work/mcp-interference.log、work/mcp-interference.pid、work/project-token.txt；
    随后空目录逐层移除。
  e2e279-new55 一次性 rc10 客户端 7 个文件逐个删除：
    cache-audit.log、cache.auth.json、cache.bin、cache.meta.json、
    software-inventory-collect.lock、cache-dek-e2e279-new55.key、work/project-token.txt；
    随后空目录逐层移除。
  /usr/local/bin/sshmgr-v0211 删除；v0211/new55/oa55 命名路径与匹配进程均为 0；
  旧权威端独立端口无监听。

演练权威端：
  本轮补充探针的 /tmp/p53-body.json 与 /tmp/p53-response.body 删除；
  受管靶机 authorized_keys 中带本轮一次性注释的公钥行：删除前 1 行、删除后 0 行。

工作机 1：
  bin-v0211-r10 中 README.md 与 sshmgr.exe 逐个删除；bin-v0211-r10、appdata-v0211-r10、
  dek-v0211-r10、store-v0211-r10 四个空目录逐个移除；路径均不存在，一次性别进程数 0。

本机：
  v0.21.1 下载校验目录中 3 个文件删除后移除空目录；13 个临时脚本/请求体文件逐个删除；
  临时 HTTP 服务停止，分发端口无监听，python3 -m http.server 进程数 0。
```

【清理后保留状态】
```text
演练权威端：0.22.0-rc10 serve 服务 Running / HEALTHY；pairing=false；待批准列表为空。
Linux 受管服务器 A：0.22.0-rc10；e2e278-linux 保留，cache status 正常；
  /usr/local/bin/sshmgr.rc9 备份保留。
工作机 1：C:\sshmgr-e2e-20261114\ 保留，含 bin-rc9 与 bin-rc10、rc10 安装包；
  e2e278-win 保留，cache.bin 在场；生产 sshmgr 进程未停止、未重启。
共享前置数据、既有 e2e278 设备码与项目令牌未清理。
```

【工作机 1 生产目录指纹（第五轮前后）】
```text
开始前：文件数=5，整体 SHA-256=0a30ecdfb17b6f4454f0885cc394fd800b251bf8b9a992f3c0841596080165b8。
收尾后：文件数=5，整体 SHA-256=0a30ecdfb17b6f4454f0885cc394fd800b251bf8b9a992f3c0841596080165b8。
结论：前后一致；本轮未改动工作机 1 生产目录。
```

【机密值命中计数与脱敏检索（第五轮新增内容）】
```text
机密值检索范围：本轮追加到本运行记录与 docs/backlog.md 的 291 行新增内容；
  检索值文件始终留在演练权威端机器，仅以 root 管道喂给 grep -c -F -f，只输出计数。
通用机密项测试值：命中 0。
NAS 一次性账号密码：命中 0。

第一轮 Brief 指定脱敏 grep：
  本运行记录命中 0；单机运行记录命中 0；
  docs/backlog.md 仅保留历史欠账第 17 条的 1 处默认端口号原文，非本轮新增，未改动。
```

## 第六轮（rc11）安装与版本

【结论】演练环境升级到 0.22.0-rc11（源提交 `eff984c`），产物 SHA-256 全部核对一致；演练权威端服务重启后健康，两个保留客户端实例完成一次缓存拉取与只读执行冒烟。

【升级与版本】
```text
本机产物核对：
  linux_amd64.tar.gz  ee83269e05f46c8cde5b0727b658c0fbed51e57b5dd831eb9048cf7608ac3220
  darwin_arm64.tar.gz 68ae5a7cfe0d736f95ab3ade9a2f50a2e41a257ef3f5eccccaecf0c25799f522
  windows_amd64.zip   ed6100135e5e8c1c8775bb6e49c8a889ea2271b7906c1de3837972f9ce46bef3

演练权威端：
  升级前 0.22.0-rc10 → 升级后 0.22.0-rc11；
  Linux 安装包经 SSH 会话分块传输（138 块 base64），远端重组后 SHA-256 与上表一致；
  /usr/local/bin/sshmgr 先备份为 /usr/local/bin/sshmgr.rc10，再替换；
  systemctl restart sshmgr-serve 后：
    service: Running / process: running / http: responding (401/200) / vault: ok / overall: HEALTHY

Linux 受管服务器 A：
  升级前 0.22.0-rc10 → 升级后 0.22.0-rc11；
  同样分块传输并核对 SHA-256 一致；备份 sshmgr.rc10 后替换；该机未安装 serve 服务。

工作机 1：
  rc11 压缩包经本机临时 HTTP 服务下载，SHA-256 与上表一致；
  解压到 C:\sshmgr-e2e-20261114\bin-rc11\，未进 PATH，未触碰生产 sshmgr；
  bin-rc11\sshmgr.exe version → 0.22.0-rc11。
```

【保留实例缓存拉取与冒烟】
```text
Linux 一次性客户端实例 e2e278-linux：
  cache pull → pulled 4 servers / 5 credentials；cache status → age 0s；servers 4；credentials 5
  MCP serverInfo.version → 0.22.0-rc11
  exec_command（Linux 受管服务器 A 条目，只读）：
    exit_code=0；stdout="rc11-linux-cache-smoke\nLinux\n"；stderr_bytes=0

Windows 一次性客户端实例 e2e278-win：
  cache pull → pulled 12 servers / 14 credentials；age 0s；source=<演练权威端转发地址>
  MCP serverInfo.version → 0.22.0-rc11
  exec_command（Windows 受管服务器 A PowerShell 条目，只读）：
    exit_code=0；stdout 首行 "rc11-windows-cache-smoke"；stderr_bytes=0
```

【本轮开始时工作机 1 生产目录指纹】文件数=5（方法与第五轮相同：逐文件 SHA-256 按路径排序拼接后再取整体 SHA-256），收尾时复核比对见第六轮收尾小节。

## 欠账复核（第六轮 rc11）

### 欠账第 53 条补充判据：拒绝提示在 v0.21.1 客户端完整显示

【结论】通过。rc11 把拒绝正文缩短到 200 字节以内，v0.21.1 旧客户端在登记阶段收到的提示完整显示到「sshmgr cache pull 入网」，末尾没有「…」；申请不进入权威端待批准列表；权威端 serve 日志有一行拒绝原因。

【前置与设置】
```text
演练权威端：0.22.0-rc11；serve 证书公钥算法 id-ecPublicKey（ECDSA）。
旧客户端：工作机 1 一次性 v0.21.1 实例 e2e279-old11（独立 APPDATA、store、缓存解密钥目录；
  安装包 SHA-256 与 v0.21.1 正式 release checksums.txt 一致：0be78209c8e5e797a3bc51004f46877d61866c2177a261eb819b1fd3c567367a）。
权威端 serve.pairing 平时为 false：临时设为 true 后空正文探针返回 HTTP 400，证明配对面已打开；
  本判据结束后已恢复 false 并留证。
```

【旧客户端真实执行（v0.21.1 pair --url --pin）】
```text
exit=1
Error: pairing enroll refused: HTTP 400 需要 0.22.0 或更新的客户端（权威端为 ECDSA 证书）。
  可升级客户端，或让 owner 执行 sshmgr cache-tokens add 发设备码后 sshmgr cache pull 入网

字节核验（客户端输出原样落盘后按 UTF-8 解码）：
  错误首行总长 214 字节；前缀 "Error: pairing enroll refused: HTTP 400 " 40 字节；
  拒绝正文 174 字节（<200），末尾为「sshmgr cache pull 入网」，无「…」截断。
  对照 rc10 轮：正文 250+ 字节时 v0.21.1 显示在「sshmgr cache p…」处截断。

待批准列表：sudo sshmgr serve pair ls → (no pending pairing requests)
权威端 serve 日志：
  sshmgr serve: pairing enroll refused: client did not declare a version and
  the serve certificate is not ed25519

收尾：serve.pairing 已恢复 false；待批准列表为空。
```

## 权威端272-2、权威端274-3 macOS 补跑（第六轮 rc11，一次性虚拟机）

### 一次性 macOS 虚拟机与共同前置

【结论】一次性 macOS 虚拟机可用，桥接网络拿到局域网地址，能到达 NAS A 的 SMB 端口；演练权威端按正常流程登记服务器条目并完成主机密钥指纹锚定，macOS 靶机首次真实挂载成功。

【虚拟机方案】
```text
工具：tart 2.41.1（Apple Virtualization.framework 命令行虚拟机工具）。
  原 cirruslabs/tart 仓库已重定向到 openai/tart；本机经 openai/tools Homebrew
  tap 安装 tart 与依赖 softnet，未修改本机生产 sshmgr。
镜像：ghcr.io/cirruslabs/macos-tahoe-base:latest（macOS 26.6.2，arm64，
  清单标注未压缩磁盘 50 GB）。
下载：先在本机缓存镜像层并做 SHA-256 校验，再经只绑定本机回环地址的
  一次性 OCI 镜像服务克隆。第一次克隆在 48% 处失败：该临时镜像服务未处理
  HTTP Range 请求，tart 重试收到完整的 HTTP 200 后拒绝。修正为按请求偏移
  返回 HTTP 206 后重跑克隆成功；这不属于 sshmgr 产品缺陷。
最终缓存：96 个清单层去重后 71 个 blob，共 26,988,883,052 字节，逐个
  SHA-256 校验通过，无半成品文件。
运行：tart run --no-graphics --net-bridged=<本机有线网卡>；8 个虚拟 CPU、
  8192 MiB 内存、50 GB 磁盘。虚拟机通过桥接网络获得局域网地址。
```

【一次性登录与网络隧道】
```text
搭建期仅使用一次性 SSH 密钥登录虚拟机；虚拟机内 admin 账号可免密提权，
  /usr/sbin/automount 与 /etc/auto_master 在场，系统睡眠已关闭。
受管连接：演练权威端生成一次性 ed25519 登录密钥，按服务器条目正常登记；
  主机 ED25519 指纹以带外方式锚定，首次产品连接输出 Darwin/arm64 并确认提权可用。
网络现实：本机所在物理网络不能直接路由到演练内网；虚拟机向演练权威端与
  Linux 一次性客户端实例宿主各保持一条仅转发 SSH 端口的一次性反向隧道。
  两条隧道只绑定对应机器的回环地址，产品服务器条目经反向隧道连接虚拟机。
NAS 可达性：虚拟机对 NAS A 的 SMB 端口现场探测成功。
```

【登记与挂载】
```text
拓扑：新一次性设备加入 NAS A 所在的物理局域网网络；地址在证据中脱敏。
授权：挂载密码机密项加入 macOS 一次性条目；Linux 一次性客户端实例的
  profile 授权该条目。
共享版本：18 → 19；可挂载服务器由原来的 Linux 与 Windows 条目增加
  macOS 一次性条目。
macOS 挂载：first-mount-success；挂载点 /mnt/sshmgr/<共享名>；
  实际网络为与 NAS A 共同的物理局域网（第 1 个地址）；
  提示「系统配置被重置，已重建」。
远端事实（不输出映射正文，避免地址与密码进入证据）：
  auto_master 引用数=1；共享标记行数=1；挂载表中的 smbfs 挂载数=1；
  撤销标记数=0。
Linux 对照：Linux 受管服务器 A 仍为已挂载，远端配置版本号 18；
  macOS 一次性条目为已挂载，远端配置版本号 19。
```

【Linux 一次性客户端实例冒烟】
```text
cache 自动保鲜后 list_servers：pulled 5 servers / 7 credentials；
  macOS 一次性条目出现在该 profile 内，host 为 hidden，has_sudo=true，
  caveats 明确「一次性 macOS 虚拟机靶机」与用途。
exec_command（macOS 一次性条目，只读）：
  exit_code=0；stdout="rc11-linux-client-to-macos-ok\nDarwin\narm64\n"；
  stderr_bytes=0；MCP serverInfo.version=0.22.0-rc11。
结论：Linux 一次性客户端实例宿主的反向隧道与产品连接路径可用。
```

【与物理 macOS 靶机的差异】本条使用 Apple 芯片虚拟机、桥接物理网卡与真实 NAS SMB 服务；CPU、网络适配器、电源与机型不是物理 Mac 硬件。共享挂载、autofs、提权、网络可达与权威端撤销/清理均为真实系统操作，不是模拟服务器。

### 权威端274-3 共享存储授权、远端操作与桌面引导（macOS 补跑）

【结论】通过。真实 macOS 一次性靶机完成授权移除、远端撤销清理、autofs 残留检查与真实卸载；Linux 对照靶机完成重选与卸载；权威端 Web UI 与命令行等价面最终登记和远端状态一致。浏览器取证分两遍执行：第一遍验证「撤销授权 + 同时卸载」任务；自动化脚本当时没有点击授权变更预览的最终确认，不足以证明重验证闸。恢复前置后第二遍重跑授权移除，补齐「未重验拒绝 → 重验后目标集合保留 → 一次确认」证据；两遍均为真实权威端与真实靶机操作。

【步骤 1：可挂载范围与重验证（Web UI）】
```text
前置：管理员登录后等待 5 分零 5 秒；macOS 一次性条目勾选=True。
取消勾选并点击「预览可挂载范围变更」：
  预览显示「新增 0 个授权，移除 1 个授权」；
  挂载密码机密项将移除 macOS 一次性条目；
  对应共享存储将移除该条目的可挂载范围；
  页面提供「同时卸载」与一次「确认保存授权」。
未重验直接点击确认：
  URL 跳到 /ui/reverify；页面只提供「管理员口令 / 验证并继续」，
  没有执行授权写入。
重验后返回同一预览：
  仍为「移除 1 个授权」，目标集合保留，未回退成原勾选集合。
再点击一次确认：
  页面显示「已保存授权变更」；返回详情页后 macOS 勾选=False；
  共享存储版本号 21 → 22。
本轮未勾选「同时卸载」，因此 macOS 挂载与 autofs 配置仍留在靶机上；
  随后 residue 只读检查如实报告「配置条目、挂载」残留。
```

【步骤 2：macOS 撤销授权与远端清理】
```text
Web UI 第一遍（重验证状态已超五分钟后）：
  撤销确认页说明将移出可挂载范围、清理远端持久配置并可同时卸载。
  勾选「同时卸载」并确认后任务完成：版本号 20；
  「远端持久配置已删除，挂载已卸载」。
  该遍用于验证撤销任务本身；随后重新授权并挂载，恢复到第二遍步骤 1 前置。

授权移除后的入口事实：
  直接打开撤销页时，产品如实显示「服务器不在可挂载范围内」，
  并提示先做残留检查；residue 发现 macOS 仍有配置与挂载。
  随后执行只清理远端、不修改登记的撤销路径：
  版本号 22；远端持久配置删除，挂载卸载。
macOS 靶机复核：
  auto_master 引用数=0；autofs 映射条目数=0；挂载表 smbfs 数=0；
  撤销标记内容只含百分号编码共享名、版本号 22 与 revoked 状态，
  不含共享地址、挂载账号或密码。
最终 residue：macOS 一次性条目无残留。
```

【步骤 3：Linux 对照重选与卸载（Web UI）】
```text
重选确认页：目标为 Linux 受管服务器 A；说明物理局域网更优地址优先、
  当前地址已最优时不改动、忙时保留并由 owner 决策。
任务结果：已切换到更优可达地址；网络为与 NAS A 共同的物理局域网；
  共享存储版本号 22。
卸载确认页：说明先删除远端持久配置再卸载。
任务结果：卸载完成；远端持久挂载配置已删除，挂载点已卸载；
  版本号 22；提示该条目仍在可挂载范围内，彻底撤销应使用撤销授权。
```

【步骤 4：全部共享残留检查（Web UI）】
```text
任务从「残留检查进行中」自动轮询到「残留检查完成」。
汇总：0 台有残留，0 台仅有撤销标记，12 台未检查，2 台无残留，2 台跳过。
Linux 受管服务器 A：无残留；macOS 一次性条目：无残留。
Windows 两条目：跳过，说明 sshmgr 不在 Windows 服务器留持久配置。
不可达或未配置 sudo 的既有条目只显示未检查原因，没有排队成待清理清单。
```

【步骤 5：桌面手动添加引导（Web UI）】
```text
选择工作机 1 对应拓扑设备并显示引导：
  推荐地址按共同物理局域网推算（地址已脱敏）；
  Windows 固定盘符、访问路径、目录约定与运行时输密码的 net use 等价命令齐全；
  macOS 访达连接步骤同样只给地址与账号输入位置。
页面明确「不显示、复制或下载共享密码」，等价命令密码位置为 *；
  没有密码显示、复制或下载入口；地址与账号在公开证据中脱敏。
```

【步骤 6：命令行等价面】
```text
先按前置重建 Linux 挂载：first-mount-success，版本号 22。
secrets disallow（macOS 一次性条目）：
  已取消 1 个机密项写入 1 个服务器条目；提示只改登记，
  撤销用 shares revoke，残留检查用 shares residue。
shares revoke（macOS，--yes）：
  条目已不在可挂载范围；本次只清理远端；版本号 22；
  远端持久配置已删除，现有挂载保留。
shares reselect（Linux 受管服务器 A，--yes）：
  当前地址已是最优可达地址，未做改动；无地址原文输出。
shares unmount（Linux 受管服务器 A，--yes）：
  卸载完成；版本号 22；提示条目仍在可挂载范围内。
shares residue：0 台有残留 / 0 台仅有撤销标记 / 2 台无残留；
  Linux 与 macOS 均无残留；Windows 按设计跳过。
shares show --check：Linux 受管服务器 A 未挂载；转发与旧一次性 Linux 条目不可达；
  Windows 不检测并说明盘符由每条命令执行前注入。
结论：页面与命令行最终登记、远端配置、挂载状态一致。
```

【审计】`share-revoke`、`share-revoke-remote`、`share-reselect`、`share-unmount`、`share-residue-check` 均有权威端审计行；Web UI 来源标记为回环浏览器，命令行来源标记为 cli。审计摘要不含共享地址、挂载账号或密码。

### 权威端272-2 撤销、残留检查与换密码说明（macOS 补跑）

【结论】失败（一处期望不满足，登记欠账第 56 条）。macOS 与 Linux 的持久配置删除、卸载、撤销标记安全字段、换密码提示、Windows 登记撤销与在线客户端停止注入均通过；但 residue 在 Linux 与 macOS 靶机只余撤销标记时都输出「无残留」，没有按册输出「仅有撤销标记（撤销后的预期状态，不需要清理）」。

【前置重建】
```text
共享版本 22 → 23；macOS 一次性条目重新进入可挂载范围并真实挂载。
Linux 受管服务器 A 重新挂载；两者远端配置版本号均为 23。
工作机 1 一次性客户端实例同步后对 Windows PowerShell 条目执行引用固定盘符的命令：
  injected_network_drives=[{drive:X, share:<NAS 共享名>}]；
  stdout 含 X-PRESENT；exit_code=0。
说明撤销前 Windows 盘符会话内注入可用。
```

【第二挂载密码】owner 不在场；按一次性靶机处置，在演练权威端生成仅 root 可读的第二随机值，经标准输入传给撤销命令，不进入命令参数、输出或证据。该值只替换 sshmgr 挂载密码机密项，未修改 NAS 账号本身；判据完成后立即从 root 专属事实源恢复原机密项值并删除第二值文件。

【步骤 1：macOS 撤销、换密码与远端清理】
```text
命令语义：shares revoke <共享> <macOS 一次性条目>
  --unmount --new-password-stdin --yes；密码来自 root 专属文件重定向。
输出：版本号 24；远端持久配置已删除，挂载已卸载。
换密码提醒：
  桌面上手动添加的共享连接也需要更换密码；
  已更换共享存储挂载密码；
  仍可挂载的 Linux 与 macOS 服务器逐台列出，并提醒 agent 按新版本号重新挂载。
macOS 靶机复核：
  auto_master 引用数=0；autofs 映射条目数=0；
  映射中挂载账号与共享地址命中数=0；挂载表 smbfs 数=0；
  撤销标记为编码共享名 + 版本号 24 + revoked 状态，不含地址、账号或密码。
```

【步骤 2：macOS residue 判据】
```text
实际输出：服务器 <macOS 一次性条目>（macOS）：无残留。
汇总：0 台有残留，0 台仅有撤销标记。
期望：该靶机只余撤销标记时应输出「仅有撤销标记（撤销后的预期状态，不需要清理）」。
远端人工复核证明撤销标记文件确实在场，因此这不是靶机状态缺失；
  判定为 residue 分类/呈现不满足验收册。
```

【步骤 3–4：Linux 撤销与 residue】
```text
Linux 受管服务器 A revoke --unmount --yes：版本号 25；
  远端持久配置已删除，挂载已卸载。
远端复核：fstab 管理行=0；凭据文件 absent；挂载表 cifs 数=0；
  撤销标记为编码共享名 + 版本号 25 + revoked 状态。
residue 实际输出同样为「无残留」，而非「仅有撤销标记」；
  与 macOS 侧一致复现欠账第 56 条。
```

【步骤 5：Windows 登记撤销】
```text
Windows PowerShell 条目撤销：版本号 26；未连接 Windows 服务器；
  各客户端同步快照后停止注入。
Windows cmd 条目撤销：版本号 27；同样未连接 Windows 服务器。
两条输出均说明：正在执行的命令保留盘符直到结束；长期离线的客户端同步前仍会注入；
  真正的撤销手段是更换共享密码。该说明与本次换密码路径一致。
```

【步骤 6：Windows 注入停止（立即同步）】
```text
工作机 1 一次性客户端实例先自动拉取新快照，再执行同一固定盘符引用命令：
  exit_code=0；stderr_bytes=0；stdout 含 X-ABSENT；
  injected_network_drives 字段不再出现（结构化结果无该共享注入）。
```

【步骤 6：Windows 注入停止（超过本地快照有效期后）】
```text
为避免再等待默认 30 分钟，本一次性客户端进程把 cache-max-age 显式设为 1 分钟；
  立即同步后等待超过 1 分钟再启动 MCP。
启动时执行本地快照新鲜度检查；权威端快照修订号与已同步缓存一致，因此未重复
  下载全量快照，只刷新联系时间。
同一固定盘符引用命令：exit_code=0；stderr_bytes=0；stdout 含 X-ABSENT；
  结构化结果仍无该共享注入。说明过期边界后的自动同步路径也没有恢复盘符注入。
偏离说明：该子步用 1 分钟一次性有效期代替默认 30 分钟等待，产品刷新边界相同；
  未改客户端持久配置。
```

【步骤 7：撤销标记清理复核】
```text
macOS unmount：exit=1；
  Error: 目标服务器上没有这个共享名的 sshmgr 挂载配置。
Linux unmount：exit=1；同一错误。
此时两台靶机的撤销标记文件均确实在场，且无配置、凭据或挂载。
期望：owner 依次用 shares unmount 清理 Linux 与 macOS 靶机上的撤销标记，
  并让 residue 复查为无残留。
判定：不满足；登记欠账第 57 条。收尾不绕过产品直接删远端文件，
  改用「重新授权 → 按新版本号挂载让产品删除旧标记 → 仍在范围内卸载 →
  再移出授权」的产品路径清干净 macOS；Linux 按保留状态重新挂载。
```

## 第六轮收尾

【虚拟机与镜像】
```text
一次性 macOS 虚拟机：已停止并删除；tart 本地虚拟机列表不再包含该虚拟机；
  无 tart run 进程。
远端最终核对：auto_master 引用=0；autofs 映射条目=0；挂载表共享挂载=0；
  撤销标记=0。
保留基础镜像：
  1. 本机 OCI 镜像层缓存：96 个清单层去重后 71 个 blob，26,988,883,052 字节，
     SHA-256 全部校验通过，占用约 26 GiB；
  2. tart OCI 缓存中的 macos-tahoe-base 基础镜像（50 GB 标称磁盘 / 约 33 GB 实占），
     本轮结束后占用约 30 GiB。
未来清理时：tart OCI 缓存用 `tart prune --oci`；镜像层缓存可在 blobs 目录内逐文件
  unlink 后移除空目录，不使用递归删除命令。
```

【演练权威端清理与保留】
```text
删除对象：macOS 一次性服务器条目（含登录私钥、sudo 凭据、主机密钥锚、profile 授权）、
  一次性拓扑设备与网络成员。删除后 servers show 与 devices show 均不存在。
共享前置数据保留：原 Linux 与 Windows 可挂载范围恢复；原挂载密码机密项值已从
  root 专属事实源恢复；Linux 受管服务器 A 重新真实挂载，现场探测为已挂载。
第二挂载密码：仅存在于权威端 root 专属临时文件，经标准输入使用后已删除；
  未进入命令参数、证据或审计。
反向隧道：两台演练机器的一次性 SSH 会话已停止，反向端口均无监听；
  authorized_keys 中的一次性公钥行删除前后计数 1 → 0。
root 专属一次性登录密钥与公钥已逐文件删除；权威端 serve 保持 0.22.0-rc11、
  Running / HEALTHY。
浏览器脚本与输出、Linux 客户端临时输出、NAS 本轮没有写入任何一次性文件。
保留客户端：e2e278-linux 与 e2e278-win 均完成最终 cache pull，
  分别回到 4 servers / 5 credentials 与 12 servers / 14 credentials。
```

【工作机 1 与本机清理】
```text
工作机 1：v0.21.1 一次性 README 与 exe 逐文件删除；bin/appdata/dek/store 四个空目录
  逐个移除；匹配文件数 0，匹配进程数 0。rc11 一次性目录与 e2e278-win 保留。
本机：临时 HTTP 分发端口与本地 OCI 镜像服务端口均无监听；相关 Python 进程 0；
  一次性 VM 登录密钥、反向隧道公钥、浏览器脚本、v0.21.1 下载物与临时日志逐文件删除。
```

【工作机 1 生产目录指纹（第六轮收尾）】
```text
文件数=5，整体 SHA-256=0a30ecdfb17b6f4454f0885cc394fd800b251bf8b9a992f3c0841596080165b8。
与第五轮收尾（本轮开始前保留状态）完全一致；本轮未改动工作机 1 生产目录。
```

【机密值命中计数与脱敏检索（第六轮新增内容）】
```text
检索范围：本轮追加到本运行记录与 docs/backlog.md 的 git diff 全量补丁；
  补丁经 SSH 会话传到演练权威端临时文件，两个值文件始终留在该机 root 目录，
  只用 grep -F -c -f 输出计数，随后逐文件删除临时补丁。
通用机密项测试值：命中 0。
NAS 一次性账号原密码：命中 0。
第二挂载密码：只经 root 专属文件与标准输入使用，测试后文件已删除；
  未进入任何证据文件。
第一轮 Brief 指定脱敏 grep：本运行记录 0；单机运行记录 0；
  docs/backlog.md 仅历史欠账第 17 条保留 1 处默认端口号原文，非本轮新增，未改动。
```

## 第七轮（rc12）安装与版本

【结论】演练环境升级到 0.22.0-rc12（源提交 `fb23583`），三个产物 SHA-256 全部核对一致；演练权威端服务重启后健康，两个保留客户端实例完成一次快照拉取与只读执行冒烟。

【升级与版本】
```text
本机产物核对：
  linux_amd64.tar.gz  dc908e850a41139ce52bbe751cc02df407651b43bec45ccabeb554977a0e5885
  darwin_arm64.tar.gz 01dafcd24298d50ce0bd113dba85e4d5f2c775dfd1e07875827e3fa603bddb85
  windows_amd64.zip   174ba40ece66e156f8d47cbaead9a090eae801cee944e1325627c16ddd86cc65

演练权威端：
  升级前 0.22.0-rc11 → 升级后 0.22.0-rc12；
  Linux 安装包经 SSH 会话分块传输（138 块 base64），远端重组后 SHA-256 与上表一致；
  /usr/local/bin/sshmgr 先备份为 /usr/local/bin/sshmgr.rc11，再替换；
  systemctl restart sshmgr-serve 后：
    service: Running / process: running / http: responding (401/200) / vault: ok / overall: HEALTHY

Linux 受管服务器 A：
  升级前 0.22.0-rc11 → 升级后 0.22.0-rc12；
  同样分块传输并核对 SHA-256 一致；备份 sshmgr.rc11 后替换；
  该机未安装 serve 服务，升级时既有共享挂载保持在场。

工作机 1：
  rc12 压缩包经本机临时 HTTP 服务下载，SHA-256 与上表一致；
  解压到 C:\sshmgr-e2e-20261114\bin-rc12\，目录内 1 个可执行文件；
  未加入 PATH，未触碰生产 sshmgr；bin-rc12\sshmgr.exe version → 0.22.0-rc12。
```

【保留实例快照拉取与冒烟】
```text
Linux 一次性客户端实例 e2e278-linux：
  cache pull → pulled 4 servers / 5 credentials；cache status → age 0s；
  MCP serverInfo.version → 0.22.0-rc12；
  exec_command（Linux 受管服务器 A 条目，只读）：exit_code=0，输出包含冒烟标记与 Linux 系统名。

Windows 一次性客户端实例：
  cache pull → pulled 12 servers / 14 credentials；age 0s；
  MCP serverInfo.version → 0.22.0-rc12；
  exec_command（Windows 受管服务器 A PowerShell 条目，只读）：
    exit_code=0；stdout 首行为冒烟标记；结构化结果按当时登记注入固定盘符。
```

偏离说明：Linux 客户端第一次直接执行 `cache pull` 时未自动带入既有实例的权威端地址与设备码，命令按提示要求显式环境变量；随后从实例自身的认证文件读取这些连接信息（不输出值）并成功拉取。该行为不属于权威端272-2 判据，未登记新欠账。

【本轮开始时工作机 1 生产目录指纹】文件数=5，整体 SHA-256=`99d595b1fecaa08c0e0cca0be938c5905b2934940fb884dd400cd564a0dec442`（逐文件 SHA-256 按完整路径排序拼接后再取整体 SHA-256）。收尾复核见第七轮收尾。

## 权威端272-2 复跑（第七轮 rc12，一次性 macOS 虚拟机）

### 一次性 macOS 虚拟机与前置

【结论】一次性 macOS 虚拟机可用，桥接本机有线网卡并拿到真实局域网地址，能访问 NAS A 的 SMB 端口；演练权威端按产品流程登记服务器条目、锚定主机密钥指纹并完成首次真实挂载。本轮完整复跑权威端272-2，判据以验收册为准，未放宽。

【虚拟机方案与登记】
```text
工具：tart 2.41.1（Apple Virtualization.framework 命令行虚拟机工具）。
基础镜像：本机已缓存的 macOS Tahoe 基础镜像（macOS 26.6.2，arm64，50 GB 标称磁盘）。
  本轮未重新下载镜像层；仅在克隆瞬间启动只绑定本机回环地址的一次性 OCI 镜像服务，
  从本机缓存读取清单与层，克隆完成后立即停止该服务。
克隆：从缓存基础镜像克隆为一次性虚拟机；Apple 文件系统采用写时复制，克隆约 0.05 秒完成。
运行：8 个虚拟 CPU、8192 MiB 内存、50 GB 磁盘；--no-graphics 后台运行并桥接本机有线网卡。
网络：虚拟机拿到物理局域网地址；对 NAS A 的 SMB 端口现场探测成功。
系统事实：虚拟机内置管理员账号可免密提权；/usr/sbin/automount 与 /etc/auto_master 在场。

受管连接：
  演练权威端生成一次性 ed25519 登录密钥，按服务器条目正常登记；
  主机 ED25519 指纹以带外方式锚定；首次产品连接输出 Darwin/arm64 并确认提权可用。
网络现实：本机所在物理网络不能直接路由到演练内网；虚拟机向演练权威端与
  Linux 一次性客户端实例宿主各保持一条仅转发 SSH 端口的一次性反向隧道。
  两条隧道只绑定对应机器的回环地址；产品服务器条目经反向隧道连接虚拟机。

共同前置：
  拓扑：新一次性设备加入 NAS A 所在的物理局域网网络；真实地址在证据中脱敏。
  授权：挂载密码机密项加入 macOS 一次性条目；Linux 一次性客户端实例的 profile 授权该条目。
  Linux 一次性客户端实例拉取后看到 5 servers / 7 credentials，
  经 MCP 对 macOS 一次性条目只读执行成功，MCP 版本 0.22.0-rc12。
  macOS 首次真实挂载：first-mount-success；版本号 34；挂载点 /mnt/sshmgr/<一次性共享名>；
  实际网络为与 NAS A 共同的物理局域网（第 1 个地址）；提示系统配置被重置，已重建。
  远端事实：auto_master 引用数=1；映射条目=1；挂载表 smbfs 数=1；撤销标记数=0。
```

【与物理 macOS 靶机的差异】本条使用 Apple 芯片虚拟机、桥接物理网卡与真实 NAS SMB 服务；CPU、网络适配器、电源与机型不是物理 Mac 硬件。共享挂载、autofs、提权、网络可达、权威端撤销与清理均为真实系统操作，不是模拟服务器。

### 权威端272-2 撤销、残留检查与换密码说明

【结论】通过。macOS 与 Linux 均先删除持久配置、再写撤销标记并卸载；residue 在只余标记时输出「仅有撤销标记」，`shares unmount` 校验共享名后删除标记并成功退出，复查为无残留。Windows 登记撤销、立即同步与超过本地快照有效期后的停止注入均通过。

【步骤 1：macOS 撤销与换密码说明】
```text
第二共享密码只由演练权威端 root 专属临时文件经标准输入提供给产品；
  未进入命令参数、证据、日志或审计，测试后恢复原机密项值并删除第二值文件。

shares revoke <一次性共享名> <macOS 一次性条目> --unmount --new-password-stdin --yes：
  版本号 35；远端持久配置已删除，挂载已卸载。
  输出包含「桌面上手动添加的共享连接也需要更换密码」。
  输出列出仍可挂载的 Linux 服务器，并提醒按新版本号重新挂载。
```

【步骤 2：macOS 残留事实与 residue】
```text
远端人工复核：
  auto_master_refs=0
  map_entry_count=0
  map_password_or_address_hits=0
  mount_table_count=0
  revoked_marker=sshmgr-share=<一次性共享名>;sshmgr-version=35;sshmgr-state=revoked

residue：
  服务器 <macOS 一次性条目>（macOS）：仅有撤销标记（撤销后的预期状态，不需要清理）
    <一次性共享名>（无需清理）：仅有撤销标记
  汇总：0 台有残留，1 台仅有撤销标记。
判定：欠账第 56 条在 macOS 侧通过。
```

【步骤 3–4：Linux 对照撤销与 residue】
```text
shares revoke <一次性共享名> <Linux 受管服务器 A> --unmount --yes：
  版本号 36；远端持久配置已删除，挂载已卸载。

远端人工复核：
  fstab_managed=0
  credentials_count=0
  cifs_mount_count=0
  revoked_marker=sshmgr-share=<一次性共享名>;sshmgr-version=36;sshmgr-state=revoked

residue：
  服务器 <Linux 受管服务器 A>（Linux）：仅有撤销标记（撤销后的预期状态，不需要清理）
  服务器 <macOS 一次性条目>（macOS）：仅有撤销标记（撤销后的预期状态，不需要清理）
  汇总：0 台有残留，2 台仅有撤销标记。
判定：欠账第 56 条在 Linux 侧同样通过。
```

【步骤 5：Windows 登记撤销】
```text
Windows PowerShell 条目撤销：版本号 37；未连接 Windows 服务器；
  各客户端同步快照后停止注入。
Windows cmd 条目撤销：版本号 38；同样未连接 Windows 服务器。
两条输出均说明：正在执行的命令保留盘符直到结束；长期离线的客户端同步前仍会注入；
  真正的撤销手段是更换共享密码。
```

【步骤 6：Windows 停止注入】
```text
立即同步后，同一固定盘符引用命令：
  exit_code=0；stderr_bytes=0；stdout 含 X-ABSENT；
  结构化结果不再出现该共享的 injected_network_drives 字段。
说明：第一次执行时输出字符串少了引号导致远端 PowerShell 把 X-ABSENT 当命令名，
  exit_code=1；该失败是测试脚本拼写错误，未触碰目标配置。修正引号后同参数重跑通过。

等待超过 1 分钟的一次性本地快照有效期后，以 cache-max-age=1m 启动 MCP：
  同一固定盘符引用命令 exit_code=0；stdout 含 X-ABSENT；结构化结果仍无该共享注入。
  权威端快照修订号与已同步缓存一致，只刷新联系时间，未恢复盘符注入。
偏离说明：用 1 分钟一次性有效期代替默认 30 分钟等待；产品过期边界相同，未改客户端持久配置。
```

【步骤 7：撤销标记清理与复查】
```text
macOS shares unmount <一次性共享名> <macOS 一次性条目> --yes：exit_code=0；
  「撤销标记已清理」；版本号 35；
  提示目标服务器上只剩撤销标记，已删除这个撤销标记，
  未发现挂载配置、凭据文件或挂载。

Linux shares unmount <一次性共享名> <Linux 受管服务器 A> --yes：exit_code=0；
  「撤销标记已清理」；版本号 36；同一提示。

复查 residue：
  服务器 <Linux 受管服务器 A>（Linux）：无残留
  服务器 <macOS 一次性条目>（macOS）：无残留
  汇总：0 台有残留，0 台仅有撤销标记，2 台无残留。

远端最终事实：
  macOS auto_master_refs=0 / map_entry_count=0 / mount_table_count=0 / revoked_marker_count=0
  Linux fstab_managed=0 / credentials_count=0 / cifs_mount_count=0 / revoked_marker_count=0
判定：欠账第 57 条在 Linux 与 macOS 侧均通过。
```

【审计】
```text
share-revoke：macOS 版本 35（password_changed=true）、Linux 版本 36、
  Windows PowerShell 版本 37、Windows cmd 版本 38 均有权威端审计行。
share-revoke-remote：Linux/macOS 远端清理成功，均有审计行。
share-residue-check：macOS 先为 marker-only，Linux 撤销后两台均 marker-only；
  清理后两台均为 no-residue。
share-unmount：两台 cleaned_revoked_marker=true、result=success、exit=0。
审计摘要不含共享地址、挂载账号或密码。
```

## 第七轮收尾

【共享前置数据恢复】
```text
挂载密码机密项值：从演练权威端 root 专属事实源按标准输入恢复；值未输出。
可写入服务器列表与共享可挂载范围：恢复为原有的 Linux 受管服务器 A、Linux 转发条目、
  Linux 一次性条目 B、Windows PowerShell 条目与 Windows cmd 条目。
Linux 受管服务器 A：重新真实挂载；现场探测为已挂载。
Linux 一次性客户端实例最终拉取：4 servers / 5 credentials。
Windows 一次性客户端实例最终拉取：12 servers / 14 credentials；
  最终固定盘符引用命令输出 X-PRESENT，结构化结果包含原共享注入，证明恢复到前置状态。
```

【一次性虚拟机与 vault 清理】
```text
反向隧道：先停止虚拟机内的一次性 macOS launchd 后台任务代理，两个远端回环监听端口均释放；
  两台演练机器 authorized_keys 中的一次性公钥行计数均 1 → 0。
虚拟机内一次性隧道密钥、启动脚本、日志与 launchd 配置文件逐文件删除；匹配路径 0、匹配进程 0。
macOS 一次性服务器条目、登录私钥、sudo 凭据、主机密钥锚、profile 授权、
  一次性拓扑设备与网络成员已删除；servers show 与 devices show 均不存在。
root 专属一次性登录密钥、公钥与第二密码文件逐文件删除。
```

【保留状态与指纹】
```text
演练权威端：0.22.0-rc12；serve Running / HEALTHY；serve.pairing=false；
  待批准配对列表为空。
Linux 受管服务器 A：0.22.0-rc12；共享前置数据恢复并已挂载；
  /usr/local/bin/sshmgr.rc11 备份保留。
Linux 一次性客户端实例与 Windows 一次性客户端实例保留，均完成最终快照拉取。
工作机 1：C:\sshmgr-e2e-20261114\ 保留，包含 bin-rc12；rc12 压缩包已删除；
  生产 sshmgr 未停止、未重启、未改动。
本机：一次性虚拟机已停止并删除；基础镜像缓存保留；本地 OCI 镜像服务与临时 HTTP
  分发端口均无监听；临时脚本、日志与传输块逐文件删除。

工作机 1 生产目录收尾指纹：
  文件数=5，整体 SHA-256=99d595b1fecaa08c0e0cca0be938c5905b2934940fb884dd400cd564a0dec442。
  与本轮开始前完全一致。
```

### 第七轮机密值命中计数与脱敏检索

```text
机密值检索范围：本轮追加到本运行记录与 docs/backlog.md 的 git diff 全量补丁；
  补丁传到演练权威端临时文件后，由 root 只读读取两个值文件并只输出字节命中计数。
通用机密项测试值：命中 0。
NAS 一次性账号原密码：命中 0。
第二共享密码：只存在于演练权威端 root 专属临时文件并经标准输入使用；
  未进入命令参数、证据或审计；测试后文件已删除。

第一轮 Brief 指定脱敏 grep：
  本运行记录命中 0；单机运行记录命中 0；
  docs/backlog.md 仅保留历史欠账第 17 条的 1 处默认端口号原文，非本轮新增，未改动。
```

## 发版检查清单核对（rc12）

核对时间：2026-11-15。核对对象为 0.22.0-rc12（源提交 `fb23583`）及本记录保留的
rc5、rc7、rc8、rc9、rc10、rc11 历史执行证据。本节只核对证据与处置，不修改
验收册、欠账清单或前文失败证据；同一条目多轮出现时，以最后一次完整执行的结论为准。

### 第 1 项：全部条目证据齐全

「单机记录」指
[2026-11-14 release87 单机运行记录](./2026-11-14-release87-single.md)；
未另注明的证据小节均在本多机运行记录。表中「缺证据」表示功能证据可能存在，
但按验收册仍缺必要的人工签收、子判据或留存输出，不能折算为通过。

| 条目编号 | 最后结论 | 执行轮次与版本 | 证据小节 |
| --- | --- | --- | --- |
| 单机270-1 | 通过 | 单机首轮，0.22.0-rc1 | 单机记录「单机270-1」 |
| 单机270-2 | 通过 | 单机首轮，0.22.0-rc1 | 单机记录「单机270-2」 |
| 单机270-3 | 通过 | 单机首轮，0.22.0-rc1 | 单机记录「单机270-3」 |
| 单机270-4 | 通过 | 单机首轮，0.22.0-rc1 | 单机记录「单机270-4」 |
| 单机270-5 | 通过 | 单机首轮，0.22.0-rc1；首次清理失败后重跑成功 | 单机记录「单机270-5」 |
| 单机271-1 | 通过 | 单机首轮，0.22.0-rc1 | 单机记录「单机271-1」 |
| 单机271-2 | 通过 | 单机首轮，0.22.0-rc1；首轮顺序失败后重建靶子重跑 | 单机记录「单机271-2」 |
| 单机271-3 | 通过 | 单机首轮候选源码自动化代证，0.22.0-rc1 | 单机记录「单机271-3」 |
| 单机271-4 | 通过 | 单机首轮候选源码自动化代证，0.22.0-rc1 | 单机记录「单机271-4」 |
| 单机272-1 | 通过 | 单机第三轮，0.22.0-rc1；密码长度阻塞解除后重跑 | 单机记录「单机272-1」 |
| 单机272-2 | 通过 | 单机第五轮，0.22.0-rc3 | 单机记录「单机272-2 / 第五轮」 |
| 单机272-3 | 通过 | 单机第六轮与重启复核，0.22.0-rc4 | 单机记录「单机272-3 / 第六轮」「单机272-3 / 第七节」 |
| 单机272-4 | 通过 | 单机首轮候选源码自动化代证，0.22.0-rc1 | 单机记录「单机272-4」 |
| 单机273-1 | 通过 | 单机首轮，0.22.0-rc1 | 单机记录「单机273-1」 |
| 单机273-2 | 通过 | 单机首轮，0.22.0-rc1 | 单机记录「单机273-2」 |
| 单机274-1 | 通过 | 单机第二轮，0.22.0-rc1 | 单机记录「单机274-1 / 第二轮」 |
| 单机274-2 | 通过 | 单机第六轮，0.22.0-rc4；此前失败证据保留 | 单机记录「单机274-2 / 第六轮」 |
| 单机274-3 | 缺证据：功能路径通过；主机密钥指纹人工比对由 agent 代证，缺 owner 签收 | 单机第七轮，0.22.0-rc5 | 单机记录「单机274-3 / 第七轮」 |
| 单机274-4 | 通过 | 单机第六轮，0.22.0-rc4；此前失败证据保留 | 单机记录「单机274-4 / 第六轮」 |
| 单机274-5 | 通过 | 单机第三轮，0.22.0-rc1 | 单机记录「单机274-5 / 第三轮」 |
| 单机274-6 | 通过 | 单机第三轮，0.22.0-rc1 | 单机记录「单机274-6 / 第三轮」 |
| 单机274-7 | 通过 | 单机第三轮，0.22.0-rc1 | 单机记录「单机274-7 / 第三轮」 |
| 单机275-1 | 通过 | 单机第二轮，0.22.0-rc1；第四轮用 rc2 复核清理问题 | 单机记录「单机275-1 / 第二轮」「单机275-1 / 第四轮」 |
| 单机276-1 | 通过 | 单机第四轮，0.22.0-rc2；第三轮失败证据保留 | 单机记录「单机276-1 / 第四轮」 |
| 单机276-2 | 缺证据：第 1–8 步功能通过；第 5 步短认证码双屏人工比对由 agent 代证，第 9 步清理未执行 | 多机第一轮，0.22.0-rc5 | 「单机276-2」 |
| 权威端270-1 | 通过 | 多机第一轮，0.22.0-rc5 | 「权威端270-1」 |
| 权威端270-2 | 通过 | 多机第一轮，0.22.0-rc5；误授权已撤销并重跑 | 「权威端270-2」 |
| 权威端270-3 | 通过 | 多机第一轮，0.22.0-rc5 | 「权威端270-3」 |
| 权威端271-1 | 通过 | 多机第一轮，0.22.0-rc5 | 「权威端271-1」 |
| 权威端271-2 | 缺证据：首次锚定转发链路通过；指纹人工比对由 agent 代证，缺 owner 签收 | 多机第一轮，0.22.0-rc5；欠账第 38 条在 rc7 复核 | 「权威端271-2」「欠账复核（第二轮 rc7）· 第 38 条」 |
| 权威端272-1 | 通过 | 多机第一轮，0.22.0-rc5 | 「权威端272-1」 |
| 权威端272-2 | 通过 | 第六轮 rc11 失败后在第七轮 rc12 全条复跑通过 | 「权威端272-2 复跑（第七轮 rc12，一次性 macOS 虚拟机）」 |
| 权威端272-3 | 通过 | 多机第三轮，0.22.0-rc8；rc7 失败证据保留 | 「权威端272-3」 |
| 权威端273-1 | 通过 | 多机第一轮，0.22.0-rc5 | 「权威端273-1」 |
| 权威端273-2 | 缺证据：登录、锁定、会话与审计通过；重验成功回跳原页、全部敏感写入口扫描未补证 | 多机第一轮，0.22.0-rc5 | 「权威端273-2」 |
| 权威端273-3 | 通过 | 多机第一轮，0.22.0-rc5 | 「权威端273-3」 |
| 权威端273-4 | 通过 | 多机第三轮，0.22.0-rc8；rc7 发现的问题已复核 | 「权威端273-4 / 第三轮（rc8）：第 48 条复核」 |
| 权威端274-1 | 通过 | 多机第二轮，0.22.0-rc7；位置移动提示补证完成 | 「权威端274-1」 |
| 权威端274-2 | 通过 | 多机第二轮，0.22.0-rc7 | 「权威端274-2」 |
| 权威端274-3 | 通过 | 第六轮，0.22.0-rc11；浏览器重验证子步分两遍补齐 | 「权威端274-3 共享存储授权、远端操作与桌面引导（macOS 补跑）」 |
| 权威端274-4 | 通过 | 多机第二轮，0.22.0-rc7 | 「权威端274-4」 |
| 权威端275-1 | 通过 | 第二轮，0.22.0-rc7；第一轮证书兼容失败证据保留 | 「权威端275-1 / 第二轮（rc7）复核结论」 |
| 权威端276-1 | 通过 | 第二轮，0.22.0-rc7；第 8 步清理按当轮执行说明处理，恢复后环境供后续票使用 | 「权威端276-1 / 第二轮（rc7）执行记录」 |
| 客户端270-1 | 通过 | 多机第一轮，0.22.0-rc5；附带旗标问题随第 37 条复核 | 「客户端270-1」「欠账复核（第二轮 rc7）· 第 37 条」 |
| 客户端270-2 | 通过 | 多机第一轮，0.22.0-rc5 | 「客户端270-2」 |
| 客户端270-3 | 通过 | 多机第一轮，0.22.0-rc5 | 「客户端270-3」 |
| 客户端270-4 | 缺证据：两次上传成功；权威端登记与替换审计、客户端完整审计和本地日志检索结论未按册留存 | 多机第一轮，0.22.0-rc5 | 「客户端270-4」「收尾与机密值检索」 |
| 客户端270-Windows-1 | 通过 | 多机第一轮，0.22.0-rc5 | 「客户端270-Windows-1」 |
| 客户端270-Windows-2 | 通过 | 第二轮补证，0.22.0-rc7；第一轮超时证据保留 | 「客户端270-Windows-2 / 第二轮（rc7）补证」 |
| 客户端270-路径-1 | 缺证据：反斜杠与正斜杠写入成功；权威端下发记录的正斜杠规范化详情未留存 | 多机第一轮，0.22.0-rc5 | 「客户端270-路径-1」「收尾与机密值检索」 |
| 客户端271-1 | 通过 | 多机第一轮，0.22.0-rc5 | 「客户端271-1」 |
| 客户端271-2 | 通过 | 第二轮补证，0.22.0-rc7；第一轮缺项证据保留 | 「客户端271-2 / 第二轮（rc7）补证」 |
| 客户端271-3 | 缺证据：首次连接与转发落库通过；指纹人工比对缺 owner 签收，第二次连接使用物理地址的本地观察摘要未留存 | 多机第一轮，0.22.0-rc5 | 「客户端271-3」「权威端271-2」 |
| 客户端272-1 | 通过 | 多机第一轮，0.22.0-rc5 | 「客户端272-1」 |
| 客户端272-2 | 通过 | 第三轮，0.22.0-rc8；rc7 失败证据保留 | 「客户端272-2 / 第三轮（rc8）」 |
| 客户端272-3 | 通过 | 第三轮，0.22.0-rc8；rc7 失败证据保留 | 「客户端272-3 / 第三轮（rc8）执行记录」 |
| 客户端273-1 | 通过 | 第三轮，0.22.0-rc8 | 「客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI」 |
| 客户端273-2 | 通过 | 第三轮，0.22.0-rc8 | 同上 |
| 客户端274-1 | 通过 | 第三轮，0.22.0-rc8 | 同上 |
| 客户端274-2 | 通过 | 第三轮，0.22.0-rc8 | 同上 |
| 客户端275-1 | 缺证据：向导、配对、只读界面与清理通过；短认证码双屏人工比对由 agent 代证，缺 owner 签收 | 第二轮，0.22.0-rc7 | 「客户端275-1 / 第二轮（rc7）执行记录」 |
| 混跑270-1 | 通过 | 第四轮，0.22.0-rc9 客户端版本为 v0.21.1，权威端为 rc9 | 「第四部分 新旧版本混跑（rc9 对 v0.21.1）」 |
| 混跑270-2 | 通过 | 第四轮，新客户端 0.22.0-rc9，旧权威端 v0.21.1 | 同上 |
| 混跑271-1 | 通过 | 第四轮，0.22.0-rc9 权威端与 v0.21.1 客户端 | 同上 |
| 混跑271-2 | 通过 | 第四轮主条目为 0.22.0-rc9 与 v0.21.1；第五轮 rc10 复核未锚定首连 | 「混跑271-2」「欠账复核（第五轮 rc10）· 第 55 条」 |
| 混跑272-1 | 通过 | 第四轮，0.22.0-rc9 权威端与 v0.21.1 客户端；多算法旧客户端限制登记第 54 条 | 「混跑272-1」 |
| 混跑272-2 | 通过 | 第四轮，新客户端 0.22.0-rc9，旧权威端 v0.21.1 | 「混跑272-2」 |
| 混跑275-1 | 通过 | 第四轮主条目为 0.22.0-rc9 与 v0.21.1；第五轮 rc10、第六轮 rc11 复核配对路径 | 「混跑275-1」「欠账复核（第五轮 rc10）· 第 53 条」「欠账复核（第六轮 rc11）」 |
| 混跑275-2 | 通过 | 第四轮，0.22.0-rc9 权威端与 v0.21.1 客户端 | 「混跑275-2」 |
| 混跑275-3 | 通过 | 第四轮，新客户端 0.22.0-rc9，旧权威端 v0.21.1 | 「混跑275-3」 |
| 混跑275-4 | 通过；另待 owner 确认是否需用正式版本产物抽查 | 第四轮，新客户端 0.22.0-rc9，旧权威端 v0.21.1 | 「混跑275-4」 |
| 混跑275-5 | 通过；另待 owner 确认是否需用正式版本产物抽查 | 第四轮，新客户端 0.22.0-rc9，旧权威端 v0.21.1 | 「混跑275-5」 |
| 人工270-1 | 缺证据：agent 检索计数为零，但 owner 对事实来源、三份证据文件与截图的最终人工对照未签收 | 单机第一至第七轮，0.22.0-rc1 至 rc5；多机第一至第七轮，rc5 至 rc12 | 单机记录「单机270-1」「收尾与机密值检索」；本记录各轮「收尾与机密值检索」 |
| 人工271-1 | 缺证据：多处指纹一致性由 agent 读取靶机事实代证，缺 owner「人工比对一致」结论 | 单机第七轮 rc5；多机第一轮 rc5 | 单机记录「单机274-3 / 第七轮」；本记录「权威端271-2」「客户端271-3」 |
| 人工272-1 | 已知缺口经 owner 处置：页面引导与无密码入口通过；Windows 桌面映射、写入读取、断开三步未完成 | 第三轮补跑，0.22.0-rc8；第五轮 rc10 仅复核 1219 提示 | 「人工272-1 Windows 桌面手动添加引导与盘符可见性目验（agent 代跑）」「欠账复核（第五轮 rc10）· 第 52 条」 |
| 人工273-1 | 缺证据：桌面与手机视口由 agent 模拟，真实手机观感与 owner 结论缺证据 | 多机第一轮与第三轮，0.22.0-rc5、rc8 | 「权威端273-4」「客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI」 |
| 人工273-2 | 缺证据：登录页证书指纹与权威端状态输出一致由 agent 代证，缺 owner 人工比对签收 | 多机第一轮，0.22.0-rc5；第二轮 rc7 复核证书算法 | 「权威端275-1」「欠账复核（第二轮 rc7）· 第 36 条」 |
| 人工274-1 | 缺证据：桌面浏览器与手机视口由 agent 代跑，页面观感、手机操作手感与安全判断缺 owner 结论 | 多机第一轮与第三轮，0.22.0-rc5、rc8 | 「权威端274-1」至「权威端274-4」；「客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI」 |
| 人工275-1 | 缺证据：多处两侧短认证码一致性由 agent 代证，缺 owner 双屏逐字比对与签收 | 多机第一轮 rc5、第二轮 rc7；单机276-2 同轮 | 「单机276-2」「客户端275-1」「权威端276-1」 |
| R01 | 通过 | 册面与册目核对，不依赖候选版本 | 验收册册首「替代旧册声明」；验收册目 `docs/acceptance/README.md` 中旧终端图形界面验收册的废弃标注 |
| R02 | 通过 | 四种组合均有执行记录：单机 rc1 至 rc5；权威端与客户端 rc5 至 rc12；混跑 rc9 对 v0.21.1 | 本表第 1 至第 4 部分条目行；单机记录同名条目小节 |
| R03 | 通过 | 汇总第 270 组条目，执行版本见各行 | 本表单机270、权威端270、客户端270、混跑270 行 |
| R04 | 通过 | 汇总各轮机密值检索，rc1 至 rc12 | 单机记录「收尾与机密值检索」；本记录各轮「收尾与机密值检索」 |
| R05 | 通过 | 汇总第 271 组条目，执行版本见各行 | 本表单机271、权威端271、客户端271、混跑271 行 |
| R06 | 缺证据：锚定与转发功能有证据；人工271-1 缺 owner 签收 | 汇总第 271 组相关条目 | 本表「人工271-1」「权威端271-2」「客户端271-3」 |
| R07 | 已知缺口经 owner 处置：共享存储功能路径补齐；人工272-1 桌面三步留本版缺口 | 汇总第 272 组相关条目，最终复核至 rc12 | 本表第 272 组条目行与「人工272-1」行 |
| R08 | 缺证据：Web UI 功能链路有证据；人工273-1 缺 owner 手机观感签收 | 汇总第 273 组相关条目 | 本表「人工273-1」及第 273 组条目行 |
| R09 | 缺证据：请求头、防跨站请求伪造与审计有证据；权威端273-2 两个重验证子判据缺证据 | 汇总第 273 组相关条目 | 本表「权威端273-2」 |
| R10a | 通过 | 基础页面等价面汇总；最终相关版本 rc1、rc5、rc8 | 单机记录「单机273-2」；本记录「权威端273-4」「客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI」 |
| R10b | 缺证据：新功能页面交互与等价面通过；人工274-1 缺 owner 观感与安全判断 | 汇总第 274 组相关条目，最终相关版本至 rc11 | 本表「人工274-1」及第 274 组条目行 |
| R11 | 缺证据：向导与配对链路有证据；人工275-1 缺 owner 双屏比对，单机276-2 第 9 步清理缺证据 | 汇总第 275 组相关条目 | 本表「人工275-1」「单机276-2」及第 275 组条目行 |
| R12 | 缺证据：Windows 反斜杠与正斜杠写入成功；下发记录正斜杠规范化详情缺证据 | 多机第一轮，0.22.0-rc5 | 本表「客户端270-路径-1」 |
| R13 | 通过 | 单机恢复 rc2；单机升级多机 rc5；权威端恢复 rc7 | 单机记录「单机276-1 / 第四轮」；本记录「单机276-2」「权威端276-1 / 第二轮（rc7）执行记录」 |
| R14 | 通过 | 册面核对，不依赖候选版本 | 验收册「条目模板」「取证模板」「人工保留面清单」 |
| R15 | 未满足：本节判定证据未齐全且存在 owner 破例与待确认事项 | 0.22.0-rc12 核对 | 本节全部小节 |

缺证据条目已在上表逐行标出。除人工272-1 已由 owner 明确处置外，其余缺证据
条目未见 owner 允许缺证据放行的记录，不能视为满足第 1 项。

### 第 2 项：失败项清零

最后一次执行结论不是「通过」的条目如下：

| 条目或覆盖项 | 情况与处置 | 证据位置 |
| --- | --- | --- |
| 人工270-1 | 缺 owner 最终人工对照与签收；待 owner 补跑或明确处置 | 本节第 1 项表；单机记录「收尾与机密值检索」 |
| 人工271-1 | 缺 owner 指纹人工比对结论；待 owner 补跑或明确处置 | 本记录「权威端271-2」「客户端271-3」 |
| 人工272-1 | 页面引导与无密码入口通过；桌面映射、写入读取、断开三步未完成。owner 2026-11-15 处置为本版已知缺口，带着放行；1219 提示已在 rc10 补上并复核通过；下个版本在没有同设备既有连接的 Windows 桌面补跑 | 「人工272-1」「欠账复核（第五轮 rc10）· 第 52 条」 |
| 人工273-1 | 真实手机观感缺 owner 结论；待 owner 补跑或明确处置 | 「权威端273-4」「客户端273-1 / 客户端273-2 / 客户端274-1 / 客户端274-2 客户端 Web UI」 |
| 人工273-2 | 证书公钥指纹人工比对缺 owner 结论；待 owner 补跑或明确处置 | 「权威端275-1」 |
| 人工274-1 | 桌面与手机观感、操作手感、安全判断缺 owner 结论；待 owner 补跑或明确处置 | 第 274 组相关小节 |
| 人工275-1 | 短认证码双屏人工比对缺 owner 结论；待 owner 补跑或明确处置 | 「单机276-2」「客户端275-1」「权威端276-1」 |
| 单机274-3 | 功能路径通过，但人工271-1 指纹比对缺 owner 签收；待补人工证据 | 单机记录「单机274-3 / 第七轮」 |
| 权威端271-2 | 转发落库通过，但人工271-1 指纹比对缺 owner 签收；待补人工证据 | 「权威端271-2」 |
| 权威端273-2 | 重验成功回跳原页、全部敏感写入口超时重验扫描未执行；待补跑 | 「权威端273-2」 |
| 客户端270-4 | 权威端登记与替换审计、客户端完整审计和本地日志检索结论未按册留存；待补证 | 「客户端270-4」 |
| 客户端270-路径-1 | 下发记录正斜杠规范化详情未留存；待补证 | 「客户端270-路径-1」 |
| 客户端271-3 | 人工指纹比对缺 owner 签收，第二次连接物理地址的本地观察摘要未留存；待补证 | 「客户端271-3」 |
| 客户端275-1 | 功能链路通过，但短认证码双屏人工比对缺 owner 签收；待补人工证据 | 「客户端275-1 / 第二轮（rc7）执行记录」 |
| 单机276-2 | 第 1–8 步通过；短认证码人工比对缺 owner 签收，第 9 步清理未执行；待补跑第 5 步人工面与第 9 步 | 「单机276-2」 |
| R06 | 因人工271-1 缺证据，覆盖项不能判通过 | 本节第 1 项表 |
| R07 | 因人工272-1 存在 owner 已处置的本版缺口，覆盖项不是全通过 | 本节第 1 项表 |
| R08 | 因人工273-1 缺证据，覆盖项不能判通过 | 本节第 1 项表 |
| R09 | 因权威端273-2 两个子判据缺证据，覆盖项不能判通过 | 本节第 1 项表 |
| R10b | 因人工274-1 缺证据，覆盖项不能判通过 | 本节第 1 项表 |
| R11 | 因人工275-1 与单机276-2 缺证据，覆盖项不能判通过 | 本节第 1 项表 |
| R12 | 因客户端270-路径-1 的规范化记录缺证据，覆盖项不能判通过 | 本节第 1 项表 |
| R15 | 发版检查清单第 1、2、5 项未全部满足，判定未放行 | 本节「发版检查清单核对结论」 |

owner 2026-11-15 对人工272-1 的处置与验收册检查清单第 2 项
「任何失败项都不得放行」的字面要求冲突。本节如实记录为 owner 破例，
不修改验收册，也不把该条折算为通过。欠账第 51 条所述 macOS 路径缺口已销项：
第六轮 rc11 复跑权威端274-3 通过，第七轮 rc12 在一次性 macOS 虚拟机复跑
权威端272-2 通过；这两条不再是本版已知缺口。

混跑275-4 与混跑275-5 的条目判据在 rc9 候选产物上通过。是否需要在正式
版本标记创建后再用正式产物抽查，列为「待 owner 确认」；本节不自行下结论。

### 第 3 项：兼容矩阵回写

已在本任务核对时向 `docs/compat-matrix.md`「已验证组合」表追加 0.22.0 相关行：

- 单机组合：rc1、rc2、rc3、rc4、rc5 的实际执行记录。
- 权威端与客户端同版本组合：rc5、rc7、rc8、rc9、rc10、rc11、rc12。
- 新旧版本混跑两个方向：0.22.0-rc9 客户端对 v0.21.1 权威端；
  v0.21.1 客户端对 0.22.0-rc9 权威端。
- 每行写执行日期、客户端先升与权威端服务后升的顺序、实际验证范围和
  本运行记录或单机运行记录的证据小节；未执行的组合未写成已验证。

### 第 4 项：失败与欠账登记

核对结论：历史产品失败与执行阻塞均已在 `docs/backlog.md` 第 18 条至第 57 条
登记，并写有修复、复核、owner 裁决或后续跟进；失败证据均保留在本运行记录或
单机运行记录，未删除。主要对应关系如下：

- 单机共享链路、备份恢复、Web UI 与主机信任失败：第 18 条至第 35 条。
- 多机第一轮至第三轮发现的配对、证书、命令行、共享与更新问题：第 36 条至第 50 条。
- macOS 权威端272-2、权威端274-3 原缺口：第 51 条；按最后执行已销项。
- 人工272-1 的 Windows 1219 缺口：第 52 条；owner 已处置为本版已知缺口，
  1219 提示在 rc10 复核通过，桌面三步下个版本补跑。
- 新旧版本混跑发现：第 53 条至第 55 条；第 53、55 条已在 rc10 或 rc11 复核通过，
  第 54 条仍登记为旧客户端多算法主机限制，后续说明方式待 owner 裁决。
- 第六轮权威端272-2暴露的残留状态与撤销标记清理问题：第 56 条、第 57 条；
  均在第七轮 rc12 复跑通过。

本节第 2 项列出的缺证据项多数是取证或人工签收缺口，不是已观察到的产品失败，
因此未要求逐条登记为产品欠账；它们仍阻塞检查清单第 1 项。人工272-1 的实际
失败与 owner 处置已登记在第 52 条，符合本项要求。

### 第 5 项：册目状态更新

已将 `docs/acceptance/README.md` 册目表 release-87 行更新为
「已执行，未放行」，并链接单机运行记录、本运行记录与本节核对小节；状态中
保留人工证据缺口、owner 破例与待 owner 确认事项，未改写其他册目行。

### 发版检查清单核对结论

当前不满足「本册发版验收通过」，结论为**未放行**。原因如下：

1. 人工270-1、人工271-1、人工273-1、人工273-2、人工274-1、人工275-1
   缺 owner 人工结论或签收；单机274-3、权威端271-2、客户端271-3、
   客户端275-1、单机276-2 因此也不能按册全条通过。
2. 权威端273-2、客户端270-4、客户端270-路径-1、单机276-2 第 9 步
   仍缺必要子判据或留存输出。
3. 人工272-1 为 owner 2026-11-15 明确破例携带的本版已知缺口；该处置与
   检查清单第 2 项字面冲突，本节如实记录，不将其视为条目通过。
4. 混跑275-4、混跑275-5 是否需在正式版本标记后用正式产物抽查，仍待 owner 确认。

在上述缺证据项补齐或获得 owner 明确处置，并完成 owner 确认的正式产物抽查
决策前，不得把本册标为发版验收通过。

## 缺证据子判据补证（第八轮 rc12）

执行时间：2026-11-15。对象为演练环境 0.22.0-rc12（源提交 `fb23583`）。本节只补
发版检查清单标出的缺证据子判据，不修改前文「发版检查清单核对（rc12）」；
本节也不给出整册放行结论。owner 不在场，允许 agent 代跑的浏览器与命令行动作
继续由 agent 代跑；本轮明确不做 owner 指纹比对和短认证码双屏人工比对。

### 权威端273-2 重验回跳与敏感写入口扫描

【结论】失败。第 5 步补证通过：超过 5 分零 1 秒后，修改口令页先跳到重新验证页，
正确口令重验后回到原修改口令页。第 6 步有三处不满足验收册：修改机密项备注或
环境变量名在未重验时直接写入；Web UI 缺少册面要求的导出或恢复备份写入口；
撤销一次性设备码在未重验时直接撤销。其余入口均先跳重新验证页且未写入靶子。

【第 5 步：重验成功回跳原页】
```text
真实 Chromium 从回环地址登录，等待 5 分零 1 秒后打开 /ui/password：
  HTTP 303 → /ui/reverify?next=/ui/password
  最终路径=/ui/reverify?next=/ui/password；页面含「重新验证 / 管理员口令 /
  验证并继续」。
输入正确管理员口令后提交 /ui/reverify：
  HTTP 303 → /ui/password
  最终路径=/ui/password；页面显示「修改管理员口令 / 新管理员口令 /
  再输入一次新口令」，未提交修改。
```

【第 6 步：全部敏感写入口扫描】使用同一个新登录会话，等待 5 分零 1 秒后逐项
发起真实浏览器表单提交。前两次脚本在选择器与服务器列表路径上出错，未发起对应
写请求；随后在同一判据下重新登录、重新等待 5 分零 1 秒并完整复跑，以下为最终
有效结果：

| 册面入口 | 未重验提交结果 | 靶子写入核对 |
| --- | --- | --- |
| 新增机密项 | HTTP 303 → `/ui/reverify?next=/ui/secrets/new?...&resume=1` | 新名字未出现 |
| 机密项换值 | HTTP 303 → `/ui/reverify?next=/ui/secrets/<一次性标识>/value?resume=1` | 值最后更改时间未变 |
| 修改机密项备注或环境变量名 | HTTP 303 → 机密项详情页，而非重新验证页 | 备注与环境变量名已改成提交值，随后用命令行恢复 |
| 删除机密项 | HTTP 303 → `/ui/reverify?next=/ui/secrets/<一次性标识>/delete?resume=1` | 机密项仍存在 |
| 立即备份 | HTTP 303 → `/ui/reverify?next=/ui/ops/backup` | 一次性备份目录文件数 0 → 0 |
| 删除一次性服务器条目 | HTTP 303 → `/ui/reverify?next=/ui/servers/<一次性标识>/delete` | 条目仍存在 |
| 导出或恢复备份 | 页面缺失 | 备份页与导航只提供立即备份、校验最新备份、保存备份配置，没有导出、导入或恢复备份入口 |
| 撤销一次性实例（设备码） | HTTP 303 → 设备码列表页，而非重新验证页 | 一次性设备码 active → revoked |

关键输出摘录：
```text
修改机密项元数据：
  POST /ui/secrets/<一次性标识>/edit → HTTP 303 Location=/ui/secrets/<一次性标识>
  页面回显新备注与新环境变量名；未进入 /ui/reverify。
  权威端审计：secret-edit / secret-env，来源 web_ui，状态 ok。
撤销一次性设备码：
  POST /ui/access/device-codes/<一次性设备码标识>/revoke
    → HTTP 303 Location=/ui/access/device-codes
  未进入 /ui/reverify；列表状态变为已撤销。
  权威端审计：cache-token.revoke，来源 web_ui，状态 ok。
扫描结束后正确口令重验：
  POST /ui/reverify → HTTP 303 Location=/ui/ops/backup；
  最终路径=/ui/ops/backup。
```

【判定】修改机密项元数据、导出或恢复备份入口、撤销设备码三处与验收册期望不符，
分别登记欠账第 58、59、60 条。第 5 步与新增、换值、删除机密项、立即备份、
删除服务器的重验拦截证据通过。

【清理】一次性机密项、服务器条目与自动生成的设备已删除；一次性备份配置恢复为
未配置，root 专属口令与值文件已删除；两枚本轮测试设备码均无 active 行，产品
保留 revoked 终态记录。浏览器脚本、截图与输出已逐文件删除。

### 客户端270-4 登记与替换审计、完整审计与本地日志

【结论】通过（补证）。第一轮同路径两次上传与最新记录已能从权威端和客户端审计
唯一对应；本轮补齐全量审计留存与两侧本地日志检索结论，不需要重放上写。

【权威端登记与替换审计】`sshmgr audit --action secret-delivery-record
--action secret-delivery-replace --limit 0` 共 14 行，与本条相关的有效链路：
```text
第一次上传：secret-delivery-record / ok / 来源=forward
  路径=<Linux一次性路径>；机密项=两个一次性名字；设备与实例=<Windows一次性实例>
同路径覆盖：secret-delivery-replace / ok / 来源=forward
  previous_path=<同一路径>；previous_secrets、previous_template、
  previous_sha256、previous_written_at 均进入审计；输出不含值。
owner 换值后第二次上传：secret-delivery-replace / ok / 来源=forward
  当前记录只保留新模板、新文件 SHA-256 与新值版本；旧记录进入审计。
```
`secrets records <通用机密项>` 显示同路径最新记录；模板只含占位符，文件哈希与
值版本号可见，机密项值不出现。

【客户端完整审计】Windows 一次性实例执行
`sshmgr cache audit --instance <Windows一次性实例> --limit 0`，全量 77 行。相关行：
```text
upload-content / ok：94 字节 → <Linux一次性路径>；两个机密项名；delivery-record=recorded
upload-content / ok：94 字节 → 同一路径；delivery-record=recorded
owner 换值后：
upload-content / ok：41 字节 → 同一路径；一个机密项名；delivery-record=recorded
```
本地审计只镜像 upload-content 结果，没有 `secret-delivery-record` 或
`secret-delivery-replace` 行，不是第二事实源。

【本地日志检索】把 Windows 客户端完整 `cache-audit.log` 与 Linux 客户端完整
`cache-audit.log` 传回演练权威端 root 专属临时文件，只与 root 专属值文件做
固定字符串命中计数，随后逐文件删除临时副本：
```text
Linux 客户端完整审计：70 行；通用机密项测试值命中 0；NAS 一次性账号原密码命中 0。
Windows 客户端完整审计：77 行；通用机密项测试值命中 0；NAS 一次性账号原密码命中 0。
权威端登记/替换审计 JSON：14 行；两个机密值命中均为 0。
权威端 serve.log：两个机密值命中均为 0。
第一轮有效上传返回均为「无 masked 字段」。
```

### 客户端270-路径-1 正斜杠规范化下发记录

【结论】通过（补证）。历史两条上传、远端长度检查与清理输出已在第一轮留存；
本轮补齐权威端下发记录详情，两个 Windows 路径均以正斜杠最终绝对路径落库。

【版本与修复线索】候选 0.22.0-rc12，源提交 `fb23583`；#47 修复合并提交
`05e538e`，实现提交 `eb7c249`。

【权威端记录】`sshmgr secrets records <通用机密项>` 相关详情：
```text
反斜杠输入产生的记录：
  路径=C:/<一次性工作目录>/path-bs.txt
  模板原文=path-test={{sshmgr-secret:<通用机密项>}}
  文件 SHA-256 哈希=21a456ec…；值版本号=1
正斜杠对照输入产生的记录：
  路径=C:/<一次性工作目录>/path-fs.txt
  模板原文=path-test={{sshmgr-secret:<通用机密项>}}
  文件 SHA-256 哈希=21a456ec…；值版本号=1
```
权威端审计同样保存 `path=C:/...`，不是 `C:\...`；两台客户端审计与权威端
serve.log 的机密值命中均为 0。两个远端文件与一次性服务器条目的清理证据沿用
第一轮收尾记录。

### 客户端271-3 第二次连接物理地址观察摘要

【结论】通过（仅补本子判据；owner 指纹人工比对仍留给 owner，不由 agent 代签）。
原目标条目、设备、物理与虚拟网络成员、既有转发锚和 Linux 一次性客户端仍在。
补证前先把客户端设备重新关联到当前 active 设备码，使最新快照重新携带本机设备
标识；这是恢复客户端自身拓扑状态，不改变目标锚。

【调用与本地观察】
```text
调用前时间：2026-11-15T09:42:50.114376297Z
exec_command(<原一次性目标>, "printf r8-2713-second-physical-after-self-id")
  → exit_code=0；stdout="r8-2713-second-physical-after-self-id"；
    stderr_bytes=0；MCP serverInfo.version=0.22.0-rc12
调用后时间：2026-11-15T09:42:51.835939855Z

probe-results.json 本次观察：
  虚拟局域网候选：ok=False，category=timeout，耗时 1500ms，
    observed_at=2026-11-15T09:42:51.723198271Z，source=dial。
ssh-dial-successes.json 同一目标：
  物理局域网地址成功，succeeded_at=2026-11-15T09:42:51.7501002Z，
    duration_ms=26（地址以代称记录）。
```

【锚未被覆盖】
```text
调用前 pin-forward 审计数=1；调用后计数=1。
锚显示前后逐字一致：source=forward，device=<Linux一次性实例>，
handshake=<虚拟局域网辅助地址>:22，created=<首次连接时间>。
```
因此第二次真实命令执行先尝试登记的虚拟候选，超时后按正常选址使用物理局域网
地址成功；没有新的锚转发或锚覆盖。

### 单机276-2 第 9 步清理

【结论】通过（在独立最小一次性靶子上补证）。原第 9 步依赖的一次性权威端已被
后续权威端276-1 的服务切换替换，不能直接清理当前保留的 rc12 权威端。按本轮
Brief 的重建条款，先新建最小单机升级多机靶子，再执行册面第 9 步清理；保留权威
端的 vault 目录在切换前整体移到 root 专属路径，最小靶子清理后原样恢复。

【最小靶子】
```text
保留权威端切换前：serve Running / HEALTHY。
serve uninstall → uninstalled service "sshmgr-serve"。
新建空 vault → init --role server → 已初始化为权威端。
serve admin set → 管理员口令已设置（口令仅 root 专属文件，不进证据）。
serve install --addr 0.0.0.0:<服务端口> → 服务已安装；
  等待后 Running / responding / vault ok / HEALTHY。
创建一次性 Profile、一个一次性机密项与一个一次性设备码；
  工作机 1 新一次性客户端实例 cache pull → 0 servers / 0 credentials，
  scope=<一次性Profile>，device=<一次性设备码>，版本 0.22.0-rc12。
```

【第 9 步清理】
```text
客户端：cache instances rm <新一次性实例> --yes
  → 已删除实例；槽目录与解密钥均清理。清理后列表仅剩既有保留实例。
权威端：cache-tokens revoke <新一次性设备码>
  → status=revoked；last_pull 与 client_version=0.22.0-rc12 留证。
serve uninstall → uninstalled service "sshmgr-serve"。
交互确认 sshmgr clear（输入 DELETE，再确认安全网口令已复制）：
  安全网加密导出已写入 root 专属一次性文件；口令输出已脱敏；
  → 已清理。下次 sshmgr ui 将重新进入初始化向导。
清理后：serve NOT INSTALLED / process not running；
  角色尚未初始化；vault 目录文件数 0；cache-tokens ls 因 vault locked 无法读出。
```

【保留权威端恢复与现场核对】
```text
最小靶子空 vault 目录移走后，原 rc12 vault 目录原样移回；
serve install --addr 0.0.0.0:<服务端口> → 服务已安装并启动；
最终：Running / process running / responding / vault ok / HEALTHY；
version=0.22.0-rc12；serve.pairing=false；待批准配对列表为空。
Linux 一次性客户端 cache pull → 4 servers / 5 credentials。
Windows 一次性客户端 cache pull → 12 servers / 14 credentials。
Linux 受管服务器 A 的共享前置挂载仍为 1 处 CIFS 挂载。
```

### 第八轮收尾与检查

【本轮清理】
```text
新建最小客户端实例：已删除，清理后仅剩两个既有保留实例。
新建最小权威端 vault、serve 服务、安全网导出、口令/值/设备码输出文件：
  均已清理；最小空 vault 目录已移除。
权威端273-2 的一次性机密项、服务器条目、自动设备、备份配置、root 专属值与
  口令文件、浏览器脚本/截图/输出：均已删除。
本轮测试设备码 active 行=0；产品保留 2 条 revoked 终态记录。
Linux 客户端本轮调用脚本与输出：已删除。
演练权威端、两台演练机与工作机 1 均无本轮临时 sshmgr ui / mcp / 浏览器进程。
本机无临时 HTTP 服务监听。
```

【工作机 1 生产目录指纹】本轮使用“排序路径 + 各文件 SHA-256”再整体 SHA-256：
```text
开始前：文件数 5，整体 SHA-256 31a448a9a7feb2b1e7d5fbc164a3b8f14431ec72f6ba30d2554a96123a54b4cd
收尾后：文件数 5，整体 SHA-256 31a448a9a7feb2b1e7d5fbc164a3b8f14431ec72f6ba30d2554a96123a54b4cd
结论：完全一致；未触碰生产 sshmgr。
```

【机密值与脱敏检索】本轮追加到运行记录与欠账清单的 git 差异补丁传回演练权威端
root 专属临时文件，只输出固定字符串命中计数后删除：
```text
通用机密项测试值：命中 0。
NAS 一次性账号原密码：命中 0。
两枚最小权威端一次性管理员口令：命中均为 0（口令文件已删除）。
第一轮 Brief 指定脱敏 grep：本运行记录 0；单机运行记录 0；
docs/backlog.md 仅历史第 17 条既有默认端口号原文 1 处，非本轮新增。
```

【偏离说明】
```text
1. 本会话仍无 mcp__ssh-manager__* 工具；按第一轮 Brief 的既定例外，远程执行走
   sshmgr ssh，未使用裸 ssh/scp。
2. 权威端273-2 前两次浏览器脚本分别为表单标签选择器与服务器列表路径出错，
   未发起对应写请求；修正后重新登录、重新等待 5 分零 1 秒并完整复跑有效。
3. 单机276-2 第 9 步在独立最小靶子上重建后清理；为满足收尾保留要求，切换期间
   把当前 rc12 vault 目录整体移到 root 专属路径，清理后原样恢复并复核健康。
4. 工作机 1 一次性目录中存在本轮开始前已有的 mcp-driver-r8.ps1，时间早于本轮，
   未识别为本轮新建文件，未删除。
```

## 第九轮（rc13）安装与版本

【时间与候选】2026-11-16，候选版本 0.22.0-rc13，源提交 `2810d4d`。本机仅用临时
HTTP 服务分发产物，结束前已停止；未使用裸 SSH 文件复制。

【产物校验】
```text
linux_amd64.tar.gz SHA-256=e2944573197d84746c1c205aefa3e6bbbcbc18b291bc4f20d385227e4fc0f41a
windows_amd64.zip SHA-256=a99100f77ce0e8750dcab099f999dbbd304b3896094bd4a4217428986b99b34d
darwin_arm64.tar.gz SHA-256=e58494bbfdb4c59774e342eaf4b2686031c68b5705e261aee90659bd53666e68
```

【演练机 1 与演练机 2】两台先备份 `/usr/local/bin/sshmgr.rc12`，再用校验通过的
Linux 产物替换当前二进制；备份与升级后哈希一致：
```text
sshmgr.rc12 SHA-256=3881f49f4d920d9ec81087013ec989520010c5d3f4438fc2a25b1a3f8775cb82
sshmgr      SHA-256=ca5e484945be0e521da09a655f3efc76091c2a9ebd0898496b64d7efffd323b8
两台 sshmgr version 均为 0.22.0-rc13。
```
演练权威端重启后：`service=Running`、`process=running`、`http=responding`、
`vault=ok`、`overall=HEALTHY`。收尾仍为 rc13 且健康。

【工作机 1】压缩包哈希核对通过后解压到一次性目录
`C:\sshmgr-e2e-20261114\bin-rc13\`，没有进入系统路径、没有安装服务、没有触碰
生产进程。一次性可执行文件版本为 0.22.0-rc13；本轮收尾删除压缩包，保留 bin-rc13
目录。生产目录采用“排序路径 + 各文件 SHA-256”再整体 SHA-256：
```text
开始前：5 个文件，整体 SHA-256=1f7d7ed6227e5bee67c97134057b98872793e2e36c345c27b5cabd5d51211fed
收尾后：5 个文件，整体 SHA-256=1f7d7ed6227e5bee67c97134057b98872793e2e36c345c27b5cabd5d51211fed
结论：完全一致。
```

## 权威端273-2 复跑（第九轮 rc13）

【结论】通过。以下为真机权威端、回环地址、真实 Chromium 的复跑结果；没有使用
测试环境变量缩短五分钟窗口，所有等待均为 5 分零 1 秒并分段输出进度。

### 第 1–5 步：登录、错误延迟、锁定、会话与重验

```text
正确登录：HTTP 303 → /ui/；
  cookie name=__Host-sshmgr_ui，Secure=True，HttpOnly=True，
  SameSite=Strict，Path=/（cookie 值未留存）。注销 HTTP 303 → /ui/login。

一次错误口令：HTTP 401，耗时 3081ms，页面显示「管理员口令不正确」；
随后正确登录：HTTP 303，耗时 4091ms，全局延迟 >=1 秒。

连续五次错误口令：第五次产生 admin.login_fail 与 admin.lockout；
  权威端命令解锁输出：来源 IP <回环地址> 的登录锁定已解除。
解锁后正确登录：HTTP 303，耗时 5082ms。

5 分钟内打开 /ui/password：直接显示「修改管理员口令」。
5 分零 1 秒后打开 /ui/password：
  最终路径=/ui/reverify?next=/ui/password；
  正确口令重验后最终路径=/ui/password。
```

### 第 6 步：敏感写入口逐项提交

每个入口都在 5 分零 1 秒无操作后提交；“未写入”用权威端命令行状态核对，
“成功”为重验回跳后再次提交的真实浏览器结果。

| 写入口 | 超时提交 | 未写入核对 | 重验后 |
| --- | --- | --- | --- |
| 新增机密项 | 跳重新验证页 | 列表中没有一次性机密项 | 回新增表单，名字、备注、环境变量名回显；重新填值后新增成功 |
| 机密项换值 | 跳重新验证页 | 值最后更改时间不变 | 回换值页；再次提交后值时间变化 |
| 修改机密项备注或环境变量名 | 跳重新验证页 | 备注与环境变量名均不变 | 回详情页并回显提交值；再次提交后两者变化 |
| 删除机密项 | 跳重新验证页 | 机密项仍存在 | 回删除确认页；再次确认后不存在 |
| 立即备份 | 跳重新验证页 | 备份总数 0→0 | 回备份页；再次提交后 0→1，页面显示已备份 |
| 删除一次性服务器条目 | 跳重新验证页 | 服务器条目仍存在 | 回删除确认页；再次确认后不存在 |
| 撤销一次性设备码 | 跳重新验证页 | 状态仍为 active | 回撤销确认页；再次确认后 revoked |

立即备份说明：第一次重验后的成功提交因测试准备的一次性备份目录缺少产品要求的
标记文件而失败，这是测试准备错误，不是重验守卫失败。补建标记文件后按原流程重新
等待 5 分零 1 秒并复跑该子步：超时提交仍先跳重新验证且 0→0，重验后 0→1 并显示
“已备份”。该重跑作为本条有效证据。

【第 8 步】登录页没有找回口令入口；源码模板将未设置口令时的唯一初始化入口写为
权威端主机命令 `sshmgr serve admin set`，浏览器没有重置或找回入口。

### 第 7 步自动化测试

在候选源码根目录执行验收册指定测试：
```text
=== RUN   TestAuthorityLoginExchangesPasswordForSecureSession --- PASS
=== RUN   TestAuthoritySessionSlidesAndGenerationInvalidates --- PASS
=== RUN   TestAuthorityPasswordChangeRequiresRecentReverification --- PASS
=== RUN   TestAuthorityLoginFailuresLockIPAndDelayAllRequests --- PASS
=== RUN   TestAuthorityLoginGlobalDelayIsCappedAtTenSeconds --- PASS
PASS
ok      ssh-manager-mcp/internal/webui      1.567s
```

【审计】成功链路可见 `admin.login`、`admin.login_fail`、`admin.lockout`、
`admin.logout`、`secret-add`、`secret-set`、`secret-edit`、`secret-env`、
`backup.create`、`server.delete`、`cache-token.revoke`，来源均为 Web UI。超时被拦截的
提交没有对应业务成功审计，目标状态也未变化。

### 欠账第 58、60 条复核

- 第 58 条通过：修改机密项备注或环境变量名在超时后先跳重新验证页，备注与环境
  变量名均未写入；重验后回详情页并回显提交值，再次提交成功。
- 第 60 条通过：撤销设备码在超时后先跳重新验证页，状态仍为 active；重验后回
  撤销确认页，再次确认后状态变为 revoked。

### 册外补充核对（rc13 新加守卫与不重验对照）

【授权分组新增】通过。超时提交跳重新验证页，授权分组不存在；重验后回授权分组
列表，重新填写一次性名称并提交成功，审计 `profile.add` 成功。

【项目令牌停用】通过。超时提交跳重新验证页，一次性项目仍为 active；重验后回
项目令牌列表并提示重新提交，再次点击停用后状态为 disabled，审计
`project.disable` 成功。

【配对拒绝】通过。临时打开 `serve.pairing` 后，用完全隔离的一次性客户端实例发起
配对申请；真实浏览器等待 5 分零 1 秒后点击拒绝，跳重新验证页且待批准数量保持
1；重验后回配对详情页，再次拒绝后待批准数量为 0，审计 `pair.reject` 成功。
结束后 `serve.pairing=false`，待批准列表为空。

【共享存储卸载】失败（守卫本身通过）。超时提交先跳重新验证页，Linux 受管服务器
A 的共享挂载数量保持 1→1，未写入；但正确重验后的自动回跳地址丢失目标服务器参
数，页面显示“缺少服务器”，不能从回跳页继续确认。测试者重新从共享详情进入该
服务器的卸载确认页后提交成功，远端挂载数量变为 0，随后重新挂载恢复为 1。因
“重验后成功”无法沿自动回跳连续完成，登记欠账第 61 条。

【拓扑成员编辑】通过。一次性拓扑设备与网络成员等待 5 分零 1 秒后经权威端拓扑
页面自己的接口提交：HTTP 200，最终仍在拓扑接口而不是重新验证页；设备在新虚拟
局域网的成员地址已修改，页面返回“已修改设备 … 的地址”。

【共享存储编辑】通过。一次性共享登记等待 5 分零 1 秒后提交备注与变更说明：
HTTP 303 回共享详情 `?saved=1`，没有跳重新验证页；命令行复核备注已写入、版本号
由 1 变为 2，审计 `share-edit` 成功。

### 第九轮收尾与清理

- 一次性机密项已删除；一次性服务器条目与自动生成设备已删除；一次性设备码无
  active 行，产品仅保留 revoked 终态记录。
- 一次性授权分组、项目令牌、共享登记、拓扑设备与网络成员均已删除；一次性备份
  配置、备份文件与标记文件已删除，配置恢复为未配置。
- 一次性配对客户端目录与日志已逐文件删除，空目录已移除；`serve.pairing=false`，
  待批准配对数为 0。
- Linux 受管服务器 A 的共享前置挂载恢复为 1 处；真实浏览器脚本、截图、输出与
  root 专属临时输出均已删除；本机临时 HTTP 服务已停止。
- 演练权威端保留 rc13 且 `Running / HEALTHY`；两台演练机保留 rc13 与 rc12 备份；
  工作机 1 保留一次性 bin-rc13 目录，不保留 rc13 压缩包。
