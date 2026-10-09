package sshbroker

import (
	"context"
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecResult holds the outcome of a remote command.
type ExecResult struct {
	Stdout      string
	Stderr      string
	ExitCode    int
	TimedOut    bool
	StdoutBytes int64 // total stdout bytes seen (may exceed len(Stdout) when capped)
	StderrBytes int64 // total stderr bytes seen (may exceed len(Stderr) when capped)
	Truncated   bool  // true if stdout or stderr exceeded maxBytes and was capped to the prefix
	// Sudo is non-nil iff the exec went through the sudo wrapper (Plan 41 §2):
	// five-state outcome with the marker-attested uid. Stderr above is the
	// CLEANED stream (marker line and sudo prompt already stripped).
	Sudo *SudoMeta
}

// runSession is the writer-seam kernel behind Exec (and the background engine):
// it runs cmd in a fresh SSH session wired to the CALLER-SUPPLIED stdout/stderr
// writers. ctx is honored as in Exec (cancel → (0, false, ctx.Err()); timeout > 0
// bounds execution via a deadline derived from ctx, on timeout → (0, true, nil));
// a non-zero remote exit folds into (code, false, nil); anything else is
// (0, false, err).
//
// Timeout/cancel teardown is three-stage (killWatchdog below): ① signal the
// session (SIGKILL) and close the session channel; ② wait the client's kill
// grace (SSHMGR_EXEC_KILL_GRACE, default 2s) watching ONLY whether the kernel
// has returned; ③ if it still has not, close the WHOLE SSH connection. Stage ③
// exists because some servers wait for the remote child process to exit before
// they close the session channel back — for those servers the channel-level
// close of stage ① never unblocks Run/Wait, and only tearing down the whole
// connection does (verified against OpenSSH 9.6p1 on Ubuntu 24.04). The
// connection-death errors this can produce (ExitMissingError, io.ErrClosedPipe)
// are swallowed by the timeout/cancellation branches below.
func (c *Client) runSession(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return 0, false, err
	}
	defer sess.Close()

	sess.Stdout = stdout
	sess.Stderr = stderr

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// killWatchdog aborts the session on EITHER the (possibly deadline-bearing)
	// ctx OR a caller cancellation. `done` lets it exit cleanly when Run returns
	// on its own, so it never outlives the call — no goroutine leak when the
	// caller passes a never-cancelled ctx (e.g. context.Background()).
	done := make(chan struct{})
	defer close(done)
	go c.killWatchdog(ctx, sess, done)

	err = sess.Run(cmd)
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return 0, true, nil // timeout is a result, not an error
	case context.Canceled:
		return 0, false, ctx.Err() // caller cancellation — surface as an error, not flagged as TimedOut
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		return exitErr.ExitStatus(), false, nil // non-zero exit is a result, not an error
	}
	return 0, false, err
}

// killWatchdog is the three-stage teardown shared by the two execution kernels
// (runSession here, runSessionRaw in sudo.go). It arms on ctx (timeout or
// caller cancellation) and then escalates:
//
//	① sess.Signal(ssh.SIGKILL) + sess.Close() — the signal is a request the
//	  server may ignore; closing the session channel additionally unblocks
//	  Wait on servers that echo the channel close (the historical behavior,
//	  which every cooperative server — testsshd included — satisfies).
//	② wait c.killGrace watching ONLY done (whether the kernel returned) —
//	  servers that wait for the remote child process to exit before closing
//	  the channel back are NOT unlocked by ①, and the grace gives them the
//	  chance to return on their own.
//	③ still not returned → c.Close() — the WRAPPED Close (not c.c.Close()),
//	  so the keepalive loop (if any) stops too. Killing the whole connection
//	  is the only teardown verified to unblock Wait against such servers;
//	  every sshmgr exec path rides a dedicated connection per call, so this
//	  has no collateral damage.
//
// done closes when the kernel returns, so stages ②/③ never fire for a command
// that finishes (or is cooperatively killed) — the watchdog exits and never
// outlives the call. The timer-vs-done race inside stage ② is benign: when
// both become ready together the kernel was returning anyway.
func (c *Client) killWatchdog(ctx context.Context, sess *ssh.Session, done <-chan struct{}) {
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	_ = sess.Signal(ssh.SIGKILL)
	_ = sess.Close()
	timer := time.NewTimer(c.killGrace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	_ = c.Close()
}

// ExecWriters 是 runSession 内核的导出 writer-seam (Plan 32 T4: 后台引擎
// mcpserver.TaskManager 消费——调用方自带 io.Writer 收原始流)。分类三元组与
// runSession 恒同。timeout 语义照旧 (>0 时叠加一层 deadline); 后台引擎传 0
// (任务的 WithTimeout 已在 ctx 上)。
func (c *Client) ExecWriters(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error) {
	return c.runSession(ctx, cmd, timeout, stdout, stderr)
}

// Exec runs cmd on the remote host. ctx is honored: if the caller cancels ctx —
// directly or via the MCP tool-call ctx it flows from — the session is signaled
// and closed and Exec returns ctx.Err() with TimedOut left false (cancellation is
// not a timeout). A timeout > 0 additionally bounds execution via a deadline
// derived from ctx; on timeout the remote process is signaled to die and TimedOut
// is set true. maxBytes > 0 caps how much of each output channel is retained (the
// prefix); bytes beyond are counted (StdoutBytes/StderrBytes) then discarded, with
// Truncated set. maxBytes == 0 means unlimited.
//
// Exec is a thin shell over runSession: it supplies cappedBuffer writers and
// folds the kernel's (exitCode, timedOut, err) triple into ExecResult.
func (c *Client) Exec(ctx context.Context, cmd string, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	exitCode, timedOut, err := c.runSession(ctx, cmd, timeout, stdout, stderr)
	res := ExecResult{
		Stdout:      stdout.buf.String(),
		Stderr:      stderr.buf.String(),
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		Truncated:   stdout.truncated || stderr.truncated,
		ExitCode:    exitCode,
		TimedOut:    timedOut,
	}
	// ExitError is already folded into exitCode by the kernel; the shell does no
	// further classification — a non-nil err here is cancel or a genuine failure.
	if err != nil {
		return res, err
	}
	return res, nil
}
