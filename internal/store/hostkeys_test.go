package store

import (
	"bytes"
	"net"
	"strconv"
	"testing"

	"ssh-manager-mcp/internal/testsshd"
)

func TestHostKeySaveGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetHostKey("gpu.example", 22)
	if err != nil || got != nil {
		t.Fatalf("absent: got %v, %v", got, err)
	}
	blob := []byte{1, 2, 3, 4}
	if err := s.SaveHostKey("gpu.example", 22, blob); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetHostKey("gpu.example", 22)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("got %v want %v", got, blob)
	}
	// upsert: saving again replaces
	if err := s.SaveHostKey("gpu.example", 22, []byte{9, 9}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetHostKey("gpu.example", 22)
	if !bytes.Equal(got, []byte{9, 9}) {
		t.Fatal("upsert did not replace")
	}
}

func TestHostKeysKeyedByHostPort(t *testing.T) {
	// Two testsshd instances on different ports get distinct host keys (testsshd
	// generates a fresh key per Start). Storing both against the SAME host must not
	// clobber — this proves host:port keying (legacy host-only keying would collide).
	addr1, hk1, cleanup1 := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup1()
	addr2, hk2, cleanup2 := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup2()

	h1, p1, err := net.SplitHostPort(addr1)
	if err != nil {
		t.Fatal(err)
	}
	h2, p2, err := net.SplitHostPort(addr2)
	if err != nil {
		t.Fatal(err)
	}
	if hk1.Marshal() == nil || bytes.Equal(hk1.Marshal(), hk2.Marshal()) {
		t.Fatal("test servers must have distinct host keys for this test")
	}

	s := newTestStore(t)
	port1, _ := strconv.Atoi(p1)
	port2, _ := strconv.Atoi(p2)
	if err := s.SaveHostKey(h1, port1, hk1.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHostKey(h2, port2, hk2.Marshal()); err != nil {
		t.Fatal(err)
	}

	got1, err := s.GetHostKey(h1, port1)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetHostKey(h2, port2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, hk1.Marshal()) || !bytes.Equal(got2, hk2.Marshal()) {
		t.Fatal("host keys clobbered across ports — keying is not host:port")
	}
}
