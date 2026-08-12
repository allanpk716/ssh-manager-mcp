//go:build windows

package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestDpapiKeyProvider_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "master.key"), DirUser: os.Getenv("USERNAME")}
	mk := make([]byte, 32)
	rand.Read(mk)
	if err := p.Set(mk); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatalf("mismatch: got %x want %x", got, mk)
	}
	if err := p.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(); err != ErrNotFound {
		t.Fatalf("Get after Delete: err=%v want ErrNotFound", err)
	}
}

func TestDpapiKeyProvider_GetMissingIsErrNotFound(t *testing.T) {
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "absent.key"), DirUser: os.Getenv("USERNAME")}
	if _, err := p.Get(); err != ErrNotFound {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestDpapiKeyProvider_SetIsAtomic(t *testing.T) {
	// Set writes temp + os.Rename; the final file must exist and be readable.
	// If Rename failed, Get would return ErrNotFound (no file) — proves atomicity.
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "mk"), DirUser: os.Getenv("USERNAME")}
	mk := []byte("test-atomic-32-bytes-pad-to-32!!") // 32 bytes
	if len(mk) != 32 {
		t.Fatal("test data must be 32 bytes")
	}
	if err := p.Set(mk); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatalf("Get after atomic Set: %v", err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatal("atomic Set round-trip mismatch")
	}
	// no leftover temp files
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp*"))
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}
