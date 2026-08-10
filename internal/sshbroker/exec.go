package sshbroker

import (
	"context"
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

// Exec runs cmd on the remote host. ctx is honored: if the caller cancels ctx —
// directly or via the MCP tool-call ctx it flows from — the session is signaled
// and closed and Exec returns ctx.Err() with TimedOut left false (cancellation is
// not a timeout). A timeout > 0 additionally bounds execution via a deadline
// derived from ctx; on timeout the remote process is signaled to die and TimedOut
// is set true. maxBytes > 0 caps how much of each output channel is retained (the
// prefix); bytes beyond are counted (StdoutBytes/StderrBytes) then discarded, with
// Truncated set. maxBytes == 0 means unlimited.
//
// Because some servers (notably the in-process testsshd) do not act on signal
// requests, we also close the session to guarantee Run unblocks; the resulting
// ExitMissingError is swallowed by the timeout/cancellation branches below.
func (c *Client) Exec(ctx context.Context, cmd string, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()

	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	sess.Stdout = stdout
	sess.Stderr = stderr

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Abort the session on EITHER the (possibly deadline-bearing) ctx OR a caller
	// cancellation. `done` lets the watchdog exit cleanly when Run returns on its
	// own, so it never outlives Exec — no goroutine leak when the caller passes a
	// never-cancelled ctx (e.g. context.Background()).
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
	res := ExecResult{
		Stdout:      stdout.buf.String(),
		Stderr:      stderr.buf.String(),
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		Truncated:   stdout.truncated || stderr.truncated,
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.TimedOut = true
		return res, nil // timeout is a result, not an error
	case context.Canceled:
		return res, ctx.Err() // caller cancellation — surface as an error, not flagged as TimedOut
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil // non-zero exit is a result, not an error
	}
	return res, err
}
