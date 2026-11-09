# 整体发版真机验收册（#87）

本册按票 #87 逐步汇总发版前的真机验收项。当前先落机密项环境变量注入的
Windows 条目；其余功能条目由对应实现票补齐后再执行。

## 前置条件

- sshmgr 已构建为包含机密项环境变量注入的版本。
- 准备一台临时 Windows 服务器，owner 分别用 PowerShell 与 cmd 两个服务器条目
  指向它；两个条目都使用一次性测试机密项，跑完删除测试数据。
- 测试机密项值至少包含中文、空格、引号、百分号、反斜杠与连字符，长度不小于
  32 个字节，且不含换行；owner 在验收记录中保留该测试值的事实来源，不把值
  写入本册或运行记录。

## W1 PowerShell 读帧【agent 可跑】

1. owner 创建机密项并设置环境变量名 `WIN_PS_TOKEN`，授权给 PowerShell 服务器
   条目。
2. agent 调用 `exec_command` 执行：
   `Write-Output $env:WIN_PS_TOKEN`
3. agent 再调用 `exec_command` 执行：
   `$process = Get-CimInstance Win32_Process -Filter "ProcessId=$PID"; $process.CommandLine`

**判据**：第 2 步输出只含 `{{sshmgr-secret:机密项名}}`，没有未注入警告；
第 3 步输出包含 PowerShell 进程命令行，但 owner 对照测试值确认命令行参数里
没有值，输出遮蔽名单也没有因命令行泄漏而出现机密项名。

## W2 cmd 读帧【agent 可跑】

1. owner 创建机密项并设置环境变量名 `WIN_CMD_TOKEN`，授权给 cmd 服务器条目。
2. agent 调用 `exec_command` 执行：
   `echo %WIN_CMD_TOKEN%`
3. agent 再调用 `exec_command` 执行：
   `powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter 'name=''cmd.exe''' | Select-Object ProcessId,CommandLine"`

**判据**：第 2 步输出只含 `{{sshmgr-secret:机密项名}}`，没有未注入警告；
第 3 步输出包含 cmd 进程命令行，但 owner 对照测试值确认命令行参数里没有值，
输出遮蔽名单也没有因命令行泄漏而出现机密项名。

## 取证与清理

- 每步保存工具调用的原始命令、返回结构化结果、`masked` 名单与审计查询输出；
  禁止把测试值写进验收证据。
- 验收结束后删除两个临时服务器条目、测试机密项与其授权；清理失败要原样
  记录，不得把失败留成无证据的完成项。
