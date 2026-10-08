// Plan 51 §2 — the client side of metadata editing: a cache-mode client's
// update_server_metadata tool POSTs a partial field edit (plus the
// optimistic-lock revision it read via list_servers) to the broker's
// /server-metadata endpoint; on 200 the caller mirrors the accepted write
// onto the local read-only store (store.ApplyForwardedMetadata). This file
// owns the ENTIRE §2.1 branch table and its verbatim texts — asserted
// byte-for-byte by metadata_forward_test.go; rewording any of them breaks
// the suite. The construction/transport skeleton is PinForwarder's twin
// (forward.go): pinned TLS only, no redirects, SplitTokenPin, fail-closed
// no-capability flavors.
package clientops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

// metaForwardTimeout bounds one metadata-edit POST. Unlike pin forwarding
// (10s — it sits on the SSH handshake critical path), a metadata edit is an
// ordinary agent tool call; 30s rides out a busy broker without hanging the
// tool.
const metaForwardTimeout = 30 * time.Second

// MetaEditResult is the 200 outcome: exactly what the local mirror applies.
type MetaEditResult struct {
	ServerName string
	Revision   int64
	UpdatedAt  int64
}

// MetaStaleError is the typed 409 outcome: the broker's current revision plus
// the six fields' current VALUES — the caller surfaces them so the agent can
// merge its intent and retry in one hop, no cache pull. Zero-leak: everything
// here is a subset of what this device code's next snapshot pull returns.
type MetaStaleError struct {
	ServerID      string
	Current       int64
	CurrentValues map[string]string
}

func (e *MetaStaleError) Error() string {
	return metaMsg409(e.ServerID, e.Current, e.CurrentValues)
}

// ---- §2.1 branch texts (verbatim contract) ----

// metaMsg401 is SHARED with pin forwarding (forwardMsg401, verbatim): a
// rejected device code is an ordinary failure on any forward path — the
// Plan-34 quarantine lives only on the pull path.
// (forwardMsg401 in forward.go is the one definition; referenced by tests.)

func metaMsg403(serverID string) string {
	return fmt.Sprintf("the broker refused the metadata edit for %s — the entry is probably not granted to this device's bound profile; ask the owner to grant it (or rebind via cache-tokens bind) and cache pull again", serverID)
}

// metaMsgDisabled: the broker's global switch is off — an owner decision with
// an owner-side remedy, distinct from a grant refusal. The serve 403 body
// carries the marker word "disabled"; this is the client's own text.
const metaMsgDisabled = "metadata editing is disabled on the broker — ask the owner to enable it (serve --metadata-edit)"

// metaMsg404: an old broker has no /server-metadata route.
const metaMsg404 = "the broker does not support metadata editing (the owner must upgrade the broker to >= v0.19.0, then retry)"

// metaMsg409 renders the stale-revision branch INCLUDING the current values —
// the agent's merge input. Field order is fixed (the six in whitelist order)
// so the text is deterministic under map iteration.
func metaMsg409(serverID string, current int64, vals map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "stale revision for %s — the entry changed since your listing (current revision %d); current values:", serverID, current)
	for _, k := range []string{"role", "services", "location", "hardware", "caveats", "description"} {
		fmt.Fprintf(&b, " %s=%q", k, vals[k])
	}
	fmt.Fprintf(&b, "; merge your intent with these values and retry with expected_revision=%d", current)
	return b.String()
}

// metaMsgPlaintext: editing is a security-sensitive change channel — strictly
// no plaintext, same posture as pin forwarding (no pull-style escape hatch).
const metaMsgPlaintext = "metadata editing requires a pinned TLS server — no plaintext editing (set the server pin used by cache pull); nothing was changed"

// metaMainMessage is the ONE fail-closed presentation for "the edit could not
// reach the broker" (unreachable, 5xx, timeout, unexpected status, missing
// credential). It states the nothing-was-applied guarantee explicitly.
func metaMainMessage(serverID string) string {
	return fmt.Sprintf("metadata edit for %s could not reach the broker — the edit was NOT applied anywhere; retry while the broker is reachable (your local cache is unchanged)", serverID)
}

// metaError is a branch-mapped failure (forwardError's twin): Error() IS the
// verbatim §2.1 text; err carries the cause (or a typed sentinel such as
// *MetaStaleError) for errors.Is/As.
type metaError struct {
	text string
	err  error
}

func (e *metaError) Error() string { return e.text }
func (e *metaError) Unwrap() error { return e.err }

