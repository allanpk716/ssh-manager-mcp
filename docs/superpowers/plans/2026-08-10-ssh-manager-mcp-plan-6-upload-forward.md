# ssh-manager-mcp Plan 6 — Upload (scp -r) + Local Port Forwarding (-L)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the remaining gaps to "ssh-functional-equivalence for operating a remote server" (per the user's requirement): **(1) upload** — the broker can push local files/directories to a server (`scp -r` put, single-file + recursive dir), symmetric with `download_file`; **(2) local port forwarding** — the broker opens a `-L` tunnel so the agent can reach a service running on a remote server via a local port. Commands/sudo/download are already ssh-equivalent (Plans 1–5e); interactive shell is explicitly OUT OF SCOPE (user doesn't need it).

**Architecture:** Extends the broker (`internal/sshbroker`) + MCP (`internal/mcpserver`). (1) `Client.Upload(localPath, remotePath, maxBytes)` walks the local path (file or dir) + SFTP-puts it (single file via `sftp.Create`+`io.Copy`; dir via `filepath.Walk`+`sftp.Mkdir`+per-file put), §6-capped on total bytes. (2) `Client.ForwardLocal(localPort, remoteHost, remotePort) → *Tunnel` opens a local `net.Listener` + pipes each accepted connection to `(*ssh.Client).Dial(remoteHost:remotePort)` (the golang.org/x/crypto/ssh forward primitive). The MCP exposes `upload_file` + `forward_port`/`close_port`; **forward_port is the first stateful broker operation** — it holds a long-lived `*ssh.Client` + listener in a `TunnelManager` (in-process, keyed by tunnel id) with idle-timeout + explicit close. All three tools auto-join `BrokerTools` (zero-tolerance surface extends for free) + are profile-gated + audited. The iron rule is unchanged (creds stay in the broker; the agent calls tools + gets results/ports).

**Tech Stack:** Go 1.24; `github.com/pkg/sftp` (already a dep from Plan 5e); `golang.org/x/crypto/ssh` (`(*ssh.Client).Dial` for the forward); the existing `cappedBuffer` (§6) + the Plan-5e eval harness.

## Global Constraints

