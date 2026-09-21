package tui

// Plan 45 T2 —— pairwizard 配对向导的失败先行测试(brief Step 1 清单):
// 表单校验拒非法实例名;发现空→回表单;多 broker 选择(+SPKI 升格与 CLI
// pickDiscovered 同规则);force 确认屏在 Enroll 前、Esc 零残留;SAS 屏在
// pwWaiting 即常显(批准者需对照 client 屏的码);pwFinishGate——批准到后方
// 出现,Enter 前 Finish 未被调用(seam 记录调用序钉死),Enter→WriteAndPull;
// Esc 在等待中 cancel ctx;generation 防串扰(注入旧 generation 的 done/tick
// 后 `r` 重试不被污染);pwWritePull 期间 Esc 无效(写入期不可取消);gone/
// timeout/error 三态结果屏 + `r` 重试走通;单槽覆盖 env 命中→拒绝启动;
// PairWizardPrefill 无 AssumeSAS(编译期钉的 reflect 形态)。
//
// 2026-09-21 两级条件表单(owner 方案 B):一级=信任模式+实例名+profile hint;
// 二级=直连的服务地址+服务器公钥指纹(仅手动直连出现,指纹必填+来源提示)。
// TestPWTwoStage_* 契约测试钉住四条:发现流两字段不出现/直连两字段出现且
// 指纹必填/失败路径重建当前一级表单(非复用)/任一级 Esc 同一中止契约。
//
// 全程零网络:session 步骤经组件持有的函数变量 seam(newSession/discover/
// isEnrolled 可替换,生产默认真实现),fake 会话记录调用序;tick 用消息注入,
// 不真 sleep。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/clientops"
)

func testPWPin() string  { return "sha256:" + strings.Repeat("ab", 32) }
func testPWPin2() string { return "sha256:" + strings.Repeat("cd", 32) }

// fakePWSess 是 pairSessionSteps 的测试替身:每个驱动方法记录调用序,终态/返回
// 值按用例注入。WaitApproval 在 cancelSeen 非空时阻塞到 ctx 取消(TUI Esc 语义)。
type fakePWSess struct {
	mu    sync.Mutex
	calls []string

	sas      string
	broker   string
	deadline time.Time
	profile  string
	artifact string
	res      clientops.PullResult

	bindErr   error
	enrollErr error
	waitErr   error
	finishErr error
	writeErr  error

	cancelSeen chan struct{} // 非空 = WaitApproval 阻塞至 ctx.Done 再返回
}

func newFakePWSess() *fakePWSess {
	return &fakePWSess{
		sas:      "135246",
		broker:   "nuc10",
		deadline: time.Now().Add(10 * time.Minute),
		profile:  "team-a",
		artifact: `X:\pair\pair.laptop.mcp.json`,
	}
}

func (f *fakePWSess) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakePWSess) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePWSess) Bind(d clientops.Discovered) error {
	f.record("Bind")
	if f.bindErr == nil {
		f.broker = d.Name // T1 契约:Bind 把 offer 显示名记入 brokerName
	}
	return f.bindErr
}
func (f *fakePWSess) Enroll(ctx context.Context) error { f.record("Enroll"); return f.enrollErr }
func (f *fakePWSess) SAS() string                      { return f.sas }
func (f *fakePWSess) BrokerName() string               { return f.broker }
func (f *fakePWSess) ApprovalDeadline() time.Time      { return f.deadline }

func (f *fakePWSess) WaitApproval(ctx context.Context, note func(clientops.PollNote)) error {
	f.record("WaitApproval")
	if f.cancelSeen != nil {
		<-ctx.Done()
		close(f.cancelSeen)
		return ctx.Err()
	}
	if note != nil {
		note(clientops.PollNote{Pending: true, Detail: "waiting for the owner's approval"})
	}
	return f.waitErr
}

func (f *fakePWSess) Finish(ctx context.Context) error { f.record("Finish"); return f.finishErr }
func (f *fakePWSess) WriteAndPull(ctx context.Context) (clientops.PullResult, error) {
	f.record("WriteAndPull")
	return f.res, f.writeErr
}
func (f *fakePWSess) AuthorizedProfile() string { return f.profile }
func (f *fakePWSess) ArtifactPath() string      { return f.artifact }

// pwHarness 组装一个注入 seam 的向导 + 可断言的假会话。
type pwHarness struct {
	w    *pairWizard
	sess *fakePWSess

	newed         []clientops.PairOpts   // newSession 的调用参数(调用序)
	discoverRet   []clientops.Discovered // discover seam 的返回(默认空)
	newSessionErr error                  // newSession seam 的注入错误
}

func newPWHarness(t *testing.T, prefill PairWizardPrefill) *pwHarness {
	t.Helper()
	isolatedConfigDir(t) // 环境隔离(含清空两个单槽覆盖 env)
	h := &pwHarness{sess: newFakePWSess()}
	w, err := newPairWizard(prefill)
	if err != nil {
		t.Fatalf("newPairWizard: %v", err)
	}
	w.newSession = func(o clientops.PairOpts) (pairSessionSteps, error) {
		if h.newSessionErr != nil {
			return nil, h.newSessionErr
		}
		h.newed = append(h.newed, o)
		return h.sess, nil
	}
	w.discover = func([]string, time.Duration) ([]clientops.Discovered, error) {
		return h.discoverRet, nil
	}
	w.isEnrolled = func(string) (bool, error) { return false, nil }
	h.w = w
	return h
}

// pwStep 同步执行一条返回的命令并把产生的消息喂回 Update(bubbletea 在真实
// 运行里做的事;测试零 goroutine、零 sleep)。
func pwStep(t *testing.T, w *pairWizard, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	w.Update(cmd())
}

// ---------------------------------------------------------------------------
// 真实键击驱动辅助(两级契约测试用):与 editpage 的 press/tap(票 01)同一
// 前提——普通字符键在纯 huh Input 字段上只产生光标闪烁重臂(530ms 睡客,无输
// 出可等),整条丢弃;结构键(Enter/方向)的命令链里混着 huh 的字段推进协议
// (nextField/nextGroup,纯函数、微秒级返回)与闪烁重臂/心跳睡客,后者经
// pwExec 的短预算超时丢弃——光标闪烁是纯视觉态,表单的聚焦/推进是同步副作用,
// 弃掉睡客不影响任何被测逻辑。
// ---------------------------------------------------------------------------

// pwCmdBudget 是 pwExec 等一条命令产出的预算:协议类命令是纯函数,远低于此
// 预算返回;闪烁重臂(530ms)与等待屏心跳(1s)超时即弃。
const pwCmdBudget = 25 * time.Millisecond

// pwExec 在 goroutine 里执行一条命令并带回其消息;预算内未产出即返回 nil
// (丢弃)。命令本身的副作用(如 huh 聚焦)在调用瞬间已同步落地,丢弃的只是
// 消息回流。
func pwExec(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		return m
	case <-time.After(pwCmdBudget):
		return nil
	}
}

