package mcpserver

import (
	"strings"
	"testing"
)

// TestBrokerToolsNoAuditFace (spec §6 matrix 8): audit data must NEVER be
// reachable through the MCP tool surface — audit rows carry other agents' full
// command text and may contain secrets (backlog #16 owner ruling). BrokerTools
// is the single source the server registers tools from (e2e_test pins the
// tools/list == BrokerTools set equality), so guarding it here guards the wire.
func TestBrokerToolsNoAuditFace(t *testing.T) {
	for _, name := range BrokerTools {
		if strings.Contains(strings.ToLower(name), "audit") {
			t.Fatalf("BrokerTools must not expose audit data over MCP: %q", name)
		}
	}
}
