package mcpserver

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"ssh-manager-mcp/internal/pairing"
	"ssh-manager-mcp/internal/store"
)

// ---- test scaffolding (newSnapshotRunner posture: seeded store + live httptest) ----

// pairLimitsLoose builds a limiter set that never trips in tests that are not
// about rate limiting (each test gets a FRESH runner, so per-IP windows never
// leak across tests).
func pairLimitsLoose() pairLimits {
	return pairLimits{
		enroll: newRateLimiter(1000),
		poll:   newRateLimiter(1000),
		finish: newRateLimiter(1000),
	}
}

// newPairRunner stands up a ServeRunner with an in-test ed25519 pairing signer
// (the real one comes from the serve cert at RunServe time) and loose rate
// limits. Returns the server, the store, the runner and the signer's public key.
func newPairRunner(t *testing.T) (*httptest.Server, *store.Store, *ServeRunner, ed25519.PublicKey) {
	t.Helper()
	st := newTestStore(t)
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r.pairSigner = priv
	sum := sha256.Sum256(pub)
	r.pairSPKI = "sha256:" + hex.EncodeToString(sum[:]) // same shape SPKIFingerprint produces
	r.pairLimits = pairLimitsLoose()
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	t.Cleanup(r.Close)
	return srv, st, r, pub
}

