package store

import (
	"errors"
	"testing"
)

// TestMemKeyProviderDelete pins the optional Delete capability QuarantineCache
// consumes via its interface{ Delete() error } assertion (Plan 37 T2): after
// Set, Get serves the key; Delete returns nil; Get then reports ErrNotFound —
// so a removal/regression of the method shows up here, not just as a silent
// "ok(no-delete-provider)" degradation downstream.
func TestMemKeyProviderDelete(t *testing.T) {
	m := &MemKeyProvider{}
	if err := m.Set([]byte("k")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(); err != nil {
		t.Fatalf("after Set: %v", err)
	}
	if err := m.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete: %v, want ErrNotFound", err)
	}
}
