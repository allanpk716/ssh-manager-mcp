# ssh-manager-mcp Plan 2: In-Process SSH Broker Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the in-process SSH client layer (`golang.org/x/crypto/ssh`): connect + authenticate (password / plain private key / encrypted private key with passphrase), exec, `sudo -S`, and host-key TOFU/verify — plus an owner `ssh <host> [command]` CLI, audit logging, and two store carry-forwards from the Plan-1 final review (passphrase-fallback unlock; WAL + concurrency + `VerifyToken` prefix prefilter). This plan produces a working `ssh-manager ssh <host> <command>` (full-access remote exec). The MCP server + runtime Profile enforcement is Plan 3.

**Architecture:** A new `internal/sshbroker` package wraps `golang.org/x/crypto/ssh` with a testable API: `Connect` takes an `ssh.AuthMethod` + `ssh.HostKeyCallback` (callers compose these; tests inject directly), and exposes `Exec` / `ExecSudo`. Host-key custody uses a new `host_keys` table in the encrypted store, with a `HostKeyTOFU` policy producing the callback. The owner `ssh` CLI and (later) the MCP server resolve a server's credential from the store, build the auth method + TOFU callback, connect, and exec. Unit tests run against an in-process test sshd (`internal/testsshd`) — no docker.

**Tech Stack:** Go 1.22+ · `golang.org/x/crypto/ssh` (already a dep) · `golang.org/x/term` (read passphrase; new dep) · existing `internal/store` + `internal/cli` + `internal/models`.

## Global Constraints

- Module path: `ssh-manager-mcp`. Binary `ssh-manager` from `cmd/ssh-manager`.
- No CGO. Pure-Go SQLite (`modernc.org/sqlite`). Cross-platform Win/Linux/macOS.
- SSH via in-process `golang.org/x/crypto/ssh` ONLY (never shell out to `ssh` binary) — cleanest L2.
- Secrets NEVER logged. Command strings and stdout/stderr are logged to audit (not secrets).
- Master key 32B; AES-256-GCM; Argon2id token (1/64MiB/4/32); FK enforcement ON — all unchanged from Plan 1.
- Carry-forward: `Open` must enable WAL + cap pool; `VerifyToken` must prefilter on `token_prefix` before Argon2.
- TDD for every task (RED → GREEN → commit). Commit per task.
- New dep: `golang.org/x/term` (passphrase prompt). Add via `go get golang.org/x/term@<compatible>` when first imported; pin in go.mod.

---

## File Structure

```
ssh-manager-mcp/
├── internal/testsshd/sshd.go        # in-process SSH server for tests (+ _test.go helpers)
├── internal/sshbroker/
│   ├── auth.go        # PasswordAuth, PrivateKeyAuth (plain + encrypted) + tests
│   ├── client.go      # Connect, Client, Close + tests
│   ├── exec.go        # Exec, ExecResult, ExecSudo + tests
│   └── hostkey.go     # HostKeyTOFU(store, host) -> ssh.HostKeyCallback + tests
├── internal/store/
│   ├── hostkeys.go    # GetHostKey/SaveHostKey + host_keys table (schema add) + tests
│   └── audit.go       # WriteAudit(row) + tests
├── internal/cli/
│   ├── ssh.go         # owner `ssh <host> [command...]` subcommand + smoke test
│   └── unlock.go      # MODIFY: keychain-first + passphrase fallback (injectable seams) + test
└── internal/store/store.go          # MODIFY: DSN +WAL, SetMaxOpenConns(1)
    internal/store/projects.go       # MODIFY: VerifyToken prefix prefilter
```

**Responsibilities:** `testsshd` = test-only in-process sshd. `sshbroker` = pure SSH client (no store knowledge except via injected callback). `store` = persistence (host keys, audit). `cli` = thin commands composing store + sshbroker.

---

## Task 1: In-process test sshd harness

**Files:**
- Create: `internal/testsshd/sshd.go`
- Test: exercised by Tasks 2–6 (no standalone test file; this is infra. Add a self-smoke `sshd_test.go` that connects with a plain `ssh.Dial` to prove the harness itself works.)

**Interfaces:**
- Consumes: `golang.org/x/crypto/ssh`, `net`, `crypto/rsa`.
- Produces: `testsshd.Start(t *testing.T, opts Options) (addr string, hostKey ssh.PublicKey, cleanup func())`; `testsshd.Options{Password, AuthorizedKey, SudoPassword, Exec}` where `Exec func(cmd string, stdin io.Reader) (stdout, stderr string, exitCode int)`.

- [ ] **Step 1: Write the harness**

