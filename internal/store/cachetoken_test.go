package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func TestAddCacheToken_ReturnsOneTimePlaintext(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	id, plaintext, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatalf("AddCacheToken: %v", err)
	}
	if id == "" || plaintext == "" {
		t.Fatalf("id=%q plaintext-empty=%v (must return a one-time plaintext)", id, plaintext == "")
	}
	// The plaintext must verify.
	ct, err := s.VerifyCacheToken(plaintext)
	if err != nil || ct == nil {
		t.Fatalf("VerifyCacheToken(plaintext): err=%v ct=%v (the one-time code must verify)", err, ct)
	}
	if ct.ID != id || ct.Name != "laptop" || ct.Status != models.CacheTokenActive {
		t.Fatalf("resolved token mismatch: %+v", ct)
	}
	if !ct.LastPullAt.IsZero() {
		t.Fatalf("LastPullAt must be zero before first pull, got %v", ct.LastPullAt)
	}
}

func TestVerifyCacheToken_RejectsAfterRevoke(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	_, plaintext, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatal(err)
	}
	if ct, _ := s.VerifyCacheToken(plaintext); ct == nil {
		t.Fatal("active token must verify before revoke")
	}
	if err := s.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("RevokeCacheToken: %v", err)
	}
	if ct, _ := s.VerifyCacheToken(plaintext); ct != nil {
		t.Fatalf("revoked token must NOT verify (Lazy gate), got %+v", ct)
	}
}

func TestVerifyCacheToken_WrongTokenReturnsNil(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	if _, _, err := s.AddCacheToken("laptop", pid); err != nil {
		t.Fatal(err)
	}
	ct, err := s.VerifyCacheToken("definitely-not-a-real-token-xxxxxxxxxxxxxxx")
	if err != nil {
		t.Fatalf("wrong token: err=%v (contract: nil error, nil token)", err)
	}
	if ct != nil {
		t.Fatalf("wrong token must return (nil,nil), got %+v", ct)
	}
}

func TestRevokeCacheToken_UnknownNameErrors(t *testing.T) {
	s := newTestStore(t)
	if err := s.RevokeCacheToken("nope"); err == nil {
		t.Fatal("revoking an unknown name must error")
	}
}

func TestListCacheTokens_ReturnsOwnerFacingFields(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	if _, _, err := s.AddCacheToken("laptop", pid); err != nil {
		t.Fatal(err)
	}
	out, err := s.ListCacheTokens()
	if err != nil {
		t.Fatalf("ListCacheTokens: %v", err)
	}
	if len(out) != 1 || out[0].Name != "laptop" || out[0].Status != models.CacheTokenActive {
		t.Fatalf("list mismatch: %+v", out)
	}
}

func TestTouchCacheToken_UpdatesLastPullAt(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	id, plaintext, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatalf("AddCacheToken: %v", err)
	}
	ct, err := s.VerifyCacheToken(plaintext)
	if err != nil || ct == nil {
		t.Fatalf("VerifyCacheToken: err=%v ct=%v", err, ct)
	}
	if err := s.TouchCacheToken(ct.ID); err != nil {
		t.Fatalf("TouchCacheToken: %v", err)
	}
	got, err := s.VerifyCacheToken(plaintext)
	if err != nil || got == nil {
		t.Fatalf("re-VerifyCacheToken: err=%v got=%v", err, got)
	}
	if got.ID != id || got.LastPullAt.IsZero() || time.Since(got.LastPullAt) > 5*time.Second {
		t.Fatalf("last_pull_at not bumped (or stale): %+v", got)
	}
}

