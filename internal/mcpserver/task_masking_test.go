package mcpserver

import (
	"ssh-manager-mcp/internal/store"
	"testing"
	"time"
)

func TestBackgroundMaskingSourceRefreshesExternalStoreChangeAfterOneSecond(t *testing.T) {
	const oldValue = "background-old-secret"
	const newValue = "background-new-secret"
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/store.db"
	primaryStore, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer primaryStore.Close()
	externalStore, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer externalStore.Close()
	if err := primaryStore.AddSecret("EXTERNAL_TOKEN", "", []byte(oldValue)); err != nil {
		t.Fatal(err)
	}
	version := primaryStore.SecretValuesForMaskingVersion()

	source := newBackgroundMaskingSource(func() *store.Store { return primaryStore })
	now := time.Unix(1000, 0)
	source.now = func() time.Time { return now }
	initialMasker, err := source.currentMasker()
	if err != nil {
		t.Fatal(err)
	}
	if got := initialMasker.Mask([]byte(newValue), "").Output; string(got) != newValue {
		t.Fatalf("new value was masked before it existed: %q", got)
	}

	if err := externalStore.SetSecretValue("EXTERNAL_TOKEN", []byte(newValue)); err != nil {
		t.Fatal(err)
	}
	if got := primaryStore.SecretValuesForMaskingVersion(); got != version {
		t.Fatalf("primary store version changed from another process: %d != %d", got, version)
	}
	now = now.Add(time.Second + time.Millisecond)
	refreshedMasker, err := source.currentMasker()
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshedMasker.Mask([]byte(newValue), "").Output; string(got) != "{{sshmgr-secret:EXTERNAL_TOKEN}}" {
		t.Fatalf("external value change was not applied: %q", got)
	}
}