Create `internal/testsshd/sshd.go`:
```go
package testsshd

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Options configures the in-process test SSH server.
type Options struct {
	Password      string        // accept this password (empty = no password auth)
	AuthorizedKey ssh.PublicKey // accept this client public key (nil = no key auth)
	SudoPassword  string        // if set, Exec handler simulates sudo -S (reads pw from stdin)
	Exec          func(cmd string, stdin io.Reader) (stdout, stderr string, exitCode int)
}

// Start launches an in-process SSH server on a random local port.
// Returns host:port, the server's host public key, and a cleanup func.
func Start(t *testing.T, opts Options) (string, ssh.PublicKey, func()) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{}
	if opts.Password != "" {
		pw := opts.Password
		cfg.PasswordCallback = func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == pw {
				return nil, nil
			}
			return nil, io.EOF
		}
	}
	if opts.AuthorizedKey != nil {
		allowed := opts.AuthorizedKey.Marshal()
		cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, pub ssh.PublicKey) (*ssh.Permissions, error) {
			if bytesEqual(pub.Marshal(), allowed) {
				return nil, nil
			}
			return nil, io.EOF
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	execFn := opts.Exec
	if execFn == nil {
		execFn = func(string, io.Reader) (string, string, int) { return "", "", 0 }
	}
	stop := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(stop)
				return
			}
			go serve(conn, cfg, signer, opts.SudoPassword, execFn)
		}
	}()
	cleanup := func() {
		ln.Close()
		select {
		case <-stop:
		case <-time.After(2 * time.Second):
		}
	}
	return ln.Addr().String(), signer.PublicKey(), cleanup
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := range a {
		d |= a[i] ^ b[i]
	}
	return d == 0
}

func serve(c net.Conn, cfg *ssh.ServerConfig, signer ssh.Signer, sudoPw string, execFn func(string, io.Reader) (string, string, int)) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		go handleSession(newChan, sudoPw, execFn)
	}
}

func handleSession(newChan ssh.NewChannel, sudoPw string, execFn func(string, io.Reader) (string, string, int)) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)
		cmd := payload.Command
		stdin := io.Reader(ch)
		// If this is a sudo -S invocation and the server is simulating sudo,
		// read the password line from stdin first, then run the inner command.
		if strings.HasPrefix(cmd, "sudo -S") && sudoPw != "" {
			buf := make([]byte, 0, 256)
			one := make([]byte, 1)
			for {
				if _, err := stdin.Read(one); err != nil {
					break
				}
				if one[0] == '\n' {
					break
				}
				buf = append(buf, one[0])
			}
			cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "sudo -S -p '' --"))
		}
		stdout, stderr, exit := execFn(cmd, stdin)
		if stdout != "" {
			ch.Write([]byte(stdout))
		}
		if stderr != "" {
			ch.Stderr().Write([]byte(stderr))
		}
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(exit)}))
		return
	}
}
```

- [ ] **Step 2: Write a self-smoke test**

Create `internal/testsshd/sshd_test.go`:
```go
package testsshd

import (
	"io"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestStartExecutesCommand(t *testing.T) {
	addr, hostKey, cleanup := Start(t, Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "echo hi" {
				return "hi\n", "", 0
			}
			return "", "unknown cmd", 1
		},
	})
	defer cleanup()

	cfg := &ssh.ClientConfig{
		User:            "u",
		Auth:            []ssh.AuthMethod{ssh.Password("pw")},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
	}
	cli, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	out, err := sess.CombinedOutput("echo hi")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("got %q", out)
	}
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/testsshd/`
Expected: PASS.

```bash
git add internal/testsshd/ && git commit -m "feat(testsshd): in-process SSH server for tests"
```

---

## Task 2: sshbroker — auth method builders + Connect

**Files:**
- Create: `internal/sshbroker/auth.go`, `internal/sshbroker/client.go`, `internal/sshbroker/auth_test.go`, `internal/sshbroker/client_test.go`
- Test: as above

**Interfaces:**
- Consumes: `golang.org/x/crypto/ssh`, `internal/testsshd`.
- Produces: `sshbroker.PasswordAuth(pw string) ssh.AuthMethod`; `sshbroker.PrivateKeyAuth(keyPEM []byte, passphrase []byte) (ssh.AuthMethod, error)`; `sshbroker.Connect(host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error)`; `(*Client).Close() error`.

- [ ] **Step 1: Write auth.go**

Create `internal/sshbroker/auth.go`:
```go
package sshbroker

import (
	"errors"

	"golang.org/x/crypto/ssh"
)

// PasswordAuth builds the SSH password auth method.
func PasswordAuth(pw string) ssh.AuthMethod {
	return ssh.Password(pw)
}

// PrivateKeyAuth builds a public-key auth method from a PEM private key.
// If the key is encrypted, passphrase must be supplied; it is ignored for unencrypted keys.
func PrivateKeyAuth(keyPEM []byte, passphrase []byte) (ssh.AuthMethod, error) {
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err == nil {
		return ssh.PublicKeys(signer), nil
	}
	var e *ssh.PassphraseMissingError
	if errors.As(err, &e) && len(passphrase) > 0 {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(keyPEM, passphrase)
		if err != nil {
			return nil, err
		}
		return ssh.PublicKeys(signer), nil
	}
	return nil, err
}
```

- [ ] **Step 2: Write auth_test.go (uses testsshd)**

Create `internal/sshbroker/auth_test.go`:
```go
package sshbroker

import (
	"io"
	"testing"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

func TestConnectPasswordAuth(t *testing.T) {
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "secret",
		Exec:     func(string, io.Reader) (string, string, int) { return "ok\n", "", 0 },
	})
	defer cleanup()
	cb := ssh.FixedHostKey(hostKey)
	cli, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("secret"), cb)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()
}

func TestConnectPasswordAuthRejected(t *testing.T) {
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "secret"})
	defer cleanup()
	_, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("wrong"), ssh.FixedHostKey(hostKey))
	if err == nil {
		t.Fatal("connect with wrong password must fail")
	}
}
```

