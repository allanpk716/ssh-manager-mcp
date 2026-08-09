package eval

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestWireBroker proves wireBroker builds the binary, seeds the vault (server,
// profile, project+token), and writes an mcp.json whose env reaches the temp
// vault — with no LLM call. The plaintext token is asserted to round-trip
// through store.VerifyToken, which is the exact path the broker's MCP server
// uses to authenticate `--token` at startup. End-to-end serving (binary actually
// answers list_servers) is T3/T4 territory.
func TestWireBroker(t *testing.T) {
	requireEval(t)
	host, port, sshdCleanup := startEvalSSHD(t)
	defer sshdCleanup()

	mcpPath, token, _, cleanup := wireBroker(t, host, port)
	defer cleanup()

	if token == "" {
		t.Fatal("wireBroker returned empty token")
	}
	if mcpPath == "" {
		t.Fatal("wireBroker returned empty mcp config path")
	}

	// mcp.json exists and parses; its command points at a built binary that exists;
	// its env carries both SSHMGR_STORE and SSHMGR_MASTERKEY_HEX overrides.
	cfg, storePath, mkHex, binPath := parseMCPConfig(t, mcpPath)
	if cfg["args"].([]any)[2] != token {
		t.Fatalf("mcp.json args token = %q, want %q", cfg["args"].([]any)[2], token)
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("built binary missing at %s: %v", binPath, err)
	}

	// The plaintext token must verify against the seeded vault: re-open the store
	// with the env-supplied master key and run the broker's own VerifyToken path.
	mk, err := hex.DecodeString(mkHex)
	if err != nil {
		t.Fatalf("decode master key hex: %v", err)
	}
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
}

// parseMCPConfig reads the isolated mcp.json and returns the ssh server block
// plus the derived store path, master key hex, and binary path. Fails the test
// if the structure is malformed or env overrides are missing.
func parseMCPConfig(t *testing.T, mcpPath string) (ssh map[string]any, storePath, mkHex, binPath string) {
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
	mkHex, ok = env["SSHMGR_MASTERKEY_HEX"].(string)
	if !ok || mkHex == "" {
		t.Fatal("mcp.json env missing SSHMGR_MASTERKEY_HEX")
	}
	// resolve store path relative to mcp.json dir if needed (it is absolute here,
	// but resolving keeps the test robust to future changes).
	if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(filepath.Dir(mcpPath), storePath)
	}
	return ssh, storePath, mkHex, binPath
}
