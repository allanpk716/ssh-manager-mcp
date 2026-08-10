package conformance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runSSHBinary invokes the real `ssh` binary and returns separated stdout/stderr + exit code.
func runSSHBinary(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command("ssh", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("ssh binary failed to start: %v", err)
	} // err == nil → exitCode stays 0
	return out.String(), errb.String(), exitCode
}

// generateKey creates an OpenSSH-format keypair via ssh-keygen in a temp dir.
// keyType ∈ {rsa, ed25519, ecdsa}; passphrase may be "" for an unencrypted key.
// Returns the path to the private key file and the public key line (authorized_keys format).
func generateKey(t *testing.T, keyType, passphrase string) (privPath, pubLine string) {
	t.Helper()
	dir := t.TempDir()
	privPath = filepath.Join(dir, "id")
	args := []string{"-q", "-t", keyType, "-f", privPath, "-N", passphrase, "-C", "conformance"}
	if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -t %s: %v\n%s", keyType, err, out)
	}
	pub, err := os.ReadFile(privPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return privPath, strings.TrimSpace(string(pub))
}

// sshBinaryKeyAuthArgs assembles the common ssh-binary args for key-auth against the
// conformance sshd: batch mode, identity pinned, no host-key prompts, quiet stderr.
// The trailing element is the destination (`user@host`) so callers can append a
// remote command or insert forward flags (`-L`, `-N`) before it.
func sshBinaryKeyAuthArgs(host string, port int, user, privPath string) []string {
	return []string{
		"-p", strconv.Itoa(port),
		"-i", privPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		user + "@" + host,
	}
}

// scpBinaryKeyAuthArgs assembles the scp-binary args (note the uppercase -P, the
// one flag where scp diverges from ssh) for key-auth against the conformance sshd.
// Source and destination are POSITIONAL and must be appended by the caller in
// `[flags...] src dst` order — scp takes no destination flag. Shared -o options
// mirror sshBinaryKeyAuthArgs so both binaries authenticate identically.
func scpBinaryKeyAuthArgs(port int, privPath string) []string {
	return []string{
		"-P", strconv.Itoa(port),
		"-i", privPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
}
