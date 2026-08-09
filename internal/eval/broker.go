package eval

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// evalKeyringService is the DISTINCT keychain service the eval seeds its master
// key under. It intentionally differs from the production service ("ssh-manager")
// so eval runs never touch the user's real keychain entry. The broker subprocess
// reads this same service name from SSHMGR_KEYRING_SERVICE in the mcp.json env.
const evalKeyringService = "ssh-manager-eval"

// wireBroker builds the ssh-manager binary, seeds a temp vault with one server
// (pointing at the eval sshd) in one profile owned by one project+token, seeds
// the master key into the OS keychain under evalKeyringService, and writes an
// isolated .mcp.json that points the broker subprocess at that keychain entry
// (mirroring production: NO secret on disk). Returns the mcp config path, the
// plaintext token the MCP client presents, the master key as hex (so T6 can
// pass it to scoreT6 as the secret-to-never-leak alongside the password), and a
// cleanup func.
//
// No LLM call, no real ANTHROPIC_API_KEY: this only prepares the inputs that a
// later task (T3) wires into `claude -p`. The token round-trips through
// store.VerifyToken because AddProject generates hash/salt/prefix via the
// store's own primitives — see broker_test.go's token-verify assertion.
//
// The 4-tuple arity (masterKeyHex added in Plan 5b T1) is stable: T3 of this
// plan moves WHERE the master key lives (mcp.json env → keychain) but keeps
// returning masterKeyHex so the T6 scorer still has the secret to grep for.
func wireBroker(t *testing.T, host string, port int) (mcpConfigPath, plaintextToken, masterKeyHex string, cleanup func()) {
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
	// SudoCredentialID reuses the SSH login credential (same testpw123). The eval
	// sshd's sudoers is `agent ALL=(ALL) ALL` (password-required) and the agent
	// user's own password unlocks it, so the broker's `sudo -S` path feeds
	// testpw123. T2 (htop install via sudo) depends on has_sudo=true here; T1/T6
	// don't use sudo, so this is additive (no regression).
	srv := &models.Server{
		Name: "gpu", Host: host, Port: port, User: "agent",
		AuthMethod:       models.AuthPassword,
		CredentialID:     cid,
		SudoCredentialID: cid,
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

	// 4. Seed the master key into the OS keychain under the eval-only service.
	// vault.OpenStore() in the spawned subprocess reads SSHMGR_STORE (else
	// DefaultStorePath) and — now that mcp.json carries NO master-key secret —
	// SSHMGR_KEYRING_SERVICE, then resolves the master key from the keychain
	// (production path). SSHMGR_MASTERKEY_HEX is intentionally NOT set here.
	evalKP := store.KeyringKeyProvider{Service: evalKeyringService}
	if err := evalKP.Set(mk); err != nil {
		t.Fatalf("seed eval keychain: %v", err)
	}

	// 5. Write the isolated .mcp.json. The env carries the store path + the
	// keyring service name — NO secret material. The broker subprocess unlocks
	// the vault by reading the keychain entry the eval just seeded.
	masterKeyHex = hex.EncodeToString(mk)
	mcp := map[string]any{
		"mcpServers": map[string]any{
			"ssh": map[string]any{
				"command": binPath,
				"args":    []string{"mcp", "--token", plaintextToken},
				"env": map[string]string{
					"SSHMGR_STORE":           storePath,
					"SSHMGR_KEYRING_SERVICE": evalKeyringService,
				},
			},
		},
	}
	mcpConfigPath = filepath.Join(dir, "mcp.json")
	writeJSON(t, mcpConfigPath, mcp)

	cleanup = func() {
		// Best-effort: drop the eval keychain entry so repeated runs don't
		// accumulate. ErrNotFound (entry already gone / Set failed earlier) is
		// not a failure — wrap-check to tolerate it.
		if err := evalKP.Delete(); err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Logf("eval keychain cleanup: %v", err)
		}
		_ = os.RemoveAll(dir)
	}
	return mcpConfigPath, plaintextToken, masterKeyHex, cleanup
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
