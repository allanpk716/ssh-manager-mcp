package clientops

import (
	"testing"

	"ssh-manager-mcp/internal/store"
)

// withEnv sets env vars for the test and restores on cleanup. Local copy of
// internal/cli's helper (moved-test rebuild; see the Task 1 brief).
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// withDEK swaps the DekProvider seam to a fresh in-memory provider for the
// test, returning it so the test can assert the DEK persisted there (not the
// real keychain). Local copy of internal/cli's helper, pointed at this
// package's exported seam.
func withDEK(t *testing.T) *store.MemKeyProvider {
	t.Helper()
	mem := &store.MemKeyProvider{}
	prev := DekProvider
	DekProvider = func() store.KeyProvider { return mem }
	t.Cleanup(func() { DekProvider = prev })
	return mem
}
