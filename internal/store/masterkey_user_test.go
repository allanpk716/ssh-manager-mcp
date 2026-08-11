package store

import "testing"

// TestKeyringKeyProvider_UserSlot asserts the User field selects the keychain user slot,
// defaulting to "master-key" when unset (backward compatibility for every existing caller,
// which constructs KeyringKeyProvider{Service: ...} with no User).
func TestKeyringKeyProvider_UserSlot(t *testing.T) {
	def := KeyringKeyProvider{Service: "ssh-manager"}
	if got := def.user(); got != "master-key" {
		t.Fatalf("default user = %q, want %q (backward compat)", got, "master-key")
	}
	cache := KeyringKeyProvider{Service: "ssh-manager", User: "cache-dek"}
	if got := cache.user(); got != "cache-dek" {
		t.Fatalf("cache user = %q, want %q", got, "cache-dek")
	}
	// empty User string must still default (not be treated as a set slot)
	empty := KeyringKeyProvider{Service: "ssh-manager", User: ""}
	if got := empty.user(); got != "master-key" {
		t.Fatalf("empty user = %q, want default master-key", got)
	}
}
