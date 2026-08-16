package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// withServeLogDirs pins every filesystem location a service-path serve start
// (program.run) touches via env — mirror of withClearDirs (same rationale: the
// dev machine REALLY runs ssh-manager, so an unpinned path would read/write
// the operator's live C:\ProgramData vault). SSHMGR_SERVE_LOG is the Plan 22
// T4 seam under test.
func withServeLogDirs(t *testing.T) (vaultDir, logPath string) {
	t.Helper()
	vaultDir = t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(vaultDir, "master.key.plain"))
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(vaultDir, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(vaultDir, "serve-key.pem"))
	t.Setenv("SSHMGR_SERVE_MARKER", filepath.Join(vaultDir, ".serve-cert-initialized"))
	logPath = filepath.Join(vaultDir, "serve.log")
	t.Setenv("SSHMGR_SERVE_LOG", logPath)
	return vaultDir, logPath
}

// seedServeVault mirrors seedClearVault: an openable vault + plaintext master
// key at the pinned paths, so program.run's vault.OpenStore(FileKeyProvider)
// succeeds.
func seedServeVault(t *testing.T, vaultDir string) {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(filepath.Join(vaultDir, "master.key.plain"), mk, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProgramRun_WritesServeLogFile drives the SERVICE path (program.run —
// Start/Stop like kardianos does, NOT the foreground RunE) with every path
// pinned via env seams, and asserts the serve.log file sink receives the
// startup line. This is the T4 production contract: a service-managed serve
// leaves a readable on-disk trail even when the platform's stderr capture
// (Windows EventLog) is hard to inspect.
func TestProgramRun_WritesServeLogFile(t *testing.T) {
	vaultDir, logPath := withServeLogDirs(t)
	seedServeVault(t, vaultDir)

	p := &program{addr: "127.0.0.1:0"}   // ephemeral port: no conflict with a real serve
	if err := p.Start(nil); err != nil { // Start ignores its service.Service arg
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop(nil) }() // idempotent safety net (double-cancel is a no-op)

	// Poll the file sink: the startup line is written BEFORE RunServe is
	// called, so it lands regardless of how fast RunServe binds. Bounded wait.
	deadline := time.Now().Add(5 * time.Second)
	var got []byte
	for {
		got, _ = os.ReadFile(logPath)
		if strings.Contains(string(got), "starting serve on") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve.log never received the startup line; content=%q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(got), "127.0.0.1:0") {
		t.Fatalf("startup line must carry the bind addr: %q", got)
	}

	// Stop must reap the run goroutine (cancels RunServe's ctx; ≤5s).
	if err := p.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestOpenServeLog_RotatesOversizeLog pins the rotation contract: a pre-seeded
// >5MiB serve.log is renamed to serve.log.1 (ONE generation) on open, and the
// fresh serve.log receives only the new writes.
func TestOpenServeLog_RotatesOversizeLog(t *testing.T) {
	_, logPath := withServeLogDirs(t)

	old := bytes.Repeat([]byte("x"), 6<<20) // 6 MiB > 5 MiB threshold
	if err := os.WriteFile(logPath, old, 0o600); err != nil {
		t.Fatal(err)
	}
	f := openServeLog()
	if f == nil {
		t.Fatal("openServeLog returned nil")
	}
	if _, err := f.WriteString("fresh line\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// serve.log.1 holds the pre-seeded 6 MiB byte-for-byte.
	fi1, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("serve.log.1 not created by rotation: %v", err)
	}
	if fi1.Size() != int64(len(old)) {
		t.Fatalf("serve.log.1 size = %d, want %d", fi1.Size(), len(old))
	}
	// serve.log is fresh: only the new line.
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh line\n" {
		t.Fatalf("serve.log = %q, want only the fresh line", got)
	}
}

// TestOpenServeLog_AppendsUnderCap: a ≤5MiB serve.log is NOT rotated — the
// sink opens O_APPEND, so history below the threshold survives.
func TestOpenServeLog_AppendsUnderCap(t *testing.T) {
	_, logPath := withServeLogDirs(t)
	if err := os.WriteFile(logPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openServeLog()
	if f == nil {
		t.Fatal("openServeLog returned nil")
	}
	if _, err := f.WriteString("appended\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("no rotation expected under the threshold; serve.log.1 stat err=%v", err)
	}
	got, _ := os.ReadFile(logPath)
	if string(got) != "existing\nappended\n" {
		t.Fatalf("serve.log = %q, want existing+appended", got)
	}
}

// TestOpenServeLog_NilOnFailure pins "ANY failure → nil": an unopenable path
// (missing parent dir) yields a nil sink, never an error — serve falls back to
// stderr-only and keeps running. Logging must never take serve down.
func TestOpenServeLog_NilOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_SERVE_LOG", filepath.Join(dir, "no-such-dir", "serve.log"))
	if f := openServeLog(); f != nil {
		f.Close()
		t.Fatal("openServeLog must return nil when the path cannot be opened")
	}
}
