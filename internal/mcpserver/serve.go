package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// serverKey carries the request's resolved *mcp.Server (set by the resolveServer
// middleware after auth + ServerForProject) so the SDK's getServer hook can
// return it without re-resolving.
type serverKey struct{}

// projectTokenNominalTTL is a synthetic far-future expiration attached to every
// TokenInfo solely to satisfy the SDK auth verifier's non-zero-Expiration
// requirement (auth.go:120-126). Project tokens have no real expiry — their
// lifecycle is governed by VerifyToken (status='active': rotate/disable/revoke).
const projectTokenNominalTTL = 100 * 365 * 24 * time.Hour

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

// verifyToken is the auth.TokenVerifier for auth.RequireBearerToken: it validates
// the project token via the SAME VerifyToken gate stdio uses, and returns a
// TokenInfo whose UserID is the project id. The SDK captures UserID at session
// creation (streamable.go:425-435) and re-checks it per request
// (streamable.go:250-258) — that is what now blocks a token from one project
// being replayed onto another project's session (403 "session user mismatch").
func (r *ServeRunner) verifyToken(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
	project, err := r.st.VerifyToken(token)
	if err != nil || project == nil {
		return nil, fmt.Errorf("%w: invalid or unknown token", auth.ErrInvalidToken)
	}
	// SDK's verify() requires a non-zero, non-expired Expiration (auth.go:120-126).
	// Our project tokens are long-lived; real lifecycle is VerifyToken's
	// status='active' filter (rotate/disable/revoke), NOT this nominal expiry.
	// The far-future expiration solely satisfies the SDK check.
	return &auth.TokenInfo{
		UserID:     project.ID,
		Expiration: time.Now().Add(projectTokenNominalTTL),
	}, nil
}

// verifyCacheToken is the auth.TokenVerifier for the /snapshot route ONLY: it validates a
// device-auth code via VerifyCacheToken (a disjoint gate from project tokens) and returns a
// TokenInfo whose UserID is the cache-token id (used by handleSnapshot to TouchCacheToken).
// It is NEVER passed to the MCP handler's RequireBearerToken; verifyToken is NEVER passed to
// /snapshot's. Two gates, never bridged — this is what keeps a project token from dumping
// the whole vault.
func (r *ServeRunner) verifyCacheToken(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
	ct, err := r.st.VerifyCacheToken(token)
	if err != nil || ct == nil {
		return nil, fmt.Errorf("%w: invalid or unknown cache token", auth.ErrInvalidToken)
	}
	// SDK's verify() requires a non-zero, non-expired Expiration (auth.go:120-126). Same
	// nominal-TTL trick as verifyToken: the real lifecycle is VerifyCacheToken's
	// status='active' filter (revoke), NOT this nominal expiry.
	return &auth.TokenInfo{
		UserID:     ct.ID,
		Expiration: time.Now().Add(projectTokenNominalTTL),
	}, nil
}

// resolveServer runs AFTER auth.RequireBearerToken has stashed the *auth.TokenInfo.
// It resolves the token's project (by UserID) to its cached scoped server and
// stashes that server under serverKey for the SDK's getServer hook.
func (r *ServeRunner) resolveServer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ti := auth.TokenInfoFromContext(req.Context())
		if ti == nil || ti.UserID == "" {
			http.Error(w, "no authenticated project", http.StatusForbidden) // fail closed
			return
		}
		project, err := r.st.GetProject(ti.UserID)
		if err != nil || project == nil {
			http.Error(w, "project not found", http.StatusServiceUnavailable)
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

// HTTPHandler returns the request mux for `ssh-manager serve`. Composition:
//
//	GET /snapshot  → cache-token RequireBearerToken → handleSnapshot (read-only vault dump)
//	everything else → project-token RequireBearerToken → resolveServer → SDK streamable MCP handler
//
// The two RequireBearerToken chains use DISJOINT verifiers (verifyCacheToken vs verifyToken).
// A project token presented at /snapshot fails verifyCacheToken (it is not a device code) and is
// rejected; a cache token presented at the MCP path fails verifyToken (it is not a project token)
// and is rejected. The gates are never bridged — a project token can never dump the whole vault,
// and a cache token can never drive an MCP tool. This is the two-disjoint-gates keystone.
func (r *ServeRunner) HTTPHandler() http.Handler {
	getServer := func(req *http.Request) *mcp.Server {
		if s, ok := req.Context().Value(serverKey{}).(*mcp.Server); ok {
			return s
		}
		// Unreachable in practice: resolveServer stashes a server (or 403/503's)
		// before the SDK handler runs. If reached, the SDK returns HTTP 400
		// "no server available" (streamable.go:328-331) — no panic.
		return nil
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(getServer, nil)
	projectAuth := auth.RequireBearerToken(r.verifyToken, &auth.RequireBearerTokenOptions{}) // no scopes
	mcpChain := projectAuth(r.resolveServer(mcpHandler))

	cacheAuth := auth.RequireBearerToken(r.verifyCacheToken, &auth.RequireBearerTokenOptions{})
	snapshotHandler := cacheAuth(http.HandlerFunc(r.handleSnapshot))

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/snapshot" {
			snapshotHandler.ServeHTTP(w, req)
			return
		}
		mcpChain.ServeHTTP(w, req)
	})
}