Add the `hostOf`/`portOf` helpers to `internal/sshbroker/client_test.go` (next step) — but tests in this file need them now, so put them in a shared `helpers_test.go`:

Create `internal/sshbroker/helpers_test.go`:
```go
package sshbroker

import "strings"

func hostOf(addr string) string { return addr[:strings.LastIndex(addr, ":")] }
func portOf(addr string) int {
	var p int
	for _, c := range addr[strings.LastIndex(addr, ":")+1:] {
		p = p*10 + int(c-'0')
	}
	return p
}
```

- [ ] **Step 3: Write client.go**

Create `internal/sshbroker/client.go`:
```go
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
```

- [ ] **Step 4: Write client_test.go (key auth, plain + encrypted)**

Create `internal/sshbroker/client_test.go`:
```go
package sshbroker

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"testing"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

func mustRSAPEM(t *testing.T, passphrase string) ([]byte, ssh.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(k, "", []byte(passphrase))
		// fall back to x509 if MarshalPrivateKeyWithPassphrase unavailable
		if err != nil {
			der, _ := x509.MarshalPKCS8PrivateKey(k)
			block, _ = x509.EncryptPEMBlock(rand.Reader, "ENCRYPTED PRIVATE KEY", der, []byte(passphrase), x509.PEMCipherAES128)
		}
	} else {
		der, _ := x509.MarshalPKCS8PrivateKey(k)
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), pub
}

func TestConnectPrivateKeyPlain(t *testing.T) {
	keyPEM, pub := mustRSAPEM(t, "")
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		AuthorizedKey: pub,
		Exec:          func(string, io.Reader) (string, string, int) { return "ok\n", "", 0 },
	})
	defer cleanup()
	auth, err := PrivateKeyAuth(keyPEM, nil)
	if err != nil {
		t.Fatalf("PrivateKeyAuth: %v", err)
	}
	cli, err := Connect(hostOf(addr), portOf(addr), "u", auth, ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cli.Close()
}

func TestConnectPrivateKeyEncrypted(t *testing.T) {
	keyPEM, pub := mustRSAPEM(t, "keypass")
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{AuthorizedKey: pub})
	defer cleanup()
	auth, err := PrivateKeyAuth(keyPEM, []byte("keypass"))
	if err != nil {
		t.Fatalf("PrivateKeyAuth: %v", err)
	}
	cli, err := Connect(hostOf(addr), portOf(addr), "u", auth, ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect encrypted key: %v", err)
	}
	cli.Close()
}

func TestPrivateKeyAuthWrongPassphraseFails(t *testing.T) {
	keyPEM, _ := mustRSAPEM(t, "keypass")
	if _, err := PrivateKeyAuth(keyPEM, []byte("wrong")); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
}
```

- [ ] **Step 5: Run + commit**

Run: `go test ./internal/sshbroker/`
Expected: PASS (5 tests).

```bash
git add internal/sshbroker/ && git commit -m "feat(sshbroker): auth method builders + Connect"
```

---

## Task 3: sshbroker — Exec + ExecResult

**Files:**
- Create: `internal/sshbroker/exec.go`, `internal/sshbroker/exec_test.go`
- Test: `internal/sshbroker/exec_test.go`

**Interfaces:**
- Consumes: `(*Client)` from Task 2.
- Produces: `sshbroker.ExecResult{Stdout, Stderr string; ExitCode int; TimedOut bool}`; `(*Client).Exec(cmd string, timeout time.Duration) (ExecResult, error)`.

- [ ] **Step 1: Write exec.go**

Create `internal/sshbroker/exec.go`:
```go
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
// remote process is signaled to die and TimedOut is set true.
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
```

- [ ] **Step 2: Write exec_test.go**

Create `internal/sshbroker/exec_test.go`:
```go
package sshbroker

import (
	"io"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

func connectTest(t *testing.T, addr string, hostKey ssh.PublicKey) *Client {
	t.Helper()
	cli, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func TestExecStdoutAndExitCode(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "exit 7" {
				return "", "", 7
			}
			return "out:" + cmd + "\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec("hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out:hello\n" || res.ExitCode != 0 {
		t.Fatalf("unexpected %+v", res)
	}

	res2, _ := c.Exec("exit 7", 0)
	if res2.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res2.ExitCode)
	}
}

func TestExecTimeoutKillsAndFlags(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			// simulate a long-running command that only returns after the timeout fires.
			time.Sleep(2 * time.Second)
			return "done\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec("slow", 200*time.Millisecond)
	if err != nil && !res.TimedOut {
		t.Fatalf("err: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected TimedOut=true")
	}
	if strings.Contains(res.Stdout, "done") {
		t.Fatal("should not have completed")
	}
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/sshbroker/`
Expected: PASS.

