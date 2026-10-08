package mcpserver

// Plan 51 T4/T5 (spec §9): the POST /server-metadata endpoint — the guard
// sequence (§1.2 ①–⑦), the 409 one-hop-retry body, the third serve switch,
// and the stderr-line convention. In-process ServeRunner + httptest, same
// pattern as serve_pin_test.go (which also donates the shape of the fixture).

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// metaFixture seeds the metadata-edit surface: gpu + mirror granted to team-a
// (laptop's bound profile), other granted to team-b (phone) — the outsider
// path. Returns the runner too (switch tests need RefreshSwitches).
type metaFixture struct {
	srv         *httptest.Server
	st          *store.Store
	r           *ServeRunner
	laptopToken string
	phoneToken  string
	projToken   string
	gpuID       string
	otherID     string
}

func newMetaRunner(t *testing.T) *metaFixture {
	t.Helper()
	st := newStoreAt(t, filepath.Join(t.TempDir(), "meta.db"))

	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	gpuID, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		Hardware: "old-hw", Role: "edge", Caveats: "old-caveat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "mirror", Host: "192.0.2.11", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	}); err != nil {
		t.Fatal(err)
	}
	otherID, err := st.AddServer(&models.Server{
		Name: "other", Host: "10.9.9.9", Port: 2222, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
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
	if err := st.GrantServers(teamA, []string{gpuID}); err != nil {
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
	return &metaFixture{srv: srv, st: st, r: r,
		laptopToken: laptopToken, phoneToken: phoneToken, projToken: projToken,
		gpuID: gpuID, otherID: otherID}
}

// postMeta fires one POST /server-metadata; returns status + body.
func postMeta(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/server-metadata", strings.NewReader(body))
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

func TestServerMetadata_HappyPath(t *testing.T) {
	fx := newMetaRunner(t)
	status, body := postMeta(t, fx.srv, fx.laptopToken, `{
		"server_id": "`+fx.gpuID+`", "expected_revision": 0,
		"fields": {"hardware": "H200 x1", "caveats": ""}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, body)
	}
	var out metaEditResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.ServerName != "gpu" || out.Revision != 1 {
		t.Fatalf("200 body = %+v, want gpu/rev 1", out)
	}
	got, err := fx.st.GetServer(fx.gpuID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hardware != "H200 x1" || got.Caveats != "" || got.Role != "edge" {
		t.Fatalf("row after edit: hardware=%q caveats=%q role=%q", got.Hardware, got.Caveats, got.Role)
	}
	rows, err := fx.st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "meta-edit" || rows[0].ServerID != fx.gpuID {
		t.Fatalf("audit row: %+v", rows)
	}
	if !strings.Contains(rows[0].Command, "device=laptop") || !strings.Contains(rows[0].Command, `old.caveats="old-caveat"`) {
		t.Fatalf("audit command must carry device + old values: %s", rows[0].Command)
	}
}

func TestServerMetadata_409CarriesCurrentValues(t *testing.T) {
	fx := newMetaRunner(t)
	// First write succeeds (revision 0→1) with distinct values.
	if status, body := postMeta(t, fx.srv, fx.laptopToken, `{
		"server_id": "`+fx.gpuID+`", "expected_revision": 0,
		"fields": {"hardware": "H200", "role": "prod pg primary"}}`); status != http.StatusOK {
		t.Fatalf("first write: %d %s", status, body)
	}
	// Stale retry (still holds revision 0) → 409 + current_revision + the six
	// fields' current values (the agent's one-hop merge input).
	status, body := postMeta(t, fx.srv, fx.laptopToken, `{
		"server_id": "`+fx.gpuID+`", "expected_revision": 0,
		"fields": {"hardware": "stale"}}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	var cr metaConflictResponse
	if err := json.Unmarshal([]byte(body), &cr); err != nil {
		t.Fatal(err)
	}
	if cr.Error != "stale revision" || cr.CurrentRevision != 1 {
		t.Fatalf("409 body = %+v", cr)
	}
	if cr.Current["hardware"] != "H200" || cr.Current["role"] != "prod pg primary" || cr.Current["caveats"] != "old-caveat" {
		t.Fatalf("409 current values = %v", cr.Current)
	}
	for _, k := range store.MetadataFieldList() {
		if _, ok := cr.Current[k]; !ok {
			t.Fatalf("409 current map missing %q", k)
		}
	}
	// And the retry WITH the current revision converges in one hop.
	if status, _ := postMeta(t, fx.srv, fx.laptopToken, `{
		"server_id": "`+fx.gpuID+`", "expected_revision": 1,
		"fields": {"hardware": "H200 merged"}}`); status != http.StatusOK {
		t.Fatalf("one-hop retry must succeed, got %d", status)
	}
}

func TestServerMetadata_GrantRefused(t *testing.T) {
	fx := newMetaRunner(t)
	// phone (team-b) is granted only "other" — gpu is out of profile.
	status, body := postMeta(t, fx.srv, fx.phoneToken, `{
		"server_id": "`+fx.gpuID+`", "expected_revision": 0, "fields": {"role": "x"}}`)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(body, "refused") || !strings.Contains(body, "cache pull again") {
		t.Fatalf("403 body must be the grant-refusal text: %s", body)
	}
	// Deliberately vague: no profile names, no vault shape.
	if strings.Contains(body, "team-a") || strings.Contains(body, "team-b") {
		t.Fatalf("403 body leaked vault shape: %s", body)
	}
	if got, _ := fx.st.GetServer(fx.gpuID); got.Role != "edge" {
		t.Fatalf("refused write must not land: role=%q", got.Role)
	}
}

func TestServerMetadata_UnboundToken(t *testing.T) {
	// A pre-Plan-39 legacy unbound device code — the store API cannot mint
	// one (by design), so the row is inserted raw after the fixture store
	// closes (the serve_pin_test laptop-legacy pattern: NULL profile_id IS
	// the unbound migration state).
	path := filepath.Join(t.TempDir(), "unbound.db")
	st := newStoreAt(t, path)
	gpuID, err := st.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	teamA, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(teamA, []string{gpuID}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
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
		VALUES ('ct1','laptop-legacy',?,?,?,'active',1,1)`,
		store.HashToken([]byte(tok), salt), salt, tok[:8]); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st2, err := store.Open(path, make([]byte, 32)) // migrate() adds profile_id (NULL = unbound)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	r, err := NewServeRunner(st2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)

	status, body := postMeta(t, srv, tok, `{
		"server_id": "`+gpuID+`", "expected_revision": 0, "fields": {"role": "x"}}`)
	if status != http.StatusForbidden || !strings.Contains(body, "cache-tokens bind") {
		t.Fatalf("unbound: %d %s", status, body)
	}
}

func TestServerMetadata_SwitchOff(t *testing.T) {
	fx := newMetaRunner(t)
	if err := fx.st.SetSetting(settingMetaEdit, "false"); err != nil {
		t.Fatal(err)
	}
	fx.r.RefreshSwitches(nil, nil, nil, nil, nil, nil) // rebuild the ≤5s memo now
	status, body := postMeta(t, fx.srv, fx.laptopToken, `{
		"server_id": "`+fx.gpuID+`", "expected_revision": 0, "fields": {"role": "x"}}`)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(body, "disabled") {
		t.Fatalf("switch-off 403 body must carry the disabled marker: %s", body)
	}
	if got, _ := fx.st.GetServer(fx.gpuID); got.Role != "edge" {
		t.Fatalf("disabled write must not land: role=%q", got.Role)
	}
	// Flip back on: the same request succeeds (no restart needed).
	if err := fx.st.SetSetting(settingMetaEdit, "true"); err != nil {
		t.Fatal(err)
	}
	fx.r.RefreshSwitches(nil, nil, nil, nil, nil, nil)
	if status, _ := postMeta(t, fx.srv, fx.laptopToken, `{
		"server_id": "`+fx.gpuID+`", "expected_revision": 0, "fields": {"role": "x"}}`); status != http.StatusOK {
		t.Fatalf("switch back on must admit the write, got %d", status)
	}
}

func TestServerMetadata_ShapeGuards(t *testing.T) {
	fx := newMetaRunner(t)
	cases := []struct {
		name   string
		method string
		body   string
		token  string
		want   int
	}{
		{"bad json", http.MethodPost, `{`, fx.laptopToken, http.StatusBadRequest},
		{"empty server_id", http.MethodPost, `{"server_id":"","expected_revision":0,"fields":{"role":"x"}}`, fx.laptopToken, http.StatusBadRequest},
		{"negative revision", http.MethodPost, `{"server_id":"x","expected_revision":-1,"fields":{"role":"x"}}`, fx.laptopToken, http.StatusBadRequest},
		{"unknown field", http.MethodPost, `{"server_id":"` + fx.gpuID + `","expected_revision":0,"fields":{"host":"1.2.3.4"}}`, fx.laptopToken, http.StatusBadRequest},
		{"null field value", http.MethodPost, `{"server_id":"` + fx.gpuID + `","expected_revision":0,"fields":{"role":null}}`, fx.laptopToken, http.StatusBadRequest},
		{"no fields", http.MethodPost, `{"server_id":"` + fx.gpuID + `","expected_revision":0,"fields":{}}`, fx.laptopToken, http.StatusBadRequest},
		{"oversized field", http.MethodPost, `{"server_id":"` + fx.gpuID + `","expected_revision":0,"fields":{"role":"` + strings.Repeat("x", 4097) + `"}}`, fx.laptopToken, http.StatusBadRequest},
		{"project token is not a device code", http.MethodPost, `{"server_id":"` + fx.gpuID + `","expected_revision":0,"fields":{"role":"x"}}`, fx.projToken, http.StatusUnauthorized},
		{"bad token", http.MethodPost, `{"server_id":"` + fx.gpuID + `","expected_revision":0,"fields":{"role":"x"}}`, "not-a-token", http.StatusUnauthorized},
		{"GET refused", http.MethodGet, "", fx.laptopToken, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, fx.srv.URL+"/server-metadata", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tc.token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("%s: status = %d (body %s), want %d", tc.name, res.StatusCode, string(b), tc.want)
		}
	}
}

func TestServerMetadata_BodyCap(t *testing.T) {
	fx := newMetaRunner(t)
	// A body past the 32 KiB cap — with a set ContentLength it must be
	// refused 413 before any decode.
	big := `{"server_id":"` + fx.gpuID + `","expected_revision":0,"fields":{"description":"` + strings.Repeat("x", 40*1024) + `"}}`
	if status, _ := postMeta(t, fx.srv, fx.laptopToken, big); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: want 413, got %d", status)
	}
}

// TestMetadataEditSwitchLayers pins the third switch's four-layer precedence
// (the switches_test injection pattern, applied to MetadataEditEnabled).
func TestMetadataEditSwitchLayers(t *testing.T) {
	st := newStoreAt(t, filepath.Join(t.TempDir(), "sw.db"))
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tv, fv := true, false

	if !r.MetadataEditEnabled() {
		t.Fatal("default must be ON (Q3-A: upgrade = live)")
	}
	if err := st.SetSetting(settingMetaEdit, "false"); err != nil {
		t.Fatal(err)
	}
	r.RefreshSwitches(nil, nil, nil, nil, nil, nil)
	if r.MetadataEditEnabled() {
		t.Fatal("store=false must disable (env/flag unset)")
	}
	r.RefreshSwitches(nil, nil, nil, nil, &tv, nil)
	if !r.MetadataEditEnabled() {
		t.Fatal("explicit env=true must beat store=false")
	}
	r.RefreshSwitches(nil, nil, nil, nil, nil, &fv)
	if r.MetadataEditEnabled() {
		t.Fatal("explicit flag=false must beat store=true")
	}
	if err := st.SetSetting(settingMetaEdit, "true"); err != nil {
		t.Fatal(err)
	}
	r.RefreshSwitches(nil, nil, nil, nil, nil, nil)
	if !r.MetadataEditEnabled() {
		t.Fatal("store=true re-enables")
	}
	// Independence: a meta injection must not disturb the other switches.
	r.RefreshSwitches(nil, nil, nil, nil, &fv, nil)
	if r.PairingEnabled() != true || r.MetadataEditEnabled() {
		t.Fatal("meta flag injection must not disturb pairing, and must flip meta only")
	}
}
