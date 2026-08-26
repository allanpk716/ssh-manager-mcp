package clientops

import (
	"errors"

	"ssh-manager-mcp/internal/store"
)

// loadOrCreateDEK returns the cache DEK from the keychain, generating + storing it on first pull.
// On subsequent pulls the existing DEK is reused, so cache.bin stays decryptable across pulls.
// Takes an instance name ("" = default instance).
func loadOrCreateDEK(instance string) ([]byte, error) {
	kp := DekProvider(instance)
	dek, err := kp.Get()
	if err == nil {
		return dek, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	dek, err = store.GenerateMasterKey()
	if err != nil {
		return nil, err
	}
	if err := kp.Set(dek); err != nil {
		return nil, err
	}
	return dek, nil
}

// loadDEK returns the cache DEK without creating it (status / mcp --cache). A missing DEK
// surfaces as store.ErrNotFound — the caller reports "run cache pull first".
// Takes an instance name ("" = default instance).
func loadDEK(instance string) ([]byte, error) {
	return DekProvider(instance).Get()
}
