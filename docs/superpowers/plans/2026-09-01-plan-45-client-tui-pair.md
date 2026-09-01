# Plan 45 实施计划:client TUI 配对向导(发起 + 重置再申请)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** client 端 TUI 支持完整 SAS 配对流程(发现→表单→SAS 屏→等待批准→结果)与两类再申请(同实例 force 重配 / 被拒后重新发起);协议与 serve 零改动。

**Architecture:** 把 `clientops.RunPair` 内部步骤提升为导出分步状态机 `PairSession`(CLI 与 TUI 共用一条管线,单一来源);TUI 新增 `pairwizard` 组件(表单→发现列表→SAS 门→等待→结果屏)挂在 client 页 `[c]` 键;重试=同 session 形态重新驱动。零新依赖,零 wire 变化。

**Tech Stack:** 既有栈(bubbletea v2 + huh;clientops http/crypto 全复用)。

**Spec:** 无独立 spec(grilling Q1-Q4 拍板 2026-09-01,四定案即本文 Global Constraints 的来源;协议细节以 Plan 42 批1 spec rev4 为权威)。

## Global Constraints

- **协议/serve 零改动**(Q4):pair 端点、wire shape、时限、限速全冻结;本 plan 只动 client 面(clientops client 侧 + tui + cli 驱动层 + docs)
- **CLI 行为零变化**(T1):`sshmgr pair` 的输出文案(frozen wordings:TOFU 拒绝/STUB 警告/instance enrolled 提示等)与退出语义逐字保留——既有 pair_test.go 端到端测试不改断言全绿是硬验收
- **force 时序保留**:URL/pin 校验先于 force 清理(TestRunPair_ForceBadURLKeepsCredentials 锁死)
- **`pairBeforePullTestHook` 保留**(先落盘后首拉的测试缝)
- **SAS 人闸语义**(Q3):TUI 的按键确认=真实人闸(看着 SAS 屏按键),**不是** AssumeSAS;AssumeSAS 通道保持 CLI-only,不进 TUI
- **私钥不出包**:PairSession 的 ECDH 私钥等敏感态一律未导出字段;导出 API 不泄露密钥材料
- 零新依赖;中文 conventional commits;每任务 `go build ./... && go test ./...` 全绿
- 版本:v0.13.1(纯加法,非 breaking)

---

### Task 1: `PairSession` 分步状态机 + CLI RunPair 迁移为驱动层

**Files:**
- Create: `internal/clientops/pairsession.go`、`internal/clientops/pairsession_test.go`
- Modify: `internal/clientops/pair.go`(RunPair 改为驱动层;内部步骤函数体**原样搬移**进 session 方法,零逻辑改写)、`internal/cli/pair.go`(仅当需要暴露 discovery 结果给 TUI 的辅助——本任务不动它)

**Interfaces:**
- Consumes: 既有 `PairOpts`、`Discover`/`Discovered`、`DoPull`、`pairBeforePullTestHook`
- Produces(T2/T3 依赖的精确签名):

```go
// internal/clientops/pairsession.go:
// 导出哨兵:TUI 据此分支"重新申请"屏
var ErrPairRejected = errors.New("pairing request rejected by broker (terminal for this request)")
var ErrPairTimeout  = errors.New("pairing approval window expired")

type PairSession struct{ /* opts + id/name/sas/keys/urls;敏感字段一律小写未导出 */ }
func NewPairSession(o PairOpts) (*PairSession, error) // 纯校验(URL/pin 合法性,含 TOFU 规则),无 I/O、无清理
func (s *PairSession) Bind(d Discovered)              // 采纳发现结果:URL=https://d.Addr:d.TCPPort,Pin=d.SPKI
func (s *PairSession) Enroll(ctx context.Context) error // POST /pair/enroll;置 ID/SAS/密钥态;ctx 可取消
func (s *PairSession) SAS() string                     // 6 位码(enroll 后有效)
func (s *PairSession) BrokerName() string
func (s *PairSession) WaitApproval(ctx context.Context) error // 2s 轮询 ≤10min(常量原样);rejected→ErrPairRejected;窗口尽→ErrPairTimeout;429=瞬态退避显示口径不变
func (s *PairSession) Finish(ctx context.Context) error       // ack 校验+解密封信封(事务语义原样)
func (s *PairSession) WriteAndPull(ctx context.Context) (PullResult, error) // 先落盘四件套(0600)→pairBeforePullTestHook→DoPull 首拉;force 清理不在此(见下)
func ForceCleanup(instance string) error // force 语义的清理(清 auth/bin/meta/quarantine 留 config)——独立函数,调用时序由驱动层保证(校验后、Enroll 前)
```

