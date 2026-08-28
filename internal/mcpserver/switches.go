package mcpserver

import (
	"os"
	"time"

	"ssh-manager-mcp/internal/store"
)

// Switch is the tri-state vocabulary for a serve-side feature switch: unset
// (not explicitly set anywhere), on, off. ResolveSwitch collapses it into a
// plain bool at the last step.
type Switch int

const (
	SwitchUnset Switch = iota
	SwitchOn
	SwitchOff
)

// Store keys holding the persisted switch values (written by the 批2 Settings
// surface, same pattern will add serve.web). Frozen names — cross-batch
// contract with the Settings writer and anything reading the vault directly.
const (
	settingPairing   = "serve.pairing"
	settingDiscovery = "serve.discovery"
)

// switchTTL bounds how stale a memoized switch value may be: the spec caps
// serve-side switch re-evaluation at ≤5s (§3.1-7) — a Settings-page flip must
// take effect without a serve restart, while per-request/per-packet gating
// stays off the store-read hot path.
const switchTTL = 5 * time.Second

// Both switches default ON (spec §3.1-8 缺省 true): silence never silently
// disables a surface — operators opt out explicitly via env/flag/store.
const (
	defaultPairing   = true
	defaultDiscovery = true
)

// parseSwitch maps a switch value to the tri-state: only the exact strings
// "true"/"false" count as explicit; anything else (absent, empty, garbage) is
// SwitchUnset and defers to the next precedence layer.
func parseSwitch(v string) Switch {
	switch v {
	case "true":
		return SwitchOn
	case "false":
		return SwitchOff
	}
	return SwitchUnset
}

// ResolveSwitch is the frozen four-layer precedence for serve switches:
// explicit env ("true"/"false", exact match) > explicit flag > store
// ("true"/"false") > def. flagChanged carries the flag's explicitness (cobra
// Flags().Changed); flagVal is only consulted when flagChanged. Pure — all
// inputs passed in, trivially testable.
func ResolveSwitch(envVal string, flagChanged bool, flagVal bool, storeVal string, def bool) bool {
	if sw := parseSwitch(envVal); sw != SwitchUnset {
		return sw == SwitchOn
	}
	if flagChanged {
		return flagVal
	}
	if sw := parseSwitch(storeVal); sw != SwitchUnset {
		return sw == SwitchOn
	}
	return def
}

// envSwitch resolves one env seam (SSHMGR_SERVE_PAIRING / SSHMGR_SERVE_DISCOVERY)
// to the injected tri-state: nil = not explicitly set (unset/empty/garbage),
// else the parsed value. The *bool shape is the RefreshSwitches injection
// contract — Task 6 feeds CLI flags as the same type (Flags().Changed + value).
func envSwitch(name string) *bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	switch parseSwitch(v) {
	case SwitchOn:
		b := true
		return &b
	case SwitchOff:
		b := false
		return &b
	}
	return nil
}

// switchInputs carries the injected explicit env/flag inputs; nil = the layer
// did not explicitly set that switch, so it defers to the next layer.
type switchInputs struct {
	envPairing, flagPairing     *bool
	envDiscovery, flagDiscovery *bool
}

// switchCache memoizes one resolve pass. Held on ServeRunner via
// atomic.Pointer: readers (HTTP route gating, UDP discovery gating) never
// take a lock, and a rebuild race is benign — two goroutines may rebuild
// concurrently and both compute the same answer from the same inputs.
type switchCache struct {
	at                 time.Time
	pairing, discovery bool
}

// switchString maps an injected tri-state back to the string form
// ResolveSwitch parses (nil → "", so the layer reads as unset).
func switchString(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "true"
	default:
		return "false"
	}
}

// RefreshSwitches injects the explicitly-set env/flag inputs (nil = not set —
// the caller owns reading the env seams and the flags' Changed() explicitness)
// and rebuilds the cache immediately, so the next PairingEnabled/
// DiscoveryEnabled observes the injection without waiting for TTL expiry.
func (r *ServeRunner) RefreshSwitches(envPairing, flagPairing, envDiscovery, flagDiscovery *bool) {
	r.switchIn.Store(&switchInputs{
		envPairing:    envPairing,
		flagPairing:   flagPairing,
		envDiscovery:  envDiscovery,
		flagDiscovery: flagDiscovery,
	})
	r.switches.Store(r.rebuildSwitches())
}

// PairingEnabled reports whether the /pair surface is on right now: a cache
// hit within switchTTL answers from the memo; an expired or absent one
// rebuilds from the injected inputs + the serve.pairing store setting.
func (r *ServeRunner) PairingEnabled() bool {
	return r.cachedSwitches().pairing
}

// DiscoveryEnabled is PairingEnabled for the UDP discovery responder.
func (r *ServeRunner) DiscoveryEnabled() bool {
	return r.cachedSwitches().discovery
}

// cachedSwitches returns the memoized resolution when fresh, else a fresh one
// (stored for the next caller).
func (r *ServeRunner) cachedSwitches() *switchCache {
	if c := r.switches.Load(); c != nil && time.Since(c.at) < switchTTL {
		return c
	}
	c := r.rebuildSwitches()
	r.switches.Store(c)
	return c
}

// rebuildSwitches re-runs the four-layer resolve for both switches. A store
// read error reads as "unset" (the layer defers onward to the default) — a
// transient vault read failure must not take serve's surfaces down; the
// underlying error still surfaces on every other store use.
func (r *ServeRunner) rebuildSwitches() *switchCache {
	in := r.switchIn.Load()
	var envP, flagP, envD, flagD *bool
	if in != nil {
		envP, flagP, envD, flagD = in.envPairing, in.flagPairing, in.envDiscovery, in.flagDiscovery
	}
	return &switchCache{
		at:        time.Now(),
		pairing:   ResolveSwitch(switchString(envP), flagP != nil, flagP != nil && *flagP, storeSwitchVal(r.st, settingPairing), defaultPairing),
		discovery: ResolveSwitch(switchString(envD), flagD != nil, flagD != nil && *flagD, storeSwitchVal(r.st, settingDiscovery), defaultDiscovery),
	}
}

// storeSwitchVal reads one persisted switch value; absent or unreadable → ""
// (unset). See rebuildSwitches for the error posture.
func storeSwitchVal(st *store.Store, key string) string {
	v, ok, err := st.GetSetting(key)
	if err != nil || !ok {
		return ""
	}
	return v
}
