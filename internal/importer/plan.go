package importer

import "fmt"

// ExistingServer is one server already in the vault, reduced to the fields
// import conflicts are judged on. The CLI import (T8) and the TUI import flow
// (T10) both feed this from store.ListServers.
type ExistingServer struct {
	Name string
	Host string
	Port int
	User string
}

// SkippedReason records one candidate excluded by the vault-conflict rules.
// Reason is "existing-name" (alias already taken by a vault server) or
// "existing-endpoint" (host:port:user triple already in the vault) — a
// distinct phase from the parse-time Skipped reasons, hence a distinct type.
type SkippedReason struct{ Alias, Reason string }

// PlanImport filters parsed candidates against the servers already in the
// vault. A candidate whose Name matches an existing server is skipped
// ("existing-name" — names are the vault's unique key); otherwise a candidate
// whose host:port:user triple matches an existing server is skipped
// ("existing-endpoint"). Everything else passes through as toImport. Both
// outputs preserve input order; together they partition cands.
//
// Candidates are assumed to come from Parse, which guarantees unique names and
// unique host:port:user triples WITHIN one batch — so only the vault side can
// conflict, and PlanImport needs no batch-internal state.
func PlanImport(cands []Candidate, existing []ExistingServer) (toImport []Candidate, skipped []SkippedReason) {
	names := make(map[string]bool, len(existing))
	occ := make(map[string]bool, len(existing))
	for _, e := range existing {
		names[e.Name] = true
		occ[fmt.Sprintf("%s:%d:%s", e.Host, e.Port, e.User)] = true
	}
	for _, c := range cands {
		switch {
		case names[c.Name]:
			skipped = append(skipped, SkippedReason{c.Name, "existing-name"})
		case occ[fmt.Sprintf("%s:%d:%s", c.Host, c.Port, c.User)]:
			skipped = append(skipped, SkippedReason{c.Name, "existing-endpoint"})
		default:
			toImport = append(toImport, c)
		}
	}
	return toImport, skipped
}
