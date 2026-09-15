package tui

// pairwizard.go — Plan 45 T2:client 端 SAS 配对向导(独立 overlay 组件,T3 接
// 线进 client 页 [c] 键)。驱动 T1 的 PairSession 分步状态机(pairsession.go,
// 签名逐字消费),本组件只做 TUI 面:表单 → 发现/选择 → force 确认 → enroll →
// SAS 常显等待 → Finish 门 → 写入 → 结果。
//
// 冻结裁决(plan):
//   - SAS 屏在 pwWaiting 即常显(批准者需要对照 client 屏的码),不是批准后才
//     显示;Finish 门(pwFinishGate)在批准到达后出现,Enter 前 Finish 绝不被
//     调用(测试用 seam 调用序钉死)。
//   - generation/epoch 防串扰:所有异步消息带 gen;Esc/重试后旧 generation 的
//     done/note/tick 一律丢弃(参照 clientpage.go dataReadyMsg 的 stale-drop);
//     tick 仅在 pwWaiting 续排。
//   - 取消语义分层:Discover 短窗口不可取消(Esc 仅弃结果);Enroll/WaitApproval
//     经 ctx cancel;pwWritePull 阶段 Esc 禁用(落盘+首拉是不可中断的收尾事务,
//     半途弃会留下指向不明的半态),界面显示「写入中,请稍候」。
//   - force 时序(Plan 46 零清理先行):校验(New/Bind)先于确认屏,确认屏先于
//     Enroll;Enroll 前不删任何旧槽文件——旧材料直到 WriteAndPull 全部成功才被
//     新凭据原子覆盖;确认屏(pwEnrollForceConfirm)在任何会话方法之前,Esc 零残留。
//   - 单槽互斥:构造时检查 SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK(共享 helper =
//     clientops.SingleSlotOverrideEnvSet,Plan 40 批2 已从 cli/common.go 的
//     checkInstanceFlag 判定上提;SSHMGR_CACHE_DEK_DIR 是目录级组合 seam,权威
//     判定明确放行)——命中即拒绝启动。
//   - AssumeSAS 永驻 CLI 驱动层:PairWizardPrefill 无该字段(测试 reflect 钉),
//     本组件不读 SSHMGR_PAIR_ASSUME_SAS。
//
// 测试缝:session 步骤经组件持有的函数变量(newSession/discover/isEnrolled/
// slotComplete),生产默认真实现;测试注入 fake(零网络),tick 用消息注入,
// 不真 sleep。

import (
	"context"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/mcpserver"
)

// PairWizardPrefill 是向导的入参预填(T3:picker 已配对行 `p` = Instance+Force)。
// 无 AssumeSAS 字段——env 判定与跳过语义永驻 CLI 驱动层(编译期钉,测试 reflect)。
type PairWizardPrefill struct {
	Instance    string
	ProfileHint string
	URL         string
	Pin         string
	Force       bool
}

// pairSessionSteps 是向导驱动的 PairSession 切面——方法集与 T1 签名逐字一致,
// *clientops.PairSession 天然满足(生产 newSession 即返回它)。测试缝:fake
// 实现同一接口记录调用序(Finish 门/零清理先行的证据面)。
type pairSessionSteps interface {
	Bind(clientops.Discovered) error
	Enroll(ctx context.Context) error
	SAS() string
	BrokerName() string
	ApprovalDeadline() time.Time
	WaitApproval(ctx context.Context, note func(clientops.PollNote)) error
	Finish(ctx context.Context) error
	WriteAndPull(ctx context.Context) (clientops.PullResult, error)
	AuthorizedProfile() string
	ArtifactPath() string
}

// pwState 是向导状态机(plan 冻结的推进序;pwClosed 是收尾后的防御态)。
type pwState int

const (
	pwForm               pwState = iota // huh 表单(实例名/地址/pin/hint)
	pwDiscovering                       // LAN 发现(短窗口,不可取消)
	pwPickBroker                        // 多 broker 选择
	pwEnrollForceConfirm                // force 重配确认(先于任何会话方法;Plan 46 零清理先行)
	pwEnrolling                         // enroll(ctx 可取消;force 不预清理)
	pwWaiting                           // SAS 大字常显 + 倒计时 + 轮询状态行
	pwFinishGate                        // 批准已到:SAS 放大复核;Enter 前 Finish 不被调
	pwWritePull                         // Finish + WriteAndPull(Esc 禁用)
	pwDone                              // 成功结果屏
	pwEnded                             // 终局结果屏(gone/timeout/error,r 可重试)
	pwClosed                            // 已交还父模型(T3 关 overlay);防御态
)

