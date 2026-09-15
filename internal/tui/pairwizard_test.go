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

// driveURLToWaiting 以直连(URL+pin)路径把向导推进到 pwWaiting(enroll 已完成;
// WaitApproval 命令未执行——等待中的用例按需自行取出执行)。
func (h *pwHarness) driveURLToWaiting(t *testing.T) {
	t.Helper()
	h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
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
	if err := pwValidateURL(""); err != nil {
		t.Fatalf("空地址 = 发现流,必须放行: %v", err)
	}
	if err := pwValidateURL("htts://192.0.2.5:7878"); err == nil {
		t.Fatal("非 https 地址必须被拒")
	}
	if err := pwValidateURL("https://192.0.2.5:7878"); err != nil {
		t.Fatalf("legal URL rejected: %v", err)
	}
	if err := pwValidatePin(""); err != nil {
		t.Fatalf("空 pin 必须放行(直连场景由会话校验给冻结 TOFU 文案): %v", err)
	}
	if err := pwValidatePin("nope"); err == nil {
		t.Fatal("畸形 pin 必须被拒")
	}
	if err := pwValidatePin(testPWPin()); err != nil {
		t.Fatalf("legal pin rejected: %v", err)
	}
}

func TestPairWizard_SubmitRejectsIllegalInstance(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.w.draft = &pwDraft{Instance: "bad/name", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	h.w.submitForm() // 返回值 = 表单复位命令(R1 发现 1),不是启动命令
	if h.w.state != pwForm || h.w.err == nil {
		t.Fatalf("refusal must stay on the form with an error, state=%v err=%v", h.w.state, h.w.err)
	}
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("refusal must reset the completed form (StateCompleted = dead input), state=%v", h.w.form.State)
	}
	if len(h.newed) != 0 {
		t.Fatalf("an illegal instance name must never reach session construction, newed=%d", len(h.newed))
	}
}

// TestPairWizard_SubmitEnrolledNeedsForce pins the form-side 已装判定:enrolled
// 且未 force → 拒绝(先于任何会话构建);force → 在任何清理之前先过确认屏。
func TestPairWizard_SubmitEnrolledNeedsForce(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.w.isEnrolled = func(string) (bool, error) { return true, nil }
	h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	h.w.submitForm() // 拒绝;返回值 = 表单复位命令
	if h.w.state != pwForm || h.w.err == nil || !strings.Contains(h.w.err.Error(), "force") {
		t.Fatalf("refusal must stay on the form and mention force, state=%v err=%v", h.w.state, h.w.err)
	}
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("refusal must reset the completed form, state=%v", h.w.form.State)
	}
	if len(h.newed) != 0 {
		t.Fatalf("the enrolled gate must precede session construction, newed=%d", len(h.newed))
	}

	// force → 确认屏(New 之后、Enroll 之前:确认屏上任何会话方法都未调用)。
	h2 := newPWHarness(t, PairWizardPrefill{Force: true})
	h2.w.isEnrolled = func(string) (bool, error) { return true, nil }
	h2.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	if cmd := h2.w.submitForm(); cmd != nil {
		t.Fatal("force path must stop at the confirm screen, not start anything")
	}
	if h2.w.state != pwEnrollForceConfirm {
		t.Fatalf("force submit must land on the confirm screen, got %v", h2.w.state)
	}
	if calls := h2.sess.order(); len(calls) != 0 {
		t.Fatalf("confirm screen must precede ANY session call, got %v", calls)
	}
}

func TestPairWizard_NewSessionErrShowsOnForm(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.newSessionErr = errors.New("refusing TOFU pairing without --pin")
	h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878"} // pin 空 → 会话校验拒 TOFU
	h.w.submitForm()                                                        // 返回值 = 表单复位命令
	if h.w.state != pwForm || h.w.err == nil {
		t.Fatalf("session error must surface on the form, state=%v err=%v", h.w.state, h.w.err)
	}
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("session error must reset the completed form, state=%v", h.w.form.State)
	}
}