// pwPump 像运行时一样追一条命令链:BatchMsg 展开、产生的消息喂回 Update、
// 循环到链尾;睡客经 pwExec 丢弃。有界防环(失控即响亮失败)。
func pwPump(t *testing.T, m tea.Model, cmd tea.Cmd) *pairWizard {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 200 {
			t.Fatal("pwPump: runaway cmd loop (>200 steps)")
		}
		c := queue[0]
		queue = queue[1:]
		msg := pwExec(c)
		switch msg := msg.(type) {
		case nil:
			continue
		case tea.BatchMsg:
			queue = append(queue, msg...)
			continue
		}
		var next tea.Cmd
		m, next = m.Update(msg)
		queue = append(queue, next)
	}
	w, ok := m.(*pairWizard)
	if !ok {
		t.Fatalf("pwPump: model drifted to %T", m)
	}
	return w
}

// pwTap 发一个结构键(Enter/方向等)并追完返回的命令链。
func pwTap(t *testing.T, w *pairWizard, code rune, text string) *pairWizard {
	t.Helper()
	m, cmd := w.Update(tea.KeyPressMsg{Code: code, Text: text})
	return pwPump(t, m, cmd)
}

// pwPress 键入一个可打印字符:值同步落进绑定的 draft(huh 在 Update 内写),
// 返回的命令整条丢弃(见本节头注释的前提)。
func pwPress(t *testing.T, w *pairWizard, r rune) *pairWizard {
	t.Helper()
	m, _ := w.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	ww, ok := m.(*pairWizard)
	if !ok {
		t.Fatalf("pwPress: update returned %T", m)
	}
	return ww
}

// pwType 连续键入一个字符串。
func pwType(t *testing.T, w *pairWizard, s string) *pairWizard {
	t.Helper()
	for _, r := range s {
		w = pwPress(t, w, r)
	}
	return w
}

// pwFocus 跑一次向导 Init(真实运行时挂载时做的事):huh 在 Init 调用内同步
// 聚焦首个字段,返回的命令链(闪烁/窗口尺寸请求)经 pwPump 丢弃——聚焦副作用
// 已落地。
func pwFocus(t *testing.T, w *pairWizard) *pairWizard {
	t.Helper()
	return pwPump(t, w, w.Init())
}

// driveURLToWaiting 以直连(URL+pin)路径把向导推进到 pwWaiting(两级形态:
// 先一级选「手动直连」落二级,再二级提交;enroll 已完成,WaitApproval 命令
// 未执行——等待中的用例按需自行取出执行)。
func (h *pwHarness) driveURLToWaiting(t *testing.T) {
	t.Helper()
	h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	pwStep(t, h.w, h.w.submitModeForm()) // 一级完成(直连)→ 二级表单
	if h.w.state != pwFormDirect {
		t.Fatalf("drive: 一级直连提交必须落到二级表单, state=%v err=%v", h.w.state, h.w.err)
	}
	pwStep(t, h.w, h.w.submitForm())
	if h.w.state != pwWaiting {
		t.Fatalf("drive to pwWaiting failed: state=%v err=%v newed=%d", h.w.state, h.w.err, len(h.newed))
	}
}

// runWait 同步执行等待命令(fake 立即返回)并把批准结果喂回 Update。
func (h *pwHarness) runWait(t *testing.T) {
	t.Helper()
	h.w.Update(h.w.pwWaitCmd()())
}

// ---------------------------------------------------------------------------
// 编译期钉(形态) + 入口互斥
// ---------------------------------------------------------------------------

