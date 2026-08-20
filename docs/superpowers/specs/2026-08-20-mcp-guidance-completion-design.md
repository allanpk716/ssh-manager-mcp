# 设计：TUI token 配置引导补全 + 双模式 TUI 教程 + agent 工具手册

- 日期：2026-08-20
- 状态：已与 owner 对齐方向（MCP 工具手册口径、三交付物方案 A），待 spec 审阅
- 触发场景：把单机版 exe 交给同事部署时，"事后在 TUI 发 token 引导断层"与"文档缺少分受众教程"暴露

---

## 1. 背景与问题

推荐本工具给同事（单机版 exe）时发现三个缺口：

### 缺口 1 — 主控台事后发 token 引导断层（代码）

首跑向导三个角色的 finish 屏都有完整 `.mcp.json` 引导，但**事后**在 TUI 主控台 Projects 页 `a` 新增 / `e` 轮换 project 时，`tokenIssuedMsg` 渲染的 `secretView`（`internal/tui/app.go:464`）**只有裸 token**：无 `.mcp.json` 片段、无"token 去哪"用途行、无"丢失→rotate"恢复行。对比同场景两条旧路径均有引导：CLI `printToken`（`internal/cli/projects.go:265`）打印完整片段；向导 `wizTokenScreen`（`internal/tui/wizardsteps.go:147`）强制带用途+丢失提示。用户凭首跑记忆手拼 `.mcp.json`，典型翻车面：token 写进 `args`（ps 可见）或漏 env 字段。

### 缺口 2 — 在线 http 形态任何 TUI 屏都没展示（代码）

`.mcp.json` 实际有三种形态，TUI 只覆盖两种：

| 形态 | 场景 | TUI 覆盖 |
|---|---|---|
| stdio（`args:["mcp"]`） | 单机 / server 本机 agent | ✅ `mcpConfigScreen`（wizardsteps.go:219） |
| http（`"type":"http"` + serve URL + Bearer header） | 联机在线 agent | ❌ 只存在于 README/multi-machine.md 文档 |
| 离线缓存（`args:["mcp","--cache"]`） | 联机离线 client | ✅ `clientFinishScreen`（clientpage.go:418），但**只**给这一种——联机用户被单一形态误导 |

### 缺口 3 — 文档缺分受众教程（文档）

- 人看的 **TUI 教程**没有单机/联机分篇（README TUI 节是紧凑参考，非教程；quickstart 是 CLI 体裁）
- **agent 视角**的工具使用手册不存在（`agent-access.md` 是 owner 视角授权文档；agent 拿到 6 个工具后，超时/截断/sudo 语义/id 纪律/错误对照无文档可读）

---

## 2. 目标 / 非目标

### 目标

1. **G1**：主控台 Projects 页发/换 token 的 `secretView` 补全引导（token + 用途 + 丢失恢复 + `.mcp.json` 片段 + 另两形态指针），与 CLI `printToken` 行为对齐。
2. **G2**：`clientFinishScreen` 补在线 http 形态，双形态并列（离线 `--cache` 为主、在线 http 为辅）。
3. **G3**：新增 `docs/tui-single-machine.md`（人·单机 TUI 教程）。
4. **G4**：新增 `docs/tui-multi-machine.md`（人·联机 TUI 教程，server 侧 + client 侧双视角）。
5. **G5**：新增 `docs/agent-tools.md`（AI agent 视角 MCP 工具手册 + 可复制 CLAUDE.md/AGENTS.md 规则模板）。
6. **G6**：README / quickstart ×2 / agent-access.md 联动更新（链接、瘦身、指针）。

### 非目标（owner 已拍板或明确排除）

- ❌ 不改首跑向导的两屏流（token 屏 → 配置屏已达标；`wizard.go:735` 发射点行为保持不变）。
- ❌ 不写 agent 侧 CLI 文档——`ssh-manager ssh` 子命令是 owner 全库直通通道（不经 token、不受 profile 限制），写进 agent 手册等于教它绕开隔离，与项目铁律矛盾。agent 的唯一授权入口是 6 个 MCP 工具。
- ❌ 不改 CLI `printToken`（现状已达标）。
- ❌ 不做 TUI 截图/动画（纯文本走查体裁；截图维护成本高）。
- ❌ 不给 `secretView` 加滚动（见 §6 风险 R1，接受小终端截断）。

---

## 3. 现状事实（调研锚点）

