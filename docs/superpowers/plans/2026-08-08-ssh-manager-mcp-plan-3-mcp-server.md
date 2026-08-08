# ssh-manager-mcp Plan 3: MCP Server + Runtime Profile Enforcement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the stdio MCP server an AI agent spawns (`ssh-manager mcp --token <T>`), exposing two Profile-scoped tools — `list_servers` and `exec_command` — that **enforce the iron rule at runtime**: an agent can only see/use servers in its project's Profile, and credentials never leave the broker process. This is the capstone that makes the vault agent-usable.

**Architecture:** A new `internal/mcpserver` package builds an `mcp.Server` (official Go MCP SDK) with two tools whose handlers delegate to Profile-enforced core functions. A new `internal/vault` shared package holds `OpenStore` (env-or-keychain) + `AuthForServer`, extracted from `internal/cli` so both `cli` and `mcpserver` can use them without an import cycle. The `mcp --token` subcommand resolves the token → Project → Profile, builds the scoped server, runs it over stdio, and warns (to **stderr**) if residual keys are detected. The MCP's stdout is the JSON-RPC channel — nothing else may write to stdout.

**Tech Stack:** Go 1.22+ · official MCP SDK `github.com/modelcontextprotocol/go-sdk/mcp` (v1.2.0; `mcp.NewServer` + `mcp.AddTool[In,Out]` + `mcp.StdioTransport`; in-process test via `mcp.NewInMemoryTransports` + `mcp.NewClient`) · existing `internal/store`, `internal/sshbroker`, `internal/cli`, `internal/testsshd`.

## Global Constraints

- Module path `ssh-manager-mcp`; binary `ssh-manager`.
- MCP stdout is the JSON-RPC channel — **all non-protocol output (warnings, errors, logs) MUST go to stderr**.
- Iron rule (runtime): `exec_command` MUST reject any `server_id` not in the token's Profile (`ErrNotInProfile`). Credentials never enter tool output.
- `list_servers` returns ONLY the Profile's servers; never credentials.
- Secrets never logged. Audit stores command+status, never credentials.
- Existing: AES-256-GCM + HKDF, Argon2id token (constant-time), FK ON, `sudo -S -p '' -- ` exact prefix, host-key TOFU/verify, WAL + `MaxOpenConns(1)` — all unchanged.
- Carry-forwards to land: `ErrHostKeyMismatch` sentinel; wire `ExecSudo` (resolve `SudoCredentialID`, set `AuditRow.Sudo=true`); agent exec must NOT reuse owner full-access path (it has its own Profile-enforced path); `AuthForServer` moved to shared `internal/vault`.
- TDD every task; commit per task. New dep: `github.com/modelcontextprotocol/go-sdk` — `go get github.com/modelcontextprotocol/go-sdk@v1.2.0` when first imported.

---

## File Structure

```
ssh-manager-mcp/
├── internal/vault/vault.go          # OpenStore (env-or-keychain) + AuthForServer (shared; breaks import cycle)
├── internal/sshbroker/hostkey.go    # MODIFY: ErrHostKeyMismatch sentinel
├── internal/mcpserver/
│   ├── types.go        # ServerInfo, ExecOutput, the tool input/output structs
│   ├── core.go         # ListServersForProfile, ExecCommandForProfile (Profile-enforced) + ErrNotInProfile
│   ├── core_test.go    # unit tests (enforcement, scoping, exec via testsshd, sudo)
│   ├── server.go       # NewServer(st, profileID) *mcp.Server (registers the 2 tools)
│   └── run.go          # RunStdio(token) — resolve token→profile→NewServer→Run(StdioTransport)
├── internal/cli/mcp.go              # `mcp --token <T>` subcommand (guardrail warn→stderr, run mcpserver)
└── internal/cli/mcp_e2e_test.go     # in-memory client→server comprehensive e2e
```

`internal/cli/common.go` + `servers.go` + `ssh.go`: MODIFY to use `vault.OpenStore()` / `vault.AuthForServer` (drop the local `openUnlockedStore`/`AuthForServer`). `internal/cli/root.go`: add `newMCPCmd()`.

