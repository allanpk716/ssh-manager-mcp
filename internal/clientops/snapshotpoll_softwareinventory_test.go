package clientops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

type fixedRefresher struct{ revision int64 }

func (r fixedRefresher) CurrentRevision() int64 { return r.revision }
func (r fixedRefresher) ReloadNow()             {}

// TestSnapshotPollerRunsSoftwareInventoryCollectorAfterRound 验证快照新鲜度
// 轮询每轮之后触发顺手补采，且同一时刻只有一份在执行。
func TestSnapshotPollerRunsSoftwareInventoryCollectorAfterRound(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"snapshot_revision": 7})
	}))
	defer srv.Close()
	pin := mcpserver.SPKIFingerprint(srv.Certificate())
	blob, err := json.Marshal(CacheCred{URL: srv.URL, Token: "devcode", Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	var active, runs, maxActive atomic.Int64
	done := make(chan struct{}, 3)
	stop := StartSnapshotPoller(SnapshotPollerConfig{
		Interval:  15 * time.Millisecond,
		Refresher: fixedRefresher{revision: 7},
		SoftwareInventoryCollect: func() {
			current := active.Add(1)
			for {
				old := maxActive.Load()
				if current <= old || maxActive.CompareAndSwap(old, current) {
					break
				}
			}
			runs.Add(1)
			time.Sleep(40 * time.Millisecond)
			active.Add(-1)
			done <- struct{}{}
		},
	})
	defer stop()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("快照轮询后没有触发软件清单采集")
		}
	}
	stop()
	if maxActive.Load() != 1 {
		t.Fatalf("顺手补采最大并发 = %d，应为 1", maxActive.Load())
	}
}