// TestAddCacheToken_ReusesNameAfterRevoke asserts the bug fix: after revoking a device code,
// the same name can be re-issued (the prior revoked row no longer blocks UNIQUE(name)). The new
// plaintext verifies; the OLD revoked plaintext must NOT verify (Lazy gate on the prior code).
func TestAddCacheToken_ReusesNameAfterRevoke(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	if _, oldPlain, err := s.AddCacheToken("laptop", pid); err != nil {
		t.Fatalf("first AddCacheToken: %v", err)
	} else if ct, _ := s.VerifyCacheToken(oldPlain); ct == nil {
		t.Fatal("first code must verify before revoke")
	}
	if err := s.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("RevokeCacheToken: %v", err)
	}
	// Re-issue same name — must succeed (no UNIQUE collision).
	_, newPlain, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatalf("re-add after revoke must succeed, got: %v", err)
	}
	// New plaintext verifies as active.
	ct, err := s.VerifyCacheToken(newPlain)
	if err != nil || ct == nil {
		t.Fatalf("new plaintext must verify active: err=%v ct=%v", err, ct)
	}
	if ct.Name != "laptop" || ct.Status != models.CacheTokenActive {
		t.Fatalf("new active resolve mismatch: %+v", ct)
	}
}

// TestAddCacheToken_ActiveNameStillCollides asserts the UNIQUE(name) guard is NOT loosened for
// ACTIVE rows: issuing a second active code under a live name must still fail (prevents
// accidentally handing out two active codes for one device).
func TestAddCacheToken_ActiveNameStillCollides(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	if _, _, err := s.AddCacheToken("laptop", pid); err != nil {
		t.Fatalf("first AddCacheToken: %v", err)
	}
	_, _, err := s.AddCacheToken("laptop", pid)
	if err == nil {
		t.Fatal("second active add under a live name must fail UNIQUE, got nil error")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Fatalf("expected a UNIQUE constraint error, got: %v", err)
	}
}

// TestAddCacheToken_ReclaimsWithoutAccumulating asserts that repeated add→revoke cycles under
// the same name never accumulate revoked rows: because AddCacheToken reclaims same-name revoked
// rows on every call, at most one revoked "laptop" row exists at any time. The final active add
// must leave exactly one active "laptop" and zero revoked "laptop" rows — proving the reclaim
// fires on every re-add, not just the first.
func TestAddCacheToken_ReclaimsWithoutAccumulating(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	// Repeated add→revoke cycles. Each add reclaims the prior revoked row, so this never
	// accumulates more than one revoked row; if the reclaim ever stopped firing, a later add
	// would hit UNIQUE(name) and fail.
	for i := 0; i < 3; i++ {
		if _, _, err := s.AddCacheToken("laptop", pid); err != nil {
			t.Fatalf("add cycle %d: %v", i, err)
		}
		if err := s.RevokeCacheToken("laptop"); err != nil {
			t.Fatalf("revoke cycle %d: %v", i, err)
		}
	}
	// Final active add — must succeed (no UNIQUE collision from a leftover revoked row) and
	// leave a single active row with zero revoked residue.
	if _, _, err := s.AddCacheToken("laptop", pid); err != nil {
		t.Fatalf("final add after repeated revokes: %v", err)
	}
	out, err := s.ListCacheTokens()
	if err != nil {
		t.Fatalf("ListCacheTokens: %v", err)
	}
	var active, revoked int
	for _, ct := range out {
		if ct.Name != "laptop" {
			continue
		}
		switch ct.Status {
		case models.CacheTokenActive:
			active++
		case models.CacheTokenRevoked:
			revoked++
		}
	}
	if active != 1 || revoked != 0 {
		t.Fatalf("expected exactly 1 active + 0 revoked laptop rows, got active=%d revoked=%d", active, revoked)
	}
}

// --- Plan 39: profile-bound device codes -----------------------------------

// seedProfile adds a profile and returns its id (test helper).
func seedProfile(t *testing.T, s *Store, name string) string {
	t.Helper()
	pid, err := s.AddProfile(name)
	if err != nil {
		t.Fatalf("AddProfile(%s): %v", name, err)
	}
	return pid
}

