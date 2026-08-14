package tui

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

func TestClientHeader(t *testing.T) {
	h := clientHeader(&clientops.CacheCred{URL: "https://192.0.2.5:7878", Pin: "sha256:" + strings.Repeat("a", 64)}, 3, 2*time.Minute)
	for _, want := range []string{"192.0.2.5", "sha256", "3 服务器", "2m"} {
		if !strings.Contains(h, want) {
			t.Fatalf("header missing %q:\n%s", want, h)
		}
	}
}

func TestClientServerList(t *testing.T) {
	snap := &store.Snapshot{Servers: []store.SnapshotServer{{Name: "gpu", Host: "192.0.2.10", User: "u"}}}
	rows := clientServerRows(snap)
	if len(rows) != 1 || !strings.Contains(rows[0], "gpu") || !strings.Contains(rows[0], "192.0.2.10") {
		t.Fatalf("rows: %v", rows)
	}
}

// TestSyncCmdRefusesEmptyPin pins the TUI's own no-plaintext invariant: with a
// stored cred lacking a pin, sync must fail fast instead of ever attempting a
// plaintext (AllowPlain=false would only fail later, after a network round
// trip) pull.
func TestSyncCmdRefusesEmptyPin(t *testing.T) {
	cred := &clientops.CacheCred{URL: "https://x", Token: "t", Pin: ""}
	msg := syncCmd(cred)()
	done, ok := msg.(syncDoneMsg)
	if !ok {
		t.Fatalf("want syncDoneMsg, got %T", msg)
	}
	if done.err == nil {
		t.Fatal("want non-nil err for empty pin")
	}
	if !strings.Contains(done.err.Error(), "明文") && !strings.Contains(done.err.Error(), "pin") {
		t.Fatalf("err should mention pin/明文: %v", done.err)
	}
}
