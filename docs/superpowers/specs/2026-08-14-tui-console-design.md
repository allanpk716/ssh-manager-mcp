# 设计 spec — Stream B：双端 TUI 主控台（`ssh-manager tui`）

> 日期：2026-08-14。状态：设计定稿（brainstorm 四问拍板：client 能力=本地配置+同步零远程写；client 形态=TUI 面板；界面=全屏主控台；broker v1 操作集=全集含 token 发放）。
> 前置调研：`docs/eval/go-tui-framework-research.md`（Charm 全家桶 v2 选型）。框架调研正本若未入库，随本 spec 一并提交。
> 范围：Stream B。与 Plan 17（缓存自动保鲜，已合并 `589f34c`）零耦合——client 面板的「立即同步」只是把 `cache pull` 按键化。

## 1. 目标与边界

给 owner 一个看得见的日常运维界面，替代记忆 CLI flag：

- **broker 模式**（本机有已解锁 vault）：k9s 式全屏主控台，覆盖**全集**操作——服务器增删改查、凭据录/换、profile 管理+授权、project + token 发放/吊销、cache-token（设备码）签发/吊销。
- **client 模式**（本机仅有离线缓存）：全屏面板——broker 连接配置（地址/端口/pin/设备码）、缓存状态与只读服务器清单、立即同步。

**边界（brainstorm 定案）**：client 端**不做任何远程写 vault**——「配置服务器」指配置*连哪个 broker*（ip/端口/pin），不是增删 vault 里的服务器。零新网络 API、零 serve 侧改动、威胁模型不变。

## 2. 入口与模式判定

`ssh-manager tui` 单命令：

```
启动 → 探测 vault（vault.OpenStore(FileKeyProvider)）
  ├─ 成功 → broker 模式（vault 在本机）
  └─ 失败且 cache.auth.json/cache.bin 存在 → client 模式
      两者皆无 → 报错引导（"本机无 vault 也无缓存：先在 broker 机初始化，或 cache pull"）
--broker / --client 强制覆盖判定
```

vault 探测失败但本机确有 vault（锁定）→ 报错提示 `unlock`，不进 client 模式（防误判）。

## 3. 交互形态与组件分工

**全屏主控台（bubbletea + bubbles + lipgloss），复杂表单推入 huh 嵌入页**（调研 §2/§5 结论：huh 可作为 tea.Model 嵌入，表单校验/掩码/翻页开箱即用）。

### 3.1 broker 主控台

```
┌─ ssh-manager ────────────────────────────────┐
│ [Tab] 切换: 服务器 | Profiles | Projects | 设备码 │
│──────────────┬────────────────────────────────│
│ ▶ 3090x2     │ 名称   3090x2                  │
│   4090x2     │ Host   192.168.200.120          │
│   NUC10      │ 用户   urit_ai  sudo:无         │
│   ml_hub …   │ 硬件   双RTX3090/62GB           │
│              │ 标签   gpu,training             │
│              │ Caveats sudo需密码…             │
│──────────────┴────────────────────────────────│
│ [a]新增 [e]编辑 [d]删除 [g]授权  Tab切页 q退出   │
└──────────────────────────────────────────────┘
```

- 四个实体页签（服务器/Profiles/Projects/设备码），Tab/Shift-Tab 循环。
- 每页各自的按键操作；多字段操作（新增/编辑服务器、新建 project、签发设备码）推入 huh 表单页，完成后刷新列表返回。
- **token/设备码发放页**：生成瞬间全屏显示一次性明文 + 指纹（对齐 CLI `printCacheToken` 语义），「已复制/确认后不可再见」提示，任意键返回。

### 3.2 client 面板

```
┌─ ssh-manager (client) ───────────────────────┐
│ Broker   https://192.168.100.235:7878  ✓可达  │
│ 指纹     sha256:c69b…(钉死)                   │
│ 缓存     9 服务器 · 2 分钟前 · TTL 30m        │
│──────────────┬────────────────────────────────│
│ ▶ ai_runner  │ Host 192.168.100.201           │
│   3090x2     │ 用户 urit_ai                   │
│   …（只读）   │ 角色 AI runner 实验机          │
│──────────────┴────────────────────────────────│
│ [s]立即同步 [c]编辑连接 [t]TTL  q退出           │
└──────────────────────────────────────────────┘
```

- `[s]` 立即同步 = 调既有 `doPull`（带 pin，成功后刷新面板数据）。
- `[c]` 编辑连接 = huh 表单（地址/端口/pin/设备码，设备码掩码），保存 = `writeCacheCred`（复用 Plan 17 的 0600+ACL 落盘）。
- `[t]` TTL 调整 = 提示 `.mcp.json` 里 `--cache-max-age` 的修改指引（TTL 属 MCP 启动参数，面板只读展示当前值来源）。
- 服务器清单读本地 cache（只读，与 `mcp --cache` 同一份数据源）。

