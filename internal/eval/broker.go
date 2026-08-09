package eval

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// wireBroker builds the ssh-manager binary, seeds a temp vault with one server
// (pointing at the eval sshd) in one profile owned by one project+token, and
// writes an isolated .mcp.json. Returns the mcp config path, the plaintext token
// the MCP client presents, and a cleanup func.
//
// No LLM call, no real ANTHROPIC_API_KEY: this only prepares the inputs that a
// later task (T3) wires into `claude -p`. The token round-trips through
// store.VerifyToken because AddProject generates hash/salt/prefix via the
// store's own primitives — see broker_test.go's token-verify assertion.
func wireBroker(t *testing.T, host string, port int) (mcpConfigPath, plaintextToken string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()

	// 1. Build the binary. Use the module-absolute package path so the build is
	// independent of the test's CWD (go test runs with CWD = internal/eval, so
	// "./cmd/ssh-manager" would resolve to internal/eval/cmd/ssh-manager — wrong).
	binPath := filepath.Join(dir, binName())
	build := exec.Command("go", "build", "-o", binPath, "ssh-manager-mcp/cmd/ssh-manager")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// 2. Seed a temp vault directly via the store API.
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	storePath := filepath.Join(dir, "store.db")
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("testpw123")})
	if err != nil {
		st.Close()
		t.Fatalf("set credential: %v", err)
	}
	srv := &models.Server{
		Name: "gpu", Host: host, Port: port, User: "agent",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	}
	srvID, err := st.AddServer(srv)
	if err != nil {
		st.Close()
		t.Fatalf("add server: %v", err)
	}
	pid, err := st.AddProfile("default")
	if err != nil {
		st.Close()
		t.Fatalf("add profile: %v", err)
	}
	if err := st.GrantServers(pid, []string{srvID}); err != nil {
		st.Close()
		t.Fatalf("grant servers: %v", err)
	}

	// 3. Project + token. AddProject generates the plaintext token internally and
	// stores hash/salt/prefix, returning the plaintext exactly once. The plaintext
	// goes into mcp.json args; VerifyToken(plaintext) accepts it because the hash
	// was derived from the same plaintext via the store's own HashToken.
	_, plaintextToken, err = st.AddProject("eval", pid)
	if err != nil {
		st.Close()
		t.Fatalf("add project: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// 4. Write the isolated .mcp.json. vault.OpenStore() reads SSHMGR_STORE (else
	// DefaultStorePath) and SSHMGR_MASTERKEY_HEX (else keychain), so both env vars
	// must be set for the spawned server process to reach this temp vault.
	mcp := map[string]any{
		"mcpServers": map[string]any{
			"ssh": map[string]any{
				"command": binPath,
				"args":    []string{"mcp", "--token", plaintextToken},
				"env": map[string]string{
					"SSHMGR_STORE":         storePath,
					"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
				},
			},
		},
	}
	mcpConfigPath = filepath.Join(dir, "mcp.json")
	writeJSON(t, mcpConfigPath, mcp)

	cleanup = func() { _ = os.RemoveAll(dir) }
	return mcpConfigPath, plaintextToken, cleanup
}

// binName returns the platform-correct binary name (Windows requires .exe).
func binName() string {
	if runtime.GOOS == "windows" {
		return "ssh-manager.exe"
	}
	return "ssh-manager"
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
