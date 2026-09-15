package mcpserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// cacheTokenNominalTTL is a synthetic far-future expiration attached to every
// TokenInfo solely to satisfy the SDK auth verifier's non-zero-Expiration
// requirement (auth.go:120-126). Device codes have no real expiry — their
// lifecycle is governed by VerifyCacheToken (status='active': revoke).
const cacheTokenNominalTTL = 100 * 365 * 24 * time.Hour

// scopedServer is a project-scoped MCP server + its tunnel manager + its
// background-task manager, cached per project so concurrent sessions of the
// same project share one instance (per-Server TaskManager = cross-project
// background-task isolation, structural like tunnels — Plan 32 spec §1).
type scopedServer struct {
	srv     *mcp.Server
	tunnels *TunnelManager
	tasks   *TaskManager
}

// ServeRunner is the stateful core of `sshmgr serve`: it holds the shared
// store. The per-project scoped-server cache (ServerForProject) is a leftover
// of the removed ②a MCP-over-HTTP surface — since Plan 42 批1 it has no HTTP
// caller (the only authenticated route, /snapshot, never touches it) and
// awaits the serve-side agent-execution-surface retirement (spec §3.1-2).
type ServeRunner struct {
	st    *store.Store
	mu    sync.Mutex
	cache map[string]*scopedServer // keyed by project ID
	// Three-state switch machinery (Plan 42 批1 T2, switches.go): the
	// injected explicit env/flag inputs and the ≤5s memoized resolve of
	// pairing/discovery. Both lock-free via atomic.Pointer.
	switchIn atomic.Pointer[switchInputs]
	switches atomic.Pointer[switchCache]

	// Pairing key state (Plan 42 批1 T5, pairserve.go): the /pair surface's
	// ephemeral X25519 private keys live ONLY here — never on disk, never in
	// the store. Keyed by the raw 32-byte pairing id. Entries are dropped when
	// their row reaches a terminal state or pairKeyMaxAgeSec passes; a serve
	// restart empties the map by construction (the pending rows become
	// unfinishable and expire through the store's time predicates).
	pairMu   sync.Mutex
	pairKeys map[[32]byte]pairKeyEntry
	// pairSigner is the serve cert's ed25519 private key (the long-term
	// identity that signs each pairing transcript — F5); pairSPKI is the
	// cert's SPKI fingerprint handed to the client inside the sealed
	// envelope so it pins the same key it is paired with. Both are set once
	// at RunServe startup (nil/"" = pairing signing unavailable).
	pairSigner ed25519.PrivateKey
	pairSPKI   string
	// per-IP rate limiters + pending-queue quotas (frozen env seams, read at
	// construction — restart-effective).
	pairLimits        pairLimits
	pairPendingPerIP  int
	pairPendingGlobal int
}

// NewServeRunner constructs a runner over an already-open store. The caller owns st.Close().
func NewServeRunner(st *store.Store) (*ServeRunner, error) {
	// Plan 47 T2 (spec §4): serve-side startup VALIDATION of the relay env
	// seams — fail-fast before RunServe binds. Per-project scoped servers
	// re-resolve at their own construction time (NewServerFromSource, the
	// 接线口径); this only refuses a bad value at process start.
	if _, err := resolveRelayChunk(); err != nil {
		return nil, err
	}
	if _, err := resolveRelayParallel(); err != nil {
		return nil, err
	}
	// Plan 40 §2.1 legacy detection: active device-code names are about to be
	// emitted as X-Sshmgr-Device-Name and used as client directory names — a
	// casefold collision or an illegal legacy name must stop the serve BEFORE it
	// serves, not mid-flight. Repair = revoke + re-add (never auto-rename).
	if anomalies, aerr := st.ScanCacheTokenNameAnomalies(); aerr != nil {
		return nil, fmt.Errorf("serve startup: device-code name scan failed: %w", aerr)
	} else if len(anomalies) > 0 {
		return nil, formatNameAnomalies(anomalies)
	}
	r := &ServeRunner{st: st, cache: make(map[string]*scopedServer), pairKeys: make(map[[32]byte]pairKeyEntry)}
	r.pairLimits, r.pairPendingPerIP, r.pairPendingGlobal = pairLimitsFromEnv()
	return r, nil
}

