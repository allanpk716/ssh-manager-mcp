package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	jsonschema "github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

// TestExecContextWindowsEmptyListsAreSchemaValidArrays pins the Windows
// output contract: with no secrets and no shared storages configured, every
// array field in the exec_context output must serialize as an EMPTY ARRAY —
// never null — and the whole structured output must validate against the
// tool's declared output schema.
func TestExecContextWindowsEmptyListsAreSchemaValidArrays(t *testing.T) {
	for _, shell := range []models.ShellType{models.ShellPowerShell, models.ShellWindowsCmd} {
		t.Run(strings.ToLower(string(shell)), func(t *testing.T) {
			st := newStore(t)
			clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
				Password:     "pw",
				SudoPassword: "sudopw",
				Exec: func(command string, _ io.Reader) (string, string, int) {
					if out, ok := fakeCtxBody(command, 1000); ok {
						return out, "", 0
					}
					return "", "unknown context command: " + command + "\n", 1
				},
			}, "sudopw")
			server, err := st.GetServer(serverID)
			if err != nil || server == nil {
				t.Fatalf("read server: %v %v", server, err)
			}
			server.ShellType = shell
			if err := st.UpdateServer(server); err != nil {
				t.Fatal(err)
			}

			result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "exec_context",
				Arguments: map[string]any{"server_id": serverID},
			})
			if err != nil {
				t.Fatalf("exec_context call failed: %v", err)
			}
			if result.IsError {
				t.Fatalf("exec_context failed: %s", toolResultText(result))
			}
			raw := structuredResultJSON(t, result)
			for _, field := range []string{
				"injected_environment_names",
				"uninjected_environment_variables",
				"planned_network_drives",
				"uninjected_network_drives",
			} {
				if !strings.Contains(raw, `"`+field+`":[]`) {
					t.Fatalf("output field %s is not an empty array: %s", field, raw)
				}
			}
			validateAgainstToolOutputSchema(t, clientSession, "exec_context", raw)
		})
	}
}

// TestExecContextProbeFailureKeepsSchemaValidOutput reproduces the Windows
// defect shape: when the context probe itself fails (the POSIX probe command
// failing on a Windows shell), the tool must return that failure as an error
// tool result — NOT let the zero-valued output fail schema validation and
// replace the real error with a transport-level "type null, want array".
func TestExecContextProbeFailureKeepsSchemaValidOutput(t *testing.T) {
	st := newStore(t)
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(command string, _ io.Reader) (string, string, int) {
			return "", "'cat' is not recognized as an internal or external command", 1
		},
	}, "sudopw")
	server, err := st.GetServer(serverID)
	if err != nil || server == nil {
		t.Fatalf("read server: %v %v", server, err)
	}
	server.ShellType = models.ShellPowerShell
	if err := st.UpdateServer(server); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_context",
		Arguments: map[string]any{"server_id": serverID},
	})
	if err != nil {
		t.Fatalf("the probe failure must surface as a tool error result, not an output-schema validation failure: %v", err)
	}
	if !result.IsError {
		t.Fatalf("probe failure must be an error result: %s", toolResultText(result))
	}
	if !strings.Contains(toolResultText(result), "exec_context probe failed") {
		t.Fatalf("error result must carry the probe failure: %s", toolResultText(result))
	}
	raw := structuredResultJSON(t, result)
	for _, field := range []string{"injected_environment_names", "planned_network_drives", "uninjected_network_drives"} {
		if !strings.Contains(raw, `"`+field+`":[]`) {
			t.Fatalf("failure-path output field %s is not an empty array: %s", field, raw)
		}
	}
	validateAgainstToolOutputSchema(t, clientSession, "exec_context", raw)
}

// structuredResultJSON returns the structured output carried by a tool
// result, normalized to JSON text.
func structuredResultJSON(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	switch value := result.StructuredContent.(type) {
	case json.RawMessage:
		return string(value)
	case []byte:
		return string(value)
	case string:
		return value
	default:
		blob, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatalf("marshal structured content: %v", err)
		}
		return string(blob)
	}
}

// validateAgainstToolOutputSchema validates raw JSON against the output
// schema the server actually declares for the tool (the same schema the
// Model Context Protocol SDK validates every structured output against).
func validateAgainstToolOutputSchema(t *testing.T, clientSession *mcp.ClientSession, toolName, rawJSON string) {
	t.Helper()
	toolList, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var declared *mcp.Tool
	for _, tool := range toolList.Tools {
		if tool.Name == toolName {
			declared = tool
			break
		}
	}
	if declared == nil {
		t.Fatalf("tool %s is not registered", toolName)
	}
	schemaJSON, err := json.Marshal(declared.OutputSchema)
	if err != nil {
		t.Fatalf("marshal declared output schema: %v", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatalf("parse declared output schema: %v\n%s", err, schemaJSON)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve declared output schema: %v", err)
	}
	var instance any
	if err := json.Unmarshal([]byte(rawJSON), &instance); err != nil {
		t.Fatalf("decode output for schema validation: %v\n%s", err, rawJSON)
	}
	if err := resolved.Validate(instance); err != nil {
		t.Fatalf("output does not satisfy the declared schema: %v\n%s", err, rawJSON)
	}
}
