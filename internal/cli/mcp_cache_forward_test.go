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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// cacheFwdFixture: one testsshd target + one authoritative serve (TLS) + the
// client snapshot materials. The serve store starts with NO anchor — every
// anchor in T5's assertions is produced by the forward under test.
type cacheFwdFixture struct {
	sshdAddr        string
	sshdKey         ssh.PublicKey
	targetID        string
	profID          string
	serve           *store.Store
	dbPath          string
	serveHTTP       *httptest.Server // TLS, /pin-hostkey POST-counted
	posts           *int32
	versionGets     *int32       // GET /snapshot/version 的请求次数
	snapshotGets    *int32       // GET /snapshot 的请求次数
	versionNotFound *atomic.Bool // 打开后,/snapshot/version 返回 404(模拟旧权威端)
	versionMu       *sync.Mutex
	versions        map[string]string   // 请求路径 → 最近一次看到的客户端版本请求头
	cred            clientops.CacheCred // pull/pin credential for the "laptop" device
	projToken       string
	snap            *store.Snapshot // exported BEFORE any test-side anchor preset
	orphanFP        string          // 旧格式孤立锚的指纹（升级迁移审计应列出）
}

// newCacheFwdFixture seeds the whole topology. The POST counter wraps only
// /pin-hostkey, so "zero second forward" is readable as a stable count.
func newCacheFwdFixture(t *testing.T) *cacheFwdFixture {
	t.Helper()
	addr, sshdKey, sshdCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, stdin io.Reader) (string, string, int) {
			// 模拟真实远端 shell：先排空这次会话的标准输入，再返回结果。
			_, _ = io.ReadAll(stdin)
			return "PONG:" + cmd + "\n", "", 0
		},
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
	orphanKey, orphanFP := mustEd25519Pin(t)
	if err := sv.Close(); err != nil {
		t.Fatal(err)
	}
	rawSQLiteStmt(t, dbPath,
		`INSERT INTO host_keys (host_port, key_blob, created_at, pin_format, pin_source, pin_device) VALUES (?,?,?,?,?,?)`,
		"192.0.2.99:22", orphanKey, time.Now().Unix(), store.PinFormatBlob, store.PinSourceTofu, "")
	// 重新打开：旧格式 vault 在这里完成升级迁移（孤立锚丢弃并写审计）。
	sv, err = store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sv.Close() })
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
	var versionGets int32
	var snapshotGets int32
	versionNotFound := &atomic.Bool{}
	versionMu := &sync.Mutex{}
	versions := map[string]string{}
	fx := &cacheFwdFixture{
		versionMu: versionMu, versions: versions,
		posts: &posts, versionGets: &versionGets, snapshotGets: &snapshotGets,
		versionNotFound: versionNotFound,
	}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/pin-hostkey" {
			atomic.AddInt32(&posts, 1)
		}
		if req.URL.Path == "/snapshot/version" {
			atomic.AddInt32(&versionGets, 1)
			if versionNotFound.Load() {
				http.NotFound(w, req)
				return
			}
		}
		if req.URL.Path == "/snapshot" {
			atomic.AddInt32(&snapshotGets, 1)
		}
		versionMu.Lock()
		versions[req.URL.Path] = req.Header.Get("X-Sshmgr-Client-Version")
		versionMu.Unlock()
		r.HTTPHandler().ServeHTTP(w, req)
	})
	serveHTTP := httptest.NewTLSServer(mux)
	t.Cleanup(serveHTTP.Close)
	_, laptopTok, err := sv.AddCacheToken("laptop", profID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := sv.ExportSnapshotForProfile(profID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	fx.sshdAddr = addr
	fx.sshdKey = sshdKey
	fx.targetID = targetID
	fx.profID = profID
	fx.serve = sv
	fx.dbPath = dbPath
	fx.serveHTTP = serveHTTP
	fx.cred = clientops.CacheCred{
		URL:   serveHTTP.URL,
		Token: laptopTok,
		Pin:   mcpserver.SPKIFingerprint(serveHTTP.Certificate()),
	}
	fx.orphanFP = orphanFP
	fx.projToken = projToken
	fx.snap = snap
	return fx
}

func (fx *cacheFwdFixture) host() string { return fx.sshdAddr[:strings.Index(fx.sshdAddr, ":")] }
func (fx *cacheFwdFixture) port() int    { return portFromAddr(fx.sshdAddr) }
func (fx *cacheFwdFixture) postCount() int {
	return int(atomic.LoadInt32(fx.posts))
}

func (fx *cacheFwdFixture) versionCount() int  { return int(atomic.LoadInt32(fx.versionGets)) }
func (fx *cacheFwdFixture) snapshotCount() int { return int(atomic.LoadInt32(fx.snapshotGets)) }

func (fx *cacheFwdFixture) seenClientVersion(path string) string {
	fx.versionMu.Lock()
	defer fx.versionMu.Unlock()
	return fx.versions[path]
}
func (fx *cacheFwdFixture) fingerprint() string { return ssh.FingerprintSHA256(fx.sshdKey) }

// mustEd25519Pin 生成一把新的 ed25519 主机密钥，返回序列化字节与指纹。
func mustEd25519Pin(t *testing.T) (marshaled []byte, fp string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pub.Marshal(), ssh.FingerprintSHA256(pub)
}

// newBrokerSnap assembles the cache broker exactly as mcp --cache does (the
// binder from the fixture credential, or a per-test override credential) and
// attaches an MCP client.
func (fx *cacheFwdFixture) newBrokerSnap(t *testing.T, snap *store.Snapshot, reload func() (*store.Snapshot, bool, error), cred clientops.CacheCred) *mcp.ClientSession {
	t.Helper()
	srv, tunnels, tasks, cleanup, err := mcpserver.NewCacheBroker(
		fx.projToken, snap, filepath.Join(t.TempDir(), "audit.log"), reload,
		clientops.ForwardingHostKeys(mustForwarder(t, cred)), "laptop", nil)
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

func callToolTexts(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) []string {
	t.Helper()
	texts, _, _ := callToolResultTexts(t, sess, name, args)
	return texts
}

func callToolResultTexts(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) ([]string, bool, any) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	return texts, res.IsError, res.StructuredContent
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

// presetServeAnchor inserts an anchor directly onto the serve-side entry —
// the owner pre-anchoring shape (blob or fingerprint format).
func (fx *cacheFwdFixture) presetServeAnchor(t *testing.T, blob []byte, format, source string) {
	t.Helper()
	rawSQLiteStmt(t, fx.dbPath,
		`INSERT INTO entry_host_keys (server_id, key_blob, pin_format, pin_source, pin_device, handshake_addr, created_at) VALUES (?,?,?,?,?,?,?)`,
		fx.targetID, blob, format, source, "", fmt.Sprintf("%s:%d", fx.host(), fx.port()), time.Now().Unix())
}

// assertServeAnchor checks the authoritative landing: anchor present, dual-mode
// matching the presented key, source=forward, device=laptop (the §6 detection
// surface reads the same columns).
func (fx *cacheFwdFixture) assertServeAnchor(t *testing.T) {
	t.Helper()
	serveSrv, err := fx.serve.GetServer(fx.targetID)
	if err != nil || serveSrv == nil {
		t.Fatalf("取条目失败：%v %v", serveSrv, err)
	}
	pin, err := fx.serve.GetEntryPin(serveSrv)
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
	if err := db.QueryRow(`SELECT pin_source, pin_device FROM entry_host_keys WHERE server_id=?`,
		fx.targetID).Scan(&source, &device); err != nil {
		t.Fatal(err)
	}
	if source != "forward" || device != "laptop" {
		t.Fatalf("serve anchor metadata = %q/%q, want forward/laptop", source, device)
	}
}

// assertServeAuditPinForward: 权威端审计行带条目、握手地址、指纹、转发设备
// 与来源（票 #58 用户故事 20、27）。
func (fx *cacheFwdFixture) assertServeAuditPinForward(t *testing.T) {
	t.Helper()
	rows, err := fx.serve.AuditRows(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Action == "pin-forward" {
			for _, want := range []string{"entry=target(", "handshake=" + fx.host() + ":" + strconv.Itoa(fx.port()), "fp=" + fx.fingerprint(), "device=laptop", "via=forward"} {
				if !strings.Contains(r.Command, want) {
					t.Fatalf("pin-forward audit row missing %q: %q", want, r.Command)
				}
			}
			if r.ServerID != fx.targetID {
				t.Fatalf("pin-forward audit row attributed to %q, want the entry %q", r.ServerID, fx.targetID)
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
		clientops.ForwardingHostKeys(mustForwarder(t, fx.cred)), "laptop", nil)
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

// TestCacheForwardE2E_MultiAddressFirstTrustUsesVirtualLan：客户端缓存模式
// 下，多地址条目首次锚定经真实虚拟局域网地址握手；转发请求、权威端锚来源
// 与审计都记录这个地址，而不是条目的兜底地址。
func TestCacheForwardE2E_MultiAddressFirstTrustUsesVirtualLan(t *testing.T) {
	fx := newCacheFwdFixture(t)
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "absent-store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := fx.serve.AddNetwork(models.Network{Name: "虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.AddDevice("目标工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.AddDevice("笔记本", "", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.MoveServerToDevice(fx.targetID, "目标工作站"); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, address string
	}{
		{"目标工作站", fx.host()},
		{"笔记本", "127.0.0.2"},
	} {
		if _, err := fx.serve.AddNetworkMembership(membership.device, "虚拟局域网", membership.address); err != nil {
			t.Fatal(err)
		}
	}
	serveSrv, err := fx.serve.GetServer(fx.targetID)
	if err != nil {
		t.Fatal(err)
	}
	serveSrv.Host = "203.0.113.20"
	if err := fx.serve.UpdateServer(serveSrv); err != nil {
		t.Fatal(err)
	}
	devices, err := fx.serve.ListDevices(store.DeviceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var laptopTokenID string
	for _, device := range devices {
		if device.Name == "笔记本" {
			laptopTokenID = device.ClientTokenID
		}
	}
	if laptopTokenID == "" {
		t.Fatal("client device is not associated with the cache token")
	}
	snapshot, err := fx.serve.ExportSnapshotForProfile(fx.profID, false, laptopTokenID)
	if err != nil {
		t.Fatal(err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	broker, tunnels, tasks, cleanup, err := mcpserver.NewCacheBroker(
		fx.projToken, snapshot, auditPath, nil,
		clientops.ForwardingHostKeys(mustForwarder(t, fx.cred)), "laptop", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	sess := attachMCPClient(t, broker)

	text, isErr := callExec(t, sess, fx.targetID, "hi")
	if isErr {
		t.Fatalf("multi-address first trust must succeed over the virtual LAN: %s", text)
	}
	if !strings.Contains(text, "PONG:hi") {
		t.Fatalf("exec output missing: %q", text)
	}
	if fx.postCount() != 1 {
		t.Fatalf("posts = %d, want exactly 1", fx.postCount())
	}
	realAddr := fmt.Sprintf("%s:%d", fx.host(), fx.port())
	pins, err := fx.serve.ListEntryPins()
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].HandshakeAddr != realAddr || !pins[0].Matches(fx.sshdKey.Marshal()) {
		t.Fatalf("权威端锚没有记录真实虚拟局域网握手地址：%+v", pins)
	}
	rows, err := fx.serve.QueryAudit(store.AuditFilter{Actions: []string{"pin-forward"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Command, "handshake="+realAddr) {
		t.Fatalf("权威端锚定转发审计没有记录真实握手地址：%+v", rows)
	}
}

// TestCacheForwardE2E_OldVaultUpgradedBeforeForward：权威端用旧格式 vault
// 打开并完成升级——没有任何条目指向的旧锚被丢弃，迁移审计列出地址与指纹；
// 目标条目仍然无锚，快照正常下发（票 #58 测试决策的主接缝场景前半段）。
func TestCacheForwardE2E_OldVaultUpgradedBeforeForward(t *testing.T) {
	fx := newCacheFwdFixture(t)

	migrations, err := fx.serve.QueryAudit(store.AuditFilter{Actions: []string{"pin-migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Fatalf("升级应写一条迁移审计，实际 %d 条", len(migrations))
	}
	if cmd := migrations[0].Command; !strings.Contains(cmd, "192.0.2.99:22") || !strings.Contains(cmd, fx.orphanFP) {
		t.Fatalf("迁移审计应列出被丢弃的地址与指纹：%q", cmd)
	}
	if pins, err := fx.serve.ListEntryPins(); err != nil || len(pins) != 0 {
		t.Fatalf("升级后目标条目应无锚（孤立锚已丢弃）：%v %v", pins, err)
	}
	var legacy int
	db, err := sql.Open("sqlite", fx.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COUNT(*) FROM host_keys`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatalf("升级后旧锚表应清空，仍有 %d 行", legacy)
	}
}

// TestCacheForwardE2E_SecondEntrySameAddressIndependent：同一兜底地址上的第
// 二个条目独立首次锚定、独立校验，一个条目的锚不影响另一个；随后的重复连
// 接不再转发。
func TestCacheForwardE2E_SecondEntrySameAddressIndependent(t *testing.T) {
	fx := newCacheFwdFixture(t)

	// 权威端补一个同地址条目并授权，再重新导出快照（客户端旧快照不认识它）。
	cid, err := fx.serve.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := fx.serve.AddServer(&models.Server{
		Name: "target-2", Host: fx.host(), Port: fx.port(), User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.GrantServers(fx.profID, []string{secondID}); err != nil {
		t.Fatal(err)
	}
	snap2, err := fx.serve.ExportSnapshotForProfile(fx.profID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	sess := fx.newBrokerSnap(t, snap2, nil, fx.cred)

	if text, isErr := callExec(t, sess, fx.targetID, "one"); isErr {
		t.Fatalf("第一个条目应经转发完成首次连接：%s", text)
	}
	if fx.postCount() != 1 {
		t.Fatalf("第一个条目转发后 posts = %d，应为 1", fx.postCount())
	}
	if text, isErr := callExec(t, sess, secondID, "two"); isErr {
		t.Fatalf("同地址第二个条目应独立完成首次锚定：%s", text)
	}
	if fx.postCount() != 2 {
		t.Fatalf("第二个条目转发后 posts = %d，应为 2", fx.postCount())
	}

	pins, err := fx.serve.ListEntryPins()
	if err != nil || len(pins) != 2 {
		t.Fatalf("两个条目应各有一把锚：%v %v", pins, err)
	}
	seen := map[string]bool{}
	for _, p := range pins {
		seen[p.ServerID] = true
		if !p.Matches(fx.sshdKey.Marshal()) || p.Source != store.PinSourceForward {
			t.Fatalf("条目锚不正确：%+v", p)
		}
	}
	if !seen[fx.targetID] || !seen[secondID] {
		t.Fatalf("两个条目应各自落锚：%v", seen)
	}

	// 各自的后续连接照常校验，不再转发。
	for i, id := range []string{fx.targetID, secondID} {
		if text, isErr := callExec(t, sess, id, "again"); isErr {
			t.Fatalf("条目 %d 的后续连接应凭已有锚通过：%s", i, text)
		}
	}
	if fx.postCount() != 2 {
		t.Fatalf("后续连接不应再转发：posts = %d", fx.postCount())
	}
}

// TestCacheVersionE2E_SnapshotCarriesRecommendedVersion: 权威端二进制版本随
// 快照下发,信封版本号仍为 1;真实 cache pull 把它原样写进客户端加密缓存;
// 旧版快照结构解码新快照时忽略新增字段。
func TestCacheVersionE2E_SnapshotCarriesRecommendedVersion(t *testing.T) {
	savedVersion := buildinfo.Version
	buildinfo.Version = "1.1.0"
	t.Cleanup(func() { buildinfo.Version = savedVersion })

	fx := newCacheFwdFixture(t)
	if fx.snap.Version != 1 {
		t.Fatalf("快写信封版本 = %d，应为 1", fx.snap.Version)
	}
	if fx.snap.RecommendedVersion != "1.1.0" {
		t.Fatalf("推荐版本 = %q，应为 1.1.0", fx.snap.RecommendedVersion)
	}

	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	mem := &store.MemKeyProvider{}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	prevDek := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prevDek })

	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{StatusOut: io.Discard}); err != nil {
		t.Fatalf("pull snapshot: %v", err)
	}
	loaded, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatalf("load cached snapshot: %v", err)
	}
	if loaded.RecommendedVersion != "1.1.0" {
		t.Fatalf("缓存中的推荐版本 = %q，应为 1.1.0", loaded.RecommendedVersion)
	}

	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	// 旧版顶层快照结构不包含推荐版本字段；嵌套类型仍引用现有类型，
	// 重点是用旧版顶层视角确认新增字段不会破坏已有字段导入。
	var legacy struct {
		Version                int                        `json:"version"`
		Credentials            []store.SnapshotCredential `json:"credentials"`
		Servers                []store.SnapshotServer     `json:"servers"`
		Profiles               []store.SnapshotProfile    `json:"profiles"`
		Grants                 []store.SnapshotGrant      `json:"grants"`
		Projects               []store.SnapshotProject    `json:"projects"`
		HostKeys               []store.SnapshotHostKey    `json:"host_keys"`
		Audit                  []store.SnapshotAudit      `json:"audit"`
		PolicyRules            []store.SnapshotPolicyRule `json:"policy_rules"`
		PolicyDisabledDefaults []string                   `json:"policy_disabled_defaults"`
	}
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatalf("旧版快照解码失败：%v", err)
	}
	if legacy.Version != loaded.Version {
		t.Fatalf("旧版快照信封版本 = %d，应为 %d", legacy.Version, loaded.Version)
	}
	for name, compare := range map[string]func() bool{
		"凭据":   func() bool { return reflect.DeepEqual(legacy.Credentials, loaded.Credentials) },
		"服务器":  func() bool { return reflect.DeepEqual(legacy.Servers, loaded.Servers) },
		"权限配置": func() bool { return reflect.DeepEqual(legacy.Profiles, loaded.Profiles) },
		"授权关系": func() bool { return reflect.DeepEqual(legacy.Grants, loaded.Grants) },
		"项目":   func() bool { return reflect.DeepEqual(legacy.Projects, loaded.Projects) },
		"主机密钥": func() bool { return reflect.DeepEqual(legacy.HostKeys, loaded.HostKeys) },
		"审计记录": func() bool { return reflect.DeepEqual(legacy.Audit, loaded.Audit) },
		"命令策略": func() bool {
			return reflect.DeepEqual(legacy.PolicyRules, loaded.PolicyRules) &&
				reflect.DeepEqual(legacy.PolicyDisabledDefaults, loaded.PolicyDisabledDefaults)
		},
	} {
		if !compare() {
			t.Errorf("旧版快照结构解码后，%s内容应与新结构一致", name)
		}
	}
}

// TestCacheVersionE2E_ToolHintComparesVersions: MCP 工具面按现有版本比较规则
// 生成会话提示;相等与无法解析不提示,成功结果的结构化输出保持原样。
func TestCacheVersionE2E_ToolHintComparesVersions(t *testing.T) {
	fx := newCacheFwdFixture(t)
	cases := []struct {
		name       string
		client     string
		recommend  string
		wantHint   string
		wantNoHint bool
	}{
		{
			name:      "落后",
			client:    "1.0.0",
			recommend: "1.1.0",
			wantHint:  "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级",
		},
		{
			name:      "较新",
			client:    "1.2.0",
			recommend: "1.1.0",
			wantHint:  "客户端 1.2.0 比权威端 1.1.0 新，建议先升级权威端",
		},
		{
			name:       "相等",
			client:     "1.1.0",
			recommend:  "1.1.0",
			wantNoHint: true,
		},
		{
			name:       "开发版本",
			client:     "dev",
			recommend:  "1.1.0",
			wantNoHint: true,
		},
		{
			name:       "推荐版本为空",
			client:     "1.1.0",
			recommend:  "",
			wantNoHint: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSHMGR_TEST_VERSION", tc.client)
			fx.snap.RecommendedVersion = tc.recommend
			sess := fx.newBroker(t, nil, fx.cred)

			res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "list_servers",
				Arguments: map[string]any{},
			})
			if err != nil {
				t.Fatal(err)
			}
			var firstText string
			for i, content := range res.Content {
				text, ok := content.(*mcp.TextContent)
				if !ok {
					continue
				}
				if i == 0 || firstText == "" {
					firstText = text.Text
				}
				if tc.wantNoHint && strings.Contains(text.Text, "权威端") {
					t.Fatalf("不应出现版本提示：%q", text.Text)
				}
			}
			if tc.wantNoHint {
				if !strings.HasPrefix(firstText, `{"servers":`) {
					t.Fatalf("原工具结果应仍在最前，实际为 %q", firstText)
				}
			} else if firstText != tc.wantHint {
				t.Fatalf("第一段文本 = %q，应为提示 %q", firstText, tc.wantHint)
			}
			structured, ok := res.StructuredContent.(map[string]any)
			if !ok || len(structured["servers"].([]any)) != 1 {
				t.Fatalf("结构化输出被改动：%#v", res.StructuredContent)
			}
		})
	}
}

// TestCacheVersionE2E_ToolHintSessionLifecycle: 同一提示内容只出现一次,版本
// 对变化后再次出现;工具失败结果同样在最前附带提示。
func TestCacheVersionE2E_ToolHintSessionLifecycle(t *testing.T) {
	fx := newCacheFwdFixture(t)
	t.Setenv("SSHMGR_TEST_VERSION", "1.0.0")
	fx.snap.RecommendedVersion = "1.1.0"
	var next *store.Snapshot
	reload := func() (*store.Snapshot, bool, error) {
		if next == nil {
			return nil, false, nil
		}
		changed := next
		next = nil
		return changed, true, nil
	}
	sess := fx.newBroker(t, reload, fx.cred)
	hint := "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级"

	first := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(first) != 2 || first[0] != hint || !strings.HasPrefix(first[1], `{"servers":`) {
		t.Fatalf("第一次工具结果内容 = %#v", first)
	}
	second := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(second) != 1 || !strings.HasPrefix(second[0], `{"servers":`) {
		t.Fatalf("同一提示只应出现一次，实际为 %#v", second)
	}

	// 保留服务器集合与修订号:这张快照只改推荐版本,不应顺带触发配置
	// 变更提示。
	next = &store.Snapshot{
		Version: 1, SnapshotRevision: fx.snap.SnapshotRevision, RecommendedVersion: "1.2.0",
		Credentials: fx.snap.Credentials, Servers: fx.snap.Servers,
		Profiles: fx.snap.Profiles, Grants: fx.snap.Grants, Projects: fx.snap.Projects,
	}
	third := callToolTexts(t, sess, "list_servers", map[string]any{})
	wantChanged := "客户端 1.0.0 落后于权威端 1.2.0，运行 `sshmgr update` 升级"
	if len(third) != 2 || third[0] != wantChanged || !strings.HasPrefix(third[1], `{"servers":`) {
		t.Fatalf("版本对变化后应再次提示，实际为 %#v", third)
	}

	failureSession := fx.newBroker(t, nil, fx.cred)
	failure, failureIsError, failureStructured := callToolResultTexts(t, failureSession, "exec_command", map[string]any{
		"server_id": "missing", "command": "true",
	})
	if !failureIsError {
		t.Fatal("越权 exec_command 应保持为失败的工具结果")
	}
	if len(failure) != 2 || failure[0] != hint || !strings.Contains(failure[1], "server is not in your profile") {
		t.Fatalf("失败的工具结果应先携带提示，实际为 %#v", failure)
	}
	failureFields, ok := failureStructured.(map[string]any)
	if !ok || len(failureFields) != 5 {
		t.Fatalf("失败工具结果的结构化输出被改动：%#v", failureStructured)
	}
	if _, exists := failureFields["stdout"]; !exists {
		t.Fatalf("失败工具结果丢失了结构化输出：%#v", failureStructured)
	}
}

// TestCacheVersionE2E_NewerClientHintChangesWithVersionPair：客户端较新的
// 提示也必须携带两边版本；版本对变化后，同一会话会再次提示。
func TestCacheVersionE2E_NewerClientHintChangesWithVersionPair(t *testing.T) {
	fx := newCacheFwdFixture(t)
	t.Setenv("SSHMGR_TEST_VERSION", "1.2.0")
	fx.snap.RecommendedVersion = "1.1.0"
	var next *store.Snapshot
	reload := func() (*store.Snapshot, bool, error) {
		if next == nil {
			return nil, false, nil
		}
		changed := next
		next = nil
		return changed, true, nil
	}
	sess := fx.newBroker(t, reload, fx.cred)

	first := callToolTexts(t, sess, "list_servers", map[string]any{})
	wantFirst := "客户端 1.2.0 比权威端 1.1.0 新，建议先升级权威端"
	if len(first) != 2 || first[0] != wantFirst || !strings.HasPrefix(first[1], `{"servers":`) {
		t.Fatalf("第一次客户端较新提示 = %#v，应为 %q", first, wantFirst)
	}

	second := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(second) != 1 || !strings.HasPrefix(second[0], `{"servers":`) {
		t.Fatalf("同样的客户端较新提示只应出现一次，实际为 %#v", second)
	}

	next = &store.Snapshot{
		Version: 1, SnapshotRevision: fx.snap.SnapshotRevision, RecommendedVersion: "1.0.0",
		Credentials: fx.snap.Credentials, Servers: fx.snap.Servers,
		Profiles: fx.snap.Profiles, Grants: fx.snap.Grants, Projects: fx.snap.Projects,
	}
	third := callToolTexts(t, sess, "list_servers", map[string]any{})
	wantThird := "客户端 1.2.0 比权威端 1.0.0 新，建议先升级权威端"
	if len(third) != 2 || third[0] != wantThird || !strings.HasPrefix(third[1], `{"servers":`) {
		t.Fatalf("版本对变化后应再次携带客户端较新提示，实际为 %#v", third)
	}
}

// TestCacheVersionE2E_StandaloneDoesNotHint: 单机部署没有推荐版本通道,即使
// 本进程版本可比较也不在 MCP 工具结果前插入提示。
func TestCacheVersionE2E_StandaloneDoesNotHint(t *testing.T) {
	fx := newCacheFwdFixture(t)
	t.Setenv("SSHMGR_TEST_VERSION", "1.0.0")
	project, err := fx.serve.VerifyToken(fx.projToken)
	if err != nil || project == nil {
		t.Fatalf("verify project token: %v, %#v", err, project)
	}
	srv, _, _, err := mcpserver.NewServer(fx.serve, project.ProfileID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess := attachMCPClient(t, srv)
	texts := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(texts) != 1 || !strings.HasPrefix(texts[0], `{"servers":`) {
		t.Fatalf("单机工具结果不应有提示，实际为 %#v", texts)
	}
}

// TestCacheVersionE2E_MultipleHintsKeepSourceOrder: 会话提示通道支持多条
// 提示,按注册顺序插在原工具结果前;后续调用全部去重。
func TestCacheVersionE2E_MultipleHintsKeepSourceOrder(t *testing.T) {
	fx := newCacheFwdFixture(t)
	project, err := fx.serve.VerifyToken(fx.projToken)
	if err != nil || project == nil {
		t.Fatalf("verify project token: %v, %#v", err, project)
	}
	srv, _, _, err := mcpserver.NewServerFromSourceWithHints(
		func() *store.Store { return fx.serve },
		project.ProfileID, project.ID, nil, nil,
		nil,
		nil,
		func() []string { return []string{"变更提示"} },
		func() []string { return []string{"版本提示"} },
	)
	if err != nil {
		t.Fatal(err)
	}
	sess := attachMCPClient(t, srv)
	first := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(first) != 3 || first[0] != "变更提示" || first[1] != "版本提示" || !strings.HasPrefix(first[2], `{"servers":`) {
		t.Fatalf("第一次工具结果内容 = %#v", first)
	}
	second := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(second) != 1 || !strings.HasPrefix(second[0], `{"servers":`) {
		t.Fatalf("两条提示都应去重，实际为 %#v", second)
	}
}

// TestCacheForwardEndToEnd_ClientVersionHeaderAndDeviceCodeRecord：新客户端的
// 拉快照、锚定转发、元数据编辑请求都声明版本；权威端只在拉快照时覆盖
// 记录，未声明会变回空值，超长值按规格截断并去掉不可打印字符。
func TestCacheForwardEndToEnd_ClientVersionHeaderAndDeviceCodeRecord(t *testing.T) {
	fx := newCacheFwdFixture(t)
	previousVersion := buildinfo.Version
	buildinfo.Version = "dev"
	t.Cleanup(func() { buildinfo.Version = previousVersion })

	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	memoryKeyProvider := &store.MemKeyProvider{}
	if err := memoryKeyProvider.Set(dek); err != nil {
		t.Fatal(err)
	}
	previousKeyProvider := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return memoryKeyProvider }
	t.Cleanup(func() { clientops.DekProvider = previousKeyProvider })

	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{StatusOut: io.Discard}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got := fx.seenClientVersion("/snapshot"); got != "dev" {
		t.Fatalf("snapshot client version header = %q, want %q", got, "dev")
	}
	if got := fx.deviceCodeClientVersion(t); got != "dev" {
		t.Fatalf("device code client version after pull = %q, want %q", got, "dev")
	}

	brokerSession := fx.newBroker(t, nil, fx.cred)
	text, isError := callExec(t, brokerSession, fx.targetID, "hi")
	if isError {
		t.Fatalf("exec through anchored forwarding must succeed: %s", text)
	}
	if got := fx.seenClientVersion("/pin-hostkey"); got != "dev" {
		t.Fatalf("pin-hostkey client version header = %q, want %q", got, "dev")
	}

	metadataForwarder, err := clientops.NewMetadataForwarder(fx.cred)
	if err != nil {
		t.Fatal(err)
	}
	defer metadataForwarder.Close()
	metadataServer, tunnels, tasks, cleanup, err := mcpserver.NewCacheBroker(
		fx.projToken, fx.snap, filepath.Join(t.TempDir(), "audit.log"), nil,
		clientops.ForwardingHostKeys(mustForwarder(t, fx.cred)), "laptop", metadataForwarder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	metadataSession := attachMCPClient(t, metadataServer)
	role := "edited by the client"
	text, isError = callTool(t, metadataSession, "update_server_metadata", map[string]any{
		"server_id":         fx.targetID,
		"expected_revision": 0,
		"role":              role,
	})
	if isError {
		t.Fatalf("metadata edit must succeed: %s", text)
	}
	if got := fx.seenClientVersion("/server-metadata"); got != "dev" {
		t.Fatalf("server-metadata client version header = %q, want %q", got, "dev")
	}

	rawSnapshotPull := func(clientVersion string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, fx.serveHTTP.URL+"/snapshot", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+fx.cred.Token)
		if clientVersion != "" {
			req.Header.Set("X-Sshmgr-Client-Version", clientVersion)
		}
		res, err := fx.serveHTTP.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}

	if status := rawSnapshotPull(""); status != http.StatusOK {
		t.Fatalf("snapshot pull without client version returned %d, want 200", status)
	}
	if got := fx.deviceCodeClientVersion(t); got != "" {
		t.Fatalf("snapshot pull without client version must clear the stored version, got %q", got)
	}

	declaredVersion := "A" + string(rune(0x200b)) + strings.Repeat("A", 69) + " tail"
	if status := rawSnapshotPull(declaredVersion); status != http.StatusOK {
		t.Fatalf("snapshot pull with a long client version returned %d, want 200", status)
	}
	if got := fx.deviceCodeClientVersion(t); got != strings.Repeat("A", 64) {
		t.Fatalf("stored client version = %q, want the first 64 printable characters", got)
	}
}

func (fx *cacheFwdFixture) deviceCodeClientVersion(t *testing.T) string {
	t.Helper()
	tokens, err := fx.serve.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range tokens {
		if token.Name == "laptop" {
			return token.ClientVersion
		}
	}
	t.Fatal("laptop device code not found")
	return ""
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
	snap2, err := fx.serve.ExportSnapshotForProfile(fx.profID, false, "")
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
		want2 := "全部候选地址连接失败：\n兜底地址：主机密钥不同，已跳过"
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
