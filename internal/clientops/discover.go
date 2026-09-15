package clientops

import (
	"bytes"
	"encoding/json"
	"net"
	"regexp"
	"strings"
	"time"
)

// Client-side UDP discovery (Plan 42 批1 T6/T7): send one probe per TARGET
// address (production: the per-interface DIRECTED BROADCAST addresses from
// NonLoopbackIPv4Broadcasts — a unicast probe to this host's own interface IP
// can never reach the LAN, the T6-Critical flaw this rework closes), receive
// offers on ONE unconnected socket, take each offer's REAL source address as
// the broker address, and deduplicate by SPKI (the same broker answering on
// two NICs is ONE broker).
//
// The wire constants below are LOCAL COPIES of the frozen format served by
// internal/mcpserver/discovery.go — clientops must not import mcpserver (the
// serve-side package owns the responder; the client shares only the wire
// contract). If either side's format changes, both copies change together.

const (
	// discoveryUDPPort is the frozen discovery port (serve binds 0.0.0.0:7878/udp).
	discoveryUDPPort = 7878
	// discoveryMagic is the frozen probe first line — must byte-match the
	// serve-side responder.
	discoveryMagic = "sshmgr-disc-v1\n"
	// discoveryReadBuf caps one datagram read (offers are tiny; anything at or
	// over the cap is malformed by construction).
	discoveryReadBuf = 512
	// defaultDiscoverWindow is the unified collection window when the caller
	// passes timeout <= 0.
	defaultDiscoverWindow = 2 * time.Second
)

// Discovered is one unique serve found on the network. Addr is the offer's
// real SOURCE IP (bare host, no port) read via ReadFromUDP on the unconnected
// socket — the client dials <Addr>:<TCPPort>; SPKI is the pin to verify the
// serve cert against (never connect on an empty pin); TCPPort is the offer's
// frozen `tcp` field, which parseOffer hard-validates (an offer without a
// well-formed name/spki/tcp is DROPPED, never defaulted).
type Discovered struct {
	Name    string
	Addr    string
	SPKI    string
	TCPPort int
}

// discOfferMsg is the frozen offer payload (mirrors the serve-side discOffer).
type discOfferMsg struct {
	T    string `json:"t"`
	Name string `json:"name"`
	SPKI string `json:"spki"`
	TCP  int    `json:"tcp"`
}

// NonLoopbackIPv4s enumerates this host's routable IPv4 addresses: interfaces
// that are Up AND broadcast-capable (point-to-point links without broadcast
// drop out), loopback excluded. Kept for callers that want the interface
// addresses themselves; the production Discover target set is
// NonLoopbackIPv4Broadcasts. An unreadable single interface is skipped, not
// fatal.
func NonLoopbackIPv4s() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	return out, nil
}

// NonLoopbackIPv4Broadcasts is the production Discover target set (T7
// rework): one DIRECTED BROADCAST address per IPv4 non-loopback
// broadcast-capable interface address — IP | ^Mask computed from the
// *net.IPNet's Mask, so a probe reaches EVERY host on the interface's subnet
// (a unicast to the interface's own address only ever finds same-host
// serves). Duplicates (two interfaces on one subnet) collapse. Unusable
// entries (non-IPv4, no 4-byte mask, loopback) are skipped; an unreadable
// single interface is skipped, not fatal.
func NonLoopbackIPv4Broadcasts() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []string
	seen := make(map[string]bool)
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			b := ipv4Broadcast(ip4, ipn.Mask)
			if b == nil {
				continue
			}
			s := b.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// ipv4Broadcast computes the directed-broadcast address IP | ^Mask for a
// 4-byte IP. nil when the mask is not the 4-byte IPv4 form (an IPv6-shaped
// mask on a v4 address is unusable arithmetic, not a broadcast target).
func ipv4Broadcast(ip4 net.IP, mask net.IPMask) net.IP {
	if ip4 == nil || len(mask) != 4 {
		return nil
	}
	b := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		b[i] = ip4[i] | ^mask[i]
	}
	return b
}

