package store

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileKeyProvider_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := FileKeyProvider{Path: filepath.Join(dir, "mk.plain")}
	mk := make([]byte, 32)
	rand.Read(mk)
	if err := p.Set(mk); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatal("mismatch")
	}
	if err := p.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v want ErrNotFound", err)
	}
}

func TestFileKeyProvider_GetMissingIsErrNotFound(t *testing.T) {
	p := FileKeyProvider{Path: filepath.Join(t.TempDir(), "absent")}
	if _, err := p.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

// TestFileKeyProvider_SetIsAtomic asserts the temp+rename path leaves no
// leftover temp files after a successful Set (corruption-resistance guarantee
// for this last-resort fallback provider — same rationale as DpapiKeyProvider).
func TestFileKeyProvider_SetIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := FileKeyProvider{Path: filepath.Join(dir, "mk.plain")}
	mk := make([]byte, 32)
	copy(mk, []byte("atomic-key"))
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
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}

// TestFileKeyProvider_DeleteMissingIsNoOp asserts Delete is idempotent: a
// missing file is not an error (matches DpapiKeyProvider semantics).
func TestFileKeyProvider_DeleteMissingIsNoOp(t *testing.T) {
	p := FileKeyProvider{Path: filepath.Join(t.TempDir(), "absent")}
	if err := p.Delete(); err != nil {
		t.Fatalf("Delete on missing file: want nil, got %v", err)
	}
}

// TestFileKeyProvider_SetCreatesParentDir asserts Set works when the parent
// directory does not yet exist (UserConfigDir/ssh-manager on first run).
func TestFileKeyProvider_SetCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	p := FileKeyProvider{Path: filepath.Join(dir, "nested", "deep", "mk.plain")}
	mk := make([]byte, 32)
	copy(mk, []byte("dir-create-key"))
	if err := p.Set(mk); err != nil {
		t.Fatalf("Set with missing parent dir: %v", err)
	}
	if _, err := os.Stat(p.Path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}