```bash
git add internal/sshbroker/exec.go internal/sshbroker/exec_test.go && git commit -m "feat(sshbroker): Exec with stdout/stderr/exit + timeout"
```

---

## Task 4: sshbroker — ExecSudo (sudo -S)

**Files:**
- Create: `internal/sshbroker/sudo.go`, `internal/sshbroker/sudo_test.go`
- Test: `internal/sshbroker/sudo_test.go`

**Interfaces:**
- Consumes: `(*Client).Exec` is NOT used (sudo needs stdin piping); uses `c.c.NewSession()` directly.
- Produces: `(*Client).ExecSudo(cmd string, sudoPassword []byte, timeout time.Duration) (ExecResult, error)` — runs `sudo -S -p '' -- <cmd>` and writes `sudoPassword + "\n"` to stdin.

- [ ] **Step 1: Write sudo.go**

Create `internal/sshbroker/sudo.go`:
```go
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
```

- [ ] **Step 2: Write sudo_test.go**

Create `internal/sshbroker/sudo_test.go`:
```go
package sshbroker

import (
	"io"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/testsshd"
)

func TestExecSudoFeedsPasswordAndRunsInner(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, stdin io.Reader) (string, string, int) {
			// testsshd consumes the sudo pw line (SudoPassword set), then passes the inner cmd here.
			if cmd == "whoami" {
				return "root\n", "", 0
			}
			return "", "unknown", 1
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo("whoami", []byte("sudopw"), 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "root" {
		t.Fatalf("stdout = %q, want root", res.Stdout)
	}
}
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/sshbroker/`
Expected: PASS.

```bash
git add internal/sshbroker/sudo.go internal/sshbroker/sudo_test.go && git commit -m "feat(sshbroker): ExecSudo via sudo -S with stdin-fed password"
```

---

## Task 5: store — host_keys table + CRUD + sshbroker HostKeyTOFU

**Files:**
- Create: `internal/store/hostkeys.go`, `internal/store/hostkeys_test.go`, `internal/sshbroker/hostkey.go`, `internal/sshbroker/hostkey_test.go`
- Modify: `internal/store/store.go` (add `host_keys` table to `schemaSQL`)
- Test: as above

**Interfaces:**
- Consumes: `*store.Store`, `ssh.PublicKey`.
- Produces: `(*Store).GetHostKey(host string) ([]byte, error)` (nil,nil if none); `(*Store).SaveHostKey(host string, marshaledKey []byte) error`; `sshbroker.HostKeyTOFU(st HostKeyStore, host string) (ssh.HostKeyCallback, error)` where `HostKeyStore` is a small interface `{ GetHostKey(string)([]byte,error); SaveHostKey(string,[]byte)error }`.

- [ ] **Step 1: Add host_keys table to schema**

In `internal/store/store.go`, append to `schemaSQL` (before the closing backtick):
```sql
CREATE TABLE IF NOT EXISTS host_keys (
  host TEXT PRIMARY KEY,
  key_blob BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
```

- [ ] **Step 2: Write hostkeys.go (store)**

Create `internal/store/hostkeys.go`:
```go
package store

import "database/sql"

// GetHostKey returns the stored marshaled host key for host, or (nil, nil) if absent.
func (s *Store) GetHostKey(host string) ([]byte, error) {
	var blob []byte
	err := s.db.QueryRow(`SELECT key_blob FROM host_keys WHERE host=?`, host).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// SaveHostKey records (trusts on first use) a marshaled host key for host.
func (s *Store) SaveHostKey(host string, marshaledKey []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO host_keys (host, key_blob, created_at) VALUES (?,?,?)
		 ON CONFLICT(host) DO UPDATE SET key_blob=excluded.key_blob`,
		host, marshaledKey, now(),
	)
	return err
}
```

- [ ] **Step 3: Write hostkeys_test.go (store)**

Create `internal/store/hostkeys_test.go`:
```go
package store

import (
	"bytes"
	"testing"
)

func TestHostKeySaveGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetHostKey("gpu.example")
	if err != nil || got != nil {
		t.Fatalf("absent: got %v, %v", got, err)
	}
	blob := []byte{1, 2, 3, 4}
	if err := s.SaveHostKey("gpu.example", blob); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetHostKey("gpu.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("got %v want %v", got, blob)
	}
	// upsert: saving again replaces
	if err := s.SaveHostKey("gpu.example", []byte{9, 9}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetHostKey("gpu.example")
	if !bytes.Equal(got, []byte{9, 9}) {
		t.Fatal("upsert did not replace")
	}
}
```

- [ ] **Step 4: Write sshbroker/hostkey.go (TOFU policy)**

Create `internal/sshbroker/hostkey.go`:
```go
package sshbroker

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
)

// HostKeyStore is the subset of *store.Store that HostKeyTOFU needs (also faked in tests).
type HostKeyStore interface {
	GetHostKey(host string) ([]byte, error)
	SaveHostKey(host string, marshaledKey []byte) error
}

