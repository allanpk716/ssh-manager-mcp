package mcpserver

import (
	"context"
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
