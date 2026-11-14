package clientops

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// Plan 42 批1 T7 —— `sshmgr pair` 一条龙 e2e:newPairingServer 用真 store
// + 真 ServeRunner(经 LoadPairingSigner 挂上 ed25519 配对签名者)+ httptest
// TLS 暴露完整 HTTPHandler(/pair/* + /snapshot),后台 goroutine 轮询 store
// 首个 pending 行 → store.ApprovePairing 直批(与 TUI/Web 审批面同一入口)。
// 客户端走 RunPair 全流程:enroll → SAS → poll → finish → 落盘 → 首拉。

// pairingServer is the e2e harness for one RunPair flow.
type pairingServer struct {
	url  string // https://127.0.0.1:<port>
	spki string // the TLS cert's SPKI pin (== the serve envelope's spki)
	st   *store.Store
	srv  *httptest.Server
	hits int32 // requests that REACHED the handler (TLS-pin failures never count)
}

// newPairingServer stands up the full pairing + snapshot surface with an
// ed25519 serve certificate. The auto-approver approves the FIRST pending row
// with profile "team-a".
func newPairingServer(t *testing.T) *pairingServer {
	return newPairingServerForSigner(t, newEd25519PairKey)
}

// newPairingServerForSigner stands up the full pairing + snapshot surface
// with a serve certificate minted from the given key factory. The
// auto-approver approves the FIRST pending row with profile "team-a".
func newPairingServerForSigner(t *testing.T, newKey func(*testing.T) (crypto.Signer, crypto.PublicKey)) *pairingServer {
	t.Helper()
	signer, publicKey := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pair-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, publicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	certDir := t.TempDir()
	certPath := filepath.Join(certDir, "cert.pem")
	keyPath := filepath.Join(certDir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	profID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := mcpserver.NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.LoadPairingSigner(certPath, keyPath); err != nil {
		t.Fatal(err)
	}

	ps := &pairingServer{st: st, spki: mcpserver.SPKIFingerprint(cert)}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ps.hits, 1)
		runner.HTTPHandler().ServeHTTP(w, r)
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: signer}}}
	srv.StartTLS()
	ps.srv = srv
	ps.url = srv.URL

	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		srv.Close()
		st.Close()
	})
	go func() { // 自动批准:首个 pending 行 → CAS 置 approved(批 profile)
		for {
			select {
			case <-done:
				return
			default:
			}
			rows, err := st.ListPendingPairing()
			if err == nil {
				for i := range rows {
					if rows[i].State == "pending" {
						_, _ = st.ApprovePairing(rows[i].ID, profID)
						return
					}
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()
	return ps
}

// newEd25519PairKey creates the pre-change serve certificate key family.
func newEd25519PairKey(t *testing.T) (crypto.Signer, crypto.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, publicKey
}

// newECDSAP256PairKey creates the new default serve certificate key family.
func newECDSAP256PairKey(t *testing.T) (crypto.Signer, crypto.PublicKey) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, privateKey.Public()
}

// pairOpts is the common RunPair input with per-test overrides.
func pairOpts(srv *pairingServer, instance string, mutate func(*PairOpts)) PairOpts {
	o := PairOpts{
		URL: srv.url, Pin: srv.spki, AssumeSAS: true, Instance: instance,
		Stdin:  strings.NewReader("\n"),
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
	if mutate != nil {
		mutate(&o)
	}
	return o
}

// withClientRoleRoot isolates role.json (written on pair completion) from the
// operator's real user configuration directory and vault directory.
func withClientRoleRoot(t *testing.T) {
	t.Helper()
	withEnv(t, map[string]string{
		"SSHMGR_CONFIG_ROOT": t.TempDir(),
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "absent-store.db"),
	})
}

func TestRunPair_EndToEnd(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	withClientRoleRoot(t)

	var out, errb bytes.Buffer
	o := pairOpts(srv, "it-laptop", func(p *PairOpts) { p.Stdout, p.Stderr = &out, &errb })
	if err := RunPair(o); err != nil {
		t.Fatalf("RunPair: %v\nstdout=%s stderr=%s", err, out.String(), errb.String())
	}

	// 凭据已落盘(URL/pin/真值设备码)。
	cred, err := ReadCacheCredFor("it-laptop")
	if err != nil || cred == nil {
		t.Fatalf("cache.auth.json: cred=%+v err=%v", cred, err)
	}
	if cred.URL != srv.url {
		t.Fatalf("cred.URL = %q, want %q", cred.URL, srv.url)
	}
	if cred.Pin != srv.spki {
		t.Fatalf("cred.Pin = %q, want the serve pin", cred.Pin)
	}
	if cred.Token == "" {
		t.Fatal("cred.Token (device code) must be non-empty")
	}
	if _, active, err := srv.st.ActiveCacheTokenInfo("it-laptop"); err != nil || !active {
		t.Fatalf("mint must leave the device active: active=%v err=%v", active, err)
	}

	// 产物:完整 .mcp.json,真值 token,无占位符。
	b, err := os.ReadFile(filepath.Join(dir, "pair.it-laptop.mcp.json"))
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if !bytes.Contains(b, []byte("SSHMGR_TOKEN")) {
		t.Fatalf("artifact missing SSHMGR_TOKEN: %s", b)
	}
	if bytes.Contains(b, []byte("<project-token>")) {
		t.Fatal("artifact must carry the REAL token, not the <project-token> placeholder")
	}
	for _, frag := range []string{`"sshmgr"`, `"mcp"`, `"--cache"`, `"--instance"`, `"it-laptop"`} {
		if !bytes.Contains(b, []byte(frag)) {
			t.Fatalf("artifact missing %s: %s", frag, b)
		}
	}

	// 先落盘后首拉:cache.bin 已在,且用内存 DEK 可解。
	if _, err := os.Stat(filepath.Join(dir, "cache.bin")); err != nil {
		t.Fatalf("first pull missing: %v", err)
	}
	if _, err := LoadCacheSnapshotFor("it-laptop"); err != nil {
		t.Fatalf("pulled cache must load: %v", err)
	}

	// cache.config.json 持久化了信封的 max_offline(serve 默认 24h)。
	cfg, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if err != nil || !bytes.Contains(cfg, []byte("24h")) {
		t.Fatalf("cache.config.json = %s err=%v, want max_offline 24h", cfg, err)
	}

	// stdout:SAS 三件套 + 已授权 profile。
	if !bytes.Contains(out.Bytes(), []byte("it-laptop @ ")) || !bytes.Contains(out.Bytes(), []byte(" SAS ")) {
		t.Fatalf("stdout missing the SAS three-piece line: %s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("已授权 profile: team-a")) {
		t.Fatalf("stdout missing the granted-profile line: %s", out.String())
	}
}

// TestRunPair_EndToEnd_ECDSAAndEd25519ServeCerts pins the CLIENT half of the
// serve certificate key-family matrix: the enroll response signature must be
// verified by the certificate's own family — P-256 (the new default) signs
// the SHA-256 transcript digest, ed25519 (already deployed) signs the raw
// transcript — and pairing must complete end to end for both.
func TestRunPair_EndToEnd_ECDSAAndEd25519ServeCerts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		newKey func(*testing.T) (crypto.Signer, crypto.PublicKey)
	}{
		{"ecdsa-p256", newECDSAP256PairKey},
		{"ed25519", newEd25519PairKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newPairingServerForSigner(t, tc.newKey)
			dir := t.TempDir()
			withDEK(t)
			withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
			withClientRoleRoot(t)

			var out bytes.Buffer
			if err := RunPair(pairOpts(srv, "it-"+tc.name, func(p *PairOpts) {
				p.Stdout, p.Stderr = &out, &out
			})); err != nil {
				t.Fatalf("RunPair: %v\nstdout=%s", err, out.String())
			}
			cred, err := ReadCacheCredFor("it-" + tc.name)
			if err != nil || cred == nil {
				t.Fatalf("cache.auth.json: cred=%+v err=%v", cred, err)
			}
			if cred.Pin != srv.spki {
				t.Fatalf("cred.Pin = %q, want the serve certificate pin %q", cred.Pin, srv.spki)
			}
			if _, err := LoadCacheSnapshotFor("it-" + tc.name); err != nil {
				t.Fatalf("first pull must land a readable cache snapshot: %v", err)
			}
		})
	}
}

