// relocate_test.go — Plan 40 批2 §1：真空 v4 自动归位。
// 真空 = 默认槽 bin/auth/meta/config 四文件均缺（rev5 §1.1 条件 4——meta/config
// 是默认槽意图标记，"曾有材料/曾配置"的痕迹）。
package clientops

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRelocateEnv redirects UserConfigDir + the whole DEK tree into temp dirs,
// clears every slot-routing/policy env (same posture as the e2e tests: never
// SSHMGR_CACHE_DIR), and returns the fake user-config root. The DEK provider
// stays the REAL FileKeyProvider pointed at dekDir — per-instance naming is
// part of what relocation must honor.
func newRelocateEnv(t *testing.T) (userDir, dekDir string) {
	t.Helper()
	userDir = redirectUserConfigDir(t)
	dekDir = t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	return userDir, dekDir
}

// seedDefaultMarker plants one vacuum-marker file in the DEFAULT slot so the
// four-file judgment flips to "non-vacuum". Content mirrors each marker's
// real shape (only meta's device_name is ever asserted back).
func seedDefaultMarker(t *testing.T, userDir, file, content string) {
	t.Helper()
	p := filepath.Join(userDir, "ssh-manager", file)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedAuthMarker writes a plausible (unused-by-pull) cache.auth.json presence marker.
func seedAuthMarker(t *testing.T, userDir string) {
	seedDefaultMarker(t, userDir, "cache.auth.json", `{"url":"https://broker.example","token":"stale-code","pin":""}`)
}

func TestRelocate_VacuumV4_BarePullLandsInInstance(t *testing.T) {
	userDir, dekDir := newRelocateEnv(t)
	date := ptr(time.Now().UTC().Format(http.TimeFormat))
	url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))

	// 真空机 + 裸 pull → 材料+meta 落 instances/<头name>/，默认槽保持全空，
	// PullResult.Instance == 头name，CLI 提示行打到 StatusOut。
	var buf bytes.Buffer
	res, err := DoPull(url, "code-laptop-agentA", pin, PullOpts{StatusOut: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instance != "laptop-agentA" {
		t.Fatalf("res.Instance = %q, want laptop-agentA", res.Instance)
	}
	instDir := filepath.Join(userDir, "ssh-manager", "instances", "laptop-agentA")
	for _, f := range []string{"cache.bin", "cache.meta.json"} {
		if _, serr := os.Stat(filepath.Join(instDir, f)); serr != nil {
			t.Fatalf("%s: %v", f, serr)
		}
	}
	m := readMetaForTest(t, instDir)
	if m.DeviceName != "laptop-agentA" {
		t.Fatalf("instance meta.device_name = %q, want laptop-agentA", m.DeviceName)
	}
	if !strings.Contains(buf.String(), "first enroll located to instance laptop-agentA") {
		t.Fatalf("hint missing: %q", buf.String())
	}
	defDir := filepath.Join(userDir, "ssh-manager")
	for _, f := range []string{"cache.bin", "cache.auth.json", "cache.meta.json", "cache.config.json"} {
		if _, serr := os.Stat(filepath.Join(defDir, f)); serr == nil {
			t.Fatalf("default slot must stay vacuum, found %s", f)
		}
	}
	// the deferred DEK went to the INSTANCE (per-instance filename), and the
	// relocated cache loads back through the normal For-path.
	dekPath := filepath.Join(dekDir, "cache-dek-laptop-agentA.key")
	if _, serr := os.Stat(dekPath); serr != nil {
		t.Fatalf("per-instance DEK missing (%s): %v", dekPath, serr)
	}
	if snap, lerr := LoadCacheSnapshotFor("laptop-agentA"); lerr != nil || snap == nil {
		t.Fatalf("LoadCacheSnapshotFor after relocation: %v", lerr)
	}
}

func TestRelocate_NonVacuumSevenStates(t *testing.T) {
	date := ptr(time.Now().UTC().Format(http.TimeFormat))
	name, code := "laptop-agentA", "code-laptop-agentA"

	// assertNotRelocated verifies the SHARED outcome of every no-go state:
	// the material landed in wantBin's directory, instances/ never appeared,
	// and StatusOut carries no relocation hint.
	assertNotRelocated := func(t *testing.T, userDir, wantBin, out string) {
		t.Helper()
		if _, serr := os.Stat(wantBin); serr != nil {
			t.Fatalf("material must be at %s: %v", wantBin, serr)
		}
		if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances")); !os.IsNotExist(serr) {
			t.Fatalf("instances/ must not appear, stat err = %v", serr)
		}
		if strings.Contains(out, "first enroll located") {
			t.Fatalf("no relocation hint expected, got %q", out)
		}
	}

	t.Run("1-auth-only-backfills-device-name", func(t *testing.T) {
		userDir, _ := newRelocateEnv(t)
		seedAuthMarker(t, userDir)
		url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))
		var buf bytes.Buffer
		if _, err := DoPull(url, code, pin, PullOpts{StatusOut: &buf}); err != nil {
			t.Fatal(err)
		}
		assertNotRelocated(t, userDir, filepath.Join(userDir, "ssh-manager", "cache.bin"), buf.String())
		// 门禁补记：gateDefaultInstance 放行的 auth-only 拉取把 device_name 写进 meta。
		if m := readMetaForTest(t, filepath.Join(userDir, "ssh-manager")); m.DeviceName != name {
			t.Fatalf("meta.device_name = %q, want %q (identity must backfill)", m.DeviceName, name)
		}
	})

	t.Run("2-meta-only-legacy-shape", func(t *testing.T) {
		userDir, _ := newRelocateEnv(t)
		seedDefaultMarker(t, userDir, "cache.meta.json", "{}") // 存量形态：无 config、无 device_name
		url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))
		var buf bytes.Buffer
		if _, err := DoPull(url, code, pin, PullOpts{StatusOut: &buf}); err != nil {
			t.Fatal(err)
		}
		assertNotRelocated(t, userDir, filepath.Join(userDir, "ssh-manager", "cache.bin"), buf.String())
	})

	t.Run("3-config-only-valid-cap", func(t *testing.T) {
		userDir, _ := newRelocateEnv(t)
		seedDefaultMarker(t, userDir, "cache.config.json", `{"max_offline":"24h"}`)
		url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))
		var buf bytes.Buffer
		if _, err := DoPull(url, code, pin, PullOpts{StatusOut: &buf}); err != nil {
			t.Fatal(err)
		}
		assertNotRelocated(t, userDir, filepath.Join(userDir, "ssh-manager", "cache.bin"), buf.String())
	})

	t.Run("4-cache-dir-env-override-wins", func(t *testing.T) {
		userDir, _ := newRelocateEnv(t)
		override := t.TempDir()
		t.Setenv("SSHMGR_CACHE_DIR", override)
		url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))
		var buf bytes.Buffer
		if _, err := DoPull(url, code, pin, PullOpts{StatusOut: &buf}); err != nil {
			t.Fatal(err)
		}
		if _, serr := os.Stat(filepath.Join(override, "cache.bin")); serr != nil {
			t.Fatalf("material must be in the override dir: %v", serr)
		}
		if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances")); !os.IsNotExist(serr) {
			t.Fatalf("instances/ must not appear under the redirected config dir")
		}
	})

	t.Run("5-cache-dek-env-keeps-default-slot", func(t *testing.T) {
		userDir, dekDir := newRelocateEnv(t)
		t.Setenv("SSHMGR_CACHE_DEK", filepath.Join(dekDir, "explicit-dek.key"))
		url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))
		var buf bytes.Buffer
		if _, err := DoPull(url, code, pin, PullOpts{StatusOut: &buf}); err != nil {
			t.Fatal(err)
		}
		assertNotRelocated(t, userDir, filepath.Join(userDir, "ssh-manager", "cache.bin"), buf.String())
		// 材料确实用 env DEK 加密落盘（文件由 eager loadOrCreateDEK 创建）。
		if _, serr := os.Stat(filepath.Join(dekDir, "explicit-dek.key")); serr != nil {
			t.Fatalf("env-DEK file must exist: %v", serr)
		}
	})

	t.Run("6-old-serve-no-header-stays-default", func(t *testing.T) {
		userDir, _ := newPinnedNoHeaderEnv(t)
		url, pin := newPinnedTLSServer(t, snapshotHandler(date, nil)) // 裸 handler：不发 X-Sshmgr-Device-Name
		var buf bytes.Buffer
		if _, err := DoPull(url, code, pin, PullOpts{StatusOut: &buf}); err != nil {
			t.Fatal(err)
		}
		assertNotRelocated(t, userDir, filepath.Join(userDir, "ssh-manager", "cache.bin"), buf.String())
	})

	t.Run("7-plaintext-never-relocates", func(t *testing.T) {
		userDir, _ := newRelocateEnv(t)
		plain := httptest.NewServer(snapshotHandler(nil, nil)) // auto Date is fine; plaintext ignores anchors anyway
		defer plain.Close()
		var buf bytes.Buffer
		if _, err := DoPull(plain.URL, code, "", PullOpts{AllowPlain: true, StatusOut: &buf}); err != nil {
			t.Fatal(err)
		}
		assertNotRelocated(t, userDir, filepath.Join(userDir, "ssh-manager", "cache.bin"), buf.String())
	})

	t.Run("8-dek-dir-env-does-not-block-positive-control", func(t *testing.T) {
		// 正向对照：SSHMGR_CACHE_DEK_DIR 是目录级一致 seam——归位照常，实例 DEK 落 env dir。
		userDir, dekDir := newRelocateEnv(t)
		url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))
		if _, err := DoPull(url, code, pin, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		instDir := filepath.Join(userDir, "ssh-manager", "instances", name)
		if _, serr := os.Stat(filepath.Join(instDir, "cache.bin")); serr != nil {
			t.Fatalf("relocated bin: %v", serr)
		}
		if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-"+name+".key")); serr != nil {
			t.Fatalf("instance DEK must land in the env dir: %v", serr)
		}
	})
}

