package mcpserver

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

func TestExecContextShowsEnvironmentInjectionPlanForEachLayer(t *testing.T) {
	const secretValue = "context-injected-secret-value"
	const zeroByteValue = "context-zero-byte-value\x00"
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("CONTEXT_TOKEN", "", []byte(secretValue), "CONTEXT_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("ZERO_CONTEXT_TOKEN", "", []byte(zeroByteValue), "ZERO_CONTEXT_TOKEN"); err != nil {
		t.Fatal(err)
	}

	var stdinMu sync.Mutex
	var standardInputs [][]byte
	connections := &atomic.Int64{}
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Connections:  connections,
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			content, err := io.ReadAll(stdin)
			if err != nil {
				return "", err.Error(), 1
			}
			stdinMu.Lock()
			standardInputs = append(standardInputs, content)
			stdinMu.Unlock()
			if out, ok := fakeCtxBody(command, 1000); ok {
				return out, "", 0
			}
			return "", "unknown context command: " + command + "\n", 1
		},
	}, "sudopw")
	if err := st.AllowSecrets([]string{"CONTEXT_TOKEN", "ZERO_CONTEXT_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(serverID)
	if err != nil || server == nil {
		t.Fatalf("read server: %v %v", server, err)
	}
	server.ShellType = models.ShellPOSIX
	if err := st.UpdateServer(server); err != nil {
		t.Fatal(err)
	}

	assertLayer := func(sudo bool) {
		t.Helper()
		result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "exec_context",
			Arguments: map[string]any{"server_id": serverID, "sudo": sudo},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("exec_context(sudo=%v) failed: %+v", sudo, result.Content)
		}
		var output struct {
			Elevated                       bool     `json:"elevated"`
			InjectedEnvironmentNames       []string `json:"injected_environment_names"`
			UninjectedEnvironmentVariables []struct {
				EnvironmentName string `json:"environment_name"`
				Reason          string `json:"reason"`
			} `json:"uninjected_environment_variables"`
		}
		unmarshalToolJSON(t, result, &output)
		if output.Elevated != sudo {
			t.Fatalf("exec_context(sudo=%v) elevated = %v", sudo, output.Elevated)
		}
		if len(output.InjectedEnvironmentNames) != 1 || output.InjectedEnvironmentNames[0] != "CONTEXT_TOKEN" {
			t.Fatalf("exec_context(sudo=%v) injected names = %#v", sudo, output.InjectedEnvironmentNames)
		}
		if len(output.UninjectedEnvironmentVariables) != 1 {
			t.Fatalf("exec_context(sudo=%v) uninjected variables = %#v", sudo, output.UninjectedEnvironmentVariables)
		}
		got := output.UninjectedEnvironmentVariables[0]
		if got.EnvironmentName != "ZERO_CONTEXT_TOKEN" || got.Reason != "值含空字节" {
			t.Fatalf("exec_context(sudo=%v) uninjected variable = %+v", sudo, got)
		}
		raw := toolResultText(result)
		if strings.Contains(raw, secretValue) || strings.Contains(raw, zeroByteValue) {
			t.Fatalf("exec_context(sudo=%v) leaked a secret value: %s", sudo, raw)
		}
	}
	assertLayer(false)
	assertLayer(true)
	if connections.Load() != 2 {
		t.Fatalf("connections = %d, want one exec_context connection per layer", connections.Load())
	}

	stdinMu.Lock()
	gotInputs := append([][]byte(nil), standardInputs...)
	stdinMu.Unlock()
	for i, input := range gotInputs {
		if strings.Contains(string(input), secretValue) || strings.Contains(string(input), zeroByteValue) {
			t.Fatalf("exec_context standard input %d leaked a secret value: %q", i, input)
		}
	}
}

func TestExecContextWindowsSudoDoesNotInjectEnvironment(t *testing.T) {
	const secretValue = "windows-sudo-context-secret-value"
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("WINDOWS_CONTEXT_TOKEN", "", []byte(secretValue), "WINDOWS_CONTEXT_TOKEN"); err != nil {
		t.Fatal(err)
	}
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(command string, _ io.Reader) (string, string, int) {
			if out, ok := fakeCtxBody(command, 0); ok {
				return out, "", 0
			}
			return "", "unknown context command: " + command + "\n", 1
		},
	}, "sudopw")
	if err := st.AllowSecrets([]string{"WINDOWS_CONTEXT_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
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
		Arguments: map[string]any{"server_id": serverID, "sudo": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_context failed: %+v", result.Content)
	}
	var output struct {
		InjectedEnvironmentNames       []string `json:"injected_environment_names"`
		UninjectedEnvironmentVariables []struct {
			EnvironmentName string `json:"environment_name"`
			Reason          string `json:"reason"`
		} `json:"uninjected_environment_variables"`
	}
	unmarshalToolJSON(t, result, &output)
	if len(output.InjectedEnvironmentNames) != 0 {
		t.Fatalf("injected names = %#v, want none for Windows sudo", output.InjectedEnvironmentNames)
	}
	if len(output.UninjectedEnvironmentVariables) != 1 ||
		output.UninjectedEnvironmentVariables[0].EnvironmentName != "WINDOWS_CONTEXT_TOKEN" ||
		output.UninjectedEnvironmentVariables[0].Reason != "Windows 服务器的提权层暂不支持环境变量注入" {
		t.Fatalf("uninjected variables = %#v", output.UninjectedEnvironmentVariables)
	}
}

func TestSecretEnvironmentInjectionWindowsSudoDoesNotAppendWarning(t *testing.T) {
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("WINDOWS_EXEC_TOKEN", "", []byte("windows-sudo-exec-secret-value"), "WINDOWS_EXEC_TOKEN"); err != nil {
		t.Fatal(err)
	}
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(_ string, stdin io.Reader) (string, string, int) {
			_, _ = io.ReadAll(stdin)
			return "ran", "", 0
		},
	}, "sudopw")
	if err := st.AllowSecrets([]string{"WINDOWS_EXEC_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(serverID)
	if err != nil || server == nil {
		t.Fatalf("read server: %v %v", server, err)
	}
	server.ShellType = models.ShellPowerShell
	if err := st.UpdateServer(server); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": "Write-Output ran", "sudo": true},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_command failed: err=%v result=%+v", err, result.Content)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "ran" {
		t.Fatalf("stdout = %q, want no unsupported-privileged-layer warning", output.Stdout)
	}
}
