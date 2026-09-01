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
		case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
			t.Fatalf("exported field %s is a %s — key material must never be exported", f.Name, f.Type.Kind())
		}
	}
}
