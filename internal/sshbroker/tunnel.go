package sshbroker

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

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

	closeOnce sync.Once
	closeErr  error
}

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

// ForwardLocal opens a local TCP listener on localPort (0 = a free port chosen
// by the kernel) and forwards each accepted connection to remoteHost:remotePort
// over the client's SSH connection — the `ssh -L 127.0.0.1:<localPort> →
// remoteHost:remotePort` semantic. Returns the Tunnel (whose LocalAddr carries
// the actual bound port; the caller needs it to dial in). The caller keeps the
// Tunnel alive and closes it when done.
//
// remoteHost:remotePort is resolved FROM THE SSH SERVER'S PERSPECTIVE (so
// "127.0.0.1" means the server's loopback, the typical case for reaching a
// service on the remote host). ForwardLocal itself binds the local listener on
// the broker host's loopback only (the agent reaches it via 127.0.0.1).
func (c *Client) ForwardLocal(localPort int, remoteHost string, remotePort int) (*Tunnel, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("local listen %s: %w", addr, err)
	}
	t := &Tunnel{
		ID:        uuid.NewString(),
		localAddr: ln.Addr().String(),
		listener:  ln,
		client:    c.c,
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
		go t.handle(local, remote)
	}
}

// closeWriter is implemented by *net.TCPConn (local side) and gossh's channel
// net.Conn (remote side) — CloseWrite sends EOF without tearing down the read
// half. Used by handle for directional half-close propagation.
type closeWriter interface{ CloseWrite() error }

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
		_, _ = io.Copy(rem, local)
		if cw, ok := rem.(closeWriter); ok {
			_ = cw.CloseWrite() // propagate local EOF toward the remote peer
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, rem)
		if cw, ok := local.(closeWriter); ok {
			_ = cw.CloseWrite() // propagate remote EOF toward the local peer
		}
		done <- struct{}{}
	}()
	<-done // wait for BOTH directions: half-close on one side must not kill the other
	<-done
}
