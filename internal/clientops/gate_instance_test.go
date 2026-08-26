package clientops

// Plan 40 Task 10: the --instance pull gate (spec §2.4 row 1 + §2.1) — header
// strong-match (old serve / illegal name / mismatched code-flag pair all
// refuse) + physical collision detection on the instance slot (different
// recorded identity refuses; bin-without-readable-meta = half-written state
// refuses with a cleanup path; blank meta identity = pre-Plan-40 residue,
// adopted and backfilled by the pull's meta write).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pullInstance(t *testing.T, instance string, srvName *string) error {
	t.Helper()
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(srvName))
	return DoPull(url, "code", pin, PullOpts{Instance: instance})
}

func instDir(t *testing.T, userDir, name string) string {
	return filepath.Join(userDir, "ssh-manager", "instances", name)
}

func TestGateNamedInstance(t *testing.T) {
	nA := "laptop-agentA"
	other := "laptop-agentB"

	t.Run("happy path writes instance slot", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		if err := pullInstance(t, nA, &nA); err != nil {
			t.Fatalf("instance pull: %v", err)
		}
		if _, err := os.Stat(filepath.Join(instDir(t, userDir, nA), "cache.bin")); err != nil {
			t.Fatal(err)
		}
		if m := readMetaForTest(t, instDir(t, userDir, nA)); m.DeviceName != nA {
			t.Fatalf("meta device_name = %q", m.DeviceName)
		}
		// 同码重拉 → 放行
		if err := pullInstance(t, nA, &nA); err != nil {
			t.Fatalf("re-pull: %v", err)
		}
	})

	t.Run("old serve header missing refused", func(t *testing.T) {
		redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		err := pullInstance(t, nA, nil) // 不发头
		if err == nil || !strings.Contains(err.Error(), "--instance requires") {
			t.Fatalf("old serve must refuse --instance: %v", err)
		}
	})

	t.Run("mismatch refused before any write", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		err := pullInstance(t, nA, &other) // 头=B,flag=A
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("mismatch must refuse: %v", err)
		}
		if _, serr := os.Stat(instDir(t, userDir, nA)); !os.IsNotExist(serr) {
			t.Fatal("no directory may be created on refusal")
		}
	})

	t.Run("illegal server name refused", func(t *testing.T) {
		redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		bad := "../evil"
		if err := pullInstance(t, "ok-name", &bad); err == nil || !strings.Contains(err.Error(), "invalid device name") {
			t.Fatalf("illegal name must refuse: %v", err)
		}
	})

	t.Run("physical collision: existing identity differs", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		if err := pullInstance(t, nA, &nA); err != nil { // 先占位
			t.Fatal(err)
		}
		// 手工把 meta 身份改成别的（模拟另一身份残留）
		dir := instDir(t, userDir, nA)
		os.WriteFile(filepath.Join(dir, "cache.meta.json"),
			[]byte(`{"url":"x","pulled_at":1,"server_anchored":true,"scoped":false,"device_name":"someone-else"}`), 0o600)
		if err := pullInstance(t, nA, &nA); err == nil || !strings.Contains(err.Error(), "different device identity") {
			t.Fatalf("physical collision must refuse: %v", err)
		}
	})

	t.Run("half-written: bin without readable meta refused", func(t *testing.T) {
		userDir := redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		dir := instDir(t, userDir, nA)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("partial"), 0o600)
		if err := pullInstance(t, nA, &nA); err == nil || !strings.Contains(err.Error(), "re-enroll") {
			t.Fatalf("half-written state must refuse with cleanup path: %v", err)
		}
	})
}