// formatNameAnomalies builds the fail-closed startup refusal for Plan 40 §2.1
// legacy detection. Pure so the wording is unit-testable without a dirty DB
// (which cannot be built through the public API — the add gate refuses every
// illegal/colliding name).
func formatNameAnomalies(anomalies []string) error {
	plural := "ies"
	if len(anomalies) == 1 {
		plural = "y"
	}
	return fmt.Errorf("serve refusing to start: %d device-code name anomal%s:\n  - %s\nrepair on this machine: `sshmgr cache-tokens revoke <name>` then `cache-tokens add --name <new-name> --profile <profile>`",
		len(anomalies), plural, strings.Join(anomalies, "\n  - "))
}

// ServerForProject returns the cached scoped server for project, building it on first use.
// NewServer is the existing project-scoped constructor — same one RunStdio uses
// (Plan 32: it now also returns the project's TaskManager, held here + closed in Close).
func (r *ServeRunner) ServerForProject(project *models.Project) (*mcp.Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.cache[project.ID]; ok {
		return s.srv, nil
	}
	srv, tunnels, tasks, err := NewServer(r.st, project.ProfileID, project.ID)
	if err != nil {
		return nil, err
	}
	r.cache[project.ID] = &scopedServer{srv: srv, tunnels: tunnels, tasks: tasks}
	return srv, nil
}

// Close tears down every cached server's tunnel manager and background-task
// manager (SIGINT/server-shutdown).
func (r *ServeRunner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.cache {
		s.tunnels.CloseAll()
		s.tasks.CloseAll()
	}
}

// verifyCacheToken is the auth.TokenVerifier for the /snapshot route: it validates a
// device-auth code via VerifyCacheToken and returns a TokenInfo whose UserID is the
// cache-token id (used by handleSnapshot to TouchCacheToken). Post-Plan-42-批1 it
// gates the ONLY authenticated HTTP route serve exposes (/snapshot) — the device-code
// gate is the only remote credential, and it is never bridged to anything else.
func (r *ServeRunner) verifyCacheToken(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
	ct, err := r.st.VerifyCacheToken(token)
	if err != nil || ct == nil {
		// Plan 34 rev4 §1: the 401 reason is observability-only (revoked vs
		// unknown via an 8-char-prefix lookup; collisions can mislabel —
		// accepted). The client NEVER branches on this text.
		reason := "unknown"
		prefix := token
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		if name, ok, nerr := r.st.RevokedCacheTokenNameByPrefix(prefix); nerr == nil && ok {
			reason = "revoked"
			fmt.Fprintf(os.Stderr, "sshmgr serve: cache token rejected: revoked (device %s, prefix %.8s)\n", name, token)
		} else {
			if nerr != nil {
				// Plan 34 T6: a failed lookup archives as unknown (the reason is
				// observability-only) but is never silent — the owner's log keeps it.
				fmt.Fprintf(os.Stderr, "sshmgr serve: cache token revoked-prefix lookup failed: %v (archived as unknown)\n", nerr)
			}
			fmt.Fprintf(os.Stderr, "sshmgr serve: cache token rejected: unknown (prefix %.8s)\n", token)
		}
		return nil, fmt.Errorf("%w: invalid cache token: %s", auth.ErrInvalidToken, reason)
	}
	// SDK's verify() requires a non-zero, non-expired Expiration (auth.go:120-126).
	// Our device codes are long-lived; the real lifecycle is VerifyCacheToken's
	// status='active' filter (revoke), NOT this nominal expiry. The far-future
	// expiration solely satisfies the SDK check.
	return &auth.TokenInfo{
		UserID:     ct.ID,
		Expiration: time.Now().Add(cacheTokenNominalTTL),
	}, nil
}

