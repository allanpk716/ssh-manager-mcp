package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sync"
	"time"
	"unicode"
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
// scanner. enabled() is evaluated once up front (false → no socket is bound at
// all, saving the port) and once per packet (a live responder that gets
// switched off goes silent without a serve restart — same ≤5s switch machinery
// as the /pair gate, switches.go).
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
	// discoveryNameFallback is the last-resort display name when neither the
	// configured name nor the hostname can be sanitized into the whitelist.
	discoveryNameFallback = "sshmgr"
	// settingDiscoveryName is the frozen store key for the discovery display
	// name (read by RunServe; written by the 批2 Settings surface like
	// serve.pairing / serve.discovery).
	settingDiscoveryName = "serve.discovery_name"
)

// discoveryNameRe is the frozen display-name whitelist: one letter or number,
// then letters/numbers/space/dot/underscore/hyphen, ≤32 runes total. Anything
// else falls back to the hostname (then to force-derivation).
var discoveryNameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`)

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

// sanitizeDiscoveryName maps the configured display name onto the whitelist:
// valid name → as-is; else a whitelisted hostname; else the hostname
// force-derived into the whitelist; else the constant fallback. Pure — the
// caller injects os.Hostname() so the chain is unit-testable. Never returns an
// empty string or a name outside the whitelist.
func sanitizeDiscoveryName(name, hostname string) string {
	if discoveryNameRe.MatchString(name) {
		return name
	}
	if discoveryNameRe.MatchString(hostname) {
		return hostname
	}
	return deriveDiscoveryName(hostname)
}

// deriveDiscoveryName force-fits a raw hostname into the whitelist: keep only
// whitelisted runes, trim a leading non-alphanumeric run, cap at 32 runes;
// nothing survives → the constant fallback.
func deriveDiscoveryName(hostname string) string {
	kept := make([]rune, 0, len(hostname))
	for _, r := range hostname {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			kept = append(kept, r)
		case r == ' ' || r == '.' || r == '_' || r == '-':
			kept = append(kept, r)
		}
	}
	i := 0
	for i < len(kept) && !unicode.IsLetter(kept[i]) && !unicode.IsNumber(kept[i]) {
		i++
	}
	kept = kept[i:]
	if len(kept) > 32 {
		kept = kept[:32]
	}
	if len(kept) == 0 {
		return discoveryNameFallback
	}
	return string(kept)
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
// Posture (all frozen by the brief):
//   - enabled() false at entry → NO socket is bound at all (省资源 — 控制裁决);
//     the returned stop is a no-op.
//   - udp/7878 bind failure (typically another serve) → one stderr line, then
//     a no-op stop — serve continues without discovery.
//   - Only valid probes are answered, unicast to the source; the offer write
//     carries a 1s deadline and its errors are ignored (an unreachable prober
//     must never kill the loop).
func StartDiscovery(ctx context.Context, name string, tcpPort int, spki string, enabled func() bool) (stop func()) {
	noop := func() {}
	if enabled == nil {
		return noop // defensive: a nil gate can never answer
	}
	if !enabled() {
		return noop // 关=干脆不监听(省端口/省资源)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: discoveryPort})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager serve: discovery: udp/%d unavailable (disabled): %v\n", discoveryPort, err)
		return noop
	}

	host, _ := os.Hostname()
	offerJSON, merr := json.Marshal(discOffer{
		T:    "offer",
		Name: sanitizeDiscoveryName(name, host),
		SPKI: spki,
		TCP:  tcpPort,
	})
	if merr != nil {
		conn.Close() // unreachable for a fixed struct; defended anyway
		return noop
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
	return stopFn
}
