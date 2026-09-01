package clientops

// Plan 45 T1 —— PairSession 分步状态机的失败先行测试(brief Step 1 清单):
// NewPairSession 校验矩阵(URL 非法 / pin 空且无 TOFU 拒绝 / URL 空=发现流不
// 校验)、Bind 等价校验 + transport 重建、ForceCleanup 前置(未校验调用报错;
// 坏 URL/坏 pin 清理不触发 = TestRunPair_ForceBadURLKeepsCredentials 的
// session 级等价)、IsEnrolled、Enroll/WaitApproval 三终态可区分
// (410→ErrPairGone / 缩窗→ErrPairTimeout / ctx 取消→context.Canceled)、
// 429 backoff note 触发且 30s 节流、Finish/WriteAndPull(落盘次序钩子保留)、
// reflect 钉:PairSession 无导出密钥材料字段。
//
// 三终态/节流用例用 fakePairServer(enroll 真签名通过,poll 行为注入);
// Finish/WriteAndPull 用真 harness newPairingServer(pair_test.go,零改动复用)。

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

// testValidPin is a well-formed (but not server-anchored) sha256 pin.
func testValidPin() string { return "sha256:" + strings.Repeat("ab", 32) }

// fakePairServer stands up a TLS pairing serve whose enroll is REAL (the
// ed25519 cert key recomputes and signs the frozen transcript, so the client's
// enroll verification passes) and whose /pair/poll behavior is injected by the
// test (a status list; the last entry repeats). These use cases stop at
// WaitApproval — finish is never reached.
type fakePairServer struct {
	url    string
	spki   string
	edPriv ed25519.PrivateKey
	xPub   []byte // 32B X25519 public half — the client's ECDH only needs a valid 32B input

	mu       sync.Mutex
	statuses []int
	polls    int
}

func newFakePairServer(t *testing.T, statuses ...int) *fakePairServer {
	t.Helper()
	if len(statuses) == 0 {
		statuses = []int{http.StatusAccepted}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pair-fake-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	xpriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakePairServer{edPriv: priv, xPub: xpriv.PublicKey().Bytes(), statuses: statuses}
	mux := http.NewServeMux()
	mux.HandleFunc("/pair/enroll", func(w http.ResponseWriter, r *http.Request) {
		var req pairEnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		clientPub, _ := base64.RawURLEncoding.DecodeString(req.ClientPub)
		cnonce, _ := base64.RawURLEncoding.DecodeString(req.Cnonce)
		id, _ := hex.DecodeString(req.ID)
		snonce := make([]byte, 16)
		if _, err := rand.Read(snonce); err != nil {
			t.Error(err)
			return
		}
		// 与 client 同一冻结拼装(pairDomainPrefix 镜像),证书私钥签署 →
		// 客户端验签必过。
		transcript := buildPairTranscript(id, req.Name, req.TargetURL, clientPub, cnonce, fs.xPub, snonce)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pairEnrollResponse{
			ServerPub: base64.RawURLEncoding.EncodeToString(fs.xPub),
			Snonce:    base64.RawURLEncoding.EncodeToString(snonce),
			Sig:       base64.RawURLEncoding.EncodeToString(ed25519.Sign(fs.edPriv, transcript)),
		})
	})
	mux.HandleFunc("/pair/poll", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		st := fs.statuses[len(fs.statuses)-1]
		if fs.polls < len(fs.statuses) {
			st = fs.statuses[fs.polls]
		}
		fs.polls++
		fs.mu.Unlock()
		w.WriteHeader(st)
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}}}
	srv.StartTLS()
	fs.url = srv.URL
	fs.spki = mcpserver.SPKIFingerprint(cert)
	t.Cleanup(srv.Close)
	return fs
}

func (fs *fakePairServer) pollCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.polls
}

// shrinkPollWindow swaps the package-level poll seam for a fast test window
// and restores both values on cleanup.
func shrinkPollWindow(t *testing.T, interval, max time.Duration) {
	t.Helper()
	oi, om := pairPollInterval, pairPollMax
	pairPollInterval, pairPollMax = interval, max
	t.Cleanup(func() { pairPollInterval, pairPollMax = oi, om })
}

