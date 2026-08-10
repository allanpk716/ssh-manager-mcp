package sshbroker

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecSudo runs cmd with privilege escalation via `sudo -S`, feeding sudoPassword
// to sudo's stdin. ctx is honored exactly as in Exec (cancel → ctx.Err(),
// TimedOut stays false). Use this when the remote user needs a password for sudo;
// for NOPASSWD sudo, plain Exec(ctx, "sudo "+cmd, …) suffices. maxBytes has the
// same meaning as in Exec (0 = unlimited).
func (c *Client) ExecSudo(ctx context.Context, cmd string, sudoPassword []byte, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return ExecResult{}, err
	}
	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
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
		return ExecResult{}, err
	}
	pw := make([]byte, len(sudoPassword)+1)
	copy(pw, sudoPassword)
	pw[len(sudoPassword)] = '\n'
	if _, err := stdin.Write(pw); err != nil {
		return ExecResult{}, err
	}
	stdin.Close()

	err = sess.Wait()
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
		return res, nil
	case context.Canceled:
		return res, ctx.Err()
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil
	}
	return res, err
}
