package testsshd

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"strings"
	"testing"
	"time"

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
			go serve(conn, cfg, signer, opts.SudoPassword, execFn)
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

func serve(c net.Conn, cfg *ssh.ServerConfig, signer ssh.Signer, sudoPw string, execFn func(string, io.Reader) (string, string, int)) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		go handleSession(newChan, sudoPw, execFn)
	}
}

func handleSession(newChan ssh.NewChannel, sudoPw string, execFn func(string, io.Reader) (string, string, int)) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
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
	}
}
