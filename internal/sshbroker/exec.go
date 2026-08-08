package sshbroker

import (
	"bytes"
	"context"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecResult holds the outcome of a remote command.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// Exec runs cmd on the remote host. A timeout > 0 bounds execution; on timeout the
// remote process is signaled to die and TimedOut is set true. Because some servers
// (notably the in-process testsshd) do not act on signal requests, we also close the
// session to guarantee Run unblocks; the resulting ExitMissingError is swallowed by
// the TimedOut branch below.
func (c *Client) Exec(cmd string, timeout time.Duration) (ExecResult, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

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
		Stdout: stdout.String(),
		Stderr: stderr.String(),
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
