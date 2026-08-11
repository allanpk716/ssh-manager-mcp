package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// dekProvider returns the KeyProvider holding the cache DEK (keychain slot "cache-dek"). A seam
// so tests inject MemKeyProvider instead of touching the real OS keychain.
var dekProvider = func() store.KeyProvider {
	return &store.KeyringKeyProvider{Service: os.Getenv("SSHMGR_KEYRING_SERVICE"), User: "cache-dek"}
}

// cachePaths resolves the cache directory (SSHMGR_CACHE_DIR override, else UserConfigDir/
// ssh-manager) and the three files within it: the encrypted snapshot, the meta sidecar, and
// the offline-audit sidecar (the audit sidecar is owned by T8; T7 only resolves the path).
func cachePaths() (dir, bin, meta, audit string, err error) {
	if dir = os.Getenv("SSHMGR_CACHE_DIR"); dir == "" {
		base, derr := os.UserConfigDir()
		if derr != nil {
			return "", "", "", "", derr
		}
		dir = filepath.Join(base, "ssh-manager")
	}
	return dir, filepath.Join(dir, "cache.bin"), filepath.Join(dir, "cache.meta.json"), filepath.Join(dir, "cache-audit.log"), nil
}

type cacheMeta struct {
	URL      string `json:"url"`
	PulledAt int64  `json:"pulled_at"` // unix seconds of the local pull
}

// loadOrCreateDEK returns the cache DEK from the keychain, generating + storing it on first pull.
// On subsequent pulls the existing DEK is reused, so cache.bin stays decryptable across pulls.
func loadOrCreateDEK() ([]byte, error) {
	kp := dekProvider()
	dek, err := kp.Get()
	if err == nil {
		return dek, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	dek, err = store.GenerateMasterKey()
	if err != nil {
		return nil, err
	}
	if err := kp.Set(dek); err != nil {
		return nil, err
	}
	return dek, nil
}

// loadDEK returns the cache DEK without creating it (status / mcp --cache). A missing DEK
// surfaces as store.ErrNotFound — the caller reports "run cache pull first".
func loadDEK() ([]byte, error) {
	return dekProvider().Get()
}

// loadCacheSnapshot reads + DEK-decrypts + unmarshals the cache. Shared by `cache status` and
// `mcp --cache`. Returns an error if the cache is absent / corrupt / the DEK is missing.
func loadCacheSnapshot() (*store.Snapshot, error) {
	_, bin, _, _, err := cachePaths()
	if err != nil {
		return nil, err
	}
	dek, err := loadDEK()
	if err != nil {
		return nil, fmt.Errorf("cache DEK not found in keychain (run `cache pull` first): %w", err)
	}
	blob, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	plaintext, err := vaultio.DecryptWithKey(dek, blob)
	if err != nil {
		return nil, fmt.Errorf("cache decrypt failed (the DEK and cache.bin may be from different installs): %w", err)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(plaintext, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Offline read-only cache (pull from a serve broker)"}
	cmd.AddCommand(cachePullCmd(), cacheStatusCmd())
	return cmd
}

func cachePullCmd() *cobra.Command {
	var url, token string
	c := &cobra.Command{
		Use:   "pull",
		Short: "Pull the whole vault from a serve broker into the local encrypted cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			if url == "" {
				url = os.Getenv("SSHMGR_CACHE_URL")
			}
			if token == "" {
				token = os.Getenv("SSHMGR_CACHE_TOKEN")
			}
			if url == "" || token == "" {
				return fmt.Errorf("--url and --token are required (or SSHMGR_CACHE_URL / SSHMGR_CACHE_TOKEN)")
			}
			dek, err := loadOrCreateDEK()
			if err != nil {
				return err
			}
			req, err := http.NewRequest(http.MethodGet, url+"/snapshot", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("pull: %w", err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				return fmt.Errorf("pull: server returned %d (is the authorization code valid/active?)", res.StatusCode)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				return err
			}
			blob, err := vaultio.EncryptWithKey(dek, body)
			if err != nil {
				return err
			}
			_, bin, metaPath, _, err := cachePaths()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
				return err
			}
			// Atomic write: temp + rename. A failed/interrupted pull never corrupts the prior cache.
			tmp := bin + ".tmp"
			if err := os.WriteFile(tmp, blob, 0o600); err != nil {
				return err
			}
			if err := os.Rename(tmp, bin); err != nil {
				os.Remove(tmp)
				return err
			}
			// Best-effort meta (url + pulled_at). A failure here leaves the cache valid.
			mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: time.Now().Unix()})
			_ = os.WriteFile(metaPath, mb, 0o600)

			var snap store.Snapshot
			_ = json.Unmarshal(body, &snap) // for the status line only
			fmt.Fprintf(cmd.ErrOrStderr(), "pulled %d servers / %d credentials into %s\n", len(snap.Servers), len(snap.Credentials), bin)
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "serve broker URL (https://host:7878)")
	c.Flags().StringVar(&token, "token", "", "device authorization code (from `cache-tokens add`)")
	return c
}

func cacheStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cache presence, freshness, and counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, bin, metaPath, _, err := cachePaths()
			if err != nil {
				return err
			}
			snap, err := loadCacheSnapshot()
			if err != nil {
				return err
			}
			info, _ := os.Stat(bin)
			var age string
			if info != nil {
				age = time.Since(info.ModTime()).Round(time.Second).String()
			}
			url := "(unknown)"
			if mb, err := os.ReadFile(metaPath); err == nil {
				var m cacheMeta
				if json.Unmarshal(mb, &m) == nil && m.URL != "" {
					url = m.URL
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cache:    %s\nage:      %s\nservers:  %d\ncreds:    %d\nsource:   %s\n",
				bin, age, len(snap.Servers), len(snap.Credentials), url)
			return nil
		},
	}
}
