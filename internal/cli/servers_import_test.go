package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// genKeyFile writes a synthetic ed25519 private key (PKCS8 PEM) and returns
// its bytes. passphrase "" → plaintext "PRIVATE KEY" block; a passphrase →
// legacy x509.EncryptPEMBlock form (Proc-Type/DEK-Info headers), which
// ssh.ParsePrivateKey answers with *PassphraseMissingError — exactly the
// import needs-passphrase detection point. Synthetic keys only (public repo).
func genKeyFile(t *testing.T, path, passphrase string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block, err = x509.EncryptPEMBlock(rand.Reader, "PRIVATE KEY", der, []byte(passphrase), x509.PEMCipherAES256)
		if err != nil {
			t.Fatal(err)
		}
	}
	b := pem.EncodeToMemory(block)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newVaultEnv gives the subtest a fresh vault via the env seams: SSHMGR_STORE
// (db path), SSHMGR_MASTERKEY_HEX (master key), SSHMGR_FILEKEY_PATH (pointed
// at a file that does not exist, so the injected FileKeyProvider tier
// deterministically falls through to the env tier — never the developer
// machine's real master.key). Returns the db path + master key for direct
// inspection and a runner capturing command output.
func newVaultEnv(t *testing.T) (dbPath string, mk []byte, run func(args ...string) (string, error)) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "store.db")
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	run = func(args ...string) (string, error) {
		root := NewRootCmd()
		root.SetArgs(args)
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		err := root.Execute() // read out AFTER Execute (Go evaluates return args in order)
		return out.String(), err
	}
	return dbPath, mk, run
}

