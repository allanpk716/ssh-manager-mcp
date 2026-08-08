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
