package mcpserver

import (
	"context"
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

// HTTPHandler returns the authenticated streamable-HTTP MCP handler. Composition:
//
//	auth.RequireBearerToken (outermost) → resolveServer → SDK streamable handler
//
// The SDK's getServer hook reads the server resolveServer stashed in the context.
// Outermost placement of RequireBearerToken is required so the SDK sees the
// *auth.TokenInfo on the initialize request that creates the session — that is
// where UserID is captured (streamable.go:425-435) for later per-request checks.
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
	authMW := auth.RequireBearerToken(r.verifyToken, &auth.RequireBearerTokenOptions{}) // no scopes
	return authMW(r.resolveServer(mcpHandler))
}

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
	// Emit the "listening" line only AFTER a successful bind so a bind failure
	// (the early return above) never prints a misleading "listening" line.
	fmt.Fprintf(os.Stderr, "ssh-manager serve: listening on %s (tls=%v)\n", addr, tlsCert != "")
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
