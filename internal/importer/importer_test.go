package importer_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"ssh-manager-mcp/internal/importer"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func checkCandidates(t *testing.T, got, want []importer.Candidate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidates: got %d (%+v), want %d (%+v)", len(got), got, len(want), want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Name != w.Name || g.Host != w.Host || g.Port != w.Port || g.User != w.User {
			t.Errorf("candidate[%d] = {Name:%s Host:%s Port:%d User:%s}, want {Name:%s Host:%s Port:%d User:%s}",
				i, g.Name, g.Host, g.Port, g.User, w.Name, w.Host, w.Port, w.User)
		}
		if !reflect.DeepEqual(g.KeyPaths, w.KeyPaths) {
			t.Errorf("candidate[%d].KeyPaths = %q, want %q", i, g.KeyPaths, w.KeyPaths)
		}
	}
}

func checkSkipped(t *testing.T, got []importer.Skipped, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("skipped: got %d (%+v), want %d (%v)", len(got), got, len(want), want)
	}
	for _, s := range got {
		reason, ok := want[s.Alias]
		if !ok || reason != s.Reason {
			t.Errorf("skipped %q = %q, want %q (present=%v)", s.Alias, s.Reason, reason, ok)
		}
	}
}

func TestParseTable(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		fbUser  string
		want    []importer.Candidate
		skipped map[string]string
		matchW  bool
	}{
		{"literal+inherit", "Host gpu\n  HostName 192.0.2.10\n  User deploy\nHost db\n  HostName 192.0.2.20\nHost *\n  Port 2222\n  User fallback\n", "fb",
			// gpu Port 2222: no Port in gpu's own block, so first-obtained
			// value comes from "Host *" (real ssh gives 2222 here too).
			[]importer.Candidate{{Name: "gpu", Host: "192.0.2.10", Port: 2222, User: "deploy"},
				{Name: "db", Host: "192.0.2.20", Port: 2222, User: "fallback"}},
			// The fixture's explicit "Host *" block is itself a wildcard
			// pattern and is reported as such.
			map[string]string{"*": "wildcard-pattern"}, false},
		{"wildcard skipped", "Host *\n  HostName x\nHost gpu-*\n  HostName y\nHost gpu1\n  HostName z\n", "u",
			// gpu1 HostName "x": the leading "Host *" block supplies the
			// first-obtained HostName (real ssh would connect to x, not z).
			[]importer.Candidate{{Name: "gpu1", Host: "x", Port: 22, User: "u"}},
			map[string]string{"*": "wildcard-pattern", "gpu-*": "wildcard-pattern"}, false},
		{"multi-name block", "Host a b\n  HostName 192.0.2.1\n", "u",
			// Both patterns are iterated, but a and b resolve to the same
			// host:port:user triple, so b hits the internal-duplicate rule
			// (same shape as the "internal dedup" case).
			[]importer.Candidate{{Name: "a", Host: "192.0.2.1", Port: 22, User: "u"}},
			map[string]string{"b": "internal-duplicate"}, false},
		{"hostname defaults to alias", "Host jump\n", "u",
			[]importer.Candidate{{Name: "jump", Host: "jump", Port: 22, User: "u"}}, nil, false},
		{"internal dedup", "Host a\n  HostName h\n  User u\n  Port 22\nHost b\n  HostName h\n  User u\n  Port 22\n", "u",
			[]importer.Candidate{{Name: "a", Host: "h", Port: 22, User: "u"}},
			map[string]string{"b": "internal-duplicate"}, false},
		{"bad port", "Host x\n  Port abc\n", "u", nil,
			map[string]string{"x": "bad-port"}, false},
		{"port out of range", "Host x\n  Port 70000\n", "u", nil,
			map[string]string{"x": "bad-port"}, false},
		{"multi identityfile ordered", "Host k\n  IdentityFile ~/.ssh/a\n  IdentityFile b_key\n", "u",
			[]importer.Candidate{{Name: "k", Host: "k", Port: 22, User: "u", KeyPaths: []string{"~/.ssh/a", "b_key"}}}, nil, false},
		{"match warning", "Match host gpu\n  User mu\nHost gpu\n  HostName h\n", "u",
			// User "mu": the library mis-evaluates Match blocks as Host
			// patterns, so the Match block wins first-obtained — exactly the
			// divergence MatchWarning flags.
			[]importer.Candidate{{Name: "gpu", Host: "h", Port: 22, User: "mu"}}, nil, true},
		{"alias in two blocks keeps first", "Host a\n  HostName h1\nHost a\n  HostName h2\n", "u",
			[]importer.Candidate{{Name: "a", Host: "h1", Port: 22, User: "u"}}, nil, false},
		{"negated pattern skipped", "Host !secret\n  HostName h\nHost ok\n  HostName h2\n", "u",
			[]importer.Candidate{{Name: "ok", Host: "h2", Port: 22, User: "u"}},
			map[string]string{"!secret": "wildcard-pattern"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := importer.Parse(writeConfig(t, tc.body), tc.fbUser)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			checkCandidates(t, res.Candidates, tc.want)
			checkSkipped(t, res.Skipped, tc.skipped)
			if res.MatchWarning != tc.matchW {
				t.Errorf("MatchWarning = %v, want %v", res.MatchWarning, tc.matchW)
			}
		})
	}
}

func TestParseMissingFile(t *testing.T) {
	if _, err := importer.Parse(filepath.Join(t.TempDir(), "does-not-exist"), "u"); err == nil {
		t.Fatal("Parse(missing file) err = nil, want error")
	}
}

func TestResolveKeyPaths(t *testing.T) {
	dir := t.TempDir()
	home, _ := os.UserHomeDir()
	got := importer.ResolveKeyPaths([]string{"~/.ssh/id_ed25519", "keys/rel", "/abs/key", "~root/x", "~"}, dir)
	want := []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(dir, "keys", "rel"),
		"/abs/key",
		"~root/x", // non "~"-prefix user form: passed through unresolved
		home,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveKeyPaths =\n%q\nwant\n%q", got, want)
	}
}
