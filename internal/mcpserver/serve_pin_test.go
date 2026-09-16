package mcpserver

// Plan 48 T1/T2/T2b/T3 (spec §8): the POST /pin-hostkey endpoint — the
// device-forwarded anchor landing with its full guard sequence (§1.2 ①–⑥).
// In-process ServeRunner + httptest, same pattern as serve_snapshot_test.go.

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testschema"
)

// pinTestHost/pinTestPort: the shared test target address. THREE entries point
// at it — two granted to the device's bound profile (the multi-candidate
// determinism pair) and one vault-wide outsider (the affects-list third) — so
// T1's "affects=N entries" and the server_name attribution have teeth.
const (
	pinTestHost = "192.0.2.10"
	pinTestPort = 22
)

// pinFixture is the seeded serve + store for the /pin-hostkey tests.
type pinFixture struct {
	srv         *httptest.Server
	st          *store.Store
	dbPath      string // for raw-connection fault injection (T3's 500/atomicity rows)
	laptopToken string // bound to team-a (gpu + mirror granted)
	phoneToken  string // bound to team-b (only "other" granted — no candidate at the target)
	projToken   string // project token — must 401 (never a remote credential)
	teamA       string
	minCandID   string // smallest-id team-a entry at host:port — the attribution anchor
	minCandName string
}

