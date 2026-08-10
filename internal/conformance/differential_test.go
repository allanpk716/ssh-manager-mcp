package conformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// fakeHostKeyStore is a minimal sshbroker.HostKeyStore keyed by "host:port",
// used to drive the broker's TOFU callback without a real store. Mirrors the
// one in internal/sshbroker/hostkey_test.go.
type fakeHostKeyStore struct{ keys map[string][]byte }

func (f *fakeHostKeyStore) GetHostKey(host string, port int) ([]byte, error) {
	return f.keys[fmt.Sprintf("%s:%d", host, port)], nil
}

func (f *fakeHostKeyStore) SaveHostKey(host string, port int, k []byte) error {
	if f.keys == nil {
		f.keys = map[string][]byte{}
	}
	f.keys[fmt.Sprintf("%s:%d", host, port)] = k
	return nil
}

// sshBinaryKnownHostsLine renders an OpenSSH known_hosts entry for a non-default
// port: "[host]:port <keytype> <base64>". The bracketed hostname is OpenSSH's
// format when the port is not 22 (see known_hosts PROTOCOL).
func sshBinaryKnownHostsLine(host string, port int, key ssh.PublicKey) string {
	return "[" + host + "]:" + strconv.Itoa(port) + " " + key.Type() + " " +
		base64.StdEncoding.EncodeToString(key.Marshal())
}

// TestDifferentialParity runs identical commands through the broker's Go SSH
// client and the real `ssh` binary against the same sshd, asserting that
// stdout, stderr, and exit code match exactly. Zero differential = the broker
// is consistent with the industry-standard client (§13.2). Timeouts and >1 MiB
// truncation are broker-specific (no ssh-binary counterpart) and are excluded
// from this differential per scope decision 3.
func TestDifferentialParity(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	brokerAuth := mustPrivAuth(t, privPath, "")
	sshArgs := sshBinaryKeyAuthArgs(host, port, "sshuser", privPath)

	type scenario struct {
		name string
		cmd  string // remote command, identical for both paths
	}
	scenarios := []scenario{
		{"normal-exec", "printf %s out123"},
		{"exit-code-7", "sh -c 'exit 7'"},
		{"stderr-only", "printf %s err-on-stderr 1>&2"},
		{"large-output", "seq 1 2000"}, // ~9 KiB, well under the 1 MiB truncation threshold
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			// Broker path (Go SSH client).
			cli, err := sshbroker.Connect(host, port, "sshuser", brokerAuth, ssh.FixedHostKey(hostKey))
			if err != nil {
				t.Fatalf("broker connect: %v", err)
			}
			defer cli.Close()
			bRes, err := cli.Exec(context.Background(), sc.cmd, 0, 0) // unlimited — differential tests SSH parity, not truncation
			if err != nil {
				t.Fatalf("broker exec: %v", err)
			}

			// ssh-binary path.
			sOut, sErr, sCode := runSSHBinary(t, append(append([]string{}, sshArgs...), sc.cmd)...)

			if bRes.Stdout != sOut {
				t.Errorf("stdout diff:\nbroker=%q\nssh   =%q", bRes.Stdout, sOut)
			}
			if bRes.Stderr != sErr {
				t.Errorf("stderr diff:\nbroker=%q\nssh   =%q", bRes.Stderr, sErr)
			}
			if bRes.ExitCode != sCode {
				t.Errorf("exit diff: broker=%d ssh=%d", bRes.ExitCode, sCode)
			}
		})
	}
}

// TestDifferentialHostKeyRejection asserts BOTH the broker and the real `ssh`
// binary refuse a server whose host key differs from the trusted one. The
// broker path pre-trusts container A's key under hostB:portB (via a fake
// HostKeyStore + HostKeyTOFU) and must return ErrHostKeyMismatch on connect to
// B; the ssh-binary path writes A's key to a known_hosts entry under
// [hostB]:portB and must exit nonzero under StrictHostKeyChecking=yes.
func TestDifferentialHostKeyRejection(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")

	// Two independent sshd containers (each generates its own ed25519 host key).
	_, _, keyA, _, cleanupA := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanupA()
	hostB, portB, keyB, _, cleanupB := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanupB()

	// Sanity: independent containers must produce distinct host keys, otherwise
	// the reject scenario is meaningless.
	if bytes.Equal(keyA.Marshal(), keyB.Marshal()) {
		t.Fatal("two independent sshd containers produced the same host key; test setup is broken")
	}

	// Broker: pre-trust keyA under hostB:portB, then connect to B → mismatch.
	st := &fakeHostKeyStore{keys: map[string][]byte{
		fmt.Sprintf("%s:%d", hostB, portB): keyA.Marshal(),
	}}
	cb, _ := sshbroker.HostKeyTOFU(st, hostB, portB)
	_, err := sshbroker.Connect(hostB, portB, "sshuser", mustPrivAuth(t, privPath, ""), cb)
	if err == nil {
		t.Fatal("broker accepted a mismatched host key")
	}
	if !errors.Is(err, sshbroker.ErrHostKeyMismatch) {
		t.Fatalf("broker rejection must wrap ErrHostKeyMismatch, got %v", err)
	}

	// ssh binary: known_hosts with keyA under [hostB]:portB, strict checking → reject.
	kh := filepath.Join(t.TempDir(), "known_hosts")
	line := sshBinaryKnownHostsLine(hostB, portB, keyA)
	if err := os.WriteFile(kh, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-p", strconv.Itoa(portB),
		"-i", privPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + kh,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "LogLevel=ERROR",
		"sshuser@" + hostB,
		"true",
	}
	_, _, code := runSSHBinary(t, args...)
	if code == 0 {
		t.Fatal("ssh binary accepted a mismatched host key")
	}
}
