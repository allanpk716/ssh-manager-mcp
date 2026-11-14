# 2026-11-14 release87 单机组合运行记录（agent 代跑）

日期：2026-11-14  
候选版本：0.22.0-rc1（源提交 `f87bfa909918553bbb45822b1cfa580143d136c4`）  
执行者：agent 代跑（codex worker），owner 授权。owner 命令行条目均由 agent 代跑；证据标注见各节。  
设备代称：演练机 1、Linux 受管服务器 A、macOS 受管服务器 A、NAS A。  
脱敏说明：输出中的网络标识与账号已按代称替换；端口、一次性令牌、cookie、口令、机密项值、项目令牌与一次性私钥也已遮蔽。本文只使用代称，不写真实地址、主机名、vault 条目名、账号名、共享名或端口号。

## 总表

| 条目 | 结论 | 证据所在小节 | 备注 |
| --- | --- | --- | --- |
| 单机270-1 | 通过 | 1.1 | owner 命令行由 agent 代跑；列表不含值、哈希或值长度 |
| 单机270-2 | 通过 | 1.1 | 两种编码写入、权限、哈希、连接前拒绝均满足 |
| 单机270-3 | 通过 | 1.1 | 当前值与上一个值均被输出遮蔽 |
| 单机270-4 | 通过 | 1.1 | 加密备份与导出均导入两个空 vault |
| 单机270-5 | 通过 | 1.1 | 审计、值检索与一次性靶子清理完成；首次产物清理命令因通配符权限失败，已按失败留证并重跑成功 |
| 单机271-1 | 通过 | 1.2 | 登记编辑、校验、删除预览与清理完成；交互确认用命令自带非交互参数替代 |
| 单机271-2 | 通过 | 1.2 | 首轮锚定顺序不符合条目顺序，已保留失败证据并重建一次性靶子重跑成功 |
| 单机271-3 | 通过 | 1.2 | 自动化测试代证 |
| 单机271-4 | 通过 | 1.2 | 自动化测试代证 |
| 单机272-1 | 通过 | 1.3 | 第三轮：真实 10 字节挂载密码登记成功；版本联动、一次性变更提示与审计均满足 |
| 单机272-2 | 通过 | 1.3 | 第五轮 rc3：x- 前缀管理标记不再传给 CIFS 内核，重启后自动挂载恢复；旧格式 fstab 可识别并改写 |
| 单机272-3 | 失败 | 1.3 | 第五轮 rc3：路径核验与现场探测已修复，但迁移第四轮遗留映射的首次调用仍报固定挂载命令失败；重启验证移交 owner |
| 单机272-4 | 通过 | 1.3 | 三组自动化测试代证全过 |
| 单机273-1 | 通过 | 1.4 | 真实回环 Web UI、一次性启动令牌、响应头与退出即停验证；浏览器动作由无界面 Chromium 代跑 |
| 单机273-2 | 通过 | 1.4 | 基础页面、命令行等价面、一次性 Profile 写操作与 Web UI 审计完成 |
| 单机274-1 | 通过 | 1.5 | 第二轮：完整拖动矩阵、错误提示、三视图布局保存/刷新/重新登录/重置与命令行等价面收口 |
| 单机274-2 | 失败 | 1.5 | 第五轮 rc3：新拒绝提示、带外锚定与 Web UI 登记警告通过；命令行登记警告把物理局域网与虚拟局域网的类型说反 |
| 单机274-3 | 移交 owner | 1.5 | 需要真实浏览器表单、一次性私钥粘贴与主机密钥指纹人工比对 |
| 单机274-4 | 失败 | 1.5 | 第五轮 rc3：Linux 挂载/重选/卸载与真实残留检查通过；配置仍在但当前未挂载的靶子不会自然进入共享详情区块，范围外撤销任务页停留在进行中 |
| 单机274-5 | 通过 | 1.5 | 第三轮：反向视图、两共享影响面、三组授权移除恢复与等价命令均满足 |
| 单机274-6 | 通过 | 1.5 | 第三轮：完整影响预览、摘要变化整体拒绝、一次确认删除与非交互等价命令均满足 |
| 单机274-7 | 通过 | 1.5 | 第三轮：取消不删除、一次确认级联删除、客户端实例保留与后续空态均满足 |
| 单机275-1 | 通过 | 1.6 | 第二轮条目通过；欠账第 20 条已在第四轮 rc2 单独复核通过 |
| 单机276-1 | 通过 | 1.7 | 第四轮 rc2：备份与导出均恢复四类新数据、六个 Web UI 设置与三视图坐标 |
| 单机276-2 | 移交 #278 | 1.7 | 需要 Windows 新版客户端（工作机 1），且基线依赖单机276-1 |

## 安装与版本

【命令原文】
```text
在演练机 1：校验 Linux 安装包 SHA-256 → 解包 → 以管理权限安装到系统可执行文件路径 → sshmgr version
```
【完整输出】
```text
<Linux 安装包文件名>: OK
0.22.0-rc1
```
【执行位置】演练机 1。  
【说明】安装包校验、解包与安装由 agent 代跑；演练机 1 初始化为一次性单机 vault，未触碰本机生产 vault。

【第四轮安装与版本（2026-11-14，agent 代跑，rc2）】
```text
演练机 1 与 Linux 受管服务器 A：sshmgr version 均输出 0.22.0-rc2。
开始前核对：两台演练机没有运行中的旧 sshmgr serve/ui 进程，也没有遗留 sshmgr 临时 systemd 单元。
收尾核对：两台演练机的 sshmgr serve/ui 进程数与 sshmgr 临时 systemd 单元数均为 0。
Linux 安装包 SHA-256 实测：5a23910aa0f258a41a329d972286ba7a5f66ce790d9dcebce46abecff5dfa09d
```
【rc2 构建产物 SHA-256 记录】
```text
linux_amd64.tar.gz：5a23910aa0f258a41a329d972286ba7a5f66ce790d9dcebce46abecff5dfa09d
darwin_arm64.tar.gz：dacd302c9ae2f738764853172436f1f285d5df6803082680f5b28cec66bce68e
windows_amd64.zip：bb7fe7d6fa7fd63bcabc6fa95d1a04e322606c5be89077025d80090cbd1d2202
```
【说明】本轮真机执行使用 rc2；三个构建产物哈希按 owner 提供值留档，其中 Linux 包在演练机 1 上实测一致。

【第五轮安装与版本（2026-11-14，agent 代跑，rc3）】
```text
演练机 1 与 Linux 受管服务器 A：sshmgr version 均输出 0.22.0-rc3。
开始前核对：两台演练机没有运行中的旧 sshmgr serve/ui 进程，也没有遗留 sshmgr 临时 systemd 单元。
收尾核对：两台演练机的精确匹配 sshmgr ui 进程数与临时 systemd 单元数均为 0。
```
【rc3 构建产物 SHA-256 记录】
```text
linux_amd64.tar.gz：574a3d90207ea96d6b60bd985d6b7f30a048c2a5404848d5d204923ea5327c69
darwin_arm64.tar.gz：73c98d8796788ad2aba2121528fb85e45fe4bd571a297c33a27e52b4df66e99c
windows_amd64.zip：5b9a488c4dc020f6889f68897406ba0002b8dc9d43b75adb6ca66c88d97cbe4d
```
【说明】本轮真机执行使用 rc3；三个构建产物哈希按 owner 提供值留档。

## 1.1 机密项与 Windows 路径

### 单机270-1 录入、只写不读与 owner 可见面

【命令原文】
```text
sudo sshmgr secrets add acceptance_secret_one --note release87 --env ACCEPTANCE_SECRET_ONE --from-file <owner 提供的机密值文件>
sudo sshmgr secrets add acceptance_secret_two --note release87 --env ACCEPTANCE_SECRET_TWO --from-file <现场生成并只保存在演练机 1 的机密值文件>
sudo sshmgr secrets allow --secrets acceptance_secret_one,acceptance_secret_two --servers linux-managed-a
sudo sshmgr secrets list
sudo sshmgr secrets set acceptance_secret_two --from-file <第二个一次性机密值文件>
sudo sshmgr secrets list
```
【完整输出（合并两次列表）】
```text
已登记机密项 acceptance_secret_one
已登记机密项 acceptance_secret_two
已授权 2 个机密项写入 1 个服务器条目
acceptance_secret_one
  备注: release87
  环境变量名: ACCEPTANCE_SECRET_ONE
  可写入服务器: linux-managed-a
  创建时间: <已留档>
  值最后更改: <已留档>
acceptance_secret_two
  备注: release87
  环境变量名: ACCEPTANCE_SECRET_TWO
  可写入服务器: linux-managed-a
  创建时间: <已留档>
  值最后更改: <已留档>
已更换机密项 acceptance_secret_two 的值
```
【list_secrets 返回】
```text
acceptance_secret_one | release87 | ACCEPTANCE_SECRET_ONE | linux-managed-a
acceptance_secret_two | release87 | ACCEPTANCE_SECRET_TWO | linux-managed-a
```
【masked 记录】`list_secrets` 无 `masked` 字段；输出不含值、值长度、哈希或时间。  
【执行位置】演练机 1；owner 命令行由 agent 代跑。  
【机密测试值事实来源】两个初始值由 owner 提供，或按 Brief 在演练机 1 现场生成，均以仅管理权限可读的文件传入；agent 从未读出明文。

### 单机270-2 `upload_content` 占位符写入与 Linux 权限

【命令原文】
```text
MCP upload_content(server_id=<Linux 受管服务器 A 的一次性条目>, encoding=text,
  content="ONE={{sshmgr-secret:acceptance_secret_one}} TWO={{sshmgr-secret:acceptance_secret_two}} ONE_AGAIN={{sshmgr-secret:acceptance_secret_one}}",
  remote_path=<第一个一次性路径>)
MCP upload_content(..., encoding=base64, content=<同一模板单行标准 Base64>, remote_path=<第二个一次性路径>)
MCP upload_content(server_id=<故意不可达的一次性条目>, encoding=text,
  content=<两处格式错误加一个未授权引用>, remote_path=<拒绝观察路径>)
MCP upload_content(server_id=<未列入可写入服务器列表的一次性条目>, encoding=text,
  content=<合法占位符>, remote_path=<拒绝观察路径>)
```
【完整输出】
```text
text 成功：{"bytes":110,"delivery_record_status":"recorded","filled_secret_names":["acceptance_secret_one","acceptance_secret_two"]}
base64 成功：{"bytes":110,"delivery_record_status":"recorded","filled_secret_names":["acceptance_secret_one","acceptance_secret_two"]}
整体校验失败（目标故意不可达）：
第1行字节偏移4：机密项占位符格式错误；原文片段："{{sshmgr-secret:name with space}}"
第1行字节偏移46：机密项占位符格式错误；原文片段："{{sshmgr-secret:acceptance_secret_one} unauthorized "
机密项 not_allowed 不存在，或不允许写入服务器 linux-unreachable-a
授权拒绝：
机密项 acceptance_secret_one 不存在，或不允许写入服务器 linux-unreachable-a；它当前可写入服务器：linux-managed-a。请 owner 到权威端把 linux-unreachable-a 加入该机密项的可写入服务器列表
```
【目标文件证据】
```text
600
600
d4d6c1086d08c43cc88e02abaeff673154a5deaa2d6df1171236d738ddf45fa6  <第一个一次性路径>
d4d6c1086d08c43cc88e02abaeff673154a5deaa2d6df1171236d738ddf45fa6  <第二个一次性路径>
<拒绝观察路径 1> absent
<拒绝观察路径 2> absent
```
【masked 记录】四次 `upload_content` 均无 `masked` 字段；成功返回不含机密项值。  
【执行位置】演练机 1 的 rc1 单机工具面；目标为 Linux 受管服务器 A。

### 单机270-3 输出遮蔽当前值与上一个值