// sessionOpts is the common PairOpts for direct session driving.
func sessionOpts(srvURL, pin, instance string) PairOpts {
	return PairOpts{
		URL: srvURL, Pin: pin, Instance: instance,
		Stdin:  strings.NewReader("\n"),
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
}

func TestNewPairSession_ValidationMatrix(t *testing.T) {
	validPin := testValidPin()

	// 非法 URL(scheme)→ 冻结文案拒绝,零 IO。
	if _, err := NewPairSession(PairOpts{URL: "htts://127.0.0.1:7878", Pin: validPin, Instance: "matrix-dev"}); err == nil || !strings.Contains(err.Error(), "must be https://") {
		t.Fatalf("want the bad-scheme refusal, got %v", err)
	}
	// pin 空且未显式 TOFU → 冻结文案拒绝。
	if _, err := NewPairSession(PairOpts{URL: "https://127.0.0.1:7878", Instance: "matrix-dev"}); err == nil || !strings.Contains(err.Error(), "refusing TOFU") {
		t.Fatalf("want the frozen TOFU refusal, got %v", err)
	}
	// URL 空 = 发现流:不做 URL/TOFU 校验,原样放行(校验推迟到 Bind)。
	s, err := NewPairSession(PairOpts{Instance: "matrix-dev"})
	if err != nil || s == nil {
		t.Fatalf("URL-empty (discovery) must construct without validation, got s=%v err=%v", s, err)
	}
	// instance 必填。
	if _, err := NewPairSession(PairOpts{URL: "https://127.0.0.1:7878", Pin: validPin}); err == nil || !strings.Contains(err.Error(), "pair requires --instance") {
		t.Fatalf("want the frozen --instance requirement, got %v", err)
	}
	// instance 非法 → instname 校验拒绝。
	if _, err := NewPairSession(PairOpts{URL: "https://127.0.0.1:7878", Pin: validPin, Instance: "bad/name"}); err == nil || !strings.Contains(err.Error(), "invalid device name") {
		t.Fatalf("want the device-name refusal, got %v", err)
	}
}

func TestBind_EquivalentValidationAndTransport(t *testing.T) {
	validPin := testValidPin()

	// 发现流会话。
	s, err := NewPairSession(PairOpts{Instance: "bind-dev"})
	if err != nil {
		t.Fatalf("discovery-flow NewPairSession: %v", err)
	}
	// Addr 空 → 无 host,拒绝。
	if err := s.Bind(Discovered{TCPPort: 7878, SPKI: validPin}); err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("Bind without Addr must refuse (no host), got %v", err)
	}
	// 畸形 SPKI(opts.Pin 空 → SPKI 升格为 pin)→ pinningTransport 拒。
	if err := s.Bind(Discovered{Addr: "127.0.0.1", TCPPort: 7878, SPKI: "not-a-pin"}); err == nil || !strings.Contains(err.Error(), "invalid server pin") {
		t.Fatalf("Bind with malformed SPKI must refuse at the transport, got %v", err)
	}
	// SPKI 空 + 无 TOFU → 冻结文案拒绝。
	if err := s.Bind(Discovered{Addr: "127.0.0.1", TCPPort: 7878}); err == nil || !strings.Contains(err.Error(), "refusing TOFU") {
		t.Fatalf("Bind without SPKI and without TOFU must refuse, got %v", err)
	}
	// 合法 → Bind 成功,等价校验 + transport 就绪;broker 名随 Discovered 记录。
	s2, err := NewPairSession(PairOpts{Instance: "bind-ok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Bind(Discovered{Name: "nuc10", Addr: "127.0.0.1", TCPPort: 7878, SPKI: validPin}); err != nil {
		t.Fatalf("Bind with a well-formed offer must succeed: %v", err)
	}
	if s2.BrokerName() != "nuc10" {
		t.Fatalf("BrokerName = %q, want nuc10", s2.BrokerName())
	}
}

func TestForceCleanup_RequiresValidatedSession(t *testing.T) {
	// 发现流且未 Bind:无任何校验,清理入口必须拒绝(带调用前提说明)。
	s, err := NewPairSession(PairOpts{Instance: "fc-dev"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.ForceCleanup()
	if err == nil {
		t.Fatal("ForceCleanup before any validation must refuse")
	}
	if !strings.Contains(err.Error(), "Bind") {
		t.Fatalf("the refusal must state the prerequisite (New with URL / Bind), got %v", err)
	}
}

// TestForceCleanup_BadInputKeepsCredentials is the session-level equivalent of
// TestRunPair_ForceBadURLKeepsCredentials (fix round 1 I1): a bad URL or a
// malformed pin must refuse at NewPairSession — BEFORE ForceCleanup can ever
// run — leaving the in-use credentials byte-identical on disk.
func TestForceCleanup_BadInputKeepsCredentials(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	validPin := testValidPin()
	if err := WriteCacheCredFor("fc-keep", &CacheCred{URL: "https://old:7878", Token: "in-use-code", Pin: validPin}); err != nil {
		t.Fatal(err)
	}

	// 坏 pin:New 阶段拒绝,清理绝不触发。
	if _, err := NewPairSession(PairOpts{URL: "https://127.0.0.1:7878", Pin: "not-a-pin", Force: true, Instance: "fc-keep"}); err == nil || !strings.Contains(err.Error(), "invalid server pin") {
		t.Fatalf("want the malformed-pin refusal, got %v", err)
	}
	if cred, cerr := ReadCacheCredFor("fc-keep"); cerr != nil || cred == nil || cred.URL != "https://old:7878" {
		t.Fatalf("credentials must survive a malformed-pin refusal: %+v err=%v", cred, cerr)
	}

	// 坏 URL:同样在 New 阶段拒绝。
	if _, err := NewPairSession(PairOpts{URL: "htts://127.0.0.1:7878", Pin: validPin, Force: true, Instance: "fc-keep"}); err == nil || !strings.Contains(err.Error(), "must be https://") {
		t.Fatalf("want the bad-scheme refusal, got %v", err)
	}
	if cred, cerr := ReadCacheCredFor("fc-keep"); cerr != nil || cred == nil || cred.Token != "in-use-code" {
		t.Fatalf("credentials must survive a bad-URL refusal: %+v err=%v", cred, cerr)
	}
}

func TestForceCleanup_DeletesEnrollStateKeepsConfig(t *testing.T) {
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

	s, err := NewPairSession(PairOpts{URL: "https://127.0.0.1:7878", Pin: testValidPin(), Instance: "fc-clean"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ForceCleanup(); err != nil {
		t.Fatalf("ForceCleanup on a validated session must run: %v", err)
	}
	for _, name := range []string{"cache.auth.json", "cache.bin", "cache.meta.json"} {
		if _, serr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(serr) {
			t.Fatalf("%s must be removed (stat err=%v)", name, serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "quarantine")); !os.IsNotExist(serr) {
		t.Fatalf("quarantine/ must be removed (stat err=%v)", serr)
	}
	if cfg, serr := os.ReadFile(filepath.Join(dir, "cache.config.json")); serr != nil || !bytes.Contains(cfg, []byte("72h")) {
		t.Fatalf("cache.config.json must be PRESERVED: %s err=%v", cfg, serr)
	}
	// 幂等:再清一次(文件已缺)不报错。
	if err := s.ForceCleanup(); err != nil {
		t.Fatalf("ForceCleanup must be idempotent: %v", err)
	}
}

func TestIsEnrolled(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

	enrolled, err := IsEnrolled("is-enr")
	if err != nil || enrolled {
		t.Fatalf("fresh instance must be un-enrolled: enrolled=%v err=%v", enrolled, err)
	}
	if err := WriteCacheCredFor("is-enr", &CacheCred{URL: "https://b:7878", Token: "c", Pin: testValidPin()}); err != nil {
		t.Fatal(err)
	}
	enrolled, err = IsEnrolled("is-enr")
	if err != nil || !enrolled {
		t.Fatalf("instance with cache.auth.json must be enrolled: enrolled=%v err=%v", enrolled, err)
	}
	if _, err := IsEnrolled("bad/name"); err == nil {
		t.Fatal("an invalid instance name must error, not report a boolean")
	}
}

// TestPairSession_TerminalStates pins the THREE distinguishable terminal states
// of WaitApproval: 410 → ErrPairGone (broker-merged semantics), local deadline →
// ErrPairTimeout, ctx cancel → context.Canceled. Each must NOT match the other two.
func TestPairSession_TerminalStates(t *testing.T) {
	t.Run("410_gone", func(t *testing.T) {
		fs := newFakePairServer(t, http.StatusGone)
		s, err := NewPairSession(sessionOpts(fs.url, fs.spki, "tri-gone"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Enroll(context.Background()); err != nil {
			t.Fatalf("Enroll: %v", err)
		}
		err = s.WaitApproval(context.Background(), nil)
		if !errors.Is(err, ErrPairGone) {
			t.Fatalf("want ErrPairGone, got %v", err)
		}
		if errors.Is(err, ErrPairTimeout) || errors.Is(err, context.Canceled) {
			t.Fatalf("ErrPairGone must not match the other terminal states: %v", err)
		}
	})

	t.Run("deadline_timeout", func(t *testing.T) {
		shrinkPollWindow(t, 10*time.Millisecond, 100*time.Millisecond)
		fs := newFakePairServer(t, http.StatusAccepted)
		s, err := NewPairSession(sessionOpts(fs.url, fs.spki, "tri-timeout"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Enroll(context.Background()); err != nil {
			t.Fatalf("Enroll: %v", err)
		}
		err = s.WaitApproval(context.Background(), nil)
		if !errors.Is(err, ErrPairTimeout) {
			t.Fatalf("want ErrPairTimeout, got %v", err)
		}
		if errors.Is(err, ErrPairGone) || errors.Is(err, context.Canceled) {
			t.Fatalf("ErrPairTimeout must not match the other terminal states: %v", err)
		}
		if fs.pollCount() < 2 {
			t.Fatalf("the poll loop must have retried before the window closed, polls=%d", fs.pollCount())
		}
	})

	t.Run("ctx_cancel", func(t *testing.T) {
		fs := newFakePairServer(t, http.StatusAccepted)
		s, err := NewPairSession(sessionOpts(fs.url, fs.spki, "tri-cancel"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Enroll(context.Background()); err != nil {
			t.Fatalf("Enroll: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		note := func(n PollNote) {
			if n.Pending {
				cancel() // TUI Esc 语义:等待阶段取消 ctx
			}
		}
		err = s.WaitApproval(ctx, note)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
		if errors.Is(err, ErrPairGone) || errors.Is(err, ErrPairTimeout) {
			t.Fatalf("context.Canceled must not match the other terminal states: %v", err)
		}
	})
}

// TestWaitApproval_BackoffNoteThrottled pins the 429 note contract: the backoff
// note FIRES (with the frozen transient wording in Detail) and is THROTTLED to
// at most one per 30s even across many poll rounds.
func TestWaitApproval_BackoffNoteThrottled(t *testing.T) {
	shrinkPollWindow(t, 5*time.Millisecond, 200*time.Millisecond)
	fs := newFakePairServer(t, http.StatusTooManyRequests)
	s, err := NewPairSession(sessionOpts(fs.url, fs.spki, "note-429"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(context.Background()); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	var backoff, pending int
	note := func(n PollNote) {
		if n.Backoff {
			backoff++
			if !strings.Contains(n.Detail, "429") {
				t.Errorf("backoff Detail must carry the frozen transient wording, got %q", n.Detail)
			}
		}
		if n.Pending {
			pending++
		}
	}
	err = s.WaitApproval(context.Background(), note)
	if !errors.Is(err, ErrPairTimeout) {
		t.Fatalf("a 429-forever serve must end in ErrPairTimeout once the window closes, got %v", err)
	}
	if backoff != 1 {
		t.Fatalf("429 backoff note must fire exactly once per 30s window (got %d across %d polls)", backoff, fs.pollCount())
	}
	if pending != 0 {
		t.Fatalf("429 rounds must not emit pending notes, got %d", pending)
	}
	if fs.pollCount() < 3 {
		t.Fatalf("the loop must have polled multiple rounds for the throttle to matter, polls=%d", fs.pollCount())
	}
}

// TestPairSession_HappyPath drives the full session pipeline twice against the
// REAL harness: once by explicit URL and once through the discovery flow
// (New URL-empty → Bind), proving Bind rebuilt a WORKING pinned transport.
func TestPairSession_HappyPath(t *testing.T) {
	t.Run("url_path", func(t *testing.T) {
		srv := newPairingServer(t)
		dir := t.TempDir()
		withDEK(t)
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
		ctx := context.Background()

		if enrolled, err := IsEnrolled("sess-laptop"); err != nil || enrolled {
			t.Fatalf("pre-state must be un-enrolled: %v %v", enrolled, err)
		}
		s, err := NewPairSession(sessionOpts(srv.url, srv.spki, "sess-laptop"))
		if err != nil {
			t.Fatalf("NewPairSession: %v", err)
		}
		if err := s.Enroll(ctx); err != nil {
			t.Fatalf("Enroll: %v", err)
		}
		if len(s.SAS()) != 6 {
			t.Fatalf("SAS() = %q, want the 6-digit short auth string", s.SAS())
		}
		if !s.ApprovalDeadline().After(time.Now()) {
			t.Fatalf("ApprovalDeadline() = %v, want an absolute future anchor", s.ApprovalDeadline())
		}
		if err := s.WaitApproval(ctx, nil); err != nil {
			t.Fatalf("WaitApproval: %v", err)
		}
		if err := s.Finish(ctx); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		if s.AuthorizedProfile() != "team-a" {
			t.Fatalf("AuthorizedProfile() = %q, want team-a", s.AuthorizedProfile())
		}

		// 落盘次序钩子保留:钩子触发时凭据与产物必须已在盘(先落盘后首拉)。
		hooked := false
		pairBeforePullTestHook = func() {
			cred, cerr := ReadCacheCredFor("sess-laptop")
			_, aerr := os.Stat(filepath.Join(dir, "pair.sess-laptop.mcp.json"))
			hooked = cerr == nil && cred != nil && aerr == nil
		}
		defer func() { pairBeforePullTestHook = nil }()

		res, err := s.WriteAndPull(ctx)
		if err != nil {
			t.Fatalf("WriteAndPull: %v", err)
		}
		if res.Instance != "sess-laptop" {
			t.Fatalf("PullResult.Instance = %q, want sess-laptop", res.Instance)
		}
		if !hooked {
			t.Fatal("pairBeforePullTestHook must observe the pre-pull files ALREADY on disk (write-before-pull order)")
		}
		if s.ArtifactPath() != filepath.Join(dir, "pair.sess-laptop.mcp.json") {
			t.Fatalf("ArtifactPath() = %q, want the pair.<name>.mcp.json landing spot", s.ArtifactPath())
		}
		if _, serr := os.Stat(s.ArtifactPath()); serr != nil {
			t.Fatalf("artifact must exist: %v", serr)
		}
		cred, cerr := ReadCacheCredFor("sess-laptop")
		if cerr != nil || cred == nil || cred.URL != srv.url || cred.Pin != srv.spki {
			t.Fatalf("credential must carry URL+pin: %+v err=%v", cred, cerr)
		}
	})

	t.Run("bind_discovery_path", func(t *testing.T) {
		srv := newPairingServer(t)
		dir := t.TempDir()
		withDEK(t)
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
		ctx := context.Background()

		s, err := NewPairSession(PairOpts{Instance: "bind-laptop", Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		u, perr := neturl.Parse(srv.url)
		if perr != nil {
			t.Fatal(perr)
		}
		port, _ := strconv.Atoi(u.Port())
		if err := s.Bind(Discovered{Name: "nuc10", Addr: "127.0.0.1", TCPPort: port, SPKI: srv.spki}); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if err := s.Enroll(ctx); err != nil {
			t.Fatalf("Enroll: %v", err)
		}
		if err := s.WaitApproval(ctx, nil); err != nil {
			t.Fatalf("WaitApproval: %v", err)
		}
		if err := s.Finish(ctx); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		res, err := s.WriteAndPull(ctx)
		if err != nil {
			t.Fatalf("WriteAndPull: %v", err)
		}
		if res.Instance != "bind-laptop" {
			t.Fatalf("PullResult.Instance = %q, want bind-laptop", res.Instance)
		}
		cred, cerr := ReadCacheCredFor("bind-laptop")
		if cerr != nil || cred == nil || cred.URL != srv.url {
			t.Fatalf("Bind-built transport must land the credential at the discovered broker: %+v err=%v", cred, cerr)
		}
	})
}

// TestPairSession_StateGuards pins the misuse guards T2 relies on: the step
// methods refuse to run out of order.
func TestPairSession_StateGuards(t *testing.T) {
	ctx := context.Background()
	s, err := NewPairSession(PairOpts{Instance: "guard-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(ctx); err == nil || !strings.Contains(err.Error(), "Bind") {
		t.Fatalf("Enroll before validation must refuse, got %v", err)
	}
	sv, err := NewPairSession(sessionOpts("https://127.0.0.1:7878", testValidPin(), "guard-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sv.WaitApproval(ctx, nil); err == nil || !strings.Contains(err.Error(), "Enroll") {
		t.Fatalf("WaitApproval before Enroll must refuse, got %v", err)
	}
	if err := sv.Finish(ctx); err == nil || !strings.Contains(err.Error(), "Enroll") {
		t.Fatalf("Finish before Enroll+WaitApproval must refuse, got %v", err)
	}
	if _, err := sv.WriteAndPull(ctx); err == nil || !strings.Contains(err.Error(), "Finish") {
		t.Fatalf("WriteAndPull before Finish must refuse, got %v", err)
	}
}

// TestPairSession_NoExportedKeyMaterial is the reflect nail: no exported
// PairSession field may be (or be able to carry) raw key material — []byte,
// interface, func/chan/unsafe. (All state is unexported; the pin fails loudly
// if a future refactor exports any.)
func TestPairSession_NoExportedKeyMaterial(t *testing.T) {
	typ := reflect.TypeOf(PairSession{})
	if typ.NumField() == 0 {
		t.Fatal("PairSession must keep its state fields (empty struct suspects a stub)")
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Slice:
			if f.Type.Elem().Kind() == reflect.Uint8 {
				t.Fatalf("exported field %s is a []byte — key material must never be exported", f.Name)
			}
		case reflect.Array:
			// The session's real key fields are [32]byte arrays — an exported
			// KAck [32]byte must trip the nail just like a []byte would.
			if f.Type.Elem().Kind() == reflect.Uint8 {
				t.Fatalf("exported field %s is a [%d]byte — key material must never be exported", f.Name, f.Type.Len())
			}
		case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
			t.Fatalf("exported field %s is a %s — key material must never be exported", f.Name, f.Type.Kind())
		}
	}
}

// ---------------------------------------------------------------------------
// Plan 46 T1 —— force 时序重构(零清理先行)失败注入矩阵。探针形态正式化自
// .xcheck/20260901-170939/exp/exp_probe_close46_test.go(自愈腿 + 419 对照腿),
// 其余注入腿为本任务的失败注入矩阵。
// ---------------------------------------------------------------------------

// approvePending CAS-approves the current pending row. WHY THIS EXISTS:
// newPairingServer 的自动批准 goroutine 只批准其生命周期的第一个 pending 行
// (approve 后 return)——任何「重跑第二轮」的用例,第二轮的 pending 行必须由
// 本 helper 手动批准(与 broker 审批面同一 store 入口)。
func approvePending(t *testing.T, srv *pairingServer) error {
	t.Helper()
	profs, err := srv.st.ListProfiles()
	if err != nil || len(profs) == 0 {
		t.Fatalf("profiles: %v (%d)", err, len(profs))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, lerr := srv.st.ListPendingPairing()
		if lerr == nil {
			for i := range rows {
				if rows[i].State == "pending" {
					ok, aerr := srv.st.ApprovePairing(rows[i].ID, profs[0].ID)
					if aerr != nil {
						return aerr
					}
					if ok {
						return nil
					}
				}
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

// slotSnapshot 是旧槽文件集的黄金快照(相对路径 → 字节):「旧槽一字不动」
// 的断言面,含子目录(quarantine/)内文件。
func slotSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("slot snapshot %s: %v", dir, err)
	}
	return out
}

// driveFullChain enrolls, waits for the harness auto-approval, finishes and
// (when withPull) writes + pulls — the shared round-1 shape of the matrix.
func driveFullChain(t *testing.T, srvURL, pin, instance string, withPull bool) (*PairSession, error) {
	t.Helper()
	s, err := NewPairSession(sessionOpts(srvURL, pin, instance))
	if err != nil {
		t.Fatalf("NewPairSession: %v", err)
	}
	ctx := context.Background()
	if err := s.Enroll(ctx); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := s.WaitApproval(ctx, nil); err != nil {
		t.Fatalf("WaitApproval: %v", err)
	}
	if err := s.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if withPull {
		if _, err := s.WriteAndPull(ctx); err != nil {
			t.Fatalf("WriteAndPull: %v", err)
		}
	}
	return s, nil
}

// TestPairForce_EnrollFailureKeepsOldSlotByteIdentical 是事故形态根除的回归钉:
// 任何 enroll 阶段失败(419 同名已拉取 / 网络失败),旧槽文件集逐字节不变
// (黄金断言)——「先删后 enroll 撞 419 → 半配对死槽」不可能再发生。
func TestPairForce_EnrollFailureKeepsOldSlotByteIdentical(t *testing.T) {
	t.Run("enroll_419_after_pull", func(t *testing.T) {
		srv := newPairingServer(t)
		dir := t.TempDir()
		withDEK(t)
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

		driveFullChain(t, srv.url, srv.spki, "gold-dev", true)
		before := slotSnapshot(t, dir)
		if len(before) < 4 {
			t.Fatalf("precondition: the pulled slot must hold the full file set, got %d files", len(before))
		}

		// 同名重跑 enroll → 419;文案必须给出 owner 吊销路径(不误导)。
		s2, err := NewPairSession(sessionOpts(srv.url, srv.spki, "gold-dev"))
		if err != nil {
			t.Fatal(err)
		}
		err = s2.Enroll(context.Background())
		if err == nil || !strings.Contains(err.Error(), "in use") {
			t.Fatalf("a PULLED code's name must 419 on re-enroll, got %v", err)
		}
		for _, want := range []string{"419", "cache-tokens revoke", "gold-dev"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the 419 wording must carry the owner revoke guidance (%q), got %v", want, err)
			}
		}

		after := slotSnapshot(t, dir)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("a failed enroll must leave the old slot byte-identical (golden)")
		}
	})

	t.Run("enroll_network_failure", func(t *testing.T) {
		dir := t.TempDir()
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
		// 富集旧槽:auth/bin/meta/config + quarantine/ 子树 + 产物,全在黄金面内。
		for name, body := range map[string]string{
			"cache.auth.json":          `{"url":"https://old:7878","token":"in-use-code","pin":"sha256:aa"}`,
			"cache.bin":                "OLD-CACHE-BYTES",
			"cache.meta.json":          `{"url":"https://old:7878","pulled_at":1700000000}`,
			"cache.config.json":        `{"max_offline":"72h"}`,
			"pair.net-dev.mcp.json":    `{"mcpServers":{}}`,
			"quarantine/manifest.json": `{"reason":"x"}`,
		} {
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before := slotSnapshot(t, dir)

		// 网络失败(无监听端口):enroll 的 dial 必败,盘上分毫不动。
		s, err := NewPairSession(sessionOpts("https://127.0.0.1:1", testValidPin(), "net-dev"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Enroll(context.Background()); err == nil {
			t.Fatal("enroll against a dead endpoint must fail")
		}
		if !reflect.DeepEqual(before, slotSnapshot(t, dir)) {
			t.Fatalf("a network-failed enroll must leave the old slot byte-identical (golden)")
		}
	})
}

// TestPairForce_FinishDropThenRerun_SelfHeals 正式化探针主腿(.xcheck/20260901-
// 170939):finish 成功但本地写盘从未发生(码 active 且从未 pull,模拟写盘失败)
// → 重跑 force 全链自愈:同名 enroll 必须被放行(serve 对 active-从未pull 的
// 同名码走 replaceInactive)→ finish → WriteAndPull 落盘成功。
func TestPairForce_FinishDropThenRerun_SelfHeals(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	ctx := context.Background()

	// 第一轮:finish 成功,写盘「失败」(= 不调 WriteAndPull)。
	driveFullChain(t, srv.url, srv.spki, "heal-dev", false)
	if cred, cerr := ReadCacheCredFor("heal-dev"); cerr != nil || cred != nil {
		t.Fatalf("pre-state sanity: no on-disk cred expected (we never wrote), got %+v err=%v", cred, cerr)
	}

	// 第二轮:同名重跑(模拟用户重跑 force)——enroll 必须放行,不得 419。
	// (自动批准 goroutine 只批准首个 pending 行——第二轮由 approvePending 手动批准。)
	s2, err := NewPairSession(sessionOpts(srv.url, srv.spki, "heal-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Enroll(ctx); err != nil {
		t.Fatalf("SELF-HEAL FAILED: re-enroll after finish-without-pull must be admitted (replaceInactive), got: %v", err)
	}
	if err := approvePending(t, srv); err != nil {
		t.Fatalf("round2 manual approve: %v", err)
	}
	if err := s2.WaitApproval(ctx, nil); err != nil {
		t.Fatalf("round2 WaitApproval: %v", err)
	}
	if err := s2.Finish(ctx); err != nil {
		t.Fatalf("round2 Finish: %v", err)
	}
	res, err := s2.WriteAndPull(ctx)
	if err != nil {
		t.Fatalf("round2 WriteAndPull (self-heal completion): %v", err)
	}
	if res.Instance != "heal-dev" {
		t.Fatalf("round2 pull routed to %q, want heal-dev", res.Instance)
	}
	if cred, cerr := ReadCacheCredFor("heal-dev"); cerr != nil || cred == nil {
		t.Fatalf("self-healed credential must be on disk: %v", cerr)
	}
}

// TestPairForce_PulledThenRerun_Enroll419 正式化探针对照腿:第一轮全链含首拉
// (码被 pull,last_pull_at 非空)→ 第二轮同名 enroll 必须 419,且文案给出
// owner 吊销路径。
func TestPairForce_PulledThenRerun_Enroll419(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

	driveFullChain(t, srv.url, srv.spki, "pulled-dev", true)

	s2, err := NewPairSession(sessionOpts(srv.url, srv.spki, "pulled-dev"))
	if err != nil {
		t.Fatal(err)
	}
	err = s2.Enroll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("CONTROL FAILED: re-enroll after a PULLED code must 419 with 'in use', got: %v", err)
	}
	if !strings.Contains(err.Error(), "cache-tokens revoke pulled-dev") {
		t.Fatalf("the 419 wording must name the owner-side revoke path, got %v", err)
	}
}

// TestPairForce_FirstPullCommitFail_DoublePathHint 注入「首拉 body 已接收后
// bin rename 必败」(cache.bin 预置为目录:gate 以可读空白身份放行,临时文件
// 写完后 rename 撞目录必败):错误文案必须含双路径指引,且旧材料不被半写、
// quarantine/ 不被清理(清理只在成功尾部)。
func TestPairForce_FirstPullCommitFail_DoublePathHint(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

	s, err := NewPairSession(sessionOpts(srv.url, srv.spki, "rename-dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Enroll(ctx); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := s.WaitApproval(ctx, nil); err != nil {
		t.Fatalf("WaitApproval: %v", err)
	}
	if err := s.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// 注入:bin = 目录(rename 必败点在 body 已接收之后)+ 可读空白身份 meta
	// (gate 放行)+ quarantine/(成功尾部才会被清)。
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"https://old:7878"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cache.bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, werr := s.WriteAndPull(ctx)
	if werr == nil {
		t.Fatal("the injected bin-rename failure must fail WriteAndPull")
	}
	for _, want := range []string{"first pull failed", "直接重跑 `sshmgr pair --force`", "cache-tokens revoke rename-dev"} {
		if !strings.Contains(werr.Error(), want) {
			t.Fatalf("the post-finish failure must carry the double-path hint (%q), got %v", want, werr)
		}
	}
	// 先落盘后首拉的姿势保持:凭据与产物已在盘;旧 bin(目录)未被半写;
	// quarantine/ 未被清(清理只属于成功尾部)。
	if _, serr := os.Stat(filepath.Join(dir, "pair.rename-dev.mcp.json")); serr != nil {
		t.Fatalf("artifact must already be on disk: %v", serr)
	}
	if cred, cerr := ReadCacheCredFor("rename-dev"); cerr != nil || cred == nil {
		t.Fatalf("cache.auth.json must already be on disk: cred=%+v err=%v", cred, cerr)
	}
	if fi, serr := os.Stat(filepath.Join(dir, "cache.bin")); serr != nil || !fi.IsDir() {
		t.Fatalf("the injected bin must be untouched by a torn write: fi=%v err=%v", fi, serr)
	}
	if _, serr := os.Stat(filepath.Join(dir, "quarantine", "manifest.json")); serr != nil {
		t.Fatalf("quarantine/ must survive a FAILED run (cleanup is success-tail only): %v", serr)
	}
}

// dropBodyWriter 吞掉 handler 的响应体(只放行首字节)却对 server 报告「已全
// 写」——客户端拿到 200 而信封丢失 =「已提交未收响应」形态。
type dropBodyWriter struct{ http.ResponseWriter }

func (d *dropBodyWriter) Write(b []byte) (int, error) {
	if len(b) > 0 {
		_, _ = d.ResponseWriter.Write(b[:1])
	}
	return len(b), nil
}

// TestPairForce_FinishTruncated_DoublePathHint_RerunAdmitted 注入 finish 响应
// 截断(serve 已铸码、客户端 200 到手但信封不完整):错误文案含双路径指引;
// 且 serve 端确已提交(active-从未pull)→ 重跑同名 enroll 必须被放行 = 全链
// 可自愈。
func TestPairForce_FinishTruncated_DoublePathHint_RerunAdmitted(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	ctx := context.Background()

	// 仅截断第一轮 /pair/finish 的响应(httptest 的 Config.Handler 每请求读取,
	// 运行中替换即刻生效)。
	inner := srv.srv.Config.Handler
	var truncated int32 = 1
	srv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pair/finish" && atomic.CompareAndSwapInt32(&truncated, 1, 0) {
			inner.ServeHTTP(&dropBodyWriter{ResponseWriter: w}, r)
			return
		}
		inner.ServeHTTP(w, r)
	})

	s1, err := NewPairSession(sessionOpts(srv.url, srv.spki, "trunc-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Enroll(ctx); err != nil {
		t.Fatalf("round1 Enroll: %v", err)
	}
	if err := s1.WaitApproval(ctx, nil); err != nil {
		t.Fatalf("round1 WaitApproval: %v", err)
	}
	err = s1.Finish(ctx)
	if err == nil || !strings.Contains(err.Error(), "response not JSON") {
		t.Fatalf("a truncated finish response must fail the envelope decode, got %v", err)
	}
	for _, want := range []string{"直接重跑 `sshmgr pair --force`", "cache-tokens revoke trunc-dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the truncated-finish failure must carry the double-path hint (%q), got %v", want, err)
		}
	}

	// 重跑:serve 端确已提交(码已铸、从未 pull)→ 同名 enroll 放行 → 全链走通。
	s2, err := NewPairSession(sessionOpts(srv.url, srv.spki, "trunc-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Enroll(ctx); err != nil {
		t.Fatalf("RERUN REFUSED: re-enroll after a truncated finish must be admitted, got: %v", err)
	}
	if err := approvePending(t, srv); err != nil {
		t.Fatalf("round2 manual approve: %v", err)
	}
	if err := s2.WaitApproval(ctx, nil); err != nil {
		t.Fatalf("round2 WaitApproval: %v", err)
	}
	if err := s2.Finish(ctx); err != nil {
		t.Fatalf("round2 Finish: %v", err)
	}
	if _, err := s2.WriteAndPull(ctx); err != nil {
		t.Fatalf("round2 WriteAndPull: %v", err)
	}
	if cred, cerr := ReadCacheCredFor("trunc-dev"); cerr != nil || cred == nil {
		t.Fatalf("credential must be on disk after the self-heal rerun: %v", cerr)
	}
}

// TestPairForce_ConfigWriteFailThenRerun419_DoublePathStands 注入 config 写失败
// (既有 WARNING+继续语义——拉取照跑,码被 pull):重跑同名 enroll 撞 419,
// 文案必须仍给 owner 吊销路径(不得误导为"直接换名"或"必定自愈")。
func TestPairForce_ConfigWriteFailThenRerun419_DoublePathStands(t *testing.T) {
	srv := newPairingServer(t)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	ctx := context.Background()

	FailNextConfigWriteForTest()
	s1, err := NewPairSession(sessionOpts(srv.url, srv.spki, "cfg-dev"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Enroll(ctx); err != nil {
		t.Fatalf("round1 Enroll: %v", err)
	}
	if err := s1.WaitApproval(ctx, nil); err != nil {
		t.Fatalf("round1 WaitApproval: %v", err)
	}
	if err := s1.Finish(ctx); err != nil {
		t.Fatalf("round1 Finish: %v", err)
	}
	res, err := s1.WriteAndPull(ctx)
	if err != nil {
		t.Fatalf("a config write failure is WARNING-only, the run must succeed: %v", err)
	}
	if res.Instance != "cfg-dev" {
		t.Fatalf("pull routed to %q, want cfg-dev", res.Instance)
	}

	// 重跑:码已被 pull → 419;文案必须仍含吊销路径(双路径不误导)。
	s2, err := NewPairSession(sessionOpts(srv.url, srv.spki, "cfg-dev"))
	if err != nil {
		t.Fatal(err)
	}
	err = s2.Enroll(ctx)
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("re-enroll after a pulled code must 419, got %v", err)
	}
	if !strings.Contains(err.Error(), "cache-tokens revoke cfg-dev") {
		t.Fatalf("the 419 wording must keep the owner revoke path (不误导), got %v", err)
	}
}

// TestWritePrivateFile_AtomicNoHalfFile 钉住产物原子写:成功路径整文件替换;
// 注入失败(rename 必败:目标预置为目录)不留半文件、不留临时残留——pair 产物
// 与 --write-mcp 副本共用本函数,同一保证。
func TestWritePrivateFile_AtomicNoHalfFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pair.atomic.mcp.json")
	if err := writePrivateFile(p, []byte("v1")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if b, rerr := os.ReadFile(p); rerr != nil || string(b) != "v1" {
		t.Fatalf("first write content: %s err=%v", b, rerr)
	}
	if err := writePrivateFile(p, []byte("v2-replacement-longer")); err != nil {
		t.Fatalf("replacement write must atomically replace: %v", err)
	}
	if b, rerr := os.ReadFile(p); rerr != nil || string(b) != "v2-replacement-longer" {
		t.Fatalf("replacement content: %s err=%v", b, rerr)
	}

	// 注入:目标路径为目录 → temp 写完后 rename 必败;目录不被半写文件顶替,
	// 同目录无 .tmp 残留。
	target := filepath.Join(dir, "pair.half.mcp.json")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(target, []byte("torn")); err == nil {
		t.Fatal("renaming a file onto a directory must fail")
	}
	if fi, serr := os.Stat(target); serr != nil || !fi.IsDir() {
		t.Fatalf("the failed write must not replace the target with a torn file: fi=%v err=%v", fi, serr)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pair.half.mcp.json.tmp-") {
			t.Fatalf("a failed atomic write must not leave a temp file behind: %s", e.Name())
		}
	}
}
