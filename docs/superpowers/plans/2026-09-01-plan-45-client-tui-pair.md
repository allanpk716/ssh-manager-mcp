# Plan 45 实施计划:client TUI 配对向导(发起 + 重置再申请)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** client 端 TUI 支持完整 SAS 配对流程(发现→表单→SAS 屏→等待批准→确认→结果)与两类再申请(同实例 force 重配 / 结束后重新发起);协议与 serve 零改动。

**Architecture:** 把 `clientops.RunPair` 内部步骤提升为导出分步状态机 `PairSession`(CLI 与 TUI 共用一条管线,单一来源);TUI 新增 `pairwizard` 组件挂在 client 页 `[c]` 键;重试=同 opts 重新驱动。零新依赖,零 wire 变化。
**修订**:rev1(盲评 27 条合并裁决后:哨兵改 ErrPairGone/ErrPairTimeout、SAS 屏常显+Finish 前门、ForceCleanup 会话化、WaitApproval 事件回调、generation 防串扰、PullOpts.Context、clientModel gate)。

**Tech Stack:** 既有栈(bubbletea v2 + huh;clientops http/crypto 全复用)。

**Spec:** 无独立 spec(grilling Q1-Q4 拍板 2026-09-01;协议细节以 Plan 42 批1 spec rev4 为权威;本文件为唯一 plan,修订即定稿——轻流程无二次盲评,owner 已拍板 1 轮)。

## Global Constraints

- **协议/serve 零改动**(Q4):pair 端点、wire shape、时限、限速全冻结;只动 client 面(clientops client 侧 + tui + cli 驱动层 + docs)
- **CLI 行为零变化**(T1):`sshmgr pair` 交互次序 **enroll → 轮询 → SAS y/N 确认 → finish**(pair.go:314 头注 ④⑤ 冻结次序)与全部 frozen wordings 逐字保留——既有 pair_test.go 端到端断言零改动全绿是硬验收;TUI 的门位=**批准到达后、Finish 前**(SAS 屏自等待起常显——broker 批准者需要对照 client 屏的码)
- **AssumeSAS 永驻 CLI 驱动层**:env `SSHMGR_PAIR_ASSUME_SAS` 读取与判定不进 session/TUI;`PairWizardPrefill` 无 AssumeSAS 字段(编译期钉)
- **force 时序**:校验(New+Bind)先于清理,清理先于 Enroll;清理入口**会话化**(见 T1 签名),不导出自由函数
- **`pairBeforePullTestHook` 保留**;私钥/密钥材料不出 session(未导出字段,T1 有 reflect 钉)
- 410 的 wire 语义=合并(rejected/expired/delivered/unknown 不可分,协议冻结)——客户端哨兵只有 `ErrPairGone`(410)与 `ErrPairTimeout`(本地 deadline)
- 零新依赖;中文 conventional commits;每任务 `go build ./... && go test ./...` 全绿
- 版本:v0.13.1(纯加法,非 breaking)

---

### Task 1: `PairSession` 分步状态机 + CLI RunPair 迁移为驱动层

**Files:**
- Create: `internal/clientops/pairsession.go`、`internal/clientops/pairsession_test.go`
- Modify: `internal/clientops/pair.go`(RunPair 改驱动层;步骤**逻辑等价搬移**——仅加 ctx 管线:pairPost 改 NewRequestWithContext、poll sleep 改 select ctx.Done,改动点列明,其余零改写);`internal/clientops/clientops.go`(`PullOpts` 增 `Context context.Context` 字段——DoPull 内部把 req 建在它之上,nil=旧行为;不改 DoPull 签名)
- **不动**:`internal/cli/pair.go`(CLI 驱动仍调 RunPair;env 读取留在此)

**Interfaces(Produces,T2/T3 依赖):**

