package sshbroker

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"syscall"
)

// addrRedactedError carries a sanitized Error() text over the ORIGINAL error
// chain. Invariants (spec §5):
//   - Unwrap() returns the original chain verbatim, because core.go's audit
//     classification does errors.Is(err, ErrHostKeyMismatch) / As(*net.OpError)
//     on Connect errors — the chain must stay walkable.
//   - ⚠ The unwrapped chain carries host/IP PLAINTEXT. No log, audit, or
//     persistence path may ever print the cause chain's own text — only
//     Error() (the sanitized message) may flow outward.
//   - net.Error is delegated (Timeout/Temporary) so present and future call
//     sites doing err.(net.Error) keep working.
type addrRedactedError struct {
	msg string
	err error
}

func (e *addrRedactedError) Error() string { return e.msg }
func (e *addrRedactedError) Unwrap() error { return e.err }
func (e *addrRedactedError) Timeout() bool {
	var ne net.Error
	return errors.As(e.err, &ne) && ne.Timeout()
}
func (e *addrRedactedError) Temporary() bool {
	var ne net.Error
	return errors.As(e.err, &ne) && ne.Temporary()
}

// Address-shape detectors for the degradation fallback (step 2). Deliberately
// narrow-but-composable: IPv4, bracketed IPv6, zone-suffixed IPv6, any "::"
// IPv6 form, dotted-host:port, and the "lookup <name>" DNS form. False
// positives only cost a degrade to a generic phrase (fail-safe direction).
var (
	ipv4Re     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	brackV6Re  = regexp.MustCompile(`\[[0-9a-fA-F:.]+%?\w*\]`)
	zoneV6Re   = regexp.MustCompile(`\b[0-9a-fA-F]{0,4}(?::[0-9a-fA-F]{0,4})+%\w+\b`)
	dblColonRe = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:)*::(?:[0-9a-fA-F]{1,4}:?)*\b`)
	hostPortRe = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)+:\d{1,5}\b`)
	lookupRe   = regexp.MustCompile(`(?i)\blookup\s+\S+`)
)

// hostBoundaryRe builds the boundary-aware matcher for one host form: the
// token must not be dot-joined into a longer name on either side (a plain \b
// is NOT a boundary for dotted names — "foo" would match inside
// "foo.corp.internal", leaking the suffix after replacement; spec §5 exp ⑤).
func hostBoundaryRe(form string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^0-9A-Za-z.\-])` + regexp.QuoteMeta(form) + `($|[^0-9A-Za-z.\-])`)
}

// degradedText is the FROZEN classification→phrase map (spec §5: frozen in
// the plan; the golden tests pin these literals as acceptance input). A
// DNS-shaped chain OR any "lookup <name>" form in the original text maps to
// the DNS phrase — the DNSError struct is not required (opaque wrappers like
// fmt.Errorf("lookup %s: ...") carry the same shape).
func degradedText(err error) string {
	var dnsErr *net.DNSError
	isDNS := errors.As(err, &dnsErr) || lookupRe.MatchString(err.Error())
	switch {
	case isDNS:
		return "ssh dial: connect failed: DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ssh dial: connect failed: connection refused"
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "ssh dial: connect failed: timed out"
		}
		return "ssh dial: connect failed"
	}
}

// redactAddr returns err with address information removed from its rendered
// text, preserving the chain (Unwrap) and net.Error (delegation). Two steps
// (spec §5):
//  1. targeted, boundary-aware replacement of the known host/addr forms;
//  2. degradation fallback: if ANY address shape or DNS form survives — or is
//     detected at all in a DNS-error chain — the whole text is replaced by a
//     classified generic phrase. Per-segment scrubbing was disproven by
//     experiment (regex misses zones/search-domains; ParseIP rejects zone and
//     host:port tokens; substring replace mangles short hosts).
func redactAddr(err error, host string, port int) error {
	// msg starts with the standard dial prefix: redactAddr's rendered text IS
	// the final error text the caller surfaces (the frozen degraded phrases
	// below carry the same "ssh dial: " prefix), so pass-through output must
	// be prefixed too — the golden tests pin this exact shape.
	msg := "ssh dial: " + err.Error()

	// Step 1: targeted replacement, longest forms first.
	forms := []string{
		net.JoinHostPort(host, fmt.Sprintf("%d", port)), // host:port / [host]:port
		strings.TrimSuffix(host, "."),
		host,
	}
	seen := map[string]bool{}
	for _, f := range forms {
		if f == "" || seen[strings.ToLower(f)] {
			continue
		}
		seen[strings.ToLower(f)] = true
		msg = hostBoundaryRe(f).ReplaceAllString(msg, "$1[REDACTED]$2")
	}

	// Step 2: degradation fallback.
	var dnsErr *net.DNSError
	addressSurvives := ipv4Re.MatchString(msg) || brackV6Re.MatchString(msg) ||
		zoneV6Re.MatchString(msg) || dblColonRe.MatchString(msg) ||
		hostPortRe.MatchString(msg) || lookupRe.MatchString(msg)
	if addressSurvives || errors.As(err, &dnsErr) {
		return &addrRedactedError{msg: degradedText(err), err: err}
	}
	return &addrRedactedError{msg: msg, err: err}
}