// TestPairWizardPrefill_NoAssumeSAS 钉住 plan 冻结:AssumeSAS 永驻 CLI 驱动层,
// 向导预填类型不含该字段(类型上不含即可;env 判定全仓唯一在 internal/cli)。
func TestPairWizardPrefill_NoAssumeSAS(t *testing.T) {
	typ := reflect.TypeOf(PairWizardPrefill{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name == "AssumeSAS" {
			t.Fatal("PairWizardPrefill must not carry AssumeSAS — the env judgment is CLI-only (plan 冻结)")
		}
	}
	if typ.NumField() != 5 {
		t.Fatalf("PairWizardPrefill shape drifted (want Instance/ProfileHint/URL/Pin/Force), got %d fields", typ.NumField())
	}
}

// TestNewPairWizard_SingleSlotOverrideRefused:命中单槽覆盖 env 拒绝启动(与
// CLI --instance 互斥同语义,权威判定 = cli/common.go 注释 + 共享 helper
// clientops.SingleSlotOverrideEnvSet);SSHMGR_CACHE_DEK_DIR 是目录级组合 seam,
// 权威判定明确放行。
func TestNewPairWizard_SingleSlotOverrideRefused(t *testing.T) {
	isolatedConfigDir(t)
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	if _, err := newPairWizard(PairWizardPrefill{Instance: "laptop"}); err == nil {
		t.Fatal("SSHMGR_CACHE_DIR override must refuse wizard startup")
	}

	isolatedConfigDir(t)
	t.Setenv("SSHMGR_CACHE_DEK", t.TempDir())
	if _, err := newPairWizard(PairWizardPrefill{Instance: "laptop"}); err == nil {
		t.Fatal("SSHMGR_CACHE_DEK override must refuse wizard startup")
	}

	isolatedConfigDir(t)
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir()) // 组合语义,不构成单槽覆盖
	if _, err := newPairWizard(PairWizardPrefill{Instance: "laptop"}); err != nil {
		t.Fatalf("SSHMGR_CACHE_DEK_DIR composes and must not refuse startup: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 表单校验 + 提交闸门
// ---------------------------------------------------------------------------

func TestPWValidators(t *testing.T) {
	if err := pwValidateInstance(""); err == nil {
		t.Fatal("instance 必填")
	}
	if err := pwValidateInstance("bad/name"); err == nil {
		t.Fatal("非法实例名必须被拒")
	}
	if err := pwValidateInstance("ok-name"); err != nil {
		t.Fatalf("legal instance rejected: %v", err)
	}
	// 两级形态(2026-09-21 方案 B)后:URL 与服务器公钥指纹只在二级表单出现
	// 且都必填——发现流根本不经过这两个字段(材料化强制空),旧断言「空地址
	// = 发现流放行」按新契约反转。
	if err := pwValidateURL(""); err == nil {
		t.Fatal("二级表单里空服务地址必须被拒(发现流不经过该字段)")
	}
	if err := pwValidateURL("htts://192.0.2.5:7878"); err == nil {
		t.Fatal("非 https 地址必须被拒")
	}
	if err := pwValidateURL("https://192.0.2.5:7878"); err != nil {
		t.Fatalf("legal URL rejected: %v", err)
	}
	// 同上:旧断言「空 pin 放行(直连由会话校验拒 TOFU)」按新契约反转——
	// 指纹必填是 owner 拍板的 D4 落地;会话层的 TOFU 拒绝仍是更深的兜底。
	if err := pwValidatePin(""); err == nil {
		t.Fatal("二级表单里空服务器公钥指纹必须被拒(指纹必填)")
	}
	if err := pwValidatePin("nope"); err == nil {
		t.Fatal("畸形 pin 必须被拒")
	}
	if err := pwValidatePin(testPWPin()); err != nil {
		t.Fatalf("legal pin rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 两级条件表单(2026-09-21 owner 方案 B)——行为契约测试(D9:与两级改造同票
// 落地、先钉契约再实现)
// ---------------------------------------------------------------------------

// TestPWTwoStage_LANFlowHidesDirectFields(契约 1):选「局域网发现」→ 服务地址
// 与服务器公钥指纹两字段根本不出现——一级表单(含文案)不含两字段标题与指纹
// 来源提示;真实键击选发现并提交后直接进发现流(不经过二级表单态);材料化时
// URL/pin 强制空,draft 里的直连预填残值也一并清空(不是隐藏空值地流入 opts;
// 发现流的 pin 由选中 offer 的 SPKI 升格补上)。
func TestPWTwoStage_LANFlowHidesDirectFields(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.discoverRet = []clientops.Discovered{
		{Name: "nuc10", Addr: "192.0.2.5", SPKI: testPWPin(), TCPPort: 7878},
		{Name: "nuc11", Addr: "192.0.2.6", SPKI: testPWPin2(), TCPPort: 7879},
	}
	w := pwFocus(t, h.w) // 先跑 Init(真实运行时挂载即做):视图断言对着可交互形态
	v := w.View().Content // 一级表单(信任模式/实例名/profile hint)
	for _, want := range []string{"信任模式", "局域网发现", "手动直连", "实例名", "profile hint"} {
		if !strings.Contains(v, want) {
			t.Fatalf("一级表单必须出现 %q:\n%s", want, v)
		}
	}
	for _, banned := range []string{"broker 服务地址", "服务器公钥指纹", "sshmgr serve cert-info"} {
		if strings.Contains(v, banned) {
			t.Fatalf("一级表单(含文案)不得出现直连字段 %q:\n%s", banned, v)
		}
	}

	// 真实键击走完一级:Enter 选中第一项(局域网发现)→ 填实例名 → 提交。
	w = pwTap(t, w, tea.KeyEnter, "")
	w = pwType(t, w, "laptop")
	w = pwTap(t, w, tea.KeyEnter, "")
	if w.state != pwFormMode {
		t.Fatalf("一级未完成前必须停在一级表单, got %v", w.state)
	}
	w = pwTap(t, w, tea.KeyEnter, "")
	if w.state != pwPickBroker {
		t.Fatalf("局域网发现提交必须走发现流并停在选择屏(不经过二级表单), got %v", w.state)
	}
	if v := w.View().Content; !strings.Contains(v, "nuc10") || !strings.Contains(v, "nuc11") {
		t.Fatalf("发现流必须列出两个 offer:\n%s", v)
	}
	if w.draft.URL != "" || w.draft.Pin != "" || w.opts.URL != "" || w.opts.Pin != "" {
		t.Fatalf("发现流材料化必须 URL/pin 全空, draft=%+v opts=%+v", w.draft, w.opts)
	}
	if len(h.newed) != 0 {
		t.Fatalf("一级提交本身不得建会话, newed=%d", len(h.newed))
	}

	// 直连预填残值在改选发现时必须被清空——不是隐藏着流进 opts。
	h2 := newPWHarness(t, PairWizardPrefill{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()})
	h2.discoverRet = h.discoverRet // 两个 offer:停在选择屏,不进会话
	if h2.w.draft.Mode != pwModeDirect {
		t.Fatalf("URL 预填必须预选「手动直连」, got %q", h2.w.draft.Mode)
	}
	h2.w.draft.Mode = pwModeDiscover // 模拟用户改选发现
	pwStep(t, h2.w, h2.w.submitModeForm())
	if h2.w.state != pwPickBroker {
		t.Fatalf("改选发现必须走发现流, got %v", h2.w.state)
	}
	if h2.w.draft.URL != "" || h2.w.draft.Pin != "" || h2.w.opts.URL != "" || h2.w.opts.Pin != "" {
		t.Fatalf("改选发现必须清空直连预填残值(两字段根本不出现,不是隐藏空值), draft=%+v opts=%+v", h2.w.draft, h2.w.opts)
	}
}

// TestPWTwoStage_DirectFlowSecondLevel(契约 2):选「手动直连」→ 串联二级表单:
// 服务地址与服务器公钥指纹两字段出现(带指纹来源提示);两字段都必填——空值
// 在字段层被拒(表单不完成、零会话);填全后提交走直连(URL+pin 逐值材料化进
// PairOpts)。
func TestPWTwoStage_DirectFlowSecondLevel(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	w := pwFocus(t, h.w)
	w = pwTap(t, w, 'j', "j")         // Select 光标移到「手动直连」(j = 下一项)
	w = pwTap(t, w, tea.KeyEnter, "") // 选中并推进到实例名
	w = pwType(t, w, "laptop")
	w = pwTap(t, w, tea.KeyEnter, "") // 推进到 profile hint
	w = pwTap(t, w, tea.KeyEnter, "") // 提交 → 一级完成 → 二级表单
	if w.state != pwFormDirect {
		t.Fatalf("选「手动直连」完成一级后必须进入二级表单, got %v", w.state)
	}
	v := w.View().Content
	for _, want := range []string{"broker 服务地址", "服务器公钥指纹", "sshmgr serve cert-info"} {
		if !strings.Contains(v, want) {
			t.Fatalf("二级表单必须出现 %q(字段标题/指纹来源提示):\n%s", want, v)
		}
	}

	// 服务地址必填:空值上 Enter 被字段校验拦下。
	w = pwTap(t, w, tea.KeyEnter, "")
	if w.state != pwFormDirect || w.form.State != huh.StateNormal {
		t.Fatalf("空服务地址必须被字段校验拦下, state=%v form=%v", w.state, w.form.State)
	}
	if v := w.View().Content; !strings.Contains(v, "直连必须填写服务地址") {
		t.Fatalf("空服务地址的拒绝必须给出文案:\n%s", v)
	}

	// 指纹必填:地址填好后,空指纹同样被拦(零会话)。
	w = pwType(t, w, "https://192.0.2.5:7878")
	w = pwTap(t, w, tea.KeyEnter, "") // 推进到指纹字段
	w = pwTap(t, w, tea.KeyEnter, "") // 空指纹上 Enter → 拦下
	if w.state != pwFormDirect || len(h.newed) != 0 {
		t.Fatalf("空指纹必须被拦在表单层(零会话), state=%v newed=%d", w.state, len(h.newed))
	}
	if v := w.View().Content; !strings.Contains(v, "直连必须提供服务器公钥指纹") {
		t.Fatalf("空指纹的拒绝必须给出文案:\n%s", v)
	}

	// 填全后提交:直连建会话,URL+pin 逐值材料化;真键击一路推进到批准门
	// (等待屏心跳的 1s 睡客被 pwExec 丢弃,不拖慢测试)。
	w = pwType(t, w, testPWPin())
	w = pwTap(t, w, tea.KeyEnter, "")
	if len(h.newed) != 1 {
		t.Fatalf("二级提交必须建直连会话, newed=%d", len(h.newed))
	}
	if h.newed[0].URL != "https://192.0.2.5:7878" || h.newed[0].Pin != testPWPin() || h.newed[0].Instance != "laptop" {
		t.Fatalf("直连提交必须逐值材料化 URL+pin, got %+v", h.newed[0])
	}
	if w.state != pwFinishGate {
		t.Fatalf("直连全流程必须推进到批准门(fake 立即批准), got %v", w.state)
	}
}

// TestPWTwoStage_FailureRebuildsCurrentLevel(契约 3):失败路径重建「当前一级」
// 表单而非复用实例——拒绝后 w.form 是新实例(StateNormal,可再输入),draft 指针
// 共享保值;一级与二级各自的失败路径都钉(T2-R1 死锁教训纪律)。
func TestPWTwoStage_FailureRebuildsCurrentLevel(t *testing.T) {
	t.Run("mode_level_illegal_instance", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{})
		h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "bad/name"}
		f0 := h.w.form
		h.w.submitModeForm()
		if h.w.state != pwFormMode || h.w.err == nil {
			t.Fatalf("一级拒绝必须留在一级表单并带错误, state=%v err=%v", h.w.state, h.w.err)
		}
		if h.w.form == f0 {
			t.Fatal("拒绝必须重建表单(新实例),不得复用停在 StateCompleted 的旧实例")
		}
		if h.w.form.State != huh.StateNormal {
			t.Fatalf("重建后的表单必须回到 StateNormal, got %v", h.w.form.State)
		}
		if h.w.draft.Instance != "bad/name" {
			t.Fatalf("重建表单必须共享 draft(值保留), got %+v", h.w.draft)
		}
	})

	t.Run("direct_level_session_error", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{})
		h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
		h.newSessionErr = errors.New("session refused")
		f0 := h.w.form
		h.w.submitForm()
		if h.w.state != pwFormDirect || h.w.err == nil {
			t.Fatalf("会话错误必须留在二级表单并带错误, state=%v err=%v", h.w.state, h.w.err)
		}
		if h.w.form == f0 {
			t.Fatal("拒绝必须重建二级表单(新实例),不得复用旧实例")
		}
		if h.w.form.State != huh.StateNormal {
			t.Fatalf("重建后的表单必须回到 StateNormal, got %v", h.w.form.State)
		}
		if h.w.draft.URL != "https://192.0.2.5:7878" || h.w.draft.Pin != testPWPin() {
			t.Fatalf("重建表单必须共享 draft(值保留), got %+v", h.w.draft)
		}
	})

	t.Run("discovery_empty_back_to_mode_form", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{})
		h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "laptop"}
		cmd := h.w.submitModeForm()
		h.w.Update(cmd()) // discover seam 返回空
		if h.w.state != pwFormMode || h.w.err == nil {
			t.Fatalf("发现落空必须回一级表单并带错误, state=%v err=%v", h.w.state, h.w.err)
		}
		if h.w.form.State != huh.StateNormal {
			t.Fatalf("回一级必须重建复位表单, got %v", h.w.form.State)
		}
		// 重建的是一级:视图无直连字段。
		if v := h.w.View().Content; strings.Contains(v, "服务器公钥指纹") {
			t.Fatalf("发现落空回表单必须回一级(无直连字段):\n%s", v)
		}
	})
}

