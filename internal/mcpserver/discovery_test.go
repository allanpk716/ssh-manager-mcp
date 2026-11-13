package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Plan 42 批1 T6 — UDP 7878 发现协议(serve 侧应答器)。报文格式冻结(spec
// §3.4 / task-6-brief):probe = 首行 `sshmgr-disc-v1\n` + JSON {"t":"probe"};
// offer = {"t":"offer","name","spki","tcp"} 只单播回源。开关逐包评估,关=不答;
// 魔数/JSON 畸形静默。
//
// 这些测试把发现服务绑定到系统分配的本机回环 UDP 端口。并发测试进程与
// 本机正式运行的 `sshmgr serve` 互不占用端口。

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

func startDiscoveryForTest(
	t *testing.T,
	name string,
	advertisedTCPPort int,
	enabled func() bool,
) *net.UDPAddr {
	t.Helper()
	stop, address, err := StartDiscoveryAt(
		context.Background(),
		"127.0.0.1:0",
		name,
		advertisedTCPPort,
		"sha256:ab",
		enabled,
	)
	if err != nil {
		t.Fatalf("StartDiscoveryAt: %v", err)
	}
	t.Cleanup(stop)
	return address
}

// TestDiscovery_ProbeOfferUnicast pins the frozen happy path (brief Step 1):
// a valid probe gets exactly one unicast offer carrying the serve's name,
// SPKI pin and TCP port.
func TestDiscovery_ProbeOfferUnicast(t *testing.T) {
	advertisedTCPPort := 42424
	address := startDiscoveryForTest(t, "nuc10", advertisedTCPPort, func() bool { return true })
	conn := openProbeConn(t)
	resp := probeOnce(t, conn, []byte(tValidProbe), address, 2*time.Second)
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
	if offer.T != "offer" || offer.Name != "nuc10" || offer.SPKI != "sha256:ab" || offer.TCP != advertisedTCPPort {
		t.Fatalf("offer fields mismatch: %+v", offer)
	}
}

// TestDiscovery_DisabledStillBoundButSilent pins the T7 controller rework: the
// socket is bound REGARDLESS of the switch state (so an off→on flip takes
// effect on the next probe without a serve restart), but enabled()==false
// keeps every probe unanswered — the per-packet gate is the authority.
func TestDiscovery_DisabledStillBoundButSilent(t *testing.T) {
	address := startDiscoveryForTest(t, "n", 42424, func() bool { return false })
	// 绑定即发生:disabled 状态下该系统分配端口已被应答器占住,第二个 bind 必败。
	own, err := net.ListenUDP("udp", address)
	if err == nil {
		own.Close()
		t.Fatal("StartDiscovery must bind its UDP address even when disabled (off→on needs no restart)")
	}
	// 逐包门:关=不答。
	conn := openProbeConn(t)
	if resp := probeOnce(t, conn, []byte(tValidProbe), address, 400*time.Millisecond); resp != nil {
		t.Fatalf("disabled discovery must not answer, got %s", resp)
	}
}

// TestDiscovery_SwitchedOffMidFlight pins the per-packet gate: a responder
// started enabled goes silent the moment enabled() flips false — no restart,
// no lingering offers.
func TestDiscovery_SwitchedOffMidFlight(t *testing.T) {
	var on atomic.Bool
	on.Store(true)
	address := startDiscoveryForTest(t, "n", 42424, on.Load)
	conn := openProbeConn(t)
	addr := address
	if resp := probeOnce(t, conn, []byte(tValidProbe), addr, 2*time.Second); resp == nil {
		t.Fatal("enabled responder must answer")
	}
	on.Store(false) // 关=不答(逐包评估,不重启进程)
	if resp := probeOnce(t, conn, []byte(tValidProbe), addr, 400*time.Millisecond); resp != nil {
		t.Fatalf("per-packet gate: switched-off responder must not answer, got %s", resp)
	}
}

// TestDiscovery_GarbageSilent pins the silent-drop contract: wrong magic,
// missing magic, malformed JSON, wrong type field and empty body all get NO
// answer, while the same read loop still answers a following valid probe
// (silence is selective, not a stalled loop).
func TestDiscovery_GarbageSilent(t *testing.T) {
	address := startDiscoveryForTest(t, "n", 42424, func() bool { return true })
	conn := openProbeConn(t)
	addr := address
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
	stop, _, err := StartDiscoveryAt(ctx, "127.0.0.1:0", "n", 42424, "sha256:ab", func() bool { return true })
	if err != nil {
		t.Fatalf("StartDiscoveryAt: %v", err)
	}
	cancel() // ctx path may invoke stop internally
	stop()
	stop() // must be a no-op, not a double-close panic
}

// TestDiscoveryNameSanitize pins the frozen display-name whitelist
// `^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`: a valid configured name passes
// through, anything else falls back to the hostname, and a unusable hostname
