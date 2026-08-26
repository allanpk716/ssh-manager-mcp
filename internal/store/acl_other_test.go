//go:build !windows

package store

import "testing"

func TestInspectFileACL_StubUnsupported(t *testing.T) {
	rep, err := InspectFileACL("/dev/null/whatever")
	if err != nil {
		t.Fatalf("stub must not error: %v", err)
	}
	if rep.Supported {
		t.Fatal("stub must report Supported=false")
	}
	if rep.TooLoose() {
		t.Fatal("non-supported report must never be loose (Supported guard pin)")
	}
}
