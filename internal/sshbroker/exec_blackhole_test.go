package sshbroker

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

// Blackhole proxy fixture — Plan 52 spec (.scratch/exec-timeout-conn-kill),
// Testing Decisions layer 2.
//
// The BLACKHOLE SHAPE is what an SSH client sees against the uncooperative
// server from the 4090x2 incident (OpenSSH 9.6p1 / Ubuntu 24.04): after the
// timeout fires, the SIGKILL signal request and the SSH_MSG_CHANNEL_CLOSE of
// watchdog stage ① go out and are never answered, the TCP connection never
// dies, and a blocked session Wait stays blocked until the WHOLE connection is
// torn down (watchdog stage ③). The in-process testsshd cannot produce this
// shape naturally — the golang.org/x/crypto/ssh server library echoes channel
// closes back automatically — so this fixture simulates it with a TCP proxy:
// it forwards bytes between one client and testsshd until blackout(), then
// stops forwarding in BOTH directions while keeping the two TCP connections
// open. To the client that is exactly "the close message went out and no echo
// will ever come".
//
// Non-vacuity (why these tests pin stage ③ and nothing weaker): in
// x/crypto/ssh v0.41.0 the client-side (*channel).Close() only SENDS
// channelCloseMsg to the peer — it does not close the local read pipes — so
// stage ① cannot unblock a blocked sess.Wait when the server never replies
// (checked in the module cache; this matches the real-machine hang that only
// a TCP teardown unlocked). With stage ③ removed, Exec never returns and the
// 10-second external deadline in boundedExec fails the test RED instead of
// hanging the run.
//
// Output shape of testsshd: its Exec callback returns (stdout, stderr) ONCE,
// written atomically after the callback returns — no output can stream while
// the callback blocks. The "identifiable text before the block" requirement is
// therefore realized as a PRE-blackout Exec whose output is asserted intact
// (the sanctioned fallback in the ticket), plus a void-after-blackout
// assertion on the blocking Exec itself.
type blackholeProxy struct {
	mu   sync.Mutex
	dead bool
	wg   sync.WaitGroup
	cli  net.Conn // proxy ↔ client side
	srv  net.Conn // proxy ↔ testsshd side
	ln   net.Listener
}

// startBlackholeProxy listens on 127.0.0.1:0, accepts ONE client connection,
// dials target (the testsshd address), and pumps bytes both ways until
// blackout(). Cleanup (registered here) stops the pumps and closes the
// listener plus both proxy-side connections, so no goroutine outlives the
// test. The client must use the proxy's Addr() as its dial address; the
// end-to-end host-key check still sees testsshd's key (the proxy is
// byte-transparent pre-blackout).
func startBlackholeProxy(t *testing.T, target string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("blackhole proxy listen: %v", err)
	}
	p := &blackholeProxy{ln: ln}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed at cleanup
		}
		s, err := net.Dial("tcp", target)
		if err != nil {
			c.Close()
			return
		}
		p.mu.Lock()
		p.cli, p.srv = c, s
		p.mu.Unlock()
		p.wg.Add(2)
		go p.pump(c, s) // client → testsshd
		go p.pump(s, c) // testsshd → client
	}()
	t.Cleanup(func() {
		p.blackout()
		ln.Close()
		p.mu.Lock()
		cli, srv := p.cli, p.srv
		p.mu.Unlock()
		if cli != nil {
			cli.Close()
		}
		if srv != nil {
			srv.Close()
		}
		p.wg.Wait()
	})
	return p
}

// Addr returns the client-facing dial address of the proxy.
func (p *blackholeProxy) Addr() string { return p.ln.Addr().String() }

// blackout stops both forwarding loops (each exits within one read deadline)
// and leaves both TCP connections open on purpose — closing them would hand
// the client the very unlock (connection death) this fixture must withhold.
// Bytes in flight at blackout time: none, in these tests — blackout fires
// while the remote command is parked inside its callback sleep, with no
// traffic moving in either direction until the watchdog's stage ① (which the
// stopped loops never read).
func (p *blackholeProxy) blackout() {
	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()
}

