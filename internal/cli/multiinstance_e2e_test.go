package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// TestDualInstance_E2E (Plan 40 §6.1/6.2/6.5/6.12): one pinned serve, two
// device codes on two profiles; A and B each pull into their own instance;
// the caches are mutually DEK-isolated; B's project token fails on A's cache;
// revoking A's device code quarantines ONLY A's instance.
func TestDualInstance_E2E(t *testing.T) {
	// --- client-side redirection (spec §9.5: instance tests never set SSHMGR_CACHE_DIR) ---
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	// NB: 不 swap DekProvider seam——默认 var 本就是 FileKeyProvider(CacheDekPathFor)，
	// per-instance DEK 隔离要测的就是这个真实形态（任何前置测试的 swap 都有 t.Cleanup 还原）。

	// --- serve side: gpu→team-a, secret→team-b; projects projA/projB; codes laptop-agentA/B ---
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	src, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cid, _ := src.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	gpuID, _ := src.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	secretID, _ := src.AddServer(&models.Server{Name: "secret", Host: "192.0.2.99", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	profA, _ := src.AddProfile("team-a")
	profB, _ := src.AddProfile("team-b")
	src.GrantServers(profA, []string{gpuID})
	src.GrantServers(profB, []string{secretID})
	_, projTokenA, _ := src.AddProject("proj-a", profA)
	_, projTokenB, _ := src.AddProject("proj-b", profB)
	_, codeA, _ := src.AddCacheToken("laptop-agentA", profA)
	_, codeB, _ := src.AddCacheToken("laptop-agentB", profB)

	// --- pinned TLS serve (ed25519 self-signed, same shape as clientops' helper) ---
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	cert, _ := x509.ParseCertificate(der)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	r, err := mcpserver.NewServeRunner(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewUnstartedServer(r.HTTPHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pin := mcpserver.SPKIFingerprint(cert)

	// --- §6.1 dual pulls into separate slots ---
	for _, tc := range []struct{ instance, code string }{
		{"laptop-agentA", codeA}, {"laptop-agentB", codeB},
	} {
		if _, err := clientops.DoPull(srv.URL, tc.code, pin, clientops.PullOpts{Instance: tc.instance}); err != nil {
			t.Fatalf("pull %s: %v", tc.instance, err)
		}
	}
	iA := filepath.Join(userDir, "ssh-manager", "instances", "laptop-agentA")
	iB := filepath.Join(userDir, "ssh-manager", "instances", "laptop-agentB")
	for _, d := range []string{iA, iB} {
		if _, err := os.Stat(filepath.Join(d, "cache.bin")); err != nil {
			t.Fatalf("%s cache.bin: %v", d, err)
		}
	}

	// --- §6.5 DEK isolation: A's DEK cannot decrypt B's bin ---
	dekA, err := os.ReadFile(filepath.Join(dekDir, "cache-dek-laptop-agentA.key"))
	if err != nil {
		t.Fatal(err)
	}
	binB, _ := os.ReadFile(filepath.Join(iB, "cache.bin"))
	if _, derr := vaultio.DecryptWithKey(dekA, binB); derr == nil {
		t.Fatal("A's DEK must NOT decrypt B's cache.bin")
	}

	// --- §6.1 per-instance loads show each profile's set ---
	snapA, err := clientops.LoadCacheSnapshotFor("laptop-agentA")
	if err != nil || len(snapA.Servers) != 1 || snapA.Servers[0].Name != "gpu" {
		t.Fatalf("A view = %+v, %v", snapA, err)
	}
	snapB, _ := clientops.LoadCacheSnapshotFor("laptop-agentB")
	if len(snapB.Servers) != 1 || snapB.Servers[0].Name != "secret" {
		t.Fatalf("B view = %+v", snapB)
	}

	// --- §6.2 cross fail-closed: B's project token does not verify on A's cache ---
	hyd, err := store.Open(filepath.Join(dir, "hyd.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer hyd.Close()
	if err := hyd.ImportSnapshot(snapA); err != nil {
		t.Fatal(err)
	}
	// NB: 断言按实测修正（disclosed）：VerifyToken 对未知 token 的契约是 (nil, nil)
	// （无匹配 prefix 行 → return nil, rows.Err()——与 GetProjectByName "returns nil, nil
	// when absent" 同一包约定；serve 端 verifyToken 也以 project==nil 判"不通过"），
	// "token 不验证"的判据因此是 proj==nil，不是 verr!=nil。
	if proj, verr := hyd.VerifyToken(projTokenB); proj != nil {
		t.Fatalf("B's project token must NOT validate against A's instance cache (proj=%v, err=%v)", proj, verr)
	}
	if proj, verr := hyd.VerifyToken(projTokenA); verr != nil || proj == nil {
		t.Fatalf("A's own token must validate: %v", verr)
	}

	// --- §6.12 revoke A → A's next pull quarantines ONLY A's slot ---
	if err := src.RevokeCacheToken("laptop-agentA"); err != nil {
		t.Fatal(err)
	}
	_, err = clientops.DoPull(srv.URL, codeA, pin, clientops.PullOpts{Instance: "laptop-agentA"})
	if !errors.Is(err, clientops.ErrCacheQuarantined) {
		t.Fatalf("revoked A pull must quarantine: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(iA, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("A's bin must be gone (moved to quarantine/)")
	}
	if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-laptop-agentA.key")); !os.IsNotExist(serr) {
		t.Fatal("A's per-instance DEK must be deleted")
	}
	if _, serr := os.Stat(filepath.Join(iB, "cache.bin")); serr != nil {
		t.Fatal("B's instance must be untouched")
	}
	if _, lerr := clientops.LoadCacheSnapshotFor("laptop-agentB"); lerr != nil {
		t.Fatalf("B must stay loadable after A's quarantine: %v", lerr)
	}
}

// TestDualInstanceEnroll_E2E (Plan 40 批2 §11.16 + §11.3/§11.7): the enroll
// LIFECYCLE over a real pinned serve, one subtest per §-requirement.
//
// ① a VACUUM machine's bare pull auto-relocates into instances/<header-name>/
//
//	leaving the default slot with zero residue (§1.2);
//
// ② auth persistence follows the slot the pull ACTUALLY landed in (the CLI
//
//	write order DoPull → PullResult.Instance → WriteCacheCredFor);
//
// ③ an explicit --instance enroll opens the second independent slot — and the
//
//	wizard WARNING arm fires when the auth write itself fails;
//
// ④ re-running the bare pull re-relocates IDEMPOTENTLY: same slot, DEK byte-
//
//	identical (§11.7 不重建);
//
// ⑤ swapping device codes back INTO the default slot — keep cache.meta.json +
//
//	cache.config.json per the §2.4 refusal recipe — lands in the DEFAULT
//	directory (never relocates) and records the new identity (§11.3);
//
// ⑥ both instances load their own cropped profile view, mutually isolated.
//
// T9 debt note: only the WARNING TRIGGER (auth-write failure) is provable
// headlessly here; the TUI wizard's WARNING rendering itself stays a
// manual-acceptance item.
func TestDualInstanceEnroll_E2E(t *testing.T) {
	// --- client-side redirection (same posture as TestDualInstance_E2E; spec §9.5:
	// instance tests never set SSHMGR_CACHE_DIR — cleared defensively along with the
	// other single-slot override envs so relocation stays armed) ---
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":         "",
		"SSHMGR_CACHE_DEK":         "",
		"SSHMGR_CACHE_MAX_OFFLINE": "", // hermetic against a developer-shell leak (a leaked cap would skew the §1.2 shape)
	})
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)

	// --- serve side: gpu→team-a, secret→team-b; device codes laptop-agentA/B ---
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	src, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cid, _ := src.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	gpuID, _ := src.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	secretID, _ := src.AddServer(&models.Server{Name: "secret", Host: "192.0.2.99", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	profA, _ := src.AddProfile("team-a")
	profB, _ := src.AddProfile("team-b")
	src.GrantServers(profA, []string{gpuID})
	src.GrantServers(profB, []string{secretID})
	_, codeA, _ := src.AddCacheToken("laptop-agentA", profA)
	_, codeB, _ := src.AddCacheToken("laptop-agentB", profB)

	// --- pinned TLS serve (ed25519 self-signed, same shape as TestDualInstance_E2E) ---
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	cert, _ := x509.ParseCertificate(der)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	r, err := mcpserver.NewServeRunner(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewUnstartedServer(r.HTTPHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pin := mcpserver.SPKIFingerprint(cert)

	defaultDir := filepath.Join(userDir, "ssh-manager")
	iA := filepath.Join(defaultDir, "instances", "laptop-agentA")
	iB := filepath.Join(defaultDir, "instances", "laptop-agentB")
	// The §1.2 vacuum-marker set (clientops.relocate.go): ANY of these present =
	// default-slot intent → a bare pull must NOT relocate.
	defaultMarkers := []string{"cache.bin", "cache.meta.json", "cache.auth.json", "cache.config.json"}
	defaultSlotEmpty := func(t *testing.T) {
		t.Helper()
		for _, f := range defaultMarkers {
			if _, serr := os.Stat(filepath.Join(defaultDir, f)); !os.IsNotExist(serr) {
				t.Fatalf("default slot must carry zero material: %s exists (%v)", f, serr)
			}
		}
	}

	// ① §11.16: a VACUUM machine, bare pull (no --instance) of codeA.
	t.Run("1-bare-pull-vacuum-relocates-to-instance-A", func(t *testing.T) {
		resA, perr := clientops.DoPull(srv.URL, codeA, pin, clientops.PullOpts{})
		if perr != nil {
			t.Fatalf("bare pull codeA: %v", perr)
		}
		if resA.Instance != "laptop-agentA" {
			t.Fatalf("first-enroll auto-relocation must report the header name, got %q", resA.Instance)
		}
		if _, serr := os.Stat(filepath.Join(iA, "cache.bin")); serr != nil {
			t.Fatalf("relocated slot must hold cache.bin: %v", serr)
		}
		// T5 review follow-up: the REAL serve scopes a bound-code snapshot
		// (X-Sshmgr-Snapshot-Scope: profile) and the pulled meta records it.
		if !clientops.CacheScopeVerifiedFor("laptop-agentA") {
			t.Fatal("scope provenance must be recorded for laptop-agentA (serve sent scope=profile)")
		}
		defaultSlotEmpty(t)
	})

	// ② CLI write order (cli/cache.go: DoPull → res.Instance → WriteCacheCredFor):
	// auth follows the pulled SLOT, closing the lazy-refresh chain in instances/.
	t.Run("2-auth-follows-the-slot-the-pull-landed-in", func(t *testing.T) {
		cred := &clientops.CacheCred{URL: srv.URL, Token: codeA, Pin: pin}
		if werr := clientops.WriteCacheCredFor("laptop-agentA", cred); werr != nil {
			t.Fatalf("WriteCacheCredFor(laptop-agentA): %v", werr)
		}
		if _, serr := os.Stat(filepath.Join(iA, "cache.auth.json")); serr != nil {
			t.Fatalf("instances/laptop-agentA/cache.auth.json must exist: %v", serr)
		}
		defaultSlotEmpty(t)
	})

	// ③ §6.x retained combined shape: explicit --instance enroll = second slot.
	// Plus the wizard WARNING arm (T9 debt, best-effort): inject an auth-write
	// failure AFTER the pull succeeds — a DIRECTORY parked at the auth path makes
	// atomicWriteUnique's rename fail — proving the trigger the CLI/TUI turns
	// into "WARNING: could not persist cache.auth.json" instead of an error.
	var binBAroundSwap []byte
	t.Run("3-explicit-instance-B-second-slot-and-wizard-warning-arm", func(t *testing.T) {
		resB, perr := clientops.DoPull(srv.URL, codeB, pin, clientops.PullOpts{Instance: "laptop-agentB"})
		if perr != nil {
			t.Fatalf("explicit --instance pull codeB: %v", perr)
		}
		if resB.Instance != "laptop-agentB" {
			t.Fatalf("explicit pull must land in the flagged instance, got %q", resB.Instance)
		}
		if _, serr := os.Stat(filepath.Join(iB, "cache.bin")); serr != nil {
			t.Fatalf("independent slot must hold cache.bin: %v", serr)
		}
		credB := &clientops.CacheCred{URL: srv.URL, Token: codeB, Pin: pin}
		authPath := filepath.Join(iB, "cache.auth.json")
		if merr := os.Mkdir(authPath, 0o700); merr != nil {
			t.Fatalf("arm auth-write failure: %v", merr)
		}
		if werr := clientops.WriteCacheCredFor("laptop-agentB", credB); werr == nil {
			t.Fatal("auth write onto a DIRECTORY placeholder must fail (the wizard WARNING arm)")
		}
		if rerr := os.Remove(authPath); rerr != nil { // disarm: drop the placeholder dir
			t.Fatalf("disarm auth-write failure: %v", rerr)
		}
		if werr := clientops.WriteCacheCredFor("laptop-agentB", credB); werr != nil {
			t.Fatalf("auth write must succeed once the path is free again: %v", werr)
		}
		b, rerrB := os.ReadFile(filepath.Join(iB, "cache.bin"))
		if rerrB != nil {
			t.Fatalf("snapshot B bin for later cross-write check: %v", rerrB)
		}
		binBAroundSwap = b
	})

	// ④ §11.7 归位幂等: the default slot is STILL vacuum (materials live only in
	// instances/), so a second bare pull re-runs the relocation branch — allowed
	// because the target slot's meta identity matches (idempotent re-relocation).
	// The DEK must NOT be regenerated.
	t.Run("4-bare-repull-idempotent-same-slot-same-dek", func(t *testing.T) {
		dekAPath := filepath.Join(dekDir, "cache-dek-laptop-agentA.key")
		before, rerr := os.ReadFile(dekAPath)
		if rerr != nil {
			t.Fatalf("read A's DEK: %v", rerr)
		}
		resAgain, perr := clientops.DoPull(srv.URL, codeA, pin, clientops.PullOpts{})
		if perr != nil {
			t.Fatalf("idempotent bare pull codeA: %v", perr)
		}
		if resAgain.Instance != "laptop-agentA" {
			t.Fatalf("re-location must stay in the SAME instance slot, got %q", resAgain.Instance)
		}
		after, rerr := os.ReadFile(dekAPath)
		if rerr != nil {
			t.Fatalf("re-read A's DEK: %v", rerr)
		}
		if string(before) != string(after) {
			t.Fatal("DEK must be LOADED, not regenerated, on idempotent re-enroll")
		}
		if _, serr := os.Stat(filepath.Join(iA, "cache.bin")); serr != nil {
			t.Fatalf("slot still holds fresh material: %v", serr)
		}
	})

	// ⑤ §11.3 换码回默认槽: this machine's default slot had a PREVIOUS life (the
	// refusal recipe in gateDefaultInstance says KEEP meta+config when clearing
	// bin/auth/quarantine — they mark it as the DEFAULT slot). Seed exactly that:
	// a legacy meta carrying the OLD device identity plus a valid config. Any
	// further bare pull therefore hits the non-vacuum path, lands IN PLACE, and
	// the rewrite backfills the new device_name (§5 zero-migration).
	t.Run("5-device-code-swap-back-to-default-slot-no-relocation", func(t *testing.T) {
		if merr := os.MkdirAll(defaultDir, 0o700); merr != nil {
			t.Fatal(merr)
		}
		legacyMeta := `{"url":"https://old-broker.example","pulled_at":1700000000,"server_anchored":true,"scoped":true,"device_name":"retired-laptop"}`
		if werr := os.WriteFile(filepath.Join(defaultDir, "cache.meta.json"), []byte(legacyMeta), 0o600); werr != nil {
			t.Fatal(werr)
		}
		if werr := os.WriteFile(filepath.Join(defaultDir, "cache.config.json"), []byte(`{"max_offline":"0"}`), 0o600); werr != nil {
			t.Fatal(werr)
		}
		resSwap, perr := clientops.DoPull(srv.URL, codeB, pin, clientops.PullOpts{})
		if perr != nil {
			t.Fatalf("bare pull codeB over a marked default slot: %v", perr)
		}
		if resSwap.Instance != "" {
			t.Fatalf("marked default slot must receive materials in place, got relocation to %q", resSwap.Instance)
		}
		if _, serr := os.Stat(filepath.Join(defaultDir, "cache.bin")); serr != nil {
			t.Fatalf("materials must land in the DEFAULT directory: %v", serr)
		}
		var m struct {
			URL        string `json:"url"`
			DeviceName string `json:"device_name"`
		}
		mb, rerr := os.ReadFile(filepath.Join(defaultDir, "cache.meta.json"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if jerr := json.Unmarshal(mb, &m); jerr != nil {
			t.Fatal(jerr)
		}
		if m.DeviceName != "laptop-agentB" || m.URL != srv.URL {
			t.Fatalf("pulled meta must record the NEW identity: got url=%q device_name=%q", m.URL, m.DeviceName)
		}
		afterB, rerr := os.ReadFile(filepath.Join(iB, "cache.bin"))
		if rerr != nil {
			t.Fatalf("re-read B's bin: %v", rerr)
		}
		if string(afterB) != string(binBAroundSwap) {
			t.Fatal("instances/laptop-agentB must stay byte-untouched by the default-slot swap")
		}
		if _, derr := os.Stat(filepath.Join(dekDir, "cache-dek.key")); derr != nil {
			t.Fatalf("default slot now has its own DEK: %v", derr)
		}
	})

	// ⑥ combined-load isolation: each instance loads exactly its own cropped
	// profile view (gpu vs secret), never the other's.
	t.Run("6-two-instances-load-isolated", func(t *testing.T) {
		snapA, lerr := clientops.LoadCacheSnapshotFor("laptop-agentA")
		if lerr != nil || len(snapA.Servers) != 1 || snapA.Servers[0].Name != "gpu" {
			t.Fatalf("A view = %+v, %v", snapA, lerr)
		}
		snapB, lerr := clientops.LoadCacheSnapshotFor("laptop-agentB")
		if lerr != nil || len(snapB.Servers) != 1 || snapB.Servers[0].Name != "secret" {
			t.Fatalf("B view = %+v, %v", snapB, lerr)
		}
		snapD, lerr := clientops.LoadCacheSnapshotFor("")
		if lerr != nil || len(snapD.Servers) != 1 || snapD.Servers[0].Name != "secret" {
			t.Fatalf("default (swapped) view = %+v, %v", snapD, lerr)
		}
	})
}
