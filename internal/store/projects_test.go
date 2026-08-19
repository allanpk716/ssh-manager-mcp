package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
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

// TestVerifyTokenRejectsDisabledAndRevoked: only status='active' admits — the Lazy gate.
// A disabled/revoked token is rejected EVEN with the correct secret.
func TestVerifyTokenRejectsDisabledAndRevoked(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	projID, token, _ := s.AddProject("p", pid)

	if got, _ := s.VerifyToken(token); got == nil {
		t.Fatal("active token must verify")
	}
	if err := s.SetProjectStatus(projID, models.ProjectDisabled); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.VerifyToken(token); got != nil {
		t.Fatal("disabled token must NOT verify")
	}
	if err := s.SetProjectStatus(projID, models.ProjectActive); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.VerifyToken(token); got == nil {
		t.Fatal("re-enabled token must verify")
	}
	if err := s.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.VerifyToken(token); got != nil {
		t.Fatal("revoked token must NOT verify")
	}
}

// TestRotateProject: old token dies, new token lives, project id + profile preserved (in-place).
func TestRotateProject(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	projID, oldToken, _ := s.AddProject("p", pid)

	newToken, err := s.RotateProject(projID)
	if err != nil {
		t.Fatal(err)
	}
	if newToken == "" || newToken == oldToken {
		t.Fatal("rotate must return a new, different token")
	}
	if got, _ := s.VerifyToken(oldToken); got != nil {
		t.Fatal("old token must NOT verify after rotate")
	}
	got, err := s.VerifyToken(newToken)
	if err != nil || got == nil {
		t.Fatalf("new token must verify: got %v err %v", got, err)
	}
	if got.ID != projID || got.ProfileID != pid {
		t.Fatalf("rotate must preserve id/profile: got id=%s profile=%s", got.ID, got.ProfileID)
	}
	if got.Status != models.ProjectActive {
		t.Fatalf("status = %v, want active (rotate does not suspend)", got.Status)
	}
	if _, err := s.RotateProject("nonexistent"); err == nil {
		t.Fatal("rotate missing id should error")
	}
}

func TestGetProjectByName(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	projID, _, _ := s.AddProject("p", pid)
	got, err := s.GetProjectByName("p")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != projID {
		t.Fatalf("GetProjectByName = %+v", got)
	}
	if got2, _ := s.GetProjectByName("nope"); got2 != nil {
		t.Fatal("missing name should return nil")
	}
}

// v0.8.5: ListProjects must fill the timestamps (were never selected — the
// projects page rendered 0001-01-01).
func TestProjectTimestampsFilled(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	if _, _, err := s.AddProject("agent", pid); err != nil {
		t.Fatal(err)
	}
	ps, err := s.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].CreatedAt.IsZero() || ps[0].UpdatedAt.IsZero() {
		t.Fatalf("ListProjects timestamps must be filled, got %+v", ps)
	}
}
