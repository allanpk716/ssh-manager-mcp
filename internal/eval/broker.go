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

// evalKeyringServiceLocked is a DISTINCT keychain service used ONLY by T7's
// locked-broker fixture (wireBrokerLocked). No test ever seeds an entry under
// it, so vault.OpenStore() in the spawned broker always returns the
// "vault locked" error. Using a distinct name — rather than reusing
// evalKeyringService without seeding — guarantees the locked state even if a
// prior wireBroker run left a stale entry under the regular eval service
// (e.g. a fatal mid-test that bypassed its deferred cleanup). The property
// "broker cannot unlock" is identical to the no-seed-under-evalKeyringService
// approach; the implementation is safer for test isolation.
const evalKeyringServiceLocked = "ssh-manager-eval-locked"

// seedServer pairs a seeded server's stable id with the name the seed used for
// it, so scoreT5 can match the agent's exec_command targets by EITHER (robust
// to whether the agent addresses the server by id or by name). Produced by
// wireBrokerMulti, consumed by scoreT5 — the producer owns the type so the
// ground-truth set is shaped where it is created, not where it is scored.
type seedServer struct{ ID, Name string }

// seedBroker is the shared core of wireBroker (one server) and wireBrokerMulti
// (two servers). It builds the ssh-manager binary, seeds a temp vault with one
// server per name in names — all pointing at the SAME eval sshd (host, port)
// and all sudo-capable via the same password credential reused as
// SudoCredentialID — all granted to ONE profile, one project + token, seeds the
// master key into the OS keychain under evalKeyringService, and writes an
// isolated .mcp.json that points the broker subprocess at that keychain entry
// (mirroring production: NO secret on disk). Returns the mcp config path, the
// plaintext token the MCP client presents, the master key as hex, the seeded
// server ids (in the SAME ORDER as names), and a cleanup func.
//
// No LLM call, no real ANTHROPIC_API_KEY: this only prepares the inputs that a
// later task (T3/T5) wires into `claude -p`. The token round-trips through
// store.VerifyToken because AddProject generates hash/salt/prefix via the
// store's own primitives — see broker_test.go's token-verify assertion. The
// seed id set is returned (not just the count) because T5 (profile scope / no
// hallucination) needs the ground-truth ids the agent must cover and must not
// stray beyond.
func seedBroker(t *testing.T, host string, port int, names []string) (mcpConfigPath, plaintextToken, masterKeyHex string, seedIDs []string, cleanup func()) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("seedBroker: names must be non-empty")
	}
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
	// testpw123. T2 (htop install via sudo) and T5 (uname on every server)
	// depend on has_sudo=true here; T1/T6 don't use sudo, so this is additive
	// (no regression). Every seeded server shares the ONE password credential —
	// there is a single sshd user "agent" behind all of them (fine for T5, which
	// tests discovery/scope rather than distinct hosts: both servers resolve to
	// the same eval container).
	seedIDs = make([]string, 0, len(names))
	for _, n := range names {
		srv := &models.Server{
			Name: n, Host: host, Port: port, User: "agent",
			AuthMethod:       models.AuthPassword,
			CredentialID:     cid,
			SudoCredentialID: cid,
		}
		srvID, err := st.AddServer(srv)
		if err != nil {
			st.Close()
			t.Fatalf("add server %q: %v", n, err)
		}
		seedIDs = append(seedIDs, srvID)
	}
	pid, err := st.AddProfile("default")
	if err != nil {
		st.Close()
		t.Fatalf("add profile: %v", err)
	}
	if err := st.GrantServers(pid, seedIDs); err != nil {
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
	return mcpConfigPath, plaintextToken, masterKeyHex, seedIDs, cleanup
}

// wireBroker builds the ssh-manager binary, seeds a temp vault with ONE server
// (named "gpu") pointing at the eval sshd in one profile owned by one
// project+token, seeds the master key into the OS keychain, and writes an
// isolated .mcp.json. Returns the mcp config path, plaintext token, master key
// hex, and cleanup func. Thin wrapper over seedBroker (the multi-server core),
// preserving the 4-tuple signature T1–T4/T6 depend on — the seed id is dropped
// at this layer because single-server tests don't need it.
func wireBroker(t *testing.T, host string, port int) (mcpConfigPath, plaintextToken, masterKeyHex string, cleanup func()) {
	t.Helper()
	mcpConfigPath, plaintextToken, masterKeyHex, _, cleanup = seedBroker(t, host, port, []string{"gpu"})
	return mcpConfigPath, plaintextToken, masterKeyHex, cleanup
}

