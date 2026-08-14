package tui

import (
	"testing"
)

// vaultProbe and cacheProbe are injectable for tests (production: real paths).
func TestDetectMode_ForceWins(t *testing.T) {
	for _, c := range []struct{ force string; want Mode }{
		{"broker", ModeBroker}, {"client", ModeClient},
	} {
		got, err := DetectModeWith(c.force, func() bool { return false }, func() bool { return false })
		if err != nil || got != c.want {
			t.Fatalf("force=%q: got (%v,%v)", c.force, got, err)
		}
	}
}

func TestDetectMode_Auto(t *testing.T) {
	// vault present → broker
	if m, err := DetectModeWith("", func() bool { return true }, func() bool { return false }); err != nil || m != ModeBroker {
		t.Fatalf("vault: (%v,%v)", m, err)
	}
	// no vault + cache → client
	if m, err := DetectModeWith("", func() bool { return false }, func() bool { return true }); err != nil || m != ModeClient {
		t.Fatalf("cache: (%v,%v)", m, err)
	}
	// neither → guided error
	if _, err := DetectModeWith("", func() bool { return false }, func() bool { return false }); err == nil {
		t.Fatal("neither vault nor cache must error with guidance")
	}
}