| 事实 | 锚点 |
|---|---|
| `tokenIssuedMsg{title, token}` 双字段，`secretView` body=裸 token | `internal/tui/tokenview.go:10`、`internal/tui/app.go:458-466` |
| Projects `a`/`e` 发射裸 token | `internal/tui/app.go:294-300`、`app.go:311-317` |
| 设备码发射点已带完整 body（指纹+示例命令） | `internal/tui/app.go:373-377`（`deviceCodeBody`） |
| 向导自持 tokenIssuedMsg 处理（两屏流） | `internal/tui/wizard.go:482-490` |
| `mcpConfigLines` 只渲染 stdio 形态（command+fieldLines） | `internal/tui/wizardsteps.go:189-211` |
| `clientFinishScreen()` 无参数，只渲染 `--cache` 形态 | `internal/tui/clientpage.go:418-431` |
| broker 主控台**无条件**建 4 页签（单机角色也有设备码页） | `internal/tui/app.go:133-136` |
| http 形态文档版（url 带尾斜杠） | `README.md` Multi-machine 节、`docs/multi-machine.md` Step 3 |
| 测试锚点 | `internal/tui/projects_test.go`、`app_test.go`、`clientpage_test.go`、`wizardsteps_test.go` |

---

## 4. 设计

### 4.1 代码 — `tokenIssuedMsg` 扩展（G1）

**tokenview.go**：

```go
// tokenIssuedMsg carries a freshly minted token from a store mutation cmd to
// App.Update, which swaps in a secretView overlay. usage/recovery/snippet are
// OPTIONAL guidance (zero value = current bare-token behavior):
//   - usage:    "token 去哪"一行（wizTokenScreen 的用途行同款纪律）
//   - recovery: "丢失→"一行（store 只存 hash，明文不可恢复）
//   - snippet:  .mcp.json 片段行（mcpConfigLines 输出，token 已代入）
type tokenIssuedMsg struct {
    title, token string
    usage        string
    recovery     string
    snippet      []string
}

// body renders the full secretView body: token first (always), then the
// optional guidance blocks in fixed order (用途 → 丢失 → 片段).
func (m tokenIssuedMsg) body() string { … }
```

- `App.Update` 的 `case tokenIssuedMsg`（app.go:458-466）改为 `a.overlay = &secretView{title: m.title, body: m.body()}`。
- 零值兼容：设备码发射点（app.go:377，token 字段放整段 `deviceCodeBody`）与向导发射点（wizard.go:735）不填新字段 → `body()` 输出 = 现状裸 token / 裸 body，**行为不变**。

**app.go — 新 helper（两发射点共用）**：

```go
// projectTokenMsg builds the guidance-complete tokenIssuedMsg for Projects
// page add/rotate: real token embedded in the snippet (single one-time
// screen, CLI printToken parity — the wizard's placeholder approach is for
// its two-screen flow and does not apply here).
func projectTokenMsg(title, token string) tokenIssuedMsg
```

body 结构（渲染后形态）：

```
<token>

用途：填进 agent 的 .mcp.json（下方片段已代入此 token，抄完即用）
⚠ 仅此一次。丢失 → Projects 页 [e] 轮换换发（旧 token 立即失效）

把下面的片段写进 agent 项目的 .mcp.json：
{
  "mcpServers": {
    "ssh": {
      "command": "ssh-manager",
      "args": ["mcp"],
      "env": { "SSHMGR_TOKEN": "<真·token>" }
    }
  }
}

说明：
- Windows 建议写绝对路径，如 "command": "C:\\Tools\\ssh-manager.exe"。
- 联机场景另有两种形态：在线 http / 离线 --cache → docs/multi-machine.md「离线只读缓存」Step 2。
- .mcp.json 含 token，不要提交进 git。
```

- 片段复用 `mcpConfigLines`（fieldLines = `args` + `env` 真 token 代入）。
- Projects `a` 用 title `"项目 token"`，`e` 用 `"项目 token（已轮换）"`（现状标题不变）。
- `secretView` 固定 footer「⚠ 仅此一次显示（关闭后不可再看）。按任意键返回。」保留——与 body 内「仅此一次」语义一致不冲突（一个讲不可再看，一个讲丢了怎么办）。

### 4.2 代码 — `clientFinishScreen` 双形态（G2）

**wizardsteps.go — 新增 http 形态渲染器**：

```go
// mcpHttpConfigLines renders the ONLINE (serve/http) .mcp.json snippet —
// sibling of mcpConfigLines (stdio shape). Extract the shared skeleton
// (members+notes rendering, trailing-comma discipline) into a private
// helper both builders call, so the comma discipline exists in ONE place.
func mcpHttpConfigLines(urlRef, tokenRef string, notes []string) []string
```

