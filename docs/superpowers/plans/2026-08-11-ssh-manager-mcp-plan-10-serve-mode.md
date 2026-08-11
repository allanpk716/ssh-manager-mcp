# ssh-manager-mcp Plan 10 — `serve` mode (HTTP MCP server, multi-client, per-token scoping)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `ssh-manager serve` — run the broker as an authenticated HTTP MCP server so agents on other machines (VLAN) can share one authoritative vault. This is **Phase 1 of the multi-machine design (路线乙)** decided in the 2026-08-11 `/grill-me`. Later phases (client read-only cache + offline, mutation forwarding + snapshot push, Synology backup, migration + DEK enroll) are separate plans.

**Architecture:** The existing single-session stdio path (`RunStdio` → `NewServer(st, profileID, projectID)` → `srv.Run(ctx, &mcp.StdioTransport{})`) is **unchanged**. `serve` wraps the SAME project-scoped `*mcp.Server` (built by the unchanged `NewServer`) behind the go-sdk's streamable-HTTP handler, and resolves a bearer token to a project **per HTTP request**. One shared `*store.Store`; one cached scoped server per project (built lazily on first request for that token, reused across its sessions). HTTP middleware extracts `Authorization: Bearer <token>`, calls the existing `store.VerifyToken` (the same gate stdio uses), 401s on miss/invalid/inactive, and hands the resolved scoped server to the SDK handler. **No tool changes, no iron-rule changes, no store-schema changes.**

**Tech Stack:** Go 1.24; `github.com/modelcontextprotocol/go-sdk v1.2.0` (`mcp.NewStreamableHTTPHandler`, already in `go.mod`); stdlib `net/http`, `os/signal`. **No new dependencies.**

## Global Constraints

