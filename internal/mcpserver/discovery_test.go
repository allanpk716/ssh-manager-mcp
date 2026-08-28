package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// Plan 42 批1 T6 — UDP 7878 发现协议(serve 侧应答器)。报文格式冻结(spec
// §3.4 / task-6-brief):probe = 首行 `sshmgr-disc-v1\n` + JSON {"t":"probe"};
// offer = {"t":"offer","name","spki","tcp"} 只单播回源。开关逐包评估,关=不答;
// 魔数/JSON 畸形静默。

// mustResolve is the brief's helper: resolve a UDP address or fail the test.
func mustResolve(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	ra, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	return ra
}

// openProbeConn opens a client-side UDP packet conn on loopback for probing.
func openProbeConn(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// probeOnce sends one payload and returns the first response bytes; nil means
// "silence within window" (no answer).
func probeOnce(t *testing.T, conn net.PacketConn, payload []byte, addr *net.UDPAddr, window time.Duration) []byte {
	t.Helper()
	if _, err := conn.WriteTo(payload, addr); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	buf := make([]byte, 512)
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		return nil // timeout = silence
	}
	return buf[:n]
}

const tValidProbe = "sshmgr-disc-v1\n{\"t\":\"probe\"}\n"

// TestDiscovery_ProbeOfferUnicast pins the frozen happy path (brief Step 1):
// a valid probe gets exactly one unicast offer carrying the serve's name,
// SPKI pin and TCP port.
func TestDiscovery_ProbeOfferUnicast(t *testing.T) {
	stop := StartDiscovery(context.Background(), "nuc10", 7878, "sha256:ab", func() bool { return true })
	defer stop()
	conn := openProbeConn(t)
	resp := probeOnce(t, conn, []byte(tValidProbe), mustResolve(t, "127.0.0.1:7878"), 2*time.Second)
	if resp == nil {
		t.Fatal("no offer within 2s for a valid probe")
	}
	// 对称报文:offer 也以魔数首行开头(与 probe 同构)。
	if !bytes.HasPrefix(resp, []byte("sshmgr-disc-v1\n")) {
		t.Fatalf("offer must carry the frozen magic prefix, got %s", resp)
	}
	if !bytes.Contains(resp, []byte(`"spki":"sha256:ab"`)) {
		t.Fatalf("offer=%s", resp)
	}
	var offer struct {
		T    string `json:"t"`
		Name string `json:"name"`
		SPKI string `json:"spki"`
		TCP  int    `json:"tcp"`
	}
	body, _ := bytes.CutPrefix(resp, []byte("sshmgr-disc-v1\n"))
	if err := json.Unmarshal(bytes.TrimSpace(body), &offer); err != nil {
		t.Fatalf("offer is not the frozen JSON shape: %v (%s)", err, resp)
	}
	if offer.T != "offer" || offer.Name != "nuc10" || offer.SPKI != "sha256:ab" || offer.TCP != 7878 {
		t.Fatalf("offer fields mismatch: %+v", offer)
	}
}

// TestDiscovery_DisabledSilent pins the controller ruling: with enabled()
// false at start the responder binds NO socket at all (0.0.0.0:7878 must still
// be free for the test to take), and a probe gets no answer.
func TestDiscovery_DisabledSilent(t *testing.T) {
	stop := StartDiscovery(context.Background(), "n", 7878, "sha256:ab", func() bool { return false })
	defer stop()
	own, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 7878})
	if err != nil {
		t.Fatalf("disabled StartDiscovery must not bind udp/7878: %v", err)
	}
	own.Close()
	conn := openProbeConn(t)
	if resp := probeOnce(t, conn, []byte(tValidProbe), mustResolve(t, "127.0.0.1:7878"), 400*time.Millisecond); resp != nil {
		t.Fatalf("disabled discovery must not answer, got %s", resp)
	}
}