```go
// internal/clientops/pairsession.go:
var ErrPairGone    = errors.New("pairing request ended by broker (rejected/expired/410 — terminal for this request)") // 410 合并语义,协议冻结
var ErrPairTimeout = errors.New("local approval window expired")                                                    // 仅本地 deadline

// PollNote 是轮询进度事件(WaitApproval 的回调载荷)。
type PollNote struct{ Pending bool; Backoff bool; Detail string } // pending=仍等待;backoff=429 瞬态;Detail=人类可读短句

type PairSession struct{ /* opts/id/name/sas/keys/urls/deadline;敏感字段一律未导出 */ }
// 驱动序(两条路径同一状态机):
//   校验:NewPairSession(URL/pin+TOFU 规则;URL 为空=发现流,校验推迟到 Bind)→(发现/选择由驱动层完成)→ Bind
//   Bind 执行等价 URL/pin 校验并(重)建 pinningTransport(URL 为空路径必须如此)
//   force:Bind 成功后 s.ForceCleanup() 才可调(返回 validation capability 语义:未 Bind 调用报错);内部复用既有 forceCleanInstance(原函数保留,既有测试零改动)
//   已装判定:驱动层在 New 后调 IsEnrolled(instance) (bool, error)(os.Stat auth.json)→CLI 打印冻结文案"instance already enrolled; pass --force";TUI 表单侧即此判定
//   Enroll(ctx) → 置 ID/SAS/密钥态/绝对 approvalDeadline(=enroll 时刻+窗口) → WaitApproval(ctx, note func(PollNote))
//   → (驱动层人闸:CLI=轮询到 approved 后 y/N;TUI=Finish 前按键) → Finish(ctx) → WriteAndPull(ctx)
func NewPairSession(o PairOpts) (*PairSession, error)
func (s *PairSession) Bind(d Discovered) error   // 等价校验+transport;https://d.Addr:d.TCPPort+Pin=d.SPKI
func (s *PairSession) ForceCleanup() error       // 仅 Bind 后可调(内部=forceCleanInstance);错误含调用前提说明
func IsEnrolled(instance string) (bool, error)
func (s *PairSession) Enroll(ctx context.Context) error
func (s *PairSession) SAS() string
func (s *PairSession) BrokerName() string
func (s *PairSession) ApprovalDeadline() time.Time // TUI 倒计时/等待共用同一绝对锚,不复制常量
func (s *PairSession) WaitApproval(ctx context.Context, note func(PollNote)) error // 2s/≤deadline;410→ErrPairGone;deadline 尽→ErrPairTimeout;ctx 取消→context.Canceled(三者可区分);429→note backoff(noteTransient 30s 节流语义保留)
func (s *PairSession) Finish(ctx context.Context) error // ack 校验+解密封信封(事务语义原样)
func (s *PairSession) WriteAndPull(ctx context.Context) (PullResult, error) // 先落盘四件套(0600)→pairBeforePullTestHook→DoPull(PullOpts{Context: ctx});成功后 AuthorizedProfile()/ArtifactPath() 可读
func (s *PairSession) AuthorizedProfile() string
func (s *PairSession) ArtifactPath() string // pair.<name>.mcp.json 实际落点
```

- **测试缝**:poll 间隔/窗口上限转**包级变量**(`pairPollInterval`/`pairPollMax` 改 var,生产值不变)——timeout 用例可缩窗。
- RunPair 重排为上述驱动序(次序=现状 ④⑤ 冻结次序,stdout 文案逐字不变;stderr 轮询提示经 note 回调回流 CLI 的既有输出点)。
- CLI 独占:discovery/多 broker 选择、y/N、AssumeSAS(env 判定留 cli/pair.go)。

- [ ] **Step 1: 失败测试**:NewPairSession 校验矩阵(URL 非法/pin 空+无 TOFU 拒绝/URL 空=发现流不校验);Bind 等价校验+transport 重建;ForceCleanup 前置(未 Bind 调用→错;坏 URL/坏 pin→清理不触发,TestRunPair_ForceBadURLKeepsCredentials 语义等价);IsEnrolled;Enroll/WaitApproval(httptest 假 serve):410→ErrPairGone、缩窗→ErrPairTimeout、ctx 取消→context.Canceled 三态可区分、429 note 回调触发且节流;Finish/WriteAndPull(落盘次序钩子保留);**reflect 钉:PairSession 无导出密钥材料字段**(遍历导出字段,类型非 []byte/接口即可粗钉);既有 pair_test.go 断言零改动全绿
- [ ] **Step 2: 确认失败 → 实现 → clientops+cli 包绿+全仓绿**
- [ ] **Step 3: 提交** `refactor(clientops): Plan 45 T1——RunPair 步骤提升为 PairSession 状态机(CLI 驱动层化零行为变化;ErrPairGone/ErrPairTimeout/PollNote/ApprovalDeadline;PullOpts.Context)`

