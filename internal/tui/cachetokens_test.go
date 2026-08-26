package tui

import (
	"strings"
	"testing"
)

func TestCacheTokenIssueFlow(t *testing.T) {
	st := newStore(t)
	pid, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	id, code, err := st.AddCacheToken("laptop", pid)
	if err != nil || code == "" || id == "" {
		t.Fatalf("(%q,%q,%v)", id, code, err)
	}
	if err := st.RevokeCacheToken("laptop"); err != nil {
		t.Fatal(err)
	}
}

// TestDeviceCodeBodyTrimsServeURL: the issue form's serve-addr hint field only
// validates nonEmpty (no URL validation), so whitespace can ride in; the
// composition point must TrimSpace so the ready-to-paste pull command stays
// copy-pasteable.
func TestDeviceCodeBodyTrimsServeURL(t *testing.T) {
	fp := "sha256:" + strings.Repeat("a", 64)
	v := deviceCodeBody("  https://192.0.2.5:7878\t", "CODE123", fp)
	if !strings.Contains(v, "--url https://192.0.2.5:7878 --token") {
		t.Fatalf("serve URL must be trimmed at composition:\n%s", v)
	}
}

func TestDeviceCodeBody(t *testing.T) {
	fp := "sha256:" + strings.Repeat("a", 64)
	v := deviceCodeBody("https://192.0.2.5:7878", "CODE123", fp)
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