渲染形态（与 multi-machine.md Step 3 文档版一致）：

```
{
  "mcpServers": {
    "ssh": {
      "type": "http",
      "url": "<urlRef>",
      "headers": { "Authorization": "Bearer <tokenRef>" }
    }
  }
}
```

**clientpage.go — 签名改造**：

```go
// clientFinishScreen(serveURL): 离线 --cache 为主（现有内容保留）+
// 在线 http 为辅（新增块）。serveURL 取 clientModel 的 cred.URL 原样代入
// （不做改写/补斜杠——与用户在连接表单里填的保持一致）；
// 空值防御：cred 为 nil 或 URL 空时显示 "<serve URL>" 占位。
func clientFinishScreen(serveURL string) overlay
```

finish 屏结构：标题不变（「配置 agent 的 .mcp.json（client 模式）」）；正文两块，各带小节引导行——「离线为主（默认推荐）」→ `--cache` 片段；「在线为主」→ http 片段，note 必含：

- `"type": "http"` **必填**——漏了 Claude Code 会当 stdio 处理并拒绝该条目；
- 两种形态用的是**同一个 project token**（server 机 Projects 页签发）；
- 保留现有三条 note（token 不是设备码 / Windows 绝对路径 / 别提交 git，按形态归属到对应块）。

调用点（2 处，client 向导 pull 成功链路）传 `m.cred.URL`。

### 4.3 代码 — 测试（G1/G2）

| 文件 | 断言 |
|---|---|
| `tokenview_test.go`（新增） | `body()`：裸 token 恒在首位；usage/recovery/snippet 零值 → 输出仅 token（设备码/向导兼容锚）；三字段齐 → 分块顺序 token→用途→丢失→片段 |
| `app_test.go` / `projects_test.go` | `projectTokenMsg` 构造的 msg：snippet 含真 token 代入的 `"env": { "SSHMGR_TOKEN": "<token>" }`、`"args": ["mcp"]`；recovery 含「轮换」；rotate title 与 add title 区分 |
| `wizardsteps_test.go` | `mcpHttpConfigLines`：`"type": "http"` / url / Bearer 代入；尾成员无逗号（空 notes 也合法）；与 `mcpConfigLines` 共享骨架不回归（现有用例不动） |
| `clientpage_test.go` | `clientFinishScreen("https://192.0.2.5:7878")`：同时含 `--cache` 片段、`"type": "http"`、serveURL、「必填」note；`clientFinishScreen("")` → 占位符不 panic |
| 向导回归 | `wizard_test.go` / `wizardserve_test.go` 现有用例零改动全绿（证明 wizard 路径未受影响） |

### 4.4 文档 — `docs/tui-single-machine.md`（G3，人·单机）

1. **定位**：一台机、全键盘、不想记命令的入门路径；与 `quickstart-single-machine.md` 互链（quickstart=CLI 速通，本篇=TUI 教程），两者殊途同归（同一套 vault 操作）。
2. **首跑向导走查**：空机 `ssh-manager tui` → 「这台电脑要保管所有 SSH 凭据吗？」→ 是→单机 → 自动建 vault（含已锁 vault 的引导报错分支）→ 录服务器表单（密码/密钥二选一强制、sudo 可选）→ 「继续添加？」循环 → profile+grant（未选=agent 暂时看不到任何服务器）→ project → token 屏（用途/丢失行）→ `.mcp.json` 配置屏 → 主控台。可中断续配语义（role.json 即存，重跑 `tui` 回到流程）。
3. **主控台四页签参考**：服务器（`a`/`e`/`d`/`i`/`!`）/ Profiles（`a`/`g`）/ Projects（`a`/`e`/`d`）/ 设备码（**注明：仅 serve 联机部署时有用，单机忽略**——页签无条件存在是现状事实，见 §3）。
4. **典型任务走查**：加第二台服务器；给第二个 agent 发 token（走新 secretView 含片段，与 §4.1 联动截图式文字走查）；批量导入 ssh config（`i` 多选→补全循环→`!` 过滤 ⚠）；轮换 token。
5. **排错**：mintty 需 `winpty ssh-manager tui`；非 TTY 直接报错不挂死；vault 锁定 → `unlock`；Windows 终端推荐。
6. **安全面注记**：凭据输入全程掩码、已设凭据只显示「已设置」、token/设备码一次性全屏显示。