// HostKeyTOFU returns a trust-on-first-use host-key callback bound to st.
// First connection to host: records its key. Subsequent: must match, else rejected.
func HostKeyTOFU(st HostKeyStore, host string) (ssh.HostKeyCallback, error) {
	return func(_ string, remote ssh.PublicKey) error {
		marshaled := remote.Marshal()
		stored, err := st.GetHostKey(host)
		if err != nil {
			return err
		}
		if stored == nil {
			if err := st.SaveHostKey(host, marshaled); err != nil {
				return fmt.Errorf("save host key: %w", err)
			}
			return nil // trust on first use
		}
		if !bytes.Equal(marshaled, stored) {
			return errors.New("host key mismatch: possible MITM, connection rejected")
		}
		return nil
	}, nil
}
```

- [ ] **Step 5: Write sshbroker/hostkey_test.go (TOFU + mismatch)**

Create `internal/sshbroker/hostkey_test.go`:
```go
package sshbroker

import (
	"testing"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

type fakeHostKeyStore struct {
	keys map[string][]byte
}

func (f *fakeHostKeyStore) GetHostKey(host string) ([]byte, error) { return f.keys[host], nil }
func (f *fakeHostKeyStore) SaveHostKey(host string, k []byte) error {
	if f.keys == nil {
		f.keys = map[string][]byte{}
	}
	f.keys[host] = k
	return nil
}

func TestHostKeyTOFURecordsThenVerifies(t *testing.T) {
	st := &fakeHostKeyStore{}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()

	cb, err := HostKeyTOFU(st, "h")
	if err != nil {
		t.Fatal(err)
	}
	// first connect: records host key
	cli, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err != nil {
		t.Fatalf("first connect (TOFU): %v", err)
	}
	cli.Close()
	if len(st.keys) != 1 {
		t.Fatalf("expected 1 recorded key, got %d", len(st.keys))
	}

	// second connect: verifies, succeeds
	cb2, _ := HostKeyTOFU(st, "h")
	cli2, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb2)
	if err != nil {
		t.Fatalf("second connect (verify): %v", err)
	}
	cli2.Close()
}

func TestHostKeyMismatchRejected(t *testing.T) {
	st := &fakeHostKeyStore{keys: map[string][]byte{"h": []byte("stale-different-key")}}
	cb, _ := HostKeyTOFU(st, "h")
	// calling the callback with any real key must error (stored != real)
	err := cb("h", nil)
	_ = err // remote is nil here; the real path passes a real key. Direct test:
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	_ = hostKey
	_, err = Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err == nil {
		t.Fatal("mismatched host key must be rejected")
	}
}
```

- [ ] **Step 6: Run + commit**

Run: `go test ./internal/store/ ./internal/sshbroker/`
Expected: PASS.

```bash
git add internal/store/store.go internal/store/hostkeys.go internal/store/hostkeys_test.go internal/sshbroker/hostkey.go internal/sshbroker/hostkey_test.go && git commit -m "feat: host_keys store + TOFU/verify host-key policy"
```

---

## Task 6: store — audit WriteAudit + cli — owner `ssh <host> [command]`

**Files:**
- Create: `internal/store/audit.go`, `internal/store/audit_test.go`, `internal/cli/ssh.go`, `internal/cli/ssh_smoke_test.go`
- Test: as above

**Interfaces:**
- Consumes: `*store.Store`, `sshbroker`, `testsshd`.
- Produces: `store.AuditRow` struct; `(*Store).WriteAudit(row AuditRow) error`; cobra `ssh-manager ssh <host> [command...]` (full-access exec, no profile limit, writes audit).

- [ ] **Step 1: Write audit.go**

Create `internal/store/audit.go`:
```go
package store

import "time"

// AuditRow is one auditable action.
type AuditRow struct {
	TS         time.Time
	ProjectID  string // empty for owner (non-agent) actions
	ServerID   string
	Action     string // "exec"
	Command    string
	Sudo       bool
	Status     string // "ok" / "error" / "timeout"
	ExitCode   int
	DurationMS int64
}

