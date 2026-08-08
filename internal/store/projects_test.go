package store

import (
	"testing"
)

func TestAddProjectReturnsTokenAndVerifies(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	projID, token, err := s.AddProject("project-A", pid)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || projID == "" {
		t.Fatal("missing id or token")
	}
	proj, err := s.VerifyToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if proj == nil || proj.ID != projID || proj.ProfileID != pid {
		t.Fatalf("verify returned %+v", proj)
	}
}

func TestVerifyTokenRejectsUnknown(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.VerifyToken("not-a-real-token"); err != nil {
		t.Fatalf("unknown token should be nil,nil; got %v", err)
	}
}

func TestVerifyTokenPrefiltersByPrefix(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	_, token, _ := s.AddProject("p1", pid)

	// a token with a different 8-char prefix must not verify (and returns nil,nil quickly)
	other := "AAAAAAAA" + token[8:] // same length, different prefix
	got, err := s.VerifyToken(other)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("token with wrong prefix must not verify")
	}
	// the real token still verifies
	got, err = s.VerifyToken(token)
	if err != nil || got == nil {
		t.Fatalf("real token must verify: got %v err %v", got, err)
	}
}