func TestPairWizard_FormEscCloses(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	_, cmd := h.w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if _, ok := cmd().(pairWizardClosedMsg); !ok {
		t.Fatalf("Esc on the form must close the wizard, got %T", cmd())
	}
	if h.w.state != pwClosed {
		t.Fatalf("closed wizard state=%v, want pwClosed", h.w.state)
	}
}

// TestPairWizard_FormRefusalResetsCompletedForm(评审 R1 发现 1 的回归钉):
// huh 的 Form.Update 在 State != StateNormal 时短路——提交失败留在 pwForm 必须
// 重建表单(draft 指针共享保值),否则用户无法再输入,且 pwForm 下任意非 Esc
// 按键都会命中 Completed 分支重跑 submitForm(发现落空屏每键重发 LAN sweep)。
func TestPairWizard_FormRefusalResetsCompletedForm(t *testing.T) {
	h := newPWHarness(t, PairWizardPrefill{})
	h.w.isEnrolled = func(string) (bool, error) { return true, nil }
	h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	h.w.submitForm() // 已配对且未 force → 拒绝
	if h.w.form.State != huh.StateNormal {
		t.Fatalf("refusal must rebuild the form to StateNormal, got %v", h.w.form.State)
	}
	if h.w.draft.Instance != "laptop" || h.w.draft.URL != "https://192.0.2.5:7878" {
		t.Fatalf("the rebuilt form must share the draft (values retained), got %+v", h.w.draft)
	}
	// 复位后的表单上任意按键 = 普通输入,不得重跑 submitForm(无重提交环)。
	before := len(h.newed)
	h.w.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if len(h.newed) != before {
		t.Fatal("a keypress on the refused form must not re-drive submitForm")
	}
	if h.w.state != pwForm {
		t.Fatalf("a stray keypress must stay on the form, got %v", h.w.state)
	}
	// 用户改主意(解除已装闸门)后可再次提交:流程正常推进。
	h.w.isEnrolled = func(string) (bool, error) { return false, nil }
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
	h.w.draft = &pwDraft{Instance: "laptop"} // 无地址 = 发现流
	cmd := h.w.submitForm()
	if h.w.state != pwDiscovering {
		t.Fatalf("URL-less submit must enter pwDiscovering, got %v", h.w.state)
	}
	h.w.Update(cmd()) // discover seam 返回空
	if h.w.state != pwForm {
		t.Fatalf("empty discovery must return to the form, got %v", h.w.state)
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
	h.w.draft = &pwDraft{Instance: "laptop"}
	pwStep(t, h.w, h.w.submitForm())
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
	h.w.draft = &pwDraft{Instance: "laptop"}
	pwStep(t, h.w, h.w.submitForm())
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
		h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		h.w.submitForm() // → pwEnrollForceConfirm
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
		h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
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
		h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
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
	h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
	h.w.submitForm() // → pwEnrollForceConfirm
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
		h.w.draft = &pwDraft{Instance: "laptop", URL: "https://192.0.2.5:7878", Pin: testPWPin()}
		h.w.submitForm() // → pwEnrollForceConfirm
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

	// force×discovery 组合钉:发现流(URL 空)+ Force 预填的完整调用序必须是
	// [Bind, Enroll](确认屏恰落在 Bind 之后;Bind 只做校验,不做任何清理)。
	t.Run("discovery_combo_bind_then_enroll", func(t *testing.T) {
		h := newPWHarness(t, PairWizardPrefill{Force: true})
		h.discoverRet = []clientops.Discovered{{Name: "nuc10", Addr: "192.0.2.5", SPKI: testPWPin(), TCPPort: 7878}}
		h.w.draft = &pwDraft{Instance: "laptop"} // 无地址 = 发现流
		pwStep(t, h.w, h.w.submitForm())         // 单 offer 自动选中 → Bind → 确认屏
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