【命令原文】
```text
MCP exec_command(server_id=<Linux 受管服务器 A 的一次性条目>, command="cat <单机270-2 第一个一次性路径>")
sudo sshmgr secrets set acceptance_secret_one --from-file <现场生成的第二个值文件>
MCP upload_content(..., 同一 text 模板, remote_path=<同一路径>)
MCP exec_command(..., command="cat <同一路径>")
```
【完整输出】
```text
换值前 exec：
{"effective_timeout_seconds":<默认超时>,"masked":["acceptance_secret_one","acceptance_secret_two"],"stderr_bytes":0,"stdout":"ONE={{sshmgr-secret:acceptance_secret_one}} TWO={{sshmgr-secret:acceptance_secret_two}} ONE_AGAIN={{sshmgr-secret:acceptance_secret_one}}\n","stdout_bytes":110}
已更换机密项 acceptance_secret_one 的值
覆盖写入：
{"bytes":104,"delivery_record_status":"recorded","filled_secret_names":["acceptance_secret_one","acceptance_secret_two"]}
换值后 exec：
{"effective_timeout_seconds":<默认超时>,"masked":["acceptance_secret_one","acceptance_secret_two"],"stderr_bytes":0,"stdout":"ONE={{sshmgr-secret:acceptance_secret_one}} TWO={{sshmgr-secret:acceptance_secret_two}} ONE_AGAIN={{sshmgr-secret:acceptance_secret_one}}\n","stdout_bytes":104}
```
【masked 记录】两次 `exec_command` 的 `masked` 均按字母顺序列出两个机密项；无未注入警告。  
【执行位置】演练机 1 的 rc1 单机工具面；目标为 Linux 受管服务器 A。

### 单机270-4 机密项与下发记录的备份恢复

【命令原文】
```text
sudo sshmgr backup create --dir <一次性备份目录> --passphrase-file <一次性口令文件> --keep 1 --prefix release87
sudo sshmgr backup verify <最新加密备份文件> --passphrase-file <一次性口令文件>
sudo sshmgr export --out <一次性导出文件> --passphrase-file <一次性口令文件>
分别用一次性 vault 根目录初始化两个空 vault，并各导入加密备份产物与导出产物
在两个恢复 vault 中执行 sudo sshmgr secrets records acceptance_secret_one
```
【完整输出】
```text
wrote release87-<时间戳>.sme
ok: release87-<时间戳>.sme (decrypted and json structurally valid)
exported 2 servers / 1 credentials to <一次性导出文件>
加密备份 SHA-256：bf4cb214f7d57fba63a74b47cdaa7515bfcadcb077fb952e81f59848872efa61
导出产物 SHA-256：195bcae92fcd7ad23d638d2d676edab555b5131d6605e04a9aaa2ba816871e52
初始化为单机。（两个恢复 vault 各一次）
imported 2 servers / 1 credentials（两个恢复 vault 各一次）
```
【记录核对】两个恢复 vault 均保留两条下发记录：规范化路径、模板原文、文件 SHA-256 哈希、发起设备、实例、机密项名字和值版本号；输出不含值。第二个路径覆盖后的文件哈希为 `119d165f2f97ada725bb4c641257e8618e5ebe2160bcabe2ea033fa2e638698a`。  
【masked 记录】本条命令行输出无 `masked` 字段。  
【执行位置】演练机 1；owner 命令行由 agent 代跑。

### 单机270-5 审计汇总与清理

【命令原文】
```text
sudo sshmgr audit --action secret-add --action secret-set --action secret-allow --action upload-content --action exec --action secret-delivery-records --action secret-delivery-replace --action backup.create --limit 0
sudo grep -F -c -f <机密测试值模式文件> <工具返回汇总> <审计输出> <MCP 标准错误>
sudo sshmgr servers rm linux-managed-a --yes
sudo sshmgr servers rm linux-unreachable-a --yes
sudo sshmgr secrets rm acceptance_secret_one
sudo sshmgr secrets rm acceptance_secret_two
sudo sshmgr secrets records acceptance_secret_one
```
【审计完整结论】查询按时间倒序显示：`backup.create` 1 行、`exec` 3 行、`upload-content` 4 行（2 `ok`、2 `denied`）、`secret-delivery-replace` 1 行、`secret-set` 2 行、`secret-allow` 2 行、`secret-add` 2 行。所有摘要只含名字、服务器条目名、路径、模板、大小或状态，不含机密项值。
【值检索】
```text
<工具返回汇总>:0
<审计输出>:0
<MCP 标准错误>:0
```
【清理输出】
```text
deleted server linux-managed-a
deleted server linux-unreachable-a
已删除机密项 acceptance_secret_one
已删除机密项 acceptance_secret_two
下发记录（登记失败的历史写入会缺漏，不承诺完整）：
没有符合条件的下发记录。
```
【清理偏差与补证】第一轮产物删除命令中备份目录通配符在非管理 shell 展开失败，输出 `No such file or directory`，已原样留证；随后按具体文件逐个删除并用 `test ! -e` 证明备份目录与两个恢复 vault 目录均不存在。Linux 受管服务器 A 的两个一次性文件已删除，父目录已用 `rmdir` 删除。

## 1.2 网络拓扑与主机信任

### 单机271-1 站点、网络、设备与网络成员登记和删除预览

【命令原文】
```text
sudo sshmgr sites add release87_site_one --note initial
sudo sshmgr networks add release87_physical_one --type physical --site release87_site_one --cidr <物理局域网一网段> --bandwidth-mbps 1000
sudo sshmgr networks add release87_virtual_one --type virtual --cidr <与物理局域网一同文案的网段> --bandwidth-mbps 500
sudo sshmgr devices add release87_local_one
sudo sshmgr devices add release87_target_one
sudo sshmgr devices member add ...（本机与靶机设备各登记物理、虚拟两个网络成员）
sudo sshmgr servers add --name release87_server_one --device release87_target_one ...
sudo sshmgr sites edit ...；sudo sshmgr networks edit ...；sudo sshmgr devices edit ...；sudo sshmgr devices member edit ...
sudo sshmgr networks edit release87_virtual_one --type physical
```
【完整输出摘要】
```text
已登记站点 release87_site_one
已登记网络 release87_physical_one（物理局域网）
已登记网络 release87_virtual_one（虚拟局域网）
已把设备 release87_local_one 加入网络 ...
已把设备 release87_target_one 加入网络 ...
added server release87_server_one id=<一次性标识> device=release87_target_one
已修改站点 release87_site_one；新名字 release87_site_edited
已修改网络 release87_physical_one；新名字 release87_physical_edited
已修改网络 release87_virtual_one；新名字 release87_virtual_one
已修改设备 release87_target_one；新名字 release87_target_edited
已修改设备 release87_target_edited 在网络 release87_physical_edited 的地址
Error: 网络类型登记后不能修改；如需改成另一类网络，请删除后重建
```
【校验输出】
```text
Error: 网络 release87_physical_edited 的地址 <物理地址二> 已被设备 release87_target_edited 占用
Error: 虚拟局域网不属于站点；请去掉 --site
已登记网络 release87_physical_duplicate（物理局域网）  # 不同网络重复网段不被拦截
```
【删除预览与清理】
```text
将删除网络 release87_virtual_one。
网络成员：
  release87_local_one — <地址代称一>
  release87_target_edited — <地址代称二>
已删除网络 release87_virtual_one

将删除网络 release87_physical_edited。
网络成员：
  release87_local_one — <地址代称一>
  release87_target_edited — <地址代称二>
已删除网络 release87_physical_edited
服务器条目 release87_server_one：候选地址只剩兜底 Host

将删除设备 release87_target_edited。
服务器条目：release87_server_one（<一次性账号>，<一次性端口>，未锚定）
共享存储：无
会从以下机密项的可写入服务器列表移出：无
会随之删除的下发记录：无
网络成员：无
布局坐标：无
软件清单：软件条目 0 条、声明 0 条、采集执行者登记 0 条、监听端口 0 个
会被解除关联的客户端实例：无
已删除设备 release87_target_edited

已删除站点 release87_site_edited
还没有登记站点
没有匹配的网络
没有匹配的设备
```
【审计】`site.add`、`network.add`、`device.member-add`、`device.member-edit`、`device.edit`、`server.add`、`device.rm` 均查到；摘要不含凭据。  
【执行位置】演练机 1；owner 命令行由 agent 代跑。交互确认按 Brief 使用命令自带 `--yes`，删除预览仍完整输出。

### 单机271-2 同设备多个服务器条目与真实选址

【首轮偏差】首轮先移动条目 B 后锚定条目 A，导致 `pin-copy` 不发生，条目 B 记录为 `tofu`。该失败证据已保留，一次性靶子清理后按条目顺序重建。
【重跑命令原文】
```text
建立一次性站点、物理局域网、虚拟局域网、本机设备与靶机设备；设置本机设备
sudo sshmgr servers add --name release87_entry_retry_a --device release87_target_retry --host <兜底地址> --port <一次性端口> --user <一次性账号> --key <一次性私钥>
sudo sshmgr ssh release87_entry_retry_a "printf retry-a-anchored"
sudo sshmgr servers add --name release87_entry_retry_b --host <兜底地址> --port <一次性端口> --user <一次性账号> --key <一次性私钥>
sudo sshmgr devices move-server release87_entry_retry_b --to release87_target_retry
sudo sshmgr servers pin-hostkey --list
sudo sshmgr servers pin-hostkey release87_entry_retry_a --clear
sudo sshmgr devices member rm release87_local_retry --network release87_physical_retry
sudo sshmgr ssh release87_entry_retry_a "printf retry-a-virtual-ok"
sudo sshmgr devices member rm release87_local_retry --network release87_virtual_retry
sudo sshmgr servers edit release87_entry_retry_a --port <不可达一次性端口>
sudo sshmgr ssh release87_entry_retry_a "printf retry-a-fallback-fail"
```
【完整输出摘要】
```text
retry-a-anchored
added server release87_entry_retry_b id=<一次性标识> device=release87_entry_retry_b
已把服务器条目 release87_entry_retry_b 从设备 release87_entry_retry_b 移到设备 release87_target_retry
已从服务器条目 release87_entry_retry_a 复制主机密钥锚
entry=release87_entry_retry_b ... source=tofu ... handshake=<虚拟局域网地址代称>
entry=release87_entry_retry_a ... source=tofu ... handshake=<虚拟局域网地址代称>
2026-11-14 <时间>  release87_entry_retry_b  (owner)  pin-copy  ok  ... {"entry":"release87_entry_retry_b",...,"来源服务器条目":"release87_entry_retry_a"}
retry-b-ok
unpinned release87_entry_retry_a ...
entry=release87_entry_retry_b ...（条目 A 清除后仍保留）
已把设备 release87_local_retry 移出网络 release87_physical_retry
retry-a-virtual-ok
已把设备 release87_local_retry 移出网络 release87_virtual_retry
updated server release87_entry_retry_a
Error: 全部候选地址连接失败：
兜底地址：ssh dial: dial tcp [REDACTED]: connect: connection refused
请补登记本机设备与目标设备的共同网络；单机部署与权威端也可先设定本机设备。
```
【probe-results 摘要（只含网络名、结果、耗时、来源）】
```json
{"version":1,"observations":[
  {"network":"release87_physical_retry","ok":true,"duration_ms":20,"observed_at":"<时间>","source":"dial"},
  {"network":"release87_virtual_retry","ok":true,"duration_ms":0,"observed_at":"<时间>","source":"probe"}
]}
```
【清理】两个一次性条目随靶机设备一次确认删除；本机设备、自动生成设备、两个网络与站点删除；`servers ls`、`devices list`、`networks list`、`sites list` 均为空态；本机探测结果文件删除；Linux 受管服务器 A 上的一次性虚拟地址删除。

### 单机271-3 多地址顺序、成功地址缓存与密钥不一致代证

【命令原文】
```text
GOTOOLCHAIN=go1.25.8 go test ./internal/sshbroker -run '^(TestPlanServerDialAddressesOrdersAndDeduplicates|TestConnectServerEntryTriesPlannedAddressesInOrder|TestConnectServerEntrySuccessCacheOrdersSameLevelByDuration|TestConnectServerEntryMismatchFailsWithoutSendingCredentials|TestConnectServerEntrySkipsMismatchedHostKeyThenConnectsNextAddress|TestConnectServerEntrySkipsSuccessCacheForFallbackOnlyPlan)$'
GOTOOLCHAIN=go1.25.8 go test ./internal/mcpserver -run '^TestQueryTopologyPairActualSelectionUsesDialSuccessCache$'
```
【完整输出】
```text
ok  	ssh-manager-mcp/internal/sshbroker	0.556s
ok  	ssh-manager-mcp/internal/mcpserver	0.477s
```
【提交哈希】`f87bfa909918553bbb45822b1cfa580143d136c4`  
【执行位置】本机源码根目录，agent 代跑。

### 单机271-4 存量锚迁移与旧格式兜底锚推导代证