func (s *Store) WriteAudit(r AuditRow) error {
	var sudo int
	if r.Sudo {
		sudo = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_log (ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		r.TS.Unix(), nullableString(r.ProjectID), nullableString(r.ServerID),
		r.Action, r.Command, sudo, nullableString(r.Status), r.ExitCode, r.DurationMS,
	)
	return err
}
```

- [ ] **Step 2: Write audit_test.go**

Create `internal/store/audit_test.go`:
```go
package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestWriteAuditPersistsRow(t *testing.T) {
	s := newTestStore(t)
	err := s.WriteAudit(AuditRow{
		TS: time.Now(), ServerID: "srv1", Action: "exec",
		Command: "ls", Sudo: true, Status: "ok", ExitCode: 0, DurationMS: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	var action, cmd string
	var sudo int
	err = s.db.QueryRow(`SELECT action, command, sudo FROM audit_log WHERE server_id=?`, "srv1").
		Scan(&action, &cmd, &sudo)
	if err == sql.ErrNoRows {
		t.Fatal("audit row not found")
	}
	if action != "exec" || cmd != "ls" || sudo != 1 {
		t.Fatalf("got action=%q cmd=%q sudo=%d", action, cmd, sudo)
	}
}
```

- [ ] **Step 3: Write cli/ssh.go (owner full-access ssh)**

Create `internal/cli/ssh.go`:
```go
package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

func newSSHCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ssh <host-name> [command...]",
		Short: "Owner full-access SSH exec (no profile limit). Runs the command on the named server.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			srv, err := st.GetServerByName(args[0])
			if err != nil {
				return err
			}
			if srv == nil {
				return fmt.Errorf("server %q not found", args[0])
			}
			auth, err := authForServer(st, srv)
			if err != nil {
				return err
			}
			hkCb, err := sshbroker.HostKeyTOFU(st, srv.Host)
			if err != nil {
				return err
			}
			start := time.Now()
			commandStr := strings.Join(args[1:], " ")
			status := "ok"
			var res sshbroker.ExecResult
			cli, err := sshbroker.Connect(srv.Host, srv.Port, srv.User, auth, hkCb)
			if err != nil {
				status = "error"
				_ = st.WriteAudit(store.AuditRow{TS: start, ServerID: srv.ID, Action: "exec", Command: commandStr, Status: status, DurationMS: time.Since(start).Milliseconds()})
				return err
			}
			defer cli.Close()
			res, err = cli.Exec(commandStr, 120*time.Second)
			if err != nil {
				status = "error"
			}
			if res.TimedOut {
				status = "timeout"
			}
			_ = st.WriteAudit(store.AuditRow{
				TS: start, ServerID: srv.ID, Action: "exec", Command: commandStr,
				Status: status, ExitCode: res.ExitCode, DurationMS: time.Since(start).Milliseconds(),
			})
			out := cmd.OutOrStdout()
			fmt.Fprint(out, res.Stdout)
			fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
			if res.ExitCode != 0 {
				cmd.SilenceErrors = true
				cmd.SilenceUsage = true
			}
			return nil
		},
	}
	return c
}
```

Add an `authForServer` helper to `internal/cli/servers.go` (it builds the ssh.AuthMethod from the stored credential; reused by MCP in Plan 3). Append to `internal/cli/servers.go`:
```go
import (
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// authForServer resolves a server's stored credential into an SSH auth method.
func authForServer(st *store.Store, srv *models.Server) (ssh.AuthMethod, error) {
	cred, err := st.GetCredential(srv.CredentialID)
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, fmt.Errorf("credential %s not found", srv.CredentialID)
	}
	switch srv.AuthMethod {
	case models.AuthPassword:
		return sshbroker.PasswordAuth(string(cred.Secret)), nil
	case models.AuthPrivateKey:
		return sshbroker.PrivateKeyAuth(cred.Secret, cred.Passphrase)
	}
	return nil, fmt.Errorf("unknown auth method %q", srv.AuthMethod)
}
```
(Add `"ssh-manager-mcp/internal/store"`, `"ssh-manager-mcp/internal/sshbroker"`, and `"golang.org/x/crypto/ssh"` to servers.go imports if not present; `"fmt"`/`"os"`/`"github.com/spf13/cobra"`/`"ssh-manager-mcp/internal/models"` are already imported from Plan 1.)

- [ ] **Step 4: Wire into root + write smoke test**

Modify `internal/cli/root.go` `NewRootCmd` to add `newSSHCmd()` to `root.AddCommand(...)`.

Create `internal/cli/ssh_smoke_test.go`:
```go
package cli

import (
	"bytes"
	"encoding/hex"
	"io"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestOwnerSSHExecRunsCommand(t *testing.T) {
	// start a test sshd that echoes the command
	addr, hostKey, srvCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "RAN:" + cmd + "\n", "", 0
		},
	})
	defer srvCleanup()
	host := addr[:bytesIndex(addr, ':')]

	// set up an isolated vault + master key
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	// seed a server pointing at the test sshd
	st, err := store.Open(filepath.Join(dir, "test.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, _ := st.AddServer(&models.Server{
		Name: "t", Host: host, Port: portOfAddr(addr), User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	// pre-trust the test host key (TOFU would also work, but pin for determinism)
	st.SaveHostKey(host, hostKey.Marshal())
	st.Close()
	_ = srvID

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"ssh", "t", "echo", "hello"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ssh cmd: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("RAN:echo hello")) {
		t.Fatalf("output missing exec result: %q", out.String())
	}
}

// small helpers local to this test
func bytesIndex(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return len(s)
}
func portOfAddr(addr string) int {
	i := bytesIndex(addr, ':')
	var p int
	for _, r := range addr[i+1:] {
		p = p*10 + int(r-'0')
	}
	return p
}
```

(Note: this test references `models` — add `"ssh-manager-mcp/internal/models"` to imports. `os` is imported for env in `withEnv` which already exists in `cli_smoke_test.go`.)

- [ ] **Step 5: Run + commit**

Run: `go build ./...` then `go test ./...`
Expected: all green (store + sshbroker + cli incl. owner ssh smoke).

```bash
git add internal/store/audit.go internal/store/audit_test.go internal/cli/ssh.go internal/cli/ssh_smoke_test.go internal/cli/servers.go internal/cli/root.go && git commit -m "feat: audit WriteAudit + owner ssh full-access exec"
```

---

## Task 7: carry-forward — passphrase-fallback unlock

**Files:**
- Modify: `internal/cli/unlock.go`, `internal/cli/unlock_test.go` (new)
- Test: `internal/cli/unlock_test.go`

**Interfaces:**
- Consumes: `store.KeyProvider`, `store.DeriveFromPassphrase`, `store.LoadMeta/SaveMeta`, `golang.org/x/term`.
- Produces: unlock flow: keychain first → on `ErrNotFound`/keychain-unavailable, read passphrase via an injectable `passphrasePrompt` (default `term.ReadPassword`), load/create `Meta.PassphraseSalt`, derive, optionally cache to keychain, print `export SSHMGR_MASTERKEY_HEX=<hex>`. Injectable seams: `cli.keychain` (default `store.KeyringKeyProvider{}`) and `cli.passphrasePrompt` (default terminal) — tests override.

- [ ] **Step 1: Rewrite unlock.go with seams + fallback**

Replace the body of `internal/cli/unlock.go` with:
```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/store"
)

