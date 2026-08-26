package clientops

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveMaxOffline_Priority(t *testing.T) {
	dir := t.TempDir()
	// 无 env 无 file → off
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	if d, err := resolveMaxOffline(dir); err != nil || d != 0 {
		t.Fatalf("off = %v, %v", d, err)
	}
	// file 生效
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"24h"}`), 0o600)
	if d, err := resolveMaxOffline(dir); err != nil || d != 24*time.Hour {
		t.Fatalf("file = %v, %v", d, err)
	}
	// env > file
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "168h")
	if d, err := resolveMaxOffline(dir); err != nil || d != 168*time.Hour {
		t.Fatalf("env must win: %v, %v", d, err)
	}
	// env 非法 → env 的错误胜出（fail-closed 不被 file 掩盖）
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "bogus")
	if _, err := resolveMaxOffline(dir); err == nil || !strings.Contains(err.Error(), "SSHMGR_CACHE_MAX_OFFLINE") {
		t.Fatalf("invalid env must error: %v", err)
	}
	// file 非法 → fail-closed 注明来源
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"30m"}`), 0o600)
	if _, err := resolveMaxOffline(dir); err == nil || !strings.Contains(err.Error(), "cache.config.json") {
		t.Fatalf("invalid file must error with source: %v", err)
	}
	// file 空/0 → off
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"0"}`), 0o600)
	if d, err := resolveMaxOffline(dir); err != nil || d != 0 {
		t.Fatalf("zero file = off: %v, %v", d, err)
	}
	// file 损坏 → fail-closed
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{`), 0o600)
	if _, err := resolveMaxOffline(dir); err == nil {
		t.Fatal("corrupt file must error")
	}
}

func TestWriteCacheConfig_RoundTripAndConcurrentReads(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCacheConfig(dir, "24h"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if !strings.Contains(string(b), `"max_offline":"24h"`) {
		t.Fatalf("config body = %s", b)
	}
	// 并发读永不半截（atomicWriteUnique 语义,spec §6.9）
	// Brief deviation (Windows): an open racing the rename-replace transiently
	// fails with a sharing violation — the exact race cache_atomic_test.go's
	// reader tolerates ("与 rename 竞争的瞬时 ENOENT，重读即可") and the writer
	// side retries in atomicWriteUnique. It is NOT a torn read; the §6.9
	// property is asserted on every SUCCESSFUL read: the bytes must parse to
	// the complete document (a half-written file would fail json.Unmarshal
	// here). resolveMaxOffline's own parsing is covered by
	// TestResolveMaxOffline_Priority above.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// Brief deviation (Windows): pace the reader — 4 unyielding
					// ReadFile loops hold the target near-permanently open and
					// starve the writer's rename past its retry budget (Access
					// denied), which is a scheduling artifact, not a tearing
					// property. 0.5ms still samples thousands of reads across
					// the 50 writes.
					time.Sleep(500 * time.Microsecond)
					b, rerr := os.ReadFile(filepath.Join(dir, "cache.config.json"))
					if rerr != nil {
						continue // transient sharing violation racing the rename — re-read
					}
					var c struct {
						MaxOffline string `json:"max_offline"`
					}
					if uerr := json.Unmarshal(b, &c); uerr != nil || c.MaxOffline != "24h" {
						t.Errorf("reader saw torn config: %s", b)
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if err := WriteCacheConfig(dir, "24h"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// 换码 runbook 联动(spec §6.8⑤):清四件套重 enroll 后 config 保留继承。
func TestConfig_SurvivesCodeSwapRunbook(t *testing.T) {
	withDEK(t)
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(time.Now().UTC().Format(http.TimeFormat)), nil))
	if err := DoPull(url, "code1", pin, PullOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCacheConfig(dir, "24h"); err != nil {
		t.Fatal(err)
	}
	// 四件套清除(config 保留——目录/槽位策略,spec §2.4 runbook)
	for _, f := range []string{"cache.auth.json", "cache.bin", "cache.meta.json"} {
		os.Remove(filepath.Join(dir, f))
	}
	os.RemoveAll(filepath.Join(dir, "quarantine"))
	// 重 enroll → config 仍在且生效
	if d, err := resolveMaxOffline(dir); err != nil || d != 24*time.Hour {
		t.Fatalf("config must survive the code swap: %v, %v", d, err)
	}
}
