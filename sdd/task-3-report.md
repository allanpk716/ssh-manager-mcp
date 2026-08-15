# Task 3 Report — 单机向导流程（standalone wizard flow）

**Status: DONE** — commit `feat(tui): standalone wizard flow — vault/server-loop/profile-grant/token screen/mcp finish`

## 交付物

### 新建 `internal/tui/wizardsteps.go` — 三角色共用步骤函数

| 函数 | 说明 |
|---|---|
| `wizEnsureVault() error` | 幂等 vault 初始化。已存在且解锁 → nil 跳过（stat-first 探测，不触碰）；存在但锁定 → 错误引导 `unlock`（绝不静默覆盖 key）；不存在 → 镜像 cli `unlock` 的初始化：`GenerateMasterKey` → `FileKeyProvider{Path: SSHMGR_FILEKEY_PATH}.Set`（ACL 加固）→ `store.Open`（建 store.db+schema）→ `Close` |
| `wizServerLoopForm(d) *huh.Form` | 复用 Plan 18 加法模式 server 表单（提交时 submitServer 强制 密码/密钥二选一）。循环本体（提交 → AddServer → 「继续添加？」确认 → 重开表单）由向导状态机驱动 |
| `wizProfileGrantForm(profileName, servers, chosen) *huh.Form` | profile 名 + grant 多选（value=id，复用 grantOptions）。零服务器时多选省略，退化为纯名称表单 |
| `dedupeProfileName(st, name) string` | 冲突自动 `-2`/`-3` 后缀；ListProfiles 失败时透传（AddProject/AddProfile 在提交时暴露真错误） |
| `defaultHostName() string` | os.Hostname() 兜底 "my-machine"，profile/project 名默认值 |
| `wizTokenScreen(title, token, usage, recovery) overlay` | 一次性密钥屏（`wizSecretView`，secretView 的兄弟类型——footer 可定制、dismiss 后由**持有它的向导**决定下一步）。body = token(secretStyle) + 用途行 + 「⚠ 仅此一次。丢失 → 」+ recovery 行 |
| `mcpConfigScreen(tokenRef) overlay` | .mcp.json 收尾屏（`wizStaticView`）。完整 JSON 片段（真实形状，来自 docs/agent-access.md）+ 三条说明：单机用普通 `mcp`（**不是** client 的 `--cache`）、Windows 绝对路径写法、别提交 git |
| `wizFinish(r) tea.Cmd` | `roles.Save(State{Role: r, SetupComplete: true})` 成功 → `wizardDoneMsg{next:"broker"}`；Save 失败 → `errMsg`（停留在收尾屏，任意键重试） |

### 改写 `internal/tui/wizard.go` — standalone 状态机

新步骤枚举：`stepVaultErr / stepServerAsk / stepServerForm / stepServerConfirm / stepProfileGrant / stepProject / stepToken / stepMcpConfig`。`stepRoleDone` 保留为 server(T4)/client(T5) 占位。

流程串联（chooseRole(standalone) 或 resume 后 `startRoleFlow()` 进入）：

1. **wizEnsureVault** — 失败 → `stepVaultErr` 横幅 + `r 重试 / q 退出`（role.json 已写，退出=安全暂停）。成功后 `vault.OpenStore` 保持打开（供流程内变更与后续 broker 交接复用）。
2. **服务器循环** — 「现在录入第一台服务器？」确认（skip 允许，显示「跳过 = profile 暂无成员，agent 将看不到任何服务器；之后可在主控台随时补录」）→ serverDraft 表单 → `submitServer`（doAction→AddServer）→ actionDone → 「继续添加下一台服务器？」确认循环。
3. **profile+grant** — 名默认 hostname；提交动作一次性 `dedupeProfileName` → `AddProfile` → `GrantServers`（未选=允许，多选标题带「未选=agent 暂时看不到任何服务器」）。
4. **project** — 单字段名默认 hostname，绑定步骤 3 的 profileID（此时只有一个 profile，无需 select，区别于主控台的 newProjectForm）→ `AddProject` → token 走 `tokenIssuedMsg` 直达 overlay（与 App 同纪律：明文只过一条消息）。
5. **wizTokenScreen** — usage「贴到本机 .mcp.json 的 --token 参数」/ recovery「主控台 Projects 页 [a] 重发」。
6. **mcpConfigScreen** → 任意键 → `wizFinish` → `wizardDoneMsg` → `tea.Quit`。

