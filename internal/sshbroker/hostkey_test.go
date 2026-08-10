package sshbroker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"ssh-manager-mcp/internal/testsshd"
)

type fakeHostKeyStore struct {
	keys map[string][]byte // keyed by host:port
}

func (f *fakeHostKeyStore) GetHostKey(host string, port int) ([]byte, error) {
	return f.keys[fmt.Sprintf("%s:%d", host, port)], nil
}
func (f *fakeHostKeyStore) SaveHostKey(host string, port int, k []byte) error {
	if f.keys == nil {
		f.keys = map[string][]byte{}
	}
	f.keys[fmt.Sprintf("%s:%d", host, port)] = k
	return nil
}

func TestHostKeyTOFURecordsThenVerifies(t *testing.T) {
	st := &fakeHostKeyStore{}
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()

	cb, err := HostKeyTOFU(st, "h", portOf(addr))
	if err != nil {
		t.Fatal(err)
	}
	// first connect: records host key
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err != nil {
		t.Fatalf("first connect (TOFU): %v", err)
	}
	defer cli.Close()
	if len(st.keys) != 1 {
		t.Fatalf("expected 1 recorded key, got %d", len(st.keys))
	}

	// second connect: verifies, succeeds
	cb2, _ := HostKeyTOFU(st, "h", portOf(addr))
	cli2, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb2)
	if err != nil {
		t.Fatalf("second connect (verify): %v", err)
	}
	defer cli2.Close()
}

func TestHostKeyMismatchRejected(t *testing.T) {
	// Pre-seed the store with a key that differs from the test server's real key,
	// so the callback must reject the connection as a MITM.
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := &fakeHostKeyStore{keys: map[string][]byte{fmt.Sprintf("h:%d", portOf(addr)): []byte("stale-different-key")}}
	cb, _ := HostKeyTOFU(st, "h", portOf(addr))
	_, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err == nil {
		t.Fatal("mismatched host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
}