// Discover sends one discovery probe per target address (production:
// NonLoopbackIPv4Broadcasts; tests inject literals such as 127.0.0.1) and
// collects offers for the unified window (timeout; <= 0 →
// defaultDiscoverWindow), deduplicating by SPKI. All probes leave ONE
// unconnected UDP socket (0.0.0.0:0) via WriteToUDP — Go's DGRAM sockets
// carry SO_BROADCAST by default, so directed-broadcast targets need no extra
// socket option — and every offer is read from the same socket with
// ReadFromUDP, which is what makes the offer's real source address (and
// therefore cross-host discovery) observable. Unparseable targets and
// per-target write failures are skipped, not errors; only offers whose type
// is "offer", that carry the frozen magic, AND that pass the three-field
// validation (parseOffer) are accepted — everything else is silently dropped.
func Discover(targets []string, timeout time.Duration) ([]Discovered, error) {
	return discoverOnPort(targets, discoveryUDPPort, timeout)
}

// discoverOnPort is Discover with the port injected (tests point it at an
// ephemeral fake responder instead of contending for the frozen 7878).
func discoverOnPort(targets []string, port int, timeout time.Duration) ([]Discovered, error) {
	if timeout <= 0 {
		timeout = defaultDiscoverWindow
	}
	// 单张 unconnected socket:绑 0.0.0.0:0,收到的每个 offer 都带真实源地址
	// (connected socket 只能收到显式 dial 对端的回包,LAN 广播场景不存在)。
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	probe := []byte(discoveryMagic + `{"t":"probe"}` + "\n")
	for _, tgt := range targets {
		ip := net.ParseIP(strings.TrimSpace(tgt))
		if ip == nil {
			continue // a malformed entry must not sink the sweep
		}
		// Write 失败(不可路由目标)只跳过该目标,继续扫其余;写错误无需上报。
		_, _ = conn.WriteToUDP(probe, &net.UDPAddr{IP: ip, Port: port})
	}

	deadline := time.Now().Add(timeout)
	buf := make([]byte, discoveryReadBuf)
	var out []Discovered
	seen := make(map[string]bool)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			break // socket closed under us — nothing more to collect
		}
		n, src, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			break // window over (deadline exceeded) or socket closed
		}
		if src == nil || src.IP == nil {
			continue
		}
		d, ok := parseOffer(buf[:n])
		if !ok {
			continue // 噪声/畸形 offer 静默丢弃
		}
		d.Addr = src.IP.String() // offer 的真实来源地址 = 客户端应拨的目标(仅 host,不带端口)
		if seen[d.SPKI] {
			continue // 同一 broker 双网卡各答一次 → 按 SPKI 归一
		}
		seen[d.SPKI] = true
		out = append(out, d)
	}
	return out, nil
}

// discoveryFieldRe holds the rev4 §3.2 field sanitization (frozen, codex#4): the
// offer is UNAUTHENTICATED input that reaches the picker UI, so every field is
// re-validated client-side and any violation drops the whole offer — no 7878
// fallback, no best-effort repair. name reuses the serve-side display-name
// whitelist (same regex as internal/mcpserver/discovery.go's sanitizeDiscoveryName
// and the §3.3-1 hint pattern — a LOCAL COPY, clientops must not import mcpserver).
var (
	discoveryNameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`)
	discoverySPKIRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// parseOffer applies the frozen acceptance rule (magic prefix + JSON whose type
// is "offer") plus the rev4 §3.2 three-field validation: name must match the
// display-name whitelist, spki must be a full "sha256:<64 lowercase hex>" pin,
// and tcp must be in [1,65535]. ANY violation drops the offer (ok=false) — the
// zero/absent-tcp→7878 fallback is gone: a serve that predates the field is not
// dialable on guesswork, it simply doesn't answer with a valid offer.
func parseOffer(data []byte) (Discovered, bool) {
	if !bytes.HasPrefix(data, []byte(discoveryMagic)) {
		return Discovered{}, false
	}
	var o discOfferMsg
	if json.Unmarshal(bytes.TrimSpace(data[len(discoveryMagic):]), &o) != nil {
		return Discovered{}, false
	}
	if o.T != "offer" {
		return Discovered{}, false
	}
	if !discoveryNameRe.MatchString(o.Name) ||
		!discoverySPKIRe.MatchString(o.SPKI) ||
		o.TCP < 1 || o.TCP > 65535 {
		return Discovered{}, false // 畸形字段 = 丢弃整个 offer,绝不兜底
	}
	return Discovered{Name: o.Name, SPKI: o.SPKI, TCPPort: o.TCP}, true
}
