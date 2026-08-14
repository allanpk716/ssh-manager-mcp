package tui

import (
	"strings"
	"testing"
)

func TestCacheTokenIssueFlow(t *testing.T) {
	st := newStore(t)
	id, code, err := st.AddCacheToken("laptop")
	if err != nil || code == "" || id == "" {
		t.Fatalf("(%q,%q,%v)", id, code, err)
	}
	if err := st.RevokeCacheToken("laptop"); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceCodeSecretView_Body(t *testing.T) {
	fp := "sha256:" + strings.Repeat("a", 64)
	sv := deviceCodeView("https://192.0.2.5:7878", "CODE123", fp)
	v := sv.View().Content // bubbletea v2: View() returns tea.View; content is .Content
	for _, want := range []string{
		"CODE123",
		fp,
		"cache pull --url https://192.0.2.5:7878",
		"'CODE123:" + fp + "'", // embedded-pin form (spec §3.3 形态 A)
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q:\n%s", want, v)
		}
	}
}
