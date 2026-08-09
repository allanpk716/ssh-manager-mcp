package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsolatedHome verifies isolatedHome returns a dir whose .ssh EXISTS and is
// EMPTY (no config, no identities, no known_hosts) — the §12 Plan-5d eval-safety
// guarantee that a Bash-equipped agent finds no real SSH config to bypass through.
func TestIsolatedHome(t *testing.T) {
	dir := isolatedHome(t)
	info, err := os.Stat(filepath.Join(dir, ".ssh"))
	if err != nil || !info.IsDir() {
		t.Fatalf("isolatedHome .ssh missing/not a dir: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".ssh"))
	if err != nil {
		t.Fatalf("readdir .ssh: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("isolatedHome .ssh not empty: %v", entries)
	}
}

// TestEvalCmdEnvIsolation verifies evalCmdEnv scrubs inherited SSH/GIT-SH env,
// forces HOME/USERPROFILE to the isolated dir, and dedups ANTHROPIC_API_KEY.
func TestEvalCmdEnvIsolation(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/fake-agent.sock")
	t.Setenv("SSH_AGENT_PID", "4242")
	t.Setenv("GIT_SSH_COMMAND", "evil-wrapper")
	t.Setenv("ANTHROPIC_API_KEY", "dummy-eval")

	home := "/tmp/iso-home-xyz"
	env := evalCmdEnv(home)

	have := map[string]string{}
	var apiKeyCount int
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		have[k] = v
		if k == "ANTHROPIC_API_KEY" {
			apiKeyCount++
		}
	}
	for _, banned := range []string{"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_SSH_COMMAND"} {
		if _, ok := have[banned]; ok {
			t.Fatalf("evalCmdEnv leaked banned env %q (the agent could reach the host ssh-agent/SSH routing)", banned)
		}
	}
	if have["HOME"] != home {
		t.Fatalf("HOME = %q, want %q", have["HOME"], home)
	}
	if have["USERPROFILE"] != home {
		t.Fatalf("USERPROFILE = %q, want %q (Windows needs USERPROFILE for home)", have["USERPROFILE"], home)
	}
	if apiKeyCount != 1 {
		t.Fatalf("ANTHROPIC_API_KEY appears %d times, want exactly 1", apiKeyCount)
	}
	// SSHMGR_* (broker env) must be preserved (different prefix than SSH_).
	t.Setenv("SSHMGR_STORE", "/tmp/store.db")
	env = evalCmdEnv(home)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "SSHMGR_STORE=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("evalCmdEnv dropped SSHMGR_STORE (only SSH_/GIT_SSH* should be scrubbed)")
	}
}