// newPinRunner seeds: gpu + mirror (team-a, both at 192.0.2.10:22), twin (same
// address, UNgranted — vault-wide affects member), other (team-b's only entry,
// different address), both device tokens, one project token.
func newPinRunner(t *testing.T) *pinFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "t.db")
	st := newStoreAt(t, dbPath)

	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	cid2, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("topsecret")})
	if err != nil {
		t.Fatal(err)
	}
	gpuID, err := st.AddServer(&models.Server{Name: "gpu", Host: pinTestHost, Port: pinTestPort, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	mirrorID, err := st.AddServer(&models.Server{Name: "mirror", Host: pinTestHost, Port: pinTestPort, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{Name: "twin", Host: pinTestHost, Port: pinTestPort, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid2}); err != nil {
		t.Fatal(err)
	}
	otherID, err := st.AddServer(&models.Server{Name: "other", Host: "10.9.9.9", Port: 2222, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	teamA, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := st.AddProfile("team-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(teamA, []string{gpuID, mirrorID}); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(teamB, []string{otherID}); err != nil {
		t.Fatal(err)
	}
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, laptopToken, err := st.AddCacheToken("laptop", teamA)
	if err != nil {
		t.Fatal(err)
	}
	_, phoneToken, err := st.AddCacheToken("phone", teamB)
	if err != nil {
		t.Fatal(err)
	}
	_, projToken, err := st.AddProject("proj-x", teamA)
	if err != nil {
		t.Fatal(err)
	}
	// Determinism anchor: of the two bound-profile candidates at the target,
	// the SMALLEST id is both the audit attribution and the response name.
	minCandID, minCandName := gpuID, "gpu"
	if mirrorID < gpuID {
		minCandID, minCandName = mirrorID, "mirror"
	}
	return &pinFixture{
		srv: srv, st: st, dbPath: dbPath,
		laptopToken: laptopToken, phoneToken: phoneToken, projToken: projToken,
		teamA: teamA, minCandID: minCandID, minCandName: minCandName,
	}
}

// pinTestKey mints a fresh ed25519 host key as (base64 wire blob, canonical
// marshaled bytes, SHA256 fingerprint) — hermetic, no listener needed.
func pinTestKey(t *testing.T) (b64 string, canonical []byte, fp string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pk.Marshal()), pk.Marshal(), ssh.FingerprintSHA256(pk)
}

// postPin fires one POST /pin-hostkey with the given bearer token and body.
func postPin(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/pin-hostkey", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

// pinBody renders the §1.1 request shape.
func pinBody(host string, port int, keyB64 string) string {
	return fmt.Sprintf(`{"host":%q,"port":%d,"key_blob":%q}`, host, port, keyB64)
}

// rawSQLite runs one statement on a second raw connection to the store file —
// the fault-injection seam for T3 (a mangled schema the public API cannot
// produce). Same trick as store-side tests (DROP TABLE audit_log).
func rawSQLite(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("raw %q: %v", stmt, err)
	}
}

// pinAnchorCount returns (host_keys rows at the target, audit rows) — the
// zero-side-effect assertion pair for every rejection path.
func pinAnchorCount(t *testing.T, f *pinFixture, host string, port int) (int, int) {
	t.Helper()
	pin, err := f.st.GetHostKey(host, port)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if pin != nil {
		n = 1
	}
	rows, err := f.st.AuditRows(100)
	if err != nil {
		t.Fatal(err)
	}
	return n, len(rows)
}

// TestPinHostkey_ForwardsAndLands201 (T1): the full chain — 201 with the
// canonical fingerprint + the deterministic server_name; the anchor lands with
// blob/forward/laptop metadata (asserted through the profile-snapshot export,
// the mcpserver-visible read path for the pin columns); exactly one audit row
// whose every field matches, command carrying the vault-wide affects list.
func TestPinHostkey_ForwardsAndLands201(t *testing.T) {
	f := newPinRunner(t)
	blob, canonical, fp := pinTestKey(t)

	status, body := postPin(t, f.srv, f.laptopToken, pinBody(pinTestHost, pinTestPort, blob))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	var created struct {
		Fingerprint string `json:"fingerprint"`
		ServerName  string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("response not the §1.1 JSON: %v\nbody=%s", err, body)
	}
	if created.Fingerprint != fp {
		t.Fatalf("response fingerprint %q != stored-key fingerprint %q", created.Fingerprint, fp)
	}
	if created.ServerName != f.minCandName {
		t.Fatalf("server_name = %q, want the smallest-id candidate %q", created.ServerName, f.minCandName)
	}

	// the anchor: canonical bytes, blob format (GetHostKey) + the three pin
	// columns via the snapshot export (pin_source=forward, pin_device=laptop).
	pin, err := f.st.GetHostKey(pinTestHost, pinTestPort)
	if err != nil || pin == nil {
		t.Fatalf("anchor did not land: %v, %v", pin, err)
	}
	if pin.Format != store.PinFormatBlob {
		t.Fatalf("pin format = %q, want blob", pin.Format)
	}
	if string(pin.Blob) != string(canonical) {
		t.Fatalf("stored blob is not the re-marshaled canonical bytes")
	}
	snap, err := f.st.ExportSnapshotForProfile(f.teamA)
	if err != nil {
		t.Fatal(err)
	}
	var exported *store.SnapshotHostKey
	for i := range snap.HostKeys {
		if snap.HostKeys[i].HostPort == fmt.Sprintf("%s:%d", pinTestHost, pinTestPort) {
			exported = &snap.HostKeys[i]
		}
	}
	if exported == nil {
		t.Fatal("granted target's anchor missing from the profile snapshot")
	}
	if exported.PinFormat != "blob" || exported.PinSource != "forward" || exported.PinDevice != "laptop" {
		t.Fatalf("exported pin metadata = %q/%q/%q, want blob/forward/laptop",
			exported.PinFormat, exported.PinSource, exported.PinDevice)
	}

	// the audit row: every field, same transaction as the anchor.
	rows, err := f.st.AuditRows(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("exactly one audit row must exist, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "pin-forward" || row.ServerID != f.minCandID || row.ProjectID != "" || row.Status != "ok" || row.TS.IsZero() {
		t.Fatalf("audit row fields wrong: action=%q server=%q project=%q status=%q ts=%v",
			row.Action, row.ServerID, row.ProjectID, row.Status, row.TS)
	}
	// affects = the OWNER full-vault view (ListServers): gpu, mirror, twin —
	// the ungranted twin included; the device never sees this line.
	wantCmd := fmt.Sprintf("host=%s:%d fp=%s device=laptop via=forward affects=3 entries: gpu, mirror, twin",
		pinTestHost, pinTestPort, fp)
	if row.Command != wantCmd {
		t.Fatalf("audit command:\n got %q\nwant %q", row.Command, wantCmd)
	}
}

// TestPinHostkey_ConcurrentDistinctKeysOneWinner (T2): N concurrent distinct
// keys racing one unpinned host:port — EXACTLY one 201, the rest 409 with
// equal=false, and the stored anchor is the winner's key (insert-only).
func TestPinHostkey_ConcurrentDistinctKeysOneWinner(t *testing.T) {
	f := newPinRunner(t)
	const n = 6

	type posted struct {
		canonical []byte
		status    int
		body      string
	}
	results := make(chan posted, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			blob, canonical, _ := pinTestKey(t)
			status, body := postPin(t, f.srv, f.laptopToken, pinBody(pinTestHost, pinTestPort, blob))
			results <- posted{canonical, status, body}
		}()
	}
	wg.Wait()
	close(results)

	created := 0
	var winner []byte
	for r := range results {
		if r.status == http.StatusCreated {
			created++
			winner = r.canonical
			continue
		}
		if r.status != http.StatusConflict {
			t.Fatalf("loser status = %d, want 409; body=%s", r.status, r.body)
		}
		if !strings.Contains(r.body, `"equal":false`) {
			t.Fatalf("loser body must carry equal:false verbatim, got %s", r.body)
		}
		if strings.Contains(r.body, "SHA256:") {
			t.Fatalf("409 must never echo a fingerprint: %s", r.body)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want exactly 1 (insert-only)", created)
	}
	pin, err := f.st.GetHostKey(pinTestHost, pinTestPort)
	if err != nil || pin == nil {
		t.Fatalf("no anchor after the race: %v", err)
	}
	if string(pin.Blob) != string(winner) {
		t.Fatal("stored anchor is not the winner's key — overwrite or corruption")
	}
	if rows, _ := f.st.AuditRows(100); len(rows) != 1 {
		t.Fatalf("exactly the winner's audit row must exist, got %d", len(rows))
	}
}

// TestPinHostkey_PreExistingAnchorConflict (T2b): an existing equal anchor
// answers 409 equal=true and stays byte-for-byte untouched (source still tofu,
// zero audit rows); a different key answers 409 equal=false, equally untouched.
func TestPinHostkey_PreExistingAnchorConflict(t *testing.T) {
	f := newPinRunner(t)
	blobA, canonicalA, _ := pinTestKey(t)
	blobB, _, _ := pinTestKey(t)

	if err := f.st.SaveHostKey(pinTestHost, pinTestPort, canonicalA); err != nil {
		t.Fatal(err)
	}

	status, body := postPin(t, f.srv, f.laptopToken, pinBody(pinTestHost, pinTestPort, blobA))
	if status != http.StatusConflict {
		t.Fatalf("equal anchor: status = %d, want 409; body=%s", status, body)
	}
	if !strings.Contains(body, `"error":"already pinned"`) || !strings.Contains(body, `"equal":true`) {
		t.Fatalf("equal-anchor body must be {\"error\":\"already pinned\",\"equal\":true}, got %s", body)
	}
	pin, err := f.st.GetHostKey(pinTestHost, pinTestPort)
	if err != nil || pin == nil || string(pin.Blob) != string(canonicalA) || pin.Format != store.PinFormatBlob {
		t.Fatalf("existing anchor disturbed by the equal conflict: %+v %v", pin, err)
	}
	snap, err := f.st.ExportSnapshotForProfile(f.teamA)
	if err != nil {
		t.Fatal(err)
	}
	for _, hk := range snap.HostKeys {
		if hk.HostPort == fmt.Sprintf("%s:%d", pinTestHost, pinTestPort) && hk.PinSource != "tofu" {
			t.Fatalf("conflict path mutated pin_source: %q", hk.PinSource)
		}
	}
	if _, auditN := pinAnchorCount(t, f, pinTestHost, pinTestPort); auditN != 0 {
		t.Fatalf("conflict path must write no audit rows, got %d", auditN)
	}

	// a different key: 409 equal=false, anchor still untouched.
	status, body = postPin(t, f.srv, f.laptopToken, pinBody(pinTestHost, pinTestPort, blobB))
	if status != http.StatusConflict || !strings.Contains(body, `"equal":false`) {
		t.Fatalf("different key: status=%d body=%s, want 409 equal:false", status, body)
	}
	if pin, _ := f.st.GetHostKey(pinTestHost, pinTestPort); pin == nil || string(pin.Blob) != string(canonicalA) {
		t.Fatal("existing anchor overwritten by a conflicting forward")
	}
}

// TestPinHostkey_NoCandidate403NoProfileEcho (T3): a valid device code whose
// bound profile has no entry at the target address gets 403 — and the body
// names neither the profile's entries nor any other vault content.
func TestPinHostkey_NoCandidate403NoProfileEcho(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)

	status, body := postPin(t, f.srv, f.phoneToken, pinBody(pinTestHost, pinTestPort, blob))
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", status, body)
	}
	// The requester-supplied address IS echoed (the §2.1 client text does the
	// same — it leaks nothing the device didn't send); the entry/profile
	// shapes behind it must not appear.
	for _, leaked := range []string{"gpu", "mirror", "twin", "team-a", "10.9.9.9"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("403 body leaks vault/profile content %q: %s", leaked, body)
		}
	}
	if n, a := pinAnchorCount(t, f, pinTestHost, pinTestPort); n != 0 || a != 0 {
		t.Fatalf("refused pin must land nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_UnboundToken403 (T3): a valid-but-unbound (legacy migrated)
// device code is refused with 403, never 401 — same discipline as /snapshot
// (a pinned 401 is the Plan-34 quarantine trigger).
func TestPinHostkey_UnboundToken403(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-unbound.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(testschema.OldShapeCacheTokens); err != nil {
		t.Fatal(err)
	}
	tok, err := store.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	if _, err := db.Exec(`INSERT INTO cache_tokens (id,name,token_hash,token_salt,token_prefix,status,created_at,updated_at)
		VALUES ('ct1','laptop-legacy',?,?,?,'active',1,1)`, store.HashToken([]byte(tok), salt), salt, tok[:8]); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := store.Open(path, make([]byte, 32)) // migrate() adds profile_id (NULL = unbound)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)

	blob, _, _ := pinTestKey(t)
	status, body := postPin(t, srv, tok, pinBody(pinTestHost, pinTestPort, blob))
	if status != http.StatusForbidden {
		t.Fatalf("unbound device code: status = %d, want 403 (never 401 — Plan 34); body=%s", status, body)
	}
	if !strings.Contains(body, "bind") {
		t.Fatalf("403 body must name the owner repair (cache-tokens bind), got: %s", body)
	}
}

// TestPinHostkey_BadShape400 (T3): ① guards — undecodable JSON, missing
// host/port/key_blob, and wrong-typed port ALL answer the §1.1 400 body and
// land nothing.
func TestPinHostkey_BadShape400(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)

	cases := map[string]string{
		"undecodable":     `{"host": `,
		"missing host":    fmt.Sprintf(`{"port":%d,"key_blob":%q}`, pinTestPort, blob),
		"missing port":    fmt.Sprintf(`{"host":%q,"key_blob":%q}`, pinTestHost, blob),
		"missing keyblob": fmt.Sprintf(`{"host":%q,"port":%d}`, pinTestHost, pinTestPort),
		"empty keyblob":   fmt.Sprintf(`{"host":%q,"port":%d,"key_blob":""}`, pinTestHost, pinTestPort),
		"wrong-type port": fmt.Sprintf(`{"host":%q,"port":"22","key_blob":%q}`, pinTestHost, blob),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, got := postPin(t, f.srv, f.laptopToken, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, got)
			}
			if !strings.Contains(got, `"error":"unparseable key_blob"`) {
				t.Fatalf("400 body must be the §1.1 verbatim shape, got %s", got)
			}
			if n, a := pinAnchorCount(t, f, pinTestHost, pinTestPort); n != 0 || a != 0 {
				t.Fatalf("rejected shape must land nothing: anchors=%d audit=%d", n, a)
			}
		})
	}
}

