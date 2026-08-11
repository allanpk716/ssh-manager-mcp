package store

import (
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

func TestListCacheTokens_OmitsHash(t *testing.T) {
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
	id, plaintext, _ := s.AddCacheToken("laptop")
	ct, _ := s.VerifyCacheToken(plaintext)
	if err := s.TouchCacheToken(ct.ID); err != nil {
		t.Fatalf("TouchCacheToken: %v", err)
	}
	got, _ := s.VerifyCacheToken(plaintext)
	if got.ID != id || got.LastPullAt.IsZero() || time.Since(got.LastPullAt) > 5*time.Second {
		t.Fatalf("last_pull_at not bumped (or stale): %+v", got)
	}
}
