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

// Exec runs cmd on the remote host. A timeout > 0 bounds execution; on timeout the
// remote process is signaled to die and TimedOut is set true. maxBytes > 0 caps how
// much of each output channel is retained (the prefix); bytes beyond that are
// counted (see StdoutBytes/StderrBytes) then discarded, with Truncated set — so a
// huge output cannot blow up memory while the caller still learns its true size and
// can tell the agent to refine the command. maxBytes == 0 means unlimited.
//
// Because some servers (notably the in-process testsshd) do not act on signal
// requests, we also close the session to guarantee Run unblocks; the resulting
// ExitMissingError is swallowed by the TimedOut branch below.
func (c *Client) Exec(cmd string, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()

	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	sess.Stdout = stdout
	sess.Stderr = stderr

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
		go func() {
			<-ctx.Done()
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		}()
	}

	err = sess.Run(cmd)
	res := ExecResult{
		Stdout:      stdout.buf.String(),
		Stderr:      stderr.buf.String(),
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		Truncated:   stdout.truncated || stderr.truncated,
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil // non-zero exit is a result, not an error
	}
	if err != nil && res.TimedOut {
		return res, nil
	}
	return res, err
}
