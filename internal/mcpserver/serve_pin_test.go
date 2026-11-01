package mcpserver

// POST /pin-hostkey（票 #58）：转发请求携带条目标识符、握手地址与主机
// 密钥；权威端检查设备码轮廓、候选地址、虚拟局域网接口（本票恒为空），
// 仅可新增落锚并与审计同事务。夹具为进程内 ServeRunner + httptest。

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

const (
	pinTestHost = "192.0.2.10"
	pinTestPort = 22
)

// pinFixture 是 /pin-hostkey 测试的服务端夹具：gpu 与 mirror（team-a，同一
// 兜底地址上的两个条目）、twin（同地址但未授权给 team-a）、other（team-b
// 的条目，另一地址）。
type pinFixture struct {
	srv         *httptest.Server
	st          *store.Store
	dbPath      string
	laptopToken string
	phoneToken  string
	projToken   string
	teamA       string
	gpuID       string
	mirrorID    string
	twinID      string
	otherID     string
}

func newPinRunner(t *testing.T) *pinFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "t.db")
	st := newStoreAt(t, dbPath)

	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
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
	twinID, err := st.AddServer(&models.Server{Name: "twin", Host: pinTestHost, Port: pinTestPort, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
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
	return &pinFixture{
		srv: srv, st: st, dbPath: dbPath,
		laptopToken: laptopToken, phoneToken: phoneToken, projToken: projToken,
		teamA: teamA, gpuID: gpuID, mirrorID: mirrorID, twinID: twinID, otherID: otherID,
	}
}

// pinTestKey mints a fresh ed25519 host key as (base64 wire blob, canonical
// marshaled bytes, SHA256 fingerprint).
func pinTestKey(t *testing.T) (b64 string, canonical []byte, fp string) {
	t.Helper()
	b64, canonical, fp, err := mintPinTestKey()
	if err != nil {
		t.Fatal(err)
	}
	return b64, canonical, fp
}

func mintPinTestKey() (b64 string, canonical []byte, fp string, err error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, "", err
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", nil, "", err
	}
	return base64.StdEncoding.EncodeToString(pk.Marshal()), pk.Marshal(), ssh.FingerprintSHA256(pk), nil
}

func postPin(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	status, respBody, err := postPinErr(srv, token, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, respBody
}

func postPinErr(srv *httptest.Server, token, body string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/pin-hostkey", strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, "", err
	}
	return res.StatusCode, string(b), nil
}

func postPinChunked(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/pin-hostkey", io.NopCloser(strings.NewReader(body)))
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

func pinAddr() string { return fmt.Sprintf("%s:%d", pinTestHost, pinTestPort) }

// pinBody 渲染新格式转发请求：条目标识符 + 握手地址 + 密钥。
func pinBody(serverID, handshakeAddr, keyB64 string) string {
	return fmt.Sprintf(`{"server_id":%q,"handshake_addr":%q,"key_blob":%q}`, serverID, handshakeAddr, keyB64)
}

// pinOldBody 渲染旧格式请求（只有主机与端口，没有条目标识符）。
func pinOldBody(host string, port int, keyB64 string) string {
	return fmt.Sprintf(`{"host":%q,"port":%d,"key_blob":%q}`, host, port, keyB64)
}

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

// pinAnchorCount 返回（按条目落库的锚数量，审计行数），用于拒绝路径的
// 「零副作用」断言。
func pinAnchorCount(t *testing.T, f *pinFixture) (int, int) {
	t.Helper()
	pins, err := f.st.ListEntryPins()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.st.AuditRows(100)
	if err != nil {
		t.Fatal(err)
	}
	return len(pins), len(rows)
}

func entryPinOf(t *testing.T, f *pinFixture, id string) *store.Pin {
	t.Helper()
	srv, err := f.st.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := f.st.GetEntryPin(srv)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

// TestPinHostkey_ForwardsAndLands201：带条目标识符与握手地址的成功落库。
// 响应维持原形状（201 指纹与条目名）；锚与元数据（来源、转发设备、学习
// 地址）只落在请求指向的条目上；审计行带条目、指纹、设备与握手地址。
func TestPinHostkey_ForwardsAndLands201(t *testing.T) {
	f := newPinRunner(t)
	blob, canonical, fp := pinTestKey(t)

	status, body := postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), blob))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	var created struct {
		Fingerprint string `json:"fingerprint"`
		ServerName  string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("response not JSON: %v\nbody=%s", err, body)
	}
	if created.Fingerprint != fp {
		t.Fatalf("response fingerprint %q != stored-key fingerprint %q", created.Fingerprint, fp)
	}
	if created.ServerName != "gpu" {
		t.Fatalf("server_name = %q, want the requested entry %q", created.ServerName, "gpu")
	}

	pin := entryPinOf(t, f, f.gpuID)
	if pin == nil || pin.Format != store.PinFormatBlob || string(pin.Blob) != string(canonical) {
		t.Fatalf("anchor did not land on the requested entry: %+v", pin)
	}
	pins, err := f.st.ListEntryPins()
	if err != nil || len(pins) != 1 {
		t.Fatalf("exactly one entry anchor must exist: %+v %v", pins, err)
	}
	got := pins[0]
	if got.ServerID != f.gpuID || got.Source != store.PinSourceForward || got.Device != "laptop" || got.HandshakeAddr != pinAddr() {
		t.Fatalf("anchor metadata = %+v, want gpu/forward/laptop with the handshake address", got)
	}
	// 同地址的另一个条目不受影响（独立锚定）。
	if other := entryPinOf(t, f, f.mirrorID); other != nil {
		t.Fatalf("anchor leaked to a sibling entry at the same address: %+v", other)
	}

	rows, err := f.st.AuditRows(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("exactly one audit row must exist, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "pin-forward" || row.ServerID != f.gpuID || row.ProjectID != "" || row.Status != "ok" || row.TS.IsZero() {
		t.Fatalf("audit row fields wrong: %+v", row)
	}
	for _, want := range []string{"gpu", pinAddr(), fp, "device=laptop", "via=forward"} {
		if !strings.Contains(row.Command, want) {
			t.Fatalf("audit command %q missing %q", row.Command, want)
		}
	}
}

