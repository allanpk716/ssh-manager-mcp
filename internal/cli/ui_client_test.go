package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiops"
)

func TestUIClientSyncUsesCachePullPathOverPinnedTLS(t *testing.T) {
	cacheDir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "cache-dek.key")
	userDir := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	t.Setenv("SSHMGR_CACHE_DEK", keyPath)

	snapshot := &store.Snapshot{
		Version: 1, SnapshotRevision: 77, RecommendedVersion: "1.2.0",
		Servers: []store.SnapshotServer{
			{ID: "one", Name: "一"}, {ID: "two", Name: "二"},
		},
		Profiles: []store.SnapshotProfile{{ID: "profile-id", Name: "团队"}},
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshot" || r.Header.Get("Authorization") != "Bearer device-code" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Sshmgr-Snapshot-Scope", "profile")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	pin := mcpserver.SPKIFingerprint(server.Certificate())
	credential := `{"url":"` + server.URL + `","token":"device-code","pin":"` + pin + `"}`
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}

	source := webuiops.ClientPageStoreWithUpdates(nil, uiClientPull)
	status, err := source.SyncClient("", "127.0.0.1")
	if err != nil {
		t.Fatalf("Web UI 立即同步失败：%v", err)
	}
	if status.SnapshotRevision != 77 || status.ServerCount != 2 || status.Profile != "团队" {
		t.Fatalf("同步后的状态不符：%+v", status)
	}
	for _, name := range []string{"cache.bin", "cache.meta.json", "cache-audit.log"} {
		if _, statErr := os.Stat(filepath.Join(cacheDir, name)); statErr != nil {
			t.Fatalf("同步后缺少 %s：%v", name, statErr)
		}
	}
	audit, err := os.ReadFile(filepath.Join(cacheDir, "cache-audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"action":"cache.pull"`) ||
		!strings.Contains(string(audit), `"source":"web_ui"`) {
		t.Fatalf("立即同步审计行不符：%s", audit)
	}

	wrongPin := "sha256:" + strings.Repeat("0", 64)
	wrongCredential := `{"url":"` + server.URL + `","token":"device-code","pin":"` + wrongPin + `"}`
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), []byte(wrongCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = source.SyncClient("", "127.0.0.1")
	var syncErr *webui.OpsClientSyncError
	if !errors.As(err, &syncErr) || syncErr.Kind != "certificate_fingerprint" {
		t.Fatalf("证书公钥指纹不符应按分类返回，实际：%v", err)
	}
}
