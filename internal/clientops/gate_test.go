package clientops

// Plan 40 Task 9: the default-instance identity gate (spec §2.4) — a different
// device code must never silently overwrite the default cache. The three
// branches: refuse-on-mismatch, adopt-on-unregistered, refuse-on-unparseable;
// plus the no-bin vacuum, the old-serve skip+hint, and the illegal-name defense.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deviceSnapshotHandler: pinned serve shape + controllable X-Sshmgr-Device-Name.
// name==nil → no header (old-serve fixture); *name=="" → empty-value header.
func deviceSnapshotHandler(name *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if name != nil && *name != "" {
			w.Header().Set("X-Sshmgr-Device-Name", *name)
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}
}

func pullWith(t *testing.T, srvName *string) error {
	t.Helper()
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(srvName))
	_, err := DoPull(url, "code", pin, PullOpts{})
	return err
}

func mustCacheDir(t *testing.T) string {
	t.Helper()
	d := os.Getenv("SSHMGR_CACHE_DIR")
	if d == "" {
		t.Fatal("test bug: SSHMGR_CACHE_DIR must be set")
	}
	return d
}

// dirSums snapshots the sha256 of every cache material file that EXISTS.
func dirSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range []string{"cache.bin", "cache.meta.json", "cache.auth.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			out[f] = fileSum(t, filepath.Join(dir, f))
		}
	}
	return out
}

func assertDirSumsUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	for f, sum := range before {
		if got := fileSum(t, filepath.Join(dir, f)); got != sum {
			t.Fatalf("%s changed on a refused pull", f)
		}
	}
}

func TestGate_DefaultInstance_ThreeBranches(t *testing.T) {
	withDEK(t) // the gate never touches the DEK, but the write path needs one
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	nX, nY := "laptop-agentA", "laptop-agentB"

	// ④ vacuum first pull → allowed + name recorded
	if err := pullWith(t, &nX); err != nil {
		t.Fatalf("vacuum pull: %v", err)
	}
	m := readMetaForTest(t, mustCacheDir(t))
	if m.DeviceName != nX {
		t.Fatalf("vacuum pull must record device_name, got %q", m.DeviceName)
	}
	// same-name re-pull → allowed (branch 1's equality side)
	if err := pullWith(t, &nX); err != nil {
		t.Fatalf("same-name re-pull: %v", err)
	}
	// ① cross-code → refused + existing material byte-identical
	before := dirSums(t, mustCacheDir(t))
	err := pullWith(t, &nY)
	if err == nil || !strings.Contains(err.Error(), "--instance") {
		t.Fatalf("cross-code pull must be refused with the three-choice text: %v", err)
	}
	if !strings.Contains(err.Error(), "device code") && !strings.Contains(err.Error(), "cache-tokens") {
		t.Fatalf("refusal must guide owner verification: %v", err)
	}
	assertDirSumsUnchanged(t, mustCacheDir(t), before) // existing bin/meta/auth sha256 unchanged

	// ② legacy unregistered (device_name empty) → allowed + backfilled (zero-migration lifeline)
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	legacyMeta := `{"url":"https://old","pulled_at":1,"server_anchored":true,"scoped":false}`
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(legacyMeta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("old-encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pullWith(t, &nX); err != nil {
		t.Fatalf("legacy unregistered meta must be adopted: %v", err)
	}
	if m := readMetaForTest(t, dir); m.DeviceName != nX {
		t.Fatalf("adoption must backfill device_name, got %q", m.DeviceName)
	}

	// ③ bin present + meta unparseable → refused
	dir2 := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir2})
	os.WriteFile(filepath.Join(dir2, "cache.bin"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir2, "cache.meta.json"), []byte("{not json"), 0o600)
	if err := pullWith(t, &nX); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unparseable meta + bin must refuse: %v", err)
	}

	// ⑥ header present but illegal name → refuse the write
	dir3 := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir3})
	bad := "../evil"
	if err := pullWith(t, &bad); err == nil || !strings.Contains(err.Error(), "invalid device name") {
		t.Fatalf("illegal header name must refuse: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir3, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("no write may happen on refusal")
	}
}

func TestGate_OldServe_SkipAndHint(t *testing.T) {
	withDEK(t)
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	// Seed material first (an old serve without the header can still pull — vacuum).
	if err := pullWith(t, nil); err != nil { // nil = no header (old serve)
		t.Fatalf("old-serve first pull: %v", err)
	}
	// old serve + bin present → gate skipped + WARNING
	var buf bytes.Buffer
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(nil))
	_, err := DoPull(url, "code", pin, PullOpts{StatusOut: &buf})
	if err != nil {
		t.Fatalf("old-serve re-pull must succeed (gate skipped): %v", err)
	}
	if !strings.Contains(buf.String(), "X-Sshmgr-Device-Name") || !strings.Contains(buf.String(), "upgrade") {
		t.Fatalf("old-serve hint missing: %q", buf.String())
	}
	if m := readMetaForTest(t, dir); m.DeviceName != "" {
		t.Fatalf("old-serve pull must leave device_name empty, got %q", m.DeviceName)
	}
}

func TestGate_PlaintextNeverRecordsDeviceName(t *testing.T) {
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sshmgr-Device-Name", "spoofed") // injected header: the plaintext channel is untrusted
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer plain.Close()
	if _, err := DoPull(plain.URL, "code", "", PullOpts{AllowPlain: true}); err != nil {
		t.Fatal(err)
	}
	if m := readMetaForTest(t, mustCacheDir(t)); m.DeviceName != "" {
		t.Fatalf("plaintext must never record device_name, got %q", m.DeviceName)
	}
}