- **Iron rule + profile gate unchanged.** `UploadForProfile` + `ForwardForProfile` mirror `ExecCommandForProfile`/`DownloadForProfile`: profile gate (`ServersForProfile` contains check) BEFORE any connect/cred lookup, audited on every branch. The agent never touches creds.
- **`upload_file` + `forward_port` + `close_port` join `mcpserver.BrokerTools`** → the §12 safety scorers (`scoreT6` BrokerToolLeak, `scoreT8` CrossProfileReach via download/upload/forward reach) auto-extend with no parallel edit.
- **§6 cap on upload** (total bytes; prevent a runaway upload blowing memory/disk). Forward has no §6 cap (it's a byte-pipe, not retained) but has an **idle-timeout** (default 10 min; closes stale tunnels) + explicit `close_port`.
- **Gated, on-demand (§12.4).** Existing gates carry over; default `go test ./...` self-skips.
- **No regression:** `go test ./...` green; `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/` green; all Plan-5e tests pass.
- **`.gitattributes` LF; `gofmt -l .` empty; `go vet ./...` clean; one logical commit per task; messages end `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.**
- **Branch:** `feat/plan-6-upload-forward`, base master HEAD (Plan 5e merge `9bc9111`).

---

## Scope decisions (surfaced for plan review)

1. **Upload = single-file + recursive dir** (`scp -r` put). The user asked for dir recursion. `Client.Upload` auto-detects file vs dir (dir → `filepath.Walk` + `sftp.Mkdir` + per-file put).
2. **Download stays single-file.** Dir download via an MCP tool result is awkward (huge/multi-file return); the agent can `exec_command` a `tar` + `download_file` the tar. (Symmetric dir-download is a possible future add; not this plan.)
3. **Forward = `-L` only** (the user confirmed). Local listener → remote `host:port` over the SSH connection. `-R` (remote→local) + `-D` (SOCKS) are OUT OF SCOPE.
4. **forward_port is stateful** (the first stateful broker op). It holds a long-lived `*ssh.Client` + `net.Listener` in a `TunnelManager` (in the MCP server process). Lifecycle: explicit `close_port(tunnel_id)` + an idle-timeout goroutine (closes a tunnel with no forwarded bytes for N minutes). The tunnel dies with the MCP server process.
5. **The agent uses a forward via its own network access** (e.g., `Bash("curl http://127.0.0.1:<localport>")`). The broker opens the tunnel + returns the local port; the agent reaches it over loopback. No creds cross that boundary (it's plain TCP).
6. **Interactive shell (`ssh -t`) is OUT OF SCOPE** (user: "不是重点").
7. **§13 conformance** for the new surfaces: upload tested differentially vs real `scp`; forward tested vs real `ssh -L` (forward a known byte, compare). These are deterministic, gated, no LLM.
8. **Carry-forwards STILL deferred** (not Plan 6): `context.Context` threading; server-side max-exec-timeout cap.

---

## File Structure

**New:**
- `internal/sshbroker/upload.go` — `Client.Upload(localPath, remotePath, maxBytes) (UploadResult, error)` (single + dir); `UploadResult{Files, Bytes, Truncated}`.
- `internal/sshbroker/upload_test.go` — in-process unit test via `testsshd` (single-file + dir).
- `internal/sshbroker/tunnel.go` — `Tunnel` struct + `Client.ForwardLocal(localPort, remoteHost, remotePort) (*Tunnel, error)` + `(*Tunnel) Close()` + the accept/pipe loop.
- `internal/sshbroker/tunnel_test.go` — in-process unit test (open tunnel, forward a byte through a loopback sshd, close).
- `internal/mcpserver/tunnels.go` — `TunnelManager` (in-process registry: tunnelID → {Tunnel, ssh.Client, lastActivity}); `Open`/`Close`/idle-timeout sweeper.

**Modified:**
- `internal/mcpserver/server.go` — append `"upload_file"`, `"forward_port"`, `"close_port"` to `BrokerTools`; register them in `NewServer`.
- `internal/mcpserver/core.go` — `UploadForProfile` (mirrors `DownloadForProfile`) + `ForwardForProfile`/`CloseForwardForProfile` (open/close via `TunnelManager`; profile-gated + audited).
- `internal/mcpserver/types.go` — `UploadInput`/`UploadOutput`, `ForwardInput`/`ForwardOutput`, `CloseForwardInput`.
- `internal/mcpserver/server_test.go` + `core_test.go` — cover the 3 new tools.
- `internal/conformance/` — add `TestUploadDifferential` (broker upload vs real `scp`) + `TestForwardDifferential` (broker forward vs real `ssh -L`).
- `internal/eval/` — a §12 upload/forward task (optional; see T5) + the scorers auto-extend via `BrokerTools`.
- `internal/eval/README.md` + `docs/eval/phase3.md` — document upload + forward.

---

## Task 1: Broker Upload (`Client.Upload`, single + dir) + unit test

**Goal:** The broker can push a local file or directory (recursive) to the server over SFTP, §6-capped on total bytes.

**Files:**
- Create: `internal/sshbroker/upload.go`, `internal/sshbroker/upload_test.go`
- Consumes: `c.c` (`*ssh.Client`), `github.com/pkg/sftp`, `cappedBuffer` (§6), `filepath.Walk`.

**Interfaces:**
- Produces: `func (c *Client) Upload(localPath, remotePath string, maxBytes int64) (UploadResult, error)`; `type UploadResult struct { Files int; Bytes int64; Truncated bool }`.

- [ ] **Step 1: Write the failing unit test (`upload_test.go`)** — in-process via testsshd (sftp subsystem already enabled by Plan 5e T1). Mirror `download_test.go`'s helper pattern.

```go
package sshbroker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUpload verifies single-file + recursive-dir upload over SFTP, + §6 capping.
func TestUpload(t *testing.T) {
	host, port, cleanup := startTestSSHD(t) // the existing helper
	defer cleanup()
	c := connectTestClient(t, host, port, "agent")
	defer c.Close()

	// Build a local dir tree: tmp/a.txt + tmp/sub/b.txt
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("file-a\n"), 0644)
	os.MkdirAll(filepath.Join(tmp, "sub"), 0755)
	os.WriteFile(filepath.Join(tmp, "sub", "b.txt"), []byte("file-b\n"), 0644)

	// Single-file upload.
	remoteFile := filepath.Join(t.TempDir(), "up-single.txt")
	if _, err := c.Upload(filepath.Join(tmp, "a.txt"), remoteFile, 0); err != nil {
		t.Fatalf("single Upload: %v", err)
	}
	got, _ := c.Download(remoteFile, 0)
	if got.Content != "file-a\n" {
		t.Fatalf("single round-trip: %q", got.Content)
	}

	// Dir upload (recursive) — remote root under a fresh temp dir.
	remoteDir := filepath.Join(t.TempDir(), "up-dir")
	res, err := c.Upload(tmp, remoteDir, 0)
	if err != nil {
		t.Fatalf("dir Upload: %v", err)
	}
	if res.Files != 2 { // a.txt + sub/b.txt
		t.Fatalf("dir Files=%d, want 2", res.Files)
	}
	// Verify both files landed.
	if g, _ := c.Download(filepath.Join(remoteDir, "a.txt"), 0); g.Content != "file-a\n" {
		t.Fatalf("dir a.txt: %q", g.Content)
	}
	if g, _ := c.Download(filepath.Join(remoteDir, "sub", "b.txt"), 0); g.Content != "file-b\n" {
		t.Fatalf("dir sub/b.txt: %q", g.Content)
	}
	// Cap: maxBytes < total → Truncated.
	if _, err := c.Upload(tmp, filepath.Join(t.TempDir(), "up-cap"), 3); err != nil {
		t.Fatalf("capped Upload: %v", err)
	}
	// (assert Truncated on the returned UploadResult for the capped call)
}
```

NOTE: read `download_test.go`/`helpers_test.go` for the exact `startTestSSHD`/`connectTestClient` names + the testsshd filesystem conventions (the in-process sshd serves the host FS, so temp paths are reachable by both Upload's local-read + SFTP-put + Download's verify). Adapt the assertions to the real helpers.

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/sshbroker/ -run TestUpload -v` → FAIL (`Upload` undefined).