// metaEditRequest is the wire body (the serve-side definition lives in
// mcpserver/serve_meta.go — the JSON field names are the contract).
type metaEditRequest struct {
	ServerID         string             `json:"server_id"`
	ExpectedRevision int64              `json:"expected_revision"`
	Fields           map[string]*string `json:"fields"`
}

// metaEditResponse is the 200 shape.
type metaEditResponse struct {
	ServerName string `json:"server_name"`
	Revision   int64  `json:"revision"`
	UpdatedAt  int64  `json:"updated_at"`
}

// metaConflictResponse is the 409 shape; an unparsable body degrades to a
// revision-less stale error (fail closed — the hard error sends the agent to
// re-list; silently passing would not).
type metaConflictResponse struct {
	Error           string            `json:"error"`
	CurrentRevision int64             `json:"current_revision"`
	Current         map[string]string `json:"current"`
}

type metaErrorBody struct {
	Error string `json:"error"`
}

// MetadataForwarder forwards partial metadata edits to the serve broker's
// POST /server-metadata. Constructed from the instance's CacheCred (the
// --instance resolver in cli owns that read); the capability decision happens
// ONCE at construction, exactly like PinForwarder:
//   - capable: pin set + https URL → pinned-TLS client, 30s cap;
//   - plaintext flavor (Pin == ""): Edit refuses — never plaintext editing;
//   - missing-credential flavor (URL/Token empty): Edit fails closed with the
//     main message, like an unreachable broker. Never a silent local write.
type MetadataForwarder struct {
	url     string
	code    string // bare device code for the Authorization header
	client  *http.Client
	capable bool
	noCred  bool
}

// NewMetadataForwarder builds the forwarder for one cache credential (the
// PinForwarder twin). Errors only for a hard misconfiguration (non-https URL
// paired with a pin); everything else degrades to a no-capability instance
// whose Edit fails closed with the mapped text.
func NewMetadataForwarder(cred CacheCred) (*MetadataForwarder, error) {
	if cred.URL == "" || cred.Token == "" {
		return &MetadataForwarder{noCred: true}, nil
	}
	if cred.Pin == "" {
		return &MetadataForwarder{}, nil // plaintext flavor
	}
	if u, err := neturl.Parse(cred.URL); err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("metadata editing requires an https:// url when a server pin is set (got %q)", cred.URL)
	}
	tr, err := pinningTransport(cred.Pin)
	if err != nil {
		return nil, err
	}
	code := cred.Token
	if c, _, ok := SplitTokenPin(cred.Token); ok {
		code = c
	}
	return &MetadataForwarder{
		url:  strings.TrimRight(cred.URL, "/"),
		code: code,
		client: &http.Client{
			Transport: tr,
			Timeout:   metaForwardTimeout,
			// Redirects are never followed (the DoPull/pin-forward posture): a
			// followed 30x would leave the pinned transport — and a
			// redirecting "broker" is not the broker.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		capable: true,
	}, nil
}

