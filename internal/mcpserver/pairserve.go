package mcpserver

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/pairing"
	"ssh-manager-mcp/internal/store"
)

// /pair/* — the SAS pairing HTTP surface (Plan 42 spec §3.3, frozen rev4).
//
// Trust architecture, one screen: this surface is UNAUTHENTICATED by design
// (a fresh device has nothing to present) and issues CREDENTIALS — so every
// guard matters:
//   - the tri-state serve.pairing switch gates the whole prefix (404 when off);
//   - per-IP fixed-window rate limits + ≤1KiB bodies + JSON-only inputs;
//   - credentials NEVER leave except AEAD-sealed under K_creds (derived from
//     the ephemeral X25519 + transcript), and only after the owner approved
//     AND the client proved possession of its ephemeral key via the ack HMAC;
//   - enroll has ZERO side effects beyond a pending row (auto-revoke is
//     deferred into the finish transaction by store.MintPairingCredentials);
//   - the X25519 private keys live ONLY in serve process memory (pairKeys),
//     keyed by the pairing id, dropped when the row reaches a terminal state
//     or its maximum possible lifetime (enroll window + approve window) ends.
//
// The store owns all state transitions (CAS + time predicates); this layer
// only maps store outcomes onto the frozen HTTP codes:
// dup id 409 / bad input 400 / body>1KiB 413 / limited 429 / in-use name 419 /
// ack wrong 403 / not approved 409 / window or replay-limit 410.

const (
	// pairMaxBodyBytes: every /pair/* request body cap (spec §3.3-1 请求体 ≤1KiB).
	pairMaxBodyBytes = 1 << 10
	// pairEnrollWindowSec: enroll → approve window (spec §3.3-2: 10 分钟) —
	// stamped into the row by enroll; store enforces it in the approve CAS.
	pairEnrollWindowSec = 600
	// pairApproveWindowSec: approve → finish window (spec §3.3-2: 120 秒).
	// Mirrors store.pairApprovedWindowSec (frozen on both sides).
	pairApproveWindowSec = 120
	// pairKeyMaxAgeSec: how long the in-memory X25519 private key may live —
	// the longest a row could still be finishable (enroll window + approve
	// window, since approval can happen at the last second of the enroll
	// window). Past this the key is dropped and the row is effectively expired.
	pairKeyMaxAgeSec = pairEnrollWindowSec + pairApproveWindowSec
	// pairDomainPrefix is the frozen transcript domain separator (spec §3.3).
	pairDomainPrefix = "sshmgr-pair-v2"
	// settingPairMaxOffline holds the operator's default max_offline; absent
	// → pairDefaultMaxOffline. Read at finish time, OUTSIDE the mint callback.
	settingPairMaxOffline = "pair.default_max_offline"
	pairDefaultMaxOffline = "24h"
)

// pairKeyEntry is one in-memory ephemeral X25519 private key. deadline is the
// unix second after which finish can no longer succeed for the row (so the
// key is worthless and swept).
type pairKeyEntry struct {
	priv     []byte // X25519 private key bytes (32)
	deadline int64  // unix seconds
}