### 4.5 文档 — `docs/tui-multi-machine.md`（G4，人·联机）

1. **定位与全景图**：server 机（broker 主控台）+ 工作机（client 面板）两个视角一张 ASCII 图（对齐 multi-machine.md 架构图风格）。
2. **server 侧走查**：空机 `tui` → 「是→分享给其他机→server」→ vault/录入/grant → 双密钥屏（project token 1/2——用途标注给 client 机 `.mcp.json`；设备码 2/2）→ serve 服务安装（向导内非阻断 + 主控台后续管理）→ 主控台。
3. **client 侧走查**：空机 `tui` → 「否→client（需先在 server 机完成设置）」→ 连接表单（url/pin/设备码，源提示 + 失败分类横幅保留输入）→ 首次 pull → finish 屏**双形态**（与 §4.2 联动，在线/离线怎么选给一句判定：笔记本常出门=离线 `--cache` 为主）→ client 面板。
4. **client 面板参考**：页头连接摘要（broker URL/pin 前缀/缓存年龄/服务器数）、`s` 同步（10s 超时失败保留旧缓存）、`c` 编辑连接（设备码掩码不预填，留空=不变）、`t` TTL 说明；零远程写语义。
5. **典型任务**：新工作机接入全流程（server 设备码页 `a` 签发 → client 向导 → agent 验证 `list_servers`）；机器失窃处置（`cache-tokens revoke` + `projects rotate`，含"已拉缓存仍可被本机 DEK 解"的如实说明）；在线/离线 `.mcp.json` 互切（改配置+重启客户端）。
6. **排错**：指纹失配 ≠ 泄露（serve 重签证书 vs MITM）；无 pin hard-fail 与 `--allow-plaintext`；缓存 TTL/自动保鲜行为；断连语义四层指针（agent-access.md）。
7. **与 multi-machine.md 分工声明**：本篇=TUI 操作教程；multi-machine.md=架构/CLI/runbook 深水区（TLS 迁移、证书轮换、export/import 等不在本篇展开）。

### 4.6 文档 — `docs/agent-tools.md`（G5，AI agent 视角）

读者是 agent 本身（或替 agent 配置环境的 owner）。语言中文、直接称「你」。

1. **你是谁/你有什么**：6 工具清单表 + 铁律（MCP 工具是**唯一**授权入口；禁裸 `ssh`/`scp`/从文件系统找私钥——那是旁路，broker 启动时检测到散落凭据会 stderr 告警）。
2. **标准工作流**：先 `list_servers` 拿真实 `id`（**name ≠ id**，跨工具引用一律用 id）→ 动手前读该机的 `caveats`/`role`/`services`（owner 给的操作须知）→ `has_sudo=false` 就别尝试提权命令。
3. **逐工具语义**：
   - `list_servers`：字段全解（id/name/host/user/has_sudo + role/services/location/hardware/caveats/tags/description，永不含凭据）。
   - `exec_command`：`sudo=true` 让 broker 跑 `sudo -S`——**别自己拼 `sudo` 前缀**；连接+执行共享 120s 默认 / 5min 硬上限——长任务拆步或后台化；每通道输出 1 MiB 封顶，`truncated=true` 时 refine 命令（`tail`/`grep`/`head`）而不是硬拉。
   - `download_file`：单文件、大小帽、截断标志同上；目录树 → 先 `exec_command` 远端 `tar` 再下 tar。
   - `upload_file`：本地文件**或目录**（递归）；上传前确认远端目标路径（preflight 拒绝语义照实写）。
   - `forward_port`：返回 `127.0.0.1:<port>` 供本地 `curl`/客户端使用；只支持本地转发（`-L`）；**创建后 ~10 分钟自动回收**，用完主动 `close_port`。
   - `close_port`：关闭转发；401 = token 已失效，别重试开新隧道。
4. **错误对照表**（每条给"你该做什么"）：token 无效/被轮换/被禁用 → 报告 owner；`server is not in your profile` → 重新 `list_servers` 核对 id；`no_credential` → 报告 owner 补凭据（别自己想办法）；timeout → 拆小命令；`truncated` → refine；host key 失配（TOFU fail-closed）→ 报告 owner 核实，**别**尝试绕过。
5. **环境差异**：在线 serve（可写）vs 离线 cache（只读）——遇 `ErrReadOnly`/read-only 报错 → 报告 owner 切在线，**别**重试写操作。
6. **附录：可复制规则模板**：十几行 CLAUDE.md/AGENTS.md 段落（铁律 + 工具前缀清单 + id 纪律 + 「工具报错查 docs/agent-tools.md 错误对照表」），同事整段贴进自己的 agent 项目即可；模板明确「按需替换工具前缀（mcp__ssh__* / 客户端实际命名）」。

