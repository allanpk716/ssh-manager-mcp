package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// inContainerEchoPort is the loopback port (inside the conformance container)
// where TestForwardDifferential runs a persistent nc-cat echo service. The sshd
// in the container dials it when a `-L ...:127.0.0.1:inContainerEchoPort` forward
// is opened (broker ForwardLocal OR real `ssh -L`) — so both paths reach the
// same echo service and the differential byte round-trip is apples-to-apples.
const inContainerEchoPort = 9123

// TestUploadDifferential proves the broker's recursive Upload produces a remote
// tree byte-identical to what real `scp -r` of the same local tree produces
// against the same real-OpenSSH sshd — the §13 differential for the upload
// surface. The broker path uses sshbroker.Client.Upload (SFTP put, single +
// recursive dir); the reference path uses the real `scp` binary (os/exec). The
// two remote trees are compared remotely via `ssh diff -r` (busybox diff on the
// alpine conformance container). Zero differential = the broker's upload
// matches the industry-standard client. Covers: single-file upload, recursive
// dir upload with nested subdirs + edge content (empty file, all-256-bytes
// binary) — enough to exercise directory recursion + non-UTF8 content.
func TestUploadDifferential(t *testing.T) {
	requireConformance(t)
	if _, err := exec.LookPath("scp"); err != nil {
		t.Skipf("upload-differential needs scp on PATH: %v", err)
	}
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	brokerAuth := mustPrivAuth(t, privPath, "")
	sshArgs := sshBinaryKeyAuthArgs(host, port, "sshuser", privPath)
	scpArgs := scpBinaryKeyAuthArgs(port, privPath)
	scpDst := "sshuser@" + host + ":"

	cli, err := sshbroker.Connect(context.Background(), host, port, "sshuser", brokerAuth, ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("broker connect: %v", err)
	}
	defer cli.Close()

	// remoteDiff runs `ssh ... 'diff [-r] A B && echo DIFF-OK'` against the
	// conformance sshd and fails the test unless the two remote paths are
	// byte-identical (diff exits 0 → DIFF-OK printed).
	remoteDiff := func(t *testing.T, recursive bool, a, b string) {
		t.Helper()
		flag := "diff"
		if recursive {
			flag = "diff -r"
		}
		cmd := fmt.Sprintf("%s '%s' '%s' && echo DIFF-OK", flag, a, b)
		out, _, code := runSSHBinary(t, append(append([]string{}, sshArgs...), cmd)...)
		if code != 0 || !strings.Contains(out, "DIFF-OK") {
			t.Fatalf("remote diff (code=%d):\n%s", code, out)
		}
	}

	// scpSingle + scpDir run the real scp binary (os/exec) with the local path
	// normalized to forward slashes — Windows-broker → Linux-server is the primary
	// deployment, and MSYS scp on the broker host accepts forward-slash local
	// paths unambiguously (a leading-drive `C:/...` form is never re-interpreted).
	scpSingle := func(t *testing.T, localFile, remoteFile string) {
		t.Helper()
		args := append(append([]string{}, scpArgs...), filepath.ToSlash(localFile), scpDst+remoteFile)
		if out, err := exec.Command("scp", args...).CombinedOutput(); err != nil {
			t.Fatalf("scp single: %v\n%s", err, out)
		}
	}
	scpDir := func(t *testing.T, localDir, remoteDir string) {
		t.Helper()
		args := append(append([]string{}, scpArgs...), "-r", filepath.ToSlash(localDir), scpDst+remoteDir)
		if out, err := exec.Command("scp", args...).CombinedOutput(); err != nil {
			t.Fatalf("scp -r: %v\n%s", err, out)
		}
	}

	t.Run("single-file", func(t *testing.T) {
		localFile := filepath.Join(t.TempDir(), "single.dat")
		content := []byte("single-file-differential-payload\n")
		if err := os.WriteFile(localFile, content, 0o644); err != nil {
			t.Fatal(err)
		}
		brokerRemote := "/home/sshuser/up-broker-single.dat"
		scpRemote := "/home/sshuser/up-scp-single.dat"

		res, err := cli.Upload(context.Background(), localFile, brokerRemote, 0)
		if err != nil {
			t.Fatalf("broker Upload single: %v", err)
		}
		if res.Files != 1 || res.Bytes != int64(len(content)) || res.Truncated {
			t.Fatalf("single Upload result = %+v, want {Files:1 Bytes:%d}", res, len(content))
		}
		scpSingle(t, localFile, scpRemote)
		remoteDiff(t, false, brokerRemote, scpRemote)
	})

	t.Run("recursive-dir", func(t *testing.T) {
		localRoot := t.TempDir()
		// Tree: a.txt + sub/b.txt + sub/deep/c.txt + empty.dat + sub/bin.bin
		// — 5 files total, 3 levels deep, exercising recursion + edge content.
		if err := os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("alpha-content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(localRoot, "sub", "deep"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "sub", "b.txt"), []byte("beta-content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "sub", "deep", "c.txt"), []byte("charlie-deep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "empty.dat"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		bin := make([]byte, 256)
		for i := range bin { // 0..255 — exercises null bytes + non-UTF8 (binary parity)
			bin[i] = byte(i)
		}
		if err := os.WriteFile(filepath.Join(localRoot, "sub", "bin.bin"), bin, 0o644); err != nil {
			t.Fatal(err)
		}

		brokerRemote := "/home/sshuser/up-broker-dir"
		scpRemote := "/home/sshuser/up-scp-dir"

		res, err := cli.Upload(context.Background(), localRoot, brokerRemote, 0)
		if err != nil {
			t.Fatalf("broker Upload dir: %v", err)
		}
		if res.Files != 5 {
			t.Fatalf("dir Upload Files = %d, want 5 (a.txt + sub/b.txt + sub/deep/c.txt + empty.dat + sub/bin.bin)", res.Files)
		}
		if res.Truncated {
			t.Fatalf("dir Upload Truncated = true, want false")
		}
		scpDir(t, localRoot, scpRemote)
		remoteDiff(t, true, brokerRemote, scpRemote)
	})
}

// TestForwardDifferential proves the broker's ForwardLocal (-L tunnel) delivers
// the SAME byte round-trip as a real `ssh -L` forward through the same real-
// OpenSSH sshd — the §13 differential for the forward surface. Both the broker
// tunnel and the real ssh -L forward reach a single persistent echo service
// running IN the conformance container on 127.0.0.1:inContainerEchoPort (the
// container-loopback nuance: `ssh -L ...:127.0.0.1:PORT` forwards to the SSH
// SERVER's loopback, i.e. the container's — so the echo service MUST run in the
// container; a host-side service would be unreachable). The differential
// payload is round-tripped through both paths and asserted byte-equal.
//
// In-container echo design (cleanest option, verified against alpine 3.20):
// busybox `nc -lk -p PORT -e /bin/cat` is a persistent TCP echo server — `-lk`
// keeps the listener up across connections, `-e /bin/cat` pipes each connection
// through cat (a true byte-for-byte echo). Launched detached via ssh exec with
// nohup + full redirection + </dev/null so it is reparented to sshd and survives
// the launcher session close. No Dockerfile change, no extra apk dep.
func TestForwardDifferential(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	brokerAuth := mustPrivAuth(t, privPath, "")
	sshArgs := sshBinaryKeyAuthArgs(host, port, "sshuser", privPath)

	// Persistent in-container echo service (nc -lk -p PORT -e /bin/cat).
	startInContainerEcho(t, sshArgs)

	// The differential payload both paths must echo back byte-for-byte. Includes
	// a trailing newline + digits to exercise multi-byte content (not just ASCII
	// letters); short enough that the round-trip is a single read.
	payload := "FORWARD-DIFFERENTIAL-PAYLOAD-1234567890\n"

	// === Broker path: sshbroker.Client.ForwardLocal (-L tunnel). ===
	cli, err := sshbroker.Connect(context.Background(), host, port, "sshuser", brokerAuth, ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("broker connect: %v", err)
	}
	defer cli.Close()
	tun, err := cli.ForwardLocal(0, "127.0.0.1", inContainerEchoPort)
	if err != nil {
		t.Fatalf("broker ForwardLocal: %v", err)
	}
	defer tun.Close()
	brokerEcho := roundTripTCP(t, tun.LocalAddr(), payload, 5*time.Second)

	// === Reference path: real `ssh -L` (os/exec, backgrounded). ===
	localPort, killFwd := startRealSSHForward(t, sshArgs, inContainerEchoPort)
	defer killFwd()
	realEcho := roundTripTCP(t, fmt.Sprintf("127.0.0.1:%d", localPort), payload, 5*time.Second)

	// === Differential assertions. ===
	if brokerEcho != payload {
		t.Errorf("broker forward echo = %q, want %q", brokerEcho, payload)
	}
	if realEcho != payload {
		t.Errorf("real ssh -L echo = %q, want %q", realEcho, payload)
	}
	if brokerEcho != realEcho {
		t.Errorf("forward differential diff: broker=%q real=%q", brokerEcho, realEcho)
	}
}

// startInContainerEcho launches a persistent nc-cat echo service in the
// conformance container on inContainerEchoPort via ssh exec, detached so it
// survives the launcher session close. Busybox `nc -lk -p PORT -e /bin/cat` is
// verified (alpine 3.20): `-lk` keeps the listener persistent, `-e /bin/cat`
// echoes each connection's bytes. Readiness is probed via a separate ssh exec
// that pipes a known marker through the echo port; the probe loops until the
// marker comes back (or the readiness deadline fires).
func startInContainerEcho(t *testing.T, sshArgs []string) {
	t.Helper()
	launch := fmt.Sprintf(
		"nohup nc -lk -p %d -e /bin/cat >/dev/null 2>&1 </dev/null & echo spawned",
		inContainerEchoPort,
	)
	out, _, code := runSSHBinary(t, append(append([]string{}, sshArgs...), launch)...)
	if code != 0 || !strings.Contains(out, "spawned") {
		t.Fatalf("start in-container echo: code=%d out=%q", code, out)
	}
	const marker = "ECHOREADY"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		probe := fmt.Sprintf("echo %s | nc -w 1 127.0.0.1 %d", marker, inContainerEchoPort)
		out, _, _ := runSSHBinary(t, append(append([]string{}, sshArgs...), probe)...)
		if strings.Contains(out, marker) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("in-container echo service did not become ready within 10s")
}

// startRealSSHForward spawns the real `ssh` binary with `-L
// 127.0.0.1:<freeport>:127.0.0.1:<remotePort> -N` in the background (no remote
// command — -N is the forward-only form) and returns the local port + a cleanup
// that kills the ssh process. The forward is polled for readiness: a loopback
// dial to the local port succeeds once ssh has authenticated + opened the
// forward. The ssh process is started (not Run) so the Go test holds the handle
// and can Kill it on cleanup (no -f fork-to-background indirection needed, so
// the PID is known + controllable on both Windows + Linux).
func startRealSSHForward(t *testing.T, sshArgs []string, remotePort int) (localPort int, cleanup func()) {
	t.Helper()
	localPort = freeLocalPort(t)
	// sshArgs's last element is the destination (user@host); insert -N -L before it.
	dst := sshArgs[len(sshArgs)-1]
	flags := sshArgs[:len(sshArgs)-1]
	args := append(append([]string{}, flags...),
		"-N",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remotePort),
		dst,
	)
	cmd := exec.Command("ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ssh -L: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return localPort, func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatalf("ssh -L forward not ready at %s within 10s: %s", addr, stderr.String())
	return 0, nil
}

// freeLocalPort returns a free TCP port on the broker host's loopback. There's
// an inherent race between close + re-bind by the caller, but for test purposes
// (immediate re-use) it's reliable.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// roundTripTCP dials addr, writes payload, reads exactly len(payload) echo
// bytes back under a deadline. nc-cat echoes as bytes arrive; reading exactly
// the payload length + closing is the deterministic assertion (no reliance on
// EOF, which nc-cat does not send while the connection stays open).
func roundTripTCP(t *testing.T, addr, payload string, timeout time.Duration) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set deadline %s: %v", addr, err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write to %s: %v", addr, err)
	}
	buf := make([]byte, len(payload))
	n, err := io.ReadFull(conn, buf)
	if err != nil {
		t.Fatalf("read echo from %s (%d/%d bytes): %v", addr, n, len(buf), err)
	}
	return string(buf[:n])
}
