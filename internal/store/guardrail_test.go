package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckResidualKeysFindsIdFiles(t *testing.T) {
	fakeSSH := t.TempDir()
	writeFile(t, filepath.Join(fakeSSH, "id_rsa"), "PRIVATE")
	writeFile(t, filepath.Join(fakeSSH, "id_ed25519"), "PRIVATE")
	writeFile(t, filepath.Join(fakeSSH, "known_hosts"), "host data")
	writeFile(t, filepath.Join(fakeSSH, "random.txt"), "x")

	got, err := checkResidualKeysIn(fakeSSH)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 key files, got %v", got)
	}
}

func TestCheckResidualKeysEmptyDir(t *testing.T) {
	got, err := checkResidualKeysIn(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0, got %v", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
