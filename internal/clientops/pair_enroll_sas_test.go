package clientops

// pair_enroll_sas_test.go —— 2026-09-01 SAS 落行修复的黄金断言(真 /pair 面):
//
//   1. TestPairEnroll_LandsClientMatchedSASInRow:enroll 后 pending 行的 sas ==
//      client 侧同输入复算的 pairing.SAS —— 批准面(TUI/CLI)读到的码与 client
//      屏显示的码同源。两个不同 client 密钥各 enroll 一次,行值各自匹配 —— 即
//      换钥(换 client_pub)必换 SAS,MITM 换钥腿在 server 侧行值上可辨。
//   2. TestPairEnroll_LowOrderClientPubRejected:低阶 client_pub(全零)在 enroll
//      的 ECDH 处即 400,不再拖到 finish 才暴露。
//
// client 侧复算式与 pair.go 的三件套打印同一冻结构造(domain "sshmgr-pair-v2"
// + transcript 各段序)与同一纯函数(pairing.SAS)。

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ssh-manager-mcp/internal/pairing"
)

// enrollClient is one client-side pairing keyset + its enroll HTTP result.
type enrollClient struct {
	id       [32]byte
	priv     ecdh.PrivateKey
	cnonce   []byte
	serverPB []byte
	snonce   []byte
}

// enrollAt POSTs a real /pair/enroll for a fresh client keyset and keeps the
// response's public values (server_pub/snonce) for the client-side SAS
// recomputation. target must be a plain URL string (goes into the transcript).
func enrollAt(t *testing.T, srv *pairingServer, name, target string, clientPubOverride []byte) (*enrollClient, int) {
	t.Helper()
	c := &enrollClient{}
	if _, err := rand.Read(c.id[:]); err != nil {
		t.Fatal(err)
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c.priv = *priv
	cnonce := make([]byte, 16)
	if _, err := rand.Read(cnonce); err != nil {
		t.Fatal(err)
	}
	c.cnonce = cnonce
	pub := priv.PublicKey().Bytes()
	if clientPubOverride != nil {
		pub = clientPubOverride
	}
	body, err := json.Marshal(map[string]string{
		"id":         hex.EncodeToString(c.id[:]),
		"name":       name,
		"target_url": target,
		"client_pub": base64.RawURLEncoding.EncodeToString(pub),
		"cnonce":     base64.RawURLEncoding.EncodeToString(cnonce),
	})
	if err != nil {
		t.Fatal(err)
	}
	// InsecureSkipVerify: 自签测试证书——SAS 人闸语义下 client 的标准姿态
	// (pair.go 同款);本测试关注的是 SAS 同源性,不是 TLS 锚。
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp, err := client.Post(srv.url+"/pair/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /pair/enroll: %v", err)
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	if code != http.StatusOK {
		return c, code
	}
	var out struct {
		ServerPub string `json:"server_pub"`
		Snonce    string `json:"snonce"`
		Sig       string `json:"sig"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}
	if c.serverPB, err = base64.RawURLEncoding.DecodeString(out.ServerPub); err != nil || len(c.serverPB) != 32 {
		t.Fatalf("server_pub decode: %v (len=%d)", err, len(c.serverPB))
	}
	if c.snonce, err = base64.RawURLEncoding.DecodeString(out.Snonce); err != nil || len(c.snonce) != 16 {
		t.Fatalf("snonce decode: %v (len=%d)", err, len(c.snonce))
	}
	return c, code
}

// clientSideSAS recomputes the SAS exactly the way the client screen prints it
// (pair.go's three-piece line): same frozen transcript composition + the same
// pairing.SAS pure function on the ECDH-shared secret.
func (c *enrollClient) clientSideSAS(t *testing.T, name, target string, clientPub []byte) string {
	t.Helper()
	spk, err := ecdh.X25519().NewPublicKey(c.serverPB)
	if err != nil {
		t.Fatal(err)
	}
	ikm, err := c.priv.ECDH(spk)
	if err != nil {
		t.Fatalf("client ECDH: %v", err)
	}
	transcript := pairing.TranscriptParts(
		[]byte("sshmgr-pair-v2"), c.id[:], []byte(name), []byte(target),
		clientPub, c.cnonce, c.serverPB, c.snonce)
	_, kCreds := pairing.DeriveKeys(ikm, transcript)
	return pairing.SAS(transcript, kCreds)
}

func TestPairEnroll_LandsClientMatchedSASInRow(t *testing.T) {
	srv := newPairingServer(t)
	target := srv.url

	for _, name := range []string{"dev-a", "dev-b"} {
		c, code := enrollAt(t, srv, name, target, nil)
		if code != http.StatusOK {
			t.Fatalf("%s: enroll status = %d, want 200", name, code)
		}
		want := c.clientSideSAS(t, name, target, c.priv.PublicKey().Bytes())
		if len(want) != 6 {
			t.Fatalf("client-side SAS must be 6 digits, got %q", want)
		}
		// 行内 sas == client 屏值(黄金断言:批准面读到的与 client 显示的同源)。
		rows, err := srv.st.ListPendingPairing()
		if err != nil {
			t.Fatal(err)
		}
		var rowSAS string
		found := false
		for i := range rows {
			if bytes.Equal(rows[i].ID, c.id[:]) {
				rowSAS, found = rows[i].SAS, true
				break
			}
		}
		if !found {
			t.Fatalf("%s: row not in the pending queue (state machine raced?)", name)
		}
		if rowSAS != want {
			t.Fatalf("%s: row SAS %q != client-screen SAS %q — the approval surface would show a NON-matching code", name, rowSAS, want)
		}
	}
}

// TestPairEnroll_LowOrderClientPubRejected: the all-zero X25519 public key is
// the classic low-order point — enroll must refuse it with 400 at the ECDH
// step (previously the garbage only surfaced at finish).
func TestPairEnroll_LowOrderClientPubRejected(t *testing.T) {
	srv := newPairingServer(t)
	if _, code := enrollAt(t, srv, "evil-pub", srv.url, make([]byte, 32)); code != http.StatusBadRequest {
		t.Fatalf("low-order client_pub must be refused with 400 at enroll, got %d", code)
	}
}
