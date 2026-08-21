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
}

// runSession is the writer-seam kernel behind Exec (and the background engine):
// it runs cmd in a fresh SSH session wired to the CALLER-SUPPLIED stdout/stderr
// writers. ctx is honored as in Exec (cancel → (0, false, ctx.Err()); timeout > 0
// bounds execution via a deadline derived from ctx, on timeout → (0, true, nil));
// a non-zero remote exit folds into (code, false, nil); anything else is
// (0, false, err).
//
// Because some servers (notably the in-process testsshd) do not act on signal
// requests, we also close the session to guarantee Run unblocks; the resulting
// ExitMissingError is swallowed by the timeout/cancellation branches below.
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

	// Abort the session on EITHER the (possibly deadline-bearing) ctx OR a caller
	// cancellation. `done` lets the watchdog exit cleanly when Run returns on its
	// own, so it never outlives the call — no goroutine leak when the caller
	// passes a never-cancelled ctx (e.g. context.Background()).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		case <-done:
		}
	}()

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