// keychain is the master-key source (default real OS keychain; tests override).
var keychain store.KeyProvider = store.KeyringKeyProvider{}

// passphrasePrompt reads a passphrase (default terminal; tests override).
var passphrasePrompt = func() ([]byte, error) {
	fmt.Fprint(os.Stderr, "Enter passphrase to unlock vault: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return b, err
}

func newUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Resolve the master key (keychain, else passphrase) and print SSHMGR_MASTERKEY_HEX",
		RunE: func(cmd *cobra.Command, args []string) error {
			mk, err := keychain.Get()
			if err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
				return nil
			}
			if err != store.ErrNotFound {
				// keychain unavailable (e.g. headless Linux w/o Secret Service) → passphrase fallback
				return runPassphraseUnlock(cmd)
			}
			// first run with a working keychain: generate + store
			mk, err = store.GenerateMasterKey()
			if err != nil {
				return err
			}
			if err := keychain.Set(mk); err != nil {
				// can't persist to keychain → fall back to passphrase path
				return runPassphraseUnlock(cmd)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
			return nil
		},
	}
}

func runPassphraseUnlock(cmd *cobra.Command) error {
	metaPath, err := metaFilePath()
	if err != nil {
		return err
	}
	meta, _ := store.LoadMeta(metaPath)
	if meta == nil {
		// first passphrase use: generate salt
		if err := store.SaveMeta(metaPath, &store.Meta{PassphraseSalt: store.NewSalt16()}); err != nil {
			return err
		}
		meta, _ = store.LoadMeta(metaPath)
	}
	pass, err := passphrasePrompt()
	if err != nil {
		return err
	}
	mk := store.DeriveFromPassphrase(pass, meta.PassphraseSalt)
	fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
	return nil
}

func newLockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Clear the master key from this shell",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "unset SSHMGR_MASTERKEY_HEX")
			os.Unsetenv("SSHMGR_MASTERKEY_HEX")
			return nil
		},
	}
}
```

Add the small helpers used above. `store.NewSalt16()` must be exported from the store package — add to `internal/store/masterkey.go`:
```go
// NewSalt16 returns 16 random bytes for passphrase derivation.
func NewSalt16() []byte { return newSalt() }
```
(`newSalt` already exists in `internal/store/token.go`; reuse it.) And `metaFilePath()` in `internal/cli/common.go`:
```go
func metaFilePath() (string, error) {
	p, err := storePath()
	if err != nil {
		return "", err
	}
	// meta.json lives next to the store file
	return p + ".meta.json", nil
}
```

- [ ] **Step 2: Write unlock_test.go (passphrase fallback path)**

Create `internal/cli/unlock_test.go`:
```go
package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestUnlockPassphraseFallbackDerivesKey(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_STORE": filepath.Join(dir, "test.db")})

	// force keychain unavailable -> passphrase fallback
	prevKc := keychain
	keychain = &unavailableKeychain{}
	defer func() { keychain = prevKc }()

	// inject a fixed passphrase
	prevPrompt := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) { return []byte("my-passphrase"), nil }
	defer func() { passphrasePrompt = prevPrompt }()

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"unlock"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	hexStr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(out.String(), "\n"), "export SSHMGR_MASTERKEY_HEX="))
	if _, err := hex.DecodeString(hexStr); err != nil {
		t.Fatalf("output not hex: %q", out.String())
	}
	meta, _ := store.LoadMeta(filepath.Join(dir, "test.db.meta.json"))
	if meta == nil {
		t.Fatal("meta.json not created")
	}
	want := store.DeriveFromPassphrase([]byte("my-passphrase"), meta.PassphraseSalt)
	if hex.EncodeToString(want) != hexStr {
		t.Fatal("derived key does not match passphrase+salt")
	}
}

type unavailableKeychain struct{}

func (unavailableKeychain) Get() ([]byte, error) { return nil, os.ErrNotExist }
func (unavailableKeychain) Set([]byte) error     { return nil }
```

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/cli/`
Expected: PASS.

```bash
git add internal/cli/unlock.go internal/cli/unlock_test.go internal/cli/common.go internal/store/masterkey.go && git commit -m "feat(cli): passphrase-fallback unlock (headless/no-keychain) via injectable seams"
```

---