- **Agent surface unchanged.** `internal/mcpserver/core.go`, `internal/sshbroker/*`, the broker tool registrations inside `NewServer`, the iron-rule per-call `serverID ∈ profileID` re-gate — **none modified**. `serve` only re-exposes the existing scoped server over HTTP. No-regression bar = `go test ./...` green (no LLM, no conformance Docker, no §12 eval — the agent surface is untouched, exactly like Plan 8).
- **`ssh-manager mcp` (stdio) unchanged.** The local single-machine path stays as-is for public-repo single-machine users.
- **Per-request token auth is mandatory.** No anonymous network access. Every HTTP request must carry a valid `Authorization: Bearer <token>` resolving to an `active` project (`VerifyToken` already filters `status='active'` — Plan 8 T3). Missing/invalid/inactive → 401.
- **Default bind is loopback.** `--addr 127.0.0.1:7878`. Remote (multi-machine) use requires explicitly setting `--addr 0.0.0.0:7878` or a VLAN IP — conscious exposure.
- **TLS is optional but documented.** `--tls-cert`/`--tls-key` enable HTTPS. Plaintext HTTP prints a STDERR warning (the bearer token is a shared secret traversing the network; on a non-loopback bind without TLS it is sniffable). The warning is the security nudge; the choice is the operator's.
- **One cached scoped server per project.** `NewServer` registers tools and allocates a `TunnelManager`; rebuilding it per request is wasteful, so `ServeRunner` caches by project ID. Concurrent sessions of the same project share that server + its tunnel manager (tunnel IDs are unguessable UUIDs, so cross-client interference is not practical; per-client tunnel isolation is a possible future hardening, out of scope here).
- **Shutdown tears down every cached tunnel manager.** On ctx cancel (SIGINT/SIGTERM wired by the CLI) `ServeRunner.Close()` calls `tunnels.CloseAll()` for each cached server so no SSH connection leaks.
- **Hygiene:** `.gitattributes` LF; `gofmt -l .` empty; `go vet ./...` clean; one logical commit per task; messages end `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Branch:** `worktree-multimachine-sync` (already created via `superpowers:using-git-worktrees`), base master HEAD.

---

## Scope decisions (surfaced for plan review)

1. **Per-request token routing (not a single hardcoded project).** `serve` is NOT started with one `--token`. Each client sends its own bearer token; the middleware resolves it to a project and the `getServer` hook returns that project's cached scoped server. This supports multiple projects/tokens over one server with **zero refactor to the existing tools** (they stay closed over profileID/projectID inside `NewServer`). Cost vs single-token MVP: ~15 extra lines (a `sync.Map` cache). Worth it — it is the security-correct design and matches how `VerifyToken` already works.
2. **Custom bearer middleware, NOT the SDK's `auth.RequireBearerToken`.** The SDK's auth package is OAuth-scoped (`Scopes`, `ResourceMetadataURL`, `TokenVerifier`). Our "token" is a project token verified by `store.VerifyToken` (Argon2id), not an OAuth token. A ~20-line custom middleware that calls `VerifyToken` and stashes the resulting `*mcp.Server` in the request context is simpler and correct. (The SDK auth package remains available if real OAuth is ever wanted.)
3. **Middleware resolves token → project → server and stashes the SERVER (not the project).** So the SDK's `getServer` hook always finds a non-nil `*mcp.Server` (the middleware already 503'd if `ServerForProject` failed). This avoids ever returning nil to the SDK (which could panic).
4. **Test-seeding defers to read-time (Plan-8 precedent).** Tests need a project+token in a fresh store. The exact store-level seeding helper is read from `internal/store/projects_test.go` (the seeding the store package itself uses). The contract — "create an `active` project bound to a profile; return its plaintext token" — is fixed; only the exact call signature is deferred. Same pattern Plan 8 T5 used for `audit.go`.
5. **HTTP-response correctness is the SDK's job; we test our wiring.** T2/T3 tests assert **status codes** (401 without/with-bad token; 200 with valid token) via raw JSON-RPC POSTs, not full MCP client sessions. Full streamable-HTTP protocol correctness is covered by the go-sdk's own tests. Our responsibility is the auth gate + correct `getServer` delegation.
6. **`RunServe` takes a `context.Context`** (cancellable). The CLI wires `signal.NotifyContext` → ctx; tests cancel ctx directly. `RunServe` creates the listener synchronously (bind errors surface before serving) then `srv.Serve(ln)` in a goroutine, selecting on ctx.Done()→`Shutdown` vs serve-error.
7. **Out of scope for Plan 10 (later plans):** client-side read-only cache + offline mode (Plan 11); mutation forwarding + snapshot push (Plan 12); Synology encrypted backup (Plan 13); existing-vault migration + per-client DEK enroll (Plan 14). Plan 10 delivers only the server's HTTP reachability.

---

## File Structure

**New:**
- `internal/mcpserver/serve.go` — `ServeRunner` (per-project scoped-server cache + `Close`), bearer-token middleware, `HTTPHandler()`, `RunServe(ctx, st, addr, tlsCert, tlsKey)`.
- `internal/cli/serve.go` — thin cobra command; wires `signal.NotifyContext` → `mcpserver.RunServe`.
- `internal/mcpserver/serve_test.go` — ServeRunner cache + middleware/auth-gate tests.
- `internal/cli/serve_smoke_test.go` — `serve` registered + flags + `RunServe` clean shutdown.

**Modified:**
- `internal/cli/root.go` — register `newServeCmd()`.
- `README.md` — "Multi-machine: `serve` mode" section (bind, token, TLS warning, point at later phases).

---

## Task 1: `ServeRunner` — per-project scoped-server cache + shutdown

**Goal:** The stateful core of `serve`: one shared store, one lazily-built cached scoped server per project, clean teardown. No HTTP yet.

**Files:**
- Create: `internal/mcpserver/serve.go`
- Create: `internal/mcpserver/serve_test.go`

**Interfaces:**
- Consumes: `store.VerifyToken(token string) (*models.Project, error)` (existing); `mcpserver.NewServer(st *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, error)` (existing, unchanged).
- Produces: `NewServeRunner(st *store.Store) *ServeRunner`; `(*ServeRunner) ServerForProject(project *models.Project) (*mcp.Server, error)`; `(*ServeRunner) Close()`.

- [ ] **Step 1: Write the failing test.**

```go
package mcpserver

import (
	"testing"

	"ssh-manager-mcp/internal/store"
)

