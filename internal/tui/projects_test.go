package tui

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestProjectTokenFlow(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p")
	id, tok, err := st.AddProject("proj", pid)
	if err != nil || tok == "" || id == "" {
		t.Fatalf("(%q,%q,%v)", id, tok, err)
	}
	newTok, err := st.RotateProject(id)
	if err != nil || newTok == tok {
		t.Fatalf("rotate: %q %v", newTok, err)
	}
	if err := st.SetProjectStatus(id, models.ProjectDisabled); err != nil {
		t.Fatal(err)
	}
}

func TestSecretView_RendersOnceNotice(t *testing.T) {
	sv := &secretView{title: "项目 token", body: "TOK-xyz"}
	v := sv.View().Content // bubbletea v2: View() returns tea.View; content is .Content
	if !strings.Contains(v, "TOK-xyz") || !strings.Contains(v, "仅此一次") {
		t.Fatalf("view: %s", v)
	}
}