// TestPWTwoStage_EscAnyLevelCloses(契约 4):两级形态下任一级 Esc 都是同一
// 中止契约(D9:沿用现行语义,不自创「Esc 返回上一级」之类的新行为)——
// Update 返回 pairWizardClosedMsg 交还父模型、gen 自增、纯返回零残留(无会话
// 构建、无会话方法调用)。
func TestPWTwoStage_EscAnyLevelCloses(t *testing.T) {
	t.Run("level1_mode_form", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{})
		gen0 := h.w.gen
		_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if _, ok := cmd().(pairWizardClosedMsg); !ok {
			t.Fatalf("一级表单 Esc 必须走既有关闭通道(pairWizardClosedMsg), got %T", cmd())
		}
		if h.w.state != pwClosed || h.w.gen != gen0+1 {
			t.Fatalf("Esc 必须收口向导并自增 generation, state=%v gen=%d(want %d)", h.w.state, h.w.gen, gen0+1)
		}
		if len(h.newed) != 0 || len(h.sess.order()) != 0 {
			t.Fatalf("Esc 必须零残留, newed=%d calls=%v", len(h.newed), h.sess.order())
		}
	})

	t.Run("level2_direct_form", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{})
		h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		pwStep(t, h.w, h.w.submitModeForm())
		if h.w.state != pwFormDirect {
			t.Fatalf("precondition: 二级表单, got %v", h.w.state)
		}
		gen0 := h.w.gen
		_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if _, ok := cmd().(pairWizardClosedMsg); !ok {
			t.Fatalf("二级表单 Esc 必须走既有关闭通道(pairWizardClosedMsg), got %T", cmd())
		}
		if h.w.state != pwClosed || h.w.gen != gen0+1 {
			t.Fatalf("Esc 必须收口向导并自增 generation, state=%v gen=%d(want %d)", h.w.state, h.w.gen, gen0+1)
		}
		if len(h.newed) != 0 || len(h.sess.order()) != 0 {
			t.Fatalf("Esc 必须零残留, newed=%d calls=%v", len(h.newed), h.sess.order())
		}
	})
}

// TestPWTwoStage_PrefillURLPreselectsDirect:prefill 带 URL(直连)时,一级的
// 信任模式预选「手动直连」——协调者裁量的落地形态:实例名留在一级(两条路径
// 都要填,不能整级跳过),模式预选后用户确认即落二级;二级提交材料化的就是
// 预填的 URL+pin。
func TestPWTwoStage_PrefillURLPreselectsDirect(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()})
	if h.w.draft.Mode != pwModeDirect {
		t.Fatalf("URL 预填必须预选「手动直连」, got %q", h.w.draft.Mode)
	}
	w := pwFocus(t, h.w)
	w = pwTap(t, w, tea.KeyEnter, "") // 选中预选的直连,推进到实例名
	w = pwTap(t, w, tea.KeyEnter, "") // 实例名已预填,推进到 hint
	w = pwTap(t, w, tea.KeyEnter, "") // 提交 → 一级完成 → 二级表单
	if w.state != pwFormDirect {
		t.Fatalf("预选直连的一级提交必须落到二级表单, got %v", w.state)
	}
	pwStep(t, w, w.submitForm())
	if len(h.newed) != 1 || h.newed[0].URL != "https://192.0.2.5:7878" || h.newed[0].Pin != testPWPin() {
		t.Fatalf("二级提交必须材料化预填的 URL+pin, got %+v", h.newed)
	}
	if w.state != pwWaiting {
		t.Fatalf("预填直连提交后必须进入等待屏, got %v", w.state)
	}
}

// TestPairWizard_SubmitRejectsIllegalInstance:实例名校验在一级提交闸住;二级
// 提交保留 belt-and-braces 复核(draft 被外部弄脏、真实表单走不到的路径)。
func TestPairWizard_SubmitRejectsIllegalInstance(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "bad/name"}
	h.w.submitModeForm() // 返回值 = 一级表单复位命令,不是启动命令
	if h.w.state != pwFormMode || h.w.err == nil {
		t.Fatalf("refusal must stay on the mode form with an error, state=%v err=%v", h.w.state, h.w.err)
	}
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("refusal must reset the completed form (StateCompleted = dead input), state=%v", h.w.form.State)
	}
	if len(h.newed) != 0 {
		t.Fatalf("an illegal instance name must never reach session construction, newed=%d", len(h.newed))
	}

	// 二级 belt-and-braces:真实表单走不到的路径(一级闸门先拦),模拟 draft
	// 在进入二级后被外部弄脏——二级提交同样拒绝(重建二级表单)。
	h2 := newPWHarness(t, PairWizardPrefill{})
	h2.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	pwStep(t, h2.w, h2.w.submitModeForm()) // → 二级表单
	h2.w.draft.Instance = "bad/name"       // 弄脏:draft 被外部改写
	h2.w.submitForm()
	if h2.w.state != pwFormDirect || h2.w.err == nil {
		t.Fatalf("level-2 refusal must stay on the direct form with an error, state=%v err=%v", h2.w.state, h2.w.err)
	}
	if h2.w.form.State != huh.StateNormal {
		t.Fatalf("level-2 refusal must reset the completed form, state=%v", h2.w.form.State)
	}
	if len(h2.newed) != 0 {
		t.Fatalf("an illegal instance name must never reach session construction, newed=%d", len(h2.newed))
	}
}


