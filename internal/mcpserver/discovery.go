package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"ssh-manager-mcp/internal/discoveryname"
)

// UDP discovery responder (Plan 42 批1 T6, spec §3.4): a serve answers LAN
// probes so a client can find the broker without typing its address.
//
// Wire format is FROZEN (task-6 brief):
//   probe = first line "sshmgr-disc-v1\n" + JSON {"t":"probe"}
//   offer = JSON {"t":"offer","name","spki","tcp"} — unicast back to the
//           probe's SOURCE address only (never rebroadcast: no amplification
//           surface, no third-party spoofing of a flood).
//
// The responder is deliberately mute and dumb: only well-formed probes get an
// answer, everything else (wrong magic, malformed JSON, wrong type) is dropped
// SILENTLY — no log noise from LAN background chatter, no error oracle for a
// scanner. enabled() is evaluated PER PACKET (a live responder that gets
// switched off goes silent without a serve restart — same ≤5s switch machinery
// as the /pair gate, switches.go — and, post-T7 rework, an off→on flip takes
// effect on the next probe too, because the socket is bound regardless of the
// switch state; the cost is one idle socket).
//
// Discovery is an ENHANCEMENT surface: a udp/7878 bind failure (typically a
// second serve on the host) costs one stderr line and serve continues.

const (
	// discoveryMagic is the frozen probe/offer first line. Versioned so a
	// future incompatible format can coexist on the same port.
	discoveryMagic = "sshmgr-disc-v1\n"
	// discoveryPort is the frozen UDP port the responder binds (0.0.0.0).
	discoveryPort = 7878
	// discoveryReadBuf caps one datagram read; probes are tiny and anything
	// at/over the cap is malformed by construction (truncated JSON → dropped).
	discoveryReadBuf = 512
	// discoveryIOWindow is the per-op socket deadline: one slow or malicious
	// peer must never stall the read loop longer than a single window.
	discoveryIOWindow = time.Second
	// settingDiscoveryName is the frozen store key for the discovery display
	// name (read by RunServe; written by the 批2 Settings surface like
	// serve.pairing / serve.discovery).
	settingDiscoveryName = "serve.discovery_name"
)

// discProbe / discOffer are the frozen JSON payloads.
type discProbe struct {
	T string `json:"t"`
}

type discOffer struct {
	T    string `json:"t"`
	Name string `json:"name"`
	SPKI string `json:"spki"`
	TCP  int    `json:"tcp"`
}

// validProbe applies the frozen acceptance rule: magic prefix + JSON body
// whose type field is exactly "probe".
func validProbe(data []byte) bool {
	if !bytes.HasPrefix(data, []byte(discoveryMagic)) {
		return false
	}
	var p discProbe
	if json.Unmarshal(bytes.TrimSpace(data[len(discoveryMagic):]), &p) != nil {
		return false
	}
	return p.T == "probe"
}

// StartDiscovery runs the UDP discovery responder until ctx is cancelled or
// the returned stop is called (idempotent; RunServe defers it AND the ctx
// hookup may fire it). Parameters: name = configured display name (sanitized
// with hostname fallback here), tcpPort = the TCP port advertised in offers
// (RunServe passes the real bound port), spki = the serve cert SPKI pin the
// client should pin, enabled = the live discovery switch (per-packet).
//
// Posture (frozen by the briefs; the first bullet reworked by T7):
//   - the socket is bound REGARDLESS of the switch state (T7 控制裁决:off→on
//     无需重启,逐包门是唯一权威;代价一个 idle socket)。enabled()==false →
//     bound but silent; the returned stop still closes the socket.
//   - udp/7878 bind failure (typically another serve) → one stderr line, then
//     a no-op stop — serve continues without discovery.
//   - Only valid probes are answered, unicast to the source; the offer write
//     carries a 1s deadline and its errors are ignored (an unreachable prober
//     must never kill the loop).
func StartDiscovery(ctx context.Context, name string, tcpPort int, spki string, enabled func() bool) (stop func()) {
	stop, _, err := StartDiscoveryAt(
		ctx,
		net.JoinHostPort("0.0.0.0", strconv.Itoa(discoveryPort)),
		name,
		tcpPort,
		spki,
		enabled,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: discovery: udp/%d unavailable (port in use): %v\n", discoveryPort, err)
	}
	return stop
}

// StartDiscoveryAt binds an explicit UDP address and returns the real bound
// address. Tests use `127.0.0.1:0` so concurrent test processes never share the
// production UDP port. Bind failures are returned instead of being hidden;
// StartDiscovery keeps the production behavior of logging and continuing.
func StartDiscoveryAt(
	ctx context.Context,
	address string,
	name string,
	tcpPort int,
	spki string,
	enabled func() bool,
) (stop func(), bound *net.UDPAddr, err error) {
	noop := func() {}
	if enabled == nil {
		return noop, nil, nil // defensive: a nil gate can never answer
	}
	udpAddress, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return noop, nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return noop, nil, err
	}
	localAddress, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		conn.Close()
		return noop, nil, fmt.Errorf("discovery returned an unexpected local address %T", conn.LocalAddr())
	}

	offerJSON, merr := json.Marshal(discOffer{
		T:    "offer",
		Name: discoveryname.DisplayName(name, currentHostname()),
		SPKI: spki,
		TCP:  tcpPort,
	})
	if merr != nil {
		conn.Close() // unreachable for a fixed struct; defended anyway
		return noop, localAddress, nil
	}
	// 对称报文:offer 与 probe 同样带魔数首行(接收方先验魔数再解 JSON)。
	offer := append([]byte(discoveryMagic), offerJSON...)

	var once sync.Once
	stopFn := func() {
		once.Do(func() { conn.Close() })
	}

	go func() {
		buf := make([]byte, discoveryReadBuf)
		for {
			// ctx 監視走 1s 读超时:每次醒来顺手检查,不必额外 goroutine。
			if ctx.Err() != nil {
				stopFn()
				return
			}
			if err := conn.SetReadDeadline(time.Now().Add(discoveryIOWindow)); err != nil {
				return // socket closed by stop()
			}
			n, src, rerr := conn.ReadFromUDP(buf)
			if rerr != nil {
				var ne net.Error
				if errors.As(rerr, &ne) && ne.Timeout() {
					continue // window elapsed → loop re-checks ctx
				}
				return // socket closed by stop() (or unrecoverable socket fault)
			}
			if !validProbe(buf[:n]) || !enabled() {
				continue // 魔数/JSON 畸形静默;开关逐包评估,关=不答
			}
			_ = conn.SetWriteDeadline(time.Now().Add(discoveryIOWindow))
			_, _ = conn.WriteToUDP(offer, src) // 只单播回源;写失败(探测方已走)无需理会
		}
	}()
	return stopFn, localAddress, nil
}

func currentHostname() string {
	host, _ := os.Hostname()
	return host
}
