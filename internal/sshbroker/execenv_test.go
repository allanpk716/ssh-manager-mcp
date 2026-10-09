package sshbroker

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

// osUnsetenvForTest removes the env var for the duration of the test and
// restores whatever value it held before afterwards (os.Unsetenv alone would
// leak the removal into tests that run later in the same process).
func osUnsetenvForTest(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	os.Unsetenv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, prev)
		} else {
			os.Unsetenv(key)
		}
	})
}

// TestKillGraceKnob covers the SSHMGR_EXEC_KILL_GRACE parsing rules: unset /
// empty / whitespace → the 2s default; any valid duration inside [100ms, 30s]
// parses verbatim (both bounds inclusive); garbage / negative / below-floor /
// above-ceiling fail-closed with an error naming the env var and the accepted
// range.
func TestKillGraceKnob(t *testing.T) {
	osUnsetenvForTest(t, "SSHMGR_EXEC_KILL_GRACE")
	if d, err := killGraceFromEnv(); err != nil || d != 2*time.Second {
		t.Fatalf("unset = (%v, %v), want (2s, nil)", d, err)
	}
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "")
	if d, err := killGraceFromEnv(); err != nil || d != 2*time.Second {
		t.Fatalf("empty = (%v, %v), want (2s, nil)", d, err)
	}
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "   ")
	if d, err := killGraceFromEnv(); err != nil || d != 2*time.Second {
		t.Fatalf("whitespace = (%v, %v), want (2s, nil)", d, err)
	}
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms")
	if d, err := killGraceFromEnv(); err != nil || d != 300*time.Millisecond {
		t.Fatalf("300ms = (%v, %v), want (300ms, nil)", d, err)
	}
	// The bounds are inclusive.
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "100ms")
	if d, err := killGraceFromEnv(); err != nil || d != 100*time.Millisecond {
		t.Fatalf("100ms (floor) = (%v, %v), want (100ms, nil)", d, err)
	}
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "30s")
	if d, err := killGraceFromEnv(); err != nil || d != 30*time.Second {
		t.Fatalf("30s (ceiling) = (%v, %v), want (30s, nil)", d, err)
	}
	for _, v := range []string{"garbage", "-1s", "50ms", "1m"} {
		t.Setenv("SSHMGR_EXEC_KILL_GRACE", v)
		d, err := killGraceFromEnv()
		if err == nil {
			t.Fatalf("%q: expected a fail-closed error, got %v", v, d)
		}
		if !strings.Contains(err.Error(), "SSHMGR_EXEC_KILL_GRACE") || !strings.Contains(err.Error(), "[100ms, 30s]") {
			t.Fatalf("%q: err = %v, want it naming the env var and the range [100ms, 30s]", v, err)
		}
	}
}

// TestKillGraceKnobInvalidFailsBeforeDial proves an invalid grace knob fails
// connection construction BEFORE the dial (the same fail-closed shape as the
// host-key-algorithms knob): the target listener accepts but never sends the
// SSH banner, so a late validation would block inside ssh.Dial — the ctx
// deadline below is only a safety net, and hitting it would mean validation
// ran after the dial started.
func TestKillGraceKnobInvalidFailsBeforeDial(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "garbage")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			_ = conn // intentionally do NOT send the SSH banner — hold any dial open
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err = Connect(ctx, hostOf(ln.Addr().String()), portOf(ln.Addr().String()), "u", PasswordAuth("pw"), ssh.InsecureIgnoreHostKey())
	if err == nil {
		t.Fatal("invalid grace knob must fail-closed")
	}
	if !strings.Contains(err.Error(), "SSHMGR_EXEC_KILL_GRACE") {
		t.Fatalf("err = %v, want error naming SSHMGR_EXEC_KILL_GRACE", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("validation took %v, want immediate (fail-closed BEFORE dial)", elapsed)
	}
}

// TestKillGraceKnobInjectedAtConstruction proves connectWith resolves the knob
// into the Client's killGrace field (immutable after construction — the
// watchdog reads it without synchronization): the parsed value lands on the
// client, and the default applies when the knob is unset.
func TestKillGraceKnobInjectedAtConstruction(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()

	osUnsetenvForTest(t, "SSHMGR_EXEC_KILL_GRACE")
	c := connectTest(t, addr, hk)
	if c.killGrace != 2*time.Second {
		t.Fatalf("killGrace = %v, want the 2s default", c.killGrace)
	}
	c.Close()

	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms")
	c2 := connectTest(t, addr, hk)
	if c2.killGrace != 300*time.Millisecond {
		t.Fatalf("killGrace = %v, want 300ms from the knob", c2.killGrace)
	}
}
