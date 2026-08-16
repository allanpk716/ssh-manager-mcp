package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// statModTime returns a file's mtime for the idempotency test (second run must
// not recreate → mtime unchanged).
func statModTime(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.ModTime()
}

// openVault opens the vault the wizard just created, reading the master key
// the wizard wrote at the pinned SSHMGR_FILEKEY_PATH.
func openVault(t *testing.T) *store.Store {
	t.Helper()
	mk, err := os.ReadFile(os.Getenv("SSHMGR_FILEKEY_PATH"))
	if err != nil {
		t.Fatalf("read master key: %v", err)
	}
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mk)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// viewString renders an overlay's view to plain text (styles are content-
// preserving, so substring asserts work through lipgloss).
func viewString(ov overlay) string { return ov.View().Content }

// TestWizEnsureVault_Idempotent: first run creates store.db (+ master key);
// second run sees an existing unlocked vault and must NOT touch it.
func TestWizEnsureVault_Idempotent(t *testing.T) {
	vd, _ := withRoleDirs(t)
	if err := wizEnsureVault(); err != nil {
		t.Fatal(err)
	}
	db1 := statModTime(t, filepath.Join(vd, "store.db"))
	if _, err := os.Stat(filepath.Join(vd, "master.key.plain")); err != nil {
		t.Fatalf("master key not written by wizard: %v", err)
	}
	if err := wizEnsureVault(); err != nil {
		t.Fatal(err)
	}
	if statModTime(t, filepath.Join(vd, "store.db")) != db1 {
		t.Fatal("second run must not recreate")
	}
}

// TestWizProfileName_SuffixOnConflict: an existing profile name gets a -2
// suffix (hostname-defaulted names collide on re-runs of the wizard).
func TestWizProfileName_SuffixOnConflict(t *testing.T) {
	withRoleDirs(t)
	if err := wizEnsureVault(); err != nil {
		t.Fatal(err)
	}
	st := openVault(t)
	if _, err := st.AddProfile("nuc10"); err != nil {
		t.Fatal(err)
	}
	if name := dedupeProfileName(st, "nuc10"); name != "nuc10-2" {
		t.Fatalf("want nuc10-2 got %s", name)
	}
	// no conflict → name passes through untouched
	if name := dedupeProfileName(st, "laptop"); name != "laptop" {
		t.Fatalf("want laptop got %s", name)
	}
}

// TestWizTokenScreen_Copy: the one-time token screen must carry all three
// copy elements — the token itself, the 用途 line, and the 「仅此一次…丢失→」
// recovery line.
func TestWizTokenScreen_Copy(t *testing.T) {
	ov := wizTokenScreen("project token", "eyJtok", "贴到 .mcp.json 的 SSHMGR_TOKEN 字段", "主控台 Projects 页 [a] 重发")
	v := viewString(ov)
	for _, want := range []string{"project token", "eyJtok", "贴到", "仅此一次", "丢失", "重发"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in:\n%s", want, v)
		}
	}
}

// TestMcpConfigScreen_Copy: the finish screen shows the real .mcp.json shape
// — plain `mcp` for the standalone role (NOT the client-role --cache mode),
// with the token in the SSHMGR_TOKEN env field, never in argv (Plan 20 B2).
func TestMcpConfigScreen_Copy(t *testing.T) {
	v := viewString(mcpConfigScreen("上方已展示的 project token"))
	for _, want := range []string{
		"mcpServers",
		`"args": ["mcp"],`,
		`"SSHMGR_TOKEN": "上方已展示的 project token"`,
		".mcp.json",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in:\n%s", want, v)
		}
	}
	if strings.Contains(v, "--token") {
		t.Fatalf("token must ride env, not argv:\n%s", v)
	}
}

// TestMcpConfigLinesJSONValid (Plan 21 A2): the emitted snippet must parse as
// JSON with AND without field lines — the command line used to hardcode a
// trailing comma, which an empty fieldLines list turned into invalid JSON.
// The JSON object is lifted out of the rendered lines (first "{" to last "}")
// and unmarshal-checked whole.
func TestMcpConfigLinesJSONValid(t *testing.T) {
	extractJSON := func(lines []string) string {
		start, end := -1, -1
		for i, l := range lines {
			if l == "{" && start < 0 {
				start = i
			}
			if l == "}" {
				end = i
			}
		}
		if start < 0 || end < start {
			t.Fatalf("no JSON object found in %q", lines)
		}
		return strings.Join(lines[start:end+1], "\n")
	}
	for _, fieldLines := range [][]string{
		{`"args": ["mcp"]`, `"env": { "SSHMGR_TOKEN": "tok" }`},
		{}, // empty: no args/env lines — command becomes the last member
		nil,
	} {
		var v any
		if err := json.Unmarshal([]byte(extractJSON(mcpConfigLines(fieldLines, nil))), &v); err != nil {
			t.Fatalf("fieldLines %q: snippet not valid JSON: %v", fieldLines, err)
		}
	}
}

// TestWizFinish_SavesSetupComplete: the finish cmd flips role.json to
// setup_complete:true and returns the handoff sentinel for Run.
func TestWizFinish_SavesSetupComplete(t *testing.T) {
	vd, _ := withRoleDirs(t)
	seedWizardVault(t, vd)
	msg := wizFinish(roles.RoleStandalone)()
	dm, ok := msg.(wizardDoneMsg)
	if !ok || dm.next != "broker" {
		t.Fatalf("want wizardDoneMsg{next:broker}, got %#v", msg)
	}
	b, err := os.ReadFile(filepath.Join(vd, "role.json"))
	if err != nil {
		t.Fatalf("role.json: %v", err)
	}
	if want := `"setup_complete":true`; !strings.Contains(string(b), want) {
		t.Fatalf("role.json must record complete setup: %s", b)
	}
}