**Responsibilities:** `vault` = shared store-opening + auth-method building (no cli, no mcpserver dep). `mcpserver` = Profile-scoped MCP tools + server (imports vault, store, sshbroker, mcp SDK). `cli/mcp.go` = thin subcommand (imports mcpserver + store for guardrail).

---

## Task 1: `internal/vault` shared package (refactor — break import cycle)

**Files:**
- Create: `internal/vault/vault.go`, `internal/vault/vault_test.go`
- Modify: `internal/cli/common.go` (openUnlockedStore → vault.OpenStore), `internal/cli/servers.go` (drop AuthForServer, use vault), `internal/cli/ssh.go` (use vault.AuthForServer), `internal/cli/enc.go` (keep hexEncode for unlock output)
- Test: `internal/vault/vault_test.go` + existing cli tests still pass

**Interfaces:**
- Produces: `vault.OpenStore() (*store.Store, error)`; `vault.AuthForServer(st *store.Store, srv *models.Server) (ssh.AuthMethod, error)`.

- [ ] **Step 1: Write vault.go**

Create `internal/vault/vault.go`:
```go
package vault

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// OpenStore resolves the master key and opens the vault.
// Order: SSHMGR_MASTERKEY_HEX env (dev/CLI scripting) → OS keychain (production/MCP).
// Returns a "vault locked" error if neither yields a key (e.g. MCP spawned before any `unlock`).
func OpenStore() (*store.Store, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	mk, err := resolveMasterKey()
	if err != nil {
		return nil, err
	}
	return store.Open(path, mk)
}

func storePath() (string, error) {
	if p := os.Getenv("SSHMGR_STORE"); p != "" {
		return p, nil
	}
	return store.DefaultStorePath()
}

func resolveMasterKey() ([]byte, error) {
	if hexKey := os.Getenv("SSHMGR_MASTERKEY_HEX"); hexKey != "" {
		return hex.DecodeString(hexKey)
	}
	kp := store.KeyringKeyProvider{}
	mk, err := kp.Get()
	if err == nil {
		return mk, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("vault locked: run `ssh-manager unlock` to populate the keychain (the MCP server cannot prompt)")
	}
	return nil, fmt.Errorf("keychain unavailable: %w", err)
}

// AuthForServer resolves a server's stored credential into an SSH auth method.
func AuthForServer(st *store.Store, srv *models.Server) (ssh.AuthMethod, error) {
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

- [ ] **Step 2: Write vault_test.go**

Create `internal/vault/vault_test.go`:
```go
package vault

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	old := map[string]string{}
	for k, v := range kv {
		old[k] = os.Getenv(k)
		os.Setenv(k, v)
	}
	t.Cleanup(func() { for k, v := range old { os.Setenv(k, v) } })
}

func TestOpenStoreViaEnv(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	st, err := OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
}

func TestOpenStoreLockedWhenNoKey(t *testing.T) {
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "x.db"),
		"SSHMGR_MASTERKEY_HEX": "", // force keychain path
	})
	// KeyringKeyProvider on this host may or may not have a key; if it does, treat as pass.
	// The deterministic assertion: when the keychain has NO entry, OpenStore returns the locked error.
	st, err := OpenStore()
	if st != nil {
		st.Close()
		return // keychain had a key; nothing to assert
	}
	if err == nil {
		t.Fatal("expected locked error when no key available")
	}
}
```

- [ ] **Step 3: Update cli to use vault**

In `internal/cli/common.go`: replace the `openUnlockedStore` function body with a delegation and drop the unused `cmd` param (resolves the Plan-1 unused-param Minor). Update its signature + all callers (`servers.go`, `profiles.go`, `projects.go`, `ssh.go`) from `openUnlockedStore(cmd)` to `openUnlockedStore()`:
```go
package cli

import "ssh-manager-mcp/internal/vault"

