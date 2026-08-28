package clientops

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"time"
)

// Client-side UDP discovery (Plan 42 批1 T6): send one probe per target
// address, collect unicast offers for one unified window, deduplicate by SPKI
// (the same broker answering on two NICs is ONE broker).
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

// Discovered is one unique serve found on the network. Addr is the source IP
// the offer arrived from — the client dials <Addr>:<serve TCP port>; SPKI is
// the pin to verify the serve cert against (never connect on an empty pin).
type Discovered struct {
	Name string
	Addr string
	SPKI string
}

// discOfferMsg is the frozen offer payload (the tcp field is read for shape
// validation but not surfaced — Discovered is the frozen cross-task struct).
type discOfferMsg struct {
	T    string `json:"t"`
	Name string `json:"name"`
	SPKI string `json:"spki"`
	TCP  int    `json:"tcp"`
}

// NonLoopbackIPv4s enumerates this host's routable IPv4 addresses: interfaces
// that are Up AND broadcast-capable (point-to-point links without broadcast
// drop out), loopback excluded. These are the production Discover targets;
// tests inject literal addresses (e.g. 127.0.0.1) instead. An unreadable
// single interface is skipped, not fatal.
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

// Discover sends one discovery probe per target address and collects unicast
// offers for the unified window (timeout; <= 0 → defaultDiscoverWindow),
// deduplicating by SPKI. Transport-honest: it probes exactly the addresses
// given — production callers pass NonLoopbackIPv4s(), tests inject literals
// such as 127.0.0.1. Unparseable targets and per-target dial/write failures
// are skipped, not errors; the error return is reserved for enumeration-level
// faults. Only offers whose type is "offer" and that carry the frozen magic
// are accepted.
func Discover(targetIfaces []string, timeout time.Duration) ([]Discovered, error) {
	return discoverOnPort(targetIfaces, discoveryUDPPort, timeout)
}

// discoverOnPort is Discover with the port injected (tests point it at an
// ephemeral fake responder instead of contending for the frozen 7878).
func discoverOnPort(targets []string, port int, timeout time.Duration) ([]Discovered, error) {
	if timeout <= 0 {
		timeout = defaultDiscoverWindow
	}
	deadline := time.Now().Add(timeout)

	offers := make(chan Discovered, 64)
	var wg sync.WaitGroup
	for _, tgt := range targets {
		ip := net.ParseIP(strings.TrimSpace(tgt))
		if ip == nil {
			continue // a malformed entry must not sink the sweep
		}
		conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: port})
		if err != nil {
			continue // unroutable target — skip, keep discovering
		}
		wg.Add(1)
		go func(c *net.UDPConn) {
			defer wg.Done()
			defer c.Close()
			if _, err := c.Write([]byte(discoveryMagic + `{"t":"probe"}` + "\n")); err != nil {
				return
			}
			buf := make([]byte, discoveryReadBuf)
			for {
				if err := c.SetReadDeadline(deadline); err != nil {
					return
				}
				n, rerr := c.Read(buf)
				if rerr != nil {
					return // window over (or socket closed)
				}
				if d, ok := parseOffer(buf[:n]); ok {
					if ra, rok := c.RemoteAddr().(*net.UDPAddr); rok && ra.IP != nil {
						d.Addr = ra.IP.String() // offer 的来源地址 = 客户端应拨的目标
					}
					select {
					case offers <- d:
					default: // never let a chatty LAN block a reader
					}
				}
			}
		}(conn)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var out []Discovered
	seen := make(map[string]bool)
	add := func(d Discovered) {
		if !seen[d.SPKI] {
			seen[d.SPKI] = true
			out = append(out, d)
		}
	}
	for {
		select {
		case d := <-offers:
			add(d)
		case <-done:
			// All readers exited (window over); drain whatever is already
			// buffered so a late offer is not dropped on the floor.
			for {
				select {
				case d := <-offers:
					add(d)
				default:
					return out, nil
				}
			}
		}
	}
}

// parseOffer applies the frozen acceptance rule (magic prefix + JSON whose
// type is "offer").
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
	return Discovered{Name: o.Name, SPKI: o.SPKI}, true
}