// newTestStore + seedActiveProjectToken follow the existing pattern in
// internal/store/projects_test.go — read it for the exact seeding signature.
// Contract: seedActiveProjectToken creates an active project bound to a profile
// and returns the plaintext token + that project's ID + profile ID.
func TestServeRunner_CachesByProject(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()

	token, projID, profileID := seedActiveProjectToken(t, st)
	project, err := st.VerifyToken(token)
	if err != nil || project == nil {
		t.Fatalf("VerifyToken: err=%v project=%v (contract: active project resolves)", err, project)
	}
	if project.ID != projID || project.ProfileID != profileID {
		t.Fatalf("resolved project mismatch: got id=%s profile=%s", project.ID, project.ProfileID)
	}

	r := NewServeRunner(st)
	defer r.Close()

	s1, err := r.ServerForProject(project)
	if err != nil || s1 == nil {
		t.Fatalf("ServerForProject: err=%v srv=%v", err, s1)
	}
	s2, err := r.ServerForProject(project)
	if err != nil {
		t.Fatalf("second ServerForProject: %v", err)
	}
	if s1 != s2 {
		t.Fatal("cache miss: same project must yield the same *mcp.Server pointer")
	}
}
```

- [ ] **Step 2: Run to fail** — `newTestStore`/`seedActiveProjectToken` may already exist (if not, copy them from `internal/store/projects_test.go`); `NewServeRunner`/`ServerForProject` are undefined.

Run: `go test ./internal/mcpserver/ -run TestServeRunner_CachesByProject -v`
Expected: FAIL (undefined symbols) — or, if the helpers don't exist in the mcpserver package, a compile error naming them.

- [ ] **Step 3: Implement `internal/mcpserver/serve.go`.**

```go
package mcpserver

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// serverKey carries the request's resolved *mcp.Server (set by the auth
// middleware after VerifyToken + ServerForProject) so the SDK's getServer hook
// can return it without re-resolving.
type serverKey struct{}

// scopedServer is a project-scoped MCP server + its tunnel manager, cached per
// project so concurrent sessions of the same project share one instance.
type scopedServer struct {
	srv     *mcp.Server
	tunnels *TunnelManager
}

// ServeRunner is the stateful core of `ssh-manager serve`: it holds the shared
// store and one cached scoped server per project. Each verified token maps to a
// stable server instance across that token's HTTP requests.
type ServeRunner struct {
	st    *store.Store
	mu    sync.Mutex
	cache map[string]*scopedServer // keyed by project ID
}

// NewServeRunner constructs a runner over an already-open store. The caller owns st.Close().
func NewServeRunner(st *store.Store) *ServeRunner {
	return &ServeRunner{st: st, cache: make(map[string]*scopedServer)}
}

// ServerForProject returns the cached scoped server for project, building it on first use.
// NewServer is the existing project-scoped constructor (unchanged) — same one RunStdio uses.
func (r *ServeRunner) ServerForProject(project *models.Project) (*mcp.Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.cache[project.ID]; ok {
		return s.srv, nil
	}
	srv, tunnels, err := NewServer(r.st, project.ProfileID, project.ID)
	if err != nil {
		return nil, err
	}
	r.cache[project.ID] = &scopedServer{srv: srv, tunnels: tunnels}
	return srv, nil
}