// TestRunPair_WritesClientRoleForDialDevice reproduces the first-anchoring
// defect: pairing used to leave no client role.json behind, so
// dialdevice.Current answered "role-unknown" and every first anchoring of a
// topology-backed entry was refused. Pair completion must record the client
// role; after the authority associates the device code with a topology
// device and the client pulls again, Current must identify this machine.
func TestRunPair_WritesClientRoleForDialDevice(t *testing.T) {
	srv := newPairingServer(t)
	cacheDir := t.TempDir()
	configRoot := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_CONFIG_ROOT": configRoot,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "absent-store.db"),
	})

	if err := RunPair(pairOpts(srv, "role-laptop", nil)); err != nil {
		t.Fatalf("RunPair: %v", err)
	}
	state, err := roles.Load()
	if err != nil {
		t.Fatalf("roles.Load after pair: %v", err)
	}
	if state == nil || state.Role != roles.RoleClient || !state.SetupComplete {
		t.Fatalf("role.json after pair = %+v, want role=client setup_complete=true", state)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "ssh-manager", "role.json")); err != nil {
		t.Fatalf("client role.json must live beside the client cache data: %v", err)
	}

	// The owner associates the new device code with a topology device, the
	// client pulls the refreshed snapshot, and the dialer must now resolve
	// this machine's own device.
	device, err := srv.st.AddDevice("office-device", "", "role-laptop")
	if err != nil {
		t.Fatalf("AddDevice bound to the paired device code: %v", err)
	}
	cred, err := ReadCacheCredFor("role-laptop")
	if err != nil || cred == nil {
		t.Fatalf("ReadCacheCredFor: cred=%+v err=%v", cred, err)
	}
	if _, err := DoPull(cred.URL, cred.Token, cred.Pin, PullOpts{Instance: "role-laptop", StatusOut: io.Discard}); err != nil {
		t.Fatalf("second pull after device association: %v", err)
	}
	snapshot, err := LoadCacheSnapshotFor("role-laptop")
	if err != nil {
		t.Fatalf("LoadCacheSnapshotFor: %v", err)
	}
	temporary, err := store.OpenTemporaryClientCacheStore(snapshot, "pair-role-*.db")
	if err != nil {
		t.Fatalf("OpenTemporaryClientCacheStore: %v", err)
	}
	t.Cleanup(func() { _ = temporary.Close() })
	current, err := dialdevice.Current(temporary.Store, "role-laptop")
	if err != nil {
		t.Fatalf("dialdevice.Current: %v", err)
	}
	if !current.Known || current.Device == nil || current.Device.ID != device.ID {
		t.Fatalf("dialdevice.Current = %+v, want the device associated with the paired device code (%s)", current, device.ID)
	}
}

