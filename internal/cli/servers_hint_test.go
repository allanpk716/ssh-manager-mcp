package cli

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
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

// ---------------------------------------------------------------------------
// Plan 28 T3: `servers import` — the same advisory scan at the import save
// point.
//
// Field-flow fact the test shape depends on: the CLI import path carries ZERO
// user free-text metadata. importer.Candidate (internal/importer/importer.go)
// holds only Name/Host/Port/User/KeyPaths — ssh_config supplies HostName/
// Port/User/IdentityFile and nothing else — and runImport (servers_import.go)
// persists exactly those four fields plus at most the FIXED
// "needs-passphrase" tag literal; Description/Location/Hardware/Services/
// Role/Caveats are always "". A config file can therefore never exercise a
// hit, and the test pins the two halves of the wiring runImport performs per
// server right before each insert instead: scanImportServer (the aggregate
// scan over the exact persisted form) and printImportHints (the aggregated
// stderr block). The metadata-carrying import leg is the TUI supplement form,
// covered in internal/tui.

// TestImportWarnsOnSuspectedSecret: a server in the exact shape the import
// persists but carrying a secret-shaped tag (simulating the day the importer
// starts populating free-text fields) is flagged by the aggregate scan, and
// the warning names server+field+rule on the warning stream without echoing
// the suspected value.
func TestImportWarnsOnSuspectedSecret(t *testing.T) {
	const sentinel = "SENTINEL-IMPORT-SK-5VQ"
	srv := &models.Server{
		Name: "leaky", Host: "192.0.2.7", Port: 22, User: "u",
		Tags: []string{"gpu", "sk-ant-api03-" + sentinel + "-fake"},
	}

	findings := scanImportServer(srv)
	if len(findings) != 1 || findings[0].Field != "tags" || findings[0].Rule != "prefix:sk-" {
		t.Fatalf("scanImportServer must flag the secret-shaped tag in the persisted form, got %+v", findings)
	}

	warn := &bytes.Buffer{}
	printImportHints(warn, []importHint{{name: "leaky", findings: findings}})
	got := warn.String()
	for _, want := range []string{"leaky", "field 'tags'", "prefix:sk-"} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning stream must carry %q, got: %s", want, got)
		}
	}
	if strings.Contains(got, sentinel) {
		t.Fatalf("suspected-secret content must not echo: %s", got)
	}
}

// TestImportNoFalsePositiveOnImportedTags: the ONLY tag the real import path
// ever writes is the fixed needs-passphrase literal — a real encrypted-key
// import must leave the warning stream CLEAN (the defensive scan must not
// fire on legal imported state) and still succeed end to end.
func TestImportNoFalsePositiveOnImportedTags(t *testing.T) {
	dir := t.TempDir()
	newHintEnv(t)
	genKeyFile(t, filepath.Join(dir, "enc_key"), "secret-pass")
	cfg := filepath.Join(dir, "config")
	writeConfig(t, cfg, fmt.Sprintf(
		"Host enc\n  HostName 192.0.2.50\n  User u\n  IdentityFile %q\n",
		filepath.ToSlash(filepath.Join(dir, "enc_key"))))

	stdout, stderr := runCaptured(t, "servers", "import", "--file", cfg)
	if !strings.Contains(stdout, "imported needs-passphrase") {
		t.Fatalf("import must succeed with the needs-passphrase note, stdout: %s", stdout)
	}
	if strings.Contains(stderr, "warning:") {
		t.Fatalf("legal imported state (fixed needs-passphrase tag) must not warn, stderr: %s", stderr)
	}
}
