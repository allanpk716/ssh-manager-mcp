package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMaskingRelayProgressOutput(t *testing.T) {
	env := newRelayE2EEnv(t)
	source := env.root + "/masking/source.txt"
	destination := env.root + "/masking/destination/relay-secret-path.bin"
	relayMkFile(t, source, "relay progress masking")
	if err := env.st.AddSecret("RELAY_PATH", "", []byte(destination)); err != nil {
		t.Fatal(err)
	}
	session := env.openSession(t)
	start := relayE2ECallOK(t, session, env.input("", source, destination, true))

	stdout, status, errText := relayE2EPoll(t, session, start.TaskID, 30*time.Second)
	if status != bgStatusDone {
		t.Fatalf("relay status = %q, error = %q", status, errText)
	}
	if strings.Contains(stdout, destination) {
		t.Fatalf("relay progress leaked the registered path:\n%s", stdout)
	}
	if !strings.Contains(stdout, "{{sshmgr-secret:RELAY_PATH}}") {
		t.Fatalf("relay progress does not contain the expected placeholder:\n%s", stdout)
	}
	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_output",
		Arguments: map[string]any{"task_id": start.TaskID},
	})
	if err != nil || result.IsError {
		t.Fatalf("final exec_output failed: err=%v result=%+v", err, result.Content)
	}
	var output BgReadOutput
	unmarshalToolJSON(t, result, &output)
	if len(output.Masked) != 1 || output.Masked[0] != "RELAY_PATH" {
		t.Fatalf("relay masked names = %#v", output.Masked)
	}
}
