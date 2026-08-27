// relocate_cli_test.go — Plan 40 批2 §5（CLI 行）：裸 pull 首次 enroll 归位后，
// auth 与 --max-offline 的 config 落盘必须跟随 PullResult.Instance（实例槽），
// 绝不写回默认槽——否则 mcp --cache 的刷新链断在"auth 在默认槽、材料在实例槽"
// （spec §0.6 原病）。plaintext 分支不归位，不受此约束。
package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
)

// runCachePull executes the real `cache pull` cobra command in-process against
// srv, capturing stderr (the relocation hint rides StatusOut = ErrOrStderr).
func runCachePull(t *testing.T, url, token, pin string) string {
	t.Helper()
	cmd := newCacheCmd()
	args := []string{"pull", "--url", url, "--token", token}
	if pin != "" {
		args = append(args, "--pin", pin, "--max-offline", "12h")
	}
	cmd.SetArgs(args)
	var errBuf bytes.Buffer
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errBuf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cache pull: %v\nstderr:\n%s", err, errBuf.String())
	}
	return errBuf.String()
}

func TestCachePull_FirstEnrollRelocateFollowsResultSlot(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":         "",
		"SSHMGR_CACHE_DEK":         "",
		"SSHMGR_CACHE_MAX_OFFLINE": "",
		"SSHMGR_SERVE_PIN":         "",
		"SSHMGR_CACHE_URL":         "",
		"SSHMGR_CACHE_TOKEN":       "",
	})
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)

	// Light pinned serve: Plan-40 header derived from the bearer code prefix
	// (plan40Serve shape), snapshot body, auto Date (valid for the anchor gate).
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer code-"); name != r.Header.Get("Authorization") {
			w.Header().Set("X-Sshmgr-Device-Name", name)
		}
		_, _ = w.Write([]byte(`{"servers":[],"credentials":[]}`))
	}))
	t.Cleanup(srv.Close)
	url := srv.URL
	pin := mcpserver.SPKIFingerprint(srv.Certificate())

	name := "laptop-agentC"
	code := "code-" + name
	out := runCachePull(t, url, code, pin)
	instDir := filepath.Join(userDir, "ssh-manager", "instances", name)
	defDir := filepath.Join(userDir, "ssh-manager")

	// 归位提示行打到 stderr。
	if !strings.Contains(out, "first enroll located to instance "+name) {
		t.Fatalf("relocation hint missing in stderr: %q", out)
	}
	// §5：auth 落实例槽（刷新链存活），且默认槽一个字节都没被碰。
	cred, cerr := clientops.ReadCacheCredFor(name)
	if cerr != nil || cred == nil || cred.Token != code || cred.Pin != pin {
		t.Fatalf("ReadCacheCredFor(%q) = %+v, %v", name, cred, cerr)
	}
	if _, serr := os.Stat(filepath.Join(instDir, "cache.auth.json")); serr != nil {
		t.Fatalf("instance auth.json missing: %v", serr)
	}
	for _, f := range []string{"cache.auth.json", "cache.config.json", "cache.bin", "cache.meta.json"} {
		if _, serr := os.Stat(filepath.Join(defDir, f)); serr == nil {
			t.Fatalf("default slot must stay vacuum, found %s", f)
		}
	}
	// --max-offline 的 config 目录解析跟随 res.Instance：cap 落实例槽并生效文件形态。
	cfgBlob, rerr := os.ReadFile(filepath.Join(instDir, "cache.config.json"))
	if rerr != nil {
		t.Fatalf("--max-offline config must follow res.Instance into the slot: %v", rerr)
	}
	if !strings.Contains(string(cfgBlob), `"max_offline":"12h"`) {
		t.Fatalf("config content unexpected: %s", cfgBlob)
	}
	if d, src, eerr := clientops.EffectiveMaxOffline(instDir); eerr != nil || d != 12*time.Hour || src != "file" {
		t.Fatalf("EffectiveMaxOffline(instance) = (%v, %s, %v)", d, src, eerr)
	}
	// 幂等再拉同码：同槽放行、auth 覆写同一槽（值不变）、DEK 不重建。
	dekPath := filepath.Join(dekDir, "cache-dek-"+name+".key")
	dekBefore, _ := os.ReadFile(dekPath)
	runCachePull(t, url, code, pin)
	cred2, cerr2 := clientops.ReadCacheCredFor(name)
	if cerr2 != nil || cred2 == nil || cred2.Token != code || cred2.Pin != pin {
		t.Fatalf("idempotent re-pull must overwrite the SAME auth slot: %+v, %v", cred2, cerr2)
	}
	if _, serr := os.Stat(filepath.Join(defDir, "cache.auth.json")); serr == nil {
		t.Fatal("default slot must STILL have no auth after re-pull")
	}
	dekAfter, _ := os.ReadFile(dekPath)
	if !bytes.Equal(dekBefore, dekAfter) {
		t.Fatal("instance DEK must not be regenerated")
	}
}