func TestRelocate_RePullSameCode_Idempotent(t *testing.T) {
	userDir, dekDir := newRelocateEnv(t)
	date := ptr(time.Now().UTC().Format(http.TimeFormat))
	url, pin := newPinnedTLSServer(t, plan40Serve("code-", snapshotHandler(date, nil)))
	instDir := filepath.Join(userDir, "ssh-manager", "instances", "laptop-agentA")
	dekPath := filepath.Join(dekDir, "cache-dek-laptop-agentA.key")

	if _, err := DoPull(url, "code-laptop-agentA", pin, PullOpts{}); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	binBefore, err := os.ReadFile(filepath.Join(instDir, "cache.bin"))
	if err != nil {
		t.Fatal(err)
	}
	dekBefore, err := os.ReadFile(dekPath)
	if err != nil {
		t.Fatal(err)
	}
	dekStat, err := os.Stat(dekPath)
	if err != nil {
		t.Fatal(err)
	}

	// §11.7 幂等：再裸 pull 同码 → 同实例目录放行 + DEK 不重复生成 + 材料不搬家。
	res, err := DoPull(url, "code-laptop-agentA", pin, PullOpts{})
	if err != nil {
		t.Fatalf("idempotent re-pull: %v", err)
	}
	if res.Instance != "laptop-agentA" {
		t.Fatalf("re-pull res.Instance = %q, want laptop-agentA", res.Instance)
	}
	if _, serr := os.Stat(filepath.Join(instDir, "cache.meta.json")); serr != nil {
		t.Fatalf("relocation target unchanged: %v", serr)
	}
	dekAfter, rerr := os.ReadFile(dekPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(dekBefore, dekAfter) {
		t.Fatal("DEK must NOT be regenerated on re-pull (content changed)")
	}
	dekAfterStat, serr := os.Stat(dekPath)
	if serr != nil {
		t.Fatal(serr)
	}
	if !dekAfterStat.ModTime().Equal(dekStat.ModTime()) {
		t.Fatalf("DEK file was rewritten (mtime %v -> %v)", dekStat.ModTime(), dekAfterStat.ModTime())
	}
	if m := readMetaForTest(t, instDir); m.DeviceName != "laptop-agentA" {
		t.Fatalf("meta identity must stay %q, got %q", "laptop-agentA", m.DeviceName)
	}
	_ = binBefore // re-pull overwrites bin in place; presence was asserted via meta reload below
	if _, lerr := LoadCacheSnapshotFor("laptop-agentA"); lerr != nil {
		t.Fatalf("relocated cache must stay loadable: %v", lerr)
	}
}

// TestRelocate_ExportedJudgments pins the TUI-facing exports: the SAME
// four-file vacuum judgment and the two-env override detector.
func TestRelocate_ExportedJudgments(t *testing.T) {
	userDir, _ := newRelocateEnv(t)
	v, err := DefaultSlotVacuum()
	if err != nil || !v {
		t.Fatalf("fresh machine must report vacuum: (%v, %v)", v, err)
	}
	seedDefaultMarker(t, userDir, "cache.meta.json", "{}")
	v, err = DefaultSlotVacuum()
	if err != nil || v {
		t.Fatalf("a present marker must clear vacuum: (%v, %v)", v, err)
	}
	if SingleSlotOverrideEnvSet() {
		t.Fatal("no override env set yet")
	}
	t.Setenv("SSHMGR_CACHE_DEK", "x")
	if !SingleSlotOverrideEnvSet() {
		t.Fatal("SSHMGR_CACHE_DEK counts as a single-slot override")
	}
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_DIR", "y")
	if !SingleSlotOverrideEnvSet() {
		t.Fatal("SSHMGR_CACHE_DIR counts as a single-slot override")
	}
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", "z") // coherent directory-level seam — does NOT count
	if SingleSlotOverrideEnvSet() {
		t.Fatal("SSHMGR_CACHE_DEK_DIR must NOT count as a single-slot override")
	}
}

// newPinnedNoHeaderEnv is ⑥'s environment shape: nothing special beyond the
// base redirect (named for readability in the table).
func newPinnedNoHeaderEnv(t *testing.T) (string, string) {
	return newRelocateEnv(t)
}
