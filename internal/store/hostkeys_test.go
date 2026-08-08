package store

import (
	"bytes"
	"testing"
)

func TestHostKeySaveGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetHostKey("gpu.example")
	if err != nil || got != nil {
		t.Fatalf("absent: got %v, %v", got, err)
	}
	blob := []byte{1, 2, 3, 4}
	if err := s.SaveHostKey("gpu.example", blob); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetHostKey("gpu.example")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("got %v want %v", got, blob)
	}
	// upsert: saving again replaces
	if err := s.SaveHostKey("gpu.example", []byte{9, 9}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetHostKey("gpu.example")
	if !bytes.Equal(got, []byte{9, 9}) {
		t.Fatal("upsert did not replace")
	}
}