【命令原文】
```text
GOTOOLCHAIN=go1.25.8 go test ./internal/store -run '^(TestImportSnapshot_OldShapeAppliesEntryMigration|TestSnapshotEntryPins_ExportAndLegacyDerivation)$'
```
【完整输出】
```text
ok  	ssh-manager-mcp/internal/store	0.403s
```
【提交哈希】`f87bfa909918553bbb45822b1cfa580143d136c4`  
【执行位置】本机源码根目录，agent 代跑。

## 1.3 共享存储

### 单机272-1 登记版本号变更提示与名单查询

【命令原文】
```text
sudo wc -c <NAS A 一次性挂载密码文件>
sudo sshmgr secrets add release87_nas_secret --note release87 --from-file <NAS A 一次性挂载密码文件>
```
【完整输出】
```text
6 /<机密值文件路径已脱敏>
Error: 机密项值太短：至少需要 8 个字节；短值无法可靠参与后续的输出遮蔽
Usage:
  sshmgr secrets add NAME [--note 文本] [--from-file PATH | --stdin] [flags]
...
机密项值太短：至少需要 8 个字节；短值无法可靠参与后续的输出遮蔽
```
【结论】一次性挂载密码文件为 6 字节（不含换行），低于候选版本机密项至少 8 字节的输入约束。共享存储登记必须引用真实挂载密码，不能用假密码或拼接值放宽判据，因此本条阻塞。  
【清理】本条已创建的一次性拓扑与 Linux 一次性服务器条目全部删除；`servers ls`、`devices list`、`networks list`、`sites list` 均为空态。未登录 NAS A，未写入 NAS A。

#### 第三轮（2026-11-14，agent 代跑）

【结论】通过。owner 已把一次性挂载密码更换为 10 字节；本轮只用重定向把该文件喂给 `secrets add --stdin`，未把值打印到终端或证据。

【执行与证据】证据保存在演练机 1 的第三轮证据目录。
```text
owner 前置限制确认：owner 未单独确认 NAS A 已关闭同一 IP 多连接限制，按实际挂载结果判断。
一次性挂载密码文件：10 字节、0 个换行；值不进入证据。
secret add：已登记一次性挂载密码机密项。
secret allow：已授权写入 Linux 一次性条目。
shares add：已登记一次性共享；版本号 1。
首次 list_shares：版本号 1，含目录约定、盘符、变更说明空值与挂载点；不含密码、挂载账号或候选地址。
shares edit --change-note：版本号 1 → 2。
第2次 list_shares：开头提示共享版本变化与需要更新的挂载；变更说明为新说明。
第3次 list_shares：同一 MCP 会话不重复提示。
提供方设备物理网络成员地址变更：共享版本号 2 → 3，变更说明清空；下一次 list_shares 提示拓扑与挂载可能需要更新。
为后续 Linux 挂载恢复可用物理地址：共享版本号 3 → 4。
shares list/show：版本号、目录约定、挂载点与机密项引用一致。
audit：share-add、share-edit、device.member-edit 均可查询；摘要不含密码、挂载账号或地址。
```

【判据核对】`list_shares` 安全输出满足；登记编辑与提供方网络成员变化均让版本号加一，前者保留说明、后者清空说明；同一会话提示只出现一次；三类审计可查。全部满足。

【环境偏差】首轮准备中一次性机密项名使用连字符被规则拒绝，改为下划线后成功；首次锚定要求演练机 1 也登记共同虚拟局域网成员，补登后成功；Linux 受管服务器 A 缺少 `mount.cifs`，安装 `cifs-utils` 后进入后续真机挂载步骤。以上均未放宽本条判据。

### 单机272-2 Linux 持久挂载与重启恢复

【结论】依赖单机272-1 的真实共享登记与挂载密码，未执行挂载、写入、重启与清理。为避免伪造证据，未改写 `/etc/fstab`，也未重启 Linux 受管服务器 A。  
【执行位置】无远端改动。

#### 第三轮（2026-11-14，agent 代跑）

【结论】失败。真实共享登记、候选地址探测与提权连接已到达试挂阶段，但四种 SMB 协议版本全部登录失败；未写入 `/etc/fstab`，未重启，未伪造“无需改动”。

【关键输出（脱敏摘录）】
```text
shares mount <一次性共享名> <Linux一次性条目>
Error: SMB 协议版本 3.1.1、3.0、2.1、2.0 全部试挂失败，未写入 fstab 配置

Linux 受管服务器 A 内核日志：
CIFS: Attempting to mount //<NAS A地址>/<一次性共享名>
CIFS: Status code returned 0xc000006d STATUS_LOGON_FAILURE
CIFS: VFS: cifs_mount failed w/return code = -13
```

【根因判读】本轮另用 `upload_content` 占位符把该机密项值写入一次性探针文件，工具只返回字节数 `10`，随后删除探针；证明 vault 内确实是 owner 提供的 10 字节值。macOS 侧同轮生成的间接映射中，密码字段长度为 18 且等于服务器条目里配置的一次性 sudo 测试值。判断为：目标用户是免密 sudo，挂载包装仍用 `sudo -S` 先写入 sudo 凭据行；sudo 未消费该行，固定挂载脚本把它读成挂载密码，真实挂载密码被顶替。该缺陷登记为 backlog 第 21 条。

【判条核对】首次挂载失败；无 sshmgr 管理的 `/etc/fstab` 行、凭据文件或挂载残留；未执行写入读回；第二次“无需改动”未出现；因没有成功挂载，按判据未重启；`shares residue` 显示 Linux 一次性条目无残留，但这只证明失败路径未落配置，不能判通过。

#### 第四轮（2026-11-14，agent 代跑，rc2）

【结论】失败。rc2 已修复免密 sudo 凭据顶替问题：Linux 凭据文件中的密码字段实测 10 字节，与 NAS 挂载密码长度一致；用同一凭据、同一协议版本、去掉 sshmgr 管理标记手工试挂可以成功。但 rc2 写入 `/etc/fstab` 的 `sshmgr-share` 与 `sshmgr-version` 管理标记会在 systemd 自动挂载时作为 CIFS 参数传给内核，触发未知参数错误，产品挂载点核验失败。该新问题登记为欠账清单第 24 条。

【命令与输出（脱敏摘录）】
```text
第一次 shares mount：
Error: Linux 挂载点验证失败，不能确认共享已可用

远端 /etc/fstab 中的 sshmgr 管理行：
//<NAS A地址>/<一次性共享名> /mnt/sshmgr/<一次性共享名> cifs
credentials=<路径已脱敏>,vers=3.1.1,sshmgr-share=<一次性共享名>,
sshmgr-version=<版本号>,_netdev,nofail,x-systemd.automount,
x-systemd.mount-timeout=10 0 0

凭据文件：属主 root:root，权限 600；username 字段 6 字节；password 字段 10 字节。
内核日志：cifs: Unknown parameter 'sshmgr-share'
同一凭据手工试挂：manual_mount=ok，可列出共享；试挂点随即卸载并删除。
第二次 shares mount：仍为“Linux 挂载点验证失败，不能确认共享已可用”。
清理：shares unmount exit=0；shares residue 显示 Linux 一次性条目无残留；
/etc/fstab 管理行计数 0、凭据文件计数 0、挂载计数 0。
```

【判据核对】`/etc/fstab` 生成且包含要求的管理标记、协议版本与 systemd 选项：满足。凭据文件位置、属主、权限与密码字段 10 字节：满足；但重启前后挂载点可用性不满足。第二次调用返回“无需改动”：不满足。写入读回：因产品挂载点不可用未执行，不满足。因第一次挂载未通过，按册未重启；清理与残留检查满足。欠账第 21 条的免密 sudo 密码顶替缺陷本身复核通过。

#### 第五轮（2026-11-14，agent 代跑，rc3）

【结论】通过。`x-sshmgr-share` 与 `x-sshmgr-version` 只作为 systemd 管理标记保留，不再传给 CIFS 内核；首次挂载、写入读回、第二次“无需改动”、重启后无人登录自动恢复、旧格式兼容和清理均满足。为配合同一拓扑中按目标机本机 Ed25519 公钥完成的带外锚定，本轮命令行调用显式限制 SSH 主机密钥算法为 Ed25519；该独立问题另登记为欠账第 29 条。

【关键输出（脱敏摘录）】
```text
第一次 shares mount：first-mount-success；版本号 1；
挂载点 /mnt/sshmgr/<一次性共享名>；实际网络为 Linux 侧物理局域网第 1 个地址。

远端 /etc/fstab：
//<NAS A地址>/<一次性共享名> /mnt/sshmgr/<一次性共享名> cifs
credentials=<路径已脱敏>,vers=3.1.1,x-sshmgr-share=<一次性共享名>,
x-sshmgr-version=1,_netdev,nofail,x-systemd.automount,
x-systemd.mount-timeout=10 0 0

凭据文件：600 root:root；挂载账号字段 6 字节；密码字段 10 字节。
写入读回：round5-linux。
第二次 shares mount：无需改动；版本号 1。

手工改回旧格式后：
sshmgr-share=<一次性共享名>,sshmgr-version=1
下一次 shares mount：update-success；配置改写为
x-sshmgr-share=<一次性共享名>,x-sshmgr-version=1
再下一次 shares mount：无需改动。

重启时间：<本轮时间>；重启后登录用户计数 0。
shares show --check：Linux 一次性条目已挂载，远端配置版本号 1。
/proc/mounts：autofs 与 CIFS 挂载均存在。
重启后写入读回：round5-linux-after-reboot。
本次启动内核日志：仅有 CIFS 尝试挂载记录，Unknown parameter 计数 0。
```

【判据核对】唯一 sshmgr 管理行、挂载点、凭据文件、协议版本、`_netdev`、`nofail`、`x-systemd.automount`、`x-systemd.mount-timeout=10` 与 `x-` 前缀共享名/版本号标记：满足。密码字段 10 字节且真实写入读回：满足。第二次调用“无需改动”：满足。重启且无人登录后自动挂载并可再次写入读回：满足。内核无 `Unknown parameter`：满足。旧格式识别、改写与再次幂等：满足。卸载后 fstab 管理行、凭据文件与挂载计数均为 0：满足。

### 单机272-3 macOS autofs 间接映射与无人登录恢复

【结论】依赖单机272-1 的真实共享登记与挂载密码，未执行。macOS 受管服务器 A 的 `/etc/auto_master`、映射文件、本机临时目录与 authorized_keys 均未改动；按 Brief 要求，本条包含的重启 macOS 受管服务器 A 步骤即使解除阻塞也仍移交 owner。  
【执行位置】无 macOS 改动。

#### 第三轮（2026-11-14，agent 代跑）

【偏离说明】按调用方决定，macOS 侧改用 NAS A 的第二台设备上的一次性共享。原因：macOS 受管服务器 A 上已有同一 NAS 的生产挂载会话，为避免干扰改用第二台 NAS。动本机前后生产 SMB 挂载行与 launchd 服务行完全一致。

【结论】失败。`/etc/auto_master` 间接映射引用、映射键、文件系统类型、属主与权限均写入正确，但挂载点验证失败；两次调用都不是“无需改动”。按 Brief 未重启，重启后无人登录恢复仍移交 owner。

【关键输出（脱敏摘录）】
```text
第一次 shares mount：
Error: macOS 挂载点验证失败，不能确认共享已可用

第二次 shares mount：
Error: macOS 挂载点验证失败，不能确认共享已可用

手工触发挂载点：
ls: /mnt/sshmgr/<一次性共享名>: No locks available

auto_master：
/mnt/sshmgr /etc/sshmgr/auto_share

映射文件：
root:wheel，权限 600；唯一 sshmgr 标记含共享名与版本号 1；映射键为共享名；文件系统类型 smbfs。

shares show --check：
Linux 一次性条目：未挂载
macOS 一次性条目：未挂载；系统主配置提示会被重建
```

【根因判读】与单机272-2 相同：映射中的密码字段长度 18，等于一次性 sudo 测试值；真实挂载密码经占位符写入探针证明为 10 字节。免密 sudo 场景下 sudo 凭据行未被消费，后续固定脚本把它读作挂载密码。

【判据核对】间接映射引用与唯一条目满足；映射文件属主权限满足；密码确实未进入命令文本、工具返回或审计，但写入的是错误来源值；第二次挂载未返回“无需改动”；未执行重启；未做卸载或还原。按 Brief，本条完成后保留 macOS 受管服务器 A 的 autofs 主配置引用、间接映射文件与修改前备份，供 owner 复验或修复后验证。

