package conformance

import (
	"context"
	"testing"
	"time"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestExecTimeoutConnKillRealSSH pins the three-stage exec-timeout contract
// (timeout escalates to connection teardown) against REAL OpenSSH: a timed-out
// Exec must ALWAYS return bounded (by its deadline plus the kill grace),
// classified as ExecResult.TimedOut with a nil error — the timeout is a result,
// not a failure — never wedging for as long as the remote command runs, and the
// server must still be healthy afterwards (a fresh connection round-trips).
//
// The assertions pin the CONTRACT (bounded return + classification + server
// health), NOT which watchdog stage unlocked the wait: a cooperative server
// honors the stage-① SIGKILL + session-channel close and unblocks right at the
// deadline, while a server that waits for the remote child process to exit
// before echoing the channel close only unblocks at stage ③ — the broker
// closing the WHOLE SSH connection once the kill grace (SSHMGR_EXEC_KILL_GRACE)
// expires. Both server shapes must pass this test; which stage fired is a
// server-side detail the contract deliberately does not care about. Real-machine
// evidence 2026-10-08 (host 4090x2, OpenSSH 9.6p1 / Ubuntu 24.04): the unbounded
// hang this contract forbids was reproduced there pre-fix, and only the
// whole-connection close unlocked it.
func TestExecTimeoutConnKillRealSSH(t *testing.T) {
	requireConformance(t)

	// The kill grace is read at connection construction — Setenv must precede
	// Connect. Dialed down so a stage-③ escalation lands around 3.3s, well
	// inside the 6s bound even with container scheduling jitter.
	t.Setenv("SSHMGR_EXEC_KILL_GRACE", "300ms")

	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	cli, err := sshbroker.Connect(context.Background(), host, port, "sshuser", mustPrivAuth(t, privPath, ""), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	ctx := context.Background()
	start := time.Now()
	res, err := cli.Exec(ctx, "sleep 60", 3*time.Second, 0)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("timed-out Exec returned err %v, want nil (timeout is a result, not an error)", err)
	}
	if !res.TimedOut {
		t.Fatalf("TimedOut = false (exit %d), want true", res.ExitCode)
	}
	// Bounded return: deadline + grace is the worst case (uncooperative
	// server, stage ③); the margin absorbs container scheduling jitter.
	if elapsed > 6*time.Second {
		t.Fatalf("timed-out Exec took %v, want <= 6s (3s deadline + 0.3s grace + container margin) — the wait was not bounded", elapsed)
	}

	// Server health: stage ③ tears down the WHOLE connection (that is the
	// point, for servers that ignore channel-level teardown), so the original
	// client may be dead by now — prove the sshd CONTAINER is still serving by
	// connecting fresh and round-tripping a command.
	cli2, err := sshbroker.Connect(context.Background(), host, port, "sshuser", mustPrivAuth(t, privPath, ""), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("post-timeout reconnect: %v", err)
	}
	defer cli2.Close()
	alive, err := cli2.Exec(ctx, "printf alive", 0, 0)
	if err != nil {
		t.Fatalf("post-timeout round-trip: %v", err)
	}
	if alive.Stdout != "alive" || alive.ExitCode != 0 {
		t.Fatalf("post-timeout round-trip = stdout %q exit %d, want \"alive\" / 0", alive.Stdout, alive.ExitCode)
	}
}
