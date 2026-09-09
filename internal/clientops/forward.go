// Plan 48 §2 — the client side of anchor forwarding: a cache-mode client that
// reaches a first-connect target the broker cannot see POSTs the presented
// host key to the broker's /pin-hostkey endpoint, so the anchor lands in the
// authoritative vault (audited) instead of a local writable overlay (which the
// spec explicitly rejects). This file owns the ENTIRE §2.1 branch table and
// the §4 fail-closed texts — every message here is a verbatim contract
// asserted byte-for-byte by forward_test.go (and again end-to-end by the
// cli T4/T6/T7/T8 tests); rewording any of them breaks the suite.
package clientops

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// forwardTimeout bounds one pin-forward POST (§2.1: "转发超时 10 秒(它在 SSH
// 握手关键路径上)"). A hanging broker must fail the handshake within this cap,
// never hang it.
const forwardTimeout = 10 * time.Second

// forwardBodyCap bounds how much of an error body is read (DoPull's 8 KiB
// precedent): §1.1 bodies are tiny; the cap only guards a pathological
// responder, and the abandoned tail at worst drops one keep-alive socket.
const forwardBodyCap = 8 << 10

// ErrPinForwardEqual is the typed 409-equal outcome (§2.1): the broker already
// holds an anchor EQUAL to the presented key (a duplicate forward after a
// restart, or a concurrent equal race). The caller (the HostKeyTOFU wrapper
// wired in the next task) errors.Is-matches this, applies the key locally via
// ApplyForwardedHostKey, and lets the handshake continue — zero user ceremony.
// The user-facing text rides the returned error's Error(); the sentinel is for
// programmatic branching only.
var ErrPinForwardEqual = errors.New("pin-forward conflict: the broker already holds an equal anchor")

// ---- §2.1 branch texts (verbatim contract) ----

// forwardMsg401: a rejected device code is an ordinary failure here — the
// Plan-34 quarantine lives ONLY on the pull path (§1.2), so the text says so
// explicitly instead of letting the user fear for their cache.
const forwardMsg401 = "device code rejected (invalid or revoked) — ask the owner to check cache-tokens; your local cache is NOT affected by this attempt"

func forwardMsg403(host string, port int) string {
	return fmt.Sprintf("the broker refused pin forwarding for %s:%d — the target's server entry is probably not granted to this device's bound profile; ask the owner to grant it (or rebind via cache-tokens bind) and cache pull again", host, port)
}

// forwardMsg404: an old broker has no /pin-hostkey route (http.NotFound) —
// both the out-of-band command and the endpoint need the owner's upgrade.
const forwardMsg404 = "the broker does not support pin forwarding (the owner must upgrade the broker to >= v0.15.0, then retry)"

func forwardMsg409Diff(host string, port int) string {
	return fmt.Sprintf("the broker already has a different pin for %s:%d — run cache pull and retry; if it still fails, ask the owner to compare fingerprints (servers pin-hostkey)", host, port)
}

func forwardMsg409Equal(host string, port int) string {
	return fmt.Sprintf("the broker already holds an equal pin for %s:%d — the forwarded key matches the anchor; applying locally", host, port)
}

// forwardMsgPlaintext: forwarding is a security-sensitive change channel, so
// unlike the pull path there is NO --allow-plaintext escape (§2.1).
func forwardMsgPlaintext(host string, port int) string {
	return fmt.Sprintf("pin forwarding requires a pinned TLS server — no plaintext forwarding (set the server pin used by cache pull); the host key for %s:%d stays unpinned", host, port)
}

// forwardMainMessage renders the §4 fail-closed text — the ONE presentation
// for "unknown anchor and cannot be forwarded" (unreachable broker, 5xx,
// timeout, unexpected status, missing credential). Placeholder rule (§4, rev1
// pinned): fingerprint is the presented key's ssh.FingerprintSHA256 output
// INCLUDING its "SHA256:" prefix; the second occurrence references the value
// verbatim and adds NO literal prefix (the rev1 "SHA256:SHA256:" rendering bug
// is closed by construction here). <name> stays a literal placeholder — the
// owner resolves the entry by host:port, and the client cannot know it.
func forwardMainMessage(host string, port int, fingerprint string) string {
	return fmt.Sprintf("host key for %s:%d is unknown and cannot be pinned here (presented fingerprint: %s). Retry while the broker is reachable — the pin is then forwarded and audited automatically — or ask the owner to run: sshmgr servers pin-hostkey <name> --fingerprint %s", host, port, fingerprint, fingerprint)
}