### 4.7 文档联动（G6）

| 文件 | 改动 |
|---|---|
| `README.md` | Documentation 表加 3 行（两篇 TUI 教程 + agent-tools.md，含一句话读者定位）；TUI 节瘦身为「模式判定 + 终端要求 + 指向两篇教程」；Multi-machine 节 http 片段旁补一句「client 向导 finish 屏现在会同时展示两形态」 |
| `quickstart-single-machine.md` | Step 3-4 的 TUI 提示改为指向 `tui-single-machine.md`；Step 5 尾部加一句「把 docs/agent-tools.md 的规则模板贴进你的 CLAUDE.md，agent 会更守规矩」 |
| `quickstart-multi-machine.md` | 对应位置指向 `tui-multi-machine.md` 与 agent-tools.md |
| `agent-access.md` | 顶部互链 agent-tools.md（「发完卡把这份手册/模板给 agent」） |
| `docs/README.md`（文档索引） | 「目录」表加 3 行（两篇 TUI 教程 + agent-tools.md）；「两个角色」表的 Agent 行补 agent-tools.md 指针（owner CLI 行不动） |

---

## 5. 验收标准（可测）

1. `go build ./...` 通过；`go test ./internal/tui/...` 全绿（含 §4.3 全部新断言；现有 wizard 用例零改动）。
2. 手工验证（worktree 内 `go run ./cmd/ssh-manager tui`，临时 `SSHMGR_STORE`）：Projects 页 `a` 新增 project → 全屏含真 token 代入的完整 `.mcp.json` 片段 + 用途 + 丢失行；`e` 轮换同理；设备码签发屏与现状一致（回归确认）。
3. client 向导路径手工验证受限于双机环境——以 `clientpage_test.go` 断言 + `clientFinishScreen` 渲染单测覆盖（真双机 E2E 不在本期验收，走既有 NUC10 惯例后续）。
4. 文档：三篇新文档存在且互链无死链（`grep -o` 相对链接目标逐一存在）；README/quickstart/agent-access 联动行落地。
5. 片段一致性：文档中的 stdio 片段与 `mcpConfigLines` 渲染输出、http 片段与 `mcpHttpConfigLines` 渲染输出**逐字节一致**（plan 阶段用临时 `go run` 渲染比对，人工核对后固化进文档）。
6. 合并时按仓库惯例 bump 版本号（建议 v0.8.10，merge 时 owner 可改）。

---

## 6. 风险与边界

- **R1 — secretView 变长 vs 小终端**：token+片段+notes 约 20 行，`secretView` 无滚动，极小终端（<24 行）会截断。**接受**：CLI `printToken` 同为单屏等价物；不为引导内容加滚动（复杂度不成比例）。
- **R2 — 片段内嵌真 token 的暴露面**：与 CLI `printToken` 完全一致（同屏一次性显示），不新增暴露面；token 只存在于 `tokenIssuedMsg` 一跳 + overlay 内存中（现有纪律，注释已声明）。
- **R3 — 文档三处片段漂移**：stdio/http 片段将存在于「代码渲染 + 3 篇新文档 + README/multi-machine.md」。缓解：验收 5 的逐字节比对 + 源头只有 `mcpConfigLines`/`mcpHttpConfigLines` 两个渲染器；文档互链以代码渲染为准。
- **R4 — `clientFinishScreen(serveURL)` 空值**：cred 为 nil / URL 空的防御已在 §4.2 定义（占位符），测试覆盖。
- **R5 — 并发 agent 冲突**：全程隔离 worktree（`worktree-tui-mcp-guidance` 分支），merge 前不过夜（仓库既有纪律）。

---

## 7. 实施注意

- 顺序：代码（§4.1→4.2→4.3）→ 文档三篇（§4.4-4.6）→ 联动（§4.7）→ 验收（§5）。
- 文档语言全部中文（对齐 docs/ 现状）；agent-tools.md 直接称呼 agent「你」。
- 不改 `internal/cli`、不改 `internal/mcpserver`、不改 store 层——纯 TUI 表现层 + 文档。
- commit 拆分建议：① 代码+测试 ② 三篇新文档 ③ 联动更新（merge 时按仓库惯例 squash/merge 均可）。
