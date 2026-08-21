package cli

import (
	"testing"

	"ssh-manager-mcp/internal/store"
)

// Plan 31: --expose-host persists the opt-in bit on add; edit toggles it
// (bare = on, --expose-host=false = off, omitted = keep current).
func TestServersExposeHostFlags(t *testing.T) {
	dbPath, mk := newHintEnv(t)

	runCaptured(t, "servers", "add", "--name", "t1", "--host", "h", "--user", "u", "--expose-host")
	runCaptured(t, "servers", "add", "--name", "t2", "--host", "h", "--user", "u")

	check := func(t *testing.T, name string, want bool) {
		t.Helper()
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		srv, _ := st.GetServerByName(name)
		if srv == nil {
			t.Fatalf("server %s missing", name)
		}
		if srv.ExposeHost != want {
			t.Fatalf("%s ExposeHost = %v, want %v", name, srv.ExposeHost, want)
		}
	}

	check(t, "t1", true)  // add --expose-host
	check(t, "t2", false) // add default

	runCaptured(t, "servers", "edit", "t2", "--expose-host")
	check(t, "t2", true) // edit bare = on

	runCaptured(t, "servers", "edit", "t2", "--expose-host=false")
	check(t, "t2", false) // edit explicit off

	runCaptured(t, "servers", "edit", "t2", "--role", "x")
	check(t, "t2", false) // edit without the flag keeps current (false)

	runCaptured(t, "servers", "edit", "t1", "--role", "x")
	check(t, "t1", true) // edit without the flag keeps current (true)
}