- RunPair 重排为:`NewPairSession`(校验)→ force?`ForceCleanup` → discovery/choose(既有逻辑)→ `Bind` → `Enroll` → 打印 SAS + y/N(或 AssumeSAS STUB 警告)→ `WaitApproval` → `Finish` → `WriteAndPull`;stdout 文案逐字不变。

- [ ] **Step 1: 失败测试**:session 级单测——NewPairSession 校验矩阵(URL 非法/pin 空+无 TOFU 拒绝/TOFU 放行);Bind 采纳字段;Enroll/WaitApproval/Finish/WriteAndPull 各步用既有 httptest 假 serve 驱动(参考 pair_test.go 的夹具),含 ctx 取消、ErrPairRejected/ErrPairTimeout 哨兵、429 退避不致错;ForceCleanup 清单(删 auth/bin/meta/quarantine、留 config)与 TestRunPair_ForceBadURLKeepsCredentials 语义等价
- [ ] **Step 2: 确认失败 → 实现(搬移不改写)→ `go test ./internal/clientops/ ./internal/cli/ -count=1` + 全仓绿(既有 pair_test.go 断言零改动)**
- [ ] **Step 3: 提交** `refactor(clientops): Plan 45 T1——RunPair 步骤提升为 PairSession 分步状态机(CLI 驱动层化,行为零变化;ErrPairRejected/ErrPairTimeout 导出)`

---

### Task 2: `pairwizard` TUI 组件(表单→发现→SAS 门→等待→结果)

**Files:**
- Create: `internal/tui/pairwizard.go`、`internal/tui/pairwizard_test.go`

**Interfaces:**
- Consumes: T1 的 `PairSession`/`Discover`/`Discovered`/哨兵错误;既有 overlay/altScreen/huh/titleStyle 惯例(instancepicker.go 为形态参照)
- Produces: `newPairWizard(prefill PairWizardPrefill) overlay` + 消息类型(供 T3 挂进 app):

```go
type PairWizardPrefill struct{ Instance, ProfileHint, URL, Pin string; Force bool }
// 状态机(内部):pwForm → pwDiscovering → pwPickBroker → pwEnrolling → pwSAS → pwWaiting → pwDone | pwFailed{reason: rejected|timeout|error}
// 关键交互:
//  - 表单:huh(实例名* / profile-hint;高级折叠组:url/pin/TOFU 说明);实例名过 instname.Valid 即时校验
//  - 发现:无 URL 时 Discover(带 spinner);结果 1 个自动进,多个列表选(Esc 回表单)
//  - SAS 屏:altScreen 大字 6 位码 + broker 名 + 10:00 倒计时 + 「与 broker 批准面核对后按 Enter 开始等待批准 / Esc 取消」——**Enter 前 WaitApproval 不启动**(真实人闸)
//  - 等待屏:已等待时长 + 最近轮询状态(pending/429 退避) + Esc 取消(ctx cancel)
//  - 结果屏:成功=实例名+pair.<name>.mcp.json 路径+后续指引(如 .mcp.json 需 --instance);被拒=终态说明+`r` 重新申请(新 session 同 opts);超时=同 `r`;其他错误=err+`r`/Esc
//  - force:prefill.Force 或表单判定实例已配对(auth.json 存在)→ Enroll 前插确认屏(列明删 auth/bin/meta/quarantine、留 config);Esc 任意步全身而退
```

- 阻塞步骤(Discover/Enroll/WaitApproval/Finish/WriteAndPull)在 tea.Cmd goroutine 里跑,经 ctx 取消;UI 更新只经消息。

