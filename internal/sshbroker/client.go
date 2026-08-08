package sshbroker

import (
	"fmt"

	"golang.org/x/crypto/ssh"
)

// Client wraps an ssh.Client.
type Client struct {
	c *ssh.Client
}

// Connect dials the SSH server and authenticates. hostKeyCb enforces host-key policy.
func Connect(host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: hostKeyCb,
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	c, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	return &Client{c: c}, nil
}

func (c *Client) Close() error { return c.c.Close() }