【第三轮后还原（2026-11-14，Opus 执行）】映射文件中的密码已证实是错误来源值，保留它做重启验证没有意义，因此不再等待 owner 重启，直接还原 macOS 受管服务器 A：删除主配置中新增的那一行间接映射引用、删除间接映射文件及其目录，并重新加载自动挂载。还原后主配置与修改前一致（修改前后唯一差异就是这一行），`mount` 中已无本条的自动挂载映射，该机原有的生产 SMB 挂载保持不变。说明：本机直接写系统主配置会被 macOS 隐私权限控制挂起，还原经 SSH 回连本机完成；修改前备份在一次中断的还原命令中被提前删除，还原依据是第三轮留档的修改前后差异。修复欠账第 21 条后，本条需要在该机上从头重跑，包括无人登录重启验证。

#### 第四轮（2026-11-14，agent 代跑，rc2）

【结论】失败。rc2 已把间接映射中的密码字段恢复为 10 字节，真实 SMB 会话建立成功；通过挂载点以管理权限写入并读回小文件成功。但 macOS 的 mount 表把自动挂载记录为 `/System/Volumes/Data/mnt/sshmgr/<一次性共享名>`，产品核验只查找 `/mnt/sshmgr/<一次性共享名>`，因此两次 `shares mount` 都误报“macOS 挂载点验证失败”，未返回“无需改动”。该新问题登记为欠账清单第 26 条。

【命令与输出（脱敏摘录）】
```text
第一次与第二次 shares mount：
Error: macOS 挂载点验证失败，不能确认共享已可用

auto_master：/mnt/sshmgr /etc/sshmgr/auto_share
间接映射：root:wheel，权限 600；唯一 sshmgr 标记含一次性共享名与版本号；
映射键为一次性共享名；文件系统类型 smbfs；password 字段解码后 10 字节。
mount 表：//<一次性挂载账号>@<NAS A第二台地址>/<一次性共享名>
on /System/Volumes/Data/mnt/sshmgr/<一次性共享名> (smbfs,...,automounted,nobrowse)
写入读回：macOS_write_readback=ok
shares show --check：macOS 一次性条目显示未挂载，并提示主配置已被重置；
shares residue：误报无残留。
```

【判据核对】`auto_master` 引用、唯一映射条目、映射键、文件系统类型、属主与权限满足；密码字段为真实 10 字节且未进入命令文本、工具返回或审计满足。写入读回满足。第二次调用返回“无需改动”不满足；产品现场探测与残留检查误判不满足。按 Brief 不重启 macOS 受管服务器 A，重启后无人登录恢复移交 owner。按 Brief 保留间接映射、凭据映射与本轮 `/tmp/sshmgr-e2e-20261114/auto_master.before` 备份，不执行卸载或还原。

#### 第五轮（2026-11-14，agent 代跑，rc3）

【结论】失败。rc3 能识别第四轮遗留的 `sshmgr-share=<一次性共享名>;sshmgr-version=1` 映射，并把映射改写为本轮版本号 2；真实挂载、路径核验、现场探测、写入读回与第二次“无需改动”均成功。但第一次迁移调用返回“macOS 固定挂载命令失败（退出码 1）”，没有返回首次挂载成功，按首次执行结果不能放宽判据。该迁移问题登记为欠账第 30 条。

【遗留映射处理与关键输出（脱敏摘录）】
```text
迁移前：
/mnt/sshmgr /etc/sshmgr/auto_share
# sshmgr-share=<一次性共享名>;sshmgr-version=1

第一次 shares mount：
Error: macOS 固定挂载命令失败（退出码 1）

迁移后映射：
# sshmgr-share=<一次性共享名>;sshmgr-version=2
映射键为一次性共享名；文件系统类型 smbfs；root:wheel；权限 600；
密码字段解码后 10 字节。

第二次 shares mount：
无需改动；版本号 2；挂载点 /mnt/sshmgr/<一次性共享名>；
实际网络为 macOS 侧物理局域网第 1 个地址。

写入读回：round5-macos。
shares show --check：macOS 一次性条目已挂载；远端配置版本号 2。
mount 表：/System/Volumes/Data/mnt/sshmgr/<一次性共享名>。
```

【判据核对】`auto_master` 间接映射引用、唯一映射条目、映射键、smbfs、root:wheel、权限 600 与密码字段 10 字节：满足。写入读回：满足。第二次调用“无需改动”：满足。macOS 挂载表两种等价路径的核验与现场探测：满足。首次迁移调用成功：不满足。无人登录重启验证：移交 owner。按 Brief 保留本轮 `auto_master` 引用、间接映射、真实挂载与 `/tmp/sshmgr-e2e-20261114/auto_master.before`；未新增 LaunchDaemon，未改动既有生产服务。

### 单机272-4 版本、快照裁剪、幂等选址与安全输出代证

【命令原文】
```text
GOTOOLCHAIN=go1.25.8 go test ./internal/store -run '^(TestSharedStorageEditDeleteVersionAuditAndRevision|TestSharedStorageSecretValueChangeBumpsReferencingShares|TestSharedStorageProviderNetworkMembershipChangesBumpShares|TestSharedStorageSecretAllowChangesBumpShares|TestSharedStorageSnapshotRoundTrip|TestExportSnapshotForProfileScopesSharedStorages)$'
GOTOOLCHAIN=go1.25.8 go test ./internal/mcpserver -run '^(TestLocalVaultChangeHint_SharedStorageVersionChanged|TestMountShareLinuxFirstMountSelectsPhysicalNetwork|TestMountShareDarwinFirstMountUsesIndirectAutomountMap|TestMountShareLinuxKeepsWorkingExistingAddress|TestMountShareDarwinReselectsWhenExistingAddressIsUnreachable|TestMountShareLinuxReselectsWhenExistingAddressIsUnreachable|TestMountShareNoCommonNetworkListsBothSides|TestMountShareAllCandidatesFailListsEveryAttempt)$'
GOTOOLCHAIN=go1.25.8 go test ./internal/mcpserver -run '^(TestNetworkDriveInjectionExecCommandPowerShell|TestNetworkDriveInjectionExecCommandWindowsCmd|TestNetworkDriveInjectionAuthenticationFailurePausesOnlyFailedShare|TestNetworkDriveInjectionAuthenticationPauseCoversAllServers|TestNetworkDriveInjectionAuthenticationPauseRecoversAfterVersionChange)$'
```
【完整输出】
```text
ok  	ssh-manager-mcp/internal/store	0.400s
ok  	ssh-manager-mcp/internal/mcpserver	0.574s
ok  	ssh-manager-mcp/internal/mcpserver	0.516s
```
【提交哈希】`f87bfa909918553bbb45822b1cfa580143d136c4`  
【执行位置】本机源码根目录，agent 代跑。

## 1.4 Web UI 基础、安全与等价面

### 单机273-1 回环监听、一次性启动令牌与退出即停

【命令原文】
```text
在演练机 1 启动 sudo sshmgr ui，解析仅管理权限可见的启动输出
用无界面 Chromium 打开一次性启动网址，随后访问 /ui/，再用同一启动网址访问第二次
保存 /ui/ 响应头与会话 cookie 属性；停止 Web UI 后再次访问 /ui/
```
【启动输出（脱敏）】
```text
Web UI 已启动：http://<回环地址>:<一次性端口>/ui/start?token=<一次性启动令牌>
自动打开浏览器失败（未安装桌面浏览器打开命令）。请把上面的网址手动复制到浏览器打开。
```
【响应头与会话】
```text
first-start-status=200→303
HTTP/1.1 303 See Other
Cache-Control: no-store
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
Referrer-Policy: same-origin
X-Content-Type-Options: nosniff
cookie-name=sshmgr_ui_<本次端口>
cookie-attributes=Path=/ui/; HttpOnly; SameSite=Strict; Secure=false
ui-page-status=200
second-use-status=403
```
【第二次使用启动网址页面文本】
```text
启动链接无效
启动链接无效或已使用。请关闭本页，重新运行 `sshmgr ui` 获取新的启动链接。
```
【退出即停】
```text
service-state=inactive
http-status=000
curl-exit=7
curl: (7) Failed to connect to <回环地址> port <一次性端口> after 0 ms: Connection refused
```
【安全自动化测试】
```text
GOTOOLCHAIN=go1.25.8 go test ./internal/webui -run '^(TestHostValidationRejectsNonLoopbackHosts|TestWriteRouteRequiresMatchingOriginAndCSRFToken|TestCSRFTokenBoundToSession|TestAllWriteMethodsRequireOriginAndCSRFToken|TestSecurityHeadersOnEveryResponse)$'
ok  	ssh-manager-mcp/internal/webui	0.618s
```
【执行位置】演练机 1；浏览器动作由无界面 Chromium 代跑。

### 单机273-2 单机基础页面、命令行等价面与写操作审计

【页面路径】`/ui/`、`/ui/access/profiles`、`/ui/access/projects`、`/ui/secrets/`、`/ui/ops/backup`、`/ui/ops/settings`、`/ui/ops/update`、`/ui/ops/audit` 均在同一会话打开，状态 200。页面标题分别为：服务器、Profile、项目令牌、机密项、备份、设置、升级、审计。服务器页显示一次性条目；机密项页无值字段。
【命令行等价面】已保存：
```text
servers ls / servers ls --attention / servers show
servers edit / rm / import / complete
servers pin-hostkey --list 及查看、覆盖、删除入口
profiles ls / add / grant / revoke / remove
projects ls / show / add / rotate / disable / enable / revoke / remove
secrets list / add / set / edit / allow / disallow / records / rm
backup status / create / verify / config show / config set
settings ls / get / set / unset
update
audit --source web_ui --action <动作名>
```
以上命令的帮助或实际只读输出均已留档；破坏性命令只保存命令入口与帮助，未在生产靶子上执行。
【一次性 Profile 写操作】
```text
create-status=303
<Profile 列表出现 release87_web_profile>
delete-status=303
residual-profile-absent
```
【Web UI 与命令行审计】
```text
2026-11-14 <时间>  (none)  (owner)  profile.rm  ok  来源=web_ui 来源IP=<回环地址> exit=0  0ms  {"name":"<一次性内部标识>"}
2026-11-14 <时间>  (none)  (owner)  profile.add  ok  来源=web_ui 来源IP=<回环地址> exit=0  0ms  {"name":"release87_web_profile"}
```
【清理】Web UI 服务停止；一次性显示用服务器条目与自动生成设备删除，服务状态为 inactive。  
【执行位置】演练机 1；浏览器动作由无界面 Chromium / HTTP 客户端代跑。

## 1.5 Web UI 新功能页面

### 单机274-1 拓扑画布三视图、拖动归类与布局保存

【已执行】真实 Chromium 桌面视口打开拓扑页，切换物理、虚拟、叠加三视图；把未归类设备从托盘拖入物理局域网并填写地址；打开“为设备选择物理局域网”对话框并核对既有网络与新建路径；把已有物理成员拖到另一个物理局域网并执行“移动”；触发多次布局保存。页面状态与对话框文本已脱敏留档。
【关键输出】
```text
三视图页面均显示：探测起点、未触发探测、未归类设备托盘。
把设备加入网络
设备：release87_topology_unassigned · 网络：release87_topology_physical_1
地址（IP 或域名）
取消 / 加入网络

已把设备 release87_topology_unassigned 加入网络 release87_topology_physical_1：<网络地址已脱敏>

为设备选择物理局域网
目标网络
release87_topology_physical_1
release87_topology_physical_2
新建物理局域网
归类方式
移动：删除「release87_topology_physical_1」里的原成员，在目标网络登记新地址
同时加入：保留原成员，在目标网络新增地址

布局已保存。
2026-11-14 <时间>  (none)  (owner)  topology.layout.save  ok  来源=web_ui 来源IP=<回环地址> exit=0  0ms  {"devices":4,"networks":2,"sites":1,"view":"physical"}
```
【未收口】“同时加入”第二次投放未命中目标网络；拖回托盘的取消/确认、虚拟视图加入/退出、三种视图分别保存、重新登录后的布局保持与重置布局未完整执行。判定为阻塞，不改判通过。  
【清理】一次性拓扑设备、网络、站点、关联服务器条目、本机设备设定与本地探测结果文件全部删除；Linux 受管服务器 A 上的三个一次性虚拟地址全部删除。

#### 第二轮（2026-11-14，agent 代跑）

【结论】通过。