// TestPinHostkey_SecondEntrySameAddressAnchorsIndependently：同一兜底地址上
// 的第二个条目用另一把密钥独立落锚（票 #58 用户故事 15、19）。
func TestPinHostkey_SecondEntrySameAddressAnchorsIndependently(t *testing.T) {
	f := newPinRunner(t)
	gpuBlob, gpuCanonical, _ := pinTestKey(t)
	mirrorBlob, mirrorCanonical, _ := pinTestKey(t)

	for _, tc := range []struct {
		id   string
		name string
		blob string
	}{
		{f.gpuID, "gpu", gpuBlob},
		{f.mirrorID, "mirror", mirrorBlob},
	} {
		status, body := postPin(t, f.srv, f.laptopToken, pinBody(tc.id, pinAddr(), tc.blob))
		if status != http.StatusCreated {
			t.Fatalf("entry %s: status = %d, want 201; body=%s", tc.name, status, body)
		}
	}
	if pin := entryPinOf(t, f, f.gpuID); pin == nil || !pin.Matches(gpuCanonical) {
		t.Fatalf("gpu anchor wrong: %+v", pin)
	}
	if pin := entryPinOf(t, f, f.mirrorID); pin == nil || !pin.Matches(mirrorCanonical) {
		t.Fatalf("mirror anchor wrong: %+v", pin)
	}
}

