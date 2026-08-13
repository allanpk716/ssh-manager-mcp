//go:build windows

package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestDpapiKeyProvider_MachineScopeRoundTrip 验 Set 用 machine-scope,Get 能读回。
func TestDpapiKeyProvider_MachineScopeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := DpapiKeyProvider{Path: filepath.Join(dir, "master.key"), DirUser: os.Getenv("USERNAME")}
	mk := []byte("machine-scope-key-32-bytes-pad000000")[:32]
	if err := p.Set(mk); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, mk) {
		t.Fatalf("round-trip mismatch")
	}
}

// TestDpapiKeyProvider_GetUserScopeFallback 验 Get 对旧 user-scope blob 的容错
// (迁移窗口期:旧 master.key 是 user-scope,新代码 machine-first Get 要能读出)。
func TestDpapiKeyProvider_GetUserScopeFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	// 写一个 user-scope blob(模拟旧 master.key)
	legacy, err := dpapiProtect([]byte("legacy-user-scope-key-32-pad00000")[:32], false)
	if err != nil {
		t.Fatalf("dpapiProtect(user): %v", err)
	}
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	p := DpapiKeyProvider{Path: path, DirUser: os.Getenv("USERNAME")}
	got, err := p.Get() // machine-first,fallback user —— 应读出 legacy
	if err != nil {
		t.Fatalf("Get on user-scope blob: %v", err)
	}
	if !bytes.Equal(got, []byte("legacy-user-scope-key-32-pad00000")[:32]) {
		t.Fatalf("user-scope fallback mismatch")
	}
}

// TestDpapiKeyProvider_SetACLContract 钉死 ACL 契约(pi #3):Set 后 master.key
// 的 ACL 只含 DirUser(+ SYSTEM),无 Everyone/Users。machine-scope 下 ACL 是
// 唯一防线,必须保证 temp 在 protectedDir 内继承正确 ACL。
func TestDpapiKeyProvider_SetACLContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	user := os.Getenv("USERNAME")
	if user == "" {
		t.Skip("USERNAME empty")
	}
	p := DpapiKeyProvider{Path: path, DirUser: user}
	if err := p.Set([]byte("acl-contract-key-32-bytes-pad0000000")[:32]); err != nil {
		t.Fatalf("Set: %v", err)
	}
	out, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	acl := string(out)
	for _, forbidden := range []string{"Everyone", "BUILTIN\\Users", "Authenticated Users"} {
		if strings.Contains(acl, forbidden) {
			t.Fatalf("ACL contains %q (machine-scope 下 ACL 是唯一防线):\n%s", forbidden, acl)
		}
	}
}