---

### Task 2: `pairwizard` TUI 组件(表单→发现→等待+SAS 屏→Finish 门→结果)

**Files:**
- Create: `internal/tui/pairwizard.go`、`internal/tui/pairwizard_test.go`

**Interfaces:**
- Consumes: T1 全部;既有 overlay/altScreen/huh 惯例(instancepicker.go 为形态参照)
- Produces:

```go
type PairWizardPrefill struct{ Instance, ProfileHint, URL, Pin string; Force bool } // 无 AssumeSAS(编译期钉)
// 状态机:pwForm → pwDiscovering → pwPickBroker → pwEnrollForceConfirm → pwEnrolling
//        → pwWaiting(SAS 大字常显 + ApprovalDeadline 倒计时 + 轮询 note 状态行) → pwFinishGate(批准已到:SAS 放大复显+Enter 完成/Esc 放弃) → pwWritePull → pwDone | pwEnded{gone|timeout|error}
// 消息纪律:所有异步消息带 generation(epoch) 字段——Esc/重试后旧 generation 的 done/note/tick 一律丢弃(参照 dataReadyMsg stale-drop,clientpage.go:143);tick 仅在对应状态续排
// 取消语义:Discover(短窗口不可取消,Esc 仅弃结果)与 Enroll/WaitApproval(ctx cancel)区分;pwWritePull 阶段 Esc 禁用(落盘+首拉不可半途弃,防与重试的 ForceCleanup 竞争写盘)——界面显示"写入中,请稍候"
// force 确认屏(pwEnrollForceConfirm):列明删 auth/bin/meta/quarantine、留 config;Esc 任意步全身而退(等待阶段=cancel ctx)
// 结果屏:成功=实例名+ArtifactPath+AuthorizedProfile+后续指引(.mcp.json 需 --instance);pwEnded(gone=「本次申请已结束(被拒或过期)」——410 合并语义措辞/timeout/error)均给 `r` 重新申请(新 generation 同 opts 重新驱动)
// 入口互斥:[c]/向导启动时检查 SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 单槽覆盖 env(SSHMGR_CACHE_DEK_DIR 是组合 seam 放行——复用/上提 cli common.go 的互斥判定为共享 helper)——命中即拒绝启动并提示(与 CLI --instance 互斥同语义)
```

- [ ] **Step 1: 失败测试**:表单校验拒非法实例名;发现空→回表单;多 broker 选择;force 确认屏在 Enroll 前、Esc 零残留;**SAS 屏在 pwWaiting 即常显**(不需要批准到达);pwFinishGate——批准到后方出现,**Enter 前 Finish 未被调用**(seam 记录调用序),Enter→WriteAndPull;Esc 在等待中 cancel ctx;generation 防串扰(注入旧 generation 的 done/tick 后 `r` 重试不被污染);pwWritePull 期间 Esc 无效;gone/timeout/error 三态结果屏+`r` 重试走通;单槽覆盖 env 命中→拒绝启动
- [ ] **Step 2: 确认失败 → 实现 → tui 包+全仓绿**
- [ ] **Step 3: 提交** `feat(tui): Plan 45 T2——配对向导(SAS 常显等待+Finish 前人闸+generation 防串扰+写入期不可取消+单槽互斥)`

---

### Task 3: client 页接线(`[c]` 真入口 + 实例重配入口)

**Files:**
- Modify: `internal/tui/clientpage.go`(`[c]` 改启动向导;clientpage.go:26-28 注释更新;**clientModel.Update 的 overlay gate**(clientpage.go:119-140)注册向导完成消息——**不动 app.go 的 App-owned 清单**(broker App 永不承载向导);完成路径优先复用白名单内 formDoneMsg 语义(clientpage.go:127)+ instancePickedMsg 同款换槽:完成消息携带新 instance→m.instance 切换→refreshDataCmdFor(新实例),默认槽 footer 的 `[c]` 在单槽覆盖 env 下隐藏)、`internal/tui/instancepicker.go`(picker 行补"已配对"标记=auth.json 存在位(pickerRowMeta 扩展);已配对行 `p` 重新配对=预填 Instance+Force;**默认实例行(instance="")禁 p**(实例名必填);底部提示 +`[p]`)
- Test: `internal/tui/clientpage_routing_test.go`(**先改写 85-95 行断言**:"指向 sshmgr pair"提示→启动向导新语义)、`clientpage_test.go`、`app_test.go`
- **app.go 仅当确需穿透消息时最小增补**(默认不需要)