## Task 8: carry-forward — WAL + SetMaxOpenConns + VerifyToken prefix prefilter

**Files:**
- Modify: `internal/store/store.go` (DSN + MaxOpenConns), `internal/store/projects.go` (VerifyToken prefilter), `internal/store/projects_test.go` (add a prefilter-correctness test)
- Test: as above

**Interfaces:**
- Consumes: existing store internals.
- Produces: `Open` enables WAL journal mode and `db.SetMaxOpenConns(1)`; `VerifyToken` prefilters candidate rows by `token_prefix = first8(token)` before running Argon2id.

- [ ] **Step 1: Enable WAL + cap pool in Open**

In `internal/store/store.go` `Open`, change the DSN and add SetMaxOpenConns after `sql.Open`:
```go
db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
if err != nil {
	return nil, err
}
db.SetMaxOpenConns(1) // SQLite single-writer; serializes access and avoids "database is locked"
```
(Retain the existing `initSchema` + defensive-copy logic after this.)

- [ ] **Step 2: VerifyToken prefix prefilter**

In `internal/store/projects.go` `VerifyToken`, restrict the query to rows whose `token_prefix` matches the first 8 chars of the presented token:
```go
func (s *Store) VerifyToken(token string) (*models.Project, error) {
	prefix := tokenPrefix(token)
	rows, err := s.db.Query(
		`SELECT id,name,token_hash,token_salt,token_prefix,profile_id FROM projects WHERE token_prefix=?`,
		prefix,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p          models.Project
			hash, salt []byte
		)
		if err := rows.Scan(&p.ID, &p.Name, &hash, &salt, &p.TokenPrefix, &p.ProfileID); err != nil {
			return nil, err
		}
		if verifyTokenHash([]byte(token), salt, hash) {
			return &p, nil
		}
	}
	return nil, rows.Err()
}
```

- [ ] **Step 3: Add a prefilter test (wrong prefix never Argon2s; right token still verifies; collisions handled)**

Append to `internal/store/projects_test.go`:
```go
func TestVerifyTokenPrefiltersByPrefix(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	_, token, _ := s.AddProject("p1", pid)

	// a token with a different 8-char prefix must not verify (and returns nil,nil quickly)
	other := "AAAAAAAA" + token[8:] // same length, different prefix
	got, err := s.VerifyToken(other)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("token with wrong prefix must not verify")
	}
	// the real token still verifies
	got, err = s.VerifyToken(token)
	if err != nil || got == nil {
		t.Fatalf("real token must verify: got %v err %v", got, err)
	}
}
```

- [ ] **Step 4: Run all + commit**

Run: `go test ./...`
Expected: all green.

```bash
git add internal/store/store.go internal/store/projects.go internal/store/projects_test.go && git commit -m "feat(store): WAL + MaxOpenConns(1) + VerifyToken token_prefix prefilter"
```

---

## Self-Review

**1. Spec/Plan coverage (Plan 2 = SSH broker layer + owner ssh + audit + 2 carry-forwards):**
- In-process SSH connect + auth (password/plain key/encrypted key) → Task 2. ✓
- Exec (stdout/stderr/exit/timeout) → Task 3. ✓
- `sudo -S` → Task 4. ✓
- host_keys table + TOFU/verify (spec §7) → Task 5 (carry-forward #2). ✓
- owner `ssh <host> [command]` full-access → Task 6. ✓
- audit WriteAudit (spec §7 audit) → Task 6. ✓
- carry-forward #1 passphrase-fallback unlock → Task 7. ✓
- carry-forward #3 WAL + MaxOpenConns + VerifyToken prefilter → Task 8. ✓
- MCP server + Profile enforcement + residual-key startup warning → explicitly **Plan 3**, not this plan.
- owner interactive PTY shell → deferred (this plan does `ssh <host> <command...>` exec only); tracked.

**2. Placeholder scan:** All code blocks complete. Task 7 Step 2 contains one redundant first-`root.Execute` block that the step explicitly instructs removing (use the `SetArgs` form only) — final code is the SetArgs form. No other placeholders.

**3. Type consistency:** `sshbroker.Connect(host, port, user, auth, hostKeyCb)`, `(*Client).Exec(cmd, timeout)`, `(*Client).ExecSudo(cmd, sudoPw, timeout)` consistent across tasks. `HostKeyStore` interface matches `*store.Store` methods `GetHostKey`/`SaveHostKey` (Task 5). `store.AuditRow` fields match `WriteAudit` + the `audit_log` schema columns. `authForServer` returns `ssh.AuthMethod` consumed by owner ssh (Task 6) and later MCP. `cli.keychain`/`cli.passphrasePrompt` seams used by unlock + its test.

**Gap noted for Plan 3:** `authForServer` (Task 6) is the shared seam the MCP `exec_command` will reuse. `VerifyToken` + Profile lookup will be the runtime-enforcement primitives.

---

## Execution Handoff

Plan 2 complete and saved to `docs/superpowers/plans/2026-08-08-ssh-manager-mcp-plan-2-ssh-broker.md`. Execute via superpowers:subagent-driven-development (same flow as Plan 1). Plan 3 (MCP server + Profile enforcement) follows after Plan 2 ships.