// pwEndReason 是 pwEnded 的三态(gone=410 合并语义措辞;timeout=本地窗口;error=其余)。
type pwEndReason int

const (
	pwEndGone pwEndReason = iota
	pwEndTimeout
	pwEndError
)

// 异步消息(全部带 gen——旧 generation 一律丢弃)。完成/关闭消息交父模型(T3
// 在 clientModel 的 gate 注册;本组件自身不消费它们)。
type (
	// pairDiscoverDoneMsg:发现窗口结束。
	pairDiscoverDoneMsg struct {
		gen   int
		found []clientops.Discovered
		err   error
	}
	// pairEnrollDoneMsg:enroll 完成(Plan 46 零清理先行:向导不再于 Enroll 前
	// 调用 ForceCleanup)。
	pairEnrollDoneMsg struct {
		gen int
		err error
	}
	// pairApprovalDoneMsg:WaitApproval 返回(nil=批准到达)。
	pairApprovalDoneMsg struct {
		gen int
		err error
	}
	// pairWriteDoneMsg:Finish+WriteAndPull 返回。
	pairWriteDoneMsg struct {
		gen int
		err error
	}
	// wizardTickMsg:等待屏心跳(1s);note 非空 = 顺带收割到的轮询事件。
	wizardTickMsg struct {
		gen  int
		note *clientops.PollNote
	}
	// pairWizardDoneMsg:成功收尾(T3:携带新实例换槽+刷新)。
	pairWizardDoneMsg struct{ instance string }
	// pairWizardClosedMsg:中止/退出收尾。
	pairWizardClosedMsg struct{}
)

// pwDraft 是 huh 表单的绑定态。
type pwDraft struct{ Instance, URL, Pin, ProfileHint string }

// pairWizard 是配对向导 overlay(overlay 接口:Init/Update/View/Title)。
// 组件实例自身持有 seam(非包级变量)——测试之间零全局串扰。
type pairWizard struct {
	prefill PairWizardPrefill
	state   pwState
	gen     int // generation/epoch:每次 Esc-中止/重试/收尾自增,旧消息一律丢弃

	form  *huh.Form
	draft *pwDraft

	opts      clientops.PairOpts // 提交时材料化(URL 直连即填;发现流在选中时填)
	sess      pairSessionSteps   // 当前会话(retry 换新:每次 enroll 新 id)
	offers    []clientops.Discovered
	cursor    int
	err       error
	note      string // 最新轮询状态行(pwNoteText 渲染)
	noteCh    chan clientops.PollNote
	cancel    context.CancelFunc // 当前 ctx 驱动阶段的取消(写入期不暴露给 Esc)
	endReason pwEndReason
	endErr    error

	// Plan 46 T3:force 确认屏入态时的本地四要素判定(419 advisory 分档)。
	// 在确认屏入态时现算而非走 prefill——用户可能在表单改过实例名,提示必须
	// 描述即将被重配的那个实例。
	forceComplete bool

	// 测试缝(组件持有,非包级变量):生产默认真实现,测试注入 fake。
	newSession   func(clientops.PairOpts) (pairSessionSteps, error)
	discover     func(targets []string, window time.Duration) ([]clientops.Discovered, error)
	isEnrolled   func(instance string) (bool, error)
	slotComplete func(instance string) bool // Plan 46 T3:四要素完整性判定(force 确认屏分档)
}

