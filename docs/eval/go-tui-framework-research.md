# Go TUI 框架选型调研报告

> 调研日期：2026-08-14。目的：为 owner 端图形化/向导式管理界面（新增服务器、授权 agent 等跨 servers/profiles/projects 的操作）选择 Go TUI 框架。
>
> 放置位置说明：本仓库研究/评估类文档现有约定目录为 `docs/eval/`（参见 `docs/eval/phase3.md`），`.omc/research/` 不存在，故本报告落在 `docs/eval/go-tui-framework-research.md`。
>
> 活跃度数据全部来自 GitHub REST API（api.github.com）当日抓取；特性描述来自各仓库 README / pkg.go.dev 官方文档。每条关键 claim 附来源链接。

---

## TL;DR 推荐结论

**主选：Bubble Tea v2 + huh v2（表单/向导）+ bubbles v2（组件）+ Lipgloss（样式）的 Charm 全家桶**，以 `ssh-manager tui` 子命令挂进现有 Cobra 二进制。

核心理由：

1. **huh v2 直接解决本项目的核心痛点**——`servers add` 十几个 flag 的多字段表单、密码输入掩码、单选/多选、跨 servers/profiles/projects 的多步向导（huh 的 Group 机制 = 分页表单），全部开箱即有，且可不写一行 Elm 架构代码直接 `form.Run()` 独立运行（[huh README](https://github.com/charmbracelet/huh)）。
2. **全家族活跃度第一梯队**：bubbletea 44k★ 上周仍在提交，v2 线已发版（v2.0.8，2026-07-03），bubbles/huh/lipgloss 同步发 v2，生态对齐（见下表及引用）。
3. **Windows 10 + Windows Terminal 兼容良好**（原生 ConPTY 路径，含鼠标支持）；mintty（Git Bash 默认终端）需 `winpty` 前缀或改用 Windows Terminal 承载 Git Bash——用户现有环境已含 Windows Terminal，风险可控。

**备选：tview（+tcell）**——单体 widget 库，Form/InputField/DropDown/Pages 一应俱全，k9s 同款架构；适合想少学一套 Elm 模型的场景。风险是表单美观度与社区热度逊于 Charm 系。

**排除**：gocui 原版（15 个月无提交）、termui（仪表盘定位不符）、tui-go 与 survey（已归档）、promptui（约 2 年停滞）。

---

## 1. 候选库总览对比

数据抓取自 GitHub API，2026-08-14。"距上次推送"以当日计算。

| 库 | Stars | 最近推送 | 最新 release | 维护状态 | 定位 | 编程模型 |
|---|---|---|---|---|---|---|
| [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea) | 44,354 | 2026-08-12 | v2.0.8 (2026-07-03) | 活跃 | 通用 TUI 框架 | Elm 架构（Init/Update/View） |
| [rivo/tview](https://github.com/rivo/tview) | 14,036 | 2026-08-11 | v0.42.0 (2025-08-27) | 活跃（release 节奏慢，commit 持续） | widget 丰富的单体 UI 库 | widget 树 + 回调 |
| [gdamore/tcell](https://github.com/gdamore/tcell)（tview 底层） | 5,212 | 2026-08-13 | v3.4.1 (2026-07-19) | 活跃 | 底层 cell 渲染层 | 事件循环 |
| [jroimartin/gocui](https://github.com/jroimartin/gocui) | 10,594 | **2025-05-01（约 15 个月前）** | 无（API 404） | 事实停更，未归档 | 极简 CUI | 视图 + 回调 |
| [awesome-gocui/gocui](https://github.com/awesome-gocui/gocui) | 386 | 2026-02-09 | — | 社区 fork，低活跃 | 同上 | 同上 |
| [jesseduffield/gocui](https://github.com/jesseduffield/gocui) | 323 | 2026-04-02 | — | lazygit 专用 fork | 同上 | 同上 |
| [gizak/termui](https://github.com/gizak/termui) | 13,582 | **2025-07-10（约 13 个月前）** | 无 latest（API 404） | 低活跃 | 终端**仪表盘**（dashboard） | widget 网格 |
| [marcusolsson/tui-go](https://github.com/marcusolsson/tui-go) | 2,111 | 2021-10-12 | — | **已归档** | UI 库 | widget 树 |
| [AlecAivazis/survey](https://github.com/AlecAivazis/survey) | 4,108 | 2024-04-07 | — | **已归档** | 交互式 prompt（非全 TUI） | 回调 |
| [manifoldco/promptui](https://github.com/manifoldco/promptui) | 6,403 | **2024-08-06（约 2 年前）** | — | 停滞 | prompt/select | 回调 |
| [pterm/pterm](https://github.com/pterm/pterm) | 5,514 | 2026-07-11 | — | 活跃 | 控制台输出美化 + 交互组件 | 过程式调用 |

Charm 配套库（与 bubbletea 组合使用）：

| 库 | Stars | 最近推送 | 最新 release | 用途 |
|---|---|---|---|---|
| [charmbracelet/bubbles](https://github.com/charmbracelet/bubbles) | ~8.1k | 2026-08-12 | v2.1.1 (2026-07-04) | 标准组件：textinput/textarea/list/table/spinner/help 等 |
| [charmbracelet/lipgloss](https://github.com/charmbracelet/lipgloss) | 11,701 | 2026-08-12 | — | 样式/布局定义 |
| [charmbracelet/huh](https://github.com/charmbracelet/huh) | 7,104 | 2026-08-12 | v2.0.3 (2026-03-10) | **终端表单/多步向导**（建于 bubbletea 之上） |

数据来源：[bubbletea API](https://api.github.com/repos/charmbracelet/bubbletea)、[tview API](https://api.github.com/repos/rivo/tview)、[tcell API](https://api.github.com/repos/gdamore/tcell)、[gocui API](https://api.github.com/repos/jroimartin/gocui)、[awesome-gocui API](https://api.github.com/repos/awesome-gocui/gocui)、[jesseduffield/gocui API](https://api.github.com/repos/jesseduffield/gocui)、[termui API](https://api.github.com/repos/gizak/termui)、[tui-go API](https://api.github.com/repos/marcusolsson/tui-go)、[survey API](https://api.github.com/repos/AlecAivazis/survey)、[promptui API](https://api.github.com/repos/manifoldco/promptui)、[pterm API](https://api.github.com/repos/pterm/pterm)、[huh API](https://api.github.com/repos/charmbracelet/huh)、[bubbles API](https://api.github.com/repos/charmbracelet/bubbles)、[lipgloss API](https://api.github.com/repos/charmbracelet/lipgloss)、[bubbletea latest release](https://api.github.com/repos/charmbracelet/bubbletea/releases/latest)、[tview releases](https://api.github.com/repos/rivo/tview/releases?per_page=5)、[huh latest](https://api.github.com/repos/charmbracelet/huh/releases/latest)、[tcell latest](https://api.github.com/repos/gdamore/tcell/releases/latest)、[bubbles latest](https://api.github.com/repos/charmbracelet/bubbles/releases/latest)。

**关于 "scrutin"**：调研未发现名为 scrutin 的 Go TUI 库。scrutin 实际是一个 Rust 测试工具（`scrutin-tui` 是其 Ratatui 前端），与 Go 无关（[lib.rs 条目](https://lib.rs/development-tools/testing)、[crates.io scrutin-tui](https://crates.io/crates/scrutin-tui/0.0.11/dependencies)）。搜索中另出现一个小型声明式库 [grindlemire/go-tui](https://github.com/grindlemire/go-tui)（flexbox 布局），star 少、生态薄，不纳入候选。

---

## 2. 候选库详评

### 2.1 Bubble Tea v2（charmbracelet/bubbletea）+ Charm 全家桶

- **活跃度**：Go TUI 生态中最热（44,354★，1,289 fork），最近推送 2026-08-12（前一天）。官方 README 称其驱动超过 18,000 个应用，使用者包括 Microsoft Azure、AWS、NVIDIA、Cockroach Labs、Ubuntu（[README](https://github.com/charmbracelet/bubbletea)）。
- **版本状态**：v2 已是当前稳定线，最新 v2.0.8 发布于 2026-07-03，模块路径迁至 `charm.land/bubbletea/v2`，官方提供 v1→v2 升级指南 `UPGRADE_GUIDE_V2.md`（[release](https://api.github.com/repos/charmbracelet/bubbletea/releases/latest)、[README](https://github.com/charmbracelet/bubbletea)）。**注意：新项目直接上 v2 import 路径。**
- **编程模型**：Elm 架构——`Init`（初始命令）、`Update`（处理事件、更新 model）、`View`（按 model 渲染字符串）。声明式视图，无 widget 树、无回调（[README](https://github.com/charmbracelet/bubbletea)）。
- **能力**：高性能 cell 渲染器、声明式视图、高保真键盘/鼠标处理、原生剪贴板、内置色彩降采样、alt screen 与光标定位（[README](https://github.com/charmbracelet/bubbletea)）。Windows 上鼠标事件已原生支持（[issue #103](https://github.com/charmbracelet/bubbletea/issues/103)）；Windows 下 resize 事件机制与 Unix 不同，`WindowSizeMsg` 初始发一次、resize 时再发（[pkg.go.dev](https://pkg.go.dev/github.com/charmbracelet/bubbletea)）。
- **组件生态**：
  - **bubbles v2**（v2.1.1，2026-07-04）：Spinner、Text Input、Text Area、Table、Progress、Paginator、Viewport、List、File Picker、Timer、Stopwatch、Help、Key（[README](https://github.com/charmbracelet/bubbles)）。`textinput` 支持密码掩码：`EchoMode` 字段，`EchoPassword` 用 `EchoCharacter` 掩码显示，另有 `EchoNone`（完全不可见），官方文档原文 "EchoPassword displays the EchoCharacter mask instead of actual characters. This is commonly used for password fields."（[pkg.go.dev/bubbles/textinput](https://pkg.go.dev/github.com/charmbracelet/bubbles/textinput)）。
  - **huh v2**（v2.0.3，2026-03-10，import `charm.land/huh/v2`）：终端表单库，字段类型覆盖 `NewInput`（单行）、`NewText`（多行）、`NewSelect[T]`（单选）、`NewMultiSelect[T]`（多选）、`NewConfirm`、文件选择；字段级自定义校验并在出错时标注显示；**Group 机制把字段组作为分页，顺序翻页即多步向导**；内置 5 套主题 + Lipgloss 自定义样式；有 accessible 模式（退化为标准 prompt，利于读屏/录屏）；**既可 `form.Run()` 独立运行，也可作为 `tea.Model` 嵌入更大的 bubbletea 应用**（[README](https://github.com/charmbracelet/huh)）。
  - **lipgloss**（11,701★，2026-08-12 仍活跃）：样式与布局（[API](https://api.github.com/repos/charmbracelet/lipgloss)）。
- **学习曲线**：纯 bubbletea 需要理解 Elm 模型（model/update/view 消息循环），中等；但**如果主要交互是表单/向导，用 huh 可以完全绕开手写 Elm 循环**，代码量接近声明式配置。复杂界面（如服务器列表 + 详情双栏）再上 bubbletea + bubbles。
- **Windows**：cmd.exe 与 Windows Terminal 下表现良好（见第 4 节）；mintty 需 winpty。

### 2.2 tview（rivo/tview，基于 tcell）

- **活跃度**：14,036★，最近推送 2026-08-11；release 节奏慢——v0.42.0（2025-08-27）起开始采用语义化版本（[API](https://api.github.com/repos/rivo/tview)、[releases](https://api.github.com/repos/rivo/tview/releases?per_page=5)）。单一维护者（rivo，同时也是 tcell 维护者之一），bus factor 偏低但十年未断。
- **编程模型**：命令式 widget 树 + 回调（`SetDoneFunc` / `SetChangedFunc` 等），接近传统 GUI 编程；`Application.Run()` 阻塞跑事件循环。对不熟悉函数式/Elm 风格的人上手更快。
- **能力**：widget 覆盖广——输入表单（文本输入、选择、checkbox、按钮）、可导航多色文本视图、多行可编辑文本区、复杂表格、树视图、可选中列表、图片、Grid/Flexbox/Page 布局、Modal 模态窗（[README](https://github.com/rivo/tview)，源码含 form.go/inputfield.go/dropdown.go/checkbox.go/modal.go/pages.go/table.go/flex.go/grid.go/styles.go）。密码掩码：`InputField.SetMaskCharacter(rune)`（[pkg.go.dev/tview](https://pkg.go.dev/github.com/rivo/tview)）；鼠标：`Application.EnableMouse()`（同上，默认关闭）。多步向导用 `Pages` 手工组装。
- **底层 tcell**：5,212★，v3.4.1（2026-07-19），Apache-2.0，纯 Go 无 CGO，"Modern Windows is supported"，鼠标（含滚轮）在 Windows 上支持，有专门的 README-windows.md（[README](https://github.com/gdamore/tcell)、[API](https://api.github.com/repos/gdamore/tcell)）。
- **成熟先例**：**k9s 就是用 tview 构建的**（[博客佐证](https://blog.dennisokeeffe.com/blog/2025-08-19-building-tuis-with-golang-and-tview)、[教程](https://earthly.dev/blog/tui-app-with-go/)），证明其撑得起密集信息型管理界面。
- **短板**：样式美观度默认不如 Charm 系；Form 的跨页向导要自己用 Pages 拼；社区新增量与文档教程量不及 Charm。

### 2.3 gocui 系（jroimartin 原版 + 各 fork）

- 原版 [jroimartin/gocui](https://github.com/jroimartin/gocui)：10,594★ 但**最近推送 2025-05-01，约 15 个月无提交**，无 GitHub release（API 404），未归档但事实停更。
- [awesome-gocui/gocui](https://github.com/awesome-gocui/gocui)（386★，2026-02-09）与 [jesseduffield/gocui](https://github.com/jesseduffield/gocui)（323★，2026-04-02）都是低流量 fork；后者主要服务 lazygit 自身（[API](https://api.github.com/repos/jesseduffield/gocui)）。
- **lazygit 用的是 jesseduffield 的私有 fork**，且社区有公开 issue 讨论迁移到 Charm 库（[lazygit issue #2705](https://github.com/jesseduffield/lazygit/issues/2705)）——即 gocui 系最著名的旗舰用户都在考虑搬家。
- widget 极少（基本只有 View + 手绘内容），表单/向导全要自建。**结论：新项目不应选 gocui。**

### 2.4 termui（gizak/termui）

- 13,582★，但**最近推送 2025-07-10（约 13 个月前）**，活跃度低（[API](https://api.github.com/repos/gizak/termui)）。
- 定位是**终端仪表盘**（"Golang terminal dashboard"）——为图表/gauge/grid 监控面板设计，不是交互表单型应用框架。与本项目"向导式录入与授权"需求不匹配。**排除。**

### 2.5 tui-go（marcusolsson/tui-go）

- 2,111★，**已归档（archived=true）**，最后推送 2021-10-12（[API](https://api.github.com/repos/marcusolsson/tui-go)）。**明确排除。**

### 2.6 survey / promptui / pterm（补充参考）

- [AlecAivazis/survey](https://github.com/AlecAivazis/survey)：4,108★，**已归档**（2024-04 停更）（[API](https://api.github.com/repos/AlecAivazis/survey)）。曾是 Cobra 生态最常用的交互 prompt 库，现不应新采用。
- [manifoldco/promptui](https://github.com/manifoldco/promptui)：6,403★，最后推送 2024-08-06，约 2 年停滞（[API](https://api.github.com/repos/manifoldco/promptui)）。
- [pterm/pterm](https://github.com/pterm/pterm)：5,514★，2026-07-11 活跃（[API](https://api.github.com/repos/pterm/pterm)）。定位是控制台输出美化 + 轻量交互（select、text input），不是全屏 TUI 框架；可作为非 TUI 场景（脚本化输出美化）的补充，不满足"图形化管理界面"需求。

---

## 3. 与现有 Cobra CLI 的集成模式

**推荐模式：TUI 作为同一 Cobra 二进制的一个子命令（`ssh-manager tui`），在 `RunE` 里启动 TUI 程序**；其余子命令保持纯 flag 式不动（脚本/CI 仍可用）。

这是社区成熟且广泛采用的模式：

- 通用做法：定义一个 Cobra command，其 `Run`/`RunE` 中调用 `tea.NewProgram(model).Run()` 把终端交给 Bubble Tea，其他子命令照旧——多篇一手教程覆盖此模式：
  - [Interactive Go CLIs with Cobra command trees and Bubble Tea](https://botmonster.com/coding/build-cli-tool-go-cobra-bubble-tea/)（同一二进制同时服务人类操作者、shell 脚本和 CI/CD）
  - [Charming Cobras with Bubbletea (elewis.dev)](https://elewis.dev/charming-cobras-with-bubbletea-part-1)
  - [Attaching TUI to Cobra (Medium)](https://medium.com/@originalrad50/building-ui-of-golang-cli-app-with-bubble-tea-68b61e25445e)
  - [Terminal Applications in Go (harrisoncramer.me)](https://harrisoncramer.me/terminal-applications-in-go/)
  - [Bubble Tea or Cobra (r/golang)](https://www.reddit.com/r/golang/comments/1v77ghn/bubble_tea_or_cobra/)：社区共识是二者互补——Cobra 管命令/参数解析，Bubble Tea 管 TUI。
- **产品级先例**：
  - **k9s**：tview 全屏 TUI 为主体，CLI 参数用 Cobra 处理，单二进制（[tview 佐证](https://blog.dennisokeeffe.com/blog/2025-08-19-building-tuis-with-golang-and-tview)）。
  - **lazygit**：独立二进制、无 Cobra，基于自家 gocui fork（[HN 讨论](https://news.ycombinator.com/item?id=32033187)、[lazygit issue #2705](https://github.com/jesseduffield/lazygit/issues/2705)）——其"独立命令"形态是因为 TUI 是产品全部，而本项目 CLI 已存在且有脚本化场景，**子命令挂载优于独立命令**。
- 细节坑：从子 shell/子命令环境启动 bubbletea 程序的注意点见 [bubbletea issue #206](https://github.com/charmbracelet/bubbletea/issues/206)。
- 本项目落地形态建议：
  - `ssh-manager tui`：全屏主界面（服务器列表 + 详情 + 操作菜单，bubbletea + bubbles list/table）。
  - `ssh-manager servers add` 等 flag 不足时自动 fallback 到 huh 向导（无 flag 交互式、有 flag 保持纯 CLI），或 `--interactive` 开关——huh 的 `form.Run()` 独立模式让"单命令向导化"成本极低（[huh README](https://github.com/charmbracelet/huh)）。

---

## 4. Windows 10 + Git Bash / Windows Terminal 兼容性（重点）

- **Bubble Tea**：在 cmd.exe 与 Windows Terminal 下工作良好；Windows 下已原生支持鼠标事件（[issue #103](https://github.com/charmbracelet/bubbletea/issues/103)）；resize 事件行为差异见 [pkg.go.dev](https://pkg.go.dev/github.com/charmbracelet/bubbletea)。代码库含 `tty_windows.go`/`termios_windows.go`/`signals_windows.go` 等一等公民 Windows 支持（[仓库文件列表](https://github.com/charmbracelet/bubbletea)）。
- **tview/tcell**："Modern Windows is supported"，纯 Go 无 CGO，鼠标（含滚轮）支持 Windows，有官方 README-windows.md 专述细节（[tcell README](https://github.com/gdamore/tcell)）。
- **mintty（Git Bash 默认终端）是两家的共同弱点**：mintty 是 Cygwin/MSYS 伪终端，不走 Windows Console API，Go TUI 程序（bubbletea/tview 均如此）在 mintty 下可能渲染/输入异常；绕法是 `winpty <app>.exe` 前缀（[搜索综述](https://www.reddit.com/r/git/comments/1dhmxvt/is_there_any_benefit_to_using_mintty_over_windows/)，及 [Devin CLI 终端兼容性文档](https://docs.devin.ai/zh/cli/reference/terminal-compatibility) 同类结论）。
- **用户环境建议**：在 **Windows Terminal 里加一个 Git Bash profile**（指向 `C:\Program Files\Git\bin\bash.exe`），获得 ConPTY 支持，即可原生运行 TUI；README/文档中注明"mintty 下请用 `winpty ssh-manager tui` 或改用 Windows Terminal"。

---

## 5. 表单/向导需求逐项核对

本项目核心交互：`servers add` 多字段录入（含密码/密钥路径）、授权 agent 的单选/多选、跨 servers/profiles/projects 的多步流程、确认弹窗。

| 需求 | Charm 系（huh/bubbles） | tview | gocui | termui |
|---|---|---|---|---|
| 多字段表单 | huh `NewInput()`/`NewText()`，字段级校验（[README](https://github.com/charmbracelet/huh)） | Form + InputField（[README](https://github.com/rivo/tview)） | 手工自建 | 不适用 |
| 密码掩码 | textinput `EchoPassword`/`EchoNone`（[pkg.go.dev](https://pkg.go.dev/github.com/charmbracelet/bubbles/textinput)）；huh Input `EchoMode(textinput.EchoPassword)` | `InputField.SetMaskCharacter`（[pkg.go.dev](https://pkg.go.dev/github.com/rivo/tview)） | 手工自建 | 不适用 |
| 单选 | huh `NewSelect[T]`（同上） | DropDown（[README](https://github.com/rivo/tview)） | 手工 | 不适用 |
| 多选 | huh `NewMultiSelect[T]` | Checkbox / Table 多选（同上） | 手工 | 不适用 |
| 多步向导 | huh Group = 分页顺序导航（[README](https://github.com/charmbracelet/huh)） | Pages 手工组装（同上） | 手工 | 不适用 |
| 确认/模态 | huh `NewConfirm` | Modal（同上） | 手工 | 不适用 |
| 校验与错误提示 | 内置，"form will mark erroneous fields and display error messages"（[README](https://github.com/charmbracelet/huh)） | InputField 自带校验/autocomplete（[pkg.go.dev](https://pkg.go.dev/github.com/rivo/tview)） | 无 | 无 |
| 无障碍/降级 | huh accessible 模式退化为标准 prompt（[README](https://github.com/charmbracelet/huh)） | 无对应 | — | — |

**结论：Charm 系对表单/向导场景的覆盖度最高且最省代码；tview 功能够用但向导需手工拼 Pages。**

---

## 6. 最终推荐

### 主选：Bubble Tea v2 + huh v2 + bubbles v2 + Lipgloss

**组合分工**：

- `ssh-manager servers add --interactive`（或无 flag 自动触发）→ **huh 向导**：一个 Form 三个 Group（基本信息 → 凭据（密码字段 `EchoMode(EchoPassword)`）→ 确认提交），替代十几个 flag。
- 授权 agent 流程 → **huh 多步向导**（选 server → 选/建 profile → 选 project → confirm），或嵌入 bubbletea 程序里作为一个页面。
- `ssh-manager tui` 全屏管理台 → **bubbletea + bubbles**（list/table 双栏 + help bar）+ lipgloss 样式。
- 挂载方式：Cobra 子命令，`RunE` 中 `tea.NewProgram(...).Run()`。

**理由**：表单/向导开箱即用（本项目第一需求）、活跃度与生态第一、Windows Terminal 兼容好、单二进制集成模式有大量先例和教程。

**风险与对策**：

1. **v1/v2 生态分水岭**：bubbletea v2 模块路径已迁至 `charm.land/bubbletea/v2`（[README](https://github.com/charmbracelet/bubbletea)）。huh v2、bubbles v2.1.1 已对齐，但第三方示例多为 v1 写法；落地时统一用 v2 import，参考官方 `UPGRADE_GUIDE_V2.md`。
2. **Elm 架构学习成本**：全屏 TUI 部分需要写 update loop；缓解——向导部分全用 huh，复杂界面从小做起。
3. **mintty 兼容**：Git Bash 默认 mintty 下需 `winpty` 或改用 Windows Terminal（第 4 节）；文档写明。
4. **huh 定制上限**：深度定制外观/非常规交互时 huh 会露底（退回手写 bubbletea）；本项目向导形态规整，风险低。
5. **Charm 供应商集中**：全家桶单靠 Charm 公司；但全部 MIT 开源，可 fork 自保，且 44k★ 社区规模下风险可接受。

### 备选：tview（+tcell）

若团队更适应命令式 widget/回调模型、或想要 k9s 同款架构，tview 能以更直白的心智模型完成同样的表单/向导（Form + InputField + DropDown + Pages），Windows 支持同样扎实。代价：向导手工组装、默认观感一般、单维护者 bus factor。

### 明确不选

- gocui（原版事实停更 15 个月；lazygit 依赖私有 fork 且公开讨论迁移 Charm）；
- termui（仪表盘定位不符 + 低活跃）；
- tui-go、survey（已归档）；
- promptui（约 2 年停滞）；
- pterm（非全屏 TUI，仅适合输出美化）；
- scrutin（不存在 Go 库，系 Rust 工具）。

---

## 引用索引

**GitHub 仓库 / API（活跃度，2026-08-14 抓取）**

- bubbletea: <https://github.com/charmbracelet/bubbletea> / <https://api.github.com/repos/charmbracelet/bubbletea> / <https://api.github.com/repos/charmbracelet/bubbletea/releases/latest>
- tview: <https://github.com/rivo/tview> / <https://api.github.com/repos/rivo/tview> / <https://api.github.com/repos/rivo/tview/releases?per_page=5>
- tcell: <https://github.com/gdamore/tcell> / <https://api.github.com/repos/gdamore/tcell> / <https://api.github.com/repos/gdamore/tcell/releases/latest>
- jroimartin/gocui: <https://github.com/jroimartin/gocui> / <https://api.github.com/repos/jroimartin/gocui>
- awesome-gocui/gocui: <https://github.com/awesome-gocui/gocui> / <https://api.github.com/repos/awesome-gocui/gocui>
- jesseduffield/gocui: <https://api.github.com/repos/jesseduffield/gocui>
- termui: <https://github.com/gizak/termui> / <https://api.github.com/repos/gizak/termui>
- tui-go: <https://api.github.com/repos/marcusolsson/tui-go>
- survey: <https://api.github.com/repos/AlecAivazis/survey>
- promptui: <https://api.github.com/repos/manifoldco/promptui>
- pterm: <https://github.com/pterm/pterm> / <https://api.github.com/repos/pterm/pterm>
- huh: <https://github.com/charmbracelet/huh> / <https://api.github.com/repos/charmbracelet/huh> / <https://api.github.com/repos/charmbracelet/huh/releases/latest>
- bubbles: <https://github.com/charmbracelet/bubbles> / <https://api.github.com/repos/charmbracelet/bubbles> / <https://api.github.com/repos/charmbracelet/bubbles/releases/latest>
- lipgloss: <https://github.com/charmbracelet/lipgloss> / <https://api.github.com/repos/charmbracelet/lipgloss>

**官方文档**

- bubbletea pkg.go.dev: <https://pkg.go.dev/github.com/charmbracelet/bubbletea>
- bubbles textinput（EchoMode）: <https://pkg.go.dev/github.com/charmbracelet/bubbles/textinput>
- tview（EnableMouse / SetMaskCharacter）: <https://pkg.go.dev/github.com/rivo/tview>

**Windows / 终端兼容**

- bubbletea Windows 鼠标: <https://github.com/charmbracelet/bubbletea/issues/103>
- mintty/Git Bash 讨论与 winpty 绕法: <https://www.reddit.com/r/git/comments/1dhmxvt/is_there_any_benefit_to_using_mintty_over_windows/>
- 终端兼容性参考（Devin CLI 文档）: <https://docs.devin.ai/zh/cli/reference/terminal-compatibility>

**Cobra 集成先例与教程**

- Interactive Go CLIs with Cobra + Bubble Tea: <https://botmonster.com/coding/build-cli-tool-go-cobra-bubble-tea/>
- Charming Cobras with Bubbletea: <https://elewis.dev/charming-cobras-with-bubbletea-part-1>
- Attaching TUI to Cobra: <https://medium.com/@originalrad50/building-ui-of-golang-cli-app-with-bubble-tea-68b61e25445e>
- Terminal Applications in Go: <https://harrisoncramer.me/terminal-applications-in-go/>
- Bubble Tea or Cobra（r/golang）: <https://www.reddit.com/r/golang/comments/1v77ghn/bubble_tea_or_cobra/>
- 子 shell 启动程序注意点: <https://github.com/charmbracelet/bubbletea/issues/206>

**架构先例**

- lazygit（gocui 系 + 迁移 Charm 讨论）: <https://github.com/jesseduffield/lazygit/issues/2705> / <https://news.ycombinator.com/item?id=32033187>
- k9s = tview: <https://blog.dennisokeeffe.com/blog/2025-08-19-building-tuis-with-golang-and-tview> / <https://earthly.dev/blog/tui-app-with-go/>

**scrutin 查证**

- <https://lib.rs/development-tools/testing> / <https://crates.io/crates/scrutin-tui/0.0.11/dependencies>
