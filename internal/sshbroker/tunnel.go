package sshbroker

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

// activityThrottle bounds how often the onActivity callback actually fires
// (spec §3): the read path stays lock- and allocation-free — one atomic
// compare per event, a real callback at most once per window.
const activityThrottle = 30 * time.Second

// Tunnel is a local TCP listener that forwards each accepted connection to a
// remote endpoint over an ssh.Client (the `ssh -L` forward). The Tunnel owns
// its listener; Close stops accepting new connections and closes the listener
// (in-flight connections finish their pipe). The Tunnel does NOT close the
// underlying ssh.Client — the caller (T4's TunnelManager) owns the SSH
// connection's lifetime and tears it down when the tunnel is unregistered.
//
// Tunnel.ID is a process-unique UUID v4 (github.com/google/uuid). The
// TunnelManager (T4) keys live tunnels by this ID for the MCP close_port tool,
// so uniqueness must survive close/reopen across a process's lifetime — a UUID
// guarantees that without state, where a counter would need monotonic care to
// avoid reuse. uuid was already an indirect dep (pulled by the MCP SDK); it is
// promoted to a direct require here.
type Tunnel struct {
	ID        string
	localAddr string
	listener  net.Listener
	client    *ssh.Client // the long-lived SSH connection the tunnel dials through

	onActivity func()       // nil = no hook; throttled activity signal for the manager's sweeper (spec §3)
	touchNano  atomic.Int64 // unix-nano of the last REAL callback (0 = never) — the throttle window start

	closeOnce sync.Once
	closeErr  error
}

// activity reports tunnel activity, throttled to one real callback per
// activityThrottle window (spec §3). Races between concurrent pipes can
// occasionally fire two callbacks at a window edge — harmless, the manager's
// Touch is idempotent. Hot path is lock- and allocation-free: one atomic
// load/compare per event, a store + call only when the window opens.
func (t *Tunnel) activity() {
	if t.onActivity == nil {
		return
	}
	now := time.Now().UnixNano()
	if now-t.touchNano.Load() < int64(activityThrottle) {
		return
	}
	t.touchNano.Store(now)
	t.onActivity()
}

// SetOnActivity attaches (or, with nil, clears) the activity callback after
// the tunnel is open. The TunnelManager wiring (Plan 35 T5) uses it to attach
// its Touch after mgr.Open; the ForwardLocal constructor param route stays
// for direct callers and tests. Plain assignment, no synchronization: it is
// meant to run before or between connections — the worst case of a racing
// swap is one extra or one missed throttled ping, which the idempotent
// manager Touch absorbs.
func (t *Tunnel) SetOnActivity(fn func()) { t.onActivity = fn }

// LocalAddr returns the tunnel's local listen address (e.g. "127.0.0.1:54321").
// Callers (the MCP forward_port tool reports it to the agent; the test dials it
// to send bytes through the tunnel).
func (t *Tunnel) LocalAddr() string { return t.localAddr }

// Close stops the accept loop and closes the local listener. Idempotent —
// repeated calls return the same error. It does NOT close the underlying
// ssh.Client: the caller (T4's TunnelManager) owns the SSH connection and
// closes it when the tunnel is torn down at the manager level (a single SSH
// connection may back multiple tunnels + Exec/SFTP ops).
//
// Goroutine lifecycle: Close → listener.Close → serve's Accept returns an error
// → serve returns (the single serve goroutine exits, no leak). In-flight handle
// goroutines continue until their pipe ends (their local conn is NOT closed by
// Close — see handle); when the peer closes they drain and exit.
func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() { t.closeErr = t.listener.Close() })
	return t.closeErr
}

