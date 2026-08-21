package sshbroker

import (
	"context"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

// ctxErrOr reports a ctx cancellation as itself, so a caller sees context.Canceled
// (or DeadlineExceeded) rather than the lower-level error it surfaced as. Used in
// runSudoSession: a cancel arriving in the narrow sess.Start → stdin.Write window closes the session (via the watchdog) before the password
// write completes, surfacing as a generic Start/Write error that would otherwise
// reach the caller as-is instead of as the cancellation it is — and at the MCP
// layer would map to status="error" rather than status="cancelled".
func ctxErrOr(ctx context.Context, err error) error {
	if ce := ctx.Err(); ce != nil {
		return ce
	}
	return err
}

// runSudoSession is the writer-seam kernel behind ExecSudo (and the background
// engine's privileged variant): like runSession but running cmd through sudo -S
// with an empty prompt (the wrapped command below) and feeding pass to sudo's
// stdin (password line, then close) before waiting. Callers supply the
// stdout/stderr writers directly; ctx,
// timeout, and the (exitCode, timedOut, err) classification are identical to
// runSession.
func (c *Client) runSudoSession(ctx context.Context, cmd, pass string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return 0, false, err
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return 0, false, err
	}
	sess.Stdout = stdout
	sess.Stderr = stderr

	wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close() // some servers ignore SIGKILL; closing forces Wait to return
		case <-done:
		}
	}()

	if err := sess.Start(wrapped); err != nil {
		return 0, false, ctxErrOr(ctx, err)
	}
	pw := make([]byte, len(pass)+1)
	copy(pw, pass)
	pw[len(pass)] = '\n'
	if _, err := stdin.Write(pw); err != nil {
		return 0, false, ctxErrOr(ctx, err)
	}
	stdin.Close()

	err = sess.Wait()
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

// ExecSudo runs cmd with privilege escalation via `sudo -S`, feeding sudoPassword
// to sudo's stdin. ctx is honored exactly as in Exec (cancel → ctx.Err(),
// TimedOut stays false). Use this when the remote user needs a password for sudo;
// for NOPASSWD sudo, plain Exec(ctx, "sudo "+cmd, …) suffices. maxBytes has the
// same meaning as in Exec (0 = unlimited).
//
// ExecSudo is a thin shell over runSudoSession: it supplies cappedBuffer writers
// and folds the kernel's (exitCode, timedOut, err) triple into ExecResult.
func (c *Client) ExecSudo(ctx context.Context, cmd string, sudoPassword []byte, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	exitCode, timedOut, err := c.runSudoSession(ctx, cmd, string(sudoPassword), timeout, stdout, stderr)
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