- [ ] **Step 3: Implement `upload.go`**

```go
package sshbroker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
)

// UploadResult holds the outcome of an upload.
type UploadResult struct {
	Files     int    // number of files uploaded
	Bytes     int64  // total bytes uploaded (may be < source size if Truncated)
	Truncated bool   // true if maxBytes was hit mid-upload
}

// Upload copies localPath (a file OR a directory, recursively) to remotePath on the
// server over SFTP — mirrors `scp -r localPath server:remotePath`. A file is put
// directly; a directory is walked (filepath.Walk), each subdir mkdir'd, each file
// sftp.Create'd + io.Copy'd. maxBytes > 0 caps the TOTAL bytes uploaded (the §6
// bound); on cap, Truncated=true + the walk stops. maxBytes == 0 = unlimited.
//
// The local file is read from the broker's filesystem (the agent's machine); the
// agent chooses localPath (it already has the file — Upload just transfers it).
func (c *Client) Upload(localPath, remotePath string, maxBytes int64) (UploadResult, error) {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return UploadResult{}, fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()
	info, err := os.Stat(localPath)
	if err != nil {
		return UploadResult{}, err
	}
	var res UploadResult
	counter := &countingWriter{cap: maxBytes} // wraps cappedBuffer-like accounting; see below
	if info.IsDir() {
		err = uploadDir(sc, localPath, remotePath, counter, &res)
	} else {
		err = uploadFile(sc, localPath, remotePath, counter, &res)
	}
	res.Bytes = counter.total
	res.Truncated = counter.truncated
	return res, err
}

// uploadFile puts a single file. uploadDir walks a dir (mkdir + per-file upload).
// Both stop early if the counter hits its cap (Truncated).
func uploadFile(sc *sftp.Client, localPath, remotePath string, ctr *countingWriter, res *UploadResult) error {
	if ctr.truncated {
		return nil
	}
	in, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := sc.Create(remotePath)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.TeeReader(in, ctr)); err != nil { // see countingWriter note
		return err
	}
	res.Files++
	return nil
}

func uploadDir(sc *sftp.Client, localRoot, remoteRoot string, ctr *countingWriter, res *UploadResult) error {
	return filepath.Walk(localRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || ctr.truncated {
			return err
		}
		rel, _ := filepath.Rel(localRoot, path)
		target := filepath.Join(remoteRoot, rel)
		if info.IsDir() {
			return sc.Mkdir(target)
		}
		return uploadFile(sc, path, target, ctr, res)
	})
}

// countingWriter is a minimal io.Writer that counts bytes + flags truncation at cap,
// WITHOUT retaining content (upload streams to the remote; no need to retain).
type countingWriter struct {
	cap       int64 // 0 = unlimited
	total     int64
	truncated bool
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	if w.cap > 0 && w.total > w.cap {
		w.truncated = true
		// NOTE: a truncated upload leaves a partial remote file; the caller decides
		// whether to sc.Remove it. For the MCP tool, surface Truncated + let the agent
		// retry with a higher cap or a smaller payload.
	}
	return len(p), nil // always accept (the cap is advisory accounting, not a hard stop on the pipe)
}
```