// newPairWizard 构造向导。单槽覆盖 env 命中即拒绝启动(与 CLI --instance 互斥
// 同语义;权威判定 = cli/common.go 的注释,共享 helper 已在 clientops)。
func newPairWizard(p PairWizardPrefill) (*pairWizard, error) {
	if clientops.SingleSlotOverrideEnvSet() {
		return nil, errors.New("配对向导与单槽覆盖 env（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK）互斥——覆盖会静默改写实例路径/DEK 归属；请 unset 后重试，或使用 sshmgr pair --instance")
	}
	d := &pwDraft{Instance: p.Instance, URL: p.URL, Pin: p.Pin, ProfileHint: p.ProfileHint}
	w := &pairWizard{
		prefill: p,
		state:   pwForm,
		draft:   d,
		opts: clientops.PairOpts{
			Instance: p.Instance, ProfileHint: p.ProfileHint, URL: p.URL, Pin: p.Pin,
			Stdout: io.Discard, Stderr: io.Discard,
		},
		// 生产默认真实现(签名逐字 = T1);测试替换组件字段。
		newSession:   func(o clientops.PairOpts) (pairSessionSteps, error) { return clientops.NewPairSession(o) },
		discover:     clientops.Discover,
		isEnrolled:   clientops.IsEnrolled,
		slotComplete: slotArtifactsComplete,
	}
	w.form = newPWForm(d)
	return w, nil
}

// newPWForm 建表单:实例名必填(instname 白名单)、地址留空 = LAN 发现、pin 留空
// 合法(直连时由会话校验给冻结 TOFU 文案——TUI 不提供 TOFU 开关)。
func newPWForm(d *pwDraft) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("实例名（设备名，如 laptop）").Value(&d.Instance).Validate(pwValidateInstance),
		huh.NewInput().Title("broker 地址（留空 = LAN 自动发现）").
			Placeholder("https://192.0.2.5:7878").Value(&d.URL).Validate(pwValidateURL),
		huh.NewInput().Title("pin（发现流自动携带；直连时必填）").
			Placeholder("sha256:…").Value(&d.Pin).Validate(pwValidatePin),
		huh.NewInput().Title("profile hint（可选，显示在 broker 批准面）").Value(&d.ProfileHint),
	))
}

// pwValidateInstance/pwValidateURL/pwValidatePin 是表单逐字段校验(与 CLI 前置
// 同语义:instname 白名单 / https+host / ParsePin);submitForm 里再全量过一遍
// (belt-and-braces)。
func pwValidateInstance(s string) error { return instname.Valid(strings.TrimSpace(s)) }

func pwValidateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil // 空 = 发现流
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return fmt.Errorf("地址无法解析: %v", err)
	}
	if u.Scheme != "https" {
		return errors.New("必须是 https:// 地址（留空 = LAN 自动发现）")
	}
	if u.Hostname() == "" {
		return errors.New("地址缺少 host")
	}
	return nil
}

func pwValidatePin(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil // 空 pin 合法:发现流升格 SPKI;直连由会话校验拒 TOFU
	}
	if _, ok := mcpserver.ParsePin(s); !ok {
		return fmt.Errorf("pin 不是合法的 sha256:<64hex> 指纹: %q", s)
	}
	return nil
}

// ---------------------------------------------------------------------------
// tea.Model
// ---------------------------------------------------------------------------

func (w *pairWizard) Title() string { return "配对向导" }

func (w *pairWizard) Init() tea.Cmd {
	if w.state == pwForm && w.form != nil {
		return w.form.Init()
	}
	return nil
}

func (w *pairWizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pairDiscoverDoneMsg:
		if w.gen != msg.gen || w.state != pwDiscovering {
			return w, nil // 旧 generation(Esc 弃结果)一律丢弃
		}
		if msg.err != nil {
			w.state, w.err = pwForm, msg.err
			return w, w.pwFormReset()
		}
		if len(msg.found) == 0 {
			w.state, w.err = pwForm, errors.New("未发现 broker——确认 broker 已运行且 discovery 开启，或在表单填写直连地址")
			return w, w.pwFormReset()
		}
		w.offers, w.cursor = msg.found, 0
		if len(msg.found) == 1 {
			// 单 offer 视作已选中:建连失败时落回选择屏(一行 + 错误),不留死态。
			w.state = pwPickBroker
			return w.pickBroker()
		}
		w.state = pwPickBroker
		return w, nil

	case pairEnrollDoneMsg:
		if w.gen != msg.gen || w.state != pwEnrolling {
			return w, nil
		}
		if msg.err != nil {
			w.state, w.endReason, w.endErr = pwEnded, pwEndError, msg.err
			return w, nil
		}
		w.note = ""
		w.noteCh = make(chan clientops.PollNote, 8) // 每个等待 generation 独立,旧 note 零泄漏
		w.state = pwWaiting
		return w, tea.Batch(w.pwWaitCmd(), pwTickCmd(w.gen, w.noteCh))

	case pairApprovalDoneMsg:
		if w.gen != msg.gen || w.state != pwWaiting {
			return w, nil
		}
		return w.approvalOutcome(msg.err)

	case pairWriteDoneMsg:
		if w.gen != msg.gen || w.state != pwWritePull {
			return w, nil
		}
		if msg.err != nil {
			w.state, w.endReason, w.endErr = pwEnded, pwEndError, msg.err
			return w, nil
		}
		w.state = pwDone
		return w, nil

	case wizardTickMsg:
		if w.state != pwWaiting || w.gen != msg.gen {
			return w, nil // tick 仅在对应状态续排
		}
		if msg.note != nil {
			w.note = pwNoteText(*msg.note)
		}
		return w, pwTickCmd(w.gen, w.noteCh)

	case tea.KeyPressMsg:
		if w.state == pwForm && w.form != nil {
			if msg.Code == tea.KeyEsc {
				return w.close()
			}
			return w.formUpdate(msg)
		}
		return w.keyUpdate(msg)
	}
	// 非按键消息:huh 的内部推进(nextField/nextGroup 等)只在表单态转发。
	if w.state == pwForm && w.form != nil {
		return w.formUpdate(msg)
	}
	return w, nil
}

