package sshbroker

import (
	"io"
	"net"
	"strings"
	"sync/atomic"
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

// startHalfCloseEchoService opens a loopback TCP listener on a random port
// whose echo handler reads the request side to EOF, THEN writes "FIN" and
// half-closes its own write side. This is the half-close core assertion: the
// service must still be able to send bytes AFTER seeing the client's EOF —
// which only reaches it if every hop in the chain (tunnel handle → ssh channel
// → sshd direct-tcpip handler) propagates a directional EOF instead of a full
// close. Returned port is the port the broker asks the sshd to dial.
func startHalfCloseEchoService(t *testing.T) (ln net.Listener, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("half-close echo listen: %v", err)
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
				_, _ = io.Copy(io.Discard, c) // drain the request side until EOF
				_, _ = c.Write([]byte("FIN")) // writable after client EOF — the assertion
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
			}(c)
		}
	}()
	return ln, portOf(ln.Addr().String())
}

// TestTunnelHalfClosePropagates proves the tunnel forwards a TCP half-close
// end-to-end: the client writes a request, CloseWrite()s (FIN, still reading),
// and MUST receive the server's response bytes that are produced AFTER the
// server saw the EOF. The pre-fix handle closed both conns when the FIRST
// io.Copy direction finished — the client's CloseWrite finished the
// local→remote copy, the defers tore down both conns, and the in-flight "FIN"
// response was truncated/errored. Lifecycle under test:
//
//   - client → tunnel: Write("hello"), TCPConn.CloseWrite(), io.ReadAll.
//   - tunnel handle: local→remote copy EOF → CloseWrite on the ssh channel
//     (SSH_MSG_CHANNEL_EOF, not a channel close).
//   - sshd handleDirectTCP: channel EOF → CloseWrite on the echo-service TCP
//     conn (FIN, symmetric half-close on the test-infra side).
//   - echo service: sees EOF, writes "FIN", CloseWrite → propagates back the
//     same way; the client's ReadAll gets "FIN" then a clean EOF.
func TestTunnelHalfClosePropagates(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	_, echoPort := startHalfCloseEchoService(t)

	tun, err := c.ForwardLocal(0, "127.0.0.1", "127.0.0.1", echoPort, nil)
	if err != nil {
		t.Fatalf("ForwardLocal: %v", err)
	}
	defer tun.Close()

	conn, err := net.DialTimeout("tcp", tun.LocalAddr(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	// Deadline keeps a broken pipe failing fast instead of hanging the test.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tc := conn.(*net.TCPConn)
	if err := tc.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll after CloseWrite: %v", err)
	}
	if string(got) != "FIN" {
		t.Fatalf("read = %q, want %q (response produced after server EOF must arrive)", got, "FIN")
	}
}

// TestForwardLocal opens an ssh -L tunnel through the in-process testsshd to a
// loopback echo service, then verifies bytes round-trip through it and that
// Close shuts the local listener down. Lifecycle under test:
//   - ForwardLocal(0, "127.0.0.1", "127.0.0.1", echoPort, nil) → Tunnel with a
//     real local addr.
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

	tun, err := c.ForwardLocal(0, "127.0.0.1", "127.0.0.1", echoPort, nil)
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

// pickLocalNonLoopbackIP returns this host's first non-loopback IPv4 address,
// skipping the test when none exists (bare CI containers often have only
// 127.0.0.1).
func pickLocalNonLoopbackIP(t *testing.T) net.IP {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP
		}
	}
	t.Skip("no non-loopback IPv4 on this host")
	return nil
}

// TestForwardLocalListenHostBindsNonLoopback proves the listenHost parameter
// actually drives the bind address: forwarding with the host's non-loopback
// IPv4 must yield a Tunnel whose LocalAddr is that IP (not 127.0.0.1), and a
// client dialing that address round-trips bytes through the tunnel (real TCP
// path, not just the addr string). Bind/dial failure against an address the
// host owns but the environment restricts (firewall, address dropped) is a
// skip — the environment cannot exercise this case, the code path is the same
// as the loopback one.
func TestForwardLocalListenHostBindsNonLoopback(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	_, echoPort := startEchoService(t)

	ip := pickLocalNonLoopbackIP(t)
	tun, err := c.ForwardLocal(0, ip.String(), "127.0.0.1", echoPort, nil)
	if err != nil {
		t.Skipf("bind %s unavailable on this host: %v", ip, err)
	}
	defer tun.Close()
	if got := tun.LocalAddr(); !strings.HasPrefix(got, "["+ip.String()+"]") && !strings.HasPrefix(got, ip.String()+":") {
		t.Fatalf("LocalAddr = %q, want bind on %s", got, ip)
	}

	// Real byte path through the non-loopback bind: dial LocalAddr, echo.
	conn, err := net.DialTimeout("tcp", tun.LocalAddr(), 3*time.Second)
	if err != nil {
		t.Skipf("dial own non-loopback bind %s blocked on this host: %v", tun.LocalAddr(), err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatalf("write via %s: %v", tun.LocalAddr(), err)
	}
	buf := make([]byte, 4)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read echo via %s: %v", tun.LocalAddr(), err)
	}
	if string(buf[:n]) != "hi" {
		t.Fatalf("echo via %s = %q, want %q", tun.LocalAddr(), buf[:n], "hi")
	}
}

// TestForwardLocalActivityHookThrottled proves the onActivity callback is
// throttled to one real call per activityThrottle window: five sequential
// connections (each an Accept + reads in BOTH pipe directions = many activity
// events) within the 30s window must produce exactly ONE callback. The count
// is deterministic, not race-dependent: the FIRST event (conn1's Accept) runs
// in the serve goroutine and stores the window timestamp before any handle
// goroutine exists, so every later event lands inside the window.
func TestForwardLocalActivityHookThrottled(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	_, echoPort := startEchoService(t)

	var calls atomic.Int32
	tun, err := c.ForwardLocal(0, "127.0.0.1", "127.0.0.1", echoPort, func() { calls.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	// 连打 5 条连接(每条 Accept 触发一次活动上报,但 30s 节流内只该有 1 次回调)
	for i := 0; i < 5; i++ {
		conn, err := net.DialTimeout("tcp", tun.LocalAddr(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte("ping\n"))
		buf := make([]byte, 16)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Read(buf)
		_ = conn.Close()
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("throttle failed: %d callbacks in one 30s window, want 1", n)
	}
}

// TestTunnelSetOnActivity proves the post-open setter route (the Plan 35 T5
// manager-wiring path): a Tunnel opened with nil onActivity gets its callback
// attached via SetOnActivity after ForwardLocal returns, and a connection
// round-trip fires exactly one throttled callback through it.
func TestTunnelSetOnActivity(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	_, echoPort := startEchoService(t)

	tun, err := c.ForwardLocal(0, "127.0.0.1", "127.0.0.1", echoPort, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	var calls atomic.Int32
	tun.SetOnActivity(func() { calls.Add(1) })

	conn, err := net.DialTimeout("tcp", tun.LocalAddr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("hi"))
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(buf)
	_ = conn.Close()

	if n := calls.Load(); n != 1 {
		t.Fatalf("SetOnActivity callback: %d calls in one 30s window, want 1", n)
	}
}