NOTE: the `TeeReader(in, ctr)` pattern counts bytes as they stream to the remote. The `countingWriter.cap` is advisory (it doesn't hard-stop the io.Copy mid-file — that would leave a corrupt remote file mid-write; instead it flags Truncated + the caller surfaces it). For a STRICT hard cap (abort the upload when exceeded), the implementer may instead check `ctr.total > cap` before each file in `uploadDir` + before `uploadFile`; pick the cleaner behavior + document it. The §6 intent (bound total upload size) is met either way.

- [ ] **Step 4: Run the unit test to verify pass** — `go test ./internal/sshbroker/ -run TestUpload -v` → PASS.

- [ ] **Step 5: Verify package + fast-lane** — `go test ./...` green; `gofmt -l . && go vet ./...` clean.

- [ ] **Step 6: Commit** — `feat(sshbroker): Client.Upload (single + recursive dir, §6-capped) (Plan 6 T1)` + body + `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

---

## Task 2: MCP `upload_file` tool

**Goal:** Expose `upload_file` (BrokerTools, profile-gated, audited, §6-capped). Mirror `download_file` (Plan 5e T2).

**Files:**
- Modify: `internal/mcpserver/server.go` (BrokerTools + NewServer), `core.go` (`UploadForProfile`), `types.go` (`UploadInput`/`UploadOutput`), `server_test.go` + `core_test.go`.

- [ ] **Step 1: Tests first** — `TestUploadForProfile` (mirror `TestDownloadForProfile`: in-profile upload round-trips + content matches via Download; out-of-profile → `ErrNotInProfile` + audit `denied`; truncation); a `upload_file` MCP wire test.
- [ ] **Step 2: Run to fail** — symbols undefined.
- [ ] **Step 3: `UploadForProfile`** in `core.go` — mirror `DownloadForProfile` EXACTLY (gate → GetServer → AuthForServer → HostKeyTOFU → Connect → defer Close → audit-on-every-branch). Differences: `Action: "upload"`, `Command:` reused for `localPath -> remotePath`, calls `cli.Upload(localPath, remotePath, MaxOutputBytes)`, builds `UploadOutput{Files, Bytes, Truncated}`. Statuses: `denied`/`auth_error`/`hostkey_mismatch`/`connect_error`/`ok`/`error`.
- [ ] **Step 4: Types** in `types.go`:
```go
type UploadInput struct {
	ServerID   string `json:"server_id" jsonschema:"server id from list_servers"`
	LocalPath  string `json:"local_path" jsonschema:"absolute local path (file or directory) to upload"`
	RemotePath string `json:"remote_path" jsonschema:"absolute remote destination path"`
}
type UploadOutput struct {
	Files     int    `json:"files" jsonschema:"number of files uploaded (>=1; >1 if local_path was a directory)"`
	Bytes     int64  `json:"bytes" jsonschema:"total bytes uploaded"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"true if the size cap was hit mid-upload (partial upload)"`
}
```
- [ ] **Step 5: Register in `server.go`** — append `"upload_file"` to `BrokerTools`; `mcp.AddTool` for it in `NewServer` (mirror the `download_file` block; Description tells the agent to push local files/dirs + that sudo isn't relevant for SFTP).
- [ ] **Step 6: Tests pass** + fast-lane green (the eval scorers auto-include `upload_file` via `BrokerTools`).
- [ ] **Step 7: Commit** — `feat(mcpserver): upload_file tool (BrokerTools, UploadForProfile) (Plan 6 T2)` + Co-Authored-By.

---

## Task 3: Broker local port forward (`Client.ForwardLocal` + `Tunnel`)

**Goal:** The broker can open a `-L` tunnel: a local TCP listener that forwards each connection to `remoteHost:remotePort` over the SSH connection.

**Files:**
- Create: `internal/sshbroker/tunnel.go`, `internal/sshbroker/tunnel_test.go`
- Consumes: `c.c` (`*ssh.Client`), `c.c.Dial("tcp", addr)` (the golang.org/x/crypto/ssh forward primitive), `net.Listen`.

**Interfaces:**
- Produces: `type Tunnel struct`; `func (c *Client) ForwardLocal(localPort int, remoteHost string, remotePort int) (*Tunnel, error)`; `func (t *Tunnel) Close() error`; `func (t *Tunnel) LocalAddr() string`.

- [ ] **Step 1: Write the failing unit test (`tunnel_test.go`)** — in-process: start a testsshd + a dummy "remote service" (a `net.Listen` on a loopback port that echoes bytes); ForwardLocal a tunnel to it; connect to the tunnel's local addr; write a byte; read the echo; assert; Close the tunnel; assert the listener is closed.
```go
// Sketch:
//   1. sshClient := connectTestClient(...)  (the broker Client over testsshd)
//   2. echo := startEchoService(t)  // a net.Listen on 127.0.0.1:0 that echoes
//   3. tun, err := sshClient.ForwardLocal(0, "127.0.0.1", echoPort)  // local 0 = random
//   4. conn, _ := net.Dial("tcp", tun.LocalAddr()); conn.Write([]byte("hi")); read echo → "hi"
//   5. tun.Close(); assert a subsequent Dial to tun.LocalAddr() fails (listener closed)
```
NOTE: `ForwardLocal` forwards to `remoteHost:remotePort` **from the SSH server's perspective**. For the in-process testsshd (same host), `127.0.0.1:<echoPort>` on the sshd host IS the test's echo service. So forwarding to `("127.0.0.1", echoPort)` reaches the echo service. Verify this against testsshd's behavior (the `(*ssh.Client).Dial` opens a connection from the sshd to the addr — for the in-process sshd, that's the host's loopback).

- [ ] **Step 2: Run to fail** — `ForwardLocal` undefined.
- [ ] **Step 3: Implement `tunnel.go`**:
```go
package sshbroker

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/google/uuid" // already an indirect dep (store uses it); promote to direct if needed
	"golang.org/x/crypto/ssh"
)