// handleSnapshot writes the full vault Snapshot (Plan-11 ExportSnapshot, reused verbatim) as
// JSON. Cache tokens are NEVER in the Snapshot (server-side only — ExportSnapshot does not
// read the cache_tokens table). Best-effort TouchCacheToken AFTER the body is written — a touch
// failure is logged, not fatal (the pull already succeeded).
func (r *ServeRunner) handleSnapshot(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ti := auth.TokenInfoFromContext(req.Context())
	if ti == nil || ti.UserID == "" {
		http.Error(w, "no authenticated cache token", http.StatusForbidden) // fail closed
		return
	}
	snap, err := r.st.ExportSnapshot()
	if err != nil {
		http.Error(w, "snapshot unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// The snapshot body is the full decrypted credential dump — never let an
	// intermediary (CDN/proxy/HTTP cache) store it. no-store + no-cache (the
	// latter also forbids reading a cached copy without revalidation).
	w.Header().Set("Cache-Control", "no-store, no-cache")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		return // client gone; nothing more to do
	}
	if err := r.st.TouchCacheToken(ti.UserID); err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager serve: cache-tokens touch %s: %v\n", ti.UserID, err)
	}
}

// RunServe runs the authenticated streamable-HTTP MCP server until ctx is
// cancelled (SIGINT/SIGTERM, wired by the caller). The listener is created
// synchronously so bind errors surface before serving. TLS is ALWAYS used:
// if tlsCert is the operator's explicit --tls-cert, that cert is served
// (backward compat); if tlsCert is empty, RunServe auto-generates + loads a
// self-signed cert (LoadOrCreateServeCert) and serves that, logging its
// fingerprint so the operator can distribute the pin to clients. A
// LoadOrCreateServeCert failure is returned (serve refuses to start — never
// silently downgrades to plaintext). Returns nil on clean ctx-cancelled shutdown.
func RunServe(ctx context.Context, st *store.Store, addr, tlsCert, tlsKey string) error {
	runner := NewServeRunner(st)
	defer runner.Close()

	// Cert resolution: if the operator did not pass an explicit --tls-cert,
	// auto-generate + load a self-signed cert. After this block tlsCert is
	// guaranteed non-empty (the auto-TLS error path returns). This means serve
	// ALWAYS serves TLS — there is no plaintext path.
	autoTLSFingerprint := ""
	if tlsCert == "" {
		certPath, keyPath, fp, err := LoadOrCreateServeCert()
		if err != nil {
			return fmt.Errorf("serve auto-TLS: %w", err)
		}
		tlsCert, tlsKey = certPath, keyPath
		autoTLSFingerprint = fp
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Emit the "listening" line only AFTER a successful bind so a bind failure
	// (the early return above) never prints a misleading "listening" line.
	// TLS is always on post-auto-TLS; "auto" denotes the self-signed case.
	tlsLabel := "true"
	if autoTLSFingerprint != "" {
		tlsLabel = "auto"
	}
	fmt.Fprintf(os.Stderr, "ssh-manager serve: listening on %s (tls=%s)\n", addr, tlsLabel)
	if autoTLSFingerprint != "" {
		fmt.Fprintf(os.Stderr, "auto-TLS cert (self-signed). client pin: %s\n", autoTLSFingerprint)
	}

	// Start heartbeat goroutine to keep the serve log fresh. Plan 16 T7 dropped
	// the old serve.log marker-scan (vaultUnlockedFromLog was Windows-specific
	// and does not generalize across kardianos's per-platform log sinks). The
	// heartbeat remains useful: it gives any platform's log sink (Windows EventLog,
	// systemd journald, launchd syslog) a periodic liveness marker so an operator
	// inspecting logs can distinguish a hung serve from a merely idle one.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fmt.Fprintf(os.Stderr, "heartbeat: still listening on %s at %s\n", addr, time.Now().Format(time.RFC3339))
			}
		}
	}()

	srv := &http.Server{Handler: runner.HTTPHandler()}

	errCh := make(chan error, 1)
	go func() {
		if tlsCert != "" {
			errCh <- srv.ServeTLS(ln, tlsCert, tlsKey)
		} else {
			// Defensive only — unreachable post-auto-TLS: the cert-resolution
			// block above guarantees tlsCert is non-empty. Kept so a future
			// refactor that drops auto-TLS has a single, obvious seam to
			// audit; if reached today it would serve plaintext (NOT safe),
			// which is why the auto-TLS block above is load-bearing.
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
