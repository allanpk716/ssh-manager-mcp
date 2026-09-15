package cli

// Plan 48 §8 integration — the cache broker (mcpserver.NewCacheBroker: the
// exact RunStdioCache assembly) against a REAL testsshd target and an
// in-process serve over pinned TLS carrying the live POST /pin-hostkey
// endpoint, driven through the MCP tool surface with in-memory transports.
//
// Environment boundary (spec §8): the test process always HAS a route to the
// target, so "broker cannot reach the target" is covered by logical
// equivalence — the serve-side anchor is only produced by the forward under
// test, never pre-seeded, and every assertion reads the serve-side truth.
//
// Surfaced-text note: the §2.1/§4 branch texts are byte-contracts at the
// forwarder/wrapper layers (clientops tests); through the MCP exec_command
// surface the ssh stack wraps handshake failures ("ssh: handshake failed: ")
// and Plan 31 redaction prefixes "ssh dial: " and replaces the target's
// host:port with [REDACTED] (the assertNoLeak invariant of core_test). The
// e2e assertions below pin exactly that composition.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// cacheFwdFixture: one testsshd target + one authoritative serve (TLS) + the
// client snapshot materials. The serve store starts with NO anchor — every
// anchor in T5's assertions is produced by the forward under test.
type cacheFwdFixture struct {
	sshdAddr  string
	sshdKey   ssh.PublicKey
	targetID  string
	profID    string
	serve     *store.Store
	dbPath    string
	serveHTTP *httptest.Server // TLS, /pin-hostkey POST-counted
	posts     *int32
	cred      clientops.CacheCred // pull/pin credential for the "laptop" device
	projToken string
	snap      *store.Snapshot // exported BEFORE any test-side anchor preset
}

// newCacheFwdFixture seeds the whole topology. The POST counter wraps only
// /pin-hostkey, so "zero second forward" is readable as a stable count.
func newCacheFwdFixture(t *testing.T) *cacheFwdFixture {
	t.Helper()
	addr, sshdKey, sshdCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "PONG:" + cmd + "\n", "", 0 },
	})
	t.Cleanup(sshdCleanup)
	host := addr[:strings.Index(addr, ":")]
	port := portFromAddr(addr)

	dbPath := filepath.Join(t.TempDir(), "serve.db")
	mk, _ := store.GenerateMasterKey()
	sv, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sv.Close() })

	cid, err := sv.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := sv.AddServer(&models.Server{Name: "target", Host: host, Port: port, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	profID, err := sv.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := sv.GrantServers(profID, []string{targetID}); err != nil {
		t.Fatal(err)
	}
	_, projToken, err := sv.AddProject("agent", profID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := mcpserver.NewServeRunner(sv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	var posts int32
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/pin-hostkey" {
			atomic.AddInt32(&posts, 1)
		}
		r.HTTPHandler().ServeHTTP(w, req)
	})
	serveHTTP := httptest.NewTLSServer(mux)
	t.Cleanup(serveHTTP.Close)
	_, laptopTok, err := sv.AddCacheToken("laptop", profID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := sv.ExportSnapshotForProfile(profID)
	if err != nil {
		t.Fatal(err)
	}
	return &cacheFwdFixture{
		sshdAddr: addr, sshdKey: sshdKey, targetID: targetID, profID: profID,
		serve: sv, dbPath: dbPath, serveHTTP: serveHTTP, posts: &posts,
		cred: clientops.CacheCred{
			URL:   serveHTTP.URL,
			Token: laptopTok,
			Pin:   mcpserver.SPKIFingerprint(serveHTTP.Certificate()),
		},
		projToken: projToken, snap: snap,
	}
}

func (fx *cacheFwdFixture) host() string { return fx.sshdAddr[:strings.Index(fx.sshdAddr, ":")] }
func (fx *cacheFwdFixture) port() int    { return portFromAddr(fx.sshdAddr) }
func (fx *cacheFwdFixture) postCount() int {
	return int(atomic.LoadInt32(fx.posts))
}
func (fx *cacheFwdFixture) fingerprint() string { return ssh.FingerprintSHA256(fx.sshdKey) }

// newBrokerSnap assembles the cache broker exactly as mcp --cache does (the
// binder from the fixture credential, or a per-test override credential) and
// attaches an MCP client.
func (fx *cacheFwdFixture) newBrokerSnap(t *testing.T, snap *store.Snapshot, reload func() (*store.Snapshot, bool, error), cred clientops.CacheCred) *mcp.ClientSession {
	t.Helper()
	srv, tunnels, tasks, cleanup, err := mcpserver.NewCacheBroker(
		fx.projToken, snap, filepath.Join(t.TempDir(), "audit.log"), reload,
		clientops.ForwardingHostKeys(mustForwarder(t, cred)), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	return attachMCPClient(t, srv)
}

func (fx *cacheFwdFixture) newBroker(t *testing.T, reload func() (*store.Snapshot, bool, error), cred clientops.CacheCred) *mcp.ClientSession {
	return fx.newBrokerSnap(t, fx.snap, reload, cred)
}

func mustForwarder(t *testing.T, cred clientops.CacheCred) *clientops.PinForwarder {
	t.Helper()
	f, err := clientops.NewPinForwarder(cred)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// attachMCPClient drives the assembled server over in-memory transports.
func attachMCPClient(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := srv.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srvSess.Close() })
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cliSess.Close() })
	return cliSess
}