// TestPairWizard_SubmitEnrolledNeedsForce pins the form-side 已装判定:enrolled
// 且未 force → 拒绝(先于任何会话构建;两级形态下两条路径各自的提交都闸);
// force → 在任何清理之前先过确认屏。
func TestPairWizard_SubmitEnrolledNeedsForce(t *testing.T) {
	// 局域网发现路径:闸门在一级提交。
	h := newPWHarness(t, PairWizardPrefill{})
	h.w.isEnrolled = func(string) (bool, error) { return true, nil }
	h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "laptop"}
	h.w.submitModeForm() // 拒绝;返回值 = 一级表单复位命令
	if h.w.state != pwFormMode || h.w.err == nil || !strings.Contains(h.w.err.Error(), "force") {
		t.Fatalf("refusal must stay on the mode form and mention force, state=%v err=%v", h.w.state, h.w.err)
	}
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("refusal must reset the completed form, state=%v", h.w.form.State)
	}
	if len(h.newed) != 0 {
		t.Fatalf("the enrolled gate must precede session construction, newed=%d", len(h.newed))
	}

	// 手动直连路径:闸门在二级提交(重建二级表单)。
	h2 := newPWHarness(t, PairWizardPrefill{})
	h2.w.isEnrolled = func(string) (bool, error) { return true, nil }
	h2.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	pwStep(t, h2.w, h2.w.submitModeForm()) // → 二级表单
	h2.w.submitForm()                      // 拒绝;返回值 = 二级表单复位命令
	if h2.w.state != pwFormDirect || h2.w.err == nil || !strings.Contains(h2.w.err.Error(), "force") {
		t.Fatalf("level-2 refusal must stay on the direct form and mention force, state=%v err=%v", h2.w.state, h2.w.err)
	}
	if h2.w.form.State != huh.StateNormal {
		t.Fatalf("level-2 refusal must reset the completed form, state=%v", h2.w.form.State)
	}
	if len(h2.newed) != 0 {
		t.Fatalf("the enrolled gate must precede session construction, newed=%d", len(h2.newed))
	}

	// force → 确认屏(New 之后、Enroll 之前:确认屏上任何会话方法都未调用)。
	h3 := newPWHarness(t, PairWizardPrefill{Force: true})
	h3.w.isEnrolled = func(string) (bool, error) { return true, nil }
	h3.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	pwStep(t, h3.w, h3.w.submitModeForm()) // → 二级表单
	if cmd := h3.w.submitForm(); cmd != nil {
		t.Fatal("force path must stop at the confirm screen, not start anything")
	}
	if h3.w.state != pwEnrollForceConfirm {
		t.Fatalf("force submit must land on the confirm screen, got %v", h3.w.state)
	}
	if calls := h3.sess.order(); len(calls) != 0 {
		t.Fatalf("confirm screen must precede ANY session call, got %v", calls)
	}
}

func TestPairWizard_NewSessionErrShowsOnForm(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.newSessionErr = errors.New("refusing TOFU pairing without --pin")
	h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
	h.w.submitForm()                     // 返回值 = 二级表单复位命令
	if h.w.state != pwFormDirect || h.w.err == nil {
		t.Fatalf("session error must surface on the direct form, state=%v err=%v", h.w.state, h.w.err)
	}
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("session error must reset the completed form, state=%v", h.w.form.State)
	}
}

// TestPWTwoStage_EscAnyLevelCloses(上方)吸收了旧 TestPairWizard_FormEscCloses
// 的全部断言(Esc → pairWizardClosedMsg + pwClosed),并按两级契约扩展到二级
// 表单与 generation/零残留断言。

// TestPairWizard_FormRefusalResetsCompletedForm(评审 R1 发现 1 的回归钉):
// huh 的 Form.Update 在 State != StateNormal 时短路——提交失败留在表单态必须
// 重建表单(draft 指针共享保值),否则用户无法再输入,且表单态下任意非 Esc
// 按键都会命中 Completed 分支重跑提交(发现落空屏每键重发 LAN sweep)。两级
// 形态下以二级(直连)提交路径钉。
func TestPairWizard_FormRefusalResetsCompletedForm(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.w.isEnrolled = func(string) (bool, error) { return true, nil }
	h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
	h.w.submitForm()                     // 已配对且未 force → 拒绝
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("refusal must rebuild the form to StateNormal, got %v", h.w.form.State)
	}
	if h.w.draft.Instance != "laptop" || h.w.draft.URL != "https://192.0.2.5:7878" {
		t.Fatalf("the rebuilt form must share the draft (values retained), got %+v", h.w.draft)
	}
	// 复位后的表单上任意按键 = 普通输入,不得重跑 submitForm(无重提交环)。
	// 按键会真实落进聚焦的字段(这里是二级首字段服务地址)——那是合法编辑,
	// 不是重提交;fresh submit 前把被按键弄花的值修正回来。
	before := len(h.newed)
	h.w.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if len(h.newed) != before {
		t.Fatal("a keypress on the refused form must not re-drive submitForm")
	}
	if h.w.state != pwFormDirect {
		t.Fatalf("a stray keypress must stay on the form, got %v", h.w.state)
	}
	// 用户改主意(解除已装闸门)后可再次提交:流程正常推进。
	h.w.isEnrolled = func(string) (bool, error) { return false, nil }
	h.w.draft.URL = "https://192.0.2.5:7878"
	pwStep(t, h.w, h.w.submitForm())
	if h.w.state != pwWaiting {
		t.Fatalf("a fresh submit after the refusal must drive on, state=%v err=%v", h.w.state, h.w.err)
	}
}

// ---------------------------------------------------------------------------
// 发现 → 选择
// ---------------------------------------------------------------------------

func TestPairWizard_DiscoverEmptyBackToForm(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "laptop"} // 发现流
	cmd := h.w.submitModeForm()
	if h.w.state != pwDiscovering {
		t.Fatalf("a discover-mode submit must enter pwDiscovering, got %v", h.w.state)
	}
	h.w.Update(cmd()) // discover seam 返回空
	if h.w.state != pwFormMode {
		t.Fatalf("empty discovery must return to the mode form, got %v", h.w.state)
	}
	if h.w.err == nil || !strings.Contains(h.w.err.Error(), "未发现") {
		t.Fatalf("empty discovery must explain itself on the form, err=%v", h.w.err)
	}
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("returning to the form must reset the completed form (R1 发现 1), state=%v", h.w.form.State)
	}
}

func TestPairWizard_SingleOfferSkipsPick(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.discoverRet = []clientops.Discovered{{Name: "nuc10", Addr: "192.0.2.5", SPKI: testPWPin(), TCPPort: 7878}}
	h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "laptop"}
	pwStep(t, h.w, h.w.submitModeForm())
	if h.w.state != pwEnrolling {
		t.Fatalf("a single offer must skip the pick screen, got %v", h.w.state)
	}
	if len(h.newed) != 1 || h.newed[0].URL != "https://192.0.2.5:7878" || h.newed[0].Pin != testPWPin() {
		t.Fatalf("the single offer must materialize into opts (SPKI 升格), got %+v", h.newed)
	}
}

