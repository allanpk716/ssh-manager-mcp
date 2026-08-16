package cli

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestOwnerSSHNoCommandErrors pins the arg contract: the owner ssh path is a
// SINGLE non-interactive command — host-only, empty-string, and whitespace-only
// command args all fail fast BEFORE any connection or audit row.
func TestOwnerSSHNoCommandErrors(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	cases := []struct {
		name string
		args []string
	}{
		{"host only", []string{"ssh", "t"}},
		{"empty string cmd", []string{"ssh", "t", ""}},
		{"whitespace cmd", []string{"ssh", "t", "   "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := NewRootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(c.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("args %v: expected error, got nil", c.args)
			}
			if !strings.Contains(err.Error(), "no command given") {
				t.Fatalf("args %v: error %q missing 'no command given'", c.args, err)
			}
			// T2 review fix: ensure store is NOT created for rejected args
			if _, err := os.Stat(filepath.Join(dir, "test.db")); !os.IsNotExist(err) {
				t.Fatalf("store must not be created for rejected args")
			}
		})
	}
}

// TestOwnerSSHPropagatesRemoteExitCode pins A4: a remote command exiting
// non-zero must surface as a cobra error (CLI exits non-zero) — output is
// still printed first. Today the exit code is swallowed (return nil).
func TestOwnerSSHPropagatesRemoteExitCode(t *testing.T) {
	addr, hostKey, srvCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "partial output\n", "boom\n", 3
		},
	})
	defer srvCleanup()
	host := addr[:bytesIndex(addr, ':')]

	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	st, err := store.Open(filepath.Join(dir, "test.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, _ := st.AddServer(&models.Server{
		Name: "t", Host: host, Port: portOfAddr(addr), User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	_ = st.SaveHostKey(host, portOfAddr(addr), hostKey.Marshal())
	st.Close()
	_ = srvID

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"ssh", "t", "false"})
	err = root.Execute()
	if err == nil {
		t.Fatal("expected error for remote exit code 3, got nil")
	}
	if !strings.Contains(err.Error(), "exited with code 3") {
		t.Fatalf("error %q missing remote exit code", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("partial output")) {
		t.Fatal("remote stdout must still be printed before the error")
	}
}

// TestOwnerSSHConnectDeadlineBounded pins A7: connect shares the command
// deadline — an unreachable host returns within the (shortened) deadline,
// not the OS TCP timeout. ssh.Dial cannot be interrupted; Connect abandons
// the in-flight dial on ctx expiry (client.go contract), so elapsed ≈ deadline.
func TestOwnerSSHConnectDeadlineBounded(t *testing.T) {
	orig := ownerSSHDeadline
	ownerSSHDeadline = 2 * time.Second
	t.Cleanup(func() { ownerSSHDeadline = orig })

	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	st, err := store.Open(filepath.Join(dir, "test.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	_, _ = st.AddServer(&models.Server{
		// RFC5737 TEST-NET-3 — non-routable by definition, safe in any CI lane.
		Name: "t", Host: "203.0.113.1", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	st.Close()

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"ssh", "t", "true"})
	start := time.Now()
	err = root.Execute()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected connect error for unreachable host, got nil")
	}
	// Deadline is 2s; allow generous slack for CI jitter but far below the
	// multi-minute OS TCP timeout this test exists to forbid.
	if elapsed > 15*time.Second {
		t.Fatalf("unreachable-host connect took %v; deadline not shared/bounded", elapsed)
	}
	t.Logf("elapsed=%v err=%v", elapsed, err)
}
