package cli

// Plan 51 T8 (spec §9): the mcp --cache wiring — cacheMetaForwarderFor builds
// the metadata forwarder at the same --instance resolver as the pin forwarder
// (one CacheCred read), and a missing credential yields the no-capability
// flavor whose Edit fails closed with the §2.1 main message.

import (
	"strings"
	"testing"
)

func TestCacheMetaForwarderFor_MissingCredentialFailsClosed(t *testing.T) {
	// No cache.auth.json for a nonexistent instance: the no-capability
	// flavor, NOT an error and NOT a silent local-write capability.
	f, err := cacheMetaForwarderFor("no-such-instance-plan51")
	if err != nil {
		t.Fatalf("missing credential must construct the no-capability flavor, got %v", err)
	}
	if f == nil {
		t.Fatal("nil forwarder — the tool face requires a non-nil editor")
	}
	_, err = f.EditMeta("srv-1", 0, map[string]*string{"role": ptrStr("x")})
	if err == nil || !strings.Contains(err.Error(), "could not reach the broker") {
		t.Fatalf("no-cred edit must fail closed with the main message, got %v", err)
	}
	if !strings.Contains(err.Error(), "NOT applied anywhere") {
		t.Fatalf("the main message must state the nothing-applied guarantee: %v", err)
	}
}

func ptrStr(v string) *string { return &v }
