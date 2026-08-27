package tui

// Plan 40 批2 T8 §4/§6/§11.11/§11.13: 连接表单实例字段 + 规范名前置校验三连 +
// 换码静态警告。驱动形态照 clientpage_test.go TestEditConnFormRequiresCodeWhenNoToken
// 的 formOverlay 提交路径；用户输入以真实键击注入（huh.Validate 层在路径上）。
// seed 形态沿用 instancepicker_test.go / clientpage_instance_test.go 既有 helper。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/clientops"
)

var formGoodPin = "sha256:" + strings.Repeat("a", 64)

// seedFormSlot redirects os.UserConfigDir (isolating every cache path AND
// clearing both single-slot override envs) and creates slot <name> ("": the
// default slot dir itself) holding a cache.meta.json that names device X plus
// an optional cache.bin placeholder.
func seedFormSlot(t *testing.T, name string, withBin bool) string {
	t.Helper()
	base := isolatedConfigDir(t)
	dir := base
	if name != "" {
		dir = filepath.Join(base, "instances", name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"url":"https://s","pulled_at":1,"device_name":"X"}`
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if withBin {
		if err := os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("placeholder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// openEditConnForm builds the edit-connection overlay and initializes it (the
// first field is focused, ready to receive keystrokes). huh travels its own
// focus setup through commands, so the Init cmd's message is pumped back in —
// the same shape huh's own test suite uses (batchUpdate).
func openEditConnForm(t *testing.T, m clientModel) *formOverlay {
	t.Helper()
	ov := m.editConnForm()
	fo, ok := ov.(*formOverlay)
	if !ok {
		t.Fatalf("editConnForm must return a formOverlay, got %T", ov)
	}
	pump(fo, func() tea.Msg { return fo.Init() })
	return fo
}

var keyEnter = tea.KeyPressMsg{Code: tea.KeyEnter}

// pump delivers ONE logical input to the overlay and keeps feeding any
// resulting command's message back in (bounded): huh's field navigation rides
// on command round-trips like nextFieldMsg, so a bare Update would strand the
// cursor on the first field forever. A produced formDoneMsg ends the round;
// the SUBMIT action's own result is returned to the caller.
func pump(fo *formOverlay, seed tea.Cmd) tea.Msg {
	msg := seed()
	for i := 0; i < 8 && msg != nil; i++ {
		switch msg.(type) {
		case formDoneMsg:
			return runSubmitAction(msg.(formDoneMsg))
		case cursor.BlinkMsg:
			return nil // never wait out the blink cmd() in tests (seconds each)
		}
		_, cmd := fo.Update(msg)
		if cmd == nil {
			return nil
		}
		msg = cmd()
	}
	return nil
}

func runSubmitAction(fm formDoneMsg) tea.Msg { return fm.after() }

// sendKey is pump with a raw message (a single keystroke round).
func sendKey(fo *formOverlay, msg tea.Msg) tea.Msg {
	return pump(fo, func() tea.Msg { return msg })
}

// typeInto feeds text as rune keystrokes into the form's focused field.
func typeInto(fo *formOverlay, text string) {
	for _, r := range text {
		fo.Update(tea.KeyPressMsg{Text: string(r), Code: r})
	}
}

// clearFocused backspaces away any prefilled value of the focused field
// (bounded; deleting from an already-empty field is a no-op).
func clearFocused(fo *formOverlay) {
	for i := 0; i < 80; i++ {
		fo.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
}

// submitForm presses Enter until the huh form completes or the budget runs
// out; it returns what the overlay's SUBMIT action produced. completed=false
// means a field Validate kept refusing — the action never ran.
func submitForm(fo *formOverlay) (res tea.Msg, completed bool) {
	for i := 0; i < 12 && res == nil; i++ {
		res = sendKey(fo, keyEnter)
	}
	return res, res != nil
}

func TestEditConnForm_InstanceFieldValidation(t *testing.T) {
	mkInstanceDir(t, "agentA")
	m := newClientModelForGate(t)
	m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "tok", Pin: formGoodPin}

	fo := openEditConnForm(t, m)
	sendKey(fo, keyEnter)     // serve 地址 prefill ok → 实例名
	clearFocused(fo)          // instance prefill (= selected) is already ""
	typeInto(fo, "bad name!") // space + '!' never match ^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$
	res, completed := submitForm(fo)
	if completed || res != nil {
		t.Fatalf(`huh.Validate must refuse "bad name!" at the instance field — completed=%v result=%T(%v)`, completed, res, res)
	}

	// Sanity (guards the DRIVER, not the product): the same keystroke path
	// with a fresh VALID name must reach the old write path's save message.
	fo2 := openEditConnForm(t, m)
	sendKey(fo2, keyEnter)  // → 实例名 (prefill "")
	typeInto(fo2, "agentZ") // a brand-new name routes cross-slot…
	sendKey(fo2, keyEnter)  // valid name → 设备码
	typeInto(fo2, "c9")     // …so a code is required by rule 1
	res2, completed2 := submitForm(fo2)
	if !completed2 {
		t.Fatal("sanity driver run: valid values must complete the form (driver regression)")
	}
	if sm, ok := res2.(clientStatusMsg); !ok || string(sm) != "连接配置已保存" {
		t.Fatalf("valid routing must fall through to the panel save message, got %T (%v)", res2, res2)
	}
}

func TestFormRules_CanonicalAndCross(t *testing.T) {
	rule1Copy := "设备码不能为空——跨实例路由或本槽无已保存设备码时不存在\"保持不变\""

	t.Run("rule2: typed value folding onto ANOTHER existing instance refuses", func(t *testing.T) {
		mkInstanceDir(t, "agentB", "zeta")
		m := newClientModelForGate(t)
		m.instance = "zeta" // SELECTED slot ≠ the fold target typed below
		m.wizard = true     // refusal happens before any write either way
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "tok", Pin: formGoodPin}

		fo := openEditConnForm(t, m)
		sendKey(fo, keyEnter)  // serve 地址 prefill ok → 实例名
		clearFocused(fo)       // wipe the "zeta" prefill
		typeInto(fo, "AGENTB") // casefolds onto the EXISTING agentB dir
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("values pass every field Validate, so the form must complete (driver broke?)")
		}
		want := "实例名与已存在实例 agentB 冲突——对其换码请先 [i] 切换到该实例（跨槽路由被拒绝）"
		em, ok := res.(errMsg)
		if !ok {
			t.Fatalf("want rule2 errMsg, got %T (%v)", res, res)
		}
		if em.err.Error() != want {
			t.Fatalf("rule2 copy mismatch:\nwant %q\ngot  %q", want, em.err.Error())
		}
	})

	t.Run("rule3: the same slot typed in another case is allowed (canonical)", func(t *testing.T) {
		seedFormSlot(t, "AGENTA", true) // dir name UPPERCASE on disk
		m := newClientModelForGate(t)
		m.instance = "AGENTA"
		m.wizard = true // an allowed pass returns connSavedMsg carrying the draft
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "keep-tok", Pin: formGoodPin}

		fo := openEditConnForm(t, m)
		sendKey(fo, keyEnter) // → 实例名
		clearFocused(fo)      // wipe the "AGENTA" prefill
		typeInto(fo, "agenta")
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("typing the SAME slot in another case must be allowed; the form did not complete")
		}
		sm, ok := res.(connSavedMsg)
		if !ok {
			t.Fatalf("want connSavedMsg on the allowed path, got %T (%v)", res, res)
		}
		// The canonical fold drives the ACTION's routing decision (target/
		// sameSlot); the draft keeps the user's typed text for T9's write-path
		// to re-canonicalize. The allow itself is the behavior lock: a missed
		// fold would have made this a cross-slot route and hit rule 2/1.
		if !strings.EqualFold(sm.draft.Instance, "AGENTA") {
			t.Fatalf("same-slot casefold must allow slot AGENTA, got %q", sm.draft.Instance)
		}
		if sm.cred.Token != "keep-tok" {
			t.Fatalf("same-slot allow with empty code must KEEP the stored token, got %q", sm.cred.Token)
		}
	})

	t.Run("rule1: brand-new cross-slot name with an empty code refuses", func(t *testing.T) {
		mkInstanceDir(t, "A")
		m := newClientModelForGate(t)
		m.instance = "A"
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "tok", Pin: formGoodPin}

		fo := openEditConnForm(t, m)
		sendKey(fo, keyEnter)  // → 实例名
		clearFocused(fo)       // wipe the "A" prefill
		typeInto(fo, "agentC") // a NEW name — matches no on-disk dir
		// 设备码 left empty on purpose: a cross-slot route has nothing to "keep"
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("values pass every field Validate, so the form must complete (driver broke?)")
		}
		em, ok := res.(errMsg)
		if !ok {
			t.Fatalf("want rule1 errMsg, got %T (%v)", res, res)
		}
		if em.err.Error() != rule1Copy {
			t.Fatalf("rule1 copy mismatch:\nwant %q\ngot  %q", rule1Copy, em.err.Error())
		}
	})

	t.Run("rule1: selected slot WITHOUT stored auth requires a code even same-slot", func(t *testing.T) {
		mkInstanceDir(t, "bare") // directory exists, zero material inside
		m := newClientModelForGate(t)
		m.instance = "bare"
		// NO m.cred — the slot holds no auth.json, so there is nothing to keep,
		// which also means the serve 地址/pin fields carry no prefill.
		fo := openEditConnForm(t, m)
		typeInto(fo, "https://s.example")
		sendKey(fo, keyEnter) // → 实例名 (prefill "bare", kept)
		sendKey(fo, keyEnter) // empty name is legal → 设备码
		sendKey(fo, keyEnter) // empty code stays (nothing to keep — that IS the case) → pin
		typeInto(fo, formGoodPin)
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("values pass every field Validate, so the form must complete (driver broke?)")
		}
		em, ok := res.(errMsg)
		if !ok {
			t.Fatalf("want rule1 errMsg, got %T (%v)", res, res)
		}
		if em.err.Error() != rule1Copy {
			t.Fatalf("rule1 copy mismatch:\nwant %q\ngot  %q", rule1Copy, em.err.Error())
		}
	})

	t.Run("rule3: empty field on an authed selected slot KEEPS the token", func(t *testing.T) {
		mkInstanceDir(t, "A")
		m := newClientModelForGate(t)
		m.instance = "A"
		m.wizard = true
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "tok0", Pin: formGoodPin}

		fo := openEditConnForm(t, m)
		// everything stays as-is: 实例名 keeps the "A" prefill, 设备码 stays empty
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("plain keep-everything submit must complete")
		}
		sm, ok := res.(connSavedMsg)
		if !ok {
			t.Fatalf("want connSavedMsg, got %T (%v)", res, res)
		}
		if sm.draft.Instance != "A" || sm.draft.Code != "" {
			t.Fatalf("keep semantics must preserve slot A with an empty code, got %+v", sm.draft)
		}
		if sm.cred.Token != "tok0" {
			t.Fatalf("empty code must keep the stored token, got %q", sm.cred.Token)
		}
	})
}

func TestFormRules_PanelVacuumEmptyField(t *testing.T) {
	t.Run("panel mode + true vacuum refuses with the walkthrough copy", func(t *testing.T) {
		isolatedConfigDir(t) // default slot holds NONE of the four marker files
		m := newClientModelForGate(t)
		// model-side cred with a DISK-side four-file vacuum: exactly the half-
		// dead state the guard exists for (rule 1 passes — there IS a token to
		// "keep" — so the refusal must come from the vacuum guard).
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "stale", Pin: formGoodPin}
		fo := openEditConnForm(t, m)
		// 实例名 stays "" (default), 设备码 stays "" (claimed keep)
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("values pass every field Validate, so the form must complete (driver broke?)")
		}
		want := "默认实例无材料——首次 enroll 请走向导流程（自动归位），或填实例名显式路由"
		em, ok := res.(errMsg)
		if !ok {
			t.Fatalf("want vacuum-guard errMsg, got %T (%v)", res, res)
		}
		if em.err.Error() != want {
			t.Fatalf("panel vacuum copy mismatch:\nwant %q\ngot  %q", want, em.err.Error())
		}
	})

	t.Run("env single-slot mode names the override env instead", func(t *testing.T) {
		mkInstanceDir(t, "agentA")
		t.Setenv("SSHMGR_CACHE_DIR", t.TempDir()) // AFTER isolation: full single-slot override
		m := newClientModelForGate(t)
		// Defensive injection: under single-slot mode the field itself is
		// HIDDEN, so a non-empty target can only be simulated through the
		// session slot — precisely what the mutual-exclusion guard protects.
		m.instance = "agentA"
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "stale", Pin: formGoodPin}
		fo := openEditConnForm(t, m)
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("values pass every field Validate, so the form must complete (driver broke?)")
		}
		want := "--instance and SSHMGR_CACHE_DIR are mutually exclusive — unset the env or clear the 实例名 field"
		em, ok := res.(errMsg)
		if !ok {
			t.Fatalf("want single-slot exclusion errMsg, got %T (%v)", res, res)
		}
		if em.err.Error() != want {
			t.Fatalf("single-slot copy mismatch:\nwant %q\ngot  %q", want, em.err.Error())
		}
	})

	t.Run("review F1: env single-slot + true default-slot vacuum refuses with the override copy", func(t *testing.T) {
		isolatedConfigDir(t) // default slot (routing unchanged by a DEK env) is a four-file vacuum
		t.Setenv("SSHMGR_CACHE_DEK", t.TempDir())
		m := newClientModelForGate(t)
		// model cred on a DISK-side vacuum: without this guard the submit
		// would silently rewrite cache.auth.json under single-slot semantics
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "stale", Pin: formGoodPin}
		fo := openEditConnForm(t, m) // single-slot: only url/code/pin fields exist
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("values pass every field Validate, so the form must complete (driver broke?)")
		}
		want := "override env（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK）覆盖中：单槽语义下无多实例路由，请清除 env 或按单槽使用"
		em, ok := res.(errMsg)
		if !ok {
			t.Fatalf("want single-slot vacuum errMsg, got %T (%v)", res, res)
		}
		if em.err.Error() != want {
			t.Fatalf("single-slot vacuum copy mismatch:\nwant %q\ngot  %q", want, em.err.Error())
		}
	})

	t.Run("review F1: env single-slot with material present does NOT trigger", func(t *testing.T) {
		base := isolatedConfigDir(t)
		if err := os.MkdirAll(base, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "cache.bin"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SSHMGR_CACHE_DEK", t.TempDir()) // routing unchanged → resolved slot holds material
		m := newClientModelForGate(t)
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "keep-me", Pin: formGoodPin}
		fo := openEditConnForm(t, m)
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("values pass every field Validate, so the form must complete (driver broke?)")
		}
		if _, isErr := res.(errMsg); isErr {
			t.Fatalf("non-vacuum resolved slot must NOT hit the vacuum guard, got errMsg: %v", res.(errMsg).err)
		}
		sm, ok := res.(clientStatusMsg)
		if !ok || string(sm) != "连接配置已保存" {
			t.Fatalf("expected the pre-existing save path to run silently, got %T (%v)", res, res)
		}
	})
}

// warningViewOf renders the edit-conn form view and strips line breaks. Note:
// in-view assertions below deliberately use PURE-CJK runs only — huh's width
// wrapping pads ASCII token boundaries with spaces ("cache.config." / "json"
// end up separated), so mixed-script fragments are not contiguous in the
// render. The verbatim sentence is pinned instead by the exact-copy test.
func warningViewOf(t *testing.T, m clientModel) string {
	t.Helper()
	return strings.ReplaceAll(openEditConnForm(t, m).View().Content, "\n", "")
}

func TestEditConnForm_WarningCopy(t *testing.T) {
	t.Run("bound default slot warns with the three-file list", func(t *testing.T) {
		seedFormSlot(t, "", true) // default slot WITH cache.bin + meta(device X)
		v := warningViewOf(t, newClientModelForGate(t))
		for _, frag := range []string{
			"已绑定设备 X",
			"更换设备码前须清三件套",
			"它们是默认槽意图标记",
			"字段填新实例名",
		} {
			if !strings.Contains(v, frag) {
				t.Fatalf("default-slot warning must contain %q:\n%s", frag, v)
			}
		}
	})

	t.Run("exact swapWarning copy (default + named slots)", func(t *testing.T) {
		seedFormSlot(t, "", true)
		wantDefault := "⚠ 默认实例已绑定设备 X——更换设备码前须清三件套（cache.auth.json + cache.bin + quarantine/，保留 cache.meta.json 与 cache.config.json——它们是默认槽意图标记，删了重 enroll 会被归位到实例槽）重 enroll，否则下次同步将被门禁拒绝；若是本机第二个 agent，请在\"实例名\"字段填新实例名。"
		if got := swapWarning("", false); got != wantDefault {
			t.Fatalf("default-slot copy mismatch:\nwant %q\ngot  %q", wantDefault, got)
		}
		seedFormSlot(t, "agentA", true)
		wantNamed := "⚠ 实例 agentA 已绑定设备 X——换码须删除该实例目录重 enroll，否则同步将被拒。"
		if got := swapWarning("agentA", false); got != wantNamed {
			t.Fatalf("named-slot copy mismatch:\nwant %q\ngot  %q", wantNamed, got)
		}
	})

	t.Run("named instance slot warns scoped to that instance", func(t *testing.T) {
		seedFormSlot(t, "agentA", true)
		m := newClientModelForGate(t)
		m.instance = "agentA"
		v := warningViewOf(t, m)
		if !strings.Contains(v, "实例 agentA 已绑定设备 X") {
			t.Fatalf("named-slot warning must contain %q:\n%s", "实例 agentA 已绑定设备 X", v)
		}
	})

	t.Run("slot without cache.bin carries no warning", func(t *testing.T) {
		seedFormSlot(t, "", false) // meta present, bin absent
		v := warningViewOf(t, newClientModelForGate(t))
		if strings.Contains(v, "已绑定") {
			t.Fatalf("bin-less slot must stay warning-free:\n%s", v)
		}
	})
}

// TestSwapWarning_UnregisteredDevice pins the fallback device label used when
// the bin exists but cache.meta.json carries no device_name.
func TestSwapWarning_UnregisteredDevice(t *testing.T) {
	seedFormSlot(t, "", true)
	_, _, metaPath, _, err := clientops.CachePathsFor("")
	if err != nil {
		t.Fatal(err)
	}
	meta := `{"url":"https://s","pulled_at":1}`
	if werr := os.WriteFile(metaPath, []byte(meta), 0o600); werr != nil {
		t.Fatal(werr)
	}
	got := swapWarning("", false)
	if !strings.Contains(got, "已绑定设备 (旧 cache 未登记)") {
		t.Fatalf("device-less meta must fall back to the unregistered label, got %q", got)
	}
}