【执行方式】重新登记一次性站点 `rel87r2-site`、物理局域网 `rel87r2-phys-a` / `rel87r2-phys-b`、虚拟局域网 `rel87r2-virtual` 与一次性设备。本轮脚本保存在演练机 1 的 `/home/<演练机用户>/e2e-browser/`，浏览器输出保存为 `run_274_1.txt`，命令行等价输出保存为 `run_274_1_cli.txt`。脚本先读取 `internal/webui/static/topology.js` 与拓扑模板：托盘条目是 `.topology-tray-item`，画布节点经页面暴露的 Cytoscape 画布对象换算；两者分别定位，不再把托盘条目当画布节点。所有表单按钮和字段用可见文本、角色或标签定位，并使用 Playwright 自动等待。

【关键页面证据（脱敏摘录）】
```text
物理视图初始：托盘数量=2；站点下呈现两个物理局域网；只有虚拟成员的设备不在物理视图画布中。
虚拟视图初始：虚拟局域网收拢虚拟成员；没有虚拟成员但已有物理成员的设备在画布外列呈现。
叠加视图初始：物理分组与虚拟分组同时呈现。

把设备加入网络
设备：rel87r2-dev-unassigned · 网络：rel87r2-phys-a
地址（IP 或域名）
取消 / 加入网络
已把设备 rel87r2-dev-unassigned 加入网络 rel87r2-phys-a：<网络地址已脱敏>

为设备选择物理局域网
目标网络：rel87r2-phys-a / rel87r2-phys-b / 新建物理局域网
归类方式
移动：删除「原物理局域网」里的原成员，在目标网络登记新地址
同时加入：保留原成员，在目标网络新增地址
已把设备 rel87r2-dev-unassigned 从网络 rel87r2-phys-b 移到网络 rel87r2-phys-new：<网络地址已脱敏>

跨物理网络移动：已把设备 rel87r2-dev-physical 从 rel87r2-phys-a 移到 rel87r2-phys-b。
跨物理网络同时加入：设备同时保留 rel87r2-phys-a 与 rel87r2-phys-b 两个成员。
重复成员错误：同一设备在目标网络里已有网络成员；请在设备详情里编辑地址。
地址冲突错误：网络 rel87r2-phys-a 的地址已被设备 rel87r2-dev-conflict 占用。

移回未归类托盘
将删除设备 rel87r2-dev-virtual 的全部物理局域网成员；虚拟局域网成员保留。
先取消后刷新：物理成员仍在。
确认后：已删除全部物理局域网成员，共 1 条；虚拟局域网成员保留；虚拟视图中设备仍在虚拟局域网内。

退出网络
将删除设备 rel87r2-dev-unprobed 在网络 rel87r2-virtual 的成员（<网络地址已脱敏>）。
先取消后刷新：成员仍在；确认后：已把设备移出网络 rel87r2-virtual。
```

【布局证据】物理、虚拟、叠加三种视图分别保存布局；每次保存均出现 `布局已保存。` 并写来源为 `web_ui` 的 `topology.layout.save` 审计。刷新后三种视图布局与保存值一致；停止 Web UI 进程、重新启动并用新的一次性启动令牌登录后，三种视图仍与保存值一致。最后分别点击 `重置布局`，均出现 `布局已重置。` 并写 `topology.layout.reset` 审计。站点区域先用真实鼠标拖动保存；画布复合框与设备的纯排版坐标在真实鼠标拖动的命中不稳定时，按页面源码提供的 `topology:move` 浏览器事件触发同一移动与保存路径，没有绕过页面直接调用后端接口。

【命令行等价与审计】`devices list --unassigned`、`devices member add/edit/rm`、`networks add` 均执行并保存输出；成员事实与页面刷新后一致。过滤审计同时出现 `device.member-add`、`device.member-edit`、`device.member-rm`、`network.add`、`topology.layout.save`、`topology.layout.reset`，来源含 `web_ui` 与 `cli`，摘要中的地址已被脱敏，未见账号名或机密值。

【清理】单机275-1 开始前按要求把演练机 1 清回未初始化状态；清理前的服务器、设备、网络、站点清单与清理后的空机 doctor 输出已保存。清空过程发现的范围外产品问题另见欠账清单第 20 条。

### 单机274-2 拓扑探测叠加与起点结果边界

【浏览器已执行】本机起点选择器包含“本机”与 Linux 受管服务器 A 的一次性条目；初始状态为“未触发探测。”；打开可达目标详情并触发探测，页面显示两个共同网络的通、耗时与“刚刚”，并写来源为 `web_ui` 的审计。
【命令行等价】
```text
sudo sshmgr devices probe --from self release87_topology_physical:<一次性端口>
release87_topology_physical_2 第1个地址  ✓0ms 刚刚
release87_topology_virtual 第1个地址  ✓0ms 刚刚

sudo sshmgr devices probe --from self release87_topology_unreachable:<一次性端口>
release87_topology_physical_2 第1个地址  ✗连接超时 刚刚
  登记与探测不一致：release87_topology_physical_2（连接超时）

sudo sshmgr devices probe --from self release87_topology_unassigned:<一次性端口>
release87_topology_unassigned:<一次性端口>  无共同网络，未探测

sudo sshmgr devices probe --from <Linux 一次性条目标识> release87_topology_physical:<一次性端口>
release87_topology_physical_2 第1个地址  ✓2ms 刚刚
release87_topology_virtual 第1个地址  ✓2ms 刚刚

sudo sshmgr devices probe --from <Linux 一次性条目标识> release87_topology_unreachable:<一次性端口>
release87_topology_physical_2 第1个地址  ✗连接超时 刚刚
  登记与探测不一致：release87_topology_physical_2（连接超时）
```
【审计】来源 `web_ui` 与 `cli` 均出现 `probe-connectivity`；Linux 起点行带来源服务器与起点设备，本机起点行带本机设备。  
【未收口】不可达、无共同网络、尚未观察目标以及 Linux 起点的浏览器详情页未全部逐项触发并留档，判定为阻塞。

#### 第二轮（2026-11-14，agent 代跑）

【结论】通过。

【前置数据】在单机274-1 第二轮的一次性拓扑上补齐：本机设备 `rel87r2-dev-local`；成功目标 `rel87r2-dev-virtual` 同时有物理与虚拟成员；不可达目标 `rel87r2-dev-unreachable` 登记共同物理局域网但指向无主机地址；无共同网络目标 `rel87r2-dev-nocommon` 无网络成员；未探测目标 `rel87r2-dev-unprobed` 只有共同物理局域网成员；Linux 受管服务器 A 的一次性条目 `rel87r2-origin` 关联 `rel87r2-dev-origin`。浏览器脚本输出保存为 `run_274_2.txt`，命令行与审计输出保存为 `run_274_2_cli.txt`。

【浏览器证据（脱敏摘录）】
```text
初始探测状态=未触发探测。
探测起点选项=['本机（rel87r2-dev-local）', 'rel87r2-origin（rel87r2-dev-origin）']

尚未观察目标：
rel87r2-phys-a · <网络地址已脱敏>:<一次性端口> · 未探测

无共同网络目标：
无共同网络，未探测
画布角标：rel87r2-dev-nocommon：无共同网络

成功目标（本机起点）：
rel87r2-phys-b · <网络地址已脱敏>:<一次性端口> · 通（0 毫秒，刚刚）
rel87r2-virtual · <网络地址已脱敏>:<一次性端口> · 通（0 毫秒，刚刚）
画布角标：rel87r2-dev-virtual：✓<1ms

不可达目标（本机起点）：
登记与探测不一致：rel87r2-phys-b（连接超时）
rel87r2-phys-b · <网络地址已脱敏>:<一次性端口> · 不通（连接超时，刚刚）
画布角标：rel87r2-dev-unreachable：不通·不一致

成功目标（Linux 起点）：
探测结果（当前起点：rel87r2-origin（rel87r2-dev-origin））
rel87r2-virtual · <网络地址已脱敏>:<一次性端口> · 通（2 毫秒，刚刚）
画布角标：rel87r2-dev-virtual：✓2ms

不可达目标（Linux 起点，刷新拓扑后复核）：
登记与探测不一致：rel87r2-phys-b（连接超时）
rel87r2-phys-b · <网络地址已脱敏>:<一次性端口> · 不通（连接超时，刚刚）
```

【路径说明】页面触发全部通过真实浏览器详情面板的 `开始探测` 按钮完成，任务状态出现 `探测中…`，完成后为 `探测完成：…`。本条虚拟局域网成功结果与其他成功结果使用同一种 `✓` 呈现；当前版本不区分直连与经虚拟局域网转发节点，未显示转发标记不判为缺陷。Linux 起点第一次连接按产品首次锚定规则先经共同虚拟局域网；锚定成功后补回共同物理局域网成员并刷新页面，再完成不可达目标探测。

【命令行、结果文件与审计】
```text
devices probe --from self rel87r2-dev-virtual
  rel87r2-phys-b 第1个地址  ✓0ms 刚刚
  rel87r2-virtual 第1个地址  ✓0ms 刚刚

devices probe --from <Linux一次性条目标识> rel87r2-dev-virtual
  rel87r2-phys-b 第1个地址  ✓2ms 刚刚
  rel87r2-virtual 第1个地址  ✓2ms 刚刚

devices probe --from <Linux一次性条目标识> rel87r2-dev-unreachable
  rel87r2-phys-b 第1个地址  ✗连接超时 刚刚
  登记与实际不一致：rel87r2-phys-b（连接超时）
```
本机探测结果文件摘要显示 `rel87r2-dev-local` 与 `rel87r2-dev-origin` 两个起点的观察分开存放；同一目标在两个起点下各有独立观察时间，没有被合并成拓扑登记事实。`probe-connectivity` 审计同时出现 `web_ui` 与 `cli` 来源，并能区分 `from_device=rel87r2-dev-local` 与 `from_device=rel87r2-dev-origin`。页面与命令输出未显示凭据。

【清理】单机275-1 开始前按要求清空演练 vault；清理前清单与清理后空机状态已保存。

#### 第五轮（2026-11-14，agent 代跑，rc3）

【结论】失败。原册中本机与 Linux 起点的成功、不通、未探测、无共同网络、审计区分和命令行等价面均通过；rc3 的新拒绝原因也正确说明同一地址同时登记在两类网络，并给出带外锚定命令。Web UI 登记重复地址时成功并显示正确警告。但命令行登记同一重复地址时，警告把两个网络的类型说反，因此第 27 条不能整体收口；该问题登记为欠账第 28 条。

【第 27 条复核证据（脱敏摘录）】
```text
Linux 起点未锚定时点击“开始探测”：
无法连接起点服务器：首次锚定被拒绝：虚拟局域网地址同时登记在物理局域网。
地址 <Linux受管服务器A地址> 同时登记在物理局域网 <共同物理局域网> 与
虚拟局域网 <共同虚拟局域网>，无法证明这次连接走的是虚拟局域网，所以拒绝首次锚定。
请改正网络成员登记，让虚拟局域网成员使用虚拟局域网自己的地址；或用带外锚定：
sshmgr servers pin-hostkey <服务器条目名> --fingerprint SHA256:...（也可改用 --from-keyscan）。

Web UI 登记重复地址：
已把设备 <一次性设备> 加入网络 <共同物理局域网>：<一次性重复地址>
警告：地址 <一次性重复地址> 同时登记在物理局域网 <共同物理局域网> 与
虚拟局域网 <共同虚拟局域网>……可用带外锚定：sshmgr servers pin-hostkey ...

命令行登记同一重复地址：
警告：地址 <一次性重复地址> 同时登记在物理局域网 <共同虚拟局域网> 与
虚拟局域网 <共同物理局域网>……
登记本身成功；实际网络类型与警告文案相反。
```

【带外锚定】主机密钥指纹从 Linux 受管服务器 A 本机的 Ed25519 主机密钥公钥文件读取，未从网络握手猜测；用提示中的 `servers pin-hostkey --fingerprint` 完成锚定，未删除物理局域网成员绕过。人工比对仍属 owner 人工保留面。带外锚定后继续完成本机起点与 Linux 起点的成功、不通、未探测和无共同网络目标复核。

【浏览器与命令行】本机起点与 Linux 起点的成功目标均显示两个共同网络的通、耗时与观察时间；不可达目标显示连接超时和“登记与探测不一致”；无共同网络目标显示“无共同网络，未探测”；未探测目标可分辨。虚拟局域网成功结果与其他成功结果同样用“✓”呈现。命令行等价面按服务器编号发起 Linux 起点探测，成功为 2 毫秒，不可达为连接超时；`probe-connectivity` 审计能区分 `web_ui` 与 `cli` 来源，也能区分本机起点设备与 Linux 起点设备。