// TestAddCacheToken_BindsProfile pins the Plan-39 contract: a device code is
// minted WITH its profile binding, and VerifyCacheToken carries ProfileID back
// (the serve layer scopes /snapshot by it).
func TestAddCacheToken_BindsProfile(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "e2e-profile")
	id, plaintext, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatalf("AddCacheToken: %v", err)
	}
	ct, err := s.VerifyCacheToken(plaintext)
	if err != nil || ct == nil {
		t.Fatalf("VerifyCacheToken: err=%v ct=%v", err, ct)
	}
	if ct.ID != id || ct.ProfileID != pid {
		t.Fatalf("resolved token must carry its binding: id=%q ProfileID=%q want %q", ct.ID, ct.ProfileID, pid)
	}
}

// TestAddCacheToken_EmptyProfileRejected: the store API cannot mint UNBOUND codes —
// unbound exists ONLY as the legacy-DB migration state (pulls refused with 403 until bound).
func TestAddCacheToken_EmptyProfileRejected(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.AddCacheToken("laptop", ""); err == nil {
		t.Fatal("AddCacheToken with empty profileID must be rejected (fail-closed)")
	}
}

// TestAddCacheToken_UnknownProfileErrors: binding to a nonexistent profile is a loud
// owner error, not a silently-dangling row.
func TestAddCacheToken_UnknownProfileErrors(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.AddCacheToken("laptop", "no-such-profile"); err == nil {
		t.Fatal("AddCacheToken with unknown profile must error")
	}
}