**Interfaces:**
- Consumes: T2 的 `newPairWizard`/`PairWizardPrefill`
- Produces: `[c]`=新配对(空表单);picker 已配对行 `p`=重配(prefill Instance+Force);向导成功→换槽+刷新;Esc 全链退回原页

- [ ] **Step 1: 失败测试**:`[c]` 启动向导;picker 已配对行 `p` 预填 Force、默认行 `p` 无效;向导成功后 m.instance=新实例+refreshDataCmdFor;Esc 退回;routing 测试断言先行改写
- [ ] **Step 2: 确认失败 → 实现 → 全仓绿**
- [ ] **Step 3: 提交** `feat(tui): Plan 45 T3——client 页 [c] 配对真入口+picker 已配对行 p 重配;完成后换槽刷新;gate 落 clientModel`

---

### Task 4: docs + 发布 checklist + 真机验收清单

**Files:**
- Modify: `docs/tui-single-machine.md`、`docs/tui-multi-machine.md`、`docs/quickstart-multi-machine.md`、`docs/multi-machine.md`(配对节补 TUI 路径,CLI 保留)、`docs/deployment-modes.md`(client 入网:CLI 一条龙+TUI 向导两条并列)、`README.md`(若 TUI 能力清单提及)

- [ ] **Step 1: 文档**(`[c]`→表单→SAS 屏(等待期常显,与 broker 批准面对照)→批准后 Enter 完成;重配=p;结束后 r;AssumeSAS 仍 CLI-only;410 措辞"已结束(被拒或过期)")
- [ ] **Step 2: 发布 checklist 注记**:v0.13.1 纯加法;资产名不变;发版后 `sshmgr update --check` 常规验证
- [ ] **Step 3: 真机验收清单附尾**:
  | # | 项 | 执行者 |
  |---|---|---|
  | GW1 | 笔记本 TUI `[c]` 全流程对 NUC10 真配对(SAS 屏肉眼比对=owner;批准在 NUC10 TUI/CLI;批准后 Enter 门体验) | owner(SAS 必人眼)+助手陪跑 |
  | GW2 | 已配对实例 `p` 重配(force 清理语义+serve 侧旧码吊销) | 助手(本机) |
  | GW3 | 结束态→`r` 重新申请走通(NUC10 侧先 reject 再 approve) | 助手(MCP)+owner |
  | GW4 | Esc 各步全身而退;`sshmgr pair` CLI 回归(同机双路径等价) | 助手 |
- [ ] **Step 4: 全仓绿 → 提交** `docs: Plan 45 T4——TUI 配对向导文档+发布注记+GW1-G4 真机清单`

---

## Self-Review(已执行)

1. **盲评闭环**:kimi 15+codex 12 合并 27 条全落地——哨兵二分改并(410 冻结事实)/次序=CLI 保 ④⑤、TUI 门在 Finish 前+SAS 常显/进度回调 PollNote/绝对 deadline 锚/ForceCleanup 会话化+原函数保留/IsEnrolled 驱动层判定/PullOpts.Context+写入期禁 Esc/generation 防串扰/gate 落 clientModel 不动 app.go/routing 测试先行改写/picker 默认行禁 p+已配对标记/完成换槽刷新/只读 accessor/AssumeSAS 永驻驱动层+编译期钉/reflect 私钥钉/单槽互斥继承/倒计时不复制常量
2. **占位扫描**:签名/状态机/门位/消息纪律具象;中文文案细节授权实现者循既有风格
3. **一致性**:ErrPairGone/ErrPairTimeout/PollNote/ApprovalDeadline/ArtifactPath 在 T1↔T2 一致;PairWizardPrefill 无 AssumeSAS 在 T2 定义=T3 使用;gate 目标 clientModel 在 T2 纪律=T3 落点一致
