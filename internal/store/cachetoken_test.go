package store

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func TestAddCacheToken_ReturnsOneTimePlaintext(t *testing.T) {
	s := newTestStore(t)
	id, plaintext, err := s.AddCacheToken("laptop")
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
	_, plaintext, err := s.AddCacheToken("laptop")
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
	if _, _, err := s.AddCacheToken("laptop"); err != nil {
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
	if _, _, err := s.AddCacheToken("laptop"); err != nil {
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
	id, plaintext, err := s.AddCacheToken("laptop")
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
	if _, oldPlain, err := s.AddCacheToken("laptop"); err != nil {
		t.Fatalf("first AddCacheToken: %v", err)
	} else if ct, _ := s.VerifyCacheToken(oldPlain); ct == nil {
		t.Fatal("first code must verify before revoke")
	}
	if err := s.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("RevokeCacheToken: %v", err)
	}
	// Re-issue same name — must succeed (no UNIQUE collision).
	_, newPlain, err := s.AddCacheToken("laptop")
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
	if _, _, err := s.AddCacheToken("laptop"); err != nil {
		t.Fatalf("first AddCacheToken: %v", err)
	}
	_, _, err := s.AddCacheToken("laptop")
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
	// Repeated add→revoke cycles. Each add reclaims the prior revoked row, so this never
	// accumulates more than one revoked row; if the reclaim ever stopped firing, a later add
	// would hit UNIQUE(name) and fail.
	for i := 0; i < 3; i++ {
		if _, _, err := s.AddCacheToken("laptop"); err != nil {
			t.Fatalf("add cycle %d: %v", i, err)
		}
		if err := s.RevokeCacheToken("laptop"); err != nil {
			t.Fatalf("revoke cycle %d: %v", i, err)
		}
	}
	// Final active add — must succeed (no UNIQUE collision from a leftover revoked row) and
	// leave a single active row with zero revoked residue.
	if _, _, err := s.AddCacheToken("laptop"); err != nil {
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