// callTool invokes a tool and returns (text, isError).
func callTool(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, res.IsError
}

func callExec(t *testing.T, sess *mcp.ClientSession, serverID, command string) (string, bool) {
	return callTool(t, sess, "exec_command", map[string]any{"server_id": serverID, "command": command})
}

// dialRefusedMainMessage renders the exec-surfaced §4 main message for the
// fixture target (handshake wrap + Plan 31 prefix and [REDACTED] host:port).
// The template's parenthesis closes after "(presented fingerprint: <fp>)" —
// the trailing "--fingerprint <fp>" has no closing paren (the exact shape the
// clientops verbatim test locks).
func (fx *cacheFwdFixture) dialRefusedMainMessage() string {
	fp := fx.fingerprint()
	return "ssh dial: ssh: handshake failed: host key for [REDACTED] is unknown and cannot be pinned here (presented fingerprint: " + fp + "). Retry while the broker is reachable — the pin is then forwarded and audited automatically — or ask the owner to run: sshmgr servers pin-hostkey <name> --fingerprint " + fp
}

// rawSQLiteStmt runs one parameterized statement on a second raw connection
// to the serve db (the owner-side anchor preset seam — pin-hostkey
// --fingerprint/--clear write the same table from the cli, later in this
// plan). Parameterized because key_blob bytes are binary.
func rawSQLiteStmt(t *testing.T, path, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("raw %q: %v", stmt, err)
	}
}

// presetServeAnchor inserts an anchor directly into the serve's host_keys —
// the owner pre-anchoring shape (blob or fingerprint format).
func (fx *cacheFwdFixture) presetServeAnchor(t *testing.T, blob []byte, format, source string) {
	t.Helper()
	rawSQLiteStmt(t, fx.dbPath,
		`INSERT INTO host_keys (host_port, key_blob, pin_format, pin_source, pin_device, created_at) VALUES (?,?,?,?,?,?)`,
		fmt.Sprintf("%s:%d", fx.host(), fx.port()), blob, format, source, "", time.Now().Unix())
}

// assertServeAnchor checks the authoritative landing: anchor present, dual-mode
// matching the presented key, source=forward, device=laptop (the §6 detection
// surface reads the same columns).
func (fx *cacheFwdFixture) assertServeAnchor(t *testing.T) {
	t.Helper()
	pin, err := fx.serve.GetHostKey(fx.host(), fx.port())
	if err != nil || pin == nil {
		t.Fatalf("serve-side anchor missing: pin=%v err=%v", pin, err)
	}
	if !pin.Matches(fx.sshdKey.Marshal()) {
		t.Fatal("serve-side anchor does not match the presented testsshd key")
	}
	db, err := sql.Open("sqlite", fx.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var source, device string
	if err := db.QueryRow(`SELECT pin_source, pin_device FROM host_keys WHERE host_port=?`,
		fmt.Sprintf("%s:%d", fx.host(), fx.port())).Scan(&source, &device); err != nil {
		t.Fatal(err)
	}
	if source != "forward" || device != "laptop" {
		t.Fatalf("serve anchor metadata = %q/%q, want forward/laptop", source, device)
	}
}

// assertServeAuditPinForward: the authoritative audit row carries the device
// name and the affects list (§1.2 ⑤ / §6).
func (fx *cacheFwdFixture) assertServeAuditPinForward(t *testing.T) {
	t.Helper()
	rows, err := fx.serve.AuditRows(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Action == "pin-forward" {
			if !strings.Contains(r.Command, "device=laptop") || !strings.Contains(r.Command, "affects=1") {
				t.Fatalf("pin-forward audit row lacks device/affects details: %q", r.Command)
			}
			return
		}
	}
	t.Fatal("no pin-forward audit row on the serve side")
}

// assertSidecarHasNoForwardRows: the local sidecar records the exec rows only
// — the forward's history lives in the broker's authoritative row (§6).
func assertSidecarHasNoForwardRows(t *testing.T, auditPath string) {
	t.Helper()
	blob, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(blob), "\n") {
		if strings.Contains(line, "pin-forward") {
			t.Fatalf("the local sidecar must not record forwarding rows: %q", line)
		}
	}
}