// formUpdate 转发一条消息给 huh 表单并在完成/中止时收口(形态同 formOverlay:
// Esc 在外层截获,huh StateAborted 只会来自 ctrl+c)。
func (w *pairWizard) formUpdate(msg tea.Msg) (tea.Model, tea.Cmd) {
	f, cmd := w.form.Update(msg)
	if nf, ok := f.(*huh.Form); ok {
		w.form = nf
	}
	if w.form.State == huh.StateAborted {
		return w.close()
	}
	if w.form.State == huh.StateCompleted && w.state == pwForm {
		return w, w.submitForm()
	}
	return w, cmd
}

// keyUpdate 是手绘屏的键位(pwForm 之外的每个状态)。
func (w *pairWizard) keyUpdate(kp tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := kp.Key()
	switch w.state {
	case pwClosed:
		// 已收尾:一切消息防御性忽略(迟到旧消息不可能复活向导)。

	case pwDiscovering:
		// 发现窗口很短、不可取消:Esc 仅弃结果(gen 自增使迟到结果过期)。
		if k.Code == tea.KeyEsc || k.Text == "q" {
			return w.close()
		}

	case pwPickBroker:
		switch {
		case k.Code == tea.KeyUp, k.Text == "k":
			if w.cursor > 0 {
				w.cursor--
			}
		case k.Code == tea.KeyDown, k.Text == "j":
			if w.cursor < len(w.offers)-1 {
				w.cursor++
			}
		case k.Code == tea.KeyEnter, k.Code == tea.KeySpace:
			return w.pickBroker()
		case k.Code == tea.KeyEsc, k.Text == "q":
			return w.close()
		}

	case pwEnrollForceConfirm:
		switch {
		case k.Code == tea.KeyEnter:
			return w, w.beginEnroll() // 授权生效:进入 enroll(零清理先行——Plan 46)
		case k.Code == tea.KeyEsc, k.Text == "q":
			return w.close() // 零残留:任何会话方法都没跑过
		}

	case pwEnrolling, pwWaiting:
		if k.Code == tea.KeyEsc || k.Text == "q" {
			return w.abortCancel() // 等待阶段全身而退 = cancel ctx
		}

	case pwFinishGate:
		switch {
		case k.Code == tea.KeyEnter:
			return w, w.beginWrite() // 人闸:Enter 前 Finish 不存在
		case k.Code == tea.KeyEsc, k.Text == "q":
			return w.abortCancel() // 放弃(broker 侧请求自行过期,协议无撤销端点)
		}

	case pwWritePull:
		// 写入期 Esc 禁用:落盘+首拉是不可中断的收尾事务,半途弃会留下指向
		// 不明的半态(见文件头「取消语义分层」)。

	case pwDone:
		if k.Code == tea.KeyEnter || k.Code == tea.KeyEsc || k.Text == "q" {
			return w.finishSuccess()
		}

	case pwEnded:
		switch {
		case k.Text == "r":
			return w, w.retry() // 相同参数、新 generation 重新驱动
		case k.Code == tea.KeyEsc, k.Text == "q":
			return w.close()
		}
	}
	return w, nil
}

