package mcpserver

// Plan 51 §1 — POST /server-metadata: the broker side of client metadata
// editing. A cache-mode client (device-code authenticated, same gate as
// /snapshot and /pin-hostkey) applies a PARTIAL edit to one server entry's
// six metadata fields under an optimistic-lock CAS on servers.revision; the
// change and its audit row land in one transaction (store.UpdateForwardedMetadata).
// Guard discipline, stderr-line convention and the 401-does-not-quarantine
// note all follow handlePinHostkey verbatim.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ssh-manager-mcp/internal/store"
)

// metaMaxBodyBytes is the POST /server-metadata request-body cap (Plan 51
// §1.1): six fields × the 4 KiB per-field cap + JSON overhead, with headroom.
// Repo precedent: /pin-hostkey's 64 KiB, /pair's 1 KiB.
const metaMaxBodyBytes = 32 << 10

// metaEditRequest is the §1.1 request body. Fields keys are the six-field
// whitelist (role/services/location/hardware/caveats/description); a key's
// ABSENCE means "keep", a present empty string means "clear", and null is
// rejected — the pointer shape carries exactly that three-way distinction.
type metaEditRequest struct {
	ServerID         string             `json:"server_id"`
	ExpectedRevision int64              `json:"expected_revision"`
	Fields           map[string]*string `json:"fields"`
}

// metaEditResponse is the 200 body: what the client mirrors locally
// (revision/updated_at are the BROKER's values, not the client's now()).
type metaEditResponse struct {
	ServerName string `json:"server_name"`
	Revision   int64  `json:"revision"`
	UpdatedAt  int64  `json:"updated_at"`
}

// metaConflictResponse is the 409 body. current_revision + the six fields'
// current values let an agent merge its intent and retry in ONE hop, without
// a cache pull. Zero-leak argument (the pin-forward equal precedent): the
// requester's device code is granted this entry, so everything in this body
// is a subset of what its next /snapshot pull already returns.
type metaConflictResponse struct {
	Error           string            `json:"error"`
	CurrentRevision int64             `json:"current_revision"`
	Current         map[string]string `json:"current"`
}

type metaErrorResponse struct {
	Error string `json:"error"`
}

func writeMetaJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// metaGrantRefusedText is the deliberately vague grant-refusal body: it names
// no profile, no entry count, no vault shape (Plan 31/39 boundary) — only the
// requester-supplied id is echoed.
func metaGrantRefusedText(serverID string) string {
	return fmt.Sprintf("metadata edit refused for server %s — no matching entry is granted to this device's bound profile; ask the owner to grant the entry (or rebind via cache-tokens bind) and cache pull again", serverID)
}

