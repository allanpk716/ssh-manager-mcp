package conformance

import (
	"bytes"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestInteropMatrix proves the broker's Go SSH client authenticates against real
// OpenSSH sshd across the MVP auth surface (scope: no SSH-CA; KEX via defaults).
func TestInteropMatrix(t *testing.T) {
	requireConformance(t)

	// Pre-generate one key per type so the matrix can authorize them all up front.
	rsaPriv, rsaPub := generateKey(t, "rsa", "")
	edPriv, edPub := generateKey(t, "ed25519", "")
	ecdsaPriv, ecdsaPub := generateKey(t, "ecdsa", "")
	encPriv, encPub := generateKey(t, "ed25519", "secret-pass")
	allKeys := strings.Join([]string{rsaPub, edPub, ecdsaPub, encPub}, "\n")

	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: allKeys})
	defer cleanup()

	type cas struct {
		name     string
		auth     ssh.AuthMethod
		marker   string
		exitCode int
	}
	cases := []cas{
		{"password", sshbroker.PasswordAuth("testpw123"), "pw-ok", 0},
		{"bare-rsa", mustPrivAuth(t, rsaPriv, ""), "rsa-ok", 0},
		{"bare-ed25519", mustPrivAuth(t, edPriv, ""), "ed-ok", 0},
		{"bare-ecdsa", mustPrivAuth(t, ecdsaPriv, ""), "ecdsa-ok", 0},
		{"encrypted-ed25519", mustPrivAuth(t, encPriv, "secret-pass"), "enc-ok", 0},
		{"wrong-password-rejected", sshbroker.PasswordAuth("nope"), "", 255}, // connect fails
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cb := ssh.FixedHostKey(hostKey) // trust the known host key
			cli, err := sshbroker.Connect(host, port, "sshuser", c.auth, cb)
			if c.marker == "" {
				// Expect auth failure.
				if err == nil {
					cli.Close()
					t.Fatal("expected connect to fail, succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer cli.Close()

			res, err := cli.Exec("printf %s "+c.marker, 0, 0)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if res.ExitCode != c.exitCode {
				t.Fatalf("exit = %d, want %d", res.ExitCode, c.exitCode)
			}
			if res.Stdout != c.marker {
				t.Fatalf("stdout = %q, want %q", res.Stdout, c.marker)
			}
		})
	}
}

// TestInteropRealSudo proves ExecSudo (sudo -S, password on stdin) runs a privileged
// command against REAL sudo (closes the Plan-2 gap where testsshd did not strictly
// check the sudo password). The conformance user requires a password for sudo.
func TestInteropRealSudo(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	cli, err := sshbroker.Connect(host, port, "sshuser", mustPrivAuth(t, privPath, ""), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	// Real sudo here requires a password; broker feeds it via sudo -S.
	res, err := cli.ExecSudo("whoami", []byte("testpw123"), 0, 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "root" {
		t.Fatalf("sudo whoami stdout = %q, want root", res.Stdout)
	}
	// A wrong sudo password must NOT escalate.
	resBad, _ := cli.ExecSudo("whoami", []byte("wrong-sudo-pw"), 0, 0)
	if bytes.Contains([]byte(resBad.Stdout), []byte("root")) {
		t.Fatalf("wrong sudo password escalated; stdout=%q", resBad.Stdout)
	}
}