func TestPairWizard_MultiBrokerPickAndSPKIUpgrade(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.discoverRet = []clientops.Discovered{
		{Name: "nuc10", Addr: "192.0.2.5", SPKI: testPWPin(), TCPPort: 7878},
		{Name: "nuc11", Addr: "192.0.2.6", SPKI: testPWPin2(), TCPPort: 7879},
	}
	h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "laptop"}
	pwStep(t, h.w, h.w.submitModeForm())
	if h.w.state != pwPickBroker {
		t.Fatalf("multiple offers must land on the pick screen, got %v", h.w.state)
	}
	v := h.w.View().Content
	if !strings.Contains(v, "nuc10") || !strings.Contains(v, "nuc11") {
		t.Fatalf("pick screen must list every discovered broker, got:\n%s", v)
	}
	h.w.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // 光标移到第二行
	_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on a broker row must start the enroll")
	}
	if h.w.state != pwEnrolling {
		t.Fatalf("picking a broker must start enrolling, got %v", h.w.state)
	}
	if len(h.newed) != 1 || h.newed[0].URL != "https://192.0.2.6:7879" || h.newed[0].Pin != testPWPin2() {
		t.Fatalf("the picked offer must materialize into opts (SPKI 升格与 CLI pickDiscovered 同规则), got %+v", h.newed)
	}
	// R1 发现 2:发现流必须 Bind 幂等重校验并把 offer 名记入 brokerName
	// (校验(New+Bind)先于 enroll,plan 冻结时序)。
	if got := h.sess.order(); len(got) == 0 || got[0] != "Bind" {
		t.Fatalf("the discovered pick must re-Bind before anything else, got %v", got)
	}
	pwStep(t, h.w, cmd) // fake enroll 立即完成
	if got := h.sess.order(); !reflect.DeepEqual(got, []string{"Bind", "Enroll"}) {
		t.Fatalf("the discovered pick must Bind then enroll, got %v", got)
	}
	if h.w.state != pwWaiting {
		t.Fatalf("enroll success must land in pwWaiting, got %v", h.w.state)
	}
	if v := h.w.View().Content; !strings.Contains(v, "nuc11") {
		t.Fatalf("the waiting screen must show the offer name (Bind 补名), got:\n%s", v)
	}
}

// ---------------------------------------------------------------------------
// force:确认屏在先、Esc 零残留、零清理先行(Plan 46)
// ---------------------------------------------------------------------------

// TestPairWizard_ForceConfirm_AdvisoryTiers (Plan 46 T3):419 advisory 分档入
// p 确认屏——完整槽=确定性提示(已拉取过,重配前需 owner 吊销);残缺槽=
// 可能性提示(材料不齐,本地无法预判远端状态)。判定取确认屏入态时的实例名
// (seam 收到的参数即 opts.Instance——表单确认后的那个)。
func TestPairWizard_ForceConfirm_AdvisoryTiers(t *testing.T) {
	t.Run("complete_slot_definite_revoke", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{Force: true})
		var seen []string
		h.w.slotComplete = func(inst string) bool { seen = append(seen, inst); return true }
		h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
		h.w.submitForm()                     // → pwEnrollForceConfirm
		if h.w.state != pwEnrollForceConfirm || !h.w.forceComplete {
			t.Fatalf("precondition: confirm screen with a complete slot, state=%v forceComplete=%v", h.w.state, h.w.forceComplete)
		}
		if len(seen) != 1 || seen[0] != "laptop" {
			t.Fatalf("the tier judgment must run on the form-confirmed instance, got %v", seen)
		}
		v := h.w.View().Content
		if !strings.Contains(v, "该实例已拉取过,重配前需 owner 在 broker 吊销其设备码") {
			t.Fatalf("a complete slot must get the definite revoke advisory, got:\n%s", v)
		}
		if strings.Contains(v, "材料不完整") {
			t.Fatalf("a complete slot must not get the partial-tier wording, got:\n%s", v)
		}
	})

	t.Run("partial_slot_possibility_only", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{Force: true})
		h.w.slotComplete = func(string) bool { return false }
		h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
		h.w.submitForm()
		if h.w.state != pwEnrollForceConfirm || h.w.forceComplete {
			t.Fatalf("precondition: confirm screen with a partial slot, state=%v forceComplete=%v", h.w.state, h.w.forceComplete)
		}
		v := h.w.View().Content
		if !strings.Contains(v, "该实例材料不完整,无法本地预判远端状态;若重跑撞 419 见错误指引") {
			t.Fatalf("a partial slot must get the possibility-only advisory, got:\n%s", v)
		}
		if strings.Contains(v, "已拉取过") || strings.Contains(v, "revoke") {
			t.Fatalf("a partial slot must not promise definite remote knowledge, got:\n%s", v)
		}
	})

	t.Run("default_seam_reads_local_disk", func(t *testing.T) {
		// 缺省 seam = slotArtifactsComplete(本地四要素 stat):真空环境读作
		// 残缺档——advisory 永不凭空断言远端状态。
		h := newPWHarness(t, PairWizardPrefill{Force: true}) // isolatedConfigDir:无任何材料
		h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
		h.w.submitForm()
		if h.w.forceComplete {
			t.Fatal("the default seam must read the local disk (vacuum = not complete)")
		}
		if v := h.w.View().Content; !strings.Contains(v, "材料不完整") {
			t.Fatalf("the vacuum slot must get the partial-tier advisory, got:\n%s", v)
		}
	})
}

func TestPairWizard_ForceConfirm_EscZeroResidue(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{Force: true})
	h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
	h.w.submitForm()                     // → pwEnrollForceConfirm
	_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if _, ok := cmd().(pairWizardClosedMsg); !ok {
		t.Fatalf("Esc at the confirm screen must close the wizard, got %T", cmd())
	}
	if h.w.state != pwClosed {
		t.Fatalf("closed wizard state=%v, want pwClosed", h.w.state)
	}
	if calls := h.sess.order(); len(calls) != 0 {
		t.Fatalf("Esc at the confirm screen must leave zero residue (no cleanup/enroll), got %v", calls)
	}
}

