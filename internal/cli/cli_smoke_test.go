package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

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