// TestDiscovery_SwitchedOffMidFlight pins the per-packet gate: a responder
// started enabled goes silent the moment enabled() flips false — no restart,
// no lingering offers.
func TestDiscovery_SwitchedOffMidFlight(t *testing.T) {
	on := true
	stop := StartDiscovery(context.Background(), "n", 7878, "sha256:ab", func() bool { return on })
	defer stop()
	conn := openProbeConn(t)
	addr := mustResolve(t, "127.0.0.1:7878")
	if resp := probeOnce(t, conn, []byte(tValidProbe), addr, 2*time.Second); resp == nil {
		t.Fatal("enabled responder must answer")
	}
	on = false // 关=不答(逐包评估,不重启进程)
	if resp := probeOnce(t, conn, []byte(tValidProbe), addr, 400*time.Millisecond); resp != nil {
		t.Fatalf("per-packet gate: switched-off responder must not answer, got %s", resp)
	}
}

// TestDiscovery_GarbageSilent pins the silent-drop contract: wrong magic,
// missing magic, malformed JSON, wrong type field and empty body all get NO
// answer, while the same read loop still answers a following valid probe
// (silence is selective, not a stalled loop).
func TestDiscovery_GarbageSilent(t *testing.T) {
	stop := StartDiscovery(context.Background(), "n", 7878, "sha256:ab", func() bool { return true })
	defer stop()
	conn := openProbeConn(t)
	addr := mustResolve(t, "127.0.0.1:7878")
	garbage := map[string]string{
		"wrong magic": "other-disc-v1\n{\"t\":\"probe\"}\n",
		"no magic":    "{\"t\":\"probe\"}\n",
		"bad json":    "sshmgr-disc-v1\n{not json\n",
		"wrong type":  "sshmgr-disc-v1\n{\"t\":\"other\"}\n",
		"empty body":  "sshmgr-disc-v1\n   \n",
		"json array":  "sshmgr-disc-v1\n[1,2]\n",
	}
	for name, payload := range garbage {
		if resp := probeOnce(t, conn, []byte(payload), addr, 300*time.Millisecond); resp != nil {
			t.Fatalf("%s: must be dropped silently, got %s", name, resp)
		}
	}
	if resp := probeOnce(t, conn, []byte(tValidProbe), addr, 2*time.Second); resp == nil {
		t.Fatal("valid probe after garbage must still be answered (read loop alive)")
	}
}

// TestDiscovery_StopIdempotent pins the stop() contract: calling it twice (and
// after ctx cancel) must not panic — RunServe defers it AND the ctx hookup may
// fire it.
func TestDiscovery_StopIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := StartDiscovery(ctx, "n", 7878, "sha256:ab", func() bool { return true })
	cancel() // ctx path may invoke stop internally
	stop()
	stop() // must be a no-op, not a double-close panic
}

// TestDiscoveryNameSanitize pins the frozen display-name whitelist
// `^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`: a valid configured name passes
// through, anything else falls back to the hostname, and a unusable hostname
// is force-derived (then the constant fallback). Pure — hostname injected.
func TestDiscoveryNameSanitize(t *testing.T) {
	cases := []struct {
		name, host, want string
	}{
		{"nuc10", "other-host", "nuc10"},       // 合法名直通
		{"my nuc.10_x-y", "", "my nuc.10_x-y"}, // 全白名单字符
		{"中文节点", "", "中文节点"},                   // \p{L} 覆盖 Unicode
		{"bad!name", "MY-NUC", "MY-NUC"},       // 非法名 → hostname 兜底
		{"", "host-1", "host-1"},               // 空名 → hostname
		{"!", "192.168.1.10", "192.168.1.10"},  // 数字开头合法
		{"", "", "sshmgr"},                     // 全空 → 常量兜底
		{"!!!", "###", "sshmgr"},               // hostname 也全非法
		{"!!!", "-_-", "sshmgr"},               // 派生后无字母数字 → 兜底
		{"!!!", "--my.nuc--", "my.nuc--"},      // 派生:去掉前导非字母数字
		{"!", "01234567890123456789012345678901234", "01234567890123456789012345678901"}, // 派生截到 32
		{"01234567890123456789012345678901234", "h", "h"},                                // 33 字超长 → hostname
	}
	for _, c := range cases {
		if got := sanitizeDiscoveryName(c.name, c.host); got != c.want {
			t.Errorf("sanitizeDiscoveryName(%q, %q) = %q, want %q", c.name, c.host, got, c.want)
		}
	}
}
