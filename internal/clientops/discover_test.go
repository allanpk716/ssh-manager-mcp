package clientops

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
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
// is the offer's source IP — received on the UNCONNECTED client socket via
// ReadFromUDP (T7 rework: the client never DialUDP'd to the responder, so the
// source address is the transport's honest answer, not an echo of the dial).
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
	if got[0].TCPPort != 7878 {
		t.Fatalf("TCPPort = %d, want the offer's tcp field 7878", got[0].TCPPort)
	}
}

// TestDiscover_TCPPortPassthrough pins the T7 struct extension: the offer's
// frozen `tcp` field surfaces in Discovered.TCPPort (non-default values ride
// through untouched), and the offer's source address is the responder's real
// address (host only, never host:port).
func TestDiscover_TCPPortPassthrough(t *testing.T) {
	offer := []byte(`{"t":"offer","name":"odd-port","spki":"sha256:odd","tcp":9999}`)
	raddr := startFakeDiscovery(t, offer)
	got, err := discoverOnPort([]string{"127.0.0.1"}, raddr.Port, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
	if got[0].TCPPort != 9999 {
		t.Fatalf("TCPPort = %d, want 9999 from the offer", got[0].TCPPort)
	}
	if got[0].Addr != "127.0.0.1" || strings.Contains(got[0].Addr, ":") {
		t.Fatalf("Addr = %q, want the bare source host", got[0].Addr)
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

// TestNonLoopbackIPv4Broadcasts pins the T7 production target set: one entry
// per IPv4 broadcast-capable non-loopback interface address, each parseable,
// IPv4, non-loopback, and equal to host | ^mask recomputed from the live
// interface table. (No non-emptiness assertion — same CI posture as above.)
func TestNonLoopbackIPv4Broadcasts(t *testing.T) {
	got, err := NonLoopbackIPv4Broadcasts()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	ifaces, _ := net.Interfaces()
	for _, a := range got {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			t.Fatalf("entry %q is not an IPv4 address", a)
		}
		if ip.IsLoopback() {
			t.Fatalf("loopback address %q leaked into NonLoopbackIPv4Broadcasts", a)
		}
		if seen[a] {
			t.Fatalf("duplicate broadcast address %q", a)
		}
		seen[a] = true
		// 每条都必须能在现网接口表里找到 IP|^mask 的出处（对账）。
		matched := false
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 {
				continue
			}
			addrs, aerr := ifc.Addrs()
			if aerr != nil {
				continue
			}
			for _, ad := range addrs {
				ipn, ok := ad.(*net.IPNet)
				if !ok {
					continue
				}
				if want := ipv4Broadcast(ipn.IP.To4(), ipn.Mask); want != nil && want.String() == a {
					matched = true
				}
			}
		}
		if !matched {
			t.Fatalf("broadcast %q has no matching interface (IP|^mask) in the live table", a)
		}
	}
}

// TestIPv4BroadcastPins pins the directed-broadcast arithmetic on synthetic
// inputs: broadcast = IP | ^Mask, per-interface (the LAN-wide reach a unicast
// probe to the interface's own address can never have — the T6 concern this
// rework closes). A non-4-byte mask (IPv6 form) is unusable → nil.
func TestIPv4BroadcastPins(t *testing.T) {
	cases := []struct {
		ip   string
		mask net.IPMask
		want string // "" = expect nil
	}{
		{"192.168.1.10", net.CIDRMask(24, 32), "192.168.1.255"},
		{"10.1.2.3", net.CIDRMask(16, 32), "10.1.255.255"},
		{"172.16.5.7", net.CIDRMask(8, 32), "172.255.255.255"},
		{"192.168.9.9", net.CIDRMask(32, 32), "192.168.9.9"}, // /32: host route → 自身
		{"192.168.1.10", net.CIDRMask(128, 128), ""},         // IPv6 掩码形态 → 不可用
	}
	for _, c := range cases {
		got := ipv4Broadcast(net.ParseIP(c.ip).To4(), c.mask)
		if c.want == "" {
			if got != nil {
				t.Fatalf("ipv4Broadcast(%s, mask) = %v, want nil", c.ip, got)
			}
			continue
		}
		if got == nil || got.String() != c.want {
			t.Fatalf("ipv4Broadcast(%s, mask) = %v, want %s", c.ip, got, c.want)
		}
	}
}