// Tunnel is a local TCP listener that forwards each accepted connection to a remote
// endpoint over an ssh.Client (the -L forward). The Tunnel owns its listener; Close
// stops accepting + closes the listener (in-flight conns finish their pipe).
type Tunnel struct {
	ID        string
	localAddr string
	listener  net.Listener
	client    *ssh.Client // the long-lived SSH connection the tunnel dials through

	closeOnce sync.Once
	closeErr  error
}

// LocalAddr returns the tunnel's local listen address (e.g. "127.0.0.1:54321").
func (t *Tunnel) LocalAddr() string { return t.localAddr }

// Close stops the accept loop + closes the listener. Idempotent. It does NOT close the
// underlying ssh.Client (the caller — TunnelManager — owns that; closing the client
// happens when the tunnel is torn down at the manager level).
func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() { t.closeErr = t.listener.Close() })
	return t.closeErr
}

// ForwardLocal opens a local TCP listener on localPort (0 = random free port) and
// forwards each accepted connection to remoteHost:remotePort over the client's SSH
// connection (the ssh -L semantic). Returns the Tunnel (whose LocalAddr has the actual
// port). The caller keeps the Tunnel alive + closes it when done.
func (c *Client) ForwardLocal(localPort int, remoteHost string, remotePort int) (*Tunnel, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("local listen %s: %w", addr, err)
	}
	t := &Tunnel{
		ID:        uuid.NewString(),
		localAddr: ln.Addr().String(),
		listener:  ln,
		client:    c.c,
	}
	go t.serve(remoteHost, remotePort)
	return t, nil
}

// serve accepts local conns + pipes each to the remote endpoint via the ssh client.
func (t *Tunnel) serve(remoteHost string, remotePort int) {
	remote := fmt.Sprintf("%s:%d", remoteHost, remotePort)
	for {
		local, err := t.listener.Accept()
		if err != nil {
			return // listener closed (Close)
		}
		go t.handle(local, remote)
	}
}