- [ ] **Step 1: 失败测试**:状态转移表测(直接驱动 Update+注入消息):表单校验拒非法实例名;发现空→提示回表单;多 broker 选择;SAS 门——**未按 Enter 前 WaitApproval 未启动**(seam:session 注入点记录调用序);Enter 后进等待;Esc 在等待中取消 ctx;rejected→结果屏含"重新申请"且 `r` 触发新 session;timeout 同;成功屏含产物路径串;force 确认屏在 Enroll 前出现、Esc 中止零残留
- [ ] **Step 2: 确认失败 → 实现 → tui 包+全仓绿;gofmt 零输出**
- [ ] **Step 3: 提交** `feat(tui): Plan 45 T2——配对向导组件(表单/发现选择/SAS 人闸门/等待轮询/结果与重试屏)`

---

### Task 3: client 页接线(`[c]` 真入口 + 实例重配入口)

**Files:**
- Modify: `internal/tui/clientpage.go`(`[c]` 从提示行改为启动向导;clientpage.go:26-28 注释更新)、`internal/tui/app.go`(overlay 路由挂 pairwizard 消息;App-owned 消息类型进 Update gate 注册清单)、`internal/tui/instancepicker.go`(已配对实例行加 `p` 重新配对=预填 Force 的向导)
- Test: `internal/tui/clientpage_test.go`/`app_test.go` 增补

**Interfaces:**
- Consumes: T2 的 `newPairWizard`/`PairWizardPrefill`
- Produces: client 页键位——`[c]` 新配对;实例 picker 行上 `p` 重新配对(prefill Instance+Force);完成后 clientpage 刷新(refreshDataCmd,新实例出现在 picker)

- [ ] **Step 1: 失败测试**:`[c]` 启动向导(表单空);picker 行 `p` 启动预填 Instance+Force;向导成功退出后 clientModel 收到刷新;Esc 全链退回原页面状态
- [ ] **Step 2: 确认失败 → 实现 → 全仓绿**
- [ ] **Step 3: 提交** `feat(tui): Plan 45 T3——client 页 [c] 配对真入口+实例重配(p 键,预填 Force);完成后实例列表自动刷新`

---

### Task 4: docs + 发布 checklist + 真机验收清单

**Files:**
- Modify: `docs/tui-single-machine.md`、`docs/tui-multi-machine.md`、`docs/quickstart-multi-machine.md`、`docs/multi-machine.md`(配对节补 TUI 路径,CLI 保留)、`README.md`(若 TUI 能力清单提及)、`docs/deployment-modes.md`(client 入网节:CLI 一条龙 + TUI 向导两条并列)

- [ ] **Step 1: 文档更新**(TUI 路径步骤化:`[c]`→表单→SAS 屏→broker 侧批准→完成;重配=`p`;被拒/超时=`r`;明确 AssumeSAS 仍 CLI-only)
- [ ] **Step 2: 发布 checklist 注记**:v0.13.1 纯加法;资产名不变(`update --check` 常规验证)
- [ ] **Step 3: 真机验收清单附尾**(执行者标注):
  | # | 项 | 执行者 |
  |---|---|---|
  | GW1 | 笔记本 TUI `[c]` 全流程对 NUC10 真配对(SAS 屏肉眼比对=owner;批准在 NUC10 TUI/CLI) | owner(SAS 必人眼)+助手陪跑 |
  | GW2 | 已配对实例 `p` 重配(force 清理语义:config 留、其余清、旧码失效由 serve 侧 revoke 语义配合) | 助手(笔记本本机) |
  | GW3 | 被拒→`r` 重新申请走通(NUC10 侧先 reject 再 approve) | 助手(MCP)+owner |
  | GW4 | Esc 各步全身而退、`sshmgr pair` CLI 回归(同机双路径等价) | 助手 |
- [ ] **Step 4: 全仓绿 → 提交** `docs: Plan 45 T4——TUI 配对向导文档+发布注记+GW1-G4 真机清单`

---

## Self-Review(已执行)

1. **拍板覆盖**:Q1 全流程+双 retry(T2 重试屏/T3 force 入口)、Q2 分步 API(T1)、Q3 SAS 真人闸(T2 Enter 门+AssumeSAS 不进 TUI)、Q4 轻流程+4 任务+v0.13.1 ✓
2. **占位扫描**:关键交互/状态机/签名均已具象;文案细节(具体中文串)授权实现者按既有 TUI 风格,不属占位
3. **一致性**:PairSession 方法名在 T1/T2 一致;PairWizardPrefill 在 T2/T3 一致;哨兵错误名在 T1/T2 一致;clientpage `[c]` 语义变更在 T3 单点落
