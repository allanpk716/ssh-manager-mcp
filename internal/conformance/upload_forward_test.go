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
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
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
// matches the industry-standard client. Covers: single-file upload; the §13
// suite tree (3-level nesting, empty dir, 0-byte file, unicode + space
// filenames, all-256-bytes binary); and the §6 byte-cap boundary — the one
// place the broker deliberately diverges from scp (the cap is a security
// feature; the boundary subtest locks boundary-exactness + honest reporting,
// NOT scp parity).
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
		scpPutFile(t, scpArgs, localFile, scpDst+scpRemote)
		remoteDiff(t, sshArgs, false, brokerRemote, scpRemote)
	})

	t.Run("suite-tree", func(t *testing.T) {
		// §13 suite tree: 3-level nesting + empty dir + 0-byte file + unicode +
		// space filenames + all-256-bytes binary. scp -r and broker Upload must
		// produce identical remote trees. The EMPTY DIR is the sharpest edge:
		// filepath.Walk visits it with no children, and uploadDir must Mkdir it
		// remotely exactly like scp -r does — a differential failure there is a
		// real broker bug (empty dirs silently dropped), not a test artifact.
		localRoot := t.TempDir()
		writeDifferentialSuite(t, localRoot)

		brokerRemote := "/home/sshuser/up-broker-suite"
		scpRemote := "/home/sshuser/up-scp-suite"

		res, err := cli.Upload(context.Background(), localRoot, brokerRemote, 0)
		if err != nil {
			t.Fatalf("broker Upload suite: %v", err)
		}
		if res.Files != 9 {
			t.Fatalf("suite Upload Files = %d, want 9 (root.txt + a/one.txt + a/b/two.txt + a/b/c/three.txt + zero-byte.txt + 中文名-测试.txt + with space.txt + pkg/🚀rocket.txt + bin.bin)", res.Files)
		}
		if res.Bytes != 496 { // 64+48+32+16+0+40+24+16+256 — exact per-file sums
			t.Fatalf("suite Upload Bytes = %d, want 496 (res=%+v)", res.Bytes, res)
		}
		if res.Truncated {
			t.Fatalf("suite Upload Truncated = true, want false")
		}
		scpPutDir(t, scpArgs, localRoot, scpDst+scpRemote)
		remoteDiff(t, sshArgs, true, brokerRemote, scpRemote)
		// Pin the empty-dir edge explicitly: a zero diff -r would ALSO pass if
		// both paths silently dropped the empty dir — this probe rules that out
		// for the broker side (TestDownloadDifferential's ssh find pins the scp
		// side), so the empty-dir coverage is self-contained per test.
		out, _, code := runSSHBinary(t, append(append([]string{}, sshArgs...), "test -d '"+brokerRemote+"/empty-dir' && echo EMPTY-DIR-OK")...)
		if code != 0 || !strings.Contains(out, "EMPTY-DIR-OK") {
			t.Fatalf("broker remote tree lacks empty-dir — Upload dropped an empty directory (code=%d):\n%s", code, out)
		}
	})

	t.Run("boundary-cap", func(t *testing.T) {
		// §6 cap boundary — the DELIBERATE divergence from scp, as of Plan 23 a
		// hard PER-FILE bound. The upload byte cap (mcpserver.MaxOutputBytes at
		// the MCP boundary) is a security feature of the broker; scp has no cap,
		// so the two paths intentionally differ here and this subtest does NOT
		// lock scp parity. It locks:
		//
		//   (a) pre-flight refusal — a lone cap+1 file is REFUSED before
		//       transfer: error naming file/size/cap, zero-value UploadResult,
		//       and the remote file ABSENT (not even a 0-byte placeholder —
		//       the refusal happens before sftp.Create);
		//   (b) boundary exactness — the refusal flips exactly at size>cap: the
		//       same upload at exactly cap bytes SUCCEEDS with Truncated=false
		//       and lands complete (the cumulative walk-halt layer — all files
		//       ≤ cap, total crossing cap → Truncated=true, in-flight file
		//       lands complete — is unit-pinned in upload_test.go);
		//   (c) scp control — the identical file via scp lands complete (cap+1),
		//       showing what the uncapped reference transfer does.
		capBytes := mcpserver.MaxOutputBytes
		boundary := detBytes(7, int(capBytes)+1) // exactly cap+1 bytes — one over

		localBoundary := filepath.Join(t.TempDir(), "boundary.bin")
		if err := os.WriteFile(localBoundary, boundary, 0o644); err != nil {
			t.Fatal(err)
		}
		localAtCap := filepath.Join(t.TempDir(), "atcap.bin")
		if err := os.WriteFile(localAtCap, boundary[:capBytes], 0o644); err != nil {
			t.Fatal(err)
		}

		// (a) the lone cap+1 file is refused before transfer.
		brokerRemote := "/home/sshuser/up-broker-boundary.bin"
		res, err := cli.Upload(context.Background(), localBoundary, brokerRemote, capBytes)
		if err == nil {
			t.Fatalf("boundary Upload: want pre-flight refusal error, got nil (res=%+v)", res)
		}
		if !strings.Contains(err.Error(), "exceeds upload cap") || !strings.Contains(err.Error(), "boundary.bin") {
			t.Fatalf("refusal error must name file + cap, got %q", err.Error())
		}
		if want := fmt.Sprintf("(%d bytes) exceeds upload cap %d", capBytes+1, capBytes); !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal error must carry size+cap evidence %q, got %q", want, err.Error())
		}
		if res.Files != 0 || res.Bytes != 0 || res.Truncated {
			t.Fatalf("refused result = %+v, want zero-value (zero bytes transferred)", res)
		}
		if !remotePathAbsent(t, sshArgs, brokerRemote) {
			t.Fatalf("refused file must be ABSENT remotely (refused before sftp.Create — not a 0-byte file): %s", brokerRemote)
		}
		// (b) the at-cap companion SUCCEEDS — the refusal flips exactly at the
		// cap boundary (size>cap), and ==cap is allowed.
		atCapRemote := "/home/sshuser/up-broker-atcap.bin"
		resAtCap, err := cli.Upload(context.Background(), localAtCap, atCapRemote, capBytes)
		if err != nil {
			t.Fatalf("broker Upload at-cap: %v", err)
		}
		if resAtCap.Truncated || resAtCap.Files != 1 || resAtCap.Bytes != capBytes {
			t.Fatalf("at-cap result = %+v, want {Files:1 Bytes:%d Truncated:false} — ==cap is allowed", resAtCap, capBytes)
		}
		if got := remoteFileSize(t, sshArgs, atCapRemote); got != capBytes {
			t.Fatalf("remote at-cap size = %d, want %d", got, capBytes)
		}
		// (c) scp control: the uncapped reference lands all cap+1 bytes.
		scpRemote := "/home/sshuser/up-scp-boundary.bin"
		scpPutFile(t, scpArgs, localBoundary, scpDst+scpRemote)
		if got := remoteFileSize(t, sshArgs, scpRemote); got != capBytes+1 {
			t.Fatalf("scp control size = %d, want %d (scp is uncapped)", got, capBytes+1)
		}
	})
}

