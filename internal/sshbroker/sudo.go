package sshbroker

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecSudo runs cmd with privilege escalation via `sudo -S`, feeding sudoPassword to sudo's stdin.
// Use this when the remote user needs a password for sudo. For NOPASSWD sudo, plain Exec("sudo "+cmd) suffices.
func (c *Client) ExecSudo(cmd string, sudoPassword []byte, timeout time.Duration) (ExecResult, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return ExecResult{}, err
	}
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
		go func() {
			<-ctx.Done()
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close() // some servers ignore SIGKILL; closing the channel forces Wait to return (mirrors Exec)
		}()
	}

	if err := sess.Start(wrapped); err != nil {
		return ExecResult{}, err
	}
	// Write the sudo password then close stdin so sudo proceeds.
	if _, err := stdin.Write(append(sudoPassword, '\n')); err != nil {
		return ExecResult{}, err
	}
	stdin.Close()

	err = sess.Wait()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil
	}
	if err != nil && res.TimedOut {
		return res, nil
	}
	return res, err
}