func (t *Tunnel) handle(local net.Conn, remote string) {
	defer local.Close()
	rem, err := t.client.Dial("tcp", remote)
	if err != nil {
		return
	}
	defer rem.Close()
	// pipe both ways; when either side closes, both conns defer-close
	done := make(chan struct{}, 2)
	go func() { io.Copy(rem, local); done <- struct{}{} }()
	go func() { io.Copy(local, rem); done <- struct{}{} }()
	<-done
}
```
NOTE: `github.com/google/uuid` — confirm it's available (it's an indirect dep via the store/MCP SDK; promote to direct if `go mod tidy` requires). If you'd rather avoid the dep, generate the tunnel ID from a counter + the local addr (uniqueness within the process is enough — the TunnelManager keys by it).

- [ ] **Step 4: Run the unit test to verify pass** — `go test ./internal/sshbroker/ -run TestForwardLocal -v` → PASS (echo through the tunnel + close).
- [ ] **Step 5: Verify package + fast-lane** — `go test ./...` green; `gofmt`/`vet` clean.
- [ ] **Step 6: Commit** — `feat(sshbroker): Client.ForwardLocal + Tunnel (ssh -L) (Plan 6 T3)` + Co-Authored-By.

---

## Task 4: MCP `forward_port` + `close_port` + TunnelManager

**Goal:** The MCP exposes the forward with a managed lifecycle. `TunnelManager` (in-process) holds the long-lived ssh.Client + Tunnel per tunnel id; `forward_port` opens (profile-gated, audited, returns the local port); `close_port` closes; an idle-timeout sweeper closes stale tunnels.

**Files:**
- Create: `internal/mcpserver/tunnels.go` (`TunnelManager`)
- Modify: `internal/mcpserver/server.go` (BrokerTools + NewServer), `core.go` (`ForwardForProfile`/`CloseForwardForProfile`), `types.go` (`ForwardInput`/`ForwardOutput`/`CloseForwardInput`), `server_test.go` + `core_test.go`.

- [ ] **Step 1: `TunnelManager` (`tunnels.go`)**:
```go
package mcpserver

import (
	"sync"
	"time"

	"ssh-manager-mcp/internal/sshbroker"
)

const forwardIdleTimeout = 10 * time.Minute

// managedTunnel is a forward held by the TunnelManager.
type managedTunnel struct {
	tunnel       *sshbroker.Tunnel
	client       *sshbroker.Client // the long-lived SSH connection (closed on Close)
	lastActivity time.Time
}

// TunnelManager holds the open forwards for the MCP server process, keyed by tunnel id.
// forward_port opens; close_port closes; the idle sweeper closes tunnels with no recent
// activity (forwardIdleTimeout). All state is in-process (dies with the MCP server).
type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]*managedTunnel
}

func NewTunnelManager() *TunnelManager { return &TunnelManager{tunnels: map[string]*managedTunnel{}} }

// Open registers a tunnel + its owning client (the client stays open for the tunnel's life).
func (m *TunnelManager) Open(t *sshbroker.Tunnel, c *sshbroker.Client) string {
	m.mu.Lock(); defer m.mu.Unlock()
	m.tunnels[t.ID] = &managedTunnel{tunnel: t, client: c, lastActivity: time.Now()}
	return t.ID
}

// Close tears down a tunnel (listener + the owning ssh client).
func (m *TunnelManager) Close(id string) bool {
	m.mu.Lock(); defer m.mu.Unlock()
	mt, ok := m.tunnels[id]
	if !ok { return false }
	_ = mt.tunnel.Close()
	_ = mt.client.Close()
	delete(m.tunnels, id)
	return true
}

