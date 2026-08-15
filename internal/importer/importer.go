// Package importer turns an OpenSSH client config into vault import candidates.
// Pure logic: no store, no network. All path/semantic decisions are documented
// deviations where they differ from OpenSSH (see spec 2026-08-15 rev1 §C1).
package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// Candidate is one literal-host alias ready for import. User is the final
// value (fallbackUser already applied). KeyPaths holds the raw strings from
// the config, in order — resolution is a separate step (ResolveKeyPaths).
type Candidate struct {
	Name, Host string
	Port       int
	User       string   // final value (fallbackUser applied)
	KeyPaths   []string // raw strings from config (resolution is a separate step)
}

// Skipped records an alias excluded from the candidate list and why.
// Reason is one of "wildcard-pattern", "bad-port", "internal-duplicate".
type Skipped struct{ Alias, Reason string }

// Result is the full outcome of parsing one config file.
type Result struct {
	Candidates   []Candidate
	Skipped      []Skipped
	MatchWarning bool // raw config contains Match blocks — inherited values may diverge from real ssh
}

var matchLine = regexp.MustCompile(`(?im)^\s*match\s`)

// Parse reads configPath and produces literal-host candidates.
//
// Semantics: values are looked up with ssh_config's first-obtained-wins Get
// (including "Host *" inheritance order), mirroring real ssh. Wildcard and
// negated Host patterns are skipped ("wildcard-pattern"); a malformed or
// out-of-range Port skips the alias ("bad-port"); a later alias resolving to
// an already-seen host:port:user triple is skipped ("internal-duplicate").
// The same alias appearing in multiple Host blocks contributes only its first
// occurrence (later blocks are irrelevant under first-obtained-wins, and the
// candidate list must not contain the alias twice).
func Parse(configPath, fallbackUser string) (*Result, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	cfg, err := ssh_config.DecodeBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}
	res := &Result{MatchWarning: matchLine.Match(raw)}
	seen := map[string]bool{}      // host:port:user triple dedup
	seenAlias := map[string]bool{} // same alias in multiple blocks: first occurrence wins
	// ssh_config seeds Hosts[0] with an implicit, empty "Host *" block
	// (parseSSH always starts from newConfig). It carries no values, so start
	// at index 1 to avoid a phantom wildcard skip on every parse.
	for _, host := range cfg.Hosts[1:] {
		for _, pat := range host.Patterns {
			alias := pat.String() // includes a leading "!" for negated patterns
			if strings.ContainsAny(alias, "*?!") {
				res.Skipped = append(res.Skipped, Skipped{alias, "wildcard-pattern"})
				continue
			}
			if seenAlias[alias] {
				continue
			}
			seenAlias[alias] = true
			hostName, _ := cfg.Get(alias, "HostName")
			hostName = strings.TrimSpace(hostName)
			if hostName == "" {
				hostName = alias // ssh semantics: alias doubles as hostname
			}
			portStr, _ := cfg.Get(alias, "Port")
			portStr = strings.TrimSpace(portStr)
			port := 22
			if portStr != "" {
				n, err := strconv.Atoi(portStr)
				if err != nil || n < 1 || n > 65535 {
					res.Skipped = append(res.Skipped, Skipped{alias, "bad-port"})
					continue
				}
				port = n
			}
			user, _ := cfg.Get(alias, "User")
			if user = strings.TrimSpace(user); user == "" {
				user = fallbackUser
			}
			// GetAll accumulates across matching blocks (incl. "Host *"),
			// matching real ssh IdentityFile accumulation.
			keys, _ := cfg.GetAll(alias, "IdentityFile")
			dedup := fmt.Sprintf("%s:%d:%s", hostName, port, user)
			if seen[dedup] {
				res.Skipped = append(res.Skipped, Skipped{alias, "internal-duplicate"})
				continue
			}
			seen[dedup] = true
			res.Candidates = append(res.Candidates, Candidate{Name: alias, Host: hostName, Port: port, User: user, KeyPaths: keys})
		}
	}
	return res, nil
}

// ResolveKeyPaths expands "~" and "~/" to the user home dir and resolves
// relative paths against configDir — a DELIBERATE, documented deviation from
// OpenSSH (which resolves against the ssh process CWD at connect time).
// "~user/..." forms pass through unresolved (the caller's ReadFile will fail
// and route to the needs-credential path). A leading "/" (POSIX absolute) and
// platform-native absolute paths pass through unchanged on every platform.
func ResolveKeyPaths(paths []string, configDir string) []string {
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		switch {
		case p == "~" || strings.HasPrefix(p, "~/"):
			out = append(out, filepath.Join(home, strings.TrimPrefix(p, "~")))
		case strings.HasPrefix(p, "~"):
			out = append(out, p) // ~user/... — left unresolved on purpose
		case filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
			out = append(out, p)
		default:
			out = append(out, filepath.Join(configDir, p))
		}
	}
	return out
}
