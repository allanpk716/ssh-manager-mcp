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
	blob, err := dpapiProtect(plain, false)
	if err != nil {
		t.Fatalf("dpapiProtect: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("empty blob")
	}
	got, err := dpapiUnprotect(blob, false)
	if err != nil {
		t.Fatalf("dpapiUnprotect: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %x want %x", got, plain)
	}
}

func TestDpapi_EmptyInput(t *testing.T) {
	// DPAPI rejects empty; our wrappers should return a clear error not panic.
	if _, err := dpapiProtect(nil, false); err == nil {
		t.Fatal("dpapiProtect(nil) should error")
	}
}

func TestDpapi_UnprotectCorruptFails(t *testing.T) {
	bad := []byte("not a valid dpapi blob")
	if _, err := dpapiUnprotect(bad, false); err == nil {
		t.Fatal("dpapiUnprotect(corrupt) should error")
	}
}

func TestDpapi_MachineRoundTrip(t *testing.T) {
	plain := make([]byte, 32)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	blob, err := dpapiProtect(plain, true) // machine-scope
	if err != nil {
		t.Fatalf("dpapiProtect(machine): %v", err)
	}
	got, err := dpapiUnprotect(blob, true)
	if err != nil {
		t.Fatalf("dpapiUnprotect(machine): %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("machine round-trip mismatch")
	}
}

// TestDpapi_CrossScopeInteroperable 钉死 spike 2 实证：DPAPI blob 自描述 scope，
// flag 不强制隔离 —— machine-protected blob 用 user flag 也能解（反之亦然）。
// v1 spec 写的"必失败"是错的（codex #6 / pi #7）。
func TestDpapi_CrossScopeInteroperable(t *testing.T) {
	plain := []byte("cross-scope-spike-2")
	machineBlob, err := dpapiProtect(plain, true)
	if err != nil {
		t.Fatalf("dpapiProtect(machine): %v", err)
	}
	// machine blob 用 user flag 解 —— 应成功（spike 2 实测 RESULT=ok）
	got, err := dpapiUnprotect(machineBlob, false)
	if err != nil {
		t.Fatalf("dpapiUnprotect(machine blob, user flag): 期望 spike-2 互通, 实际 err=%v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("cross-scope mismatch")
	}
}