// ForwardLocal opens a local TCP listener on listenHost:localPort (0 = a free
// port chosen by the kernel) and forwards each accepted connection to
// remoteHost:remotePort over the client's SSH connection — the `ssh -L
// listenHost:<localPort> → remoteHost:remotePort` semantic. Returns the Tunnel
// (whose LocalAddr carries the actual bound address:port; the caller needs it
// to dial in). The caller keeps the Tunnel alive and closes it when done.
//
// remoteHost:remotePort is resolved FROM THE SSH SERVER'S PERSPECTIVE (so
// "127.0.0.1" means the server's loopback, the typical case for reaching a
// service on the remote host). ForwardLocal binds the local listener on the
// given listenHost (validated by the caller's gate; loopback by default) —
// listenHost must be an already-VALIDATED IP literal, the mcpserver gate owns
// the policy (spec §2); IPv6 bracketing is handled by net.JoinHostPort.
//
// onActivity (optional, may be nil) receives throttled activity pings: on each
// accepted connection and on every read of both pipe directions, at most one
// REAL callback per activityThrottle window (spec §3). SetOnActivity attaches
// it post-open instead.
func (c *Client) ForwardLocal(localPort int, listenHost, remoteHost string, remotePort int, onActivity func()) (*Tunnel, error) {
	addr := net.JoinHostPort(listenHost, strconv.Itoa(localPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("local listen %s: %w", addr, err)
	}
	t := &Tunnel{
		ID:         uuid.NewString(),
		localAddr:  ln.Addr().String(),
		listener:   ln,
		client:     c.c,
		onActivity: onActivity,
	}
	go t.serve(remoteHost, remotePort)
	return t, nil
}

// serve accepts local connections and pipes each to the remote endpoint via the
// ssh client. It runs in its own goroutine for the Tunnel's lifetime and exits
// when Accept returns an error (only happens on listener Close — temporary
// errors are not possible on a TCP listener's Accept, so any error means close).
func (t *Tunnel) serve(remoteHost string, remotePort int) {
	remote := net.JoinHostPort(remoteHost, fmt.Sprintf("%d", remotePort))
	for {
		local, err := t.listener.Accept()
		if err != nil {
			return // listener closed (Close) — exit serve goroutine
		}
		t.activity() // new connection = activity (spec §3)
		go t.handle(local, remote)
	}
}

// closeWriter is implemented by *net.TCPConn (local side) and gossh's channel
// net.Conn (remote side) — CloseWrite sends EOF without tearing down the read
// half. Used by handle for directional half-close propagation.
type closeWriter interface{ CloseWrite() error }

// countingReader wraps one pipe direction's reader so every read that carries
// bytes reports tunnel activity (spec §3). Only reads with n>0 count — EOF and
// errors carry no data and are not activity.
type countingReader struct {
	r io.Reader
	t *Tunnel
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.t.activity()
	}
	return n, err
}

// handle pipes one local connection to its remote counterpart over the SSH
// connection. Both conns are defer-closed; the two io.Copy goroutines run in
// parallel, each propagating a directional EOF (CloseWrite) toward the peer it
// was writing to when its copy finishes, and the handle goroutine waits for
// BOTH directions to complete before the defers fire (the buffered `done`
// channel lets each copy send-and-exit without blocking, so no goroutine
// leaks).
//
// Half-close semantics (matches real `ssh -L`): when the local peer
// half-closes (TCPConn.CloseWrite — done writing, still reading), the
// local→remote copy sees EOF and forwards it as SSH_MSG_CHANNEL_EOF on the
// ssh channel (rem.CloseWrite), which the sshd turns into a TCP FIN toward
// the forwarded service — so a server that answers AFTER seeing end-of-request
// (HTTP/1.0 request-then-response, `cmd | ssh host filter` style pipes) still
// gets its in-flight response bytes back. The symmetric path applies for
// remote→local EOF (CloseWrite on the local conn). Only when BOTH directions
// finish do the defers fully close both conns.
func (t *Tunnel) handle(local net.Conn, remote string) {
	defer local.Close()
	rem, err := t.client.Dial("tcp", remote)
	if err != nil {
		return
	}
	defer rem.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(rem, &countingReader{r: local, t: t}) // local→remote bytes = activity
		if cw, ok := rem.(closeWriter); ok {
			_ = cw.CloseWrite() // propagate local EOF toward the remote peer
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, &countingReader{r: rem, t: t}) // remote→local bytes = activity
		if cw, ok := local.(closeWriter); ok {
			_ = cw.CloseWrite() // propagate remote EOF toward the local peer
		}
		done <- struct{}{}
	}()
	<-done // wait for BOTH directions: half-close on one side must not kill the other
	<-done
}