// pump copies src → dst until blackout() marks the proxy dead (the loop then
// exits WITHOUT closing either conn) or until src errors for real (EOF/reset
// — only possible pre-blackout; after blackout the loop no longer reads).
// The bounded read deadline makes every iteration poll the dead flag, so a
// quiet connection cannot pin the goroutine.
func (p *blackholeProxy) pump(dst, src net.Conn) {
	defer p.wg.Done()
	buf := make([]byte, 16*1024)
	for {
		p.mu.Lock()
		dead := p.dead
		p.mu.Unlock()
		if dead {
			return
		}
		_ = src.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
	}
}

// connectVia is connectTest pointed at the PROXY address (connectTest dials
// the testsshd address directly). Same no-keepalive Connect product, same
// fixed host key — end-to-end through the byte-transparent proxy.
func connectVia(t *testing.T, addr string, hostKey ssh.PublicKey) *Client {
	t.Helper()
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect via blackhole proxy: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

// execOutcome is what boundedExec reports back to the test goroutine.
type execOutcome struct {
	res     ExecResult
	err     error
	elapsed time.Duration
}

// boundedExec runs fn on a goroutine and bounds it with a 10-second external
// deadline: if watchdog stage ③ (whole-connection close) ever fails to unblock
// the kernel against the blackhole server, the test fails HERE — red, not
// hung (spec: 修前代码必红而非挂死). t.Fatal is only ever called from the test
// goroutine; the worker only sends on the channel.
func boundedExec(t *testing.T, name string, fn func() (ExecResult, error)) execOutcome {
	t.Helper()
	ch := make(chan execOutcome, 1)
	go func() {
		start := time.Now()
		res, err := fn()
		ch <- execOutcome{res: res, err: err, elapsed: time.Since(start)}
	}()
	select {
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: did not return within the 10s external deadline — stage-3 connection close failed to unblock the wait against the blackhole server", name)
		return execOutcome{}
	case oc := <-ch:
		return oc
	}
}

// TestExecTimeoutEscalatesToConnClose proves watchdog stage ③ against the
// blackhole shape: the blocking command IS running server-side when the wire
// goes dark, the 300ms timeout's stage ① teardown (SIGKILL + channel close) is
// emitted into a void that never echoes, and only the connection kill unlocks
// Exec — bounded, TimedOut=true, and no connection-death error
// (ExitMissingError / io.ErrClosedPipe) leaking out.
func TestExecTimeoutEscalatesToConnClose(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms") // read at connection construction — must precede connectVia
	started := make(chan struct{})              // closed when the blocking command is in-flight server-side
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			switch cmd {
			case "hello":
				return "out:hello\n", "", 0
			default: // "slow"
				close(started) // safe to cut the wire: command parked, output 2s away
				time.Sleep(2 * time.Second)
				return "done\n", "", 0
			}
		},
	})
	defer cleanup()
	p := startBlackholeProxy(t, addr)
	c := connectVia(t, p.Addr(), hk)

	// Pre-blackout round-trip: the proxy is transparent, this Exec must be
	// unaffected — and it carries the "identifiable text before the block"
	// assertion in the ticket's sanctioned fallback form (testsshd's callback
	// returns stdout atomically, so nothing can stream during the block).
	res, err := c.Exec(context.Background(), "hello", 0, 0)
	if err != nil || res.Stdout != "out:hello\n" {
		t.Fatalf("pre-blackout exec through the proxy: res=%+v err=%v", res, err)
	}

	// Cut the wire the moment the blocking command is confirmed in-flight —
	// every teardown reply the watchdog could wait for is now blackholed.
	go func() {
		<-started
		p.blackout()
	}()

	oc := boundedExec(t, "Exec", func() (ExecResult, error) {
		return c.Exec(context.Background(), "slow", 300*time.Millisecond, 0)
	})

	if oc.err != nil {
		t.Fatalf("timeout must surface as a result, not an error (ExitMissingError / io.ErrClosedPipe from the stage-3 connection kill must not leak): %v", oc.err)
	}
	if !oc.res.TimedOut {
		t.Fatalf("expected TimedOut=true, got %+v", oc.res)
	}
	// 300ms timeout + 300ms grace + scheduler slack; the command needs 2s and
	// the blackholed channel close never confirms — a bounded return here can
	// only come from the connection kill.
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("Exec returned after %v, want bounded well under 5s", oc.elapsed)
	}
	if strings.Contains(oc.res.Stdout, "done") {
		t.Fatal("blackholed server output must not have arrived")
	}
}