// SweepIdle closes tunnels idle longer than forwardIdleTimeout. Called periodically.
func (m *TunnelManager) SweepIdle() []string {
	m.mu.Lock(); defer m.mu.Unlock()
	var closed []string
	for id, mt := range m.tunnels {
		if time.Since(mt.lastActivity) > forwardIdleTimeout {
			_ = mt.tunnel.Close(); _ = mt.client.Close()
			delete(m.tunnels, id); closed = append(closed, id)
		}
	}
	return closed
}
```
(The idle sweeper is launched as a goroutine in `NewServer` (or the MCP server's start), ticking every minute to call `SweepIdle`. Note: tracking per-tunnel byte activity precisely is complex; for the MVP, `lastActivity` = open time, and the sweeper closes tunnels open longer than `forwardIdleTimeout` unless re-asserted. The implementer may add a `Touch(id)` the agent calls to keep it alive, OR keep it simple: idle = open-duration. Document the choice.)

- [ ] **Step 2: `ForwardForProfile` + `CloseForwardForProfile`** in `core.go` — `ForwardForProfile`: gate (profile) → GetServer → AuthForServer → HostKeyTOFU → Connect (a FRESH long-lived client for the tunnel — do NOT defer-close it; the TunnelManager owns it) → `cli.ForwardLocal(localPort, remoteHost, remotePort)` → `tunnelManager.Open(tunnel, cli)` → audit `Action:"forward"` status ok → return `{tunnel_id, local_port}`. `CloseForwardForProfile`: `tunnelManager.Close(tunnelID)` → audit `Action:"close-forward"`. (Profile gate: only the server the tunnel targets; the tunnelID is opaque to the agent so close_port doesn't re-gate, but it only closes tunnels THIS manager owns.)
- [ ] **Step 3: Types** in `types.go`:
```go
type ForwardInput struct {
	ServerID    string `json:"server_id" jsonschema:"server id from list_servers (the SSH endpoint)`
	RemoteHost  string `json:"remote_host" jsonschema:"the host TO forward to, from the server's perspective (often '127.0.0.1')"`
	RemotePort  int    `json:"remote_port" jsonschema:"the port on remote_host to reach"`
	LocalPort   int    `json:"local_port,omitempty" jsonschema:"optional local listen port (0/random if omitted)"`
}
type ForwardOutput struct {
	TunnelID  string `json:"tunnel_id" jsonschema:"opaque id; pass to close_port when done"`
	LocalPort int    `json:"local_port" jsonschema:"the local port now forwarding to remote_host:remote_port — reach it via 127.0.0.1:local_port"`
}
type CloseForwardInput struct {
	TunnelID string `json:"tunnel_id"`
}
```
- [ ] **Step 4: Register in `server.go`** — append `"forward_port"`, `"close_port"` to `BrokerTools`; `mcp.AddTool` both in `NewServer`; construct + hold the `TunnelManager` in `NewServer` (pass to the handlers); launch the idle sweeper goroutine. Descriptions: `forward_port` tells the agent it opens a local port forwarding to a remote service (use via `curl 127.0.0.1:<local_port>`), returns a tunnel_id; `close_port` closes it (call when done; tunnels also auto-close after idle).
- [ ] **Step 5: Tests** — `TestForwardForProfile` (in-profile open → returns local_port + tunnel_id; out-of-profile → denied + audit; the TunnelManager holds the tunnel); `TestCloseForward` (close tears down); the MCP wire test (forward_port → ForwardOutput; close_port → ok). Fast-lane green; scorers auto-extend (`forward_port`/`close_port` join BrokerTools).
- [ ] **Step 6: Commit** — `feat(mcpserver): forward_port + close_port + TunnelManager (ssh -L, stateful) (Plan 6 T4)` + Co-Authored-By. Note: forward_port is the first STATEFUL broker op (long-lived client + tunnel; TunnelManager lifecycle).

---

## Task 5: §13 conformance (upload vs scp; forward vs ssh -L) + optional §12 task

**Goal:** Deterministic proof the new surfaces match real openssh: broker upload == real `scp -r`; broker forward == real `ssh -L`.

**Files:**
- Modify: `internal/conformance/` (add `TestUploadDifferential` + `TestForwardDifferential`).

- [ ] **Step 1: `TestUploadDifferential`** — against the conformance Docker sshd (real openssh): upload a known dir tree via the broker; verify the remote files (download via broker + content match) AND verify the result is byte-identical to a real `scp -r` of the same tree (run `scp -r` via `os/exec` against the same sshd + diff). Gated `SSHMGR_CONFORMANCE=1`.
- [ ] **Step 2: `TestForwardDifferential`** — start a remote echo service (in the conformance container, or on a known loopback port the sshd can reach); open a broker `-L` tunnel to it; send a byte + read the echo. Then do the same via real `ssh -L` (spawn `ssh -L <port>:127.0.0.1:<echoport> ...` + curl); compare the byte round-trip. Gated.
- [ ] **Step 3: Optional §12 task** — if you want agent coverage: add a §12 task "upload a config file to the gpu server + verify it landed" (uses `upload_file`) + maybe a forward task. The scorers auto-extend via BrokerTools. (This is OPTIONAL — the deterministic conformance covers correctness; the §12 task covers agent-usability. Given the §12 suite is already comprehensive, a single upload task is enough; skip the forward §12 task unless you want agent-tested forwarding.)
- [ ] **Step 4: Gated green** — `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/` green.
- [ ] **Step 5: Commit** — `test(conformance): upload vs scp + forward vs ssh -L differential (Plan 6 T5)` + Co-Authored-By.

---

## Task 6: Gate re-run (glm) + docs + final opus review + merge

**Goal:** Confirm no regression (the new tools didn't break the §12 gate), document upload + forward, final review, merge.

**Files:**
- Modify: `internal/eval/README.md`, `docs/eval/phase3.md`, `internal/eval/summary_test.go` (if a §12 upload task was added).

- [ ] **Step 1: Full §12 gate on glm (local surrogate)** — `SSHMGR_GATE=1 ANTHROPIC_API_KEY=eval go test ./internal/eval/ -run TestEvalGate -v` (no Fable-5 needed for regression — the new tools are broker features; the gate confirms the existing T1-T8 still pass + the scorers auto-extended cleanly). Optional: a Fable-5 gate if you want authoritative upload/forward agent numbers (but the §13 conformance is the load-bearing correctness proof for these).
- [ ] **Step 2: Docs** — README + phase3: document `upload_file` (single + dir) + `forward_port`/`close_port` (-L, stateful, idle-timeout); note the agent uses a forward via `curl 127.0.0.1:<port>`; note download stays single-file (tar workaround for dirs); note interactive shell is intentionally not provided.
- [ ] **Step 3: Final whole-branch opus review** — `scripts/review-package 9bc9111 HEAD`; opus review (the upload §6 cap, the forward lifecycle/TunnelManager correctness, the stateful-client resource cleanup, the profile gate on forward, the iron rule preserved — upload reads agent-local files but exposes no creds; forward is plain TCP). Resolve findings in one fix wave.
- [ ] **Step 4: Merge to master (`--no-ff`)** per the user's finishing choice.

---

## Self-Review (run before handoff)

1. **Spec coverage:** ssh-functional-equivalence (minus interactive shell): commands ✓ (existing), download ✓ (existing), **upload ✓ (T1-T2, +dir)**, **forward -L ✓ (T3-T4)**. Interactive shell OUT OF SCOPE (documented). Iron rule preserved (creds in broker; upload/forward profile-gated + audited; auto-join BrokerTools zero-tol).
2. **Placeholder scan:** the testsshd filesystem convention (T1/T3), the uuid dep promotion (T3), the idle-sweeper "Touch" design choice (T4) — these are genuine implementer-resolves items with the contracts specified, not TBDs. No `<...>` placeholders.
3. **Type consistency:** `UploadResult{Files,Bytes,Truncated}` → `UploadOutput{Files,Bytes,Truncated}`; `Tunnel{ID,localAddr,...}` → `ForwardOutput{TunnelID,LocalPort}`; `BrokerTools` append (upload_file/forward_port/close_port) referenced by the scorers + the new ForProfile funcs. `TunnelManager` keyed by tunnel ID.
4. **Scope:** 6 tasks. T1-T2 (upload, no LLM); T3-T4 (forward, the stateful piece — most complex); T5 (§13 conformance, gated no-LLM); T6 (gate + docs + review + merge). Carry-forwards deferred. The forward lifecycle (TunnelManager + stateful client) is the load-bearing new design — reviewers must verify resource cleanup (tunnel close → listener + ssh client closed; idle sweeper; MCP-server-shutdown cleanup).

---

## Execution Handoff

**Subagent-Driven (recommended):** T1-T4 sonnet (no LLM — broker + MCP + unit/conformance tests); T5 sonnet (gated §13 conformance, no LLM); T6 sonnet + final opus whole-branch review. **No Fable-5/$ required** for correctness (the §13 differential conformance is the proof; the §12 gate is regression-only on glm). Merge per the user's choice (--no-ff to master, matching 5c/5d/5e).

**Honest scope note:** the forward (T3-T4) is the first STATEFUL broker operation (long-lived ssh.Client + listener + TunnelManager). The reviewers must verify resource cleanup + the idle-sweeper + MCP-shutdown teardown carefully — that's the new risk surface. Upload (T1-T2) mirrors download (well-trodden). If a smaller cut is wanted: T1-T2 (upload) alone deliver scp-equivalence; T3-T4 (forward) + T5 can be a follow-up. Recommend the full plan (both close the ssh-equivalence gap the user asked for).
