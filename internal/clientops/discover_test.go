package clientops

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// Plan 42 批1 T6 — client 侧发现(internal/clientops/discover.go):对每个给定
// 目标地址发一个 probe,统一收集窗内收单播 offer,按 SPKI 去重。生产目标由
// NonLoopbackIPv4s() 枚举;测试注入 127.0.0.1 定向(brief 冻结)。

// startFakeDiscovery binds an ephemeral loopback UDP socket and answers every
// well-formed probe with the given offer bytes — a faithful miniature of the
// serve-side responder (internal/mcpserver/discovery.go). Returns the address
// tests should point Discover at.
func startFakeDiscovery(t *testing.T, offer []byte) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("fake responder bind: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // test over (socket closed)
			}
			if !bytes.HasPrefix(buf[:n], []byte(discoveryMagic)) {
				continue
			}
			var p struct {
				T string `json:"t"`
			}
			if json.Unmarshal(bytes.TrimSpace(buf[len(discoveryMagic):n]), &p) != nil || p.T != "probe" {
				continue
			}
			_, _ = conn.WriteToUDP(append([]byte(discoveryMagic), offer...), src) // 只单播回源,对称带魔数
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

// TestDiscover_LoopbackDirected pins the brief's directed-injection path: one
// 127.0.0.1 target against a responder yields exactly one result whose Addr
// is the offer's source IP.
func TestDiscover_LoopbackDirected(t *testing.T) {
	offer := []byte(`{"t":"offer","name":"nuc10","spki":"sha256:abc","tcp":7878}`)
	raddr := startFakeDiscovery(t, offer)
	got, err := discoverOnPort([]string{"127.0.0.1"}, raddr.Port, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
	if got[0].Name != "nuc10" || got[0].SPKI != "sha256:abc" || got[0].Addr != "127.0.0.1" {
		t.Fatalf("result mismatch: %+v", got[0])
	}
}

// TestDiscover_DedupBySPKI pins the dedup contract: two probes (two targets)
// answered by the same broker (same SPKI) collapse to one entry.
func TestDiscover_DedupBySPKI(t *testing.T) {
	offer := []byte(`{"t":"offer","name":"nuc10","spki":"sha256:same","tcp":7878}`)
	raddr := startFakeDiscovery(t, offer)
	got, err := discoverOnPort([]string{"127.0.0.1", "127.0.0.1"}, raddr.Port, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("two same-SPKI offers must dedup to 1, got %d: %+v", len(got), got)
	}
}

// TestDiscover_GarbageAndDeadTargets pins robustness: a target that answers
// garbage and a target that is not a valid IP are skipped without failing the
// sweep; the offer arriving from a responder is still collected.
func TestDiscover_GarbageAndDeadTargets(t *testing.T) {
	offer := []byte(`{"t":"offer","name":"n","spki":"sha256:ok","tcp":7878}`)
	raddr := startFakeDiscovery(t, offer)
	// "not-an-ip" 被跳过;"127.0.0.1" 探到 fake responder(它只答合法 probe)。
	got, err := discoverOnPort([]string{"not-an-ip", "127.0.0.1"}, raddr.Port, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SPKI != "sha256:ok" {
		t.Fatalf("want exactly the valid offer, got %+v (err=%v)", got, err)
	}
}

// TestNonLoopbackIPv4s pins the enumeration contract: every entry parses as a
// non-loopback IPv4 address. (No non-emptiness assertion — a CI container may
// legitimately have no broadcast-capable interface.)
func TestNonLoopbackIPv4s(t *testing.T) {
	got, err := NonLoopbackIPv4s()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			t.Fatalf("entry %q is not an IPv4 address", a)
		}
		if ip.IsLoopback() {
			t.Fatalf("loopback address %q leaked into NonLoopbackIPv4s", a)
		}
	}
}
