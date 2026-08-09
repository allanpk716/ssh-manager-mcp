package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedHome returns a throwaway HOME for a `claude -p` subprocess: a temp dir
// whose .ssh is EMPTY (no config, no identities, no known_hosts). This is the
// §12 Plan-5d eval-safety fix for the 5c T7 finding — a Bash-equipped agent
// whose broker was locked bypassed via the HOST's real ~/.ssh (reading real SSH
// aliases, touching real GPU/SSH hosts). Under an isolated HOME the agent's
// `cat ~/.ssh/config` and `ssh <alias>` find nothing real, which matches the
// production iron-rule reality: direct ssh fails because credentials live ONLY
// in the encrypted store. The broker subprocess (a child of `claude -p`)
// inherits this HOME; the OS keychain it unlocks is session-scoped, not
// HOME-scoped, so isolation does not break the unlock (empirically confirmed by
// TestEvalSkeletonT1 running green under isolation).
//
// t.TempDir is auto-cleaned by the test framework, so no explicit cleanup is
// returned — the caller does not need to defer anything.
func isolatedHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".ssh"), 0o700); err != nil {
		t.Fatalf("isolatedHome: create .ssh: %v", err)
	}
	return dir
}

// evalCmdEnv returns the child env for `claude -p`: the parent environment with
// (1) HOME and USERPROFILE forced to the isolated `home`, (2) inherited SSH_*
// and GIT_SSH* env dropped so the child cannot reach the host's ssh-agent or SSH
// routing, and (3) ANTHROPIC_API_KEY set exactly once from the parent
// (requireEval guaranteed it non-empty). SSHMGR_* (the broker's env, a different
// prefix) is preserved. ANTHROPIC_BASE_URL is carried untouched — that is the
// route to the local proxy (dropping it would send the subprocess to the real
// Anthropic API and fail auth in the dev's proxy setup).
func evalCmdEnv(home string) []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+3)
	for _, e := range parent {
		k, _, _ := strings.Cut(e, "=")
		// Drop inherited SSH agent/routing env (SSH_AUTH_SOCK, SSH_AGENT_PID, …).
		// SSHMGR_* does not start with "SSH_" (next char is 'M'), so it survives.
		if strings.HasPrefix(k, "SSH_") {
			continue
		}
		// Drop GIT_SSH / GIT_SSH_COMMAND (could route ssh elsewhere).
		if strings.HasPrefix(k, "GIT_SSH") {
			continue
		}
		// Drop inherited HOME/USERPROFILE so the child takes the isolated one.
		if k == "HOME" || k == "USERPROFILE" {
			continue
		}
		// De-dup ANTHROPIC_API_KEY (re-added exactly once below).
		if k == "ANTHROPIC_API_KEY" {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "HOME="+home, "USERPROFILE="+home)
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		out = append(out, "ANTHROPIC_API_KEY="+key)
	}
	return out
}