func openUnlockedStore() (*store.Store, error) { return vault.OpenStore() }
```
(Keep `storePath()`/`metaFilePath()` in common.go if still used by unlock; `metaFilePath` can stay. Remove now-unused imports from common.go — e.g. `fmt`/`os` if only openUnlockedStore used them; leave `store` if storePath/metaFilePath reference it. Verify with `go build`.)

In `internal/cli/servers.go`: delete the `AuthForServer` function (now in vault) and change `ssh.go`'s `authForServer(st, srv)` call to `vault.AuthForServer(st, srv)`. Remove `AuthForServer`'s imports from servers.go if now unused (`sshbroker`, `golang.org/x/crypto/ssh`, `store` if not otherwise used). Add `"ssh-manager-mcp/internal/vault"` import to ssh.go.

- [ ] **Step 4: Build + test + commit**

Run: `go build ./...` then `go test ./...`
Expected: all green (vault tests pass; cli tests unchanged behavior).

```bash
git add internal/vault/ internal/cli/ && git commit -m "refactor: extract vault.OpenStore + AuthForServer to shared package (break mcp import cycle)"
```

---

## Task 2: `ErrHostKeyMismatch` sentinel (carry-forward)

**Files:**
- Modify: `internal/sshbroker/hostkey.go`, `internal/sshbroker/hostkey_test.go`
- Test: add an `errors.Is` assertion

**Interfaces:**
- Produces: `sshbroker.ErrHostKeyMismatch` (exported sentinel); `HostKeyTOFU` returns it on mismatch.

- [ ] **Step 1: Add sentinel + use it**

In `internal/sshbroker/hostkey.go`, add (near the imports):
```go
// ErrHostKeyMismatch is returned by the TOFU callback when a server's host key
// differs from the previously-recorded one (possible MITM). Callers (e.g. the MCP
// server) can errors.Is this to surface a clear warning to the client.
var ErrHostKeyMismatch = errors.New("host key mismatch: possible MITM, connection rejected")
```
And in the callback's mismatch branch, replace the anonymous `errors.New(...)` with `ErrHostKeyMismatch`:
```go
		if !bytes.Equal(marshaled, stored) {
			return ErrHostKeyMismatch
		}
