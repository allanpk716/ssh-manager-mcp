package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// newRootForTest mirrors the pattern every internal/cli test uses: build the
// root *cobra.Command via NewRootCmd() WITHOUT calling Execute. Existing tests
// (cli_smoke_test.go, root_test.go, mcp_test.go) all call NewRootCmd() inline;
// this thin wrapper exists so the brief's test code reads as written and gives
// one place to evolve if the root-builder ever needs test-only wiring.
func newRootForTest(t *testing.T) *cobra.Command {
	t.Helper()
	return NewRootCmd()
}

func TestServeCmd_RegisteredWithFlags(t *testing.T) {
	root := newRootForTest(t) // contract: returns the root cobra command, no Execute
	srv, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatal("serve subcommand not registered:", err)
	}
	for _, flag := range []string{"addr", "tls-cert", "tls-key"} {
		if srv.Flags().Lookup(flag) == nil {
			t.Errorf("serve missing --%s flag", flag)
		}
	}
}