关键工程决策：
- **`wizardData` 改为堆分配一次的指针字段**（`data *wizardData`）——与 T2 的 `wizAnswers` 同理：模型按值穿越 Update，huh 的 Value 指针绑定必须指向唯一稳定分配，否则第一次拷贝后失效。提交闭包通过该指针写回 profileName/profileID，跨模型拷贝可见。
- **错误恢复**：任一变更失败（errMsg）→ 重开**同一表单绑定同一状态**（用户改完重交，不丢已填内容）；vault 错误 → r 重试。
- **交接 sentinel（本任务被授权选最简可行模式）**：`wizardDoneMsg{next}` → wizard 置 `done/next` 后 `tea.Quit`；`Run`（mode.go）从 `p.Run()` 的**最终模型**读 `done && next=="broker"`，用向导**已打开的 store** 直接 `NewBrokerApp` 起第二个 program，结束后统一 Close。无需跨 program 消息。

### 改 `internal/tui/mode.go` — Run 向导交接

wizard 分支结束后检查最终模型：done+broker → 链入主控台；否则按原样退出（q/Esc = 安全暂停，下次 `tui` resume）。

### 改 `internal/roles/roles.go:316` — T2 review 遗留

错误文案 `invalid --force` → `invalid --mode`（该入口实为 `tui --mode` 的值，与 DetectModeWith 的文案对齐）。

## 测试（TDD：先红后绿）

新建 `internal/tui/wizardsteps_test.go`：

- `TestWizEnsureVault_Idempotent` — 首跑建 store.db + master.key.plain；二跑 mtime 不变（幂等跳过）。
- `TestWizProfileName_SuffixOnConflict` — "nuc10" 冲突 → "nuc10-2"；无冲突透传。
- `TestWizTokenScreen_Copy` — 三要素文案（token/用途/仅此一次/丢失/重发）。
- `TestMcpConfigScreen_Copy`（补充）— 真实 .mcp.json 形状 + plain `mcp` args。
- `TestWizFinish_SavesSetupComplete`（补充）— role.json `setup_complete:true` + `wizardDoneMsg{broker}`。

helpers：`statModTime`/`openVault`/`viewString` 落在 wizardsteps_test.go；`withRoleDirs`/`seedWizardVault` 复用 wizard_test.go。

验证：`go build ./... && go vet ./...` 干净；`go test ./internal/tui/ ./internal/roles/ -count=1` 全绿；**全仓 `go test ./...` 14 包全绿**；gofmt 干净。

## 对 brief 的三处有据偏差

1. **`wizTokenScreen` 增加 `token` 参数**（brief 签名 3 参，但 body 必须渲染 token 明文——不传无法渲染）。测试相应加 token 实参与断言。
2. **brief 测试里 `wizEnsureVault(t)` 带参是笔误**——实现按接口 `wizEnsureVault() error`，测试去掉 `t`。
3. **profile 名冲突不在 Validate 里改写**（brief 措辞「Validate 冲突时自动 -2 后缀再放行」）：huh Input 每次击键都跑 Validate，在 Validate 里改写绑定值会把 "nuc10" 边打字边劫持成 "nuc10-2"。改为**提交时** `dedupeProfileName`（与 parent 的 binding resolution「dedupeProfileName suffix -2」一致）。`dedupeProfileName` 单测照 brief 原样通过。