// TestPinHostkey_EntryOutsideProfile403：条目不在设备码绑定 profile 内被拒，
// 响应不泄露库内其他条目或 profile 形状。
func TestPinHostkey_EntryOutsideProfile403(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)

	cases := []struct {
		name  string
		token string
		id    string
		addr  string
	}{
		{"ungranted same-address entry", f.laptopToken, f.twinID, pinAddr()},
		{"entry of another profile", f.phoneToken, f.gpuID, pinAddr()},
		{"unknown entry id", f.laptopToken, "no-such-entry", pinAddr()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postPin(t, f.srv, tc.token, pinBody(tc.id, tc.addr, blob))
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", status, body)
			}
			for _, leaked := range []string{"gpu", "mirror", "twin", "team-a", "team-b", "10.9.9.9"} {
				if strings.Contains(body, leaked) {
					t.Fatalf("403 body leaks vault/profile content %q: %s", leaked, body)
				}
			}
		})
	}
	if n, a := pinAnchorCount(t, f); n != 0 || a != 0 {
		t.Fatalf("refused pin must land nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_HandshakeAddrNotCandidate400：握手地址不是该条目的候选地址
// （本票阶段即兜底 Host 加端口）时拒绝。
func TestPinHostkey_HandshakeAddrNotCandidate400(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)

	status, body := postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, "10.9.9.9:2222", blob))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
	if !strings.Contains(body, "handshake") {
		t.Fatalf("400 body should name the handshake address problem: %s", body)
	}
	if n, a := pinAnchorCount(t, f); n != 0 || a != 0 {
		t.Fatalf("wrong-address pin must land nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_OldFormatRejected400：缺少条目标识符的旧格式请求一律拒绝，
// 错误信息提示升级客户端。
func TestPinHostkey_OldFormatRejected400(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)

	status, body := postPin(t, f.srv, f.laptopToken, pinOldBody(pinTestHost, pinTestPort, blob))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
	if !strings.Contains(strings.ToLower(body), "upgrade") {
		t.Fatalf("400 body must tell the user to upgrade the client: %s", body)
	}
	if n, a := pinAnchorCount(t, f); n != 0 || a != 0 {
		t.Fatalf("old-format pin must land nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_ConcurrentDistinctKeysOneWinner：并发请求只有一个成功，
// 其余按密钥异同得到 409 equal。
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
			blob, canonical, _, err := mintPinTestKey()
			if err != nil {
				t.Errorf("worker key mint: %v", err)
				return
			}
			status, body, err := postPinErr(f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), blob))
			if err != nil {
				t.Errorf("worker post: %v", err)
				return
			}
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
	if pin := entryPinOf(t, f, f.gpuID); pin == nil || !pin.Matches(winner) {
		t.Fatalf("stored anchor is not the winner's key — overwrite or corruption: %+v", pin)
	}
	if rows, _ := f.st.AuditRows(100); len(rows) != 1 {
		t.Fatalf("exactly the winner's audit row must exist, got %d", len(rows))
	}
}

// TestPinHostkey_PreExistingAnchorConflict：条目已有锚时绝不覆盖——密钥相同
// 返回 409 equal=true，不同返回 equal=false；两者都不写审计。
func TestPinHostkey_PreExistingAnchorConflict(t *testing.T) {
	f := newPinRunner(t)
	blobA, canonicalA, _ := pinTestKey(t)
	blobB, _, _ := pinTestKey(t)

	gpu, err := f.st.GetServer(f.gpuID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SaveEntryPin(gpu, canonicalA); err != nil {
		t.Fatal(err)
	}

	status, body := postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), blobA))
	if status != http.StatusConflict || !strings.Contains(body, `"equal":true`) {
		t.Fatalf("equal anchor: status=%d body=%s, want 409 equal:true", status, body)
	}
	if pin := entryPinOf(t, f, f.gpuID); pin == nil || !pin.Matches(canonicalA) {
		t.Fatalf("existing anchor disturbed by the equal conflict: %+v", pin)
	}

	status, body = postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), blobB))
	if status != http.StatusConflict || !strings.Contains(body, `"equal":false`) {
		t.Fatalf("different key: status=%d body=%s, want 409 equal:false", status, body)
	}
	if pin := entryPinOf(t, f, f.gpuID); pin == nil || !pin.Matches(canonicalA) {
		t.Fatal("existing anchor overwritten by a conflicting forward")
	}
	if n, a := pinAnchorCount(t, f); n != 1 || a != 0 {
		t.Fatalf("conflict paths must write nothing: anchors=%d audit=%d", n, a)
	}
}

// TestPinHostkey_EquivalentFingerprintAnchorIsConflict：owner 已写入同一密钥
// 的指纹格式锚时，转发请求没有插入新行，必须回答 409 且 equal=true。
func TestPinHostkey_EquivalentFingerprintAnchorIsConflict(t *testing.T) {
	f := newPinRunner(t)
	blob, canonical, fp := pinTestKey(t)
	gpu, err := f.st.GetServer(f.gpuID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertManualEntryPin(gpu, "", []byte(fp), store.PinFormatFingerprint, false, func(old *store.PinMeta) store.AuditRow {
		return store.AuditRow{Action: "pin-manual"}
	}); err != nil {
		t.Fatal(err)
	}

	status, body := postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), blob))
	if status != http.StatusConflict || !strings.Contains(body, `"equal":true`) {
		t.Fatalf("equivalent fingerprint anchor: status=%d body=%s, want 409 equal:true", status, body)
	}
	if pin := entryPinOf(t, f, f.gpuID); pin == nil || pin.Format != store.PinFormatFingerprint || !pin.Matches(canonical) {
		t.Fatalf("existing fingerprint anchor disturbed: %+v", pin)
	}
	forwards, err := f.st.QueryAudit(store.AuditFilter{Actions: []string{"pin-forward"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(forwards) != 0 {
		t.Fatalf("non-inserting forward must not write audit, got %+v", forwards)
	}
}

// TestPinHostkey_OversizedChunkedBodyReclassified413：无 Content-Length 的超大
// 请求体在解码时重新归类为 413。
func TestPinHostkey_OversizedChunkedBodyReclassified413(t *testing.T) {
	f := newPinRunner(t)
	big := pinBody(f.gpuID, pinAddr(), strings.Repeat("x", pinMaxBodyBytes+1024))
	status, body := postPinChunked(t, f.srv, f.laptopToken, big)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", status, body)
	}
}

// TestPinHostkey_UnboundToken403：未绑定 profile 的设备码被拒（403 而不是
// 401），提示 owner 用 cache-tokens bind 修复。
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

	st, err := store.Open(path, make([]byte, 32))
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
	status, body := postPin(t, srv, tok, pinBody("any", pinAddr(), blob))
	if status != http.StatusForbidden {
		t.Fatalf("unbound device code: status = %d, want 403; body=%s", status, body)
	}
	if !strings.Contains(body, "bind") {
		t.Fatalf("403 body must name the owner repair (cache-tokens bind), got: %s", body)
	}
}

