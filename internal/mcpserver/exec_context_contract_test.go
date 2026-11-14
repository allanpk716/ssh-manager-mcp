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
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestExecContextWindowsEmptyListsAreSchemaValidArrays 钉住 Windows 输出契
// 约：不下发上下文探测命令，数组字段是数组而不是 null，并返回已算出的
// 网络盘符预览。
func TestExecContextWindowsEmptyListsAreSchemaValidArrays(t *testing.T) {
	for _, shell := range []models.ShellType{models.ShellPowerShell, models.ShellWindowsCmd} {
		t.Run(strings.ToLower(string(shell)), func(t *testing.T) {
			st := newStore(t)
			probeCommandSent := false
			clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
				Password:     "pw",
				SudoPassword: "sudopw",
				Exec: func(command string, _ io.Reader) (string, string, int) {
					probeCommandSent = true
					return "", "不应下发的上下文探测命令：" + command + "\n", 1
				},
			}, "sudopw")
			server, err := st.GetServer(serverID)
			if err != nil || server == nil {
				t.Fatalf("读取服务器条目失败：%v %v", server, err)
			}
			server.ShellType = shell
			if err := st.UpdateServer(server); err != nil {
				t.Fatal(err)
			}
			addExecContextDrivePreview(t, st, server)

			result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "exec_context",
				Arguments: map[string]any{"server_id": serverID},
			})
			if err != nil {
				t.Fatalf("调用 exec_context 失败：%v", err)
			}
			if result.IsError {
				t.Fatalf("exec_context failed: %s", toolResultText(result))
			}
			if probeCommandSent {
				t.Fatal("Windows exec_context 不应下发上下文探测命令")
			}
			raw := structuredResultJSON(t, result)
			var output ExecContextOutput
			if err := json.Unmarshal([]byte(raw), &output); err != nil {
				t.Fatalf("解析 exec_context 输出失败：%v：%s", err, raw)
			}
			if len(output.PlannedNetworkDrives) != 1 ||
				output.PlannedNetworkDrives[0].Share != "datasets" ||
				output.PlannedNetworkDrives[0].Drive != "S" {
				t.Fatalf("Windows 盘符预览不正确：%+v", output.PlannedNetworkDrives)
			}
			if len(output.UninjectedNetworkDrives) != 0 {
				t.Fatalf("不应有未注入盘符警告：%+v", output.UninjectedNetworkDrives)
			}
			for _, field := range []string{
				"injected_environment_names",
				"uninjected_environment_variables",
				"uninjected_network_drives",
			} {
				if !strings.Contains(raw, `"`+field+`":[]`) {
					t.Fatalf("字段 %s 不是空数组：%s", field, raw)
				}
			}
			validateAgainstToolOutputSchema(t, clientSession, "exec_context", raw)
		})
	}
}

// TestExecContextProbeFailureKeepsSchemaValidOutput 覆盖探测失败路径：失败
// 必须作为工具错误结果返回，不能让空输出触发结构校验错误并掩盖真实原
// 因；同时保留探测前已经算出的注入预览。
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
		t.Fatalf("读取服务器条目失败：%v %v", server, err)
	}
	server.ShellType = models.ShellPOSIX
	if err := st.UpdateServer(server); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName(
		"CONTEXT_TOKEN", "", []byte("context-value"), "CONTEXT_TOKEN", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"CONTEXT_TOKEN"}, []string{server.Name}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	addExecContextDrivePreview(t, st, server)

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_context",
		Arguments: map[string]any{"server_id": serverID},
	})
	if err != nil {
		t.Fatalf("探测失败必须作为工具错误结果返回，而不是结构校验失败：%v", err)
	}
	if !result.IsError {
		t.Fatalf("探测失败必须是错误结果：%s", toolResultText(result))
	}
	if !strings.Contains(toolResultText(result), "exec_context probe failed") {
		t.Fatalf("错误结果必须携带探测失败原因：%s", toolResultText(result))
	}
	raw := structuredResultJSON(t, result)
	for _, field := range []string{"planned_network_drives", "uninjected_network_drives"} {
		if !strings.Contains(raw, `"`+field+`":[]`) {
			t.Fatalf("失败路径的字段 %s 不是空数组：%s", field, raw)
		}
	}
	var output ExecContextOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("解析失败路径输出失败：%v：%s", err, raw)
	}
	if len(output.InjectedEnvironmentNames) != 1 || output.InjectedEnvironmentNames[0] != "CONTEXT_TOKEN" {
		t.Fatalf("探测失败丢弃了已算出的注入预览：%#v", output.InjectedEnvironmentNames)
	}
	validateAgainstToolOutputSchema(t, clientSession, "exec_context", raw)
}

func TestExecContextConfigurationFailureKeepsWindowsDrivePreview(t *testing.T) {
	st := newStore(t)
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(string, io.Reader) (string, string, int) {
			return "", "不应下发的上下文探测命令", 1
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
	addExecContextDrivePreview(t, st, server)
	server.SudoCredentialID = ""
	if err := st.UpdateServer(server); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_context",
		Arguments: map[string]any{"server_id": serverID, "sudo": true},
	})
	if err != nil {
		t.Fatalf("调用 exec_context 失败：%v", err)
	}
	if !result.IsError || !strings.Contains(toolResultText(result), "sudo not configured") {
		t.Fatalf("sudo 配置失败必须保持可见：%s", toolResultText(result))
	}
	raw := structuredResultJSON(t, result)
	var output ExecContextOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("解析 exec_context 输出失败：%v：%s", err, raw)
	}
	if len(output.UninjectedNetworkDrives) != 1 || output.UninjectedNetworkDrives[0].Drive != "S" {
		t.Fatalf("配置失败丢弃了盘符预览：%+v", output.UninjectedNetworkDrives)
	}
	if output.PlannedNetworkDrives == nil {
		t.Fatal("配置失败把计划盘符列表变成了 null")
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

func addExecContextDrivePreview(t *testing.T, st *store.Store, server *models.Server) {
	t.Helper()
	if _, err := st.AddDevice("exec-context-NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("exec-context-site", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "exec-context-physical-network", Type: models.NetworkPhysical, SiteName: "exec-context-site",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership(
		"exec-context-NAS", "exec-context-physical-network", "192.0.2.10",
	); err != nil {
		t.Fatal(err)
	}
	targetDevice, err := st.AddDevice("exec-context-target", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership(
		"exec-context-target", "exec-context-physical-network", "192.0.2.20",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MoveServerToDevice(server.Name, targetDevice.Name); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(
		"EXEC_CONTEXT_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"EXEC_CONTEXT_MOUNT_PASSWORD"}, []string{server.Name}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "exec-context-NAS", User: "mount-user",
		PasswordSecret: "EXEC_CONTEXT_MOUNT_PASSWORD", Drive: "S",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
}
