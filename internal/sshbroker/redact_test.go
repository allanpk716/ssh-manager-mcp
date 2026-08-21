package sshbroker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// redactGolden: every case pins the EXACT output text (golden), so the
// acceptance regex and the redaction detector can never circularly validate
// each other (spec §6 methodology). The negative checks are auxiliary only.
func TestRedactAddrGolden(t *testing.T) {
	const host = "vault.example.internal"
	const port = 22
	refused := &net.OpError{
		Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 22},
		Err:  &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ip refused degrades to classified phrase",
			refused, "ssh dial: connect failed: connection refused"},
		{"dns failure degrades to DNS phrase",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: host, Err: "no such host"}},
			"ssh dial: connect failed: DNS lookup failed"},
		{"wrapped handshake keeps cached ip and degrades (fmt.wrapError caching)",
			fmt.Errorf("ssh: handshake failed: %w", &net.OpError{
				Op: "read", Net: "tcp",
				Source: &net.TCPAddr{IP: net.ParseIP("10.0.0.9"), Port: 53210},
				Addr:   &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 22},
				Err:    &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET},
			}),
			"ssh dial: connect failed"},
		{"dns case+trailing-dot form degrades (lookup regex)",
			&net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("lookup Vault.Example.Internal.: no such host")},
			"ssh dial: connect failed: DNS lookup failed"},
		{"dns search-domain form degrades (host=foo, name=foo.corp.internal)",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: "foo.corp.internal", Err: "no such host"}},
			"ssh dial: connect failed: DNS lookup failed"},
		{"ipv6 zone form degrades",
			errors.New("read tcp fe80::1%eth0->10.0.0.5:22: connection reset by peer"),
			"ssh dial: connect failed"},
		{"malformed addr error (legacy Sprintf join) degrades",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Addr: "2001:db8::1:22", Err: "too many colons in address"}},
			"ssh dial: connect failed"},
		{"address-free text passes through untouched",
			errors.New("ssh: handshake failed: EOF"),
			"ssh dial: ssh: handshake failed: EOF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactAddr(c.err, host, port).Error()
			if got != c.want {
				t.Fatalf("golden mismatch:\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// Targeted-replacement cases (step 1 of the two-step design): the host token
// and host:port forms ARE redacted in place when they appear standalone —
// boundary-aware, so a dotted longer name sharing the prefix is NOT clobbered.
func TestRedactAddrTargeted(t *testing.T) {
	// Standalone host token → [REDACTED], rest of text intact.
	got := redactAddr(errors.New("connect to vault.example.internal refused"), "vault.example.internal", 22)
	if want := "ssh dial: connect to [REDACTED] refused"; got.Error() != want {
		t.Fatalf("token: got %q want %q", got.Error(), want)
	}
	// host:port form → [REDACTED].
	got = redactAddr(errors.New("dial vault.example.internal:22: refused"), "vault.example.internal", 22)
	if want := "ssh dial: dial [REDACTED]: refused"; got.Error() != want {
		t.Fatalf("host:port: got %q want %q", got.Error(), want)
	}
	// Case-insensitive host match.
	got = redactAddr(errors.New("connect to VAULT.EXAMPLE.INTERNAL refused"), "vault.example.internal", 22)
	if want := "ssh dial: connect to [REDACTED] refused"; got.Error() != want {
		t.Fatalf("case-insensitive: got %q want %q", got.Error(), want)
	}
	// Short host known trade-off: standalone "db" is redacted (fail-safe —
	// only diagnostic damage), surrounding words untouched.
	got = redactAddr(errors.New("postgres db cluster check failed"), "db", 22)
	if want := "ssh dial: postgres [REDACTED] cluster check failed"; got.Error() != want {
		t.Fatalf("short host: got %q want %q", got.Error(), want)
	}
	// A LONGER dotted name containing the host as dot-joined prefix is NOT
	// boundary-matched; the residual address form then triggers degradation.
	got = redactAddr(errors.New("dial tcp: lookup foo.corp.internal: no such host"), "foo", 22)
	if want := "ssh dial: connect failed: DNS lookup failed"; got.Error() != want {
		t.Fatalf("search-domain: got %q want %q", got.Error(), want)
	}
}

// The wrapper must preserve errors.Is (audit classification) and satisfy
// net.Error via delegation (spec §5; the delegation experiment proved both).
func TestRedactAddrWrapperContract(t *testing.T) {
	wrapped := redactAddr(fmt.Errorf("ssh: handshake failed: %w", ErrHostKeyMismatch), "h", 22)
	if !errors.Is(wrapped, ErrHostKeyMismatch) {
		t.Fatal("errors.Is must traverse Unwrap to ErrHostKeyMismatch")
	}
	inner := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ETIMEDOUT}}
	w2 := redactAddr(inner, "h", 22)
	var ne net.Error
	if !errors.As(w2, &ne) || !ne.Timeout() {
		t.Fatal("wrapper must delegate net.Error (Timeout)")
	}
	var oe *net.OpError
	if !errors.As(w2, &oe) {
		t.Fatal("errors.As must reach the original *net.OpError")
	}
}