// remoteDiff runs `ssh ... 'diff [-r] A B && echo DIFF-OK'` against the
// conformance sshd and fails the test unless the two remote paths are
// byte-identical (diff exits 0 → DIFF-OK printed). Package-level (not a test
// closure) so the upload AND download differentials share one definition.
func remoteDiff(t *testing.T, sshArgs []string, recursive bool, a, b string) {
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

// scpPutFile + scpPutDir run the real scp binary (os/exec) with the local path
// normalized to forward slashes — Windows-broker → Linux-server is the primary
// deployment, and MSYS scp on the broker host accepts forward-slash local paths
// unambiguously (a leading-drive `C:/...` form is never re-interpreted).
// remoteTarget is the full scp destination (`user@host:path`). Package-level so
// the upload AND download differentials share them.
func scpPutFile(t *testing.T, scpArgs []string, localFile, remoteTarget string) {
	t.Helper()
	args := append(append([]string{}, scpArgs...), filepath.ToSlash(localFile), remoteTarget)
	if out, err := exec.Command("scp", args...).CombinedOutput(); err != nil {
		t.Fatalf("scp single: %v\n%s", err, out)
	}
}

func scpPutDir(t *testing.T, scpArgs []string, localDir, remoteTarget string) {
	t.Helper()
	args := append(append([]string{}, scpArgs...), "-r", filepath.ToSlash(localDir), remoteTarget)
	if out, err := exec.Command("scp", args...).CombinedOutput(); err != nil {
		t.Fatalf("scp -r: %v\n%s", err, out)
	}
}

// remoteFileSize stats a remote file's exact size via real ssh (busybox
// `stat -c %s` on the alpine conformance container). The boundary-cap subtest
// asserts exact remote sizes, so the size probe rides the same reference ssh
// path as the differentials themselves.
func remoteFileSize(t *testing.T, sshArgs []string, path string) int64 {
	t.Helper()
	cmd := fmt.Sprintf("stat -c %%s '%s'", path)
	out, _, code := runSSHBinary(t, append(append([]string{}, sshArgs...), cmd)...)
	if code != 0 {
		t.Fatalf("stat %s (code=%d):\n%s", path, code, out)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("stat %s: parse %q: %v", path, out, err)
	}
	return n
}

// remotePathAbsent probes a remote path via real ssh (`test -e`) and reports
// whether it does NOT exist. The Plan 23 boundary-cap subtest asserts a refused
// upload leaves the remote ABSENT (the pre-flight refusal happens before
// sftp.Create, so not even a 0-byte placeholder may exist) — a failing
// remoteFileSize probe cannot distinguish "absent" from "stat error", so this
// companion makes absence a first-class assertion on the same reference ssh
// path as the differentials themselves.
func remotePathAbsent(t *testing.T, sshArgs []string, path string) bool {
	t.Helper()
	cmd := fmt.Sprintf("test -e '%s' && echo PRESENT || echo ABSENT", path)
	out, _, _ := runSSHBinary(t, append(append([]string{}, sshArgs...), cmd)...)
	return strings.Contains(out, "ABSENT") && !strings.Contains(out, "PRESENT")
}

// writeDifferentialSuite builds the §13 differential suite tree under dir:
//
//	root.txt         — regular file (deterministic pseudo-random bytes)
//	a/one.txt        ┐
//	a/b/two.txt      ├ 3-level nesting (recursion)
//	a/b/c/three.txt  ┘
//	empty-dir/       — empty directory (scp -r preserves it; broker must too)
//	zero-byte.txt    — 0-byte file
//	中文名-测试.txt   — unicode filename
//	with space.txt   — space in filename
//	pkg/🚀rocket.txt — emoji filename (4-byte UTF-8 — must round-trip byte-faithfully)
//	bin.bin          — all-256-bytes binary (null bytes + non-UTF8 parity)
//
// 9 files + 5 subdirs. Shared by the upload and download differentials.
// Content is deterministic per file (detBytes) — synthetic, reproducible, and
// byte-verifiable (public repo: no real hosts or data ever appear).
func writeDifferentialSuite(t *testing.T, dir string) {
	t.Helper()
	mk := func(rel string, b []byte) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("root.txt", detBytes(1, 64))
	mk("a/one.txt", detBytes(2, 48))
	mk("a/b/two.txt", detBytes(3, 32))
	mk("a/b/c/three.txt", detBytes(4, 16))
	mk("zero-byte.txt", nil)
	mk("中文名-测试.txt", detBytes(5, 40))
	mk("with space.txt", detBytes(6, 24))
	mk("pkg/🚀rocket.txt", detBytes(8, 16)) // emoji filename — 4-byte UTF-8 edge
	bin := make([]byte, 256)
	for i := range bin { // 0..255 — exercises null bytes + non-UTF8 (binary parity)
		bin[i] = byte(i)
	}
	mk("bin.bin", bin)
	if err := os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// detBytes returns n deterministic pseudo-random bytes from a tiny LCG —
// synthetic, reproducible fixture content (public repo: no real data).
func detBytes(seed, n int) []byte {
	b := make([]byte, n)
	x := uint32(seed)<<13 | 1 // | 1: never zero (LCG would stick at 0)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 23)
	}
	return b
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