## 未尽事项 / 交接 T4、T5

- server 流程（T4）与 client 流程（T5）仍停在 `stepRoleDone` 占位页——按计划。
- T4 可直接复用：`wizEnsureVault`、`wizServerLoopForm`、`wizProfileGrantForm`、`dedupeProfileName`、`wizTokenScreen`（换 usage/recovery 文案）、`mcpConfigScreen`、`wizFinish`、`wizardDoneMsg` 交接机制（next 换值即可）。
- 现场细节：`stepServerConfirm` 确认默认 No（Enter=完成，单服务器场景少敲一次）；token 屏/收尾屏上任意键推进（含 q——文案已写明「按任意键继续」）。

## Fix round: panics + resume idempotency

Review C1/C2/I1/M1/M2 全部修复（I1 为行为修复，M1/M2 为可见性/注释）。

### C1 — `wm.st.Close()` nil-deref（mode.go）

早退路径（首屏 q、T4/T5 占位页、stepVaultErr）下 `st == nil`，Run 的无条件 `wm.st.Close()` 必 panic。抽出演示 cleanup 到 `(*wizardModel).closeStore()`（`if wm.st != nil { wm.st.Close() }`），Run 的两处调用点（handoff 失败分支 + 程序退出统一收尾）都走它。测试直接调用同一 helper（nil st + stepVaultErr 形态）断言不 panic。

### C2 — `stepFormDone` 对 nil form 调 Init（wizard.go）

fresh `chooseRole(standalone)` → `enterStandalone` 失败 → step=stepVaultErr、form=nil，stepPick 分支的 `w.step == stepRoleDone` 检查不放行，落到 `w.form.Init()` panic。条件改为 `w.step == stepRoleDone || w.form == nil`。测试：锁死 vault（删 master.key.plain）后走 stepFormDone 完整路径断言 stepVaultErr 不 panic，再对同形态模型喂 `formDoneMsg` 断言 no-op。

### I1 — resume 幂等（wizard.go `enterStandalone`）

中途退出后重跑会再跑 askFirstServer → 铸出第二个 profile（hostname-2）+ 第二个 project。启发式（已写入注释）：store 打开后数既有实体——

- **≥1 profile 且 ≥1 project** → 三步全视为完成，直接进 mcpConfig 收尾屏；tokenRef 写「既有 project 的 token（丢失可在主控台 Projects 页 [a] 重发）」而非假装在屏上（一次性 token 上轮已展示、库里只有 hash，不可恢复）。
- **≥1 profile、0 project** → 跳过服务器循环与 profile 创建，复用既有 profile（name/id 载入 `w.data`），直接从 stepProject 续跑 project+token+finish。
- **0 profile** → 全新流程照旧 askFirstServer。服务器录入只要有 profile 就视为已完成（零授权 profile 是合法早退结局，补录随时可在主控台做）。

测试两条：① seed profile+project → resume → 断言直达 mcpConfig，走完 finish 后 vault 里 profile/project 计数仍各为 1（不变）；② seed 仅 profile → resume → 断言落在 stepProject、`data.profileID == 既有 id`，submitProject 后 project 恰好 1 个且 `profile_id` 绑既有 profile、profile 仍 1 个。

### M1 — errMsg 在 overlay 下不可见（wizard.go View）

`w.ov != nil` 分支现在把 `w.err` 追加渲染在 overlay 之下（wizFinish Save 失败不再静默，附「任意键重试」提示）。

### M2 — overlay 吞 Esc 有意为之（wizard.go Update）

在 overlay-first 路由处补注释：一次性密钥屏（token/.mcp.json）吞 q/Esc 是有意的——数据（profile/project/token hash）已落 vault，没有可「暂停回去」的未持久状态；任意键推进。

### 验证

`go build ./... && go vet ./...` 干净；全仓 `go test ./... -count=1` 14 包全绿；`gofmt -l` 无输出。
