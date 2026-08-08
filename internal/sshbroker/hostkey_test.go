package sshbroker

import (
	"errors"
	"testing"

	"ssh-manager-mcp/internal/testsshd"
)

type fakeHostKeyStore struct {
	keys map[string][]byte
}

func (f *fakeHostKeyStore) GetHostKey(host string) ([]byte, error) { return f.keys[host], nil }
func (f *fakeHostKeyStore) SaveHostKey(host string, k []byte) error {
	if f.keys == nil {
		f.keys = map[string][]byte{}
	}
	f.keys[host] = k
	return nil
}

func TestHostKeyTOFURecordsThenVerifies(t *testing.T) {
	st := &fakeHostKeyStore{}
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()

	cb, err := HostKeyTOFU(st, "h")
	if err != nil {
		t.Fatal(err)
	}
	// first connect: records host key
	cli, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err != nil {
		t.Fatalf("first connect (TOFU): %v", err)
	}
	defer cli.Close()
	if len(st.keys) != 1 {
		t.Fatalf("expected 1 recorded key, got %d", len(st.keys))
	}

	// second connect: verifies, succeeds
	cb2, _ := HostKeyTOFU(st, "h")
	cli2, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb2)
	if err != nil {
		t.Fatalf("second connect (verify): %v", err)
	}
	defer cli2.Close()
}

func TestHostKeyMismatchRejected(t *testing.T) {
	// Pre-seed the store with a key that differs from the test server's real key,
	// so the callback must reject the connection as a MITM.
	st := &fakeHostKeyStore{keys: map[string][]byte{"h": []byte("stale-different-key")}}
	cb, _ := HostKeyTOFU(st, "h")
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	_, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err == nil {
		t.Fatal("mismatched host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
}