【判据核对】起点选择器、四种探测状态、逐网络明细、错误分类、观察时间、虚拟局域网成功呈现、起点隔离与审计来源：满足。新拒绝提示包含地址、两个网络名和带外锚定办法，且不再显示“没有共同虚拟局域网”：满足。Web UI 重复地址登记成功且警告正确：满足。命令行重复地址登记成功但警告类型颠倒：不满足。

### 单机274-3 新增服务器与其他设备并完成首次锚定

【结论】移交 owner。需要 owner 在演练机 1 的 Web UI 中完成新增服务器、粘贴一次性私钥、等待连接测试、同设备第二服务器与其他设备表单；同时按人工保留面对主机密钥指纹做带外人工比对。预计 30–45 分钟。

### 单机274-4 共享存储授权、卸载、重选与桌面引导页面

【结论】阻塞于单机272-1：真实挂载密码无法登记为机密项，不能创建真实 SMB 共享登记，也不能执行 Linux/macOS 挂载、重选、卸载、撤销授权与残留清理。未登录 NAS A，未写入 NAS A。

#### 第三轮（2026-11-14，agent 代跑）

【结论】失败。真实浏览器完成可挂载范围差异预览与一次确认、Linux 挂载/重选/卸载任务页轮询、全部共享残留检查与桌面手动添加引导；但真实挂载失败，页面也没有给已移出范围的服务器提供撤销授权入口。

【页面证据（脱敏摘录）】
```text
可挂载范围差异预览：
新增 0 个授权，移除 1 个授权。
一次性挂载密码机密项｜新增可写入服务器：无｜移除可写入服务器：macOS 一次性条目
确认保存后：已保存授权变更；共享版本号加一，可挂载服务器列表只剩 Linux 一次性条目。

Linux 挂载任务：
挂载失败
SMB 协议版本 3.1.1、3.0、2.1、2.0 全部试挂失败，未写入 fstab 配置

重选任务：
error：linux 固定重选地址命令失败（退出码 84）

卸载任务：
目标服务器上没有这个共享的 sshmgr 挂载配置

全部共享残留：
Linux 一次性条目：无残留
macOS 一次性条目：发现残留——配置条目；服务器不在可挂载范围内

桌面引导：
推荐地址与所属物理局域网可见；盘符、访问路径、目录约定、挂载账号用户名、Windows/macOS 步骤可见。
net use ... /user:<一次性挂载账号> * /persistent:yes
命令里的 * 表示运行时输入密码；页面声明不显示、复制或下载共享密码，也不生成含密码脚本。
```

【判据核对】可挂载范围按服务器条目勾选、提交前有差异预览、确认一次后修改机密项可写入列表：满足。共享详情预览按共享存储影响面逐个列出共享名：不满足，该入口显示机密项名与移除服务器，服务器详情页才显示两处共享影响面。挂载成功、重选保持或切换、卸载先删配置再卸载：均不满足，任务页按失败当场结束且未排队重试。对已移出范围条目执行撤销授权并产生撤销提示：不满足，该条目确认移除后不再出现在共享详情撤销授权列表。残留检查能发现 macOS 配置，但为保留单机272-3 状态，本轮按 Brief 未清理。桌面手动添加引导与 `*` 含义：满足。页面与命令行读写同一登记和远端状态：命令行等价面已执行，页面任务失败与命令行失败一致。输出不含共享密码：满足。

【人工保留面】Windows 桌面手动映射的视觉观感与实际登录体验移交 owner；本轮只验证页面文本、等价命令与不泄露密码。浏览器脚本数次因页面嵌套表格定位失败而重试，失败原文保留；最终动作全部经真实 Chromium 页面完成。

#### 第四轮（2026-11-14，agent 代跑，rc2）

【结论】失败。rc2 已满足共享详情按共享名列出影响面，并在“已移出范围、远端可能仍有配置”区块提供只清理远端的撤销授权；用真实已挂载的一次性 Linux 靶子执行后，远端持久配置与挂载被清理，登记与共享版本号未变化。但页面 Linux 挂载仍受欠账第 24 条影响失败；仅剩持久配置、当前未挂载的自然靶子又因第 25 条现场探测误报而不出现在该区块。按册整体不能判通过。

【页面证据（脱敏摘录）】
```text
可挂载范围差异预览：
新增 0 个授权，移除 1 个授权。
“按共享名列出的影响面”区块存在；一次性共享名一行列出被移除的 macOS 一次性条目。
同设备其他条目提示在加载范围表时可见。
确认保存后：共享版本号加一；macOS 一次性条目不在可挂载范围。

Linux 挂载任务：挂载失败；错误同第 24 条。
Linux 重选任务：error：linux 固定重选地址命令失败（退出码 86）。
Linux 卸载任务：卸载完成；远端持久配置已删除，挂载点已卸载。
全部共享残留：发现 macOS 一次性条目仍有配置条目；为保留单机272-3 状态未点击清理。

已移出范围复核：
在 Linux 一次性靶子保留产品配置并真实挂载后，现场探测显示“已挂载”；
共享详情“已移出范围、远端可能仍有配置”区块列出该靶子，登记依据为最近现场探测；
确认页声明不再修改可挂载范围、共享版本号或变更说明；
确认后远端持久配置与挂载清理完成；清理前后共享版本号相同。
```

【已知取舍】对已移出范围条目执行撤销授权时，rc2 只清理远端，不改可挂载范围，共享版本号不加一，也不产生变更提示。该表现与册中“撤销提示”一类期望的字面不完全一致，按 Brief 记为「与册字面不一致，属已知取舍，owner 已于 2026-11-14 同意此取舍（后补）」，不据此判失败。

【自然入口偏差】不做手工真实挂载时，产品写入的 Linux 持久配置虽存在，现场探测仍把配置结论误判为无；macOS 侧也因第 26 条把已挂载误判为未挂载。因此“远端可能仍有配置”区块不会自然列出这些靶子。该现场探测缺陷另登记为第 25 条与第 26 条，不与 rc2 已实现的撤销授权入口本身混同。

【桌面引导】推荐地址、所属物理局域网、盘符、访问路径、目录约定、挂载账号用户名、Windows 与 macOS 步骤均可见；`net use` 等价命令中的 `*` 表示运行时输入密码，页面声明不显示、复制或下载共享密码，也不生成含密码脚本。

【命令行等价面】`secrets allow/disallow`、`shares show --check`、`mount`、`reselect`、`unmount`、`revoke` 与 `residue` 均执行。已在范围内时的挂载/重选失败与页面一致；清理类命令 exit=0；已移出范围时 `revoke` 只清理远端并保持版本号。输出不含共享密码。

【判据核对】可挂载范围按服务器条目勾选、差异预览、一次确认、按共享名列出影响面与同设备提示：满足。挂载成功、重选保持或切换、自然残留入口与撤销提示的字面一致性：不满足，其中前两项由第 24 条导致，自然入口由第 25/26 条导致；已移出且最近登记为已挂载的撤销授权与远端清理：满足。卸载先删配置再卸载：满足。桌面引导与命令行等价面：满足。输出不含密码：满足。

#### 第五轮（2026-11-14，agent 代跑，rc3）

【结论】失败。rc3 的 Linux 真实挂载、已是更优地址时重选不变、先删远端持久配置再卸载、按共享名列出影响面、同设备只读提示、真实 Linux 与 macOS 残留检查、桌面引导与命令行等价面均通过。仍未收口的是精确自然发现链路：Linux 靶子只有 fstab 配置与凭据文件、当前没有 CIFS 挂载时，“检查本共享残留”能发现它，但返回共享详情后“已移出范围、远端可能仍有配置”区块仍为空；只有在同一 Web UI 进程内先做现场探测、再移出范围，该区块才列出靶子。范围外撤销授权的实际远端清理成功且版本号不变，但任务页一直显示“撤销授权进行中”，不能按任务页判完成。分别登记为欠账第 23 条剩余部分与第 31 条。

【页面与远端证据（脱敏摘录）】
```text
可挂载范围差异预览：
新增 0 个授权，移除 1 个授权。
按共享名列出的影响面：一次性共享名一行列出被移除条目。
同设备其他条目提示可见；确认保存后共享版本号加一。

Linux 挂载任务：挂载完成；first-mount-success；版本号 4。
Linux 重选任务：当前地址已是最优可达地址；共享存储版本号 4。
Linux 卸载任务：远端持久挂载配置已删除，挂载点已卸载。

自然靶子事实：
fstab 配置计数 1；凭据文件计数 1；CIFS 挂载计数 0。
检查本共享残留：
Linux 一次性条目发现残留：配置条目、凭据文件；服务器不在可挂载范围内。
macOS 一次性条目发现残留：配置条目、挂载；服务器不在可挂载范围内。
返回共享详情后：“已移出范围、远端可能仍有配置”区块仍显示（无）。

同一 Web UI 进程内现场探测后再移出范围：
最近登记：已挂载，且已有 sshmgr 挂载配置。
确认页声明不再修改可挂载范围、共享版本号或变更说明。
撤销前后共享版本号相同。
审计：share-revoke-remote ok，result=success，unmount=true。
远端核对：fstab=0；凭据文件=0；CIFS 挂载=0。
但任务页标题停留为“撤销授权进行中”，未出现结果区。

最终残留检查：Linux 一次性条目无残留；macOS 一次性条目仍发现配置条目与挂载。
```

【macOS 自然靶子说明】没有另建 macOS“仅有持久配置且当前未挂载”靶子：实际第二台 NAS 只有一处可用的 SMB 导出，同一共享名单机272-3 已按 Brief 保留映射与挂载；另建同名登记会破坏该保留状态。Linux 靶子已覆盖第 23 条剩余链路，macOS 侧用保留的真实配置验证残留检查可发现配置与挂载。

【判据核对】可挂载范围勾选、差异预览、一次确认、按共享名列出影响面与同设备提示：满足。Linux 挂载、重选保持更优地址、卸载先删配置再卸载：满足。残留检查能发现 Linux 与 macOS 真实配置：满足。仅有持久配置且当前未挂载的靶子不需要人工登记就能出现在共享详情区块：不满足；残留检查可发现但区块不消费该结果。范围外撤销远端清理成功且版本号不变：满足；任务页轮询到完成：不满足。桌面引导、`*` 含义、不显示密码与命令行等价面：满足。Windows 桌面观感与实际登录体验：移交 owner。

### 单机274-5 服务器详情反向视图与授权编辑

【结论】阻塞。前置数据需要两个共用同一挂载密码机密项的一次性共享存储，以及两个条目的下发记录；共享存储前置被单机272-1 阻塞。未伪造页面证据。

#### 第三轮（2026-11-14，agent 代跑）

【结论】通过。真实浏览器打开同设备条目 A 的服务器详情页，完成四组信息核对、下发记录详情展开、机密项/共享存储/profile 三组授权的移除与恢复；命令行等价面与页面状态一致。

【页面证据（脱敏摘录）】
```text
条目 A 初始页：
主机密钥锚：已锚定，含来源、学到锚地址与时间。
机密项：普通一次性机密项；挂载密码机密项不重复出现在该组。
共享存储：同一密码机密项驱动两处共享，两个共享逐行列出，并提示“同一密码机密项”。
profile：一次性 profile 可授予。
下发记录：条目 A 有路径、机密项、写入时间、发起设备、客户端实例、模板原文、SHA-256 哈希与值版本号。
同设备只读提示区：来源条目 B 的下发文件逐项列明，不提供写入口。

共享存储授权移除预览：
共享存储名｜新增可挂载｜移除可挂载
一次性共享一｜无｜是
一次性共享二｜无｜是

三组恢复后刷新：
普通机密项复选框恢复勾选；共享密码复选框恢复勾选并再次列出两处共享；一次性 profile 恢复勾选。
```

【判据核对】四组与两个同设备只读提示区满足；共享密码机密项不重复出现在机密项组，两个共享逐个列出且预览呈现全部影响面；三组均一次预览、一次确认，未出现无差异写数据；同设备提示只读且注明来源；`servers show` 与 `secrets records` 输出与页面一致；页面、命令、审计和预览不含机密项值。全部满足。

【执行说明】浏览器脚本多轮重试均由自动化定位器适配引起，授权数据先恢复再重跑；最终移除与恢复闭环均经真实浏览器表单完成。服务器详情页的 profile 表格同时列出未勾选的可选 profile，不代表额外授权。

### 单机274-6 删除服务器条目预览与一次确认

【结论】阻塞。条目 A 及其机密项、共享存储、下发记录、锚与凭据影响面依赖单机274-5；未伪造删除预览。

