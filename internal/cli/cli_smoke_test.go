package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// withEnv sets env vars for the test and restores on cleanup.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	old := map[string]string{}
	for k, v := range kv {
		old[k] = os.Getenv(k)
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k, v := range old {
			os.Setenv(k, v)
		}
	})
}

func TestServersAddAndListEndToEnd(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	root := NewRootCmd()
	root.SetArgs([]string{"servers", "add", "--name", "gpu", "--host", "10.0.0.5", "--user", "ubuntu", "--password", "pw"})

	out := &bytes.Buffer{}
	root.SetOut(out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	root2 := NewRootCmd()
	root2.SetArgs([]string{"servers", "ls"})
	root2.SetOut(out)
	if err := root2.Execute(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("gpu")) {
		t.Fatalf("ls output missing gpu: %s", out.String())
	}
}

func TestServersEditAndDescription(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	dbPath := filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	// mustCli runs a CLI command, failing the test on error; returns captured stdout.
	mustCli := func(args ...string) *bytes.Buffer {
		root := NewRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
		return out
	}

	// add WITH structured fields + description
	mustCli("servers", "add", "--name", "gpu", "--host", "h", "--user", "u",
		"--password", "pw",
		"--role", "prod ml", "--special-handling", "do not reboot 02-03:00",
		"--location", "dc2", "--hardware", "8x A100",
		"--services", "jupyter", "--description", "owner note")

	// ls now shows role + truncated caveats (description no longer on the ls line)
	if out := mustCli("servers", "ls"); !bytes.Contains(out.Bytes(), []byte("prod ml")) || !bytes.Contains(out.Bytes(), []byte("do not reboot 02-03:00")) {
		t.Fatalf("ls missing role/caveats: %s", out.String())
	}
	if out := mustCli("servers", "ls"); bytes.Contains(out.Bytes(), []byte("owner note")) {
		t.Fatalf("ls should not show description anymore: %s", out.String())
	}

	// inspect directly via the store to assert id/cred invariants across edit
	inspect := func() *models.Server {
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		srv, _ := st.GetServerByName("gpu")
		return srv
	}
	before := inspect()
	if before == nil {
		t.Fatal("server not found after add")
	}
	if before.Description != "owner note" {
		t.Fatalf("description = %q", before.Description)
	}
	if before.Role != "prod ml" || before.Caveats != "do not reboot 02-03:00" {
		t.Fatalf("structured fields not stored: %+v", before)
	}
	oldCid := before.CredentialID

	// edit host + role + clear caveats (pass empty); id + cred preserved (no re-credential flag given)
	mustCli("servers", "edit", "gpu", "--host", "newhost", "--role", "prod ml replica", "--special-handling", "")
	after := inspect()
	if after.Host != "newhost" || after.Role != "prod ml replica" || after.Caveats != "" {
		t.Fatalf("edit did not apply: %+v", after)
	}
	if after.Description != "owner note" {
		t.Fatalf("description should be unchanged when --description not passed: %q", after.Description)
	}
	if after.ID != before.ID {
		t.Fatalf("id changed: %s -> %s", before.ID, after.ID)
	}
	if after.CredentialID != oldCid {
		t.Fatalf("credential_id changed without a re-credential flag")
	}

	// re-credential via --password: credential_id rotates, auth stays password, id preserved
	mustCli("servers", "edit", "gpu", "--password", "newpw")
	rc := inspect()
	if rc.ID != before.ID {
		t.Fatal("id changed on re-credential")
	}
	if rc.CredentialID == oldCid {
		t.Fatal("credential_id should change on re-credential")
	}
	if rc.AuthMethod != models.AuthPassword {
		t.Fatalf("auth_method = %v, want password", rc.AuthMethod)
	}

	// edit on an unknown server errors
	root := NewRootCmd()
	root.SetArgs([]string{"servers", "edit", "nope", "--host", "x"})
	if err := root.Execute(); err == nil {
		t.Fatal("edit unknown server should error")
	}
}