// The two-gates keystone narrows in v0.11.0: a project token is no longer a REMOTE MCP
// credential at all (the MCP-over-HTTP route is gone) — it survives only as the
// client-side spawn gate, validated by `mcp --cache` against the snapshot's projects.
// The device-code gate on /snapshot is unchanged and remains the only remote credential.
func (r *ServeRunner) HTTPHandler() http.Handler {
	cacheAuth := auth.RequireBearerToken(r.verifyCacheToken, &auth.RequireBearerTokenOptions{})
	snapshotHandler := cacheAuth(http.HandlerFunc(r.handleSnapshot))
	// Plan 48 §1: /pin-hostkey rides the SAME cacheAuth instance — one
	// listener, one device-code gate, zero new authentication forms.
	pinHandler := cacheAuth(http.HandlerFunc(r.handlePinHostkey))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/snapshot" {
			snapshotHandler.ServeHTTP(w, req)
			return
		}
		if req.URL.Path == "/pin-hostkey" {
			pinHandler.ServeHTTP(w, req)
			return
		}
		if strings.HasPrefix(req.URL.Path, "/pair/") {
			r.handlePair(w, req) // unauthenticated SAS pairing surface (Plan 42 §3.3) — self-gated
			return
		}
		http.NotFound(w, req) // 其余一律 404
	})
}

// handleSnapshot writes the authorization-scoped vault Snapshot for the device
// code's BOUND profile (Plan 39, via store.ExportSnapshotForProfile): exactly
// the granted servers + their referenced credentials, the profile/grants, the
// same-profile projects, and those servers' host keys — NO audit rows. Cache
// tokens are NEVER in the Snapshot (server-side only — ExportSnapshot does not
// read the cache_tokens table). Best-effort TouchCacheToken AFTER the body is
// written — a touch failure is logged, not fatal (the pull already succeeded).
//
// An UNBOUND device code (pre-Plan-39 legacy migration state) is refused with
// 403 — deliberately NOT 401: a pinned 401 is the Plan-34 quarantine trigger
// (the client destroys its local cache on it). 403 keeps the client's cache
// intact and names the owner-side repair (`cache-tokens bind`).
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
	ct, err := r.st.GetCacheToken(ti.UserID)
	if err != nil {
		// A store fault is NOT an authorization verdict — 403 here would send
		// the owner chasing a pointless `cache-tokens bind` while the real DB
		// error stays buried (code-review #5). 500 + stderr, like the export
		// branch below.
		fmt.Fprintf(os.Stderr, "sshmgr serve: cache token lookup %s: %v\n", ti.UserID, err)
		http.Error(w, "cache token lookup failed", http.StatusInternalServerError)
		return
	}
	if ct == nil {
		http.Error(w, "no authenticated cache token", http.StatusForbidden) // fail closed
		return
	}
	if ct.ProfileID == "" {
		fmt.Fprintf(os.Stderr, "sshmgr serve: cache token %s unbound (device %s) — refusing snapshot; owner: cache-tokens bind\n", ct.ID, ct.Name)
		http.Error(w, "device code not bound to a profile — owner: run `sshmgr cache-tokens bind "+ct.Name+" <profile>` on the server", http.StatusForbidden)
		return
	}
	snap, err := r.st.ExportSnapshotForProfile(ct.ProfileID)
	if err != nil {
		http.Error(w, "snapshot unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Plan 39 provenance: this response IS the bound-profile-cropped snapshot.
	// DoPull records it in cache.meta (scoped=true) so `cache status` / the
	// client header can tell a cropped cache from a pre-Plan-39 whole-vault
	// one (identical snapshot SHAPE when the vault has one profile — the
	// header is the only discriminator). Old clients ignore it.
	w.Header().Set("X-Sshmgr-Snapshot-Scope", "profile")
	// Plan 40 §2.3: the device code's NAME rides the same trusted channel — the
	// client uses it to route/verify instance identity (instances/<name>/). Not a
	// security boundary (the name is not a secret; pinned TLS blocks tampering);
	// old clients ignore it.
	w.Header().Set("X-Sshmgr-Device-Name", ct.Name)
	// The snapshot body is the decrypted credential dump — never let an
	// intermediary (CDN/proxy/HTTP cache) store it. no-store + no-cache (the
	// latter also forbids reading a cached copy without revalidation).
	w.Header().Set("Cache-Control", "no-store, no-cache")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		return // client gone; nothing more to do
	}
	if err := r.st.TouchCacheToken(ti.UserID); err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: cache-tokens touch %s: %v\n", ti.UserID, err)
	}
}

