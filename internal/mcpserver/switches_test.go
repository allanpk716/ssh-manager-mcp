package mcpserver

import (
	"testing"
	"time"
)

// TestResolveSwitch_Precedence pins the frozen four-layer precedence
// (spec §3.1-8): explicit env ("true"/"false", exact match) > explicit flag >
// store ("true"/"false") > default. The first five rows are the brief's
// verbatim table; the rest harden the same boundaries.
func TestResolveSwitch_Precedence(t *testing.T) {
	cases := []struct {
		env          string
		flagCh, flag bool
		store        string
		def, want    bool
	}{
		{"false", true, true, "true", true, false},   // 显式 env 压一切
		{"", true, false, "true", true, false},       // 显式 flag=false 压 store
		{"", false, false, "false", true, false},     // store 压缺省
		{"", false, false, "", true, true},           // 缺省
		{"garbage", false, false, "", false, false},  // env 非法当未设→store空→def=false
		{"true", false, false, "false", false, true}, // env true 压 store false
		{"", true, true, "false", false, true},       // 显式 flag=true 压 store false
		{"false", true, true, "", false, false},      // env=false 显式关,即使全下层的 def=false
	}
	for i, c := range cases {
		if got := ResolveSwitch(c.env, c.flagCh, c.flag, c.store, c.def); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

// TestEnvSwitch pins the env-seam tri-state parse: only exact "true"/"false"
// participate; unset/empty/garbage read as nil (not explicitly set) so they
// defer to the next precedence layer. This is the RefreshSwitches injection
// shape — Task 6 feeds CLI flags as the same *bool type.
func TestEnvSwitch(t *testing.T) {
	if got := envSwitch("SSHMGR_TEST_SWITCH_ABSENT"); got != nil {
		t.Fatalf("unset env: got %v want nil", *got)
	}
	t.Setenv("SSHMGR_TEST_SWITCH", "true")
	if got := envSwitch("SSHMGR_TEST_SWITCH"); got == nil || !*got {
		t.Fatalf(`"true": got %v want true`, got)
	}
	t.Setenv("SSHMGR_TEST_SWITCH", "false")
	if got := envSwitch("SSHMGR_TEST_SWITCH"); got == nil || *got {
		t.Fatalf(`"false": got %v want false`, got)
	}
	for _, garbage := range []string{"", "garbage", "TRUE", "1", " true"} {
		t.Setenv("SSHMGR_TEST_SWITCH", garbage)
		if got := envSwitch("SSHMGR_TEST_SWITCH"); got != nil {
			t.Fatalf("%q: got %v want nil (exact match only)", garbage, *got)
		}
	}
}

// TestServeRunner_SwitchesDefaultStoreAndTTL walks the runner-level resolve:
// defaults ON with nothing set anywhere → store=false respected only after the
// memo expires → the two switches resolve independently.
func TestServeRunner_SwitchesDefaultStoreAndTTL(t *testing.T) {
	st := newTestStore(t)
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	// 缺省 true: no env injected, no store rows, no flags.
	if !r.PairingEnabled() || !r.DiscoveryEnabled() {
		t.Fatal("both switches must default ON with nothing set")
	}
	// Store turns pairing off — but the ≤5s memo still answers the old value.
	if err := st.SetSetting(settingPairing, "false"); err != nil {
		t.Fatal(err)
	}
	if !r.PairingEnabled() {
		t.Fatal("cache must memoize the previous resolve within TTL")
	}
	// Force-expire the memo (white-box backdate) → rebuild sees store=false.
	r.switches.Store(&switchCache{at: time.Now().Add(-switchTTL - time.Second)})
	if r.PairingEnabled() {
		t.Fatal("after TTL expiry the store=false setting must disable pairing")
	}
	// Independence: discovery untouched by the pairing setting (still memoized default).
	if !r.DiscoveryEnabled() {
		t.Fatal("discovery must resolve independently of the pairing setting")
	}
	st.SetSetting(settingDiscovery, "false")
	r.switches.Store(&switchCache{at: time.Now().Add(-switchTTL - time.Second)})
	if r.DiscoveryEnabled() {
		t.Fatal("store=false must disable discovery after rebuild")
	}
}

// TestServeRunner_RefreshSwitchesInjection pins the injection contract: the
// *bool inputs (nil = not explicitly set) participate immediately in the
// resolve, and the four layers stack per ResolveSwitch precedence.
func TestServeRunner_RefreshSwitchesInjection(t *testing.T) {
	st := newTestStore(t)
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	tv, fv := true, false

	// store=false + explicit env=true → env wins, effective immediately.
	if err := st.SetSetting(settingPairing, "false"); err != nil {
		t.Fatal(err)
	}
	r.RefreshSwitches(&tv, nil, nil, nil)
	if !r.PairingEnabled() {
		t.Fatal("explicit env=true must beat store=false")
	}
	// explicit env=false (no store row for the env layer to fight) → off.
	r.RefreshSwitches(&fv, nil, nil, nil)
	if r.PairingEnabled() {
		t.Fatal("explicit env=false must disable pairing")
	}
	// explicit flag=false beats store=true.
	if err := st.SetSetting(settingPairing, "true"); err != nil {
		t.Fatal(err)
	}
	r.RefreshSwitches(nil, &fv, nil, nil)
	if r.PairingEnabled() {
		t.Fatal("explicit flag=false must beat store=true")
	}
	// All layers unset → back to the store value.
	r.RefreshSwitches(nil, nil, nil, nil)
	if !r.PairingEnabled() {
		t.Fatal("with env/flag unset the store=true setting must enable pairing")
	}
	// Discovery injection is independent: flag=false flips only discovery.
	r.RefreshSwitches(nil, nil, nil, &fv)
	if !r.PairingEnabled() || r.DiscoveryEnabled() {
		t.Fatal("discovery flag injection must not disturb pairing")
	}
}
