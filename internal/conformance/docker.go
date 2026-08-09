package conformance

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const imageTag = "sshmgr-conformance-sshd:local"

// requireConformance skips unless SSHMGR_CONFORMANCE=1 and docker/ssh/ssh-keygen are on PATH.
// This keeps the default fast-lane `go test ./...` docker-free (spec §12.4).
func requireConformance(t *testing.T) {
	t.Helper()
	if os.Getenv("SSHMGR_CONFORMANCE") != "1" {
		t.Skip("set SSHMGR_CONFORMANCE=1 to run OpenSSH conformance tests (needs docker + ssh + ssh-keygen)")
	}
	for _, bin := range []string{"docker", "ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("conformance needs %q on PATH: %v", bin, err)
		}
	}
}

// OpenSSHOpts configures the conformance sshd container.
type OpenSSHOpts struct {
	AuthorizedPubKey string // OpenSSH-format public key line to authorize; "" = password-only
}

// ensureImage builds the conformance sshd image (idempotent; docker caches).
func ensureImage(t *testing.T) {
	t.Helper()
	dir, err := packageDir()
	if err != nil {
		t.Fatalf("packageDir: %v", err)
	}
	cmd := exec.Command("docker", "build", "-q", "-t", imageTag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", dir, err, out)
	}
}

// startOpenSSH launches a real OpenSSH sshd in Docker on a random loopback port.
// Returns host, port, the container's ed25519 host public key, its id, and a cleanup func.
func startOpenSSH(t *testing.T, opts OpenSSHOpts) (host string, port int, hostKey ssh.PublicKey, containerID string, cleanup func()) {
	t.Helper()
	ensureImage(t)

	args := []string{"run", "-d", "--rm", "-p", "127.0.0.1::22"}
	if opts.AuthorizedPubKey != "" {
		dir := t.TempDir()
		authFile := dir + "/authorized_keys"
		if err := os.WriteFile(authFile, []byte(strings.TrimSpace(opts.AuthorizedPubKey)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-v", authFile+":/home/sshuser/.ssh/authorized_keys:ro")
	}
	args = append(args, imageTag)

	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	containerID = strings.TrimSpace(string(out))

	// Resolve the random host port.
	portOut, err := exec.Command("docker", "port", containerID, "22").Output()
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("docker port: %v", err)
	}
	portLine := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	_, p, err := net.SplitHostPort(portLine)
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("parse port %q: %v", portLine, err)
	}
	if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
		dockerKill(containerID)
		t.Fatalf("parse port int %q: %v", p, err)
	}
	host = "127.0.0.1"

	// Wait for sshd to accept TCP connections (container start is async).
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 500*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Retrieve the container's ed25519 host public key.
	pubOut, err := exec.Command("docker", "exec", containerID, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").Output()
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("cat host key: %v", err)
	}
	// ParseAuthorizedKey takes an authorized_keys line (algo + base64 + comment),
	// which is exactly what `cat *.pub` produces. (ssh.ParsePublicKey expects the
	// length-prefixed wire format and would return "short read" here.)
	hostKey, _, _, _, err = ssh.ParseAuthorizedKey(pubOut)
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("parse host key: %v", err)
	}

	cleanup = func() { dockerKill(containerID) }
	return host, port, hostKey, containerID, cleanup
}

func dockerKill(id string) {
	_ = exec.Command("docker", "rm", "-f", id).Run()
}

// packageDir returns this package's directory (the Dockerfile lives alongside).
// `go test ./internal/conformance/` runs with CWD = the package directory.
func packageDir() (string, error) {
	return os.Getwd()
}