// Close tears down every cached server's tunnel manager (SIGINT/server-shutdown).
func (r *ServeRunner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.cache {
		s.tunnels.CloseAll()
	}
}
```

(Add the test helpers `newTestStore` / `seedActiveProjectToken` to `serve_test.go` if they do not already exist in the `mcpserver` package, copying the seeding approach from `internal/store/projects_test.go`. They are test-only.)

- [ ] **Step 4: Run test to verify it passes.**

Run: `go test ./internal/mcpserver/ -run TestServeRunner_CachesByProject -v`
Expected: PASS.

- [ ] **Step 5: Commit** — `feat(mcpserver): ServeRunner per-project scoped-server cache (Plan 10 T1)` + Co-Authored-By.

---

## Task 2: Bearer-token middleware + `HTTPHandler`

**Goal:** The authenticated streamable-HTTP MCP handler. Missing/invalid/inactive token → 401; valid token → request reaches the SDK handler with the resolved server in context.

**Files:**
- Modify: `internal/mcpserver/serve.go` (append middleware + `HTTPHandler`)
- Modify: `internal/mcpserver/serve_test.go` (append auth-gate tests)

**Interfaces:**
- Produces: `(*ServeRunner) HTTPHandler() http.Handler`.

- [ ] **Step 1: Write the failing tests** (status-code-only — protocol correctness is the SDK's job).

```go
func TestHTTPHandler_AuthGate(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	token, _, _ := seedActiveProjectToken(t, st)

	r := NewServeRunner(st)
	defer r.Close()
	h := r.HTTPHandler()

	// Minimal JSON-RPC initialize body the streamable handler accepts.
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

	cases := []struct {
		name   string
		auth   string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"bad token", "Bearer not-a-real-token", http.StatusUnauthorized},
		{"malformed header", "Token " + token, http.StatusUnauthorized},
		{"valid token", "Bearer " + token, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(initBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Fatalf("%s: status = %d, want %d (body=%q)", c.name, rr.Code, c.want, rr.Body.String())
			}
		})
	}
}
```

Add the `net/http/httptest` and `strings` imports to the test file.

- [ ] **Step 2: Run to fail** — `HTTPHandler` undefined.

Run: `go test ./internal/mcpserver/ -run TestHTTPHandler_AuthGate -v`
Expected: FAIL (compile error: `r.HTTPHandler undefined`).

- [ ] **Step 3: Implement middleware + handler** (append to `internal/mcpserver/serve.go`).

```go
// bearerToken parses an "Authorization: Bearer <tok>" header value. Returns ""
// for anything that is not an exact-case-prefix "Bearer " (RFC 6750 is
// case-insensitive on the scheme; we accept "Bearer" only, matching clients
// like Claude Code which send exactly that).
func bearerToken(h string) string {
	const scheme = "Bearer "
	if len(h) <= len(scheme) {
		return ""
	}
	if !strings.EqualFold(h[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(h[len(scheme):])
}

// requireProjectToken is HTTP middleware: extract bearer token, VerifyToken it,
// build/fetch the scoped server, stash the server in the request context. 401
// on missing/invalid/inactive token; 503 if the scoped server cannot be built.
func (r *ServeRunner) requireProjectToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tok := bearerToken(req.Header.Get("Authorization"))
		if tok == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		project, err := r.st.VerifyToken(tok)
		if err != nil || project == nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		srv, err := r.ServerForProject(project)
		if err != nil {
			http.Error(w, "server unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx := context.WithValue(req.Context(), serverKey{}, srv)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// HTTPHandler returns the authenticated streamable-HTTP MCP handler. The SDK's
// getServer hook reads the server the middleware stashed in the context.
func (r *ServeRunner) HTTPHandler() http.Handler {
	getServer := func(req *http.Request) *mcp.Server {
		if s, ok := req.Context().Value(serverKey{}).(*mcp.Server); ok {
			return s
		}
		return nil // unreachable: middleware 401/503's before the handler runs
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(getServer, nil)
	return r.requireProjectToken(mcpHandler)
}
```

- [ ] **Step 4: Run tests to verify they pass.**

Run: `go test ./internal/mcpserver/ -run TestHTTPHandler_AuthGate -v`
Expected: PASS — no/bad/malformed → 401; valid → 200.

- [ ] **Step 5: Commit** — `feat(mcpserver): bearer-token auth middleware + HTTPHandler (Plan 10 T2)` + Co-Authored-By.

---

## Task 3: `RunServe` lifecycle + `serve` cobra command + register

**Goal:** The blocking server runner (ctx-cancellable, TLS-aware) and the thin CLI command wired to SIGINT/SIGTERM.

**Files:**
- Modify: `internal/mcpserver/serve.go` (append `RunServe`)
- Create: `internal/cli/serve.go`
- Modify: `internal/cli/root.go` (register `newServeCmd()`)
- Create: `internal/cli/serve_smoke_test.go`

**Interfaces:**
- Produces: `mcpserver.RunServe(ctx, st, addr, tlsCert, tlsKey) error`; the `serve` cobra subcommand.

- [ ] **Step 1: Write the failing tests.**

`internal/mcpserver/serve_test.go` — clean shutdown on ctx cancel:

```go
func TestRunServe_ShutdownOnCancel(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunServe(ctx, st, "127.0.0.1:0", "", "") }()

	cancel() // RunServe must exit cleanly regardless of timing
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe returned %v; want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunServe did not shut down within 2s")
	}
}
```

Add `"time"` to the test imports.

`internal/cli/serve_smoke_test.go` — the command is registered with the expected flags:

```go
package cli

import (
	"testing"
)

// newRootForTest mirrors however internal/cli's existing tests build the root
// command (read an existing internal/cli/*_test.go for the helper name).
func TestServeCmd_RegisteredWithFlags(t *testing.T) {
	root := newRootForTest(t) // contract: returns the root cobra command, no Execute
	srv, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatal("serve subcommand not registered:", err)
	}
	for _, flag := range []string{"addr", "tls-cert", "tls-key"} {
		if srv.Flags().Lookup(flag) == nil {
			t.Errorf("serve missing --%s flag", flag)
		}
	}
}
```

- [ ] **Step 2: Run to fail** — `RunServe` undefined; `serve` subcommand not found.

Run: `go test ./internal/mcpserver/ -run TestRunServe_ShutdownOnCancel -v && go test ./internal/cli/ -run TestServeCmd_RegisteredWithFlags -v`
Expected: FAIL.

- [ ] **Step 3: Implement `RunServe`** (append to `internal/mcpserver/serve.go`).

```go
// RunServe runs the authenticated streamable-HTTP MCP server until ctx is
// cancelled (SIGINT/SIGTERM, wired by the caller). The listener is created
// synchronously so bind errors surface before serving. TLS is used when
// tlsCert != "". Returns nil on clean ctx-cancelled shutdown.
func RunServe(ctx context.Context, st *store.Store, addr, tlsCert, tlsKey string) error {
	runner := NewServeRunner(st)
	defer runner.Close()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: runner.HTTPHandler()}

	errCh := make(chan error, 1)
	go func() {
		if tlsCert != "" {
			errCh <- srv.ServeTLS(ln, tlsCert, tlsKey)
		} else {
			errCh <- srv.Serve(ln)
		}
	}()

	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}
```

- [ ] **Step 4: Implement `internal/cli/serve.go`.**

```go
package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/vault"
)