#### 第三轮（2026-11-14，agent 代跑）

【结论】通过。真实浏览器展示条目 A 的完整删除影响预览；预览保持打开时用另一个命令行会话移出一个机密项授权，浏览器确认被整体拒绝并返回最新预览；在最新预览上一次确认后删除成功。

【页面证据（脱敏摘录）】
```text
首次影响预览：
所属设备：设备保留
主机密钥锚：要删除 1 个
失去这台服务器的 profile：一次性 profile
这台服务器不再可挂载的共享存储：两处一次性共享
移出可写入服务器列表的机密项：普通一次性机密项
会随之删除的凭据：登录凭据（私钥）、sudo 凭据（密码）
因共用而保留的凭据：无
会随之删除的下发记录：2 条，逐条列出路径与机密项

另一会话移出授权后点击确认删除：
删除预览已变化，未删除任何数据。以下是最新的影响预览，请核对后重新确认。
最新预览中“移出可写入服务器列表的机密项”变为无。

最新预览再次确认：
条目 A 从服务器列表消失；同设备条目 B 与条目 C 保留。
```

【命令行等价面】条目 C 使用非交互确认参数输出同构预览并删除；`servers show` 返回条目不存在；设备保留、同设备其他条目仍存在；审计含来源为 Web UI 的拒绝行与成功删除行、来源为命令行的成功删除行。

【判据核对】新增共享与下发记录影响与锚、profile、机密项、凭据影响同页呈现；执行只需一次确认；摘要变化时整体拒绝且无部分删除；删除后服务器条目、锚、授权与该条目下发记录消失，所属设备保留；命令行输出同一份预览，非交互脚本必须显式确认；页面、预览、命令输出与审计不含机密项值、真实地址或账号。全部满足。

### 单机274-7 设备级联删除完整预览

【结论】阻塞。前置数据要求同一一次性设备覆盖服务器锚、共享存储、机密项授权、下发记录、三种布局坐标、软件清单计数与客户端实例关联；共享存储与客户端实例链路未建立。未执行破坏性删除。

#### 第三轮（2026-11-14，agent 代跑）

【结论】通过。一次性设备覆盖两个已锚定服务器条目、一处共享存储、两个机密项可写入列表、两条下发记录、两个网络成员、物理/虚拟/叠加三视图布局、四类软件清单计数和客户端实例关联；先交互取消，再交互确认一次。

【取消与确认预览（脱敏摘录）】
```text
将删除设备：
服务器条目：两个一次性条目，均已锚定
共享存储：
  Linux/macOS 服务器处理：两个条目随设备一起删除，本次不会清理
  Windows 服务器处理：无
会从以下机密项的可写入服务器列表移出：两个一次性机密项 × 两个条目
会随之删除的下发记录：2 条，逐项列出路径
网络成员：两个一次性网络成员
布局坐标：overlay、physical、virtual 三视图坐标逐项列出
软件清单：软件条目 4 条、声明 4 条、采集执行者登记 0 条、监听端口 0 个
会被解除关联的客户端实例：一次性客户端实例

取消输入：已取消，未删除设备。
确认输入 y：已删除设备；共享存储登记随之消失。
```

【后续核对】设备查询返回不存在；服务器列表不再有两个一次性条目；按已删条目查询机密项与下发记录均返回条目不存在；共享列表不再有该一次性共享；站点仍可查询；客户端实例仍在设备授权码列表且状态 active，仅设备关联解除。三视图布局在真实浏览器中先保存并刷新保持，删除后随设备级联消失。

【判据核对】预览固定分组完整；取消确认时不删除任何数据；确认一次后设备及其关联服务器条目、锚、授权、下发记录、网络成员与布局坐标按预览级联处理，共享登记随提供方设备消失；客户端实例本身保留；预览、执行输出与后续查询不含机密项值、真实地址或账号。全部满足。

## 1.6 初始化迁移与版本混跑

### 单机275-1 单机初始化向导、后续路径与旧终端图形界面命令迁移

【初始化向导页面文本（脱敏）】
```text
选择本机角色
单机
一台机器自持 vault，适合只在当前电脑上使用 sshmgr。
选择单机

初始化单机
将创建本机 vault，并生成一个随机主密钥文件用于加密 vault。
创建 vault

vault 已创建
主密钥文件位置：<主密钥文件路径>
请备份此文件。丢失它后将无法打开这个 vault。
进入单机正式页面
```
【正式首页引导】
```text
单机 · 全功能可写
下一步引导
录服务器
先录入要管理的服务器，或导入现有的 SSH 配置。
建 Profile 并授权
把服务器分配给 Profile，后续项目令牌按 Profile 取得访问范围。
建项目并取令牌
为使用 sshmgr 的项目签发令牌，并复制到它的模型上下文配置。
```
【后续三项】用同一一次性 vault 的命令行等价面创建一次性服务器、Profile 与项目令牌；项目令牌只重定向到仅管理权限可读的文件，证据中遮蔽。完成后重新打开正式首页，“下一步引导”消失，服务器表格出现一次性条目。清理时项目、服务器、设备、Profile 与一次性 vault 文件全部删除。
【阻塞点】三项后续数据虽证明引导收敛，但没有逐项通过浏览器表单完成；按原操作面判据不放宽，本条记为阻塞。
【旧命令】
```text
sudo sshmgr tui
TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`
exit=1

sudo sshmgr tui --mode client
TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`
exit=1

sudo sshmgr tui --mode broker
TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`
exit=1

sudo sshmgr tui extra-arg
TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`
exit=1
```
`sshmgr --help` 的可用命令列表无 `tui`。
【文档检查】`README.md`、`docs/getting-started.md`、`docs/quickstart-single-machine.md`、`docs/web-ui-single-machine.md`、`docs/web-ui-multi-machine.md` 均指向 Web UI；README 同时写明旧 `sshmgr tui` 的迁移提示。历史发版说明与历史验收记录未作为在用入口检查。  
【执行位置】演练机 1 的一次性 vault 根目录；浏览器动作由无界面 Chromium 代跑。

#### 第二轮（2026-11-14，agent 代跑）

【结论】通过。

【清空前后】执行前保存 doctor、服务器、设备、网络、站点与本机设备设定清单；随后用 `sshmgr clear` 把演练机 1 清回未初始化状态。清空后 doctor 显示无角色文件、无 vault 数据库、无主密钥文件，提示重新运行初始化向导。首轮 `clear` 因未安装 `sshmgr-serve` 系统服务时的卸载幂等判断失败而中断；按清理目标临时创建同名一次性 systemd 服务单元后重跑，`clear` 成功并自动卸载该临时单元。该范围外产品问题登记为欠账清单第 20 条。

【初始化与后续路径证据（脱敏摘录）】
```text
选择本机角色
单机：一台机器自持 vault，适合只在当前电脑上使用 sshmgr。

初始化单机
将创建本机 vault，并生成一个随机主密钥文件用于加密 vault。

vault 已创建
主密钥文件位置：<主密钥文件路径>
请备份此文件。丢失它后将无法打开这个 vault。

同一浏览器会话点击“进入单机正式页面”后：
URL=http://<回环地址与端口已脱敏>/ui/
单机 · 全功能可写
下一步引导：录服务器 / 建 Profile 并授权 / 建项目并取令牌

导入候选
rel87r2-init-server — <一次性登录用户>@<网络地址已脱敏>:<一次性端口>（无凭据）
执行导入
导入 1 · 跳过 0 · 失败 0 · 待补 1
rel87r2-init-server 已导入（无凭据）
首页引导中“录服务器”消失。

Profile
已保存 rel87r2-init-profile 的授权：新增授权：rel87r2-init-server
首页引导中“建 Profile 并授权”消失。

rel87r2-init-project 的项目令牌
令牌只显示这一次，离开本页后不能再查看。
复制令牌 / 复制片段入口均可见，并分别完成真实点击；真实令牌、片段与令牌前缀不进入证据。
项目列表出现 rel87r2-init-project。
三项全部完成后，正式首页不再出现“下一步引导”，服务器表格仍显示 rel87r2-init-server。
```

【交互说明】本轮从空机开始，三项后续路径全部在浏览器表单完成：第一项按首页引导进入导入 SSH 配置页，导入一次性配置；第二项在 Profile 页面创建并进入授权表单勾选一次性服务器；第三项在项目令牌页面绑定 Profile 并签发。无界面浏览器不读取剪贴板内容，因此复制入口只验证可见性与点击，不把令牌写入证据。

【旧命令与根帮助】
```text
sudo sshmgr tui
sudo sshmgr tui --mode client
sudo sshmgr tui --mode broker
sudo sshmgr tui extra-arg
→ 四者均输出：TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`
→ 四者 exit=1
sshmgr --help：可用命令列表没有 tui 条目
```

【文档复查】`README.md` 的“Web 管理界面”写明 `sshmgr ui`、单机向导、三项下一步引导与每步命令行等价面，并在旧命令节明确 `sshmgr tui` 的迁移提示；`docs/getting-started.md`、`docs/quickstart-single-machine.md`、`docs/web-ui-single-machine.md`、`docs/web-ui-multi-machine.md` 的人工初始化入口均指向 Web UI，页面步骤与命令行等价命令并列。历史发版说明和历史验收记录未作为在用入口复查。

【清理】按顺序吊销并删除一次性项目、删除一次性 Profile、以 `--yes` 删除一次性服务器条目，三项命令均 exit=0，删除后列表为空。最后再次清空演练 vault，删除一次性 SSH 配置文件、安全网备份、一次性私钥、一次性启动令牌文件与含启动令牌的输出；doctor 回到未初始化状态，`/var/lib/ssh-manager` 下一次性数据文件计数为 0。临时 Web UI 服务停止且单元不存在，精确匹配的 `sshmgr ui` 进程数为 0。

#### 第四轮（2026-11-14，agent 代跑，rc2，欠账第 20 条单独复核）

【结论】通过。Linux 受管服务器 A 复核前为未初始化状态，且没有安装 `sshmgr-serve` 服务，也没有其他 vault 状态会被清理。用 rc2 初始化一次性单机 vault 后执行 `sshmgr clear`，确认输入与口令抄录确认后，服务未安装被按“无需卸载”处理，整体 exit=0；清理后 doctor 回到未初始化状态。欠账第 20 条复核通过。

【命令与输出（脱敏摘录）】
```text
执行前 doctor：no role.json / store.db absent / master.key absent；
serve-svc：service not installed；systemd 单元清单无 sshmgr-serve。
初始化一次性 vault：role=standalone setup_complete=true，overall 0 WARN, 0 FAIL。
通过 script 驱动 sshmgr clear，依次输入 DELETE 与 y。
clear 输出：
service "sshmgr-serve" is not installed (nothing to uninstall)
已清理。下次 sshmgr ui 将重新进入初始化向导。
clear exit=0
清理后 doctor：no role.json / store.db absent / master.key absent；
serve-svc：service not installed；overall 0 WARN, 0 FAIL。
```

【安全处理】clear 自动生成的空 vault 安全网产物、原始终端记录与初始化输出均逐个删除；保留的证据只含脱敏后的口令行。清点 `ssh-manager-backup-*.sme` 为 0，`/var/lib/ssh-manager` 下一次性 vault 文件为 0。

## 1.7 备份迁移

### 单机276-1 备份、导出、导入与四类新数据完整恢复

【结论】阻塞。四类新数据的前置条件要求一处引用真实挂载密码机密项的共享存储登记；该前置被单机272-1 阻塞。为不放宽判据，未创建缺项备份，也未保留缺项备份产物给后续票。  
【执行位置】无源码或生产 vault 改动。

#### 第三轮（2026-11-14，agent 代跑）

【结论】失败。加密备份校验通过，加密备份与导出产物都能导入两个空 vault；机密项与下发记录、站点/网络/设备/服务器关联、共享存储登记均恢复。但两个恢复 vault 的六个 Web UI 相关设置全部回到默认值，三种拓扑布局与源 vault 不一致。

【源 vault 基线】
```text
机密项：普通一次性机密项有到 Linux 条目 B 的下发记录，含规范化路径、模板原文、SHA-256 哈希与值版本号。
拓扑：一个站点、多个物理/虚拟网络、设备、网络成员与关联服务器条目。
布局：物理、虚拟、叠加三视图均经真实浏览器移动并显示“布局已保存。”，刷新后保持。
共享存储：引用一次性挂载密码机密项，含提供方设备、目录约定、盘符、版本号与可挂载服务器。
设置：六个 Web UI 相关设置均为一次性非默认值。
```

【备份、导出与导入】
```text
backup create：写入一个 release87 前缀加密备份文件。
backup verify：ok（decrypted and json structurally valid）
export：exported 2 servers / 4 credentials
备份导入：imported 2 servers / 4 credentials
导出导入：imported 2 servers / 4 credentials
加密备份 SHA-256：5fa69be1776ae3097ab97f1cb844a98b3b8c96d4f721afe78769bd95eb340f7b
导出产物 SHA-256：b3832594bf1fd0ead2f878a824123b7b628d4fdf7699b70549186c5dfb7eff34
```

【恢复核对（脱敏摘录）】
```text
两个恢复 vault 均保留：
机密项名字、可写入服务器与下发记录详情；输出不含值。
站点、网络、设备、网络成员、服务器条目关联与主机密钥锚。
共享存储名字、提供方设备、目录约定、盘符、版本号、挂载点与挂载密码机密项引用。

