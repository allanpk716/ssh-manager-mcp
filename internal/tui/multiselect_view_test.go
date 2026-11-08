package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/importer"
	"ssh-manager-mcp/internal/models"
)

// sizeForm initializes a form with the terminal size used by the tutorial
// screenshots and returns the updated form.
func sizeForm(t *testing.T, form *huh.Form) *huh.Form {
	t.Helper()
	_ = form.Init()
	model, _ := form.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	updated, ok := model.(*huh.Form)
	if !ok {
		t.Fatalf("form.Update returned %T, want *huh.Form", model)
	}
	return updated
}

func sizedFormView(t *testing.T, form *huh.Form) string {
	t.Helper()
	return sizeForm(t, form).View()
}

func TestWizardProfileGrantFormShowsServerOption(t *testing.T) {
	name := "demo-profile"
	chosen := []string{}
	form := wizProfileGrantForm(
		&name,
		[]*models.Server{{ID: "demo-gpu-id", Name: "demo-gpu"}},
		&chosen,
	)
	if view := sizedFormView(t, form); !strings.Contains(view, "demo-gpu") {
		t.Fatalf("profile grant form must show the server option demo-gpu:\n%q", view)
	}
}

func TestGrantFormShowsServerOption(t *testing.T) {
	chosen := []string{}
	form := newGrantForm(
		[]*models.Server{{ID: "demo-gpu-id", Name: "demo-gpu"}},
		&chosen,
	)
	if view := sizedFormView(t, form); !strings.Contains(view, "demo-gpu") {
		t.Fatalf("grant form must show the server option demo-gpu:\n%q", view)
	}
}

func TestImportPickFormShowsServerOption(t *testing.T) {
	flow := &importFlow{
		cands: []importer.Candidate{{
			Name: "demo-gpu", Host: "127.0.0.1", Port: 22, User: "deploy",
		}},
		pick: []string{},
	}
	if view := sizedFormView(t, flow.pickForm()); !strings.Contains(view, "demo-gpu") {
		t.Fatalf("server import form must show the server option demo-gpu:\n%q", view)
	}
}

func TestPolicyRuleFormShowsShellOption(t *testing.T) {
	draft := &policyRuleDraft{ID: "demo-rule"}
	form := sizeForm(t, newPolicyRuleForm(draft))
	overlay := newFormOverlay("新增自定义规则", form, func() tea.Cmd { return nil })
	model, cmd := overlay.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model, cmd = drain(t, model, cmd).(*formOverlay).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drain(t, model, cmd)
	if view := overlay.View().Content; !strings.Contains(view, "PowerShell（Windows）") {
		t.Fatalf("policy rule form must show the PowerShell shell option:\n%q", view)
	}
}