// forwardError is a branch-mapped failure. Error() IS the verbatim §2.1/§4
// text — T6/T7/T8 byte-compare the final surfaced text, so the cause must not
// leak into it — while err carries the underlying cause (or a sentinel such as
// ErrPinForwardEqual) for errors.Is/As inspection.
type forwardError struct {
	text string
	err  error
}

func (e *forwardError) Error() string { return e.text }
func (e *forwardError) Unwrap() error { return e.err }

// pinForwardRequest is the §1.1 request body (client-side copy; the serve-side
// definition lives in mcpserver/serve.go — the JSON field names are the
// contract). key_blob is base64(std) of the marshaled SSH wire-format key,
// the same bytes remote.Marshal() produced in the handshake callback.
type pinForwardRequest struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	KeyBlob string `json:"key_blob"`
}

// pinForwardConflictBody is the 409 shape: the error text is fixed
// ("already pinned") and equal is the §1.1 zero-leak boolean. A 409 whose body
// does not parse as this shape is treated as equal=false (fail closed — a
// hard error sends the user to the pull-and-retry path; silently passing would
// not).
type pinForwardConflictBody struct {
	Error string `json:"error"`
	Equal bool   `json:"equal"`
}

// pinForwardErrorBody is the 400 shape ({"error": …}); non-JSON bodies (413's
// http.Error text, proxies, WAFs) pass through raw.
type pinForwardErrorBody struct {
	Error string `json:"error"`
}

// PinForwarder forwards presented host keys to the serve broker's
// POST /pin-hostkey. Constructed from the instance's CacheCred; the capability
// decision happens ONCE at construction (§2.1):
//   - capable: pin set + https URL → a pinned-TLS client (DoPull's twin:
//     pinningTransport + no redirects) with the 10s handshake-path cap;
//   - plaintext flavor (Pin == ""): Forward refuses with the §2.1 plaintext
//     text — strictly no plaintext forwarding, no pull-style escape hatch;
//   - missing-credential flavor (URL/Token empty — the cache.auth.json-deleted
//     case): Forward fails closed with the §4 main message, exactly like an
//     unreachable broker. Never a silent local fallback.
type PinForwarder struct {
	url     string
	code    string // bare device code for the Authorization header
	client  *http.Client
	capable bool
	noCred  bool // the missing-credential flavor of "no forwarding capability"
}

// NewPinForwarder builds the forwarder for one cache credential (already read
// from the instance's cache.auth.json by the caller — the --instance resolver
// in cli owns that read, never this package's default-instance fallback).
// Errors only for a hard misconfiguration: a non-https URL paired with a pin.
// Everything else degrades to a no-capability instance whose Forward fails
// closed with the mapped text.
func NewPinForwarder(cred CacheCred) (*PinForwarder, error) {
	if cred.URL == "" || cred.Token == "" {
		return &PinForwarder{noCred: true}, nil
	}
	if cred.Pin == "" {
		return &PinForwarder{}, nil // plaintext flavor
	}
	if u, err := neturl.Parse(cred.URL); err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("pin forwarding requires an https:// url when a server pin is set (got %q)", cred.URL)
	}
	tr, err := pinningTransport(cred.Pin)
	if err != nil {
		return nil, err
	}
	// The device code goes to the Authorization header; the pin is for TLS
	// only. If the token is the composite "<code>:<pin>", strip so the header
	// carries just the code (the DoPull twin).
	code := cred.Token
	if c, _, ok := SplitTokenPin(cred.Token); ok {
		code = c
	}
	return &PinForwarder{
		url:  strings.TrimRight(cred.URL, "/"),
		code: code,
		client: &http.Client{
			Transport: tr,
			Timeout:   forwardTimeout,
			// Redirects are never followed (Plan 37 §2.0, same as DoPull): a
			// followed 30x would take the request off the transport the
			// forwarder chose — and a redirecting "broker" is not the broker.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		capable: true,
	}, nil
}