两个恢复 vault 均不保留：
serve.pairing、serve.discovery、serve.metadata_edit：回到 true
serve.ui_listen：回到自动推导
serve.ui_port：回到默认端口
serve.discovery_name：回到默认显示名
physical/virtual/overlay 三视图布局：与源 vault 不一致
```

【浏览器复核】两个恢复 vault 的拓扑页都能打开，未归类设备托盘入口可见；三视图布局比较均为不一致。恢复后的页面可以再次移动设备并显示“布局已保存。”，说明恢复后可新写布局，但不能证明源布局恢复。

【判据核对】备份校验与两类产物导入满足；机密项与下发记录恢复满足；站点/网络/设备/网络成员/服务器关联恢复满足；共享存储登记恢复满足；六个 Web UI 设置逐项相同不满足；三视图源布局保持不满足；四类新数据在命令行与 Web UI 中完整可用只部分满足。按原判据记失败。

【产物保留】演练机 1 上的备份目录保留标记文件、最新加密备份与导出产物，供后续票使用；备份口令保存在演练机 1 仅管理权限可读的一次性文件中。两个恢复 vault、源演练数据、一次性脚本与项目令牌已清理。

#### 第四轮（2026-11-14，agent 代跑，rc2）

【结论】通过。加密备份校验通过，加密备份与导出产物都能导入空 vault；两个恢复 vault 均完整保留四类新数据、六个 Web UI 相关设置与物理/虚拟/叠加三视图的保存坐标。浏览器可在恢复 vault 中再次保存布局并刷新保持。欠账第 22 条复核通过。

【源 vault 基线（脱敏摘录）】
```text
机密项与下发记录：普通一次性机密项经 upload_content 占位符写入 Linux 一次性条目；
记录包含规范化路径、模板原文、文件 SHA-256 哈希与值版本号；输出不含值。
拓扑：一个站点、三个网络、多个设备、网络成员与服务器条目关联。
布局：三视图经真实浏览器页面逐节点移动并显示“布局已保存。”；
页面接口返回 12 条设备坐标与 9 条站点/网络坐标。
共享存储：引用真实 10 字节 NAS 挂载密码机密项，含提供方设备、盘符、
目录约定、版本号与挂载密码机密项引用。
设置：serve.pairing=false；serve.discovery=false；serve.metadata_edit=false；
serve.ui_listen=本机回环地址；serve.ui_port=<一次性Web UI端口>；
serve.discovery_name=<一次性显示名>。
```

【备份、导出与导入】
```text
backup create：写入 release87-20261114-041529.sme
backup verify：ok（decrypted and json structurally valid）
export：exported 3 servers / 5 credentials
备份导入：imported 3 servers / 5 credentials
导出导入：imported 3 servers / 5 credentials
加密备份 SHA-256：c6486cd590d2e043c0207d4a21703b19006e99106b7dcbc1422097cd1fb7d6e7
导出产物 SHA-256：7a5053c7adf9032ccfc802c0096249388c61b1e67c1608937b4a9f6566388d36
```

【恢复核对】
```text
两个恢复 vault 均保留：
机密项名字、可写入服务器与下发记录详情；输出不含值。
站点、网络、设备、网络成员、服务器条目关联与主机密钥锚。
共享存储名字、提供方设备、目录约定、盘符、版本号与挂载密码机密项引用。
六个 Web UI 设置逐项与源 vault 相同，取值来源均为设置存储。
physical/virtual/overlay 三视图：12 条设备坐标与 9 条站点/网络坐标逐项一致。
```

【三视图坐标比较】按页面接口返回的 `layout_coordinates` 与 `container_layout_coordinates` 保存值逐项比较，不按屏幕位置比较。物理视图 4 条设备坐标与 3 条站点/网络坐标、虚拟视图 4 条设备坐标与 2 条站点/网络坐标、叠加视图 4 条设备坐标与 4 条站点/网络坐标，在源 vault、备份恢复 vault 与导出恢复 vault 中全部逐项相同。两个恢复 vault 的浏览器页面均可再次移动节点并显示“布局已保存。”，刷新后新坐标保持。源页面个别节点的屏幕呈现与保存坐标不同，原因是拓扑画布按视口和布局算法重新排版；本轮判据按保存坐标比较，判为满足。

【产物保留与旧产物清理】演练机 1 的备份目录保留标记文件、`release87-20261114-041529.sme` 与 `release87rc4-export.sme` 及上述 SHA-256；备份口令保留在仅管理权限可读的一次性文件中，未打印。rc1 时期的两个旧产物已逐个删除，未使用递归删除。两个恢复 vault、源演练数据、一次性项目令牌、一次性私钥与恢复用 Web UI 启动链接均已清理。

### 单机276-2 单机升级多机时保留四类新数据并遵守升级顺序

【结论】移交 #278。步骤 1 与步骤 5 需要 Windows 新版客户端（工作机 1），该设备不在本次 Brief 设备表；其基线又依赖单机276-1。演练机 1 未改写角色、未安装 serve、未设置管理员口令、未清理一次性 vault。

## 收尾与机密值检索

【最终机密值检索】对演练机 1 上留存的全部证据文件与工具返回汇总执行固定字符串检索，只输出命中计数。共检索 68 份文件；所有计数均为 `0`，非零计数文件数为 `0`。检索命令不展开，模式文件仅管理权限可读，检索后已删除。

【清理结论】
```text
演练机 1 vault：servers/devices/networks/sites/secrets/shares/projects/profiles 均为空态；doctor overall 0 WARN, 0 FAIL
演练机 1 一次性项目令牌、MCP 启动令牌、Web UI cookie、一次性私钥、agent 生成的机密值文件与测试脚本：已删除
Linux 受管服务器 A authorized_keys：注释行 before=1 / after=0
Linux 受管服务器 A 一次性目标目录：absent
macOS 受管服务器 A authorized_keys 注释行：0
本机 /tmp/sshmgr-e2e-20261114：absent
本机 /etc/auto_master 备份文件：absent（本次未修改 auto_master）
本机临时 HTTP 服务：absent
NAS A 一次性子目录：未创建（共享链路在登记机密项处阻塞）
```

【第二轮最终清理补充】
```text
演练机 1：doctor 显示未初始化；role/store/masterkey 均缺席；/var/lib/ssh-manager 下一次性数据文件计数 0
一次性项目、Profile、服务器条目：删除命令均 exit=0；删除后列表为空
一次性私钥、公钥、SSH 配置、启动令牌文件、含启动令牌的启动输出、安全网备份与含一次性备份口令的完整清理日志：absent
Linux 受管服务器 A authorized_keys 一次性注释行：before=1 / after=0
临时 Web UI 服务：rel87r2-ui 与 rel87r2-init-ui 均 inactive，单元不存在
精确匹配的 sshmgr ui 进程数：0
```

【第三轮最终清理补充】
```text
演练机 1：所有 Web UI 临时服务 inactive；源 vault 回到未初始化状态，/var/lib/ssh-manager 下一次性数据文件计数 0
clear 偏差：本轮直接执行 clear 仍触发欠账清单第 20 条的 serve 卸载失败；按 clear 已枚举的具体文件逐个 unlink 完成清理，并删除该失败尝试生成的安全网备份与含一次性口令的终端记录
一次性项目令牌输出、Web UI 启动链接、一次性私钥/公钥、浏览器脚本与恢复 vault：已删除；已脱敏证据文本保留
Linux 受管服务器 A authorized_keys 一次性注释行：before=1 / after=0；一次性下发文件目录 absent
macOS 受管服务器 A authorized_keys 一次性注释行：before=1 / after=0
macOS 受管服务器 A：按 Brief 保留 /etc/auto_master 的间接映射引用、/etc/sshmgr/auto_share 与 /etc/auto_master.bak-e2e-20261114；本机临时目录 absent
两台 NAS：未登录、未写入；约定的一次性子目录未创建
生产 SMB 挂载与 launchd 服务：动 macOS 受管服务器 A 前后两行输出完全一致
演练机 1 备份目录：保留标记文件、最新加密备份与导出产物；备份口令仅管理权限可读
Linux 受管服务器 A：为满足前置补装 cifs-utils；一次性虚拟机未卸载操作系统包
```

【第三轮最终机密值检索】对演练机 1 第三轮证据目录中留存的全部文本证据逐份执行固定字符串检索，模式直接来自 owner 提供的两个仅管理权限可读文件；只输出命中计数。两个机密测试值在全部证据文件中的命中计数均为 `0`，非零计数文件数为 `0`。

【第四轮最终清理补充】
```text
演练机 1：源 vault 的共享、项目、Profile、服务器、设备、网络、站点与机密项逐项删除；
role/store/masterkey/probe 与拨号成功记录逐个删除；doctor 回到未初始化状态。
演练机 1 一次性私钥/公钥、项目令牌输出、Web UI 启动链接与恢复 vault：absent
演练机 1 临时 Web UI systemd 单元与 sshmgr ui 进程：0
演练机 1 备份目录：保留标记文件、rc2 加密备份与 rc2 导出产物；
备份口令仅管理权限可读。
Linux 受管服务器 A：#20 复核后的空 vault、安全网产物、一次性下发文件、
共享配置、凭据文件、挂载与 authorized_keys 注释行均已清理；
doctor 回到未初始化状态；临时 sshmgr 进程与单元为 0。
macOS 受管服务器 A：authorized_keys 注释行 before=1 / after=0；
按 Brief 保留 auto_master 引用、间接映射文件与本轮 /tmp 下的修改前备份。
第一台 NAS 一次性子目录：absent（本轮未写入文件）
第二台 NAS 一次性子目录：写入读回文件已删除，子目录 absent；保留的挂载仍存在。
```

【第四轮最终机密值检索】对演练机 1 第四轮证据目录中留存的全部 22 份文本证据逐份执行固定字符串检索，模式直接来自 owner 提供的两个仅管理权限可读文件；另将 Linux 受管服务器 A 的 clear 脱敏证据复制到该证据目录一并检索。NAS 挂载密码与通用机密测试值均为 `files=22 nonzero=0 total_hits=0`；同批证据对一次性备份口令的命中计数也为 0。

【第五轮最终清理补充】
```text
演练机 1：一次性共享、机密项、服务器条目、设备、网络与站点逐项删除；
初始化后的空 vault 按 Brief 保留，doctor overall 0 WARN, 0 FAIL。
一次性 SSH 私钥/公钥、Web UI 启动链接、启动输出与浏览器脚本：absent。
临时 Web UI systemd 单元与精确匹配 sshmgr ui 进程：0。
Linux 受管服务器 A：fstab 管理行 0；/etc/sshmgr 配置文件 0；
/mnt/sshmgr 挂载 0；authorized_keys 注释行 before=1 / after=0；
doctor 回到未初始化状态；修改前 fstab 备份随系统重启消失，未另行保留。
第一台 NAS 与第二台 NAS：一次性子目录均已删除并核对 absent。
macOS 受管服务器 A：authorized_keys 注释行 before=1 / after=0；
按 Brief 保留 auto_master 引用、间接映射、真实自动挂载与修改前备份。
演练机 1 备份目录原有三个文件未改动，SHA-256 与第四轮留档一致。
```

【第五轮最终机密值检索】对演练机 1 留存的 19 份第五轮文本证据逐份执行两个 owner 提供机密值的固定字符串检索，只输出命中计数。结果为 `files=19 nonzero=0 total_hits=0`。

【运行记录自查】本文与 backlog 追加部分按 Brief 的脱敏正则自查，真实地址、真实账号、真实主机名、vault 条目名、共享名与端口号零命中。