func openInspect(t *testing.T, dbPath string, mk []byte) *store.Store {
	t.Helper()
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func serverCount(t *testing.T, st *store.Store) int {
	t.Helper()
	ss, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	return len(ss)
}

// fixtureMain builds the main config: gpu (unencrypted key), bare (no key),
// dup (explicit User me — without it the fallback would be the OS account
// name and the triple would not collide with the pre-seeded server).
func fixtureMain(t *testing.T, keyPath string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config")
	writeConfig(t, cfg, fmt.Sprintf(
		"Host gpu\n  HostName 192.0.2.10\n  User deploy\n  IdentityFile %q\n"+
			"Host bare\n  HostName 192.0.2.20\n  User deploy\n"+
			"Host dup\n  HostName 192.0.2.99\n  User me\n",
		filepath.ToSlash(keyPath)))
	return cfg
}

// seedDupTarget pre-seeds a vault server occupying dup's host:port:user.
func seedDupTarget(t *testing.T, dbPath string, mk []byte) {
	t.Helper()
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "taken", Host: "192.0.2.99", Port: 22, User: "me",
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// TestServersImportFlow is the import smoke suite: dry-run writes nothing;
// real import lands servers with skip-existing on vault conflicts; re-run is
// idempotent; one key file shared by two hosts mints ONE credential row;
// --profile prechecks before import (dry-run included) and grants after;
// an encrypted key imports with a needs-passphrase warning.
func TestServersImportFlow(t *testing.T) {
	// 1) --dry-run: prints the will-import/skip table, writes nothing.
	t.Run("dry-run writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		dbPath, mk, run := newVaultEnv(t)
		keyPath := filepath.Join(dir, "plain_key")
		genKeyFile(t, keyPath, "")
		cfg := fixtureMain(t, keyPath)
		seedDupTarget(t, dbPath, mk)

		out, err := run("servers", "import", "--file", cfg, "--dry-run")
		if err != nil {
			t.Fatalf("dry-run: %v\n%s", err, out)
		}
		for _, want := range []string{
			"gpu", "will-import (key)",
			"bare", "will-import (needs-credential)",
			"dup", "skip-existing (host:port:user)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("dry-run output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "提示") {
			t.Errorf("dry-run must not print the post-import 提示 line:\n%s", out)
		}
		if n := serverCount(t, openInspect(t, dbPath, mk)); n != 1 {
			t.Fatalf("dry-run wrote to the vault: %d servers, want 1 (pre-seeded)", n)
		}
	})

	// 2) real import lands gpu+bare, skips dup; re-run is idempotent.
	t.Run("import then idempotent rerun", func(t *testing.T) {
		dir := t.TempDir()
		dbPath, mk, run := newVaultEnv(t)
		keyPath := filepath.Join(dir, "plain_key")
		genKeyFile(t, keyPath, "")
		cfg := fixtureMain(t, keyPath)
		seedDupTarget(t, dbPath, mk)

		out, err := run("servers", "import", "--file", cfg)
		if err != nil {
			t.Fatalf("import: %v\n%s", err, out)
		}
		for _, want := range []string{
			"imported key",                   // gpu
			"imported needs-credential",      // bare
			"skip-existing (host:port:user)", // dup vs pre-seeded
			"提示：",                            // closing line
		} {
			if !strings.Contains(out, want) {
				t.Errorf("import output missing %q:\n%s", want, out)
			}
		}
		st := openInspect(t, dbPath, mk)
		if n := serverCount(t, st); n != 3 {
			t.Fatalf("after import: %d servers, want 3 (taken+gpu+bare)", n)
		}
		gpu, _ := st.GetServerByName("gpu")
		if gpu == nil || gpu.CredentialID == "" || gpu.AuthMethod != models.AuthPrivateKey ||
			gpu.Host != "192.0.2.10" || gpu.Port != 22 || gpu.User != "deploy" {
			t.Fatalf("gpu not imported correctly: %+v", gpu)
		}
		bare, _ := st.GetServerByName("bare")
		if bare == nil || bare.CredentialID != "" {
			t.Fatalf("bare should be credential-less: %+v", bare)
		}

		// idempotent rerun: everything now conflicts by name (dup still by endpoint)
		out2, err := run("servers", "import", "--file", cfg)
		if err != nil {
			t.Fatalf("rerun: %v\n%s", err, out2)
		}
		if c := strings.Count(out2, "skip-existing (name)"); c != 2 {
			t.Errorf("rerun: %d skip-existing (name) lines, want 2:\n%s", c, out2)
		}
		if !strings.Contains(out2, "skip-existing (host:port:user)") {
			t.Errorf("rerun: dup skip line missing:\n%s", out2)
		}
		if strings.Contains(out2, "imported ") {
			t.Errorf("rerun must import nothing:\n%s", out2)
		}
		if n := serverCount(t, openInspect(t, dbPath, mk)); n != 3 {
			t.Fatalf("after rerun: %d servers, want 3", n)
		}
	})

	// 3) batch key dedup: two hosts sharing one key file → one credential row.
	t.Run("batch key dedup", func(t *testing.T) {
		dir := t.TempDir()
		dbPath, mk, run := newVaultEnv(t)
		keyPEM := genKeyFile(t, filepath.Join(dir, "shared_key"), "")
		cfg := filepath.Join(dir, "config")
		writeConfig(t, cfg, fmt.Sprintf(
			"Host k1\n  HostName 192.0.2.1\n  User u\n  IdentityFile %q\n"+
				"Host k2\n  HostName 192.0.2.2\n  User u\n  IdentityFile %q\n",
			filepath.ToSlash(filepath.Join(dir, "shared_key")),
			filepath.ToSlash(filepath.Join(dir, "shared_key"))))

		out, err := run("servers", "import", "--file", cfg)
		if err != nil {
			t.Fatalf("import: %v\n%s", err, out)
		}
		if c := strings.Count(out, "imported key"); c != 2 {
			t.Fatalf("want 2 'imported key' lines, got %d:\n%s", c, out)
		}
		st := openInspect(t, dbPath, mk)
		k1, _ := st.GetServerByName("k1")
		k2, _ := st.GetServerByName("k2")
		if k1 == nil || k2 == nil {
			t.Fatalf("k1/k2 not imported: %+v %+v", k1, k2)
		}
		if k1.CredentialID == "" || k1.CredentialID != k2.CredentialID {
			t.Fatalf("shared key must mint ONE credential row: k1=%q k2=%q", k1.CredentialID, k2.CredentialID)
		}
		cred, err := st.GetCredential(k1.CredentialID)
		if err != nil || cred == nil {
			t.Fatalf("GetCredential: %v %v", cred, err)
		}
		if string(cred.Secret) != string(keyPEM) || cred.Type != models.CredPrivateKey || len(cred.Passphrase) != 0 {
			t.Fatalf("credential content mismatch: type=%v passphrase=%d", cred.Type, len(cred.Passphrase))
		}
	})

	// 4) --profile precheck fails before any import (dry-run included).
	t.Run("profile missing errors before import", func(t *testing.T) {
		dir := t.TempDir()
		dbPath, mk, run := newVaultEnv(t)
		keyPath := filepath.Join(dir, "plain_key")
		genKeyFile(t, keyPath, "")
		cfg := fixtureMain(t, keyPath)

		for _, extra := range [][]string{
			{"--dry-run"},
			nil,
		} {
			args := append([]string{"servers", "import", "--file", cfg, "--profile", "nope"}, extra...)
			out, err := run(args...)
			if err == nil {
				t.Errorf("cli %v: expected error, got none\n%s", args, out)
			}
			if !strings.Contains(err.Error(), `profile "nope" not found`) {
				t.Errorf("cli %v: error = %v, want profile-not-found", args, err)
			}
		}
		if n := serverCount(t, openInspect(t, dbPath, mk)); n != 0 {
			t.Fatalf("failed precheck must import nothing: %d servers", n)
		}
	})

	// 5) --profile grant: dry-run prints only; real run grants both.
	t.Run("profile grant", func(t *testing.T) {
		dir := t.TempDir()
		dbPath, mk, run := newVaultEnv(t)
		cfg := filepath.Join(dir, "config")
		writeConfig(t, cfg,
			"Host t1\n  HostName 192.0.2.1\n  User u\n"+
				"Host t2\n  HostName 192.0.2.2\n  User u\n")
		st := openInspect(t, dbPath, mk)
		profID, err := st.AddProfile("team")
		if err != nil {
			t.Fatal(err)
		}

		out, err := run("servers", "import", "--file", cfg, "--dry-run", "--profile", "team")
		if err != nil {
			t.Fatalf("dry-run+profile: %v\n%s", err, out)
		}
		if !strings.Contains(out, "grant: 2 server(s) -> team (dry-run, not granted)") {
			t.Errorf("dry-run+profile grant line missing:\n%s", out)
		}
		if ids, _ := st.ServersForProfile(profID); len(ids) != 0 {
			t.Fatalf("dry-run must not grant, got %d bindings", len(ids))
		}

		out, err = run("servers", "import", "--file", cfg, "--profile", "team")
		if err != nil {
			t.Fatalf("import+profile: %v\n%s", err, out)
		}
		if !strings.Contains(out, "granted 2 server(s) -> team") {
			t.Errorf("granted line missing:\n%s", out)
		}
		ids, err := st.ServersForProfile(profID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 {
			t.Fatalf("ServersForProfile = %v, want 2 ids", ids)
		}
		if n := serverCount(t, st); n != 2 {
			t.Fatalf("servers = %d, want 2", n)
		}
	})

	// 6) encrypted key imports (no passphrase) + needs-passphrase warning.
	t.Run("encrypted key needs-passphrase", func(t *testing.T) {
		dir := t.TempDir()
		dbPath, mk, run := newVaultEnv(t)
		encPEM := genKeyFile(t, filepath.Join(dir, "enc_key"), "secret-pass")
		cfg := filepath.Join(dir, "config")
		writeConfig(t, cfg, fmt.Sprintf(
			"Host enc\n  HostName 192.0.2.50\n  User u\n  IdentityFile %q\n",
			filepath.ToSlash(filepath.Join(dir, "enc_key"))))

		out, err := run("servers", "import", "--file", cfg)
		if err != nil {
			t.Fatalf("import: %v\n%s", err, out)
		}
		if !strings.Contains(out, "needs-passphrase") {
			t.Errorf("needs-passphrase warning missing:\n%s", out)
		}
		if !strings.Contains(out, "imported needs-passphrase") {
			t.Errorf("import must still succeed for the encrypted key:\n%s", out)
		}
		st := openInspect(t, dbPath, mk)
		enc, _ := st.GetServerByName("enc")
		if enc == nil || enc.CredentialID == "" || enc.AuthMethod != models.AuthPrivateKey {
			t.Fatalf("enc not imported with its key: %+v", enc)
		}
		cred, err := st.GetCredential(enc.CredentialID)
		if err != nil || cred == nil {
			t.Fatalf("GetCredential: %v %v", cred, err)
		}
		if string(cred.Secret) != string(encPEM) || len(cred.Passphrase) != 0 {
			t.Fatal("encrypted key must be stored as-is, without a passphrase")
		}
	})
}
