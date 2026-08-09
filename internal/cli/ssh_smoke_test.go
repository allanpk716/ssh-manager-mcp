package cli

import (
	"bytes"
	"encoding/hex"
	"io"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestOwnerSSHExecRunsCommand(t *testing.T) {
	// start a test sshd that echoes the command
	addr, hostKey, srvCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "RAN:" + cmd + "\n", "", 0
		},
	})
	defer srvCleanup()
	host := addr[:bytesIndex(addr, ':')]

	// set up an isolated vault + master key
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	// seed a server pointing at the test sshd
	st, err := store.Open(filepath.Join(dir, "test.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, _ := st.AddServer(&models.Server{
		Name: "t", Host: host, Port: portOfAddr(addr), User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	// pre-trust the test host key (TOFU would also work, but pin for determinism)
	st.SaveHostKey(host, portOfAddr(addr), hostKey.Marshal())
	st.Close()
	_ = srvID

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"ssh", "t", "echo", "hello"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ssh cmd: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("RAN:echo hello")) {
		t.Fatalf("output missing exec result: %q", out.String())
	}
}

// small helpers local to this test
func bytesIndex(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return len(s)
}
func portOfAddr(addr string) int {
	i := bytesIndex(addr, ':')
	var p int
	for _, r := range addr[i+1:] {
		p = p*10 + int(r-'0')
	}
	return p
}