// pairReq posts to the pair surface and returns the response.
func pairReq(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readBody(t *testing.T, res *http.Response) []byte {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wantStatus drains the body and asserts the status code.
func wantStatus(t *testing.T, res *http.Response, code int) {
	t.Helper()
	body := readBody(t, res)
	if res.StatusCode != code {
		t.Fatalf("status = %d, want %d; body=%s", res.StatusCode, code, body)
	}
}

// pairClient mimics the `sshmgr pair` client side of the protocol: it
// holds its X25519 ephemeral key and the enroll fields, and can compute the
// shared transcript/keys exactly as the frozen contract prescribes.
type pairClient struct {
	priv   *ecdh.PrivateKey
	id     [32]byte
	name   string
	url    string
	cnonce [16]byte
	hint   string
}

func newPairClient(t *testing.T, name, url string) *pairClient {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &pairClient{priv: priv, name: name, url: url}
	if _, err := rand.Read(c.id[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(c.cnonce[:]); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *pairClient) enrollBody() string {
	b := fmt.Sprintf(`{"id":%q,"name":%q,"target_url":%q,"client_pub":%q,"cnonce":%q`,
		hex.EncodeToString(c.id[:]), c.name, c.url,
		base64.RawURLEncoding.EncodeToString(c.priv.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(c.cnonce[:]))
	if c.hint != "" {
		b += fmt.Sprintf(`,"profile_hint":%q`, c.hint)
	}
	return b + "}"
}

// learnServer computes the transcript + both keys from the enroll response
// (server_pub/snonce/sig), verifying sig against the given cert public key.
func (c *pairClient) learnServer(t *testing.T, body []byte, certPub ed25519.PublicKey) (kAck, kCreds [32]byte) {
	t.Helper()
	var resp struct {
		ServerPub string `json:"server_pub"`
		Snonce    string `json:"snonce"`
		Sig       string `json:"sig"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("enroll response not JSON: %v\nbody=%s", err, body)
	}
	serverPub, err := base64.RawURLEncoding.DecodeString(resp.ServerPub)
	if err != nil || len(serverPub) != 32 {
		t.Fatalf("server_pub bad: %v len=%d", err, len(serverPub))
	}
	snonce, err := base64.RawURLEncoding.DecodeString(resp.Snonce)
	if err != nil || len(snonce) != 16 {
		t.Fatalf("snonce bad: %v len=%d", err, len(snonce))
	}
	sig, err := base64.RawURLEncoding.DecodeString(resp.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		t.Fatalf("sig bad: %v len=%d", err, len(sig))
	}
	tr := buildPairTranscript(c.id[:], []byte(c.name), []byte(c.url),
		c.priv.PublicKey().Bytes(), c.cnonce[:], serverPub, snonce)
	if !ed25519.Verify(certPub, tr, sig) {
		t.Fatal("sig does not verify over the transcript with the serve cert public key")
	}
	remote, err := ecdh.X25519().NewPublicKey(serverPub)
	if err != nil {
		t.Fatal(err)
	}
	ikm, err := c.priv.ECDH(remote)
	if err != nil {
		t.Fatal(err)
	}
	return pairing.DeriveKeys(ikm, tr)
}

func (c *pairClient) ackFor(kAck [32]byte) string {
	return hex.EncodeToString(pairing.FinishAck(kAck, c.id[:]))
}

func decodeFinish(t *testing.T, body []byte) []byte {
	t.Helper()
	var resp struct {
		Sealed string `json:"sealed"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("finish response not JSON: %v\nbody=%s", err, body)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(resp.Sealed)
	if err != nil {
		t.Fatalf("sealed not b64u: %v", err)
	}
	return sealed
}

// findRow fetches the pending queue row for id (nil when absent).
func findRow(t *testing.T, st *store.Store, id []byte) *store.PendingPairing {
	t.Helper()
	rows, err := st.ListPendingPairing()
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if bytes.Equal(rows[i].ID, id) {
			return &rows[i]
		}
	}
	return nil
}

// enrollOK runs one enroll expected to succeed and returns the derived keys.
func enrollOK(t *testing.T, srv *httptest.Server, st *store.Store, certPub ed25519.PublicKey, c *pairClient) (kAck, kCreds [32]byte) {
	t.Helper()
	res := pairReq(t, srv, http.MethodPost, "/pair/enroll", c.enrollBody())
	body := readBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("enroll status = %d, want 200; body=%s", res.StatusCode, body)
	}
	kAck, kCreds = c.learnServer(t, body, certPub)
	if row := findRow(t, st, c.id[:]); row == nil {
		t.Fatal("enroll must land a pending row in pairing_pending")
	}
	return kAck, kCreds
}

// approveOK flips the row via the owner surface (the same call the TUI/Web/CLI
// approver makes — the HTTP /pair plane never approves).
func approveOK(t *testing.T, st *store.Store, c *pairClient, profile string) {
	t.Helper()
	ok, err := st.ApprovePairing(c.id[:], profile)
	if err != nil || !ok {
		t.Fatalf("ApprovePairing: ok=%v err=%v", ok, err)
	}
}

// ---- tests (brief-frozen list) ----

func TestPairEnroll_HappyAndValidation(t *testing.T) {
	srv, st, r, certPub := newPairRunner(t)

	c := newPairClient(t, "devA", "https://192.0.2.10:7878")
	c.hint = "工作站" // \p{L} payload is legal
	kAck, _ := enrollOK(t, srv, st, certPub, c)
	_ = kAck

	row := findRow(t, st, c.id[:])
	if row.State != "pending" || row.Name != "devA" || row.TargetURL != "https://192.0.2.10:7878" {
		t.Fatalf("row mismatch: %+v", row)
	}
	if !bytes.Equal(row.ClientPub, c.priv.PublicKey().Bytes()) || !bytes.Equal(row.Cnonce, c.cnonce[:]) {
		t.Fatal("row must carry the client's public values verbatim")
	}
	if row.ReplaceInactive {
		t.Fatal("no collision: replace_inactive must be false")
	}
	r.pairMu.Lock()
	_, keyed := r.pairKeys[c.id]
	r.pairMu.Unlock()
	if !keyed {
		t.Fatal("enroll must stash the X25519 private key in the in-memory map")
	}

	// Invalid public key (not base64url) → 400.
	bad := newPairClient(t, "devB", "https://192.0.2.10:7878")
	badBody := fmt.Sprintf(`{"id":%q,"name":"devB","target_url":"https://x:1","client_pub":"!!!not-b64","cnonce":%q}`,
		hex.EncodeToString(bad.id[:]), base64.RawURLEncoding.EncodeToString(bad.cnonce[:]))
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/enroll", badBody), http.StatusBadRequest)
	// Invalid nonce (right b64 shape, wrong length) → 400.
	bad2 := newPairClient(t, "devB", "https://192.0.2.10:7878")
	bad2Body := fmt.Sprintf(`{"id":%q,"name":"devB","target_url":"https://x:1","client_pub":%q,"cnonce":"AAAAAAAAAAA"}`,
		hex.EncodeToString(bad2.id[:]), base64.RawURLEncoding.EncodeToString(bad2.priv.PublicKey().Bytes()))
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/enroll", bad2Body), http.StatusBadRequest)
	// hint 首字符非法(空格开头)→ 400。
	bad3 := newPairClient(t, "devB", "https://192.0.2.10:7878")
	bad3.hint = " leading space"
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/enroll", bad3.enrollBody()), http.StatusBadRequest)
	// body > 1KiB → 413。
	big := newPairClient(t, "devB", "https://192.0.2.10:7878")
	bigBody := fmt.Sprintf(`{"id":%q,"name":"devB","target_url":"https://x:1","client_pub":%q,"cnonce":%q,"pad":%q}`,
		hex.EncodeToString(big.id[:]),
		base64.RawURLEncoding.EncodeToString(big.priv.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(big.cnonce[:]), make([]byte, 2048))
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/enroll", bigBody), http.StatusRequestEntityTooLarge)
	// 重复 id → 409。
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/enroll", c.enrollBody()), http.StatusConflict)
	// GET → 405。
	wantStatus(t, pairReq(t, srv, http.MethodGet, "/pair/enroll", ""), http.StatusMethodNotAllowed)
}

func TestPairEnroll_RateLimit429(t *testing.T) {
	srv, _, r, _ := newPairRunner(t)
	r.pairLimits.enroll = newRateLimiter(5) // the frozen default, explicit
	body := newPairClient(t, "rl", "https://192.0.2.10:7878").enrollBody()
	for i := 1; i <= 5; i++ {
		res := pairReq(t, srv, http.MethodPost, "/pair/enroll", body)
		readBody(t, res)
		if res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d must not be limited yet", i)
		}
	}
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/enroll", body), http.StatusTooManyRequests)
}

func TestPairEnroll_UnauthenticatedZeroSideEffects(t *testing.T) {
	srv, st, _, certPub := newPairRunner(t)
	profID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}

	// 撞名 last_pull=0(未激活码):enroll 放行并标记 replace_inactive,但绝不吊销。
	if _, _, err := st.AddCacheToken("laptop", profID); err != nil {
		t.Fatal(err)
	}
	c := newPairClient(t, "laptop", "https://192.0.2.10:7878")
	enrollOK(t, srv, st, certPub, c)
	row := findRow(t, st, c.id[:])
	if row == nil || !row.ReplaceInactive {
		t.Fatalf("collision with never-pulled code must record replace_inactive, got %+v", row)
	}
	toks, err := st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, tk := range toks {
		if tk.Name == "laptop" {
			n++
			if tk.Status != "active" {
				t.Fatalf("enroll must NOT revoke the old code (zero side effects), status=%s", tk.Status)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 laptop token, got %d", n)
	}

	// 撞名 last_pull>0(在用码):enroll 即拒 419。
	busyID, _, err := st.AddCacheToken("busy", profID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TouchCacheToken(busyID); err != nil { // sets last_pull_at
		t.Fatal(err)
	}
	b := newPairClient(t, "busy", "https://192.0.2.10:7878")
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/enroll", b.enrollBody()), 419)
	if findRow(t, st, b.id[:]) != nil {
		t.Fatal("a refused enroll must not land a row")
	}
}

func TestPairPoll_PostOnlyAndStates(t *testing.T) {
	srv, st, _, certPub := newPairRunner(t)
	profID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}

	// GET → 405。
	wantStatus(t, pairReq(t, srv, http.MethodGet, "/pair/poll", ""), http.StatusMethodNotAllowed)
	// 未知 id → 410。
	ghost := newPairClient(t, "ghost", "https://192.0.2.10:7878")
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/poll",
		fmt.Sprintf(`{"id":%q}`, hex.EncodeToString(ghost.id[:]))), http.StatusGone)

	c := newPairClient(t, "devP", "https://192.0.2.10:7878")
	enrollOK(t, srv, st, certPub, c)
	body := fmt.Sprintf(`{"id":%q}`, hex.EncodeToString(c.id[:]))

	// 未批 → 202 {"t":"pending"}。
	res := pairReq(t, srv, http.MethodPost, "/pair/poll", body)
	pb := readBody(t, res)
	if res.StatusCode != http.StatusAccepted || !bytes.Contains(pb, []byte("pending")) {
		t.Fatalf("pending poll: status = %d body=%s, want 202 pending", res.StatusCode, pb)
	}
	// 批后 → 200 {"t":"approved"}。
	approveOK(t, st, c, profID)
	res = pairReq(t, srv, http.MethodPost, "/pair/poll", body)
	pb = readBody(t, res)
	if res.StatusCode != http.StatusOK || !bytes.Contains(pb, []byte("approved")) {
		t.Fatalf("approved poll: status = %d body=%s, want 200 approved", res.StatusCode, pb)
	}
	// enroll 窗口过 → 410(store 时钟注入 +11 分钟)。
	st.NowFn = func() time.Time { return time.Now().Add(11 * time.Minute) }
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/poll", body), http.StatusGone)
}

func TestPairFinish_AckAndWindow(t *testing.T) {
	profName := "team-a"

	t.Run("ack wrong → 403", func(t *testing.T) {
		srv, st, _, certPub := newPairRunner(t)
		profID, err := st.AddProfile(profName)
		if err != nil {
			t.Fatal(err)
		}
		c := newPairClient(t, "devF", "https://192.0.2.10:7878")
		enrollOK(t, srv, st, certPub, c)
		approveOK(t, st, c, profID)
		body := fmt.Sprintf(`{"id":%q,"ack":%q}`, hex.EncodeToString(c.id[:]), hex.EncodeToString(make([]byte, 32)))
		wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/finish", body), http.StatusForbidden)
	})

	t.Run("not approved → 409", func(t *testing.T) {
		srv, st, _, certPub := newPairRunner(t)
		c := newPairClient(t, "devF", "https://192.0.2.10:7878")
		kAck, _ := enrollOK(t, srv, st, certPub, c)
		body := fmt.Sprintf(`{"id":%q,"ack":%q}`, hex.EncodeToString(c.id[:]), c.ackFor(kAck))
		wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/finish", body), http.StatusConflict)
	})

	t.Run("approval window over → 410", func(t *testing.T) {
		srv, st, _, certPub := newPairRunner(t)
		profID, err := st.AddProfile(profName)
		if err != nil {
			t.Fatal(err)
		}
		c := newPairClient(t, "devF", "https://192.0.2.10:7878")
		enrollOK(t, srv, st, certPub, c)
		approveOK(t, st, c, profID)
		st.NowFn = func() time.Time { return time.Now().Add(3 * time.Minute) } // 120s 窗外
		body := fmt.Sprintf(`{"id":%q,"ack":%q}`, hex.EncodeToString(c.id[:]), hex.EncodeToString(make([]byte, 32)))
		wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/finish", body), http.StatusGone)
	})

	t.Run("success → sealed four-piece envelope", func(t *testing.T) {
		srv, st, r, certPub := newPairRunner(t)
		profID, err := st.AddProfile(profName)
		if err != nil {
			t.Fatal(err)
		}
		c := newPairClient(t, "devG", "https://192.0.2.10:7878")
		kAck, kCreds := enrollOK(t, srv, st, certPub, c)
		approveOK(t, st, c, profID)
		body := fmt.Sprintf(`{"id":%q,"ack":%q}`, hex.EncodeToString(c.id[:]), c.ackFor(kAck))
		res := pairReq(t, srv, http.MethodPost, "/pair/finish", body)
		fb := readBody(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("finish: status = %d body=%s, want 200", res.StatusCode, fb)
		}
		sealed := decodeFinish(t, fb)
		pt, err := pairing.OpenCreds(kCreds, sealed)
		if err != nil {
			t.Fatalf("OpenCreds with the derived K_creds failed: %v", err)
		}
		var env struct {
			SPKI         string `json:"spki"`
			Profile      string `json:"profile"`
			DeviceCode   string `json:"device_code"`
			ProjectToken string `json:"project_token"`
			MaxOffline   string `json:"max_offline"`
		}
		if err := json.Unmarshal(pt, &env); err != nil {
			t.Fatalf("envelope not JSON: %v\npt=%s", err, pt)
		}
		if env.SPKI != r.pairSPKI {
			t.Fatalf("envelope spki = %q, want the serve cert pin %q", env.SPKI, r.pairSPKI)
		}
		if env.Profile != profName {
			t.Fatalf("envelope profile = %q, want %q", env.Profile, profName)
		}
		if env.DeviceCode == "" || env.ProjectToken == "" {
			t.Fatalf("envelope must carry both one-time credentials: %+v", env)
		}
		if env.MaxOffline != "24h" {
			t.Fatalf("envelope max_offline = %q, want default 24h", env.MaxOffline)
		}
		// Row reached terminal delivered; the in-memory key is gone.
		if row := findRow(t, st, c.id[:]); row != nil {
			t.Fatalf("delivered row must leave the live queue, got %+v", row)
		}
		r.pairMu.Lock()
		_, keyed := r.pairKeys[c.id]
		r.pairMu.Unlock()
		if keyed {
			t.Fatal("terminal row must drop the in-memory X25519 key")
		}
		// The mint really ran: the device name is now an ACTIVE code.
		if _, active, err := st.ActiveCacheTokenInfo("devG"); err != nil || !active {
			t.Fatalf("mint must leave the device name active: active=%v err=%v", active, err)
		}
	})
}

func TestPairFinish_ReplayReturnsCachedSealed(t *testing.T) {
	srv, st, _, certPub := newPairRunner(t)
	profID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	c := newPairClient(t, "devR", "https://192.0.2.10:7878")
	kAck, _ := enrollOK(t, srv, st, certPub, c)
	approveOK(t, st, c, profID)
	body := fmt.Sprintf(`{"id":%q,"ack":%q}`, hex.EncodeToString(c.id[:]), c.ackFor(kAck))
	res := pairReq(t, srv, http.MethodPost, "/pair/finish", body)
	first := readBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first finish: status = %d body=%s", res.StatusCode, first)
	}
	res = pairReq(t, srv, http.MethodPost, "/pair/finish", body)
	second := readBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("replay finish: status = %d body=%s, want 200 with cached sealed", res.StatusCode, second)
	}
	if !bytes.Equal(decodeFinish(t, first), decodeFinish(t, second)) {
		t.Fatal("replay must return the IDENTICAL cached sealed bytes")
	}
}

func TestForeignTarget(t *testing.T) {
	// 本机环回不在集合内(诚实验证 LocalNonLoopbackIPs 不含环回)。
	if !ForeignTarget("https://127.0.0.1:7878") {
		t.Fatal("127.0.0.1 must be FOREIGN (loopback is not in LocalNonLoopbackIPs)")
	}
	if !ForeignTarget("http://[::1]:7878") {
		t.Fatal("::1 must be FOREIGN")
	}
	// 第一个本机非环回 IP → 本机。
	ips := LocalNonLoopbackIPs()
	if len(ips) > 0 {
		if ForeignTarget(fmt.Sprintf("https://%s:7878", ips[0])) {
			t.Fatalf("local IP %s must NOT be foreign", ips[0])
		}
	} else {
		t.Log("no non-loopback IPs on this host; local-IP case vacuously skipped")
	}
	// hostname 形态(带 port 与不带 port 两形态)→ 本机。
	if host, err := os.Hostname(); err == nil && host != "" {
		if ForeignTarget(fmt.Sprintf("https://%s:7878", host)) {
			t.Fatalf("hostname %s must NOT be foreign", host)
		}
		if ForeignTarget(fmt.Sprintf("https://%s", host)) {
			t.Fatalf("hostname without port %s must NOT be foreign", host)
		}
	}
	// 垃圾串/外部地址 → true(fail-closed)。
	for _, s := range []string{"", "not a url", "https://[bad", "https://203.0.113.9:7878"} {
		if !ForeignTarget(s) {
			t.Fatalf("%q must be foreign", s)
		}
	}
}

func TestPairDisabled_404(t *testing.T) {
	srv, st, r, _ := newPairRunner(t)
	if err := st.SetSetting("serve.pairing", "false"); err != nil {
		t.Fatal(err)
	}
	r.RefreshSwitches(nil, nil, nil, nil) // rebuild the ≤5s memo immediately
	for _, path := range []string{"/pair/enroll", "/pair/poll", "/pair/finish"} {
		wantStatus(t, pairReq(t, srv, http.MethodPost, path, `{"id":"x"}`), http.StatusNotFound)
	}
	// 未知 /pair/ 路径 → 404(开关开着也一样)。
	r.RefreshSwitches(nil, nil, nil, nil)
	_ = st.SetSetting("serve.pairing", "true")
	r.RefreshSwitches(nil, nil, nil, nil)
	wantStatus(t, pairReq(t, srv, http.MethodPost, "/pair/nope", `{}`), http.StatusNotFound)
}

// ---- ratelimit.go unit legs ----

func TestRateLimiter_FixedWindow(t *testing.T) {
	l := newRateLimiter(2)
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("first two hits must pass")
	}
	if l.Allow("a") {
		t.Fatal("third hit in the same window must fail")
	}
	if !l.Allow("b") {
		t.Fatal("per-IP independence: a full window for a must not touch b")
	}
	// 窗口翻页:伪造一个 61s 前开窗的计数 → 放行。
	l.mu.Lock()
	l.windows["a"] = rateWindow{start: time.Now().Unix() - 61, count: 99}
	l.mu.Unlock()
	if !l.Allow("a") {
		t.Fatal("an aged-out window must reset")
	}
}

func TestPairLimitsFromEnv_Clamps(t *testing.T) {
	t.Setenv("SSHMGR_SERVE_PAIR_ENROLL_PER_MIN", "0")
	t.Setenv("SSHMGR_SERVE_PAIR_POLL_PER_MIN", "9999")
	t.Setenv("SSHMGR_SERVE_PAIR_FINISH_PER_MIN", "junk")
	t.Setenv("SSHMGR_SERVE_PAIR_PENDING_MAX_IP", "-3")
	t.Setenv("SSHMGR_SERVE_PAIR_PENDING_MAX_GLOBAL", "128")
	lim, perIP, global := pairLimitsFromEnv()
	if lim.enroll.perMin != 1 || lim.poll.perMin != 120 || lim.finish.perMin != 5 {
		t.Fatalf("per-min clamps wrong: %+v (want enroll=1 poll=120 finish=5)", lim)
	}
	if perIP != 1 || global != 128 {
		t.Fatalf("pending clamps wrong: perIP=%d global=%d (want 1/128)", perIP, global)
	}
}