// Edit POSTs the partial metadata edit (§2.1 branch table). fields keys are
// the six-field whitelist; a nil VALUE is never valid (the caller builds the
// map from non-nil tool params). Every failure returns an error whose
// Error() is the branch's verbatim text; the 409 outcome is a *MetaStaleError
// (whose own Error() is the retry-ready text) for errors.As branching.
func (f *MetadataForwarder) Edit(serverID string, expectedRevision int64, fields map[string]*string) (MetaEditResult, error) {
	if !f.capable {
		if f.noCred {
			return MetaEditResult{}, &metaError{text: metaMainMessage(serverID)}
		}
		return MetaEditResult{}, &metaError{text: metaMsgPlaintext}
	}
	body, err := json.Marshal(metaEditRequest{
		ServerID:         serverID,
		ExpectedRevision: expectedRevision,
		Fields:           fields,
	})
	if err != nil {
		return MetaEditResult{}, &metaError{text: metaMainMessage(serverID), err: err}
	}
	req, err := http.NewRequest(http.MethodPost, f.url+"/server-metadata", bytes.NewReader(body))
	if err != nil {
		return MetaEditResult{}, &metaError{text: metaMainMessage(serverID), err: err}
	}
	req.Header.Set("Authorization", "Bearer "+f.code)
	res, err := f.client.Do(req)
	if err != nil {
		return MetaEditResult{}, &metaError{text: metaMainMessage(serverID), err: err}
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode == http.StatusOK:
		var out metaEditResponse
		if err := json.NewDecoder(io.LimitReader(res.Body, forwardBodyCap)).Decode(&out); err != nil {
			return MetaEditResult{}, &metaError{text: metaMainMessage(serverID), err: err}
		}
		return MetaEditResult{ServerName: out.ServerName, Revision: out.Revision, UpdatedAt: out.UpdatedAt}, nil
	case res.StatusCode == http.StatusUnauthorized:
		return MetaEditResult{}, &metaError{text: forwardMsg401}
	case res.StatusCode == http.StatusForbidden:
		// Two 403 flavors: the global switch (body says "disabled") vs the
		// grant refusal. The bodies are http.Error texts from OUR serve; a
		// foreign 403 body without the marker degrades to the grant text.
		if bodyHasDisabledMarker(res) {
			return MetaEditResult{}, &metaError{text: metaMsgDisabled}
		}
		return MetaEditResult{}, &metaError{text: metaMsg403(serverID)}
	case res.StatusCode == http.StatusNotFound:
		return MetaEditResult{}, &metaError{text: metaMsg404}
	case res.StatusCode == http.StatusConflict:
		var cr metaConflictResponse
		if jerr := json.NewDecoder(io.LimitReader(res.Body, forwardBodyCap)).Decode(&cr); jerr != nil || cr.CurrentRevision < 0 {
			// Fail closed: an unparsable 409 still means "retry from fresh
			// state"; report revision-less so the agent re-lists.
			return MetaEditResult{}, &metaError{
				text: fmt.Sprintf("stale revision for %s — the entry changed since your listing; re-run list_servers and retry", serverID),
				err:  jerr,
			}
		}
		stale := &MetaStaleError{ServerID: serverID, Current: cr.CurrentRevision, CurrentValues: cr.Current}
		return MetaEditResult{}, &metaError{text: stale.Error(), err: stale}
	case res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusRequestEntityTooLarge:
		// The client sent something the broker cannot parse or accept — a
		// defect state; pass the server's own text through with the id
		// attached (the pin-forward 400/413 arm).
		return MetaEditResult{}, &metaError{text: fmt.Sprintf("metadata edit for %s: broker returned %d — %s",
			serverID, res.StatusCode, metaServerErrorText(res))}
	default:
		// 5xx and everything unmapped (3xx via ErrUseLastResponse, 405, ...):
		// fail closed — nothing was applied.
		return MetaEditResult{}, &metaError{text: metaMainMessage(serverID),
			err: fmt.Errorf("broker returned %d", res.StatusCode)}
	}
}

// EditMeta adapts Edit onto mcpserver.MetadataEditor — the cache face's
// injection seam (Plan 51 §7). The import direction is clientops →
// mcpserver (pairsession), so the interface lives there and THIS method maps
// the clientops result type into the mcpserver outcome type. Errors pass
// through unchanged (their Error() IS the §2.1 branch text).
func (f *MetadataForwarder) EditMeta(serverID string, expectedRevision int64, fields map[string]*string) (mcpserver.MetadataEditOutcome, error) {
	res, err := f.Edit(serverID, expectedRevision, fields)
	if err != nil {
		return mcpserver.MetadataEditOutcome{}, err
	}
	return mcpserver.MetadataEditOutcome{ServerName: res.ServerName, Revision: res.Revision, UpdatedAt: res.UpdatedAt}, nil
}

// Close releases the pinned transport's pooled connections at shutdown.
// Nil-client flavors are a no-op; Close on a nil forwarder is safe.
func (f *MetadataForwarder) Close() {
	if f == nil || f.client == nil {
		return
	}
	f.client.CloseIdleConnections()
}

// bodyHasDisabledMarker reads (bounded) a 403 body looking for the switch-off
// marker word. Serve's disabled text contains "disabled"; the grant text and
// any foreign body do not. A read failure reads as "not disabled" (grant
// flavor — the more common operational cause).
func bodyHasDisabledMarker(res *http.Response) bool {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, forwardBodyCap))
	return strings.Contains(strings.ToLower(string(raw)), "disabled")
}

// metaServerErrorText extracts the broker's error body for the 400/413
// passthrough (serverErrorText's twin — JSON {"error":…} or raw text,
// control-sanitized, falling back to the status text).
func metaServerErrorText(res *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, forwardBodyCap))
	var er metaErrorBody
	if json.Unmarshal(raw, &er) == nil && er.Error != "" {
		return sanitizePassthrough(er.Error)
	}
	if s := sanitizePassthrough(strings.TrimSpace(string(raw))); s != "" {
		return s
	}
	return http.StatusText(res.StatusCode)
}