// ---- Plan 48: POST /pin-hostkey — anchored forwarding (spec §1) ----

// pinMaxBodyBytes is the POST /pin-hostkey request-body cap (Plan 48 §1.1: a
// NEW 64 KiB constant; the repo precedent is /pair's 1 KiB — a forwarded SSH
// wire-format public key is two orders of magnitude smaller, so this bounds
// memory with generous headroom).
const pinMaxBodyBytes = 64 << 10

// pinHostKeyRequest is the §1.1 request body: the base64 of the marshaled SSH
// wire-format public key exactly as the client's HostKeyTOFU callback saw it.
type pinHostKeyRequest struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	KeyBlob string `json:"key_blob"`
}

// The §1.1 response bodies — verbatim field contracts (the client's §2.1
// branch table keys off exactly these shapes; the 409 NEVER carries a
// fingerprint).
type pinHostKeyResponse struct {
	Fingerprint string `json:"fingerprint"`
	ServerName  string `json:"server_name"`
}

type pinConflictResponse struct {
	Error string `json:"error"`
	Equal bool   `json:"equal"`
}

type pinErrorResponse struct {
	Error string `json:"error"`
}

// writePinJSON emits one of the §1.1 JSON bodies with its status header.
func writePinJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handlePinHostkey lands a device-forwarded host-key anchor. The guard
// sequence is ①–⑥ nailed by spec §1.2; every request leaves ONE stderr line
// (the deferred log) — a rejected probe (403/400/409) is otherwise completely
// traceless, and that line is the minimum detection surface (rev3).
//
// 401 semantics stay pure RequireBearerToken: the Plan-34 cache quarantine
// lives only on the pull path — a bad token on THIS route is an ordinary
// failure and must never destroy the client's local cache (§1.2).
func (r *ServeRunner) handlePinHostkey(w http.ResponseWriter, req *http.Request) {
	host, port := "", 0
	device := "-"
	status := http.StatusInternalServerError
	defer func() {
		// %q on both request-provided fields: an authenticated rogue device can
		// send arbitrary bytes in host, and this line is the probe forensics
		// surface — without quoting, embedded newlines forge service-log lines.
		fmt.Fprintf(os.Stderr, "sshmgr serve: pin-hostkey %q:%d -> %d (device %q)\n", host, port, status, device)
	}()

	// ① shape guards — method, body cap, JSON decode, field presence — ALL
	// before any vault access, so a malformed probe can never ride the 403
	// "not granted" wording (JSON/field validation precedes guard ③).
	if req.Method != http.MethodPost {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	if req.ContentLength > pinMaxBodyBytes {
		status = http.StatusRequestEntityTooLarge
		http.Error(w, "request body too large", status)
		return
	}
	// A lied-about (or absent) ContentLength falls through to MaxBytesReader
	// and is reclassified as 413 at decode time (the pairDecode precedent).
	req.Body = http.MaxBytesReader(w, req.Body, pinMaxBodyBytes)
	var in pinHostKeyRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			status = http.StatusRequestEntityTooLarge
			http.Error(w, "request body too large", status)
			return
		}
		status = http.StatusBadRequest
		writePinJSON(w, status, pinErrorResponse{Error: "unparseable key_blob"}) // §1.1's one 400 body
		return
	}
	if in.Host == "" || in.Port <= 0 || in.KeyBlob == "" {
		status = http.StatusBadRequest
		writePinJSON(w, status, pinErrorResponse{Error: "unparseable key_blob"})
		return
	}
	host, port = in.Host, in.Port

	// ② the middleware's TokenInfo carries only the cache-token id; the row
	// (and its profile binding) is re-resolved here, same as handleSnapshot.
	ti := auth.TokenInfoFromContext(req.Context())
	if ti == nil || ti.UserID == "" {
		status = http.StatusForbidden
		http.Error(w, "no authenticated cache token", status) // fail closed
		return
	}
	ct, err := r.st.GetCacheToken(ti.UserID)
	if err != nil {
		// A store fault is NOT an authorization verdict — 403 here would send
		// the owner chasing a pointless `cache-tokens bind` while the real DB
		// error stays buried (the handleSnapshot lesson, verbatim).
		fmt.Fprintf(os.Stderr, "sshmgr serve: cache token lookup %s: %v\n", ti.UserID, err)
		status = http.StatusInternalServerError
		http.Error(w, "cache token lookup failed", status)
		return
	}
	if ct == nil {
		status = http.StatusForbidden
		http.Error(w, "no authenticated cache token", status) // fail closed
		return
	}
	device = ct.Name
	if ct.ProfileID == "" {
		status = http.StatusForbidden
		http.Error(w, "device code not bound to a profile — owner: run `sshmgr cache-tokens bind "+ct.Name+" <profile>` on the server", status)
		return
	}

	// ③ candidate walk. ServersForProfile's row order is unspecified (it
	// feeds the TUI), so the ids are sorted HERE — the first host+port hit is
	// then the smallest-id candidate, the ONE deterministic attribution used
	// for both the audit row and the response's server_name (§1.1/§1.2 ③⑤).
	ids, err := r.st.ServersForProfile(ct.ProfileID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: pin-hostkey grant walk %s: %v\n", ct.ProfileID, err)
		status = http.StatusInternalServerError
		http.Error(w, "grant walk failed", status)
		return
	}
	sort.Strings(ids)
	var attributed *models.Server
	for _, id := range ids {
		srv, err := r.st.GetServer(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sshmgr serve: pin-hostkey server lookup %s: %v\n", id, err)
			status = http.StatusInternalServerError
			http.Error(w, "server lookup failed", status)
			return
		}
		if srv == nil {
			// The row vanished mid-walk (servers rm race) — skip it, never
			// dereference (the core.go GetServer nil-check convention).
			continue
		}
		if srv.Host == host && srv.Port == port {
			attributed = srv
			break
		}
	}
	if attributed == nil {
		// Deliberately vague: the body names no profile, no entry, no count —
		// a device code must not learn vault shape from a refused pin (the
		// Plan 31/39 boundary; only the requester-supplied address is echoed).
		status = http.StatusForbidden
		http.Error(w, fmt.Sprintf("pin forwarding refused for %s:%d — no matching server entry is granted to this device's bound profile; ask the owner to grant the entry (or rebind via cache-tokens bind) and cache pull again", host, port), status)
		return
	}

	// ④ the forwarded bytes must parse as an SSH public key; the STORED value
	// is the re-marshaled canonical form, byte-comparable with what a TOFU
	// callback's remote.Marshal() produces (§1.2 ④).
	raw, err := base64.StdEncoding.DecodeString(in.KeyBlob)
	if err != nil {
		status = http.StatusBadRequest
		writePinJSON(w, status, pinErrorResponse{Error: "unparseable key_blob"})
		return
	}
	pub, err := ssh.ParsePublicKey(raw)
	if err != nil {
		status = http.StatusBadRequest
		writePinJSON(w, status, pinErrorResponse{Error: "unparseable key_blob"})
		return
	}
	canonical := pub.Marshal()
	fp := ssh.FingerprintSHA256(pub)

	// The affects list is the OWNER full-vault view (ListServers — the device
	// never receives it): every entry sharing this host:port is affected by
	// the anchor's creation, and the audit line must let `audit --server`
	// find "pinned me" for EACH of them (§6, symmetric with pin-clear).
	all, err := r.st.ListServers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: pin-hostkey affects walk: %v\n", err)
		status = http.StatusInternalServerError
		http.Error(w, "server lookup failed", status)
		return
	}
	var affected []string
	for _, s := range all {
		if s.Host == host && s.Port == port {
			affected = append(affected, s.Name)
		}
	}
	// The audit row is CONSTRUCTED here and written by InsertForwardedPin
	// inside the insert transaction (§1.2 ⑤): a pin never lands without its
	// history, and a rolled-back conflict leaves no audit row. project_id
	// stays empty — a device initiated this, never a project (§6).
	audit := store.AuditRow{
		TS:       time.Now(),
		ServerID: attributed.ID,
		Action:   "pin-forward",
		Command: fmt.Sprintf("host=%s:%d fp=%s device=%s via=forward affects=%d entries: %s",
			host, port, fp, ct.Name, len(affected), strings.Join(affected, ", ")),
		Status: "ok",
	}

	// ⑤ atomic landing. InsertForwardedPin answers insert-only's three
	// outcomes with two values: equal=true is a matched existing anchor (the
	// dual-mode judgment ran INSIDE the transaction); but a fresh insert AND
	// a lost race to a different key both return (false, nil) — so the
	// surviving anchor is point-read to tell them apart: ours → 201, someone
	// else's → 409 equal=false.
	equal, err := r.st.InsertForwardedPin(host, port, canonical, ct.Name, audit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: pin-hostkey insert %s:%d: %v\n", host, port, err)
		status = http.StatusInternalServerError
		http.Error(w, "pin landing failed", status)
		return
	}
	if equal {
		// §1.1: the conflict body never echoes the fingerprint; equal lets an
		// honest client auto-close a duplicate forward (§2.1).
		status = http.StatusConflict
		writePinJSON(w, status, pinConflictResponse{Error: "already pinned", Equal: true})
		return
	}
	landed, err := r.st.GetHostKey(host, port)
	if err != nil || landed == nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: pin-hostkey confirm %s:%d: %v\n", host, port, err)
		status = http.StatusInternalServerError
		http.Error(w, "pin landing failed", status)
		return
	}
	if !bytes.Equal(landed.Blob, canonical) {
		// Lost the race to a different key — insert-only means our bytes are
		// NOT what persisted. (An owner --force landing exactly inside this
		// commit→read window would mislabel a real win as a conflict; the
		// client's pull-and-retry recovery converges regardless — accepted
		// micro-race, same class as the in-tx judgment's justification.)
		status = http.StatusConflict
		writePinJSON(w, status, pinConflictResponse{Error: "already pinned", Equal: false})
		return
	}

	// ⑥
	status = http.StatusCreated
	writePinJSON(w, status, pinHostKeyResponse{Fingerprint: fp, ServerName: attributed.Name})
}