// ---------------------------------------------------------------------------
// 推进
// ---------------------------------------------------------------------------

// pwFormReset 重建表单并返回其 Init 命令(draft 指针共享,已填值保留)。提交
// 失败留在 pwForm 的路径必须调用:huh 的 Form.Update 在 State != StateNormal
// 时直接短路——不复位则表单永远停在 StateCompleted,用户无法再输入,且 pwForm
// 下任意非 Esc 按键都会命中 Completed 分支重跑 submitForm(发现落空屏每键重发
// LAN sweep)。
func (w *pairWizard) pwFormReset() tea.Cmd {
	w.form = newPWForm(w.draft)
	return w.form.Init()
}

// submitForm 在 huh 表单完成后运行:全量校验 → 已装判定(force 闸门)→ 材料
// 化 opts → 直连建会话 / 进入发现。拒绝时留在表单(重建复位)并给出 w.err。
func (w *pairWizard) submitForm() tea.Cmd {
	w.err = nil
	inst := strings.TrimSpace(w.draft.Instance)
	if err := pwValidateInstance(inst); err != nil {
		w.err = err
		return w.pwFormReset()
	}
	rawURL := strings.TrimSpace(w.draft.URL)
	if err := pwValidateURL(rawURL); err != nil {
		w.err = err
		return w.pwFormReset()
	}
	rawPin := strings.TrimSpace(w.draft.Pin)
	if err := pwValidatePin(rawPin); err != nil {
		w.err = err
		return w.pwFormReset()
	}
	enrolled, ierr := w.isEnrolled(inst)
	if ierr != nil {
		w.err = ierr
		return w.pwFormReset()
	}
	if enrolled && !w.prefill.Force {
		// CLI 冻结语义("instance already enrolled; pass --force")的 TUI 等价:
		// 重配入口在实例选择器(p 预填 Force),表单不静默覆盖在用凭据。
		w.err = fmt.Errorf("实例 %s 已配对——重配请回实例选择器按 p(force 重配)进入", inst)
		return w.pwFormReset()
	}
	w.opts = clientops.PairOpts{
		Instance: inst, ProfileHint: strings.TrimSpace(w.draft.ProfileHint),
		URL: rawURL, Pin: rawPin,
		Stdout: io.Discard, Stderr: io.Discard,
	}
	if rawURL != "" {
		return w.beginTarget(nil) // 直连:New 即完成等价校验(先于任何清理)
	}
	// 发现流:短窗口不可取消(Esc 在 pwDiscovering 仅弃结果)。
	w.state = pwDiscovering
	gen := w.gen
	targets, terr := clientops.NonLoopbackIPv4Broadcasts()
	return func() tea.Msg {
		if terr != nil {
			return pairDiscoverDoneMsg{gen: gen, err: terr}
		}
		found, derr := w.discover(targets, 0)
		return pairDiscoverDoneMsg{gen: gen, found: found, err: derr}
	}
}

// pickBroker 把光标 offer 材料化进 opts(URL 拼装 + SPKI 升格,与 CLI
// pickDiscovered 同规则)并建会话。发现流与直连两条路径在 beginTarget 汇合。
func (w *pairWizard) pickBroker() (tea.Model, tea.Cmd) {
	if w.cursor < 0 || w.cursor >= len(w.offers) {
		return w, nil
	}
	d := w.offers[w.cursor]
	w.opts.URL = fmt.Sprintf("https://%s:%d", d.Addr, d.TCPPort)
	if w.opts.Pin == "" {
		w.opts.Pin = d.SPKI // discovery 的 SPKI 升格为 pin(TLS 硬校验)
	}
	return w, w.beginTarget(&d)
}