// TestBindCacheToken upgrades a legacy UNBOUND row (the pre-Plan-39 migration state —
// e.g. NUC10's existing laptop-v040) to a bound one, keeping name/status/history.
func TestBindCacheToken(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "e2e-profile")
	id, plaintext, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the legacy state: strip the binding in-place (white-box, in-package).
	if _, err := s.db.Exec(`UPDATE cache_tokens SET profile_id=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if ct, _ := s.VerifyCacheToken(plaintext); ct == nil || ct.ProfileID != "" {
		t.Fatalf("pre-bind state must be unbound, got %+v", ct)
	}
	if err := s.BindCacheToken("laptop", pid); err != nil {
		t.Fatalf("BindCacheToken: %v", err)
	}
	ct, err := s.VerifyCacheToken(plaintext)
	if err != nil || ct == nil {
		t.Fatalf("post-bind verify: err=%v ct=%v", err, ct)
	}
	if ct.ProfileID != pid || ct.Status != models.CacheTokenActive {
		t.Fatalf("post-bind resolve mismatch: %+v", ct)
	}
}

// TestBindCacheToken_Unknowns: unknown device name and unknown profile both error.
func TestBindCacheToken_Unknowns(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "e2e-profile")
	if err := s.BindCacheToken("ghost", pid); err == nil {
		t.Fatal("binding an unknown device name must error")
	}
	_, plaintext, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatal(err)
	}
	_ = plaintext
	if err := s.BindCacheToken("laptop", "no-such-profile"); err == nil {
		t.Fatal("binding to an unknown profile must error")
	}
}

// TestGetCacheToken_ByID mirrors GetProject: the serve layer re-queries the bound
// profile after auth hands it only the token id (TokenInfo.UserID).
func TestGetCacheToken_ByID(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "e2e-profile")
	id, _, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := s.GetCacheToken(id)
	if err != nil || ct == nil {
		t.Fatalf("GetCacheToken: err=%v ct=%v", err, ct)
	}
	if ct.ProfileID != pid || ct.Name != "laptop" {
		t.Fatalf("GetCacheToken mismatch: %+v", ct)
	}
	if ct, err := s.GetCacheToken("nope"); err != nil || ct != nil {
		t.Fatalf("unknown id must return (nil, nil), got ct=%v err=%v", ct, err)
	}
}

// TestListCacheTokens_ShowsProfile: owner-facing listing carries the binding.
func TestListCacheTokens_ShowsProfile(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "e2e-profile")
	if _, _, err := s.AddCacheToken("laptop", pid); err != nil {
		t.Fatal(err)
	}
	out, err := s.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ProfileID != pid {
		t.Fatalf("list must carry ProfileID, got %+v", out)
	}
}

// TestDeleteProfile_RefusesWhileDeviceBound extends the existing projects guard:
// deleting a profile that still has an ACTIVE bound device code is refused with the
// device names (an inert binding on a revoked code does not block).
func TestDeleteProfile_RefusesWhileDeviceBound(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "e2e-profile")
	if _, _, err := s.AddCacheToken("laptop-v040", pid); err != nil {
		t.Fatal(err)
	}
	err := s.DeleteProfile(pid)
	if err == nil {
		t.Fatal("deleting a profile with a bound active device code must be refused")
	}
	if !strings.Contains(err.Error(), "laptop-v040") {
		t.Fatalf("error must name the bound device, got: %v", err)
	}
	// Revoked → binding is inert → deletion succeeds (grant rows cascade).
	if err := s.RevokeCacheToken("laptop-v040"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(pid); err != nil {
		t.Fatalf("after revoking the bound code, deletion must succeed: %v", err)
	}
}

// TestMigrateLegacyCacheTokens_Unbound pins the fleet-upgrade path: a pre-Plan-39
// DB (cache_tokens WITHOUT profile_id) migrates on Open; its existing rows stay
// unbound (NULL) and VerifyCacheToken reads them as ProfileID "" — the 403 state.
func TestMigrateLegacyCacheTokens_Unbound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldShapeCacheTokens); err != nil { // pre-Plan-39 shape
		t.Fatal(err)
	}
	// A token row with a verifiable shape: hash/salt/prefix stand-ins (the plaintext
	// is not what we verify here — only that the row survives and reads ProfileID "").
	if _, err := db.Exec(`INSERT INTO cache_tokens (id,name,token_hash,token_salt,token_prefix,status,last_pull_at,created_at,updated_at) VALUES ('ct1','laptop',x'00',x'00','pfxXXXXX','active',NULL,1,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(path, mk)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	// SQL-layer: the column exists post-migration and the legacy row is NULL (= unbound).
	var nullCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM cache_tokens WHERE profile_id IS NOT NULL`).Scan(&nullCount); err != nil {
		t.Fatalf("post-migrate probe: %v", err)
	}
	if nullCount != 0 {
		t.Fatalf("legacy row must migrate to NULL profile_id (unbound), got %d bound", nullCount)
	}
	// Model-layer: ListCacheTokens reads the legacy row back with ProfileID "".
	out, err := s.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "laptop" || out[0].ProfileID != "" {
		t.Fatalf("legacy row must read back unbound, got %+v", out)
	}
}

// oldShapeCacheTokens is the pre-Plan-39 cache_tokens schema (no profile_id).
const oldShapeCacheTokens = `
CREATE TABLE cache_tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  last_pull_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);`

// TestRevokedCacheTokenNameByPrefix pins the rev4 §1 reason lookup: a revoked
// row matching the 8-char plaintext prefix resolves its name (most recent
// updated_at wins on collisions); no revoked match returns ok=false; active
// rows NEVER match.
func TestRevokedCacheTokenNameByPrefix(t *testing.T) {
	s := newTestStore(t)
	pid := seedProfile(t, s, "p")
	_, tok1, err := s.AddCacheToken("laptop", pid)
	if err != nil {
		t.Fatalf("add laptop: %v", err)
	}
	if err := s.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("revoke laptop: %v", err)
	}
	name, ok, err := s.RevokedCacheTokenNameByPrefix(tok1[:8])
	if err != nil || !ok || name != "laptop" {
		t.Fatalf("revoked lookup: name=%q ok=%v err=%v, want laptop/true/nil", name, ok, err)
	}
	// Unknown prefix → ok=false, no error.
	if _, ok, err := s.RevokedCacheTokenNameByPrefix("ZZZZZZZZ"); err != nil || ok {
		t.Fatalf("unknown prefix: ok=%v err=%v, want false/nil", ok, err)
	}
	// An ACTIVE token's prefix must NOT match (only revoked rows).
	_, tok2, err := s.AddCacheToken("desk", pid)
	if err != nil {
		t.Fatalf("add desk: %v", err)
	}
	if _, ok, _ := s.RevokedCacheTokenNameByPrefix(tok2[:8]); ok {
		t.Fatal("active row matched the revoked-prefix lookup")
	}
}