// TestRunPair_KeepsExistingNonClientRole pins that pairing a client cache on
// the authoritative machine itself succeeds without rewriting that machine's
// authoritative role.
func TestRunPair_KeepsExistingNonClientRole(t *testing.T) {
	srv := newPairingServer(t)
	cacheDir := t.TempDir()
	configRoot := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_CONFIG_ROOT": configRoot,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "vault.db"),
	})
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatalf("seed server role: %v", err)
	}

	if err := RunPair(pairOpts(srv, "downgrade-laptop", nil)); err != nil {
		t.Fatalf("RunPair must keep an existing non-client role.json and still succeed: %v", err)
	}
	state, loadErr := roles.Load()
	if loadErr != nil || state == nil || state.Role != roles.RoleServer {
		t.Fatalf("server role.json must survive the pairing: state=%+v err=%v", state, loadErr)
	}
}

func TestRunPair_FirstPullFails_ArtifactAlreadyOnDisk(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	withClientRoleRoot(t)

	pairBeforePullTestHook = func() { srv.srv.Close() } // 首拉前强杀 server(确定性)
	defer func() { pairBeforePullTestHook = nil }()

	err := RunPair(pairOpts(srv, "kill-later", nil))
	if err == nil {
		t.Fatal("RunPair must fail when the first pull cannot reach the server")
	}
	if !strings.Contains(err.Error(), "first pull") {
		t.Fatalf("error should name the first-pull step: %v", err)
	}
	// 产物与凭据已在盘(先落盘后首拉的冻结次序)。
	if _, serr := os.Stat(filepath.Join(dir, "pair.kill-later.mcp.json")); serr != nil {
		t.Fatalf("artifact must already be on disk: %v", serr)
	}
	if cred, cerr := ReadCacheCredFor("kill-later"); cerr != nil || cred == nil {
		t.Fatalf("cache.auth.json must already be on disk: cred=%+v err=%v", cred, cerr)
	}
	// 拉取失败 → 无 cache.bin。
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatalf("failed first pull must not leave cache.bin (stat err=%v)", serr)
	}
}