func newServeCmd() *cobra.Command {
	var addr, tlsCert, tlsKey string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the SSH MCP server over HTTP for remote (multi-machine) agents",
		Long: `Run the broker as an authenticated HTTP MCP server so agents on other
machines can share one authoritative vault.

Each request must carry 'Authorization: Bearer <project-token>'. The token
resolves to a project and its profile scope (same gate as 'ssh-manager mcp').

Default bind is loopback (127.0.0.1:7878) — safe. For multi-machine use, set
--addr to 0.0.0.0:7878 or a VLAN IP, and prefer --tls-cert/--tls-key: without
TLS the bearer token travels in cleartext on the network.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := vault.OpenStore()
			if err != nil {
				return err
			}
			defer st.Close()

			if tlsCert == "" && !isLoopback(addr) {
				fmt.Fprintln(os.Stderr, "WARNING: serving plaintext HTTP on a non-loopback address — the bearer token is sniffable. Use --tls-cert/--tls-key.")
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			fmt.Fprintf(os.Stderr, "ssh-manager serve: listening on %s (tls=%v)\n", addr, tlsCert != "")
			return mcpserver.RunServe(ctx, st, addr, tlsCert, tlsKey)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7878", "listen address (use 0.0.0.0:port or a VLAN IP for remote agents)")
	c.Flags().StringVar(&tlsCert, "tls-cert", "", "path to TLS cert (enables HTTPS)")
	c.Flags().StringVar(&tlsKey, "tls-key", "", "path to TLS key")
	return c
}

// isLoopback reports whether addr's host part is loopback (best-effort parse).
// Used to suppress the cleartext warning when serving on loopback only.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
```

- [ ] **Step 5: Register the command** — in `internal/cli/root.go`, add `newServeCmd()` next to wherever `newMCPCmd()` is registered (find the existing `rootCmd.AddCommand(...)` block and add `rootCmd.AddCommand(newServeCmd())`). Read `root.go` for the exact pattern (likely `rootCmd.AddCommand(newMCPCmd(), newServersCmd(cmd), ...)`).

- [ ] **Step 6: Run tests to verify they pass.**

Run: `go test ./internal/mcpserver/ ./internal/cli/ -v`
Expected: PASS (RunServe shutdown; serve registered with addr/tls-cert/tls-key).

- [ ] **Step 7: Commit** — `feat(cli): ssh-manager serve (HTTP MCP, TLS, signal shutdown) (Plan 10 T3)` + Co-Authored-By.

---

## Task 4: README docs + verify + review + merge

**Goal:** Document `serve`, confirm green, final review, merge.

**Files:** `README.md`.

- [ ] **Step 1: README** — add a "Multi-machine: `serve` mode (Phase 1)" section: what it does (authoritative broker over HTTP for VLAN agents), the bearer-token requirement, `--addr` (loopback default; `0.0.0.0`/VLAN IP for remote), the TLS warning, a minimal client config sketch (agent points its MCP client at `http://<vlan-ip>:7878` with header `Authorization: Bearer <token>`), and an explicit note that **client-side offline cache + mutation sync are later phases** — for now the remote agent connects live to the server (no offline capability yet).
- [ ] **Step 2: Verify** — `go test ./...` green; `gofmt -l .` empty; `go vet ./...` clean.
- [ ] **Step 3: Manual smoke** — `go run . serve --addr 127.0.0.1:7878` (with an unlocked store + an active project token); `curl -X POST http://127.0.0.1:7878/ -H "Authorization: Bearer <token>" -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'` → 200 + serverInfo; same without the header → 401.
- [ ] **Step 4: Final whole-branch review** — focus on: (a) **every** path to the SDK handler passes through `requireProjectToken` (no unauthenticated route — the iron rule is meaningless if the network front door is open); (b) `VerifyToken` is the sole admission authority (the existing `status='active'` filter carries over); (c) `ServeRunner.Close` is reachable on every exit path (deferred in `RunServe` and the CLI's RunE via `st.Close`-adjacent teardown); (d) cache keyed by project ID is stable across a token's requests. Resolve findings in one fix wave.
- [ ] **Step 5: Merge** to master per the user's finishing choice (`--no-ff`, matching Plan 5c/5d/5e/6/8).

---

## Self-Review (run before handoff)

1. **Spec coverage (from the grill + 路线乙 Phase 1):** authoritative broker reachable over the network by remote agents (✓ T1+T2+T3); per-request token auth reusing `VerifyToken` (✓ T2); no agent-surface change (✓ — `NewServer`/core.go/sshbroker untouched); stdio path unchanged (✓); default-safe bind + TLS option + warning (✓ T3); clean shutdown (✓ T1 `Close` + T3 `RunServe` ctx). Phase-2-onwards items (client cache, offline, mutation push, backup, migration) are **intentionally out of scope** — flagged in README + scope decision 7.
2. **Placeholder scan:** the `splitHostPortNet` shim in T3 Step 4 is explicitly resolved to `net.SplitHostPort` in the same step's note (the committed code is the `net` version, not a placeholder). Test helpers `newTestStore`/`seedActiveProjectToken`/`newRootForTest` are "verify on read" with fixed contracts (Plan-8 precedent: T5's `audit.go` signature). No `<...>` or "TODO" left in code blocks.
3. **Type consistency:** `ServeRunner` → `ServerForProject(*models.Project)` → cached `scopedServer{srv *mcp.Server, tunnels *TunnelManager}`. `serverKey{}` set in `requireProjectToken`, read in `HTTPHandler`'s `getServer`. `RunServe(ctx, *store.Store, addr, tlsCert, tlsKey)` called by the CLI with the same five-arg shape. `NewServer`'s `(srv, tunnels, err)` return is consumed unchanged.
4. **Security (the load-bearing concern):** the ONE thing reviewers must confirm is that **no request reaches `mcp.NewStreamableHTTPHandler`'s handler without a verified token**. `HTTPHandler` wraps the handler in `requireProjectToken` and returns the composed handler; `RunServe` serves only `runner.HTTPHandler()`. There is no alternate route. A disabled/revoked project token is rejected by `VerifyToken` (Plan 8 T3's `status='active'` filter) → 401, identical to stdio admission. This is the network analog of stdio's `--token` gate; the iron rule (per-call `serverID ∈ profileID`) then applies identically inside the shared `NewServer`.

---

## Execution Handoff

**Subagent-Driven (recommended):** T1–T3 sonnet (pure Go + stdlib HTTP + the go-sdk streamable handler + tests, no LLM); T4 sonnet docs + a final **opus** whole-branch review focused on the auth-gate-completeness check in Self-Review §4. **No Fable-5/$ required** — correctness is proven by unit tests + the auth-gate HTTP test, not the §12 eval (agent surface untouched). Merge per the user's choice (`--no-ff` to master).

**Honest scope note:** Plan 10 delivers only the server's HTTP reachability — it does NOT yet give a remote client offline capability or vault replication (those are Plans 11+). The immediate win: a remote agent on another VLAN machine can use the broker live. Plan 11 (next) adds the client-side encrypted read-only cache + offline-read/no-write semantics that the grill settled on.