// TestPairWizard_Force_EnrollWithoutCleanup 是 Plan 46 零清理先行的 TUI 回归钉:
// force 确认屏 Enter 后,向导只驱动 Enroll——任何会话方法里都不再出现
// ForceCleanup(旧材料直到 WriteAndPull 成功才被覆盖,enroll 阶段失败旧槽
// 一字不动)。
func TestPairWizard_Force_EnrollWithoutCleanup(t *testing.T) {
	t.Run("url_combo_enroll_only", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{Force: true})
		// Plan 46 T3:确认屏 419 advisory 分档——本用例断言含 revoke 指引,即
		// 完整槽档,注入 seam 让判定为真(缺省真实现在本隔离环境读作残缺)。
		h.w.slotComplete = func(string) bool { return true }
		h.w.draft = &pwDraft{Mode: pwModeDirect, Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		pwStep(t, h.w, h.w.submitModeForm()) // → 二级表单
		h.w.submitForm()                     // → pwEnrollForceConfirm
		// 确认屏文案如实化(Plan 46):说「覆盖」,不再有「删除文件」清单;并
		// 给出 419 时的 owner 吊销指引。
		v := h.w.View().Content
		if !strings.Contains(v, "覆盖") || !strings.Contains(v, "cache.config.json") || !strings.Contains(v, "revoke") {
			t.Fatalf("confirm screen must carry the honest overwrite+revoke wording, got:\n%s", v)
		}
		if strings.Contains(v, "删除以下文件") || strings.Contains(v, "清理") {
			t.Fatalf("confirm screen must not promise a pre-enroll deletion anymore, got:\n%s", v)
		}
		_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if h.w.state != pwEnrolling {
			t.Fatalf("confirming must start the enroll, got %v", h.w.state)
		}
		pwStep(t, h.w, cmd)
		if got := h.sess.order(); !reflect.DeepEqual(got, []string{"Enroll"}) {
			t.Fatalf("force must NOT clean before enroll (零清理先行), got %v", got)
		}
		if h.w.state != pwWaiting {
			t.Fatalf("enroll success must land in pwWaiting, got %v", h.w.state)
		}
	})

	// force×discovery 组合钉:发现流(一级选「局域网发现」)+ Force 预填的完整
	// 调用序必须是 [Bind, Enroll](确认屏恰落在 Bind 之后;Bind 只做校验,不做
	// 任何清理)。
	t.Run("discovery_combo_bind_then_enroll", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{Force: true})
		h.discoverRet = []clientops.Discovered{{Name: "nuc10", Addr: "192.0.2.5", SPKI: testPWPin(), TCPPort: 7878}}
		h.w.draft = &pwDraft{Mode: pwModeDiscover, Instance: "laptop"}
		pwStep(t, h.w, h.w.submitModeForm()) // 单 offer 自动选中 → Bind → 确认屏
		if h.w.state != pwEnrollForceConfirm {
			t.Fatalf("force×discovery must stop at the confirm screen after Bind, got %v", h.w.state)
		}
		if got := h.sess.order(); !reflect.DeepEqual(got, []string{"Bind"}) {
			t.Fatalf("the confirm screen must sit after Bind, got %v", got)
		}
		_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		pwStep(t, h.w, cmd)
		if got := h.sess.order(); !reflect.DeepEqual(got, []string{"Bind", "Enroll"}) {
			t.Fatalf("the combo order must be Bind → Enroll (no cleanup), got %v", got)
		}
		if h.w.state != pwWaiting {
			t.Fatalf("enroll success must land in pwWaiting, got %v", h.w.state)
		}
	})
}

// ---------------------------------------------------------------------------
// SAS 常显等待 + Finish 门(人闸)
// ---------------------------------------------------------------------------

func TestPairWizard_SASShownInWaitingBeforeApproval(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.driveURLToWaiting(t) // 尚无任何批准消息
	v := h.w.View().Content
	if !strings.Contains(v, "1 3 5 2 4 6") {
		t.Fatalf("SAS must be visible in pwWaiting WITHOUT approval (对照屏), got:\n%s", v)
	}
	if !strings.Contains(v, "剩余") {
		t.Fatalf("the ApprovalDeadline countdown must show in pwWaiting, got:\n%s", v)
	}
}

func TestPairWizard_FinishGate_NotCalledBeforeEnter(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.driveURLToWaiting(t)
	h.w.Update(pairApprovalDoneMsg{gen: h.w.gen}) // 批准到达
	if h.w.state != pwFinishGate {
		t.Fatalf("approval must open the finish gate, got %v", h.w.state)
	}
	for _, c := range h.sess.order() {
		if c == "Finish" || c == "WriteAndPull" {
			t.Fatalf("the gate must not have called %s before Enter: %v", c, h.sess.order())
		}
	}
	v := h.w.View().Content
	if !strings.Contains(v, "1  3  5  2  4  6") {
		t.Fatalf("the gate must re-show the SAS enlarged, got:\n%s", v)
	}
	_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if h.w.state != pwWritePull {
		t.Fatalf("Enter at the gate must enter the write phase, got %v", h.w.state)
	}
	pwStep(t, h.w, cmd)
	if got := h.sess.order(); !reflect.DeepEqual(got, []string{"Enroll", "Finish", "WriteAndPull"}) {
		t.Fatalf("call order must be Enroll → Finish → WriteAndPull (Finish only after Enter), got %v", got)
	}
	if h.w.state != pwDone {
		t.Fatalf("write success must land on the done screen, got %v", h.w.state)
	}
	v = h.w.View().Content
	for _, want := range []string{"laptop", "team-a", h.sess.artifact, "--instance"} {
		if !strings.Contains(v, want) {
			t.Fatalf("done view must contain %q (实例/授权 profile/产物/后续指引), got:\n%s", want, v)
		}
	}
}

// TestPairWizard_WaitingEscCancelsCtx:等待阶段 Esc 取消 ctx(fake 阻塞在
// ctx.Done 上,取消后立刻返回 context.Canceled);迟到 done 是旧 generation,
// 已关闭的向导不受污染。
func TestPairWizard_WaitingEscCancelsCtx(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.sess.cancelSeen = make(chan struct{})
	h.driveURLToWaiting(t)
	wcmd := h.w.pwWaitCmd() // 生产由 enrollDone 的 Batch 调度;测试直接取出执行
	waitDone := make(chan tea.Msg, 1)
	go func() { waitDone <- wcmd() }()

	_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if _, ok := cmd().(pairWizardClosedMsg); !ok {
		t.Fatalf("Esc in pwWaiting must close the wizard, got %T", cmd())
	}
	select {
	case <-h.sess.cancelSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("Esc in pwWaiting must cancel the WaitApproval ctx")
	}
	msg := <-waitDone
	done, ok := msg.(pairApprovalDoneMsg)
	if !ok {
		t.Fatalf("the wait cmd must report back, got %T", msg)
	}
	if !errors.Is(done.err, context.Canceled) {
		t.Fatalf("the canceled wait must report context.Canceled, got %v", done.err)
	}
	h.w.Update(msg) // 旧 generation 的迟到消息
	if h.w.state != pwClosed {
		t.Fatalf("a stale done must not revive a closed wizard, state=%v", h.w.state)
	}
}

// ---------------------------------------------------------------------------
// generation 防串扰 + tick 纪律
// ---------------------------------------------------------------------------