// handleServerMetadata lands a device-forwarded metadata edit. The guard
// sequence ①–⑥ follows spec §1.2; every request leaves ONE stderr line (the
// deferred log) — a rejected probe is otherwise traceless.
//
// 401 semantics stay pure RequireBearerToken: the Plan-34 cache quarantine
// lives only on the pull path — a bad token on THIS route is an ordinary
// failure and must never destroy the client's local cache.
func (r *ServeRunner) handleServerMetadata(w http.ResponseWriter, req *http.Request) {
	serverID := "-"
	device := "-"
	status := http.StatusInternalServerError
	defer func() {
		fmt.Fprintf(os.Stderr, "sshmgr serve: server-metadata %q -> %d (device %q)\n", serverID, status, device)
	}()

	// ① shape guards — method, body cap, JSON decode, field presence — ALL
	// before any vault access.
	if req.Method != http.MethodPost {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	if req.ContentLength > metaMaxBodyBytes {
		status = http.StatusRequestEntityTooLarge
		http.Error(w, "request body too large", status)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, metaMaxBodyBytes)
	var in metaEditRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			status = http.StatusRequestEntityTooLarge
			http.Error(w, "request body too large", status)
			return
		}
		status = http.StatusBadRequest
		writeMetaJSON(w, status, metaErrorResponse{Error: "unparseable request"})
		return
	}
	if in.ServerID == "" {
		status = http.StatusBadRequest
		writeMetaJSON(w, status, metaErrorResponse{Error: "server_id must not be empty"})
		return
	}
	serverID = in.ServerID
	if in.ExpectedRevision < 0 {
		status = http.StatusBadRequest
		writeMetaJSON(w, status, metaErrorResponse{Error: "expected_revision must be >= 0"})
		return
	}
	if err := validateMetaRequestFields(in.Fields); err != nil {
		status = http.StatusBadRequest
		writeMetaJSON(w, status, metaErrorResponse{Error: err.Error()})
		return
	}

	// ② TokenInfo → GetCacheToken (the handleSnapshot/handlePinHostkey lesson:
	// a store fault is 500, never a 403 that sends the owner chasing bind).
	ti := auth.TokenInfoFromContext(req.Context())
	if ti == nil || ti.UserID == "" {
		status = http.StatusForbidden
		http.Error(w, "no authenticated cache token", status) // fail closed
		return
	}
	ct, err := r.st.GetCacheToken(ti.UserID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: server-metadata cache token lookup %s: %v\n", ti.UserID, err)
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

	// ③ the global switch: off is an OWNER decision, not a grant question —
	// the body says "disabled" so the client surfaces the right remedy.
	if !r.MetadataEditEnabled() {
		status = http.StatusForbidden
		http.Error(w, "metadata editing is disabled by the owner — ask the owner to enable it (serve --metadata-edit)", status)
		return
	}

	// ④ grant check: membership in the device's bound-profile grant set.
	// ids (not host:port walks — metadata keys by entry id).
	granted, err := r.st.ServersForProfile(ct.ProfileID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: server-metadata grant walk %s: %v\n", ct.ProfileID, err)
		status = http.StatusInternalServerError
		http.Error(w, "grant walk failed", status)
		return
	}
	ok := false
	for _, id := range granted {
		if id == in.ServerID {
			ok = true
			break
		}
	}
	if !ok {
		status = http.StatusForbidden
		http.Error(w, metaGrantRefusedText(in.ServerID), status)
		return
	}

	// ⑤+⑥ atomic landing: validate sizes, CAS on revision, write fields,
	// bump, audit — all inside UpdateForwardedMetadata's single transaction.
	newRev, err := r.st.UpdateForwardedMetadata(in.ServerID, in.Fields, in.ExpectedRevision, ct.Name)
	var stale *store.ErrStaleRevision
	var tooBig *store.ErrMetaFieldTooLarge
	switch {
	case err == nil:
	case errors.As(err, &tooBig):
		status = http.StatusBadRequest
		writeMetaJSON(w, status, metaErrorResponse{Error: err.Error()})
		return
	case errors.Is(err, store.ErrServerGone):
		// Vanished between the grant walk and the tx (servers rm race) — the
		// same vague 403 as "not granted": either way the entry is gone.
		status = http.StatusForbidden
		http.Error(w, metaGrantRefusedText(in.ServerID), status)
		return
	case errors.As(err, &stale):
		// 409 + current values: one-hop retry without a pull. The values are
		// advisory (the row may move again) — merging against them converges.
		cur, gerr := r.st.GetServer(in.ServerID)
		if gerr != nil || cur == nil {
			fmt.Fprintf(os.Stderr, "sshmgr serve: server-metadata current read %s: %v\n", in.ServerID, gerr)
			status = http.StatusInternalServerError
			http.Error(w, "metadata edit conflicted and the current row could not be read", status)
			return
		}
		status = http.StatusConflict
		writeMetaJSON(w, status, metaConflictResponse{
			Error:           "stale revision",
			CurrentRevision: stale.Current,
			Current: map[string]string{
				"role":        cur.Role,
				"services":    cur.Services,
				"location":    cur.Location,
				"hardware":    cur.Hardware,
				"caveats":     cur.Caveats,
				"description": cur.Description,
			},
		})
		return
	default:
		fmt.Fprintf(os.Stderr, "sshmgr serve: server-metadata write %s: %v\n", in.ServerID, err)
		status = http.StatusInternalServerError
		http.Error(w, "metadata edit landing failed", status)
		return
	}

	// ⑦ 200 — name/updated_at from the freshly written row (GetServer reads
	// the committed values; the response mirrors what the client applies
	// locally).
	srv, err := r.st.GetServer(in.ServerID)
	if err != nil || srv == nil {
		// Committed but unreadable — the edit IS landed; report it with the
		// values we know rather than fail the client into a blind retry.
		fmt.Fprintf(os.Stderr, "sshmgr serve: server-metadata confirm read %s: %v\n", in.ServerID, err)
		status = http.StatusOK
		writeMetaJSON(w, status, metaEditResponse{ServerName: "", Revision: newRev, UpdatedAt: 0})
		return
	}
	status = http.StatusOK
	writeMetaJSON(w, status, metaEditResponse{ServerName: srv.Name, Revision: newRev, UpdatedAt: srv.UpdatedAt.Unix()})
}

// validateMetaRequestFields is the route-side half of the whitelist gate:
// unknown keys (a map absorbs them — this is the DisallowUnknownFields
// equivalent), null values, and the empty edit. Per-field SIZE lives in the
// store primitive (one copy, defense in depth on both sides of the wire).
func validateMetaRequestFields(fields map[string]*string) error {
	if len(fields) == 0 {
		return fmt.Errorf("fields must carry at least one of: %s", strings.Join(store.MetadataFieldList(), ", "))
	}
	for k, v := range fields {
		if !store.ValidMetadataField(k) {
			return fmt.Errorf("unknown metadata field %q (allowed: %s)", k, strings.Join(store.MetadataFieldList(), ", "))
		}
		if v == nil {
			return fmt.Errorf("metadata field %q is null (values must be strings — empty string clears)", k)
		}
	}
	return nil
}