## 4. 组件与数据流

| 单元 | 职责 | 依赖 |
|---|---|---|
| `internal/tui/app.go` | tea.Model 顶层：模式判定结果、页签路由、页面栈（console 页 ↔ huh 表单页/token 展示页） | bubbletea |
| `internal/tui/broker/` | 四个实体页（list+detail 渲染、按键→动作映射）；动作直接调 store 层既有方法（`AddServer`/`UpdateServer`/`DeleteServer`/`GrantServers`/`AddProject`/`SetProjectStatus`/`RotateProject`/`AddCacheToken`/`RevokeCacheToken`…） | store |
| `internal/tui/client/` | 连接状态加载（`readCacheCred`+`cache status` 逻辑）、只读清单（`loadCacheSnapshot`）、同步动作（`doPull`） | cli 层导出函数或薄封装 |
| `internal/tui/forms/` | huh 表单定义：新增/编辑服务器（含凭据掩码+互斥类型）、profile、project、设备码签发、client 连接编辑 | huh |
| `internal/cli`（mcp 旁） | `tui` 子命令（RunE 里判模式→tea.NewProgram().Run()）；cli 包既有 store 调用编排供 tui 复用的部分导出 | cobra |

**数据流**：所有写操作走 store 层既有 API（与 CLI 同一条路，含全部校验如 4096 字节字段上限）；TUI 层不新增任何持久化。client 模式只读 cache.auth.json/cache.bin + 触发 doPull。

**包边界注意**：`internal/cli` 的 `doPull`/`writeCacheCred`/`readCacheCred`/`loadCacheSnapshot` 目前包内私有。方案：把这四个函数迁移到新的 `internal/clientops` 包（cli 薄封装转发，`cache` 子命令与 tui 共用）——迁移是纯移动+改引用，不改行为（迁移任务带回归测试）。

## 5. 敏感面（安全设计）

- **凭据永不明文回显**：vault 只存密文，编辑页显示「已设置（输入新值以更换）」；输入用 huh `EchoMode(EchoPassword)` 掩码。
- **token/设备码**：只在签发瞬间显示一次（同 CLI）；TUI 内不持久化、不进剪贴板自动复制（用户手动选择）。
- **client 设备码编辑**：掩码输入；落盘复用 `writeCacheCred`（0600 + HardenACL）。
- **审计**：broker 模式的写操作与 CLI 完全同一条代码路径——CLI 有的行为（校验、既有审计点）TUI 原样继承，不多不少；TUI 不新增绕过点。
- serve/MCP/iron rule 零改动。

## 6. 错误处理

| 场景 | 行为 |
|---|---|
| vault 锁定 | 启动报错提示 `ssh-manager unlock`，不降级 client 模式 |
| store 写失败（UNIQUE 冲突等） | console 内错误横幅显示，列表不动 |
| client 同步失败（离线/401/pin 失配） | 面板错误横幅 + 缓存数据照常展示 |
| 表单校验失败 | huh 内建字段级标注 |
| 终端不支持（mintty） | 启动检测 `TERM`/stdin 非 TTY → 报错提示 winpty/Windows Terminal |

## 7. 测试

1. **纯函数单测**：模式判定、列表渲染数据组装、按键→动作映射。
2. **表单逻辑单测**：字段校验（必填/互斥凭据类型/端口范围）、提交值组装。
3. **store 动作集成**：TUI 动作函数直接调 store（已有测试覆盖 store 层；补「TUI 动作函数→store」一层薄集成）。
4. **bubbletea 交互测试**：tea.Program test harness（teatest 或手动 Update 注入按键消息）断言模型状态流转（选中/页签/表单提交）。
5. **真机验收**：NUC10 上 `tui` 完成「新增服务器→授权→签发设备码」全流程；笔记本 `tui --client` 完成「编辑连接→立即同步」。

## 8. 依赖与平台

- 新增依赖：`charm.land/bubbletea/v2`、`charm.land/bubbles/v2`、`charm.land/huh/v2`、`github.com/charmbracelet/lipgloss`（v2 生态对齐，调研 §6；MIT）。
- Windows Terminal/cmd 原生支持（ConPTY）；mintty 需 `winpty` 前缀——README/tui 启动报错均注明。
- 依赖安全：Charm 全家桶纯 Go 无 CGO，不破坏 `CGO_ENABLED=0` 交叉编译（release pipeline 需验证一次）。

## 9. 边界与不做

- 不做远程写 vault（client 零写 API，brainstorm Q1 定案）。
- 不做 TUI 内的 SSH 连通性测试/远程执行（那是 agent 的活）。
- 不做鼠标操作 v1（键盘优先，鼠标 v2 再议）。
- 不动 serve/MCP/broker 逻辑；不新增网络端点。
