package importer_test

import (
	"testing"

	"ssh-manager-mcp/internal/importer"
)

func TestPlanImport(t *testing.T) {
	cand := func(name, host string, port int, user string) importer.Candidate {
		return importer.Candidate{Name: name, Host: host, Port: port, User: user}
	}
	exist := func(name, host string, port int, user string) importer.ExistingServer {
		return importer.ExistingServer{Name: name, Host: host, Port: port, User: user}
	}
	gpu := cand("gpu", "192.0.2.10", 22, "deploy")
	db := cand("db", "192.0.2.20", 2222, "u")
	web := cand("web", "192.0.2.30", 22, "u")
	cases := []struct {
		name     string
		cands    []importer.Candidate
		existing []importer.ExistingServer
		want     []importer.Candidate
		skips    map[string]string // alias -> reason
	}{
		{"empty vault passes all", []importer.Candidate{gpu, db}, nil,
			[]importer.Candidate{gpu, db}, nil},
		{"name conflict skips regardless of endpoint", []importer.Candidate{gpu, db},
			[]importer.ExistingServer{exist("gpu", "elsewhere", 2200, "other")},
			// gpu's endpoint does NOT match, but names are the vault's unique
			// key — the alias alone blocks the import.
			[]importer.Candidate{db}, map[string]string{"gpu": "existing-name"}},
		{"endpoint conflict skips", []importer.Candidate{gpu, db},
			[]importer.ExistingServer{exist("old", "192.0.2.10", 22, "deploy")},
			[]importer.Candidate{db}, map[string]string{"gpu": "existing-endpoint"}},
		{"name takes precedence over endpoint", []importer.Candidate{gpu, db},
			// one existing row matches gpu by NAME and db by ENDPOINT
			[]importer.ExistingServer{exist("gpu", "192.0.2.20", 2222, "u")},
			nil, map[string]string{"gpu": "existing-name", "db": "existing-endpoint"}},
		{"different port or user is no conflict", []importer.Candidate{gpu, db},
			[]importer.ExistingServer{exist("a", "192.0.2.10", 23, "deploy"), exist("b", "192.0.2.20", 2222, "other")},
			[]importer.Candidate{gpu, db}, nil},
		{"order preserved both sides", []importer.Candidate{gpu, db, web},
			[]importer.ExistingServer{exist("old", "192.0.2.20", 2222, "u")},
			[]importer.Candidate{gpu, web}, map[string]string{"db": "existing-endpoint"}},
		{"no candidates no skips", nil, []importer.ExistingServer{exist("gpu", "h", 22, "u")}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toImport, skipped := importer.PlanImport(tc.cands, tc.existing)
			if len(toImport) != len(tc.want) {
				t.Fatalf("toImport: got %d (%+v), want %d (%+v)", len(toImport), toImport, len(tc.want), tc.want)
			}
			for i := range tc.want {
				g, w := toImport[i], tc.want[i]
				if g.Name != w.Name || g.Host != w.Host || g.Port != w.Port || g.User != w.User {
					t.Errorf("toImport[%d] = %+v, want %+v", i, g, w)
				}
			}
			if len(skipped) != len(tc.skips) {
				t.Fatalf("skipped: got %d (%+v), want %d (%v)", len(skipped), skipped, len(tc.skips), tc.skips)
			}
			for _, s := range skipped {
				if reason, ok := tc.skips[s.Alias]; !ok || reason != s.Reason {
					t.Errorf("skipped %q = %q, want %q (present=%v)", s.Alias, s.Reason, reason, ok)
				}
			}
		})
	}
}