func TestRunPair_TOFUDefaultRefused(t *testing.T) {
	err := RunPair(PairOpts{
		URL: "https://127.0.0.1:7878", Instance: "tofu-dev",
		Stdin: strings.NewReader("\n"), Stdout: io.Discard, Stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing TOFU") {
		t.Fatalf("want the frozen TOFU refusal, got %v", err)
	}
}

func TestRunPair_PinMismatchAborts(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	withClientRoleRoot(t)

	wrong := "sha256:" + strings.Repeat("ab", 32)
	err := RunPair(pairOpts(srv, "pinmiss", func(p *PairOpts) { p.Pin = wrong }))
	if err == nil {
		t.Fatal("a wrong pin must abort the pairing at the TLS layer")
	}
	if n := atomic.LoadInt32(&srv.hits); n != 0 {
		t.Fatalf("no request may reach the server handler on a pin mismatch, got %d", n)
	}
}

func TestRunPair_SameNameNeedsForce(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	withClientRoleRoot(t)

	// 预置已 enroll 态 + 干扰物(bin/meta/quarantine)+ 保留对象(config)。
	if err := WriteCacheCredFor("re-pair", &CacheCred{URL: "https://old:7878", Token: "old-code", Pin: srv.spki}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("OLD-CACHE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"https://old:7878"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteCacheConfig(dir, "24h"); err != nil {
		t.Fatal(err)
	}

	// 默认拒(冻结文案)。
	err := RunPair(pairOpts(srv, "re-pair", nil))
	if err == nil || !strings.Contains(err.Error(), "instance already enrolled; pass --force") {
		t.Fatalf("want the frozen already-enrolled refusal, got %v", err)
	}

	// --force 后成功,cache.config.json 保留,quarantine/ 清掉,cache.bin 换新。
	if err := RunPair(pairOpts(srv, "re-pair", func(p *PairOpts) { p.Force = true })); err != nil {
		t.Fatalf("RunPair --force: %v", err)
	}
	if cfg, cerr := os.ReadFile(filepath.Join(dir, "cache.config.json")); cerr != nil || len(cfg) == 0 {
		t.Fatalf("cache.config.json must survive --force: %s err=%v", cfg, cerr)
	}
	if _, serr := os.Stat(filepath.Join(dir, "quarantine")); !os.IsNotExist(serr) {
		t.Fatalf("force must remove the quarantine/ subtree (stat err=%v)", serr)
	}
	if b, rerr := os.ReadFile(filepath.Join(dir, "cache.bin")); rerr != nil || bytes.Equal(b, []byte("OLD-CACHE")) {
		t.Fatalf("cache.bin must be replaced by the fresh pull (err=%v)", rerr)
	}
	cred, err := ReadCacheCredFor("re-pair")
	if err != nil || cred == nil || cred.URL != srv.url {
		t.Fatalf("credential must be rewritten for the new pairing: %+v err=%v", cred, err)
	}
}

// TestRunPair_ForceBadURLKeepsCredentials pins fix round 1 I1: pure-input
// validation (bad target URL, and the same-class malformed pin) must precede
// the --force cleanup — a typo'd URL must error out and leave the IN-USE
// credentials byte-identical on disk, never silently destroyed.
func TestRunPair_ForceBadURLKeepsCredentials(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	goodPin := "sha256:" + strings.Repeat("ab", 32)
	if err := WriteCacheCredFor("force-url", &CacheCred{URL: "https://old:7878", Token: "in-use-code", Pin: goodPin}); err != nil {
		t.Fatal(err)
	}

	// 拼错的 scheme + --force → 报错文案,凭据原样在盘。
	err := RunPair(PairOpts{
		URL: "htts://127.0.0.1:7878", Pin: goodPin, Force: true, Instance: "force-url",
		AssumeSAS: true, Stdin: strings.NewReader("\n"), Stdout: io.Discard, Stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "must be https://") {
		t.Fatalf("want the bad-scheme refusal, got %v", err)
	}
	cred, cerr := ReadCacheCredFor("force-url")
	if cerr != nil || cred == nil || cred.URL != "https://old:7878" || cred.Token != "in-use-code" {
		t.Fatalf("--force with a bad URL must leave credentials untouched: %+v err=%v", cred, cerr)
	}

	// 同类不变量:坏 pin(pinningTransport 拒)同样不得消耗一次 force 销毁。
	err = RunPair(PairOpts{
		URL: "https://127.0.0.1:7878", Pin: "not-a-pin", Force: true, Instance: "force-url",
		AssumeSAS: true, Stdin: strings.NewReader("\n"), Stdout: io.Discard, Stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid server pin") {
		t.Fatalf("want the malformed-pin refusal, got %v", err)
	}
	if cred, cerr := ReadCacheCredFor("force-url"); cerr != nil || cred == nil || cred.URL != "https://old:7878" {
		t.Fatalf("credentials must survive a malformed-pin refusal: %+v err=%v", cred, cerr)
	}
}

// TestForceCleanInstance_KeepsConfig pins the --force deletion set directly
// (the e2e test above cannot observe preservation — RunPair rewrites
// cache.config.json after the clean): enroll-state files die, the Plan-40
// offline-cap policy survives untouched.
func TestForceCleanInstance_KeepsConfig(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	for name, body := range map[string]string{
		"cache.auth.json":   `{"url":"https://old:7878","token":"c"}`,
		"cache.bin":         "OLD",
		"cache.meta.json":   `{}`,
		"cache.config.json": `{"max_offline":"72h"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := forceCleanInstance("any-dev"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cache.auth.json", "cache.bin", "cache.meta.json"} {
		if _, serr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(serr) {
			t.Fatalf("%s must be removed by --force (stat err=%v)", name, serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "quarantine")); !os.IsNotExist(serr) {
		t.Fatalf("quarantine/ must be removed by --force (stat err=%v)", serr)
	}
	cfg, serr := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if serr != nil || !bytes.Contains(cfg, []byte("72h")) {
		t.Fatalf("cache.config.json must be PRESERVED byte-for-byte: %s err=%v", cfg, serr)
	}
	// 幂等:再清一次(文件已缺)不报错。
	if err := forceCleanInstance("any-dev"); err != nil {
		t.Fatalf("force-clean must be idempotent: %v", err)
	}
}
