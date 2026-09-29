package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/vaultio"
)

// runBackupArgs runs `backup create [args...]` verbatim (no implicit --dir) so
// --config-only invocations are exercisable. Returns stdout.
func runBackupArgs(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	root := NewRootCmd()
	root.SetArgs(append([]string{"backup", "create"}, args...))
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

// writeBackupConfig writes raw JSON content to path and returns the path.
func writeBackupConfig(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// backupConfigJSON builds a backup.json body from the three fields (%q escapes
// Windows backslashes into valid JSON strings).
func backupConfigJSON(dir string, keep int, passFile string) string {
	return fmt.Sprintf(`{"dir":%q,"keep":%d,"passphrase_file":%q}`, dir, keep, passFile)
}

// --- loadBackupConfig unit tests ---

// TestBackupConfig_LoadOk: a well-formed config parses to all three fields;
// keep=0 must survive (pointer-presence decoding — 0 is legal, "no rotation").
func TestBackupConfig_LoadOk(t *testing.T) {
	dir := t.TempDir()
	pass := writePassFile(t, t.TempDir(), encPass1)
	cfgPath := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"), backupConfigJSON(dir, 30, pass))

	cfg, err := loadBackupConfig(cfgPath)
	if err != nil {
		t.Fatalf("valid config must load: %v", err)
	}
	if cfg.Dir != dir || cfg.PassphraseFile != pass || cfg.Keep != 30 {
		t.Fatalf("fields mismatch: %+v", cfg)
	}

	cfgPath0 := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"), backupConfigJSON(dir, 0, pass))
	cfg0, err := loadBackupConfig(cfgPath0)
	if err != nil {
		t.Fatalf("keep=0 is a legal value (no rotation), must load: %v", err)
	}
	if cfg0.Keep != 0 {
		t.Fatalf("keep=0 must be preserved, got %d", cfg0.Keep)
	}
}

// TestBackupConfig_FileMissing_FailClosed: a --config path that does not exist
// is a hard error.
func TestBackupConfig_FileMissing_FailClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "backup.json")
	if _, err := loadBackupConfig(missing); err == nil {
		t.Fatal("missing config file must fail closed")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error must say the file was not found: %v", err)
	}
}

// TestBackupConfig_NoConfig_Nil: an empty --config value means "no config" —
// no error, nil config, built-in defaults stay in charge.
func TestBackupConfig_NoConfig_Nil(t *testing.T) {
	cfg, err := loadBackupConfig("")
	if err != nil || cfg != nil {
		t.Fatalf("empty config path must be (nil, nil), got (%+v, %v)", cfg, err)
	}
}

// TestBackupConfig_BadJSON_FailClosed: malformed JSON must fail closed.
func TestBackupConfig_BadJSON_FailClosed(t *testing.T) {
	cfgPath := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"), `{"dir": "C:\x", "keep":`)
	if _, err := loadBackupConfig(cfgPath); err == nil {
		t.Fatal("bad JSON must fail closed")
	} else if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error must name the JSON problem: %v", err)
	}
}

