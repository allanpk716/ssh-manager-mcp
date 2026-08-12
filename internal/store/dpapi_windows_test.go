//go:build windows

package store

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestDpapi_RoundTrip(t *testing.T) {
	plain := make([]byte, 32)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	blob, err := dpapiProtect(plain)
	if err != nil {
		t.Fatalf("dpapiProtect: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("empty blob")
	}
	got, err := dpapiUnprotect(blob)
	if err != nil {
		t.Fatalf("dpapiUnprotect: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %x want %x", got, plain)
	}
}

func TestDpapi_EmptyInput(t *testing.T) {
	// DPAPI rejects empty; our wrappers should return a clear error not panic.
	if _, err := dpapiProtect(nil); err == nil {
		t.Fatal("dpapiProtect(nil) should error")
	}
}

func TestDpapi_UnprotectCorruptFails(t *testing.T) {
	bad := []byte("not a valid dpapi blob")
	if _, err := dpapiUnprotect(bad); err == nil {
		t.Fatal("dpapiUnprotect(corrupt) should error")
	}
}
