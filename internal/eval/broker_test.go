package eval

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestWireBroker proves wireBroker builds the binary, seeds the vault (server,
// profile, project+token), seeds the master key into the OS keychain under the
// eval service, and writes an mcp.json whose env carries SSHMGR_STORE +
// SSHMGR_KEYRING_SERVICE (and NOT the master key) — with no LLM call. The
// plaintext token is asserted to round-trip through store.VerifyToken, which is
// the exact path the broker's MCP server uses to authenticate `--token` at
// startup. The subprocess round-trip (servers ls) proves the built binary
// unlocks the vault via the keychain — the production path, now exercised by
// the eval. End-to-end serving (binary actually answers list_servers over MCP)
// is T4 territory.
func TestWireBroker(t *testing.T) {
	requireEval(t)
	host, port, _, sshdCleanup := startEvalSSHD(t)
	defer sshdCleanup()

	mcpPath, token, masterKeyHex, cleanup := wireBroker(t, host, port)
	defer cleanup()

	if token == "" {
		t.Fatal("wireBroker returned empty token")
	}
	if mcpPath == "" {
		t.Fatal("wireBroker returned empty mcp config path")
	}
	if masterKeyHex == "" {
		t.Fatal("wireBroker returned empty masterKeyHex")
	}

	// mcp.json exists and parses; its command points at a built binary that exists;
	// its env carries SSHMGR_STORE + SSHMGR_KEYRING_SERVICE (NOT SSHMGR_MASTERKEY_HEX
	// — the on-disk secret is gone, by design).
	cfg, storePath, binPath := parseMCPConfig(t, mcpPath)
	env := cfg["env"].(map[string]any)
	if _, leaked := env["SSHMGR_MASTERKEY_HEX"]; leaked {
		t.Fatal("mcp.json env STILL carries SSHMGR_MASTERKEY_HEX — eval-fidelity gap not closed")
	}
	if cfg["args"].([]any)[2] != token {
		t.Fatalf("mcp.json args token = %q, want %q", cfg["args"].([]any)[2], token)
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("built binary missing at %s: %v", binPath, err)
	}

	// The keychain seed round-trips: read it back via the provider and assert it
	// matches the returned masterKeyHex. This proves wireBroker seeded the eval
	// service (not the production service), and that the broker's own keyring
	// provider can fetch it.
	gotMK, err := store.KeyringKeyProvider{Service: evalKeyringService}.Get()
	if err != nil {
		t.Fatalf("read eval keychain entry: %v", err)
	}
	mk, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		t.Fatalf("decode master key hex: %v", err)
	}
	if !bytes.Equal(gotMK, mk) {
		t.Fatal("keychain master key mismatch: Get() != wireBroker's masterKeyHex")
	}

	// The plaintext token must verify against the seeded vault: re-open the store
	// with the keychain-resolved master key and run the broker's own VerifyToken path.
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	proj, err := st.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if proj == nil {
		t.Fatal("VerifyToken returned nil project — token does not match seeded vault")
	}
	if proj.Name != "eval" {
		t.Fatalf("verified project name = %q, want %q", proj.Name, "eval")
	}

	// At least one server is seeded and points at the eval sshd.
	servers, err := st.ListServers()
	if err != nil {
		t.Fatalf("list servers: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 seeded server, got %d", len(servers))
	}
	if servers[0].Host != host || servers[0].Port != port {
		t.Fatalf("seeded server = %s:%d, want %s:%d", servers[0].Host, servers[0].Port, host, port)
	}

	// Subprocess round-trip (no LLM): the BUILT binary, given only SSHMGR_STORE +
	// SSHMGR_KEYRING_SERVICE in its env (exactly what the mcp.json provides), must
	// unlock the vault via the keychain and list the seeded gpu server. A "vault
	// locked" error here means the keychain wiring is broken — the secret the
	// broker subprocess needs is NOT reachable via the new path.
	listOut := exec.Command(binPath, "servers", "ls")
	listOut.Env = append(os.Environ(),
		"SSHMGR_STORE="+storePath,
		"SSHMGR_KEYRING_SERVICE="+evalKeyringService,
		"SSHMGR_MASTERKEY_HEX=", // ensure the hex shortcut is NOT used
	)
	var stdout, stderr bytes.Buffer
	listOut.Stdout = &stdout
	listOut.Stderr = &stderr
	if err := listOut.Run(); err != nil {
		t.Fatalf("servers ls via keychain: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gpu") {
		t.Fatalf("servers ls via keychain did not list seeded gpu server\nstdout: %s\nstderr: %s",
			stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "vault locked") {
		t.Fatalf("broker subprocess could not read the master key from the keychain\nstderr: %s", stderr.String())
	}
}

// parseMCPConfig reads the isolated mcp.json and returns the ssh server block
// plus the derived store path and binary path. Fails the test if the structure
// is malformed or the SSHMGR_STORE / SSHMGR_KEYRING_SERVICE env overrides are
// missing. The mcp.json deliberately does NOT carry SSHMGR_MASTERKEY_HEX
// (asserted by callers) — the master key now lives in the OS keychain.
func parseMCPConfig(t *testing.T, mcpPath string) (ssh map[string]any, storePath, binPath string) {
	t.Helper()
	raw, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read mcp.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse mcp.json: %v", err)
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcp.json missing mcpServers")
	}
	ssh, ok = servers["ssh"].(map[string]any)
	if !ok {
		t.Fatal("mcp.json missing mcpServers.ssh")
	}
	binPath, ok = ssh["command"].(string)
	if !ok || binPath == "" {
		t.Fatal("mcp.json missing mcpServers.ssh.command")
	}
	args, ok := ssh["args"].([]any)
	if !ok || len(args) != 3 || args[0] != "mcp" || args[1] != "--token" {
		t.Fatalf("mcp.json args malformed: %v", ssh["args"])
	}
	env, ok := ssh["env"].(map[string]any)
	if !ok {
		t.Fatal("mcp.json missing mcpServers.ssh.env")
	}
	storePath, ok = env["SSHMGR_STORE"].(string)
	if !ok || storePath == "" {
		t.Fatal("mcp.json env missing SSHMGR_STORE")
	}
	svc, ok := env["SSHMGR_KEYRING_SERVICE"].(string)
	if !ok || svc == "" {
		t.Fatal("mcp.json env missing SSHMGR_KEYRING_SERVICE")
	}
	if svc != evalKeyringService {
		t.Fatalf("mcp.json SSHMGR_KEYRING_SERVICE = %q, want %q", svc, evalKeyringService)
	}
	// resolve store path relative to mcp.json dir if needed (it is absolute here,
	// but resolving keeps the test robust to future changes).
	if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(filepath.Dir(mcpPath), storePath)
	}
	return ssh, storePath, binPath
}
