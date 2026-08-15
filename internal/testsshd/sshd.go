package testsshd

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Options configures the in-process test SSH server.
type Options struct {
	Password      string        // accept this password (empty = no password auth)
	AuthorizedKey ssh.PublicKey // accept this client public key (nil = no key auth)
	SudoPassword  string        // if set, Exec handler simulates sudo -S (reads pw from stdin)
	Exec          func(cmd string, stdin io.Reader) (stdout, stderr string, exitCode int)
}

// Start launches an in-process SSH server on a random local port.
// Returns host:port, the server's host public key, and a cleanup func.
// Teardown note: cleanup() closes only the listener; in-flight sessions drain when
// the client disconnects. Callers MUST `defer cleanup()` BEFORE `defer cli.Close()`
// (LIFO) so the client closes first and server goroutines exit cleanly.
func Start(t *testing.T, opts Options) (string, ssh.PublicKey, func()) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{}
	cfg.AddHostKey(signer)
	if opts.Password != "" {
		pw := opts.Password
		cfg.PasswordCallback = func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == pw {
				return nil, nil
			}
			return nil, io.EOF
		}
	}
	if opts.AuthorizedKey != nil {
		allowed := opts.AuthorizedKey.Marshal()
		cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, pub ssh.PublicKey) (*ssh.Permissions, error) {
			if bytesEqual(pub.Marshal(), allowed) {
				return nil, nil
			}
			return nil, io.EOF
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	execFn := opts.Exec
	if execFn == nil {
		execFn = func(string, io.Reader) (string, string, int) { return "", "", 0 }
	}
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(stop)
				return
			}
			go serve(conn, cfg, opts.SudoPassword, execFn)
		}
	}()
	cleanup := func() {
		ln.Close()
		select {
		case <-stop:
		case <-time.After(2 * time.Second):
		}
	}
	return ln.Addr().String(), signer.PublicKey(), cleanup
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := range a {
		d |= a[i] ^ b[i]
	}
	return d == 0
}

func serve(c net.Conn, cfg *ssh.ServerConfig, sudoPw string, execFn func(string, io.Reader) (string, string, int)) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go handleSession(newChan, sudoPw, execFn)
		case "direct-tcpip":
			// Direct TCP/IP forwarding (RFC 4254 §7.2) — the server-side of `ssh -L`.
			// The broker's ForwardLocal opens this channel via (*ssh.Client).Dial; the
			// test sshd resolves the destination from the host's perspective (the
			// in-process sshd shares the test's loopback, so forwarding to
			// 127.0.0.1:<echoPort> reaches the test's echo service). No Options flag —
			// a real sshd supports -L by default, and this is purely additive (existing
			// session-only tests are unaffected).
			go handleDirectTCP(newChan)
		default:
			newChan.Reject(ssh.UnknownChannelType, "only session or direct-tcpip")
		}
	}
}

func handleSession(newChan ssh.NewChannel, sudoPw string, execFn func(string, io.Reader) (string, string, int)) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			cmd := payload.Command
			stdin := io.Reader(ch)
			// If this is a sudo -S invocation and the server is simulating sudo,
			// read the password line from stdin first, then run the inner command.
			if strings.HasPrefix(cmd, "sudo -S") && sudoPw != "" {
				buf := make([]byte, 0, 256)
				one := make([]byte, 1)
				for {
					if _, err := stdin.Read(one); err != nil {
						break
					}
					if one[0] == '\n' {
						break
					}
					buf = append(buf, one[0])
				}
				cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "sudo -S -p '' --"))
			}
			stdout, stderr, exit := execFn(cmd, stdin)
			if stdout != "" {
				ch.Write([]byte(stdout))
			}
			if stderr != "" {
				ch.Stderr().Write([]byte(stderr))
			}
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(exit)}))
			return
		case "subsystem":
			var sub struct{ Subsystem string }
			if err := ssh.Unmarshal(req.Payload, &sub); err != nil {
				req.Reply(false, nil)
				continue
			}
			if sub.Subsystem != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			// Serve SFTP over this channel against the host filesystem. The
			// in-process testsshd shares this OS process's FS, so the client can
			// read/write real paths (the broker download test writes a fixture
			// under t.TempDir() and Downloads it). Serve blocks until the client
			// closes the channel; returning then lets `defer ch.Close()` finish it.
			// Using sftp.NewServer (the canonical host-FS server) instead of the
			// brief's NewRequestServer+NativeFilesystem: NativeFilesystem was
			// removed from pkg/sftp; NewServer is its supported replacement.
			srv, err := sftp.NewServer(ch)
			if err != nil {
				return
			}
			_ = srv.Serve()
			_ = srv.Close()
			return
		default:
			req.Reply(false, nil)
			continue
		}
	}
}

// directTCPPayload is the channel-open extra data for a "direct-tcpip" channel
// (RFC 4254 §7.2): the host:port to connect to, plus the originator's address.
// x/crypto/ssh exposes these fields via NewChannel.ExtraData() (the bytes after
// the standard SSH_MSG_CHANNEL_OPEN header).
type directTCPPayload struct {
	Addr     string // host to connect
	Port     uint32 // port to connect
	OrigAddr string // originator IP (informational)
	OrigPort uint32 // originator port (informational)
}

// closeWriter is implemented by *net.TCPConn (dialed side) and ssh.Channel
// (client side) — CloseWrite sends EOF without tearing down the read half.
// Mirror of sshbroker's closeWriter (test infra; the broker cannot export it
// without widening its API, so the trivial interface is duplicated here).
type closeWriter interface{ CloseWrite() error }

// handleDirectTCP accepts a "direct-tcpip" channel, dials the requested
// host:port from the sshd's perspective (the host's loopback for the in-process
// testsshd), and pipes both ways until BOTH directions complete. Mirrors the
// broker's Tunnel.handle symmetrically, including directional half-close: when
// one copy direction sees EOF, it CloseWrites the conn it was writing to (the
// dialed TCP conn gets a FIN; the channel sends SSH_MSG_CHANNEL_EOF) instead of
// closing everything — so half-close propagates through the test link the same
// way it does through the broker tunnel (see TestTunnelHalfClosePropagates).
// ssh.Channel itself implements CloseWrite; the dialed conn is *net.TCPConn.
func handleDirectTCP(newChan ssh.NewChannel) {
	var p directTCPPayload
	if err := ssh.Unmarshal(newChan.ExtraData(), &p); err != nil {
		newChan.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)
	dest := net.JoinHostPort(p.Addr, strconv.Itoa(int(p.Port)))
	remote, err := net.Dial("tcp", dest)
	if err != nil {
		return // reject-by-close: the channel Accept already succeeded; Close signals failure to the client
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, ch)
		if cw, ok := remote.(closeWriter); ok {
			_ = cw.CloseWrite() // channel EOF from the client → FIN toward the service
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(ch, remote)
		if cw, ok := ch.(closeWriter); ok {
			_ = cw.CloseWrite() // service FIN → channel EOF toward the client
		}
		done <- struct{}{}
	}()
	<-done // wait for BOTH directions (mirror of Tunnel.handle)
	<-done
}