// beginTarget 建立已校验会话并分流。发现流传非 nil d:New 之外再 Bind(d)——
// 幂等重校验,并把 offer 显示名记入 brokerName(force 时序:校验(New+Bind)
// 先于确认屏/Enroll);URL 直连路径 brokerName 恒空,broker 标签渲染 URL。
// Force → 确认屏(Plan 46:确认的是"重配覆盖"而非"预删除"——零清理先行);
// 否则直接 enroll。newSession 失败:错误落在当前屏(表单态须重建复位,选择屏
// 状态不推进)。
func (w *pairWizard) beginTarget(d *clientops.Discovered) tea.Cmd {
	s, serr := w.newSession(w.opts)
	if serr != nil {
		w.err = serr
		if w.state == pwForm {
			return w.pwFormReset()
		}
		return nil
	}
	w.sess = s
	if d != nil {
		if berr := s.Bind(*d); berr != nil {
			w.err = berr
			return nil
		}
	}
	if w.prefill.Force {
		// Plan 46 T3:确认屏入态时现算四要素(表单里实例名可能已被改过)——
		// 419 advisory 按这个判定分档。
		w.forceComplete = w.slotComplete(w.opts.Instance)
		w.state = pwEnrollForceConfirm
		return nil
	}
	return w.beginEnroll()
}

// beginEnroll 进入 enroll 阶段(Plan 46 零清理先行:force 不再于 Enroll 前调用
// ForceCleanup,旧槽材料直到 WriteAndPull 全部成功才被原子覆盖);ctx 由组件
// 持有,Esc(仅本阶段)触发取消。
func (w *pairWizard) beginEnroll() tea.Cmd {
	w.state = pwEnrolling
	gen := w.gen
	s := w.sess
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	return func() tea.Msg {
		if eerr := s.Enroll(ctx); eerr != nil {
			return pairEnrollDoneMsg{gen: gen, err: eerr}
		}
		return pairEnrollDoneMsg{gen: gen}
	}
}

// pwWaitCmd 启动 WaitApproval(note 经 chan 回流,由 tick 收割上屏)。
func (w *pairWizard) pwWaitCmd() tea.Cmd {
	gen := w.gen
	s := w.sess
	notes := w.noteCh
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	return func() tea.Msg {
		err := s.WaitApproval(ctx, func(n clientops.PollNote) {
			select { // 满则弃(节点流有 30s 节流,缓冲 8 足够;绝不阻塞轮询)
			case notes <- n:
			default:
			}
		})
		return pairApprovalDoneMsg{gen: gen, err: err}
	}
}

// pwTickCmd 是等待屏心跳:1s 后收割一条 note 上屏。测试注入 wizardTickMsg,
// 从不调用本命令(零真实 sleep)。
func pwTickCmd(gen int, notes <-chan clientops.PollNote) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return wizardTickMsg{gen: gen, note: pwHarvestNote(notes)}
	})
}

// pwHarvestNote 非阻塞收割一条轮询事件(空则 nil)。
func pwHarvestNote(notes <-chan clientops.PollNote) *clientops.PollNote {
	select {
	case n := <-notes:
		return &n
	default:
		return nil
	}
}

// pwNoteText 渲染轮询状态行(瞬态受阻打标)。
func pwNoteText(n clientops.PollNote) string {
	if n.Backoff {
		return "瞬态:" + n.Detail
	}
	return n.Detail
}

// approvalOutcome 处理 WaitApproval 的三态终局(nil/gone/timeout/canceled/其他)。
func (w *pairWizard) approvalOutcome(err error) (tea.Model, tea.Cmd) {
	switch {
	case err == nil:
		w.state = pwFinishGate // 人闸:批准已到,SAS 放大复核,Enter 才 Finish
		return w, nil
	case errors.Is(err, clientops.ErrPairGone):
		w.state, w.endReason = pwEnded, pwEndGone
	case errors.Is(err, clientops.ErrPairTimeout):
		w.state, w.endReason = pwEnded, pwEndTimeout
	case errors.Is(err, context.Canceled):
		// 正常路径 Esc 已自行收口;此处兜底(防御态,不产生二次关闭消息)。
		return w, nil
	default:
		w.state, w.endReason, w.endErr = pwEnded, pwEndError, err
	}
	return w, nil
}

// beginWrite 进入写入阶段(Finish → WriteAndPull 同一命令顺序执行)。此阶段的
// ctx 不暴露给 Esc(写入期不可取消),Esc 在 keyUpdate 已被吞。
func (w *pairWizard) beginWrite() tea.Cmd {
	w.state = pwWritePull
	gen := w.gen
	s := w.sess
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	return func() tea.Msg {
		if ferr := s.Finish(ctx); ferr != nil {
			return pairWriteDoneMsg{gen: gen, err: ferr}
		}
		_, werr := s.WriteAndPull(ctx)
		return pairWriteDoneMsg{gen: gen, err: werr}
	}
}