// TestPinHostkey_BadShape400：无法解码、缺握手地址、缺密钥、空密钥都回答
// 400，且什么都不落。
func TestPinHostkey_BadShape400(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)

	cases := map[string]string{
		"undecodable":         `{"server_id": `,
		"missing handshake":   fmt.Sprintf(`{"server_id":%q,"key_blob":%q}`, f.gpuID, blob),
		"missing keyblob":     fmt.Sprintf(`{"server_id":%q,"handshake_addr":%q}`, f.gpuID, pinAddr()),
		"empty keyblob":       fmt.Sprintf(`{"server_id":%q,"handshake_addr":%q,"key_blob":""}`, f.gpuID, pinAddr()),
		"wrong-typed entryid": fmt.Sprintf(`{"server_id":123,"handshake_addr":%q,"key_blob":%q}`, pinAddr(), blob),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, got := postPin(t, f.srv, f.laptopToken, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, got)
			}
			if n, a := pinAnchorCount(t, f); n != 0 || a != 0 {
				t.Fatalf("rejected shape must land nothing: anchors=%d audit=%d", n, a)
			}
		})
	}
}

// TestPinHostkey_Auth401：项目令牌、错误设备码、缺令牌都是普通 401。
func TestPinHostkey_Auth401(t *testing.T) {
	f := newPinRunner(t)
	blob, _, _ := pinTestKey(t)
	body := pinBody(f.gpuID, pinAddr(), blob)

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
}

// TestPinHostkey_Method405：非 POST 方法被拒。
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

// TestPinHostkey_OverCap413：超过上限的请求体被拒。
func TestPinHostkey_OverCap413(t *testing.T) {
	f := newPinRunner(t)
	huge := pinBody(f.gpuID, pinAddr(), strings.Repeat("A", 70<<10))
	status, body := postPin(t, f.srv, f.laptopToken, huge)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", status, body)
	}
}

// TestPinHostkey_MalformedKeyBlob400：能解码但不是 SSH 公钥的字节被拒。
func TestPinHostkey_MalformedKeyBlob400(t *testing.T) {
	f := newPinRunner(t)
	notAKey := base64.StdEncoding.EncodeToString([]byte("definitely not an ssh public key"))
	status, body := postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), notAKey))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
	if !strings.Contains(body, "unparseable") {
		t.Fatalf("400 body should name the unparseable key: %s", body)
	}
}

// TestPinHostkey_GetCacheTokenFault500：解析设备码行的存储故障是 500，不是
// 授权结论。
func TestPinHostkey_GetCacheTokenFault500(t *testing.T) {
	f := newPinRunner(t)
	rawSQLite(t, f.dbPath, `ALTER TABLE cache_tokens DROP COLUMN created_at`)

	blob, _, _ := pinTestKey(t)
	status, body := postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), blob))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", status, body)
	}
	if n, _ := pinAnchorCount(t, f); n != 0 {
		t.Fatal("no anchor may land when the device row cannot be resolved")
	}
}

// TestPinHostkey_AuditFailureAnchorNotLanded：同一事务的审计写失败时锚不落库。
func TestPinHostkey_AuditFailureAnchorNotLanded(t *testing.T) {
	f := newPinRunner(t)
	rawSQLite(t, f.dbPath, `DROP TABLE audit_log`)

	blob, _, _ := pinTestKey(t)
	status, body := postPin(t, f.srv, f.laptopToken, pinBody(f.gpuID, pinAddr(), blob))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (store fault); body=%s", status, body)
	}
	if pin := entryPinOf(t, f, f.gpuID); pin != nil {
		t.Fatalf("anchor landed despite the audit failure: %+v", pin)
	}
}
