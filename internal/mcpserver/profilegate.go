package mcpserver

// The shared profile gate (ticket 06, 2026-09-21 pilot three-gaps spec).
// Every agent-facing tool answers "is this server in the caller's profile"
// through gateServer / gateServerIn so the deny text — including the
// case-fold neighbor hint — is decided in exactly one place.

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// gateServer enforces the profile iron rule for one server id: it returns nil
// iff serverID is in profileID's grant set, otherwise an ErrNotInProfile
// error (or the store read error, unchanged). When the requested id misses
// the set but case-folds to EXACTLY ONE granted id, the deny text appends a
// neighbor hint naming the correct id — a mistyped-case id self-corrects in
// one round trip. Zero or multiple case-fold hits keep the original text
// (a hint must never mislead). The hint form wraps ErrNotInProfile, so
// errors.Is(err, ErrNotInProfile) holds on every deny and the text keeps the
// "server is not in your profile" prefix the docs/agent-tools.md error table
// matches on. Only ids are compared — never names; ids are already exposed
// by list_servers, so echoing the correct id leaks nothing.
func gateServer(st *store.Store, profileID, serverID string) error {
	allowed, err := st.ServersForProfile(profileID)
	if err != nil {
		return err
	}
	return gateServerIn(allowed, serverID)
}

// gateServerIn is gateServer against an already-fetched grant set —
// relay_file gates BOTH endpoints against one store read so the pair is
// judged on one consistent snapshot.
func gateServerIn(allowed []string, serverID string) error {
	if contains(allowed, serverID) {
		return nil
	}
	hint, hits := "", 0
	for _, id := range allowed {
		if strings.EqualFold(id, serverID) {
			hits++
			hint = id
		}
	}
	if hits == 1 {
		return fmt.Errorf("%w (did you mean %q? server ids are case-sensitive)", ErrNotInProfile, hint)
	}
	return ErrNotInProfile
}
