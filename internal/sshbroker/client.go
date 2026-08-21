package sshbroker

import (
	"context"
	"net"
	"strconv"

	"golang.org/x/crypto/ssh"
)

// Client wraps an ssh.Client.
type Client struct {
	c *ssh.Client
}

// Connect dials the SSH server and authenticates. hostKeyCb enforces host-key
// policy. The SSHMGR_SSH_HOST_KEY_ALGORITHMS knob (if set) is validated FIRST
// and fail-closed: a typo'd value returns an error before any dial. ctx is
// honored: ssh.Dial itself cannot be interrupted, so on cancellation Connect
// returns ctx.Err() immediately and abandons the in-flight dial; a background
// goroutine closes the connection the dial eventually yields (so no *ssh.Client
// leaks). This bounds a cancelled dial to an unreachable host to milliseconds
// rather than the OS TCP timeout (~minutes).
func Connect(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error) {
	algos, err := hostKeyAlgosChecked() // fail-closed BEFORE dial (typo → no connection attempt)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: hostKeyCb,
	}
	if algos != nil {
		cfg.HostKeyAlgorithms = algos
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	type result struct {
		c   *ssh.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ssh.Dial("tcp", addr, cfg)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			// Plan 31: scrub the dialed address (and any resolved-IP / DNS
			// residue) from the error text AT THE SOURCE, so every consumer —
			// MCP tool errors included — is safe by construction. redactAddr's
			// rendered text is itself prefixed with "ssh dial: " (and its frozen
			// degraded phrases carry the same prefix), so Connect returns it
			// AS-IS — no outer wrap, or the prefix would double end-to-end
			// (owner ruling). The chain survives via Unwrap (errors.Is
			// classification) and net.Error is delegated; see redact.go.
			return nil, redactAddr(r.err, host, port)
		}
		return &Client{c: r.c}, nil
	case <-ctx.Done():
		go func() {
			r := <-ch // let the in-flight Dial finish, then reclaim its connection
			if r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (c *Client) Close() error { return c.c.Close() }