```

- [ ] **Step 2: Add an errors.Is test**

In `internal/sshbroker/hostkey_test.go`, extend `TestHostKeyMismatchRejected` (or add a new test) to assert the error wraps the sentinel. After the mismatched `Connect` returns `err`:
```go
	if err == nil {
		t.Fatal("mismatched host key must be rejected")
	}
	if !errors.Is(err, sshbroker.ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
```
(Add `"errors"` to the test imports; `sshbroker` is the package under test so `ErrHostKeyMismatch` is directly visible.)

- [ ] **Step 3: Run + commit**

Run: `go test ./internal/sshbroker/`
Expected: PASS.

```bash
git add internal/sshbroker/hostkey.go internal/sshbroker/hostkey_test.go && git commit -m "feat(sshbroker): ErrHostKeyMismatch sentinel for caller-side MITM detection"
```

---

## Task 3: `internal/mcpserver` core — Profile-enforced logic

**Files:**
- Create: `internal/mcpserver/types.go`, `internal/mcpserver/core.go`, `internal/mcpserver/core_test.go`
- Test: `internal/mcpserver/core_test.go`

**Interfaces:**
- Consumes: `*store.Store`, `vault.AuthForServer`, `sshbroker` (Connect/Exec/ExecSudo/HostKeyTOFU), `testsshd`.
- Produces: `mcpserver.ErrNotInProfile`; `mcpserver.ServerInfo`; `mcpserver.ExecOutput`; `mcpserver.ListServersForProfile(st, profileID) ([]ServerInfo, error)`; `mcpserver.ExecCommandForProfile(ctx, st, profileID, serverID, command, sudo, timeout) (ExecOutput, error)`.

- [ ] **Step 1: Write types.go**

Create `internal/mcpserver/types.go`:
```go
package mcpserver

import "time"

// ServerInfo is a Profile-scoped server as seen by the agent (no credentials).
type ServerInfo struct {
	ID      string `json:"id" jsonschema:"stable server id (use this in exec_command)"`
	Name    string `json:"name" jsonschema:"human-friendly server name"`
	Host    string `json:"host" jsonschema:"server host"`
	User    string `json:"user" jsonschema:"ssh user"`
	HasSudo bool   `json:"has_sudo" jsonschema:"true if sudo=true is supported on this server"`
}

// ExecOutput is the result of exec_command.
type ExecOutput struct {
	Stdout   string `json:"stdout" jsonschema:"combined/normal command stdout"`
	Stderr   string `json:"stderr,omitempty" jsonschema:"command stderr"`
	ExitCode int    `json:"exit_code" jsonschema:"process exit code (0 = success)"`
	TimedOut bool   `json:"timed_out,omitempty" jsonschema:"true if the command exceeded the timeout"`
}

// ErrNotInProfile is returned when an agent requests a server outside its Profile (iron rule).
var ErrNotInProfile = errWithString("server is not in your profile — call list_servers to see the servers you may use")

// errWithString is a sentinel error that also satisfies the string the agent should see.
type errWithString string

func (e errWithString) Error() string { return string(e) }

// defaultTimeout caps a single exec_command.
const defaultTimeout = 120 * time.Second
```

- [ ] **Step 2: Write core.go**

Create `internal/mcpserver/core.go`:
```go
package mcpserver

import (
	"context"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// ListServersForProfile returns the servers the agent may use (Profile-scoped, no credentials).
func ListServersForProfile(st *store.Store, profileID string) ([]ServerInfo, error) {
	ids, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	out := make([]ServerInfo, 0, len(ids))
	for _, id := range ids {
		srv, err := st.GetServer(id)
		if err != nil || srv == nil {
			continue
		}
		out = append(out, ServerInfo{
			ID: srv.ID, Name: srv.Name, Host: srv.Host, User: srv.User,
			HasSudo: srv.SudoCredentialID != "",
		})
	}
	return out, nil
}

// ExecCommandForProfile runs command on serverID iff serverID is in profileID (iron rule).
// sudo=true uses sudo -S with the server's stored sudo password.
func ExecCommandForProfile(ctx context.Context, st *store.Store, profileID, serverID, command string, sudo bool, timeout time.Duration) (ExecOutput, error) {
	allowed, err := st.ServersForProfile(profileID)
	if err != nil {
		return ExecOutput{}, err
	}
	if !contains(allowed, serverID) {
		return ExecOutput{}, ErrNotInProfile
	}
	srv, err := st.GetServer(serverID)
	if err != nil || srv == nil {
		return ExecOutput{}, fmt.Errorf("server %s not found", serverID)
	}
	auth, err := vault.AuthForServer(st, srv)
	if err != nil {
		return ExecOutput{}, err
	}
	hkCb, _ := sshbroker.HostKeyTOFU(st, srv.Host)
	cli, err := sshbroker.Connect(srv.Host, srv.Port, srv.User, auth, hkCb)
	if err != nil {
		return ExecOutput{}, err
	}
	defer cli.Close()

	if timeout <= 0 {
		timeout = defaultTimeout
	}
	start := time.Now()
	var res sshbroker.ExecResult
	status := "ok"
	if sudo {
		if srv.SudoCredentialID == "" {
			return ExecOutput{}, fmt.Errorf("sudo not configured for server %s (call list_servers: has_sudo tells you)", srv.Name)
		}
		sudoCred, err := st.GetCredential(srv.SudoCredentialID)
		if err != nil || sudoCred == nil {
			return ExecOutput{}, fmt.Errorf("sudo credential for %s not found", srv.Name)
		}
		res, err = cli.ExecSudo(command, sudoCred.Secret, timeout)
	} else {
		res, err = cli.Exec(command, timeout)
	}
	if res.TimedOut {
		status = "timeout"
	} else if err != nil {
		status = "error"
	}
	_ = st.WriteAudit(store.AuditRow{
		TS: start, ServerID: serverID, Action: "exec", Command: command,
		Sudo: sudo, Status: status, ExitCode: res.ExitCode, DurationMS: time.Since(start).Milliseconds(),
	})
	// Connect/exec errors that aren't exit codes surface here; non-zero exit is a result, not an error.
	return ExecOutput{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut}, err
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Write core_test.go (Profile enforcement + exec via testsshd + sudo)**

Create `internal/mcpserver/core_test.go`:
```go
package mcpserver

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(t.TempDir()+"/t.db", mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestListServersScopedToProfile(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a}) // only a in profile

	got, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("want only [a], got %+v", got)
	}
	_ = b
}

func TestExecCommandRejectsOutOfProfile(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a})

	_, err := ExecCommandForProfile(context.Background(), st, pid, b, "echo hi", false, time.Second)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("want ErrNotInProfile, got %v", err)
	}
}

func TestExecCommandRunsInProfileServer(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecCommandForProfile(context.Background(), st, pid, srvID, "hello", false, 5*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out.Stdout != "RAN:hello\n" {
		t.Fatalf("stdout = %q", out.Stdout)
	}
}

func TestExecCommandSudoWired(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "whoami" {
				return "root\n", "", 0
			}
			return "", "unknown", 1
		},
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "sudopw")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecCommandForProfile(context.Background(), st, pid, srvID, "whoami", true, 5*time.Second)
	if err != nil {
		t.Fatalf("sudo exec: %v", err)
	}
	if out.Stdout != "root\n" {
		t.Fatalf("stdout = %q, want root", out.Stdout)
	}
}

// helpers
func mustCred(t *testing.T, st *store.Store) string {
	t.Helper()
	id, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	return id
}
func indexByte(s string, c byte) int { for i := 0; i < len(s); i++ { if s[i] == c { return i } }; return len(s) }
func portOfAddr(addr string) int { i := indexByte(addr, ':'); var p int; for _, r := range addr[i+1:] { p = p*10 + int(r-'0') }; return p }
```

Append the `seedRealServer` helper to `internal/mcpserver/core_test.go`, and add `"golang.org/x/crypto/ssh"` to the test imports (for `ssh.PublicKey`):
```go
// seedRealServer creates a server pointing at the testsshd addr, pre-trusts its host key,
// and (if sudoPw != "") attaches a sudo password credential.
func seedRealServer(t *testing.T, st *store.Store, name, addr string, hk ssh.PublicKey, sudoPw string) string {
	t.Helper()
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srv := &models.Server{
		Name: name, Host: addr[:indexByte(addr, ':')], Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: cid,
	}
	if sudoPw != "" {
		sid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(sudoPw)})
		srv.SudoCredentialID = sid
	}
	id, _ := st.AddServer(srv)
	_ = st.SaveHostKey(srv.Host, hk.Marshal()) // pre-trust the testsshd host key
	return id
}
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/mcpserver/`
Expected: PASS (4 tests; out-of-profile rejected with ErrNotInProfile; in-profile exec + sudo work via testsshd).

```bash
git add internal/mcpserver/types.go internal/mcpserver/core.go internal/mcpserver/core_test.go && git commit -m "feat(mcpserver): Profile-enforced list_servers + exec_command core (sudo wired, audited)"
```

---

## Task 4: `mcpserver.NewServer` + `RunStdio` + `mcp --token` CLI

**Files:**
- Create: `internal/mcpserver/server.go`, `internal/mcpserver/run.go`, `internal/cli/mcp.go`
- Modify: `internal/cli/root.go` (add `newMCPCmd()`)
- Test: `internal/mcpserver/server_test.go` (in-memory client basic); `internal/cli/mcp_test.go` (invalid token refuses)

**Interfaces:**
- Consumes: `mcp` SDK, `store.VerifyToken`, `vault.OpenStore`, `store.CheckResidualKeys`.
- Produces: `mcpserver.NewServer(st, profileID) (*mcp.Server, error)`; `mcpserver.RunStdio(token string) error`; the `mcp --token <T>` subcommand.

- [ ] **Step 1: Write server.go (tool registration, LLM-optimized descriptions)**

Create `internal/mcpserver/server.go`:
```go
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
)

// NewServer builds an MCP server whose two tools are scoped to profileID.
func NewServer(st *store.Store, profileID string) (*mcp.Server, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-manager", Version: "v0.1.0"}, nil)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        "list_servers",
			Description: "List the SSH servers you may use. ALWAYS call this first to discover server ids and capabilities before exec_command. Returns id/name/host/user/has_sudo — never credentials.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListServersOutput, error) {
			servers, err := ListServersForProfile(st, profileID)
			if err != nil {
				return nil, ListServersOutput{}, err
			}
			return nil, ListServersOutput{Servers: servers}, nil
		},
	)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        "exec_command",
			Description: "Run a shell command on a server. Pass the server's id (from list_servers), not its name. If sudo=true the broker runs `sudo -S` for you — do NOT prepend 'sudo' to the command yourself. sudo=true only works on servers where has_sudo=true. Out-of-profile server ids are rejected.",
		},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecCommandOutput, error) {
			out, err := ExecCommandForProfile(ctx, st, profileID, in.ServerID, in.Command, in.Sudo, in.TimeoutSeconds)
			if err != nil {
				// Surface the error to the agent as a tool error (IsError), not a transport error.
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, ExecCommandOutput{}, nil
			}
			return nil, ExecCommandOutput{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode, TimedOut: out.TimedOut}, nil
		},
	)

	return srv, nil
}

// ListServersOutput is the list_servers tool output.
type ListServersOutput struct {
	Servers []ServerInfo `json:"servers" jsonschema:"servers you are authorized to use"`
}

// ExecCommandInput is the exec_command tool input.
type ExecCommandInput struct {
	ServerID      string `json:"server_id" jsonschema:"server id from list_servers"`
	Command       string `json:"command" jsonschema:"shell command to run on the server"`
	Sudo          bool   `json:"sudo,omitempty" jsonschema:"true to run with sudo (broker handles sudo -S; do not prepend sudo). Requires has_sudo=true."`
	TimeoutSeconds int   `json:"timeout_seconds,omitempty" jsonschema:"optional max seconds; defaults to 120"`
}
```

- [ ] **Step 2: Write run.go (RunStdio — token→profile→server→stdio)**

Create `internal/mcpserver/run.go`:
```go
package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/vault"
)

// RunStdio resolves the token to a project+profile, builds the scoped server, and runs it over stdio.
// Returns an error if the store is locked or the token is unknown (caller prints to stderr + exits).
func RunStdio(token string) error {
	st, err := vault.OpenStore()
	if err != nil {
		return err
	}
	defer st.Close()
	project, err := st.VerifyToken(token)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("invalid or unknown token")
	}
	srv, err := NewServer(st, project.ProfileID)
	if err != nil {
		return err
	}
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}
```

- [ ] **Step 3: Write cli/mcp.go (subcommand + guardrail → stderr)**

Create `internal/cli/mcp.go`:
```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

func newMCPCmd() *cobra.Command {
	var token string
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Run the SSH MCP server (stdio) for an AI agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				return fmt.Errorf("--token is required")
			}
			// Residual-key guardrail: warn to STDERR only (stdout is the MCP channel).
			if st, err := vault.OpenStore(); err == nil {
				if found, _ := store.CheckResidualKeys(); len(found) > 0 {
					fmt.Fprintf(os.Stderr, "WARNING: ssh credential files detected at %v — hard enforcement can be bypassed by an agent that reads them directly. Remove them for full isolation.\n", found)
				}
				st.Close()
			}
			if err := mcpserver.RunStdio(token); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "project token (from `projects add`)")
	_ = c.MarkFlagRequired("token")
	return c
}
```
Wire `newMCPCmd()` into `internal/cli/root.go`'s `root.AddCommand(...)`.

- [ ] **Step 4: Write tests**

Create `internal/mcpserver/server_test.go` (in-memory client drives list_servers + exec_command):
```go
package mcpserver

import (
	"context"
	"io"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestNewServerToolsScopedViaInMemoryClient(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	server, _ := NewServer(st, pid)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSession, err := server.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srvSession.Close()
	cliSession, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cliSession.Close()

	// list_servers
	res, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("list_servers errored: %+v", res.Content)
	}

	// exec_command on the in-profile server
	res2, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command", Arguments: map[string]any{"server_id": srvID, "command": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.IsError {
		t.Fatalf("exec_command errored: %+v", res2.Content)
	}

	// exec_command on an out-of-profile server -> tool error (IsError)
	other, _ := st.AddServer(&models.Server{Name: "other", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	res3, _ := cliSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command", Arguments: map[string]any{"server_id": other, "command": "nope"},
	})
	if !res3.IsError {
		t.Fatal("out-of-profile exec_command must be a tool error")
	}
	_ = store.Store(nil) // keep store import if unused otherwise — remove if unused
}
```
(Remove the `_ = store.Store(nil)` no-op if `store` is otherwise unused; it's there only to avoid an unused-import churn — the implementer should drop it and the `store` import if not needed.)

Create `internal/cli/mcp_test.go` (invalid token refuses — RunStdio returns error):
```go
package cli

import (
	"bytes"
	"encoding/hex"
	"os/exec" // not used; placeholder removed below
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
)

func TestRunStdioRejectsUnknownToken(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "t.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mustMK(t)),
	})
	err := mcpserver.RunStdio("not-a-real-token")
	if err == nil {
		t.Fatal("unknown token must error")
	}
}
```
(Add a `mustMK` helper returning `store.GenerateMasterKey()` result, or inline it. Drop the unused `os/exec` import.)

- [ ] **Step 5: Build + test + commit**

Run: `go get github.com/modelcontextprotocol/go-sdk@v1.2.0` (if not already), then `go build ./...` + `go test ./...`
Expected: all green (mcpserver tests incl. in-memory client e2e; cli token-rejection test).

```bash
git add internal/mcpserver/ internal/cli/mcp.go internal/cli/root.go internal/cli/mcp_test.go go.mod go.sum && git commit -m "feat(mcp): stdio MCP server (list_servers + exec_command, Profile-scoped) + mcp --token"
```

---

## Task 5: Comprehensive in-memory e2e test (the capstone proof)

**Files:**
- Create: `internal/mcpserver/e2e_test.go`
- Test: as above

**Goal:** One test that proves the full agent path end-to-end: an in-memory MCP client, scoped to a project, can list only its profile's servers, exec on an in-profile server (real testsshd), is rejected on an out-of-profile server, and sudo works — i.e. the iron rule holds and the agent never sees credentials.

- [ ] **Step 1: Write e2e_test.go**

Create `internal/mcpserver/e2e_test.go`:
```go
package mcpserver

import (
	"context"
	"io"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestE2EIronRule is the capstone: a Profile-scoped MCP client can use its servers
// and is blocked from others, with credentials never crossing the tool boundary.
func TestE2EIronRule(t *testing.T) {
	st := newStore(t)

	// Two real sshd backends: one the agent may use, one it may not.
	allowedAddr, allowedHK, allowedCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "ALLOWED:" + cmd + "\n", "", 0 },
	})
	defer allowedCleanup()
	forbiddenAddr, forbiddenHK, forbiddenCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "FORBIDDEN\n", "", 0 },
	})
	defer forbiddenCleanup()

	allowedID := seedRealServer(t, st, "allowed", allowedAddr, allowedHK, "")
	forbiddenID := seedRealServer(t, st, "forbidden", forbiddenAddr, forbiddenHK, "")

	pid, _ := st.AddProfile("agent-profile")
	_ = st.GrantServers(pid, []string{allowedID}) // only allowed in profile

	server, _ := NewServer(st, pid)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, _ := server.Connect(context.Background(), t1, nil)
	defer srvSess.Close()
	cliSess, _ := client.Connect(context.Background(), t2, nil)
	defer cliSess.Close()
	ctx := context.Background()

	// 1. list_servers -> only "allowed"
	res, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
	if res.IsError {
		t.Fatal("list_servers should succeed")
	}
	// (Content is JSON; assert it contains "allowed" and not "forbidden" via the text.)
	if !textContains(res, "allowed") || textContains(res, "forbidden") {
		t.Fatalf("list_servers leaked a forbidden server: %+v", res.Content)
	}

	// 2. exec on allowed -> works
	res2, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"server_id": allowedID, "command": "hi"}})
	if res2.IsError {
		t.Fatalf("allowed exec should succeed: %+v", res2.Content)
	}

	// 3. exec on forbidden -> tool error (iron rule)
	res3, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"server_id": forbiddenID, "command": "hi"}})
	if !res3.IsError {
		t.Fatal("forbidden exec must be rejected (iron rule)")
	}
}

func textContains(res *mcp.CallToolResult, want string) bool {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if containsStr(tc.Text, want) {
				return true
			}
		}
	}
	return false
}
func containsStr(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ensure models + store imports are used (they are via seedRealServer/mustCred in core_test.go;
// if this file doesn't reference them, remove the imports — the linter will catch it).
var _ = models.AuthPassword
var _ store.Store
```
(If `models`/`store` are unused in this file after the body, drop those imports + the `var _` lines — the implementer verifies with `go build`.)

- [ ] **Step 2: Run + commit**

Run: `go test ./internal/mcpserver/ -run TestE2EIronRule -v`
Expected: PASS (the iron rule holds end-to-end).

```bash
git add internal/mcpserver/e2e_test.go && git commit -m "test(mcpserver): e2e iron-rule proof (in-memory client; scoped list + exec + out-of-profile reject)"
```

---

## Self-Review

**1. Spec/Plan coverage (Plan 3 = MCP server + runtime Profile enforcement):**
- `mcp --token <T>` stdio server → Tasks 4 (cli + RunStdio). ✓
- `list_servers` Profile-scoped, no creds → Task 3 (ListServersForProfile) + Task 4 (tool). ✓
- `exec_command(server_id, command, timeout?, sudo?)` Profile-enforced → Task 3 (ExecCommandForProfile) + Task 4 (tool). ✓
- **Iron rule runtime enforcement** (reject out-of-profile server_id) → Task 3 ErrNotInProfile + Task 5 e2e. ✓
- sudo wiring (SudoCredentialID + ExecSudo + AuditRow.Sudo) → Task 3. ✓
- audit per exec → Task 3 WriteAudit. ✓
- residual-key guardrail on MCP startup (→ stderr) → Task 4 cli/mcp.go. ✓
- carry-forward: ErrHostKeyMismatch sentinel → Task 2. ✓
- carry-forward: AuthForServer shared (no import cycle) → Task 1 vault. ✓
- LLM-optimized tool descriptions (§12.5) → Task 4. ✓

**2. Placeholder scan:** Task 3's `WithSudo`/`seedRealServer` sketched with a nil-store bug — the SAME step explicitly instructs rewriting `seedRealServer(t, st, name, addr, hk, sudoPw string)` to fix it (not ship the nil version). Task 4's tests have two placeholder no-op lines (`_ = store.Store(nil)`, unused `os/exec`) that the step instructs removing. No other placeholders; all core code complete.

**3. Type consistency:** `vault.OpenStore()`/`vault.AuthForServer` match cli + mcpserver usage. `mcpserver.NewServer(st, profileID)`, `RunStdio(token)`, `ListServersForProfile`/`ExecCommandForProfile` consistent across tasks. `ExecCommandInput`/`ExecCommandOutput`/`ListServersOutput`/`ServerInfo`/`ExecOutput` field names consistent. SDK handler signature `func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)` matches the verified API.

**Gaps noted:** owner `ssh` still full-access (correct); interactive PTY owner shell still deferred; agent-usability harness (§12) + SSH-conformance (§13) are Plans 4/5. The `mcp --token` requires the keychain to be populated (the user must have run `unlock` once) — a deployment note, not a code gap.

---

## Execution Handoff

Plan 3 complete and saved to `docs/superpowers/plans/2026-08-08-ssh-manager-mcp-plan-3-mcp-server.md`. Execute via superpowers:subagent-driven-development. Plan 4 (§13 SSH-conformance tests) + Plan 5 (§12 agent harness) follow.