// TestCacheForwardE2E_FirstConnectOnceSuccess is T5: mcp --cache + testsshd +
// in-process serve, entry + credential in the snapshot, NO anchor anywhere.
// exec_command succeeds ON THE FIRST TRY; the anchor + audit row land on the
// serve; the same process's next connection reuses the applied in-memory
// anchor — zero second forwarding; the local sidecar stays exec-only.
func TestCacheForwardE2E_FirstConnectOnceSuccess(t *testing.T) {
	fx := newCacheFwdFixture(t)
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	srv, tunnels, tasks, cleanup, err := mcpserver.NewCacheBroker(
		fx.projToken, fx.snap, auditPath, nil,
		clientops.ForwardingHostKeys(mustForwarder(t, fx.cred)), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	sess := attachMCPClient(t, srv)

	text, isErr := callExec(t, sess, fx.targetID, "hi")
	if isErr {
		t.Fatalf("first exec must succeed via forwarding: %s", text)
	}
	if !strings.Contains(text, "PONG:hi") {
		t.Fatalf("exec output missing: %q", text)
	}
	if fx.postCount() != 1 {
		t.Fatalf("posts = %d, want exactly 1", fx.postCount())
	}
	fx.assertServeAnchor(t)
	fx.assertServeAuditPinForward(t)

	// Same process, new connection (exec opens a fresh SSH connection per
	// call): the applied anchor matches — no second forward.
	if _, isErr = callExec(t, sess, fx.targetID, "again"); isErr {
		t.Fatal("second exec must succeed from the applied in-memory anchor")
	}
	if fx.postCount() != 1 {
		t.Fatalf("the second connection re-forwarded: posts = %d", fx.postCount())
	}
	assertSidecarHasNoForwardRows(t, auditPath)
}

// TestCacheForwardE2E_GenerationSwapKeepsThePin is T5b (black-box): a hot
// rebuild fires between the broker's 201 and the local apply (the reload is
// consulted exactly there by the real-time resolution rule). The anchor must
// land in the SWAPPED-IN generation — observable because the reloaded
// generation contains a witness server (proving the swap) and the reconnect
// still does not re-forward (proving the pin is in the CURRENT generation, not
// the swapped-out one).
func TestCacheForwardE2E_GenerationSwapKeepsThePin(t *testing.T) {
	fx := newCacheFwdFixture(t)

	// Witness: a second granted server, added to the serve AFTER the client
	// snapshot was exported — a genuine same-profile change for the reloader.
	witnessID, err := fx.serve.AddServer(&models.Server{Name: "witness", Host: "10.9.9.9", Port: 2222, User: "u", AuthMethod: models.AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.GrantServers(fx.profID, []string{witnessID}); err != nil {
		t.Fatal(err)
	}
	snap2, err := fx.serve.ExportSnapshotForProfile(fx.profID)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	reload := func() (*store.Snapshot, bool, error) {
		calls++
		if calls == 2 { // the apply path's consult, AFTER the broker's 201
			return snap2, true, nil
		}
		return nil, false, nil
	}

	sess := fx.newBroker(t, reload, fx.cred)
	if _, isErr := callExec(t, sess, fx.targetID, "hi"); isErr {
		t.Fatal("first exec must succeed via forwarding")
	}
	if fx.postCount() != 1 {
		t.Fatalf("posts = %d, want 1", fx.postCount())
	}
	// The swap took effect: the witness (only in the reloaded generation) is
	// visible, so every later read serves the CURRENT generation.
	listText, isErr := callTool(t, sess, "list_servers", map[string]any{})
	if isErr || !strings.Contains(listText, "witness") {
		t.Fatalf("the hot rebuild did not swap in (witness missing): %q", listText)
	}
	// Reconnect: the anchor must live in the CURRENT (swapped-in) generation —
	// zero re-forward. If the apply had stranded the anchor in generation 0,
	// this exec would forward again (posts = 2).
	if _, isErr := callExec(t, sess, fx.targetID, "again"); isErr {
		t.Fatal("reconnect after the swap must succeed from the applied anchor")
	}
	if fx.postCount() != 1 {
		t.Fatalf("the anchor did not land in the current generation — re-forwarded: posts = %d", fx.postCount())
	}
}

// TestCacheForwardE2E_ServeDownMainMessage is T6: the serve is GONE (closed
// listener) — the §4 main message surfaces verbatim modulo the ssh handshake
// wrap and Plan 31 redaction, carrying the REAL presented fingerprint.
func TestCacheForwardE2E_ServeDownMainMessage(t *testing.T) {
	fx := newCacheFwdFixture(t)
	dead := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL, deadPin := dead.URL, mcpserver.SPKIFingerprint(dead.Certificate())
	dead.Close() // connection refused from here on

	cred := fx.cred
	cred.URL, cred.Pin = deadURL, deadPin
	sess := fx.newBroker(t, nil, cred)

	text, isErr := callExec(t, sess, fx.targetID, "hi")
	if !isErr {
		t.Fatal("exec with the serve down must fail")
	}
	if text != fx.dialRefusedMainMessage() {
		t.Fatalf("surfaced text mismatch:\n got: %q\nwant: %q", text, fx.dialRefusedMainMessage())
	}
	if strings.Contains(text, fx.host()) {
		t.Fatalf("surfaced text leaks the target host: %q", text)
	}
}

// TestCacheForwardE2E_OldBroker404 is T7: a broker without the /pin-hostkey
// route (old version simulated by an empty mux) → the 404 text, exact modulo
// the wrap+redaction prefix (the 404 text names no address).
func TestCacheForwardE2E_OldBroker404(t *testing.T) {
	fx := newCacheFwdFixture(t)
	old := httptest.NewTLSServer(http.NewServeMux())
	defer old.Close()

	cred := fx.cred
	cred.URL = old.URL
	cred.Pin = mcpserver.SPKIFingerprint(old.Certificate())
	sess := fx.newBroker(t, nil, cred)

	text, isErr := callExec(t, sess, fx.targetID, "hi")
	if !isErr {
		t.Fatal("exec against an old broker must fail")
	}
	want := "ssh dial: ssh: handshake failed: the broker does not support pin forwarding (the owner must upgrade the broker to >= v0.15.0, then retry)"
	if text != want {
		t.Fatalf("404 text mismatch:\n got: %q\nwant: %q", text, want)
	}
}

// TestCacheForwardE2E_ConflictBranches is T8's two branches:
//  1. the owner pre-anchored the TRUE key as a FINGERPRINT anchor
//     (servers pin-hostkey --fingerprint shape) while the client snapshot is
//     stale (no anchor) → the forward hits 409 equal=true → automatic pass,
//     zero user action;
//  2. the broker holds a DIFFERENT anchor → 409 equal=false hard error with
//     the exact text → a real cache pull rehydrates the authoritative anchor →
//     the reconnect fails with the ErrHostKeyMismatch double-fingerprint text.
func TestCacheForwardE2E_ConflictBranches(t *testing.T) {
	t.Run("equal fingerprint anchor auto-passes", func(t *testing.T) {
		fx := newCacheFwdFixture(t)
		// Owner pre-anchor AFTER the snapshot export (stale client): the true
		// key, fingerprint-anchor form (pin_format='fingerprint').
		fx.presetServeAnchor(t, []byte(fx.fingerprint()), "fingerprint", "manual")

		sess := fx.newBroker(t, nil, fx.cred)
		if text, isErr := callExec(t, sess, fx.targetID, "hi"); isErr {
			t.Fatalf("409 equal=true must auto-pass with zero user action: %s", text)
		}
		if fx.postCount() != 1 {
			t.Fatalf("posts = %d, want 1", fx.postCount())
		}
		// The auto-closure applies locally: the reconnect does not re-forward.
		if _, isErr := callExec(t, sess, fx.targetID, "again"); isErr {
			t.Fatal("reconnect must succeed from the applied anchor")
		}
		if fx.postCount() != 1 {
			t.Fatalf("reconnect re-forwarded: posts = %d", fx.postCount())
		}
	})

	t.Run("different anchor hard error then pull then mismatch", func(t *testing.T) {
		fx := newCacheFwdFixture(t)
		_, foreign, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		foreignPub, err := ssh.NewPublicKey(foreign.Public())
		if err != nil {
			t.Fatal(err)
		}
		foreignFP := ssh.FingerprintSHA256(foreignPub)
		fx.presetServeAnchor(t, foreignPub.Marshal(), "blob", "tofu")

		sess := fx.newBroker(t, nil, fx.cred)
		text, isErr := callExec(t, sess, fx.targetID, "hi")
		if !isErr {
			t.Fatal("409 equal=false must fail the connection")
		}
		want := "ssh dial: ssh: handshake failed: the broker already has a different pin for [REDACTED] — run cache pull and retry; if it still fails, ask the owner to compare fingerprints (servers pin-hostkey)"
		if text != want {
			t.Fatalf("409 equal=false text mismatch:\n got: %q\nwant: %q", text, want)
		}

		// Recovery per the text: cache pull (REAL pull over pinned TLS) →
		// rehydrate → reconnect → the honest double-fingerprint refusal.
		dir := t.TempDir()
		t.Setenv("SSHMGR_CACHE_DIR", dir)
		dek, _ := store.GenerateMasterKey()
		mem := &store.MemKeyProvider{}
		_ = mem.Set(dek)
		prev := clientops.DekProvider
		clientops.DekProvider = func(string) store.KeyProvider { return mem }
		t.Cleanup(func() { clientops.DekProvider = prev })

		if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{StatusOut: io.Discard}); err != nil {
			t.Fatalf("pull: %v", err)
		}
		snap2, err := clientops.LoadCacheSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		sess2 := fx.newBrokerSnap(t, snap2, nil, fx.cred)
		text2, isErr := callExec(t, sess2, fx.targetID, "hi")
		if !isErr {
			t.Fatal("the rehydrated foreign anchor must reject the true key")
		}
		want2 := "ssh dial: ssh: handshake failed: host key mismatch: possible MITM, connection rejected: presented " + fx.fingerprint() + " != pinned " + foreignFP
		if text2 != want2 {
			t.Fatalf("mismatch text mismatch:\n got: %q\nwant: %q", text2, want2)
		}
	})
}

// TestCacheForwardE2E_HangingServeTimesOut is T8b: the serve accepts and never
// responds — the forwarder's 10 s cap trips, the surfaced text is the §4 main
// message, and the handshake (and the test) stay bounded. The 10 s wait is the
// production constant by design (no test seam) — this is the suite's one slow
// test.
func TestCacheForwardE2E_HangingServeTimesOut(t *testing.T) {
	fx := newCacheFwdFixture(t)
	hang := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the response until the client gives up (the 10 s forward cap
		// cancels the request) — so the server closes cleanly after the
		// assertion instead of blocking on a 20 s sleep.
		select {
		case <-time.After(20 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer hang.Close()

	cred := fx.cred
	cred.URL = hang.URL
	cred.Pin = mcpserver.SPKIFingerprint(hang.Certificate())
	sess := fx.newBroker(t, nil, cred)

	start := time.Now()
	text, isErr := callExec(t, sess, fx.targetID, "hi")
	elapsed := time.Since(start)
	if !isErr {
		t.Fatal("a hanging serve must fail the connection")
	}
	if elapsed > 25*time.Second {
		t.Fatalf("the handshake hung %v — the 10 s forward cap must bound it", elapsed)
	}
	if text != fx.dialRefusedMainMessage() {
		t.Fatalf("timeout text mismatch:\n got: %q\nwant: %q", text, fx.dialRefusedMainMessage())
	}
}

// portFromAddr splits the port off a host:port literal (loopback fixtures).
func portFromAddr(addr string) int {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0
	}
	n := 0
	for _, c := range addr[idx+1:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
