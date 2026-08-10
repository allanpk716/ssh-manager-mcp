package sshbroker

import (
	"io"
	"net"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

// startEchoService opens a loopback TCP listener on a random port that echoes
// every byte it reads back to the writer (a minimal `socat -` stand-in). It is
// the "remote service" the tunnel forwards to: because the in-process testsshd
// shares the test's loopback, `127.0.0.1:<echoPort>` from the sshd's perspective
// IS this listener. Returned port is the port the broker will ask the sshd to
// dial. The listener is wired to t.Cleanup so it always tears down with the test.
func startEchoService(t *testing.T) (ln net.Listener, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed (test cleanup)
			}
			go func(c net.Conn) {
				defer c.Close()
				// io.Copy(c, c) reads bytes from the conn and writes them back —
				// a single-direction copy suffices because the test client writes
				// first then reads; on client close the Read returns and the defer
				// closes the conn.
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln, portOf(ln.Addr().String())
}

// TestForwardLocal opens an ssh -L tunnel through the in-process testsshd to a
// loopback echo service, then verifies bytes round-trip through it and that
// Close shuts the local listener down. Lifecycle under test:
//   - ForwardLocal(0, "127.0.0.1", echoPort) → Tunnel with a real local addr.
//   - A client Dial to the tunnel's LocalAddr is accepted + piped to the echo
//     service via (*ssh.Client).Dial over the SSH connection (direct-tcpip).
//   - Write "hi"; read the echo → "hi" (end-to-end byte path: client → local
//     listener → ssh channel → sshd → host loopback → echo service).
//   - tunnel.Close() closes the local listener; a subsequent Dial to LocalAddr
//     must fail (the listener is gone, no new conns accepted).
//
// Resource-cleanup contract: Close → listener.Close → serve's Accept errors →
// serve returns (no goroutine leak). Each handle's local + remote conns defer-
// close when their pipe ends.
func TestForwardLocal(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	_, echoPort := startEchoService(t)

	tun, err := c.ForwardLocal(0, "127.0.0.1", echoPort)
	if err != nil {
		t.Fatalf("ForwardLocal: %v", err)
	}
	if tun.LocalAddr() == "" {
		t.Fatal("LocalAddr is empty")
	}
	if tun.ID == "" {
		t.Fatal("ID is empty")
	}

	// Dial the tunnel's local addr and round-trip a byte. Use a deadline so a
	// broken pipe fails fast instead of hanging the test.
	conn, err := net.DialTimeout("tcp", tun.LocalAddr(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf[:n]) != "hi" {
		t.Fatalf("echo = %q, want %q", buf[:n], "hi")
	}

	// Close the tunnel; a subsequent Dial must fail (listener closed).
	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent — a second call must not panic or error unexpectedly.
	if err := tun.Close(); err != nil {
		t.Fatalf("second Close (idempotent): %v", err)
	}

	// Give the kernel a moment to release the port after listener.Close. The
	// listener is closed synchronously inside Close, but the OS may take a tick
	// to refuse new SYNs; a short retry loop distinguishes "closed" (want) from
	// "slow to close" (flaky). After ~300ms a still-connecting Dial is a real
	// leak.
	deadline := time.Now().Add(500 * time.Millisecond)
	var lastDialErr error
	for time.Now().Before(deadline) {
		probe, derr := net.DialTimeout("tcp", tun.LocalAddr(), 50*time.Millisecond)
		if derr != nil {
			lastDialErr = derr // want: connection refused
			break
		}
		probe.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if lastDialErr == nil {
		t.Fatal("post-Close Dial to LocalAddr succeeded, want refused (listener not closed)")
	}
}