// TestExecSudoTimeoutEscalatesToConnClose is the elevated-path twin of
// TestExecTimeoutEscalatesToConnClose: same fixture, same blackout gating, the
// blocking command behind ExecSudo's sudo -S wrapper (password fed through the
// proxy pre-blackout). Same contract: bounded return, TimedOut=true, no error
// leak.
func TestExecSudoTimeoutEscalatesToConnClose(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms")
	started := make(chan struct{})
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			switch cmd {
			case "whoami":
				return "root\n", "", 0
			default: // "slow"
				close(started) // server already consumed the password line (it reads it before calling this)
				time.Sleep(2 * time.Second)
				return "done\n", "", 0
			}
		},
	})
	defer cleanup()
	p := startBlackholeProxy(t, addr)
	c := connectVia(t, p.Addr(), hk)

	// Pre-blackout elevated round-trip (output-preservation assertion, fallback
	// form — same reasoning as the plain-Exec test).
	res, err := c.ExecSudo(context.Background(), "whoami", []byte("sudopw"), 0, 0)
	if err != nil || strings.TrimSpace(res.Stdout) != "root" {
		t.Fatalf("pre-blackout ExecSudo through the proxy: res=%+v err=%v", res, err)
	}

	go func() {
		<-started
		p.blackout()
	}()

	oc := boundedExec(t, "ExecSudo", func() (ExecResult, error) {
		return c.ExecSudo(context.Background(), "slow", []byte("sudopw"), 300*time.Millisecond, 0)
	})

	if oc.err != nil {
		t.Fatalf("timeout must surface as a result, not an error: %v", oc.err)
	}
	if !oc.res.TimedOut {
		t.Fatalf("expected TimedOut=true, got %+v", oc.res)
	}
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("ExecSudo returned after %v, want bounded well under 5s", oc.elapsed)
	}
	if strings.Contains(oc.res.Stdout, "done") {
		t.Fatal("blackholed server output must not have arrived")
	}
}

// TestExecCancelEscalatesToConnClose pins the caller-cancellation path against
// the blackhole shape: cancel while the command is in flight (the 100ms of the
// cooperative TestExecCancelContext is far more than the exec request needs to
// cross the still-live proxy), stage ① is blackholed like in the timeout
// tests, and the cancellation must surface as context.Canceled (not
// TimedOut) with a bounded return.
func TestExecCancelEscalatesToConnClose(t *testing.T) {
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms")
	started := make(chan struct{})
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			close(started)
			time.Sleep(30 * time.Second) // in-flight; only the teardown can end this
			return "done\n", "", 0
		},
	})
	defer cleanup()
	p := startBlackholeProxy(t, addr)
	c := connectVia(t, p.Addr(), hk)

	go func() {
		<-started
		p.blackout()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	oc := boundedExec(t, "Exec(cancel)", func() (ExecResult, error) {
		return c.Exec(ctx, "slow", 0, 0) // timeout=0 → only the ctx cancel can fire
	})

	if !errors.Is(oc.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", oc.err)
	}
	if oc.res.TimedOut {
		t.Fatal("TimedOut=true on cancel, want false (cancel ≠ timeout)")
	}
	// 100ms to cancel + 300ms grace + scheduler slack, against a 30s command.
	if oc.elapsed >= 5*time.Second {
		t.Fatalf("Exec returned after %v on cancel, want bounded well under 5s", oc.elapsed)
	}
}
