package mcpserver

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestSecretEnvironmentInjectionExecCommandSudo(t *testing.T) {
	requirePOSIXRemoteSimulation(t)
	const secretValue = "sudo-injected-secret-%-backslash-\\-value\n"
	const userCommand = `printf '%s\n' "$ELEVATED_API_TOKEN"`

	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("ELEVATED_API_TOKEN", "", []byte(secretValue), "ELEVATED_API_TOKEN", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	var commandMu sync.Mutex
	var remoteCommand string
	var remoteStdin []byte
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", "read standard input: " + err.Error(), 1
			}
			commandMu.Lock()
			remoteCommand, remoteStdin = command, append([]byte(nil), stdinBytes...)
			commandMu.Unlock()

			return runRecordedPOSIXCommand(command, stdinBytes)
		},
	}, "sudopw")
	if err := st.AllowSecrets([]string{"ELEVATED_API_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": userCommand, "sudo": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout string   `json:"stdout"`
		Masked []string `json:"masked"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "{{sshmgr-secret:ELEVATED_API_TOKEN}}\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
	if len(output.Masked) != 1 || output.Masked[0] != "ELEVATED_API_TOKEN" {
		t.Fatalf("masked = %#v", output.Masked)
	}

	commandMu.Lock()
	gotCommand, gotStdin := remoteCommand, remoteStdin
	commandMu.Unlock()
	if strings.Contains(gotCommand, secretValue) {
		t.Fatalf("remote command leaked secret value: %q", gotCommand)
	}
	if !strings.Contains(gotCommand, userCommand) {
		t.Fatalf("remote command does not retain the user command: %q", gotCommand)
	}
	delimiterLine, framedValue, foundLineBreak := bytes.Cut(gotStdin, []byte("\n"))
	if !foundLineBreak || !bytes.HasPrefix(delimiterLine, []byte("__sshmgr_environment_frames_")) ||
		!bytes.Contains([]byte(gotCommand), delimiterLine) || string(framedValue) != secretValue {
		t.Fatalf("standard input after the sudo password line = %q, delimiter = %q", gotStdin, delimiterLine)
	}

	auditRows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"exec"}, Statuses: []string{"ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 {
		t.Fatalf("successful exec audit rows = %d, want 1", len(auditRows))
	}
	audit := auditRows[0]
	if !audit.Sudo || len(audit.InjectedEnvironmentNames) != 1 || audit.InjectedEnvironmentNames[0] != "ELEVATED_API_TOKEN" {
		t.Fatalf("audit sudo and injected names = %v %#v", audit.Sudo, audit.InjectedEnvironmentNames)
	}
}

func TestSecretEnvironmentInjectionSudoToleratesPasswordNotConsumed(t *testing.T) {
	requirePOSIXRemoteSimulation(t)
	const sudoPassword = "sudopw-that-must-not-become-a-value"
	const secretValue = "sudo-nopasswd-secret-value"
	const userCommand = `printf '%s\n' "$ELEVATED_API_TOKEN"`

	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("ELEVATED_API_TOKEN", "", []byte(secretValue), "ELEVATED_API_TOKEN", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password:                "pw",
		SudoPassword:            sudoPassword,
		SudoDoesNotReadPassword: true,
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", "read standard input: " + err.Error(), 1
			}
			return runRecordedPOSIXCommand(command, stdinBytes)
		},
	}, sudoPassword)
	if err := st.AllowSecrets([]string{"ELEVATED_API_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": userCommand, "sudo": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	raw := toolResultText(result)
	if strings.Contains(raw, sudoPassword) {
		t.Fatalf("sudo password leaked when sudo did not consume it: %s", raw)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "{{sshmgr-secret:ELEVATED_API_TOKEN}}\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
}

func TestSecretEnvironmentInjectionExecBackgroundSudo(t *testing.T) {
	requirePOSIXRemoteSimulation(t)
	const secretValue = "sudo-background-injected-secret-value"
	const userCommand = `printf '%s\n' "$ELEVATED_API_TOKEN"`

	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("ELEVATED_API_TOKEN", "", []byte(secretValue), "ELEVATED_API_TOKEN", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	var commandMu sync.Mutex
	var remoteCommand string
	var remoteStdin []byte
	clientSession, serverID := newMaskingSSHToolClientWithSudo(t, st, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		ExecStream: func(command string, stdin io.Reader, stdout, stderr io.Writer) int {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				_, _ = stderr.Write([]byte("read standard input: " + err.Error()))
				return 1
			}
			commandMu.Lock()
			remoteCommand, remoteStdin = command, append([]byte(nil), stdinBytes...)
			commandMu.Unlock()

			return runRecordedPOSIXCommandStream(command, stdinBytes, stdout, stderr)
		},
	}, "sudopw")
	if err := st.AllowSecrets([]string{"ELEVATED_API_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": serverID, "command": userCommand, "sudo": true, "timeout_seconds": 5},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_background(sudo) failed: err=%v result=%+v", err, result.Content)
	}
	var start struct {
		TaskID            string   `json:"task_id"`
		Status            string   `json:"status"`
		InjectionWarnings []string `json:"injection_warnings"`
	}
	unmarshalToolJSON(t, result, &start)
	if start.TaskID == "" || start.Status != bgStatusRunning || len(start.InjectionWarnings) != 0 {
		t.Fatalf("exec_background(sudo) output = %+v", start)
	}

	final := waitBackgroundTerminal(t, clientSession, start.TaskID)
	if final.Status != bgStatusDone {
		t.Fatalf("background sudo task status = %q, want done", final.Status)
	}
	if final.Stdout != "{{sshmgr-secret:ELEVATED_API_TOKEN}}\n" {
		t.Fatalf("background sudo stdout = %q", final.Stdout)
	}
	if len(final.Masked) != 1 || final.Masked[0] != "ELEVATED_API_TOKEN" {
		t.Fatalf("background sudo masked names = %#v", final.Masked)
	}

	commandMu.Lock()
	gotCommand, gotStdin := remoteCommand, remoteStdin
	commandMu.Unlock()
	if strings.Contains(gotCommand, secretValue) {
		t.Fatalf("remote background sudo command leaked secret value: %q", gotCommand)
	}
	if !strings.Contains(gotCommand, userCommand) {
		t.Fatalf("remote background sudo command does not retain the user command: %q", gotCommand)
	}
	if !bytes.Contains(gotStdin, []byte(secretValue)) {
		t.Fatalf("remote background sudo standard input does not carry the secret value: %q", gotStdin)
	}

	auditRows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"exec-bg-start"}, Statuses: []string{"ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 || !auditRows[0].Sudo ||
		len(auditRows[0].InjectedEnvironmentNames) != 1 ||
		auditRows[0].InjectedEnvironmentNames[0] != "ELEVATED_API_TOKEN" {
		t.Fatalf("successful background sudo audit rows = %#v", auditRows)
	}
	if strings.Contains(strings.Join(auditRows[0].InjectedEnvironmentNames, "\n"), secretValue) {
		t.Fatal("background sudo audit injected environment names leaked secret value")
	}
}