func TestPairWizard_GenerationStaleDrop(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.driveURLToWaiting(t)
	g0 := h.w.gen

	// 新鲜 tick 注入 note → 状态行更新。
	h.w.Update(wizardTickMsg{gen: g0, note: &clientops.PollNote{Pending: true, Detail: "waiting for the owner's approval"}})
	if !strings.Contains(h.w.note, "waiting") {
		t.Fatalf("a fresh tick note must update the status line, note=%q", h.w.note)
	}

	// 批准面走了 gone → pwEnded。
	h.w.Update(pairApprovalDoneMsg{gen: g0, err: clientops.ErrPairGone})
	if h.w.state != pwEnded || h.w.endReason != pwEndGone {
		t.Fatalf("gone must end the wizard run, state=%v reason=%v", h.w.state, h.w.endReason)
	}
	if v := h.w.View().Content; !strings.Contains(v, "本次申请已结束(被拒或过期)") {
		t.Fatalf("the gone screen must carry the merged-410 wording, got:\n%s", v)
	}

	// r → 新 generation、新会话、重走 enroll。
	_, cmd := h.w.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if h.w.state != pwEnrolling || h.w.gen != g0+1 {
		t.Fatalf("retry must re-drive on a NEW generation, state=%v gen=%d", h.w.state, h.w.gen)
	}
	if len(h.newed) != 2 {
		t.Fatalf("retry must construct a NEW PairSession (fresh id per run), newed=%d", len(h.newed))
	}

	// 旧 generation 的 done/tick 一律丢弃。
	h.w.Update(pairApprovalDoneMsg{gen: g0, err: nil})
	if h.w.state != pwEnrolling {
		t.Fatalf("a stale approvalDone must be dropped, state=%v", h.w.state)
	}
	h.w.Update(wizardTickMsg{gen: g0, note: &clientops.PollNote{Detail: "STALE"}})
	if strings.Contains(h.w.note, "STALE") {
		t.Fatalf("a stale tick note must be dropped, note=%q", h.w.note)
	}

	// 新 generation 走通:enrollDone → waiting,渲染新会话的 SAS。
	h.sess.sas = "654321"
	pwStep(t, h.w, cmd)
	if h.w.state != pwWaiting {
		t.Fatalf("the retried enroll must land in pwWaiting, got %v", h.w.state)
	}
	if v := h.w.View().Content; !strings.Contains(v, "6 5 4 3 2 1") {
		t.Fatalf("the retry must render the NEW session's SAS, got:\n%s", v)
	}
}

func TestPairWizard_TickOnlyReschedulesInWaiting(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.driveURLToWaiting(t)
	if _, cmd := h.w.Update(wizardTickMsg{gen: h.w.gen + 7}); cmd != nil {
		t.Fatal("a stale tick must be dropped (no reschedule)")
	}
	if _, cmd := h.w.Update(wizardTickMsg{gen: h.w.gen}); cmd == nil {
		t.Fatal("pwWaiting must reschedule the tick")
	}
	h.w.Update(pairApprovalDoneMsg{gen: h.w.gen})
	if h.w.state != pwFinishGate {
		t.Fatalf("precondition: gate, got %v", h.w.state)
	}
	if _, cmd := h.w.Update(wizardTickMsg{gen: h.w.gen}); cmd != nil {
		t.Fatal("the gate must NOT reschedule the tick (tick 仅在对应状态续排)")
	}
}

func TestPWHarvestNote(t *testing.T) {
	ch := make(chan clientops.PollNote, 2)
	if pwHarvestNote(ch) != nil {
		t.Fatal("an empty channel must harvest nothing")
	}
	ch <- clientops.PollNote{Backoff: true, Detail: "pairing poll: HTTP 429 (retrying)"}
	n := pwHarvestNote(ch)
	if n == nil || !n.Backoff {
		t.Fatalf("a queued note must harvest, got %+v", n)
	}
	if !strings.Contains(pwNoteText(*n), "429") {
		t.Fatalf("the note text must carry the poll detail, got %q", pwNoteText(*n))
	}
}

// ---------------------------------------------------------------------------
// 写入期不可取消 + 三态结果屏
// ---------------------------------------------------------------------------

func TestPairWizard_WritePullEscDisabled(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.driveURLToWaiting(t)
	h.w.Update(pairApprovalDoneMsg{gen: h.w.gen})
	_, wcmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // gate → write cmd(未执行)
	if h.w.state != pwWritePull {
		t.Fatalf("precondition: pwWritePull, got %v", h.w.state)
	}
	if v := h.w.View().Content; !strings.Contains(v, "写入中") {
		t.Fatalf("the write phase must say 写入中,请稍候, got:\n%s", v)
	}
	nm, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if nw := nm.(*pairWizard); nw.state != pwWritePull || cmd != nil {
		t.Fatalf("Esc during write must be a no-op (写入期不可取消), state=%v cmd=%v", nw.state, cmd)
	}
	pwStep(t, h.w, wcmd)
	if h.w.state != pwDone {
		t.Fatalf("write success must land on the done screen, got %v", h.w.state)
	}
}

func TestPairWizard_EndedReasonsAndRetry(t *testing.T) {
	cases := []struct {
		name      string
		waitErr   error
		want      pwEndReason
		wantTexts []string
	}{
		{"gone", clientops.ErrPairGone, pwEndGone, []string{"本次申请已结束(被拒或过期)", "[r]"}},
		{"timeout", clientops.ErrPairTimeout, pwEndTimeout, []string{"超时", "[r]"}},
		{"error", errors.New("boom"), pwEndError, []string{"boom", "[r]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPWHarness(t, PairWizardPrefill{})
			h.sess.waitErr = tc.waitErr
			h.driveURLToWaiting(t)
			h.runWait(t)
			if h.w.state != pwEnded || h.w.endReason != tc.want {
				t.Fatalf("state=%v reason=%v, want %v", h.w.state, h.w.endReason, tc.want)
			}
			v := h.w.View().Content
			for _, want := range tc.wantTexts {
				if !strings.Contains(v, want) {
					t.Fatalf("ended view must contain %q, got:\n%s", want, v)
				}
			}
			_, cmd := h.w.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
			if h.w.state != pwEnrolling || len(h.newed) != 2 {
				t.Fatalf("r must re-drive with the same opts on a new session, state=%v newed=%d", h.w.state, len(h.newed))
			}
			// R1 发现 6:重试必须是同参数重驱(PairOpts 可比较,逐值相等)。
			if h.newed[1] != h.newed[0] {
				t.Fatalf("retry must reuse the SAME opts, got %+v vs %+v", h.newed[0], h.newed[1])
			}
			pwStep(t, h.w, cmd)
			if h.w.state != pwWaiting {
				t.Fatalf("the retried run must reach pwWaiting again, got %v", h.w.state)
			}
		})
	}
}

// TestPairWizard_WritePullErrorOffersRetry:首拉失败落 pwEnded(error),
// `r` 可重新申请(plan:pwEnded 三态均给 r)。
func TestPairWizard_WritePullErrorOffersRetry(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.sess.writeErr = errors.New("first pull failed")
	h.driveURLToWaiting(t)
	h.w.Update(pairApprovalDoneMsg{gen: h.w.gen})
	_, wcmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pwStep(t, h.w, wcmd)
	if h.w.state != pwEnded || h.w.endReason != pwEndError {
		t.Fatalf("write failure must land on the ended screen, state=%v reason=%v", h.w.state, h.w.endReason)
	}
	if _, cmd := h.w.Update(tea.KeyPressMsg{Code: 'r', Text: "r"}); cmd == nil || h.w.state != pwEnrolling || len(h.newed) != 2 {
		t.Fatalf("r after a write error must re-drive, state=%v cmd=%v newed=%d", h.w.state, cmd, len(h.newed))
	}
}

// TestPairWizard_DoneClosesWithInstance:成功收尾必须把新实例交给父模型
// (T3 换槽+刷新的消息载体)。
func TestPairWizard_DoneClosesWithInstance(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.driveURLToWaiting(t)
	h.w.Update(pairApprovalDoneMsg{gen: h.w.gen})
	_, wcmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pwStep(t, h.w, wcmd)
	_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if h.w.state != pwClosed {
		t.Fatalf("done close must park the wizard, state=%v", h.w.state)
	}
	done, ok := cmd().(pairWizardDoneMsg)
	if !ok {
		t.Fatalf("success close must emit pairWizardDoneMsg, got %T", cmd())
	}
	if done.instance != "laptop" {
		t.Fatalf("the done msg must carry the new instance, got %q", done.instance)
	}
}
