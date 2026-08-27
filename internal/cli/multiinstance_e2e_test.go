package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