// pairHintPattern validates profile_hint (spec §3.3-1): optional, but when
// present the FIRST character must be a letter or number (Unicode classes) —
// " leading space" class display hacks are refused with 400.
var pairHintPattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`)

// pairCredsEnvelope is the AEAD plaintext handed to the client at finish
// (spec §3.3-6, frozen shape {spki, profile, device_code, project_token,
// max_offline}). Both credential values are ONE-TIME plaintexts.
type pairCredsEnvelope struct {
	SPKI         string `json:"spki"`
	Profile      string `json:"profile"`
	DeviceCode   string `json:"device_code"`
	ProjectToken string `json:"project_token"`
	MaxOffline   string `json:"max_offline"`
}

// loadPairSigner extracts the pairing signature key + SPKI pin from the serve
// TLS cert: the ed25519 private key (auto-TLS always generates ed25519; an
// operator-supplied RSA/ECDSA --tls-cert cannot sign pairings) and the cert's
// SPKI fingerprint (the same pin clients cache). Called once at RunServe.
func loadPairSigner(certPath, keyPath string) (ed25519.PrivateKey, string, error) {
	k, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", err
	}
	priv, ok := k.PrivateKey.(ed25519.PrivateKey)
	if !ok || len(priv) != ed25519.PrivateKeySize {
		return nil, "", fmt.Errorf("serve key is %T, want ed25519.PrivateKey — pairing signatures need the auto-TLS ed25519 cert", k.PrivateKey)
	}
	if k.Leaf == nil {
		if len(k.Certificate) == 0 {
			return nil, "", errors.New("cert chain empty")
		}
		leaf, perr := x509.ParseCertificate(k.Certificate[0])
		if perr != nil {
			return nil, "", perr
		}
		k.Leaf = leaf
	}
	return priv, SPKIFingerprint(k.Leaf), nil
}

// LoadPairingSigner wires the pairing signer + SPKI pin from a TLS cert/key
// pair on disk (RunServe's startup path, extracted so both callers share one
// body): the serve cert's ed25519 key signs every pairing transcript and its
// SPKI pin rides the sealed envelope. RunServe calls it at startup and only
// logs a failure (the TLS surface itself is unaffected); the clientops pairing
// e2e test calls it to stand up the REAL /pair surface over httptest TLS
// without a full RunServe (no udp/7878 side effects, no port bookkeeping).
// Auto-TLS is always ed25519; an operator-supplied non-ed25519 --tls-cert
// returns an error — /pair enroll then answers 500 (a server-side
// misconfiguration, not a client input problem).
func (r *ServeRunner) LoadPairingSigner(certPath, keyPath string) error {
	signer, spki, err := loadPairSigner(certPath, keyPath)
	if err != nil {
		return err
	}
	r.pairSigner = signer
	r.pairSPKI = spki
	return nil
}

// handlePair is the /pair/ prefix router: switch gate → method/body caps →
// endpoint. The switch gate answers 404 (the surface does not EXIST when
// pairing is off — never a 403 hinting at a hidden feature). Body caps are
// applied here for all three endpoints; the frozen 5s read-timeout concern is
// covered by the body cap + rate limits (a dribbling client cannot grow the
// body past 1KiB and each IP burns its own window) — deliberately no global
// server config changes (spec §3.3-1 note).
func (r *ServeRunner) handlePair(w http.ResponseWriter, req *http.Request) {
	if !r.PairingEnabled() {
		http.NotFound(w, req)
		return
	}
	switch req.URL.Path {
	case "/pair/enroll":
		if !r.pairLimits.enroll.Allow(requestIP(req)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if !pairReadBody(w, req) {
			return
		}
		r.handlePairEnroll(w, req)
	case "/pair/poll":
		if !r.pairLimits.poll.Allow(requestIP(req)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if !pairReadBody(w, req) {
			return
		}
		r.handlePairPoll(w, req)
	case "/pair/finish":
		if !r.pairLimits.finish.Allow(requestIP(req)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if !pairReadBody(w, req) {
			return
		}
		r.handlePairFinish(w, req)
	default:
		http.NotFound(w, req)
	}
}

// pairReadBody enforces POST-only + the 1KiB cap, returning false when the
// response is already written. 413 is distinguished from 400 via
// http.MaxBytesError so an oversized body is never mislabeled a JSON error.
func pairReadBody(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if req.ContentLength > pairMaxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	req.Body = http.MaxBytesReader(w, req.Body, pairMaxBodyBytes)
	return true
}

// requestIP extracts the bare host from RemoteAddr (serve is LAN-direct; no
// proxy header is honored — a spoofable XFF would defeat per-IP limiting).
func requestIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// pairDecode JSON-decodes a capped body into v. Any decode failure → 400,
// except the MaxBytesReader overflow → 413. Returns false when it responded.
func pairDecode(w http.ResponseWriter, req *http.Request, v any) bool {
	if err := json.NewDecoder(req.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, "malformed JSON", http.StatusBadRequest)
		return false
	}
	return true
}

// buildPairTranscript is the frozen transcript composition (spec §3.3: the
// length-prefixing itself lives in pairing.TranscriptParts — the one place
// both ends must call identically):
// "sshmgr-pair-v2" ‖ id ‖ name ‖ target_url ‖ client_pub ‖ cnonce ‖ server_pub ‖ snonce
func buildPairTranscript(id []byte, name, targetURL, clientPub, cnonce, serverPub, snonce []byte) []byte {
	return pairing.TranscriptParts(
		[]byte(pairDomainPrefix), id, name, targetURL, clientPub, cnonce, serverPub, snonce)
}

// ---- enroll ----

type pairEnrollRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TargetURL   string `json:"target_url"`
	ClientPub   string `json:"client_pub"`
	Cnonce      string `json:"cnonce"`
	ProfileHint string `json:"profile_hint"`
}

// handlePairEnroll validates, mints the server's ephemeral X25519 pair,
// signs the transcript with the serve cert key, lands the pending row (the
// ONLY side effect) and answers the three public values so the client can
// show its three-piece SAS line immediately (spec §3.3-1 ④).
func (r *ServeRunner) handlePairEnroll(w http.ResponseWriter, req *http.Request) {
	var in pairEnrollRequest
	if !pairDecode(w, req, &in) {
		return
	}
	id, err := hex.DecodeString(in.ID)
	if err != nil || len(id) != 32 {
		http.Error(w, "id must be 32 bytes hex", http.StatusBadRequest)
		return
	}
	if err := instname.Valid(in.Name); err != nil {
		http.Error(w, "invalid instance name", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.TargetURL) == "" {
		http.Error(w, "target_url required", http.StatusBadRequest)
		return
	}
	clientPub, err := base64.RawURLEncoding.DecodeString(in.ClientPub)
	if err != nil || len(clientPub) != 32 {
		http.Error(w, "client_pub must be 32 bytes base64url", http.StatusBadRequest)
		return
	}
	cnonce, err := base64.RawURLEncoding.DecodeString(in.Cnonce)
	if err != nil || len(cnonce) != 16 {
		http.Error(w, "cnonce must be 16 bytes base64url", http.StatusBadRequest)
		return
	}
	if in.ProfileHint != "" && !pairHintPattern.MatchString(in.ProfileHint) {
		http.Error(w, "invalid profile_hint", http.StatusBadRequest)
		return
	}
	if r.pairSigner == nil {
		// Operator-supplied non-ed25519 --tls-cert: pairing cannot sign. 500
		// (a server-side misconfiguration, not a client input problem).
		http.Error(w, "pairing unavailable: serve cert cannot sign", http.StatusInternalServerError)
		return
	}

	// 撞名只查不改 (spec §3.3-1): active code never pulled → record
	// replace_inactive (the revoke happens in the finish transaction);
	// active and pulled → refuse now. The casefold twin is probed too —
	// variants of an existing name are reserved, and mint would refuse later.
	replaceInactive := false
	if zero, active, err := r.st.ActiveCacheTokenInfo(in.Name); err != nil {
		http.Error(w, "name check failed", http.StatusInternalServerError)
		return
	} else if active {
		if !zero {
			http.Error(w, "device name in use", 419)
			return
		}
		replaceInactive = true
	} else if folded := instname.Fold(in.Name); folded != in.Name {
		if _, active, err := r.st.ActiveCacheTokenInfo(folded); err == nil && active {
			http.Error(w, "device name collides with an existing code (case-insensitive)", 419)
			return
		}
	}

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		http.Error(w, "keygen failed", http.StatusInternalServerError)
		return
	}
	serverPub := priv.PublicKey().Bytes()
	snonce := make([]byte, 16)
	if _, err := rand.Read(snonce); err != nil {
		http.Error(w, "noncegen failed", http.StatusInternalServerError)
		return
	}
	transcript := buildPairTranscript(id, []byte(in.Name), []byte(in.TargetURL), clientPub, cnonce, serverPub, snonce)
	sig := ed25519.Sign(r.pairSigner, transcript)

	now := time.Now().Unix()
	row := &store.PendingPairing{
		ID:              id,
		Name:            in.Name,
		TargetURL:       in.TargetURL,
		ClientPub:       clientPub,
		Cnonce:          cnonce,
		ServerPub:       serverPub,
		Snonce:          snonce,
		Sig:             sig,
		ProfileHint:     in.ProfileHint,
		ReplaceInactive: replaceInactive,
		State:           "pending",
		SourceIP:        requestIP(req),
		EnrollDeadline:  now + pairEnrollWindowSec,
	}
	if err := r.st.AddPendingPairing(row, r.pairPendingPerIP, r.pairPendingGlobal); err != nil {
		switch {
		case errors.Is(err, store.ErrPairingQuota):
			http.Error(w, "pairing queue full", http.StatusTooManyRequests)
		case strings.Contains(err.Error(), "UNIQUE constraint"):
			http.Error(w, "pairing id already enrolled", http.StatusConflict)
		default:
			fmt.Fprintf(os.Stderr, "ssh-manager serve: pairing enroll store error: %v\n", err)
			http.Error(w, "enroll failed", http.StatusInternalServerError)
		}
		return
	}
	// Key state lands AFTER the row (a crashed enroll leaves a row the owner
	// can reject; a key without a row would be unreachable garbage — swept by
	// deadline anyway).
	r.pairStoreKey(id, priv.Bytes(), now+pairKeyMaxAgeSec)

	writeJSON(w, http.StatusOK, map[string]string{
		"server_pub": base64.RawURLEncoding.EncodeToString(serverPub),
		"snonce":     base64.RawURLEncoding.EncodeToString(snonce),
		"sig":        base64.RawURLEncoding.EncodeToString(sig),
	})
}

// pairStoreKey stashes the ephemeral private key for id, sweeping entries
// whose deadline passed (rows that expired/reached terminal state while serve
// kept running — restart clears the whole map by construction).
func (r *ServeRunner) pairStoreKey(id []byte, priv []byte, deadline int64) {
	var key [32]byte
	copy(key[:], id)
	now := time.Now().Unix()
	r.pairMu.Lock()
	defer r.pairMu.Unlock()
	for k, e := range r.pairKeys {
		if e.deadline <= now {
			delete(r.pairKeys, k)
		}
	}
	r.pairKeys[key] = pairKeyEntry{priv: priv, deadline: deadline}
}

// pairTakeKey fetches (a copy of) the private key for id; present=false when
// absent or past deadline.
func (r *ServeRunner) pairTakeKey(id []byte) (priv []byte, present bool) {
	var key [32]byte
	copy(key[:], id)
	now := time.Now().Unix()
	r.pairMu.Lock()
	defer r.pairMu.Unlock()
	e, ok := r.pairKeys[key]
	if !ok || e.deadline <= now {
		return nil, false
	}
	out := make([]byte, len(e.priv))
	copy(out, e.priv)
	return out, true
}

// pairDropKey removes the entry for id (row reached a terminal state).
func (r *ServeRunner) pairDropKey(id []byte) {
	var key [32]byte
	copy(key[:], id)
	r.pairMu.Lock()
	delete(r.pairKeys, key)
	r.pairMu.Unlock()
}

// ---- poll ----

type pairIDRequest struct {
	ID string `json:"id"`
}

// handlePairPoll reports the row's approval state to the polling client:
// pending → 202, approved → 200. Everything else (expired / rejected /
// delivered / unknown) is 410 — the row is not actionable for pairing, and
// the live-queue lookup (store.ListPendingPairing, which lazily expires
// out-of-window rows) cannot and need not distinguish terminal flavors.
func (r *ServeRunner) handlePairPoll(w http.ResponseWriter, req *http.Request) {
	var in pairIDRequest
	if !pairDecode(w, req, &in) {
		return
	}
	id, err := hex.DecodeString(in.ID)
	if err != nil || len(id) != 32 {
		http.Error(w, "id must be 32 bytes hex", http.StatusBadRequest)
		return
	}
	row, err := r.pairFindRow(id)
	if err != nil {
		// A store fault is NOT an approval verdict — 500, never a fake 410
		// that would send the client re-enrolling over a transient fault.
		fmt.Fprintf(os.Stderr, "ssh-manager serve: pairing queue read failed: %v\n", err)
		http.Error(w, "pairing queue unavailable", http.StatusInternalServerError)
		return
	}
	if row == nil {
		http.Error(w, "pairing not actionable", http.StatusGone)
		return
	}
	if row.State == "approved" {
		writeJSON(w, http.StatusOK, map[string]string{"t": "approved"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"t": "pending"})
}

// pairFindRow locates id in the live queue (pending with enroll window open,
// approved with finish window open). Also lazily expires stale rows — the
// store's own hygiene pass. row=nil, err=nil = id not live.
func (r *ServeRunner) pairFindRow(id []byte) (*store.PendingPairing, error) {
	rows, err := r.st.ListPendingPairing()
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if string(rows[i].ID) == string(id) {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// ---- finish ----

type pairFinishRequest struct {
	ID  string `json:"id"`
	Ack string `json:"ack"`
}

// handlePairFinish adjudicates the ack and — inside ONE store transaction —
// mints the credentials and seals them under K_creds (spec §3.3-6).
//
// Load-bearing single-connection discipline: FinishPairing's ackOK/mint
// callbacks run on the store's ONLY pooled connection. Neither callback may
// touch s.db again (any public Store call would self-deadlock forever), so
// EVERYTHING they need — the derived keys, the row's mint inputs, the cert
// pin, max_offline — is computed BEFORE FinishPairing and captured in the
// closures. mint calls only store.MintPairingCredentials(tx, …), which runs
// on the passed tx.
func (r *ServeRunner) handlePairFinish(w http.ResponseWriter, req *http.Request) {
	var in pairFinishRequest
	if !pairDecode(w, req, &in) {
		return
	}
	id, err := hex.DecodeString(in.ID)
	if err != nil || len(id) != 32 {
		http.Error(w, "id must be 32 bytes hex", http.StatusBadRequest)
		return
	}
	ack, err := hex.DecodeString(in.Ack)
	if err != nil || len(ack) != 32 {
		http.Error(w, "ack must be 32 bytes hex", http.StatusBadRequest)
		return
	}

	// Row + key state read OUTSIDE the transaction (plain reads, no lock
	// held across Begin — the store's in-transaction time predicate remains
	// the real gate).
	row, err := r.pairFindRow(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager serve: pairing queue read failed: %v\n", err)
		http.Error(w, "pairing queue unavailable", http.StatusInternalServerError)
		return
	}
	if row != nil && row.State == "pending" {
		http.Error(w, "pairing not approved yet", http.StatusConflict)
		return
	}
	kAck, kCreds, haveKeys := r.pairDeriveKeys(id, row)
	if row != nil && row.State == "approved" && !haveKeys {
		// Approval in-window but no key state: serve restarted (in-memory
		// keys gone) — the pairing is unrecoverable by protocol design. 410
		// (honest "gone"), never a misleading ack 403.
		http.Error(w, "pairing key state unavailable (serve restarted?)", http.StatusGone)
		return
	}
	ackOK := func() bool {
		return haveKeys && hmac.Equal(pairing.FinishAck(kAck, id), ack)
	}

	// Everything the mint closure needs, resolved on the handler's own
	// connection BEFORE FinishPairing (never inside the callbacks).
	maxOffline := pairDefaultMaxOffline
	if v, ok, err := r.st.GetSetting(settingPairMaxOffline); err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager serve: %s read failed: %v (defaulting %s)\n", settingPairMaxOffline, err, pairDefaultMaxOffline)
	} else if ok && strings.TrimSpace(v) != "" {
		maxOffline = strings.TrimSpace(v)
	}
	pin := r.pairSPKI
	name := ""
	profileID := ""
	replaceInactive := false
	if row != nil {
		name, profileID, replaceInactive = row.Name, row.Profile, row.ReplaceInactive
	}
	profileDisplay := profileID
	if profileID != "" {
		if p, err := r.st.GetProfile(profileID); err == nil && p != nil && p.Name != "" {
			profileDisplay = p.Name // display value for the client's「已授权 profile: X」
		}
	}
	mint := func(tx *sql.Tx) ([]byte, error) {
		deviceCode, projectToken, err := r.st.MintPairingCredentials(tx, name, profileID, replaceInactive)
		if err != nil {
			return nil, err
		}
		envelope, err := json.Marshal(pairCredsEnvelope{
			SPKI:         pin,
			Profile:      profileDisplay,
			DeviceCode:   deviceCode,
			ProjectToken: projectToken,
			MaxOffline:   maxOffline,
		})
		if err != nil {
			return nil, err
		}
		return pairing.SealCreds(kCreds, envelope)
	}

	sealed, err := r.st.FinishPairing(id, ackOK, mint)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrPairingWindow):
			r.pairDropKey(id) // row terminal or unrecoverable — key is dead weight
			http.Error(w, "pairing window expired or row not actionable", http.StatusGone)
		case errors.Is(err, store.ErrPairingReplayLimit):
			r.pairDropKey(id) // row now expired by the replay guard
			http.Error(w, "pairing replay limit exceeded", http.StatusGone)
		case errors.Is(err, store.ErrPairingNameActive):
			// The name came alive between enroll and finish — the in-transaction
			// recheck refused to revoke an in-use code. Same class as the enroll
			// 419; row stays approved until its window lapses.
			http.Error(w, "device name in use", 419)
		default:
			if !ackOK() {
				// ack mismatch: FinishPairing aborts with a plain error before
				// minting; the row stays approved so the client may retry with
				// the correct ack until the window closes.
				http.Error(w, "ack mismatch", http.StatusForbidden)
				return
			}
			fmt.Fprintf(os.Stderr, "ssh-manager serve: pairing finish failed: %v\n", err)
			http.Error(w, "finish failed", http.StatusInternalServerError)
		}
		return
	}
	// Delivered (fresh or replay): terminal state — drop the ephemeral key.
	r.pairDropKey(id)
	writeJSON(w, http.StatusOK, map[string]string{
		"sealed": base64.RawURLEncoding.EncodeToString(sealed),
	})
}

// pairDeriveKeys computes (kAck, kCreds) from the in-memory private key and
// the live row's public values. haveKeys=false when either side is missing
// (unknown row / delivered replay / expired entry) — the caller decides
// whether that is fatal (fresh approved row) or irrelevant (replay, where
// FinishPairing never invokes ackOK).
func (r *ServeRunner) pairDeriveKeys(id []byte, row *store.PendingPairing) (kAck, kCreds [32]byte, haveKeys bool) {
	if row == nil {
		return kAck, kCreds, false
	}
	privBytes, present := r.pairTakeKey(id)
	if !present {
		return kAck, kCreds, false
	}
	priv, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		return kAck, kCreds, false
	}
	clientPub, err := ecdh.X25519().NewPublicKey(row.ClientPub)
	if err != nil {
		return kAck, kCreds, false
	}
	ikm, err := priv.ECDH(clientPub)
	if err != nil {
		return kAck, kCreds, false
	}
	tr := buildPairTranscript(id, []byte(row.Name), []byte(row.TargetURL),
		row.ClientPub, row.Cnonce, row.ServerPub, row.Snonce)
	kAck, kCreds = pairing.DeriveKeys(ikm, tr)
	return kAck, kCreds, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ForeignTarget reports whether targetURL is NOT one of this host's own
// addresses — the mechanical address check the approval surface renders as a
// hard ⚠ (spec §3.3-3): a pairing whose declared target is not this machine
// smells of a relay/fake-discovery/wrong-network setup and requires explicit
// OVERRIDE to approve. host[:port] must be in LocalNonLoopbackIPs() ∪
// {os.Hostname()} (both bare and :port forms). Parse failure or empty host →
// foreign (fail closed). Loopback addresses are deliberately NOT in the
// local set (LocalNonLoopbackIPs excludes them) — a "pair with myself on
// 127.0.0.1" target is exactly the shape the check should flag.
func ForeignTarget(targetURL string) bool {
	u, err := url.Parse(targetURL)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "" {
		return true
	}
	// IP literal? Compare numerically (normalizes IPv6 spellings).
	if ip := net.ParseIP(host); ip != nil {
		for _, local := range LocalNonLoopbackIPs() {
			if local.Equal(ip) {
				return false
			}
		}
		return true
	}
	// Name form: hostname match (case-insensitive).
	if h, err := os.Hostname(); err == nil && h != "" && strings.EqualFold(host, h) {
		return false
	}
	return true
}
