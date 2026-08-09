package eval

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const evalImageTag = "sshmgr-eval-sshd:local"

// requireEval skips unless SSHMGR_AGENT_EVAL=1, ANTHROPIC_API_KEY is set, and
// claude/docker/ssh-keygen are on PATH. Keeps the default fast-lane LLM-cost-free.
func requireEval(t *testing.T) {
	t.Helper()
	if os.Getenv("SSHMGR_AGENT_EVAL") != "1" {
		t.Skip("set SSHMGR_AGENT_EVAL=1 (and ANTHROPIC_API_KEY) to run agent eval — real LLM cost")
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("SSHMGR_AGENT_EVAL=1 set but ANTHROPIC_API_KEY missing (--bare needs an API key, not OAuth)")
	}
	for _, bin := range []string{"claude", "docker", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("eval needs %q on PATH: %v", bin, err)
		}
	}
}

// ensureImage builds the eval sshd image (idempotent; docker caches).
// `go test ./internal/eval/` runs with CWD = the package directory, where the Dockerfile lives.
func ensureImage(t *testing.T) {
	t.Helper()
	dir, _ := os.Getwd()
	if out, err := exec.Command("docker", "build", "-q", "-t", evalImageTag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", dir, err, out)
	}
}

// startEvalSSHD launches the eval sshd on a random loopback port.
// Returns host, port, the container id (for dockerExec end-state checks), and a
// cleanup func. The host key is regenerated per container start (see Dockerfile
// CMD); callers that need the key fetch it via dockerExec — the smoke test uses
// InsecureIgnoreHostKey, later broker wiring does TOFU through the store.
func startEvalSSHD(t *testing.T) (host string, port int, containerID string, cleanup func()) {
	t.Helper()
	ensureImage(t)
	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::22", evalImageTag).Output()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	kill := func() { _ = exec.Command("docker", "rm", "-f", id).Run() }

	portOut, err := exec.Command("docker", "port", id, "22").Output()
	if err != nil {
		kill()
		t.Fatalf("docker port: %v", err)
	}
	_, p, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(portOut), "\n")[0]))
	if err != nil {
		kill()
		t.Fatalf("parse port %q: %v", portOut, err)
	}
	fmt.Sscanf(p, "%d", &port)
	host = "127.0.0.1"

	// Readiness poll: docker port forwarding may accept the TCP dial before sshd
	// is actually listening, but the SSH handshake below is the real gate. The
	// hardening here is the fallthrough: if the port never opens at all within
	// the deadline (broken image / crashed sshd), we fail loudly instead of
	// silently continuing into a useless test. (Mirrors the conformance helper
	// with a `ready` flag added — conformance silently falls through.)
	ready := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 500*time.Millisecond); err == nil {
			c.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		kill()
		t.Fatalf("eval sshd never became ready on %s:%d", host, port)
	}
	return host, port, id, kill
}

// dockerExec runs a command in the eval container. id is the container id from
// startEvalSSHD; callers that need a shell pass cmd as a single sh -c string.
// Unused by the T1 smoke test (which connects over SSH) but kept for Phase 2
// end-state checks (sudo, file ownership, env) that read container state directly.
func dockerExec(t *testing.T, id, cmd string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", "exec", id, "sh", "-c", cmd).Output()
	return string(out), err
}