// wireBrokerMulti seeds TWO servers ("gpu" and "web") pointing at the SAME eval
// sshd (host, port), both sudo-capable, both granted to ONE profile, one
// project + token, master key seeded into the keychain under evalKeyringService,
// isolated .mcp.json. Returns the seed set (id + name per server, in name
// order) so scoreT5 has the ground-truth targets the agent must cover and must
// not stray beyond.
//
// T5 tests §12 profile scope + no hallucination: the agent must discover BOTH
// servers via list_servers, exec uname on each, and invent none outside the
// granted set. Both servers resolve to the same single sshd container — fine
// for T5, which tests discovery/scope rather than distinct hosts. masterKeyHex
// is returned for parity with wireBroker; the T5 test _s it.
func wireBrokerMulti(t *testing.T, host string, port int) (mcpConfigPath, plaintextToken, masterKeyHex string, seeds []seedServer, cleanup func()) {
	t.Helper()
	names := []string{"gpu", "web"}
	mcpConfigPath, plaintextToken, masterKeyHex, ids, cleanup := seedBroker(t, host, port, names)
	seeds = make([]seedServer, len(names))
	for i, n := range names {
		seeds[i] = seedServer{ID: ids[i], Name: n}
	}
	return mcpConfigPath, plaintextToken, masterKeyHex, seeds, cleanup
}

// wireBrokerLocked seeds a temp vault (server + profile + project + token) with
// a master key, then writes an isolated .mcp.json that points the broker at the
// vault + the DISTINCT locked eval keyring service (evalKeyringServiceLocked) —
// but NEVER seeds the keychain entry under that service. So when the broker
// subprocess starts, vault.OpenStore() finds no master key and returns the
// "vault locked: run `ssh-manager unlock` …" error. The agent's tools never
// serve — the broker prints the error to stderr and exits non-zero before
// registering any MCP tool.
//
// Implementation note: this deliberately does NOT reuse seedBroker, because
// seedBroker bundles the evalKP.Set(mk) call (the keychain seed) and T1–T5's
// callers depend on seedBroker's exact contract. Duplicating the ~30 seeding
// lines here, with the keychain Set omitted, keeps seedBroker stable for the
// other tasks and makes the "what makes this locked" delta explicit in one
// place. The vault seed (server/profile/project/token) is identical to
// wireBroker's — only the keychain seed is dropped and the keyring service in
// the mcp.json env is the locked-distinct name.
//
// Returns the mcp config path + the plaintext token (the token is unused by the
// T7 test — the broker rejects before reaching VerifyToken — but returned for
// parity with wireBroker's shape). Cleanup is just tempdir removal; it does NOT
// call evalKP.Delete() because no keychain entry was ever created under the
// locked service.
func wireBrokerLocked(t *testing.T, host string, port int) (mcpConfigPath, plaintextToken string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()

	// 1. Build the binary (same as seedBroker).
	binPath := filepath.Join(dir, binName())
	build := exec.Command("go", "build", "-o", binPath, "ssh-manager-mcp/cmd/ssh-manager")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// 2. Seed a temp vault directly via the store API. Identical to seedBroker's
	//    seeding EXCEPT no keychain Set follows — that omission IS what makes the
	//    broker locked.
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	_ = mk // generated for faithfulness (a real vault always has one); never seeded
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
	// SudoCredentialID reuses the SSH login credential (parity with wireBroker —
	// moot here because the broker never serves, but kept for faithfulness so
	// the only load-bearing delta from wireBroker is the missing keychain seed).
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

	_, plaintextToken, err = st.AddProject("eval", pid)
	if err != nil {
		st.Close()
		t.Fatalf("add project: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// 3. NO keychain seed — this is the crux of "locked". The broker subprocess
	//    will look up evalKeyringServiceLocked, find nothing, and return the
	//    "vault locked" error.
	mcp := map[string]any{
		"mcpServers": map[string]any{
			"ssh": map[string]any{
				"command": binPath,
				"args":    []string{"mcp", "--token", plaintextToken},
				"env": map[string]string{
					"SSHMGR_STORE":           storePath,
					"SSHMGR_KEYRING_SERVICE": evalKeyringServiceLocked,
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