// ServeOpts carries the explicitly-set CLI flag inputs for the serve switches
// (Plan 42 批1 T6): a non-nil field = the flag was passed and carries that
// value; nil = the flag is absent (defer to env → store → default). The env
// seams are read INSIDE RunServe via envSwitch, so the foreground and the
// service-managed paths behave identically (the service re-runs the same
// `serve --flags...` command line, so flags ride along automatically).
type ServeOpts struct {
	DiscoveryFlag *bool
	PairingFlag   *bool
}

// RunServe runs the serve HTTP server until ctx is cancelled (SIGINT/SIGTERM,
// wired by the caller). Post-Plan-42-批1 the served surface is the
// authenticated /snapshot route plus the unauthenticated SAS pairing surface
// /pair/* (Plan 42 §3.3; the MCP-over-HTTP agent surface was removed — see
// HTTPHandler). The listener is created synchronously so bind errors surface
// before serving.
// TLS is ALWAYS used: if tlsCert is the operator's explicit --tls-cert, that
// cert is served (backward compat); if tlsCert is empty, RunServe auto-generates
// + loads a self-signed cert (LoadOrCreateServeCert) and serves that, logging
// its fingerprint so the operator can distribute the pin to clients. A
// LoadOrCreateServeCert failure is returned (serve refuses to start — never
// silently downgrades to plaintext). Returns nil on clean ctx-cancelled shutdown.
//
// Startup side effects (Plan 42 批1): in-flight pairing rows are expired (the
// in-memory X25519 keys died with the previous process — a stale client poll
// must get the frozen 410), the injected switch inputs are resolved (ServeOpts
// flags + env seams), and the UDP discovery responder is started on ctx (an
// enhancement surface — it never fails the serve).
func RunServe(ctx context.Context, st *store.Store, addr, tlsCert, tlsKey string, opts ServeOpts) error {
	runner, err := NewServeRunner(st)
	if err != nil {
		return err
	}
	defer runner.Close()

	// T5→T6 移交:serve 重启后内存 X25519 私钥已丢,in-flight 配对行永远
	// finish 不完 — 启动时统一过期,stale client 的 finish poll 立刻得到冻结
	// 的 410(ErrPairingWindow)。失败只记一行(表卫生性质,不拦 serve)。
	if err := st.ExpireInFlightPairings(); err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: expire in-flight pairings: %v\n", err)
	}

	// 开关注入(Plan 42 批1 T2/T6):显式 env 在此读取(前台/服务路径一致),
	// 显式 flag 由 CLI 经 ServeOpts 注入;store/缺省两层由 switch 机制解析。
	runner.RefreshSwitches(envSwitch(envServePairing), opts.PairingFlag,
		envSwitch(envServeDiscovery), opts.DiscoveryFlag)

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

	// Pairing signature state (Plan 42 批1 T5): the serve cert's ed25519 key
	// signs every pairing transcript and its SPKI pin rides the sealed
	// envelope. A load failure (operator-supplied non-ed25519 --tls-cert) is
	// logged and serve continues — /pair enroll answers 500, the TLS surface
	// is unaffected (LoadPairingSigner doc).
	if err := runner.LoadPairingSigner(tlsCert, tlsKey); err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: /pair disabled: serve key unusable for pairing signatures: %v\n", err)
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
	fmt.Fprintf(os.Stderr, "sshmgr serve: listening on %s (tls=%s)\n", addr, tlsLabel)
	if autoTLSFingerprint != "" {
		fmt.Fprintf(os.Stderr, "auto-TLS cert (self-signed). client pin: %s\n", autoTLSFingerprint)
	}

	// UDP discovery (Plan 42 批1 T6/T7): the state line sits next to the
	// listening line; the responder's lifecycle rides ctx (stop is deferred as
	// well, since stop and the ctx hookup are the same idempotent Once).
	// StartDiscovery binds the socket regardless of the switch state (T7) —
	// off is answered per packet, so an off→on flip needs no restart.
	discLabel := "off"
	if runner.DiscoveryEnabled() {
		discLabel = "on"
	}
	fmt.Fprintf(os.Stderr, "sshmgr serve: discovery: udp/%d (%s)\n", discoveryPort, discLabel)
	discName := ""
	if v, ok, gerr := st.GetSetting(settingDiscoveryName); gerr == nil && ok {
		discName = v // 缺省(未设/读失败)由 StartDiscovery 兜底到 hostname
	}
	tcpPort := discoveryPort
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		tcpPort = ta.Port // offer 带 TCP 真实端口(测试/非常规 --addr 端口都对)
	}
	stopDiscovery := StartDiscovery(ctx, discName, tcpPort, runner.pairSPKI, runner.DiscoveryEnabled)
	defer stopDiscovery()

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
