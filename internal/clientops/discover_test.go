package clientops

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// Plan 42 批1 T6 — client 侧发现(internal/clientops/discover.go):对每个给定
// 目标地址发一个 probe,统一收集窗内收单播 offer,按 SPKI 去重。生产目标由
// NonLoopbackIPv4s() 枚举;测试注入 127.0.0.1 定向(brief 冻结)。

// spkiOf mints a well-formed "sha256:<64 lowercase hex>" pin deterministically
// from a seed — the strict rev4 §3.2 parseOffer validation drops anything else,
// so fixtures must use real-shaped pins.
func spkiOf(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

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
	offer := []byte(fmt.Sprintf(`{"t":"offer","name":"nuc10","spki":%q,"tcp":7878}`, spkiOf("nuc10")))
	raddr := startFakeDiscovery(t, offer)
	got, err := discoverOnPort([]string{"127.0.0.1"}, raddr.Port, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
	if got[0].Name != "nuc10" || got[0].SPKI != spkiOf("nuc10") || got[0].Addr != "127.0.0.1" {
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
	offer := []byte(fmt.Sprintf(`{"t":"offer","name":"odd-port","spki":%q,"tcp":9999}`, spkiOf("odd-port")))
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
	offer := []byte(fmt.Sprintf(`{"t":"offer","name":"nuc10","spki":%q,"tcp":7878}`, spkiOf("same")))
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
	offer := []byte(fmt.Sprintf(`{"t":"offer","name":"n","spki":%q,"tcp":7878}`, spkiOf("ok")))
	raddr := startFakeDiscovery(t, offer)
	// "not-an-ip" 被跳过;"127.0.0.1" 探到 fake responder(它只答合法 probe)。
	got, err := discoverOnPort([]string{"not-an-ip", "127.0.0.1"}, raddr.Port, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SPKI != spkiOf("ok") {
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

// TestParseOffer_FieldValidation pins the rev4 §3.2 three-field gate (终审修复
// Important-2): name whitelist / spki shape / tcp range — any violation drops
// the WHOLE offer, and the old 0/absent→7878 fallback is gone (a legal offer
// must carry an in-range tcp; guessing a port is never dialable).
func TestParseOffer_FieldValidation(t *testing.T) {
	valid := fmt.Sprintf(`{"t":"offer","name":"nuc10","spki":%q,"tcp":7878}`, spkiOf("ok"))
	d, ok := parseOffer([]byte(discoveryMagic + valid))
	if !ok {
		t.Fatalf("legal offer must pass, got dropped")
	}
	if d.Name != "nuc10" || d.SPKI != spkiOf("ok") || d.TCPPort != 7878 {
		t.Fatalf("legal offer parsed wrong: %+v", d)
	}

	longName := strings.Repeat("n", 33) // 33 chars: one over the {0,31}+first shape
	badSPKI := spkiOf("x")
	cases := []struct{ label, offer string }{
		{"name empty", `{"t":"offer","name":"","spki":"` + spkiOf("x") + `","tcp":7878}`},
		{"name leading space", `{"t":"offer","name":" nuc","spki":"` + spkiOf("x") + `","tcp":7878}`},
		{"name NUL", `{"t":"offer","name":"nu\u0000c","spki":"` + badSPKI + `","tcp":7878}`},
		{"name escape", `{"t":"offer","name":"nu\u001bc","spki":"` + badSPKI + `","tcp":7878}`},
		{"name punctuation", `{"t":"offer","name":"nu;c","spki":"` + spkiOf("x") + `","tcp":7878}`},
		{"name too long", `{"t":"offer","name":"` + longName + `","spki":"` + spkiOf("x") + `","tcp":7878}`},
		{"spki bare hex", `{"t":"offer","name":"nuc10","spki":"` + strings.Repeat("a", 64) + `","tcp":7878}`},
		{"spki short", `{"t":"offer","name":"nuc10","spki":"sha256:abc","tcp":7878}`},
		{"spki uppercase", `{"t":"offer","name":"nuc10","spki":"sha256:` + strings.Repeat("A", 64) + `","tcp":7878}`},
		{"spki non-hex", `{"t":"offer","name":"nuc10","spki":"sha256:` + strings.Repeat("g", 64) + `","tcp":7878}`},
		{"tcp zero", `{"t":"offer","name":"nuc10","spki":"` + spkiOf("x") + `","tcp":0}`},
		{"tcp negative", `{"t":"offer","name":"nuc10","spki":"` + spkiOf("x") + `","tcp":-1}`},
		{"tcp over range", `{"t":"offer","name":"nuc10","spki":"` + spkiOf("x") + `","tcp":65536}`},
		{"tcp absent (no fallback)", fmt.Sprintf(`{"t":"offer","name":"nuc10","spki":%q}`, spkiOf("x"))},
	}
	for _, c := range cases {
		if d, ok := parseOffer([]byte(discoveryMagic + c.offer)); ok {
			t.Errorf("%s: malformed offer must be dropped, got %+v", c.label, d)
		}
	}
}

// TestDiscover_DropsMalformedOffer is the socket-level leg: a responder whose
// offer fails the field validation contributes ZERO entries (silently skipped,
// the sweep itself stays error-free).
func TestDiscover_DropsMalformedOffer(t *testing.T) {
	raddr := startFakeDiscovery(t, []byte(`{"t":"offer","name":"evil","spki":"sha256:nothex","tcp":7878}`))
	got, err := discoverOnPort([]string{"127.0.0.1"}, raddr.Port, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("malformed offer must not surface, got %+v", got)
	}
}

// TestStripC0C1 pins the render guard (终审修复 Important-2): C0 (U+0000–U+001F,
// incl. \n \r \t ESC) and C1+DEL (U+007F–U+009F) are removed; every other rune
// — CJK, ⚠/·, punctuation — passes through untouched.
func TestStripC0C1(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain-nuc10_01", "plain-nuc10_01"},
		{"中文节点 · ⚠", "中文节点 · ⚠"},
		{"\x1b[2Jcleared", "[2Jcleared"}, // ESC 清屏序列:控制字节剥、可印字保留
		{"a\rb\nc\td", "abcd"},           // CR/LF/TAB 全剥(渲染恒单行)
		{"x\x00y\x7fz", "xyz"},           // NUL + DEL
		{"s\u009ft", "st"},               // C1 U+009F(UTF-8 双字节编码)
		{"a\u0085b", "ab"},               // C1 U+0085 (NEL)
		{"https://10.0.0.5:7878", "https://10.0.0.5:7878"},
	}
	for _, c := range cases {
		if got := StripC0C1(c.in); got != c.want {
			t.Errorf("StripC0C1(%q) = %q, want %q", c.in, got, c.want)
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