// retry 以相同参数、新 generation、全新会话(enroll 每次生成新 id)重新驱动。
// 同一向导运行已过 force 确认屏,retry 直接重驱 enroll(不再二次确认)。
func (w *pairWizard) retry() tea.Cmd {
	w.gen++ // 新 generation:旧 run 的在途消息自此全部过期
	w.err, w.note = nil, ""
	s, serr := w.newSession(w.opts)
	if serr != nil {
		w.state, w.endReason, w.endErr = pwEnded, pwEndError, serr
		return nil
	}
	w.sess = s
	return w.beginEnroll()
}

// abortCancel 是 ctx 驱动阶段(enroll/wait/gate)的全身而退:先取消在途操作,
// 再收口。gate 阶段无在途操作,取消是空操作。
func (w *pairWizard) abortCancel() (tea.Model, tea.Cmd) {
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	return w.close()
}

// close 收口向导(gen 自增使一切在途消息过期)并交还父模型。
func (w *pairWizard) close() (tea.Model, tea.Cmd) {
	w.gen++
	w.state = pwClosed
	return w, func() tea.Msg { return pairWizardClosedMsg{} }
}

// finishSuccess 是成功收尾:交还父模型并携带新实例(T3:换槽+刷新)。
func (w *pairWizard) finishSuccess() (tea.Model, tea.Cmd) {
	w.gen++
	w.state = pwClosed
	inst := w.opts.Instance
	return w, func() tea.Msg { return pairWizardDoneMsg{instance: inst} }
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

func (w *pairWizard) View() tea.View {
	var b strings.Builder
	switch w.state {
	case pwForm:
		b.WriteString(titleStyle.Render(" 配对向导") + "\n")
		b.WriteString("新机入网:实例名必填;地址留空 = LAN 自动发现(udp/7878)。\n\n")
		b.WriteString(w.form.View() + "\n")
		if w.err != nil {
			b.WriteString("\n" + errStyle.Render("✗ "+w.err.Error()) + "\n")
		}
		b.WriteString(footerStyle.Render("（Esc 退出）"))

	case pwDiscovering:
		b.WriteString(titleStyle.Render(" 配对向导") + "\n\n")
		b.WriteString("正在发现 LAN 内的 broker(udp/7878)……\n\n")
		b.WriteString(footerStyle.Render("（发现窗口很短、不可中断;Esc 放弃结果并退出）"))

	case pwPickBroker:
		b.WriteString(titleStyle.Render(" 选择 broker") + "\n")
		b.WriteString("发现多个 broker,选择要配对的:\n\n")
		for i, d := range w.offers {
			cur := "  "
			if i == w.cursor {
				cur = "> "
			}
			b.WriteString(fmt.Sprintf("%s%s @ %s:%d  spki %s…\n", cur, clientops.StripC0C1(d.Name), d.Addr, d.TCPPort, pwClip16(d.SPKI)))
		}
		if w.err != nil {
			b.WriteString("\n" + errStyle.Render("✗ "+w.err.Error()) + "\n")
		}
		b.WriteString("\n" + footerStyle.Render("（↑/↓ 选择,Enter 确认,Esc 退出）"))

	case pwEnrollForceConfirm:
		b.WriteString(warnStyle.Render(" ⚠ Force 重配确认 —— 实例 "+w.opts.Instance) + "\n\n")
		b.WriteString("重配成功后,新凭据将原子覆盖本实例旧材料;重配成功前,旧材料一律不动\n")
		b.WriteString("(cache.config.json 离线 cap 保留)。\n\n")
		// Plan 46 T3:419 advisory 分档——完整槽=确定性提示(已拉取过,重配前
		// 需 owner 吊销);残缺槽=可能性提示(材料不齐,本地无法预判远端状态)。
		// 判定 = 确认屏入态时的本地四要素(forceComplete)。
		if w.forceComplete {
			b.WriteString("该实例已拉取过,重配前需 owner 在 broker 吊销其设备码\n")
			b.WriteString("(`sshmgr cache-tokens revoke " + w.opts.Instance + "`)。\n\n")
		} else {
			b.WriteString("该实例材料不完整,无法本地预判远端状态;若重跑撞 419 见错误指引。\n\n")
		}
		b.WriteString("Enter 继续重配    Esc 放弃(不改动任何文件)")

	case pwEnrolling:
		b.WriteString(titleStyle.Render(" 配对向导") + "\n\n")
		b.WriteString(fmt.Sprintf("正在 enroll(实例 %s @ %s)……\n\n", w.opts.Instance, w.brokerLabel()))
		b.WriteString(footerStyle.Render("（Esc 取消）"))

	case pwWaiting:
		b.WriteString(titleStyle.Render(" 等待批准") + "\n")
		b.WriteString(fmt.Sprintf("broker: %s · 实例: %s\n\n", w.brokerLabel(), w.opts.Instance))
		b.WriteString("SAS 码 —— 请与 broker 审批面逐位比对:\n\n")
		b.WriteString(warnStyle.Render("   "+pwSASBig(w.sess.SAS(), false)) + "\n\n")
		b.WriteString(fmt.Sprintf("剩余审批窗口: %s\n", pwRemaining(w.sess.ApprovalDeadline())))
		note := w.note
		if note == "" {
			note = "等待 broker 批准…"
		}
		b.WriteString(footerStyle.Render("状态: "+note) + "\n")
		b.WriteString("\n" + footerStyle.Render("（批准到达后进入最终核对;Esc 取消本次配对）"))

	case pwFinishGate:
		b.WriteString(titleStyle.Render(" ✓ broker 已批准 —— 最终核对") + "\n\n")
		b.WriteString("SAS 码(放大复核,与 broker 审批面一致才继续):\n\n")
		b.WriteString(warnStyle.Render("   "+pwSASBig(w.sess.SAS(), true)) + "\n\n")
		b.WriteString("Enter 完成配对(finish + 首次拉取)    Esc 放弃本次配对")

	case pwWritePull:
		b.WriteString(titleStyle.Render(" 配对向导") + "\n\n")
		b.WriteString("写入中,请稍候……(落盘 + 首次拉取,不可中断)")

	case pwDone:
		b.WriteString(titleStyle.Render(" ✓ 配对完成") + "\n\n")
		b.WriteString(fmt.Sprintf("实例: %s\n", w.opts.Instance))
		b.WriteString(fmt.Sprintf("已授权 profile: %s\n", w.sess.AuthorizedProfile()))
		b.WriteString(fmt.Sprintf("产物(0600,含真值 token,勿提交/外发): %s\n\n", w.sess.ArtifactPath()))
		b.WriteString("后续指引:agent 的 .mcp.json 使用 mcp --cache --instance " + w.opts.Instance + "(完整片段见产物文件)\n")
		b.WriteString("\n" + footerStyle.Render("（Enter/Esc 返回）"))

	case pwEnded:
		reason := ""
		switch w.endReason {
		case pwEndGone:
			reason = "本次申请已结束(被拒或过期)"
		case pwEndTimeout:
			reason = "审批窗口已超时——broker 侧未在窗口内批准"
		default:
			reason = "配对失败:" + w.endErr.Error()
		}
		b.WriteString(errStyle.Render(" ✗ "+reason) + "\n\n")
		b.WriteString(footerStyle.Render("（[r] 重新申请(相同参数,全新 generation)   Esc 退出）"))

	default:
		b.WriteString("(已关闭)")
	}
	return altScreen(tea.NewView(b.String()))
}

// brokerLabel 是等待/enroll 屏的 broker 标签:发现流 = Bind 记入的 offer 显示
// 名;URL 直连路径 brokerName 恒空,回退渲染连接 URL。
func (w *pairWizard) brokerLabel() string {
	if w.sess != nil {
		if n := w.sess.BrokerName(); n != "" {
			return clientops.StripC0C1(n)
		}
	}
	return w.opts.URL
}

// pwSASBig 把 6 位 SAS 摊开渲染(门上复核用更宽间距)。
func pwSASBig(sas string, gate bool) string {
	sep := " "
	if gate {
		sep = "  "
	}
	return strings.Join(strings.Split(sas, ""), sep)
}

// pwRemaining 从 ApprovalDeadline 绝对锚取倒计时(不复制窗口常量)。
func pwRemaining(deadline time.Time) string {
	d := time.Until(deadline).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

// pwClip16 渲染 pin 前 16 字符(CLI pickDiscovered 的冻结展示形态,足以区分
// 两个 broker)。
func pwClip16(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
