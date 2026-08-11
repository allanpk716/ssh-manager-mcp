package mcpserver

import (
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