// TestPinHostkey_Auth401 (T3): project tokens and bad/absent device codes are
// plain RequireBearerToken 401s — no quarantine semantics on this route.
func TestPinHostkey_Auth401(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)
	body := pinBody(pinTestHost, pinTestPort, blob)

	for name, token := range map[string]string{
		"project token": f.projToken,
		"bad code":      "definitely-not-a-real-code-123456",
		"no token":      "",
	} {
		t.Run(name, func(t *testing.T) {
			status, got := postPin(t, f.srv, token, body)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", status, got)
			}
		})
	}
	if n, a := pinAnchorCount(t, f, pinTestHost, pinTestPort); n != 0 || a != 0 {
		t.Fatalf("unauthenticated attempts must land nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_Method405 (T3): non-POST (with a VALID token — the method
// guard is the handler's job ①, not the auth layer's).
func TestPinHostkey_Method405(t *testing.T) {
	f := newPinRunner(t)
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/pin-hostkey", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.laptopToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /pin-hostkey = %d, want 405", res.StatusCode)
	}
}

// TestPinHostkey_OverCap413 (T3): a body over the new 64 KiB cap is refused
// with 413 and lands nothing.
func TestPinHostkey_OverCap413(t *testing.T) {
	f := newPinRunner(t)
	huge := pinBody(pinTestHost, pinTestPort, strings.Repeat("A", 70<<10))
	status, body := postPin(t, f.srv, f.laptopToken, huge)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", status, body)
	}
	if n, a := pinAnchorCount(t, f, pinTestHost, pinTestPort); n != 0 || a != 0 {
		t.Fatalf("over-cap attempt must land nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_MalformedKeyBlob400 (T3): base64 that decodes but is not an
// SSH public key → the 400 body, and ZERO rows written anywhere.
func TestPinHostkey_MalformedKeyBlob400(t *testing.T) {
	f := newPinRunner(t)
	notAKey := base64.StdEncoding.EncodeToString([]byte("definitely not an ssh public key"))
	status, body := postPin(t, f.srv, f.laptopToken, pinBody(pinTestHost, pinTestPort, notAKey))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
	if !strings.Contains(body, `"error":"unparseable key_blob"`) {
		t.Fatalf("400 body must be the §1.1 verbatim shape, got %s", body)
	}
	if n, a := pinAnchorCount(t, f, pinTestHost, pinTestPort); n != 0 || a != 0 {
		t.Fatalf("malformed key must land nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_GetCacheTokenFault500 (T3): a store fault resolving the
// device row is 500 + stderr — NOT 403 (403 would send the owner chasing
// `cache-tokens bind` while the real DB error stays buried; the handleSnapshot
// lesson). Injection: drop created_at from cache_tokens on a raw connection —
// GetCacheToken's SELECT names it, VerifyCacheToken's (the auth middleware's)
// does not, so 401-vs-500 is cleanly separated.
func TestPinHostkey_GetCacheTokenFault500(t *testing.T) {
	f := newPinRunner(t)
	rawSQLite(t, f.dbPath, `ALTER TABLE cache_tokens DROP COLUMN created_at`)

	blob, _, _ := pinTestKey(t)
	status, body := postPin(t, f.srv, f.laptopToken, pinBody(pinTestHost, pinTestPort, blob))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", status, body)
	}
	if n, _ := pinAnchorCount(t, f, pinTestHost, pinTestPort); n != 0 {
		t.Fatal("no anchor may land when the device row cannot be resolved")
	}
}

// TestPinHostkey_AuditFailureAnchorNotLanded (T1 atomicity): when the same-tx
// audit write fails, the anchor must NOT land. Injection: DROP TABLE audit_log
// (the store-side T9 trick) — every pre-insert guard passes, InsertForwardedPin
// fails, the tx rolls back, the endpoint answers 500.
func TestPinHostkey_AuditFailureAnchorNotLanded(t *testing.T) {
	f := newPinRunner(t)
	rawSQLite(t, f.dbPath, `DROP TABLE audit_log`)

	blob, _, _ := pinTestKey(t)
	status, body := postPin(t, f.srv, f.laptopToken, pinBody(pinTestHost, pinTestPort, blob))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (store fault, not an auth verdict); body=%s", status, body)
	}
	pin, err := f.st.GetHostKey(pinTestHost, pinTestPort)
	if err != nil {
		t.Fatal(err)
	}
	if pin != nil {
		t.Fatalf("anchor landed despite the audit failure — the pin is not atomic with its history: %+v", pin)
	}
}