// TestBackupConfig_MissingFields_FailClosed: all three fields are required —
// each missing field is named in the error; an empty object names all three.
func TestBackupConfig_MissingFields_FailClosed(t *testing.T) {
	dir := t.TempDir()
	pass := writePassFile(t, t.TempDir(), encPass1)
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"missing dir", fmt.Sprintf(`{"keep":7,"passphrase_file":%q}`, pass), `"dir"`},
		{"missing keep", fmt.Sprintf(`{"dir":%q,"passphrase_file":%q}`, dir, pass), `"keep"`},
		{"missing passphrase_file", fmt.Sprintf(`{"dir":%q,"keep":7}`, dir), `"passphrase_file"`},
		{"empty object", `{}`, `"dir"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"), tc.content)
			if _, err := loadBackupConfig(cfgPath); err == nil {
				t.Fatal("missing required field must fail closed")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error must name the missing field %s: %v", tc.want, err)
			}
		})
	}
	// the empty object must name ALL three missing fields
	emptyPath := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"), `{}`)
	_, err := loadBackupConfig(emptyPath)
	if err == nil {
		t.Fatal("empty object must fail closed")
	}
	for _, field := range []string{`"dir"`, `"keep"`, `"passphrase_file"`} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("empty-object error must name %s: %v", field, err)
		}
	}
}

// TestBackupConfig_RelativePaths_FailClosed: both path fields must be absolute.
func TestBackupConfig_RelativePaths_FailClosed(t *testing.T) {
	absDir := t.TempDir()
	pass := writePassFile(t, t.TempDir(), encPass1)

	relDir := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON("relative-backups", 7, pass))
	if _, err := loadBackupConfig(relDir); err == nil {
		t.Fatal("relative dir must fail closed")
	} else if !strings.Contains(err.Error(), "absolute") || !strings.Contains(err.Error(), `"dir"`) {
		t.Fatalf("error must name dir and require an absolute path: %v", err)
	}

	relPass := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(absDir, 7, "relative/backup.pass"))
	if _, err := loadBackupConfig(relPass); err == nil {
		t.Fatal("relative passphrase_file must fail closed")
	} else if !strings.Contains(err.Error(), "absolute") || !strings.Contains(err.Error(), `"passphrase_file"`) {
		t.Fatalf("error must name passphrase_file and require an absolute path: %v", err)
	}
}

// TestBackupConfig_PassInSubtree_FailClosed: the passphrase-location invariant —
// passphrase_file inside the backup dir subtree (or equal to it) is rejected;
// a sibling path outside the dir is fine.
func TestBackupConfig_PassInSubtree_FailClosed(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "backups")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		pass string
	}{
		{"direct child", filepath.Join(dir, "backup.pass")},
		{"nested child", filepath.Join(dir, "sub", "backup.pass")},
		{"equal to dir", dir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
				backupConfigJSON(dir, 7, tc.pass))
			_, err := loadBackupConfig(cfgPath)
			if err == nil {
				t.Fatal("passphrase inside the backup dir subtree must fail closed")
			}
			if !strings.Contains(err.Error(), "must not be inside the backup dir") {
				t.Fatalf("error must explain the passphrase file cannot live in the backup dir: %v", err)
			}
		})
	}

	// sibling OUTSIDE the dir: fine
	sibling := filepath.Join(parent, "backup.pass")
	sibPath := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(dir, 7, sibling))
	if _, err := loadBackupConfig(sibPath); err != nil {
		t.Fatalf("passphrase outside the backup dir must load: %v", err)
	}
}

// TestBackupConfig_SubtreeSlashAndCaseVariants: the subtree judgment must be
// made on cleaned/folded paths — forward-slash spellings and (on Windows)
// case-variant spellings of the same location are still violations.
func TestBackupConfig_SubtreeSlashAndCaseVariants(t *testing.T) {
	dir := t.TempDir()

	// forward slashes everywhere (filepath.Clean must normalize before compare)
	slashPass := filepath.ToSlash(dir) + "/nested/backup.pass"
	if err := validatePassphraseNotInDir(dir, slashPass); err == nil {
		t.Fatal("forward-slash subtree spelling must be rejected")
	}

	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive folding is a Windows filesystem property; slash variant above covers the rest")
	}
	casePass := filepath.Join(strings.ToUpper(dir), "BACKUP.pass")
	if err := validatePassphraseNotInDir(dir, casePass); err == nil {
		t.Fatal("case-variant subtree spelling must be rejected on Windows")
	}
}

// --- CLI-level: --config wiring ---

// TestBackupCreate_ConfigOnly_EncryptedMode: `backup create --config <file>`
// with NO other flags runs the full ticket-01 encrypted chain — the config is
// the single source of truth (config beats built-in defaults for all three
// fields: dir supplies the target, keep=2 rotates, passphrase_file activates
// encrypted .sme mode).
func TestBackupCreate_ConfigOnly_EncryptedMode(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	pass := writePassFile(t, t.TempDir(), encPass1)
	cfgPath := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(bdir, 2, pass))

	for i := 0; i < 3; i++ {
		if _, err := runBackupArgs(t, "--config", cfgPath); err != nil {
			t.Fatalf("config-only create run %d: %v", i, err)
		}
	}
	// keep=2 came from config, not the built-in default 7 (which would keep 3)
	if m := smeFiles(t, bdir); len(m) != 2 {
		t.Fatalf("config keep=2 must rotate 3 runs down to 2 files; got %v", m)
	}
	for _, p := range smeFiles(t, bdir) {
		blob, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !vaultio.IsEncrypted(blob) {
			t.Fatalf("%s must be SSHMGRV1-encrypted (passphrase_file from config activates encrypted mode)", filepath.Base(p))
		}
		if _, err := vaultio.Decrypt([]byte(encPass1), blob); err != nil {
			t.Fatalf("%s must decrypt with the config passphrase: %v", filepath.Base(p), err)
		}
	}
}

// TestBackupCreate_Config_GenerationCheck: --config enters the SAME encrypted
// chain as the direct flags, including the pre-flight passphrase generation
// check — a config whose passphrase cannot decrypt the newest existing .sme
// aborts before writing.
func TestBackupCreate_Config_GenerationCheck(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	pass1 := writePassFile(t, t.TempDir(), encPass1)
	pass2 := writePassFile(t, t.TempDir(), encPass2)
	cfg1 := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(bdir, 7, pass1))
	cfg2 := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(bdir, 7, pass2))

	if _, err := runBackupArgs(t, "--config", cfg1); err != nil {
		t.Fatalf("first config-only create: %v", err)
	}
	_, err := runBackupArgs(t, "--config", cfg2)
	if err == nil {
		t.Fatal("config with a wrong-passphrase generation state must be rejected")
	}
	if !strings.Contains(err.Error(), "generation check") {
		t.Fatalf("must fail on the ticket-01 generation check: %v", err)
	}
	if m := smeFiles(t, bdir); len(m) != 1 {
		t.Fatalf("rejected run must not write; got %v", m)
	}
}

// TestBackupCreate_Priority_FlagOverConfig: explicit flags beat config values
// for all three fields — dir lands in the flagged dir, the product encrypts
// with the flagged passphrase, and keep follows the flag.
func TestBackupCreate_Priority_FlagOverConfig(t *testing.T) {
	seedVaultForBackup(t)
	dirA := t.TempDir()
	touchMarker(t, dirA)
	dirB := t.TempDir()
	touchMarker(t, dirB)
	passCfg := writePassFile(t, t.TempDir(), encPass1)
	passFlag := writePassFile(t, t.TempDir(), encPass2)

	// (a) dir: flag --dir B wins over config dir A; nothing lands in A
	cfgA := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(dirA, 7, passCfg))
	if _, err := runBackupArgs(t, "--config", cfgA, "--dir", dirB); err != nil {
		t.Fatalf("flag dir override: %v", err)
	}
	if m := smeFiles(t, dirB); len(m) != 1 {
		t.Fatalf("flag --dir must win over config dir; B has %v", m)
	}
	if m := smeFiles(t, dirA); len(m) != 0 {
		t.Fatalf("config dir must be ignored when --dir is explicit; A has %v", m)
	}

	// (b) passphrase-file: the flag's passphrase seals the product, not the
	// config's (fresh dir — a shared dir with (a)'s config-pass lineage would
	// trip the generation check, which is ticket-01 behavior, not priority)
	dirC := t.TempDir()
	touchMarker(t, dirC)
	cfgPass := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(dirC, 7, passCfg))
	if _, err := runBackupArgs(t, "--config", cfgPass, "--passphrase-file", passFlag); err != nil {
		t.Fatalf("flag passphrase override: %v", err)
	}
	m := smeFiles(t, dirC)
	if len(m) != 1 {
		t.Fatalf("expected 1 .sme in C, got %v", m)
	}
	blob, err := os.ReadFile(m[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vaultio.Decrypt([]byte(encPass2), blob); err != nil {
		t.Fatalf("flag --passphrase-file must win over config passphrase_file: %v", err)
	}
	if _, err := vaultio.Decrypt([]byte(encPass1), blob); err == nil {
		t.Fatal("config passphrase must NOT seal the product when the flag overrides it")
	}

	// (c) keep: flag keep=1 over config keep=9 leaves exactly 1 after 3 runs
	dirD := t.TempDir()
	touchMarker(t, dirD)
	cfgKeep := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(dirD, 9, passFlag))
	for i := 0; i < 3; i++ {
		if _, err := runBackupArgs(t, "--config", cfgKeep, "--keep", "1"); err != nil {
			t.Fatalf("flag keep run %d: %v", i, err)
		}
	}
	if m := smeFiles(t, dirD); len(m) != 1 {
		t.Fatalf("flag --keep 1 must win over config keep=9; got %v", m)
	}
}

// TestBackupCreate_ConfigErrors_FailClosed: every config defect is a hard error
// before anything runs — and a --passphrase-file flag may not smuggle the
// passphrase into the config's backup dir (the invariant holds on EFFECTIVE
// values, not just the config file's own).
func TestBackupCreate_ConfigErrors_FailClosed(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)

	missing := filepath.Join(t.TempDir(), "backup.json")
	if _, err := runBackupArgs(t, "--config", missing); err == nil {
		t.Fatal("missing config file must fail closed at the CLI")
	}

	badJSON := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"), `{oops`)
	if _, err := runBackupArgs(t, "--config", badJSON); err == nil {
		t.Fatal("bad JSON config must fail closed at the CLI")
	}

	passOutside := writePassFile(t, t.TempDir(), encPass1)
	relDirCfg := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON("relative/dir", 7, passOutside))
	if _, err := runBackupArgs(t, "--config", relDirCfg); err == nil {
		t.Fatal("relative dir in config must fail closed at the CLI")
	}

	passInside := filepath.Join(bdir, "backup.pass")
	subtreeCfg := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(bdir, 7, passInside))
	if _, err := runBackupArgs(t, "--config", subtreeCfg); err == nil {
		t.Fatal("passphrase inside the config backup dir must fail closed at the CLI")
	} else if !strings.Contains(err.Error(), "must not be inside the backup dir") {
		t.Fatalf("error must explain the passphrase file cannot live in the backup dir: %v", err)
	}

	// effective-value invariant: config is clean, but the --passphrase-file
	// flag points INTO the config dir — the merged run must be rejected too.
	cfgClean := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(bdir, 7, passOutside))
	if err := os.WriteFile(passInside, []byte(encPass1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runBackupArgs(t, "--config", cfgClean, "--passphrase-file", passInside); err == nil {
		t.Fatal("a flag passphrase inside the effective backup dir must fail closed")
	} else if !strings.Contains(err.Error(), "must not be inside the backup dir") {
		t.Fatalf("error must explain the passphrase file cannot live in the backup dir: %v", err)
	}
	if m := smeFiles(t, bdir); len(m) != 0 {
		t.Fatalf("no failed run may write: %v", m)
	}
}

// TestBackupCreate_NoDirNoConfig_ExplainsBoth: with the cobra required
// annotation gone from --dir (a config file can now supply it), a bare create
// must fail with a message that names both --dir and --config.
func TestBackupCreate_NoDirNoConfig_ExplainsBoth(t *testing.T) {
	seedVaultForBackup(t)
	_, err := runBackupArgs(t)
	if err == nil {
		t.Fatal("bare backup create must fail")
	}
	if !strings.Contains(err.Error(), "--dir") || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("error must point at --dir / --config: %v", err)
	}
}

// TestBackupCreate_RelativePassphraseInsideDir_FailClosed: the effective-value
// subtree check must Abs the flag passphrase first — a relative path that
// resolves into the backup dir (cwd = parent of it) must not slip past the
// volume-name comparison (ticket-02 review finding, fixed same night).
func TestBackupCreate_RelativePassphraseInsideDir_FailClosed(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	passOutside := writePassFile(t, t.TempDir(), encPass1)

	cfg := writeBackupConfig(t, filepath.Join(t.TempDir(), "backup.json"),
		backupConfigJSON(bdir, 7, passOutside))

	passInside := filepath.Join(bdir, "backup.pass")
	if err := os.WriteFile(passInside, []byte(encPass1), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Dir(bdir)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	relPass := filepath.Join(filepath.Base(bdir), "backup.pass")

	if _, err := runBackupArgs(t, "--config", cfg, "--passphrase-file", relPass); err == nil {
		t.Fatal("a relative flag passphrase resolving inside the effective backup dir must fail closed")
	} else if !strings.Contains(err.Error(), "must not be inside the backup dir") {
		t.Fatalf("error must explain the passphrase file cannot live in the backup dir: %v", err)
	}
	if m := smeFiles(t, bdir); len(m) != 0 {
		t.Fatalf("no failed run may write: %v", m)
	}
}
