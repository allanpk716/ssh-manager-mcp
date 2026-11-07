package clientops

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// snapshotServeWithDeviceIdentity shapes a pinned test serve that reports the
// given topology device identifier. An empty identifier omits the header, as
// an old authority or an unassociated device code does.
func snapshotServeWithDeviceIdentity(deviceID string) http.HandlerFunc {
	date := time.Now().UTC().Format(http.TimeFormat)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", date)
		w.Header().Set("X-Sshmgr-Device-Name", "laptop")
		if deviceID != "" {
			w.Header().Set("X-Sshmgr-Device-ID", deviceID)
		}
		_, _ = w.Write([]byte(`{"servers":[],"credentials":[]}`))
	}
}

func TestDoPullRecordsDeviceID(t *testing.T) {
	t.Run("associated device code", func(t *testing.T) {
		redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		t.Setenv("SSHMGR_CACHE_DEK", "")
		url, pin := newPinnedTLSServer(t, snapshotServeWithDeviceIdentity("device-laptop"))

		if _, err := DoPull(url, "code-laptop", pin, PullOpts{Instance: "laptop"}); err != nil {
			t.Fatal(err)
		}
		if got := CacheDeviceIDFor("laptop"); got != "device-laptop" {
			t.Fatalf("CacheDeviceIDFor = %q, want device-laptop", got)
		}
		_, bin, metaPath, _, err := CachePathsFor("laptop")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(bin); err != nil {
			t.Fatal(err)
		}
		meta, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(meta), `"device_id":"device-laptop"`) {
			t.Fatalf("cache.meta.json must record the topology device identifier: %s", meta)
		}
	})

	t.Run("device code without association", func(t *testing.T) {
		redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		t.Setenv("SSHMGR_CACHE_DEK", "")
		url, pin := newPinnedTLSServer(t, snapshotServeWithDeviceIdentity(""))

		if _, err := DoPull(url, "code-laptop", pin, PullOpts{Instance: "laptop"}); err != nil {
			t.Fatal(err)
		}
		if got := CacheDeviceIDFor("laptop"); got != "" {
			t.Fatalf("unassociated CacheDeviceIDFor = %q, want empty", got)
		}
	})

	t.Run("old authority without the field", func(t *testing.T) {
		redirectUserConfigDir(t)
		t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
		t.Setenv("SSHMGR_CACHE_DEK", "")
		url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(time.Now().UTC().Format(http.TimeFormat)), nil))

		if _, err := DoPull(url, "code-laptop", pin, PullOpts{}); err != nil {
			t.Fatal(err)
		}
		if got := CacheDeviceIDFor(""); got != "" {
			t.Fatalf("old-authority CacheDeviceIDFor = %q, want empty", got)
		}
	})
}
