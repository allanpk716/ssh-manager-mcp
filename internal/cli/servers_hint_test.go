package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// Plan 28 T2: `servers add` / `servers edit` print advisory, non-blocking
// suspected-secret warnings for the free-text metadata fields — a pasted PEM
// key or API token in a note would otherwise ride into every list_servers
// LLM context verbatim. The command's success path, return values, and exit
// codes are untouched, and the warning never echoes field content.

// newHintEnv pins an isolated vault (never the developer machine's real
// master.key — newVaultEnv pattern) and returns the db path + master key for
// post-hoc store inspection.
func newHintEnv(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	return dbPath, mk
}

// runCaptured executes one CLI invocation with stdout and stderr captured
// SEPARATELY, so a test can pin the warning stream independently of the
// success line and prove a suspected-secret value never echoes to either.
func runCaptured(t *testing.T, args ...string) (string, string) {
	t.Helper()
	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("cli %v: %v\nstdout: %s\nstderr: %s", args, err, stdout, stderr)
	}
	return stdout.String(), stderr.String()
}

// TestServersAddWarnsOnSuspectedSecret: a pasted PEM private key in
// --special-handling must NOT block the add (success path unchanged) but
// must print one advisory warning to stderr naming the field and rule —
// without echoing any of the suspected content.
func TestServersAddWarnsOnSuspectedSecret(t *testing.T) {
	dbPath, mk := newHintEnv(t)

	const sentinel = "SENTINEL-PEM-BODY-7QX"
	stdout, stderr := runCaptured(t, "servers", "add",
		"--name", "leaky", "--host", "h", "--user", "u",
		"--special-handling", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA"+sentinel+"AAAA\n-----END OPENSSH PRIVATE KEY-----")

	if !strings.Contains(stdout, "added server leaky") {
		t.Fatalf("add success line missing (success path must be unchanged), stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "field 'caveats'") || !strings.Contains(stderr, "pem-private-key") {
		t.Fatalf("stderr must carry the caveats/pem-private-key warning, got: %s", stderr)
	}
	if strings.Contains(stdout, sentinel) || strings.Contains(stderr, sentinel) {
		t.Fatalf("suspected-secret content must not echo to any stream:\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	// the server really landed in the vault — the warning is non-blocking
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if srv, _ := st.GetServerByName("leaky"); srv == nil {
		t.Fatal("server must be persisted despite the warning")
	}
}

// TestServersEditWarnsOnSuspectedSecret: partial-update semantics — only
// fields the operator actually passed are scanned. Setting description to an
// sk- token warns on stderr while the edit succeeds and persists; a later
// UNRELATED edit (--port only) must stay silent, proving unchanged fields
// are not re-scanned.
func TestServersEditWarnsOnSuspectedSecret(t *testing.T) {
	dbPath, mk := newHintEnv(t)

	runCaptured(t, "servers", "add", "--name", "ok", "--host", "h", "--user", "u")

	const sentinel = "SENTINEL-SK-BODY-4ZT"
	const skDesc = "key was sk-ant-api03-" + sentinel + "-fake"
	stdout, stderr := runCaptured(t, "servers", "edit", "ok", "--description", skDesc)

	if !strings.Contains(stdout, "updated server ok") {
		t.Fatalf("edit success line missing (edit must succeed), stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "field 'description'") || !strings.Contains(stderr, "prefix:sk-") {
		t.Fatalf("stderr must carry the description/sk- warning, got: %s", stderr)
	}
	if strings.Contains(stdout, sentinel) || strings.Contains(stderr, sentinel) {
		t.Fatalf("suspected-secret content must not echo to any stream:\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, _ := st.GetServerByName("ok")
	if srv == nil {
		t.Fatal("server vanished")
	}
	if srv.Description != skDesc {
		t.Fatalf("description must persist despite the warning, got %q", srv.Description)
	}
	st.Close()

	// unrelated partial edit: the (now stored) secret description is NOT
	// re-scanned — only Changed() fields go through the scanner.
	stdout, stderr = runCaptured(t, "servers", "edit", "ok", "--port", "2222")
	if !strings.Contains(stdout, "updated server ok") {
		t.Fatalf("port edit must succeed, stdout: %s", stdout)
	}
	if strings.Contains(stderr, "warning:") {
		t.Fatalf("unrelated edit must not warn about unchanged fields, stderr: %s", stderr)
	}
}

// TestServersAddNoWarningOnCleanMetadata: clean, legal metadata across all
// seven free-text fields produces ZERO warning lines — the hint is
// advisory-only and silent on clean input.
func TestServersAddNoWarningOnCleanMetadata(t *testing.T) {
	newHintEnv(t)

	stdout, stderr := runCaptured(t, "servers", "add",
		"--name", "clean", "--host", "h", "--user", "u",
		"--tags", "prod,gpu",
		"--description", "ml training box",
		"--location", "dc1 rack 4",
		"--hardware", "64c/512G/8xA100",
		"--services", "postgres primary",
		"--role", "prod pg primary",
		"--special-handling", "do not reboot during business hours")

	if !strings.Contains(stdout, "added server clean") {
		t.Fatalf("add must succeed, stdout: %s", stdout)
	}
	if strings.Contains(stderr, "warning:") {
		t.Fatalf("clean metadata must produce no warning, stderr: %s", stderr)
	}
}

// TestServersAddWarnsOnSuspectedSecretTag: tags are scanned in their final
// raw DB form (the JSON array string insertServerTx persists), so a token
// pasted as a tag warns as field 'tags' without blocking the add.
func TestServersAddWarnsOnSuspectedSecretTag(t *testing.T) {
	newHintEnv(t)

	const sentinel = "SENTINEL-TAG-9LM"
	stdout, stderr := runCaptured(t, "servers", "add",
		"--name", "tagged", "--host", "h", "--user", "u",
		"--tags", "gpu,ghp_"+sentinel+"fake")

	if !strings.Contains(stdout, "added server tagged") {
		t.Fatalf("add must succeed, stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "field 'tags'") || !strings.Contains(stderr, "prefix:ghp_") {
		t.Fatalf("stderr must carry the tags/ghp_ warning, got: %s", stderr)
	}
	if strings.Contains(stdout, sentinel) || strings.Contains(stderr, sentinel) {
		t.Fatalf("suspected-secret content must not echo to any stream:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}