// Forward POSTs the presented host key to the broker (§2.1 branch table).
// marshaledKey is the handshake's remote.Marshal() bytes; every failure returns
// an error whose Error() is the branch's verbatim text (nil on 201; the 409
// equal=true outcome wraps ErrPinForwardEqual for the caller's branching).
// The HTTP call is synchronous and bounded by forwardTimeout — it sits on the
// SSH handshake critical path.
func (f *PinForwarder) Forward(host string, port int, marshaledKey []byte) error {
	fp := presentedFingerprint(marshaledKey)
	if !f.capable {
		if f.noCred {
			return &forwardError{text: forwardMainMessage(host, port, fp)}
		}
		return &forwardError{text: forwardMsgPlaintext(host, port)}
	}
	body, err := json.Marshal(pinForwardRequest{
		Host:    host,
		Port:    port,
		KeyBlob: base64.StdEncoding.EncodeToString(marshaledKey),
	})
	if err != nil {
		return &forwardError{text: forwardMainMessage(host, port, fp), err: err}
	}
	req, err := http.NewRequest(http.MethodPost, f.url+"/pin-hostkey", bytes.NewReader(body))
	if err != nil {
		return &forwardError{text: forwardMainMessage(host, port, fp), err: err}
	}
	req.Header.Set("Authorization", "Bearer "+f.code)
	res, err := f.client.Do(req)
	if err != nil {
		// Unreachable broker or the 10s cap tripped: §4 main message. The cause
		// rides Unwrap for diagnostics; the text stays exactly the contract.
		return &forwardError{text: forwardMainMessage(host, port, fp), err: err}
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode == http.StatusCreated:
		// 201: the broker landed the anchor (audited server-side); the caller
		// applies the key locally and the handshake continues (wiring next task).
		return nil
	case res.StatusCode == http.StatusUnauthorized:
		// Deliberately NOT the Plan-34 quarantine face: the isolation lives only
		// on the pull path (§1.2) — one bad-token forward attempt must never
		// destroy the local cache. Not even QuarantineCacheFor is referenced.
		return &forwardError{text: forwardMsg401}
	case res.StatusCode == http.StatusForbidden:
		return &forwardError{text: forwardMsg403(host, port)}
	case res.StatusCode == http.StatusNotFound:
		return &forwardError{text: forwardMsg404}
	case res.StatusCode == http.StatusConflict:
		var cr pinForwardConflictBody
		if json.NewDecoder(io.LimitReader(res.Body, forwardBodyCap)).Decode(&cr) != nil || !cr.Equal {
			// Fail closed: an unparsable 409 counts as "different pin" — the
			// pull-and-retry path resolves it either way.
			return &forwardError{text: forwardMsg409Diff(host, port)}
		}
		return &forwardError{text: forwardMsg409Equal(host, port), err: ErrPinForwardEqual}
	case res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusRequestEntityTooLarge:
		// 400/413: the client sent something the broker cannot parse — a defect
		// state, so the server's own text is passed through with the target
		// attached (§2.1).
		return &forwardError{text: fmt.Sprintf("pin forward for %s:%d: broker returned %d — %s",
			host, port, res.StatusCode, serverErrorText(res))}
	default:
		// 5xx and everything unmapped (3xx surfacing via ErrUseLastResponse,
		// 405, ...) is fail-closed: the pin could not be forwarded.
		return &forwardError{text: forwardMainMessage(host, port, fp),
			err: fmt.Errorf("broker returned %d", res.StatusCode)}
	}
}

// presentedFingerprint renders the presented key's OpenSSH fingerprint —
// ssh.FingerprintSHA256 output, "SHA256:"-prefixed (the §4 placeholder rule).
// A key that fails to parse cannot come from remote.Marshal() in production;
// the placeholder merely keeps the fail-closed text renderable.
func presentedFingerprint(marshaledKey []byte) string {
	if pub, err := ssh.ParsePublicKey(marshaledKey); err == nil {
		return ssh.FingerprintSHA256(pub)
	}
	return "unparseable"
}

// serverErrorText extracts the broker's error body for the 400/413 passthrough:
// the §1.1 400 body is {"error": …} JSON; anything else (the 413 http.Error
// text, a proxy page) passes through trimmed. An empty body degrades to the
// status text.
func serverErrorText(res *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, forwardBodyCap))
	var er pinForwardErrorBody
	if json.Unmarshal(raw, &er) == nil && er.Error != "" {
		return er.Error
	}
	if s := strings.TrimSpace(string(raw)); s != "" {
		return s
	}
	return http.StatusText(res.StatusCode)
}
