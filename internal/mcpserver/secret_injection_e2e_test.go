package mcpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestSecretEnvironmentInjectionExecCommand covers the main end-to-end seam:
// the in-memory MCP client calls exec_command against an in-process SSH server,
// and that server runs the received command with a local POSIX shell. The test
// observes both the command text and the standard input seen by the target.
func TestSecretEnvironmentInjectionExecCommand(t *testing.T) {
	const secretValue = "injected-secret-%-backslash-\\-value\n"
	const secondSecretValue = "second-injected-value-\xff\n"
	const userCommand = `if IFS= read -r extra_stdin; then printf 'unexpected standard input'; fi; printf '%s\n' "$HF_TOKEN"; printf '%s\n' "$SECOND_TOKEN"`

	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("API_TOKEN", "Deploy token", []byte(secretValue), "HF_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("SECOND_TOKEN", "", []byte(secondSecretValue), "SECOND_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("NO_ENV_TOKEN", "", []byte("not-an-environment-secret")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("OTHER_SERVER_TOKEN", "", []byte("other-server-secret"), "OTHER_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{Name: "other-server", Host: "192.0.2.20", Port: 22, User: "u"}); err != nil {
		t.Fatal(err)
	}

	// If the injection wrapper reaches command policy, this rule blocks the call.
	// The original user command does not match it, so success proves policy saw
	// only the original command.
	if err := st.AddPolicyRule(policy.CustomRule{
		ID:          "INJECTION-LEAK",
		Shells:      []models.ShellType{models.ShellPOSIX},
		Regex:       `__sshmgr_.*`,
		Disposition: policy.DispositionBlock,
		Explanation: "test rule that catches an injection wrapper leaked into policy",
	}); err != nil {
		t.Fatal(err)
	}

	var commandMu sync.Mutex
	var remoteCommand string
	var remoteStdin []byte
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", "read standard input: " + err.Error(), 1
			}
			commandMu.Lock()
			remoteCommand, remoteStdin = command, stdinBytes
			commandMu.Unlock()

			localShell := exec.Command("/bin/sh", "-c", command)
			localShell.Stdin = bytes.NewReader(stdinBytes)
			output := &bytes.Buffer{}
			errors := &bytes.Buffer{}
			localShell.Stdout = output
			localShell.Stderr = errors
			if err := localShell.Run(); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					return output.String(), errors.String(), exitErr.ExitCode()
				}
				return output.String(), errors.String() + err.Error(), 1
			}
			return output.String(), errors.String(), 0
		},
	})
	if err := st.AllowSecrets([]string{"API_TOKEN", "SECOND_TOKEN", "NO_ENV_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"OTHER_SERVER_TOKEN"}, []string{"other-server"}); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": userCommand},
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
	if output.Stdout != "{{sshmgr-secret:API_TOKEN}}\n{{sshmgr-secret:SECOND_TOKEN}}\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
	if len(output.Masked) != 2 || output.Masked[0] != "API_TOKEN" || output.Masked[1] != "SECOND_TOKEN" {
		t.Fatalf("masked = %#v", output.Masked)
	}

	commandMu.Lock()
	gotCommand, gotStdin := remoteCommand, remoteStdin
	commandMu.Unlock()
	if strings.Contains(gotCommand, secretValue) {
		t.Fatalf("remote command leaked secret value: %q", gotCommand)
	}
	if strings.Contains(gotCommand, secondSecretValue) {
		t.Fatalf("remote command leaked second secret value: %q", gotCommand)
	}
	if !strings.Contains(gotCommand, userCommand) {
		t.Fatalf("remote command does not retain the original user command: %q", gotCommand)
	}
	if wantStdin := secretValue + secondSecretValue; string(gotStdin) != wantStdin {
		t.Fatalf("remote standard input = %q, want only the framed secret value", gotStdin)
	}
	auditRows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"exec"}, Statuses: []string{"ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 {
		t.Fatalf("successful exec audit rows = %d, want 1: %#v", len(auditRows), auditRows)
	}
	audit := auditRows[0]
	if audit.Command != userCommand {
		t.Fatalf("audit command = %q, want original command", audit.Command)
	}
	if len(audit.InjectedEnvironmentNames) != 2 || audit.InjectedEnvironmentNames[0] != "HF_TOKEN" || audit.InjectedEnvironmentNames[1] != "SECOND_TOKEN" {
		t.Fatalf("audit injected environment names = %#v", audit.InjectedEnvironmentNames)
	}
	if strings.Contains(strings.Join(audit.InjectedEnvironmentNames, "\n"), secretValue) || strings.Contains(strings.Join(audit.InjectedEnvironmentNames, "\n"), secondSecretValue) {
		t.Fatal("audit injected environment names leaked secret value")
	}
}

func TestSecretEnvironmentInjectionScopeAndRevocation(t *testing.T) {
	const secretValue = "authorized-secret-value"
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("API_TOKEN", "", []byte(secretValue), "API_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("NO_ENV_TOKEN", "", []byte("no-environment-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("OTHER_SERVER_TOKEN", "", []byte("other-server-secret"), "OTHER_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{Name: "other-server", Host: "192.0.2.20", Port: 22, User: "u"}); err != nil {
		t.Fatal(err)
	}
	var commandMu sync.Mutex
	var commands []string
	var standardInputs [][]byte
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			content, err := io.ReadAll(stdin)
			if err != nil {
				return "", err.Error(), 1
			}
			commandMu.Lock()
			commands = append(commands, command)
			standardInputs = append(standardInputs, content)
			commandMu.Unlock()
			return "ran", "", 0
		},
	})
	if err := st.AllowSecrets([]string{"API_TOKEN", "NO_ENV_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"OTHER_SERVER_TOKEN"}, []string{"other-server"}); err != nil {
		t.Fatal(err)
	}

	call := func() {
		result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "exec_command",
			Arguments: map[string]any{"server_id": serverID, "command": "printf ran"},
		})
		if err != nil || result.IsError {
			t.Fatalf("exec_command: err=%v result=%+v", err, result.Content)
		}
	}
	call()
	if err := st.DisallowSecrets([]string{"API_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	call()

	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	profileID := ""
	for _, profile := range profiles {
		if profile.Name == "agent-profile" {
			profileID = profile.ID
		}
	}
	if profileID == "" {
		t.Fatal("could not find the seeded profile")
	}
	revokedSnapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	revokedStore := newStore(t)
	if err := revokedStore.ImportSnapshot(revokedSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := ExecCommandForProfile(context.Background(), revokedStore, "project-test", profileID, serverID, "printf ran", false, 5*time.Second, ExecConfirmation{}); err != nil {
		t.Fatal(err)
	}

	commandMu.Lock()
	gotCommands, gotInputs := append([]string(nil), commands...), append([][]byte(nil), standardInputs...)
	commandMu.Unlock()
	if len(gotCommands) != 3 || len(gotInputs) != 3 {
		t.Fatalf("calls = %d, want 3: commands=%#v inputs=%#v", len(gotCommands), gotCommands, gotInputs)
	}
	for i, command := range gotCommands {
		if strings.Contains(command, secretValue) || strings.Contains(command, "no-environment-secret-value") || strings.Contains(command, "other-server-secret") {
			t.Fatalf("command %d leaked a secret value: %q", i, command)
		}
	}
	if string(gotInputs[0]) != secretValue {
		t.Fatalf("first standard input = %q, want the authorized value", gotInputs[0])
	}
	if len(gotInputs[2]) != 0 {
		t.Fatalf("standard input after revocation = %q, want empty", gotInputs[2])
	}
	if strings.Contains(gotCommands[2], "API_TOKEN") {
		t.Fatalf("command after revocation still contains injection variable name: %q", gotCommands[2])
	}
}

func TestSecretEnvironmentInjectionPowerShell(t *testing.T) {
	const secretValue = "windows-secret-%-\\-\"中文\"\nsecond-line"
	const userCommand = `Write-Output $env:HF_TOKEN`
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("API_TOKEN", "", []byte(secretValue), "HF_TOKEN"); err != nil {
		t.Fatal(err)
	}

	var commandMu sync.Mutex
	var remoteCommand string
	var remoteStdin []byte
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", "read standard input: " + err.Error(), 1
			}
			commandMu.Lock()
			remoteCommand, remoteStdin = command, stdinBytes
			commandMu.Unlock()
			return runRecordedPowerShellCommand(t, command, stdinBytes)
		},
	})
	if err := st.AllowSecrets([]string{"API_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	setRealServerShell(t, st, serverID, models.ShellPowerShell)

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": userCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)

	commandMu.Lock()
	gotCommand, gotStdin := remoteCommand, remoteStdin
	commandMu.Unlock()
	if strings.Contains(gotCommand, secretValue) {
		t.Fatalf("remote command leaked secret value: %q", gotCommand)
	}
	if !strings.Contains(gotCommand, userCommand) || !strings.Contains(gotCommand, "$env:HF_TOKEN") {
		t.Fatalf("remote command does not retain the PowerShell user command: %q", gotCommand)
	}
	if string(gotStdin) != secretValue {
		t.Fatalf("remote standard input = %q, want only the framed secret value", gotStdin)
	}
	if output.Stdout != "{{sshmgr-secret:API_TOKEN}}\n" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
}

func runRecordedPowerShellCommand(t *testing.T, command string, stdinBytes []byte) (string, string, int) {
	t.Helper()
	powerShellName := "pwsh"
	if _, err := exec.LookPath(powerShellName); err != nil {
		if runtime.GOOS != "windows" {
			return "ran", "", 0
		}
		powerShellName = "powershell"
		if _, err := exec.LookPath(powerShellName); err != nil {
			return "ran", "", 0
		}
	}
	script, err := os.CreateTemp("", "remote-command-*.ps1")
	if err != nil {
		return "", err.Error(), 1
	}
	if _, err := script.Write([]byte(command)); err != nil {
		_ = script.Close()
		_ = os.Remove(script.Name())
		return "", err.Error(), 1
	}
	if err := script.Close(); err != nil {
		_ = os.Remove(script.Name())
		return "", err.Error(), 1
	}
	defer os.Remove(script.Name())
	localShell := exec.Command(powerShellName, "-NoProfile", "-NonInteractive", "-File", script.Name())
	localShell.Stdin = bytes.NewReader(stdinBytes)
	output := &bytes.Buffer{}
	errors := &bytes.Buffer{}
	localShell.Stdout = output
	localShell.Stderr = errors
	if err := localShell.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return output.String(), errors.String(), exitErr.ExitCode()
		}
		return output.String(), errors.String() + err.Error(), 1
	}
	return output.String(), errors.String(), 0
}

func TestSecretEnvironmentInjectionWindowsCmd(t *testing.T) {
	const secretValue = `cmd-secret-%-^&!-"中文"`
	const userCommand = "set HF_TOKEN"
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("API_TOKEN", "", []byte(secretValue), "HF_TOKEN"); err != nil {
		t.Fatal(err)
	}

	var commandMu sync.Mutex
	var remoteCommand string
	var remoteStdin []byte
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", "read standard input: " + err.Error(), 1
			}
			commandMu.Lock()
			remoteCommand, remoteStdin = command, stdinBytes
			commandMu.Unlock()
			return runRecordedWindowsCmdCommand(t, command, stdinBytes)
		},
	})
	if err := st.AllowSecrets([]string{"API_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	setRealServerShell(t, st, serverID, models.ShellWindowsCmd)

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": userCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)

	commandMu.Lock()
	gotCommand, gotStdin := remoteCommand, remoteStdin
	commandMu.Unlock()
	if strings.Contains(gotCommand, secretValue) {
		t.Fatalf("remote command leaked secret value: %q", gotCommand)
	}
	if !strings.Contains(gotCommand, userCommand) || !strings.Contains(gotCommand, "set /p HF_TOKEN=") {
		t.Fatalf("remote command does not retain the cmd injection and user command: %q", gotCommand)
	}
	if wantStdin := secretValue + "\r\n"; string(gotStdin) != wantStdin {
		t.Fatalf("remote standard input = %q, want the value followed by a line break", gotStdin)
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(output.Stdout, "{{sshmgr-secret:API_TOKEN}}") || strings.Contains(output.Stdout, secretValue) {
			t.Fatalf("stdout = %q", output.Stdout)
		}
	} else if output.Stdout != "ran" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
}

func runRecordedWindowsCmdCommand(t *testing.T, command string, stdinBytes []byte) (string, string, int) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return "ran", "", 0
	}
	localShell := exec.Command("cmd", "/d", "/c", command)
	localShell.Stdin = bytes.NewReader(stdinBytes)
	output := &bytes.Buffer{}
	errors := &bytes.Buffer{}
	localShell.Stdout = output
	localShell.Stderr = errors
	if err := localShell.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return output.String(), errors.String(), exitErr.ExitCode()
		}
		return output.String(), errors.String() + err.Error(), 1
	}
	return output.String(), errors.String(), 0
}

func TestSecretEnvironmentInjectionWindowsValueWarningsDoNotStopCommand(t *testing.T) {
	tests := []struct {
		name     string
		shell    models.ShellType
		value    []byte
		wantWarn string
	}{
		{
			name:     "PowerShell 值不是合法 UTF-8",
			shell:    models.ShellPowerShell,
			value:    []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			wantWarn: "环境变量 WINDOWS_TOKEN 未注入：Windows 上值不是合法 UTF-8\n",
		},
		{
			name:     "cmd 值不是合法 UTF-8",
			shell:    models.ShellWindowsCmd,
			value:    []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			wantWarn: "环境变量 WINDOWS_TOKEN 未注入：Windows 上值不是合法 UTF-8\n",
		},
		{
			name:     "cmd 值含换行",
			shell:    models.ShellWindowsCmd,
			value:    []byte("first\r\nsecond\n"),
			wantWarn: "环境变量 WINDOWS_TOKEN 未注入：cmd 下值含换行\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newStore(t)
			if err := st.AddSecretWithEnvironmentName("WINDOWS_SECRET", "", test.value, "WINDOWS_TOKEN"); err != nil {
				t.Fatal(err)
			}
			var commandMu sync.Mutex
			var remoteCommand string
			var remoteStdin []byte
			clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
				Password: "pw",
				Exec: func(command string, stdin io.Reader) (string, string, int) {
					stdinBytes, err := io.ReadAll(stdin)
					if err != nil {
						return "", "read standard input: " + err.Error(), 1
					}
					commandMu.Lock()
					remoteCommand, remoteStdin = command, stdinBytes
					commandMu.Unlock()
					return "ran", "", 0
				},
			})
			if err := st.AllowSecrets([]string{"WINDOWS_SECRET"}, []string{"real"}); err != nil {
				t.Fatal(err)
			}
			setRealServerShell(t, st, serverID, test.shell)

			result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "exec_command",
				Arguments: map[string]any{"server_id": serverID, "command": "echo ok"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("exec_command failed: %+v", result.Content)
			}
			var output struct {
				Stdout string `json:"stdout"`
			}
			unmarshalToolJSON(t, result, &output)

			commandMu.Lock()
			gotCommand, gotStdin := remoteCommand, remoteStdin
			commandMu.Unlock()
			if want := "ran\n" + test.wantWarn; output.Stdout != want {
				t.Fatalf("stdout = %q, want %q", output.Stdout, want)
			}
			if len(gotStdin) != 0 {
				t.Fatalf("remote standard input = %q, want empty", gotStdin)
			}
			if gotCommand != "echo ok" {
				t.Fatalf("remote command = %q, want only the original user command", gotCommand)
			}
		})
	}
}

func setRealServerShell(t *testing.T, st *store.Store, serverID string, shell models.ShellType) {
	t.Helper()
	server, err := st.GetServer(serverID)
	if err != nil || server == nil {
		t.Fatalf("read server: %v %v", server, err)
	}
	server.ShellType = shell
	if err := st.UpdateServer(server); err != nil {
		t.Fatal(err)
	}
}

func TestSecretEnvironmentInjectionWarningsDoNotStopCommand(t *testing.T) {
	const zeroByteValue = "zero-byte-secret-value\x00"
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("ZERO_TOKEN", "", []byte(zeroByteValue), "ZERO_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("UNKNOWN_SHELL_TOKEN", "", []byte("unknown-shell-secret-value"), "UNKNOWN_TOKEN"); err != nil {
		t.Fatal(err)
	}
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(_ string, stdin io.Reader) (string, string, int) {
			_, _ = io.ReadAll(stdin)
			return "ran", "", 0
		},
	})
	if err := st.AllowSecrets([]string{"ZERO_TOKEN", "UNKNOWN_SHELL_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServer(serverID)
	if err != nil || srv == nil {
		t.Fatalf("read server: %v %v", srv, err)
	}
	srv.ShellType = "unknown-shell"
	if err := st.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}

	firstResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": "printf ran"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !firstResult.IsError {
		t.Fatalf("unknown shell should first ask for command-policy confirmation: %+v", firstResult.Content)
	}
	firstText := toolResultText(firstResult)
	confirmation := regexp.MustCompile(`confirmation code ([A-Z2-9]+):`).FindStringSubmatch(firstText)
	if len(confirmation) != 2 {
		t.Fatalf("could not read confirmation code from %q", firstText)
	}
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command",
		Arguments: map[string]any{
			"server_id": serverID, "command": "printf ran",
			"confirmation_code": confirmation[1], "confirmation_reason": "test consent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed after confirmation: %+v", result.Content)
	}
	var unknownShellOutput struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &unknownShellOutput)
	wantUnknownShell := "ran\n环境变量 UNKNOWN_TOKEN 未注入：服务器 shell 类型未知\n环境变量 ZERO_TOKEN 未注入：服务器 shell 类型未知\n"
	if unknownShellOutput.Stdout != wantUnknownShell {
		t.Fatalf("unknown-shell stdout = %q, want %q", unknownShellOutput.Stdout, wantUnknownShell)
	}

	srv.ShellType = models.ShellPOSIX
	if err := st.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}
	result, err = clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": "printf ran"},
	})
	if err != nil || result.IsError {
		t.Fatalf("POSIX exec_command: err=%v text=%q", err, toolResultText(result))
	}
	var posixOutput struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &posixOutput)
	wantZeroByte := "ran\n环境变量 ZERO_TOKEN 未注入：值含空字节\n"
	if posixOutput.Stdout != wantZeroByte {
		t.Fatalf("zero-byte stdout = %q, want %q", posixOutput.Stdout, wantZeroByte)
	}
}

func TestSecretEnvironmentInjectionReadFailureWarnsOnlyFailedVariable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "secrets.db")
	st := newStoreAt(t, dbPath)
	if err := st.AddSecretWithEnvironmentName("GOOD_SECRET", "", []byte("read-failure-good-value"), "GOOD_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("BAD_SECRET", "", []byte("read-failure-bad-value"), "BAD_TOKEN"); err != nil {
		t.Fatal(err)
	}

	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(command string, stdin io.Reader) (string, string, int) {
			stdinBytes, err := io.ReadAll(stdin)
			if err != nil {
				return "", err.Error(), 1
			}
			localShell := exec.Command("/bin/sh", "-c", command)
			localShell.Stdin = bytes.NewReader(stdinBytes)
			output := &bytes.Buffer{}
			localShell.Stdout = output
			if err := localShell.Run(); err != nil {
				return output.String(), err.Error(), 1
			}
			return output.String(), "", 0
		},
	})
	if err := st.AllowSecrets([]string{"GOOD_SECRET", "BAD_SECRET"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	if _, err := rawDB.Exec(`UPDATE secrets SET value_blob=? WHERE name=?`, []byte("not-encrypted"), "BAD_SECRET"); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": `printf '%s' "${GOOD_TOKEN:-unset}${BAD_TOKEN:-unset}"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	unmarshalToolJSON(t, result, &output)
	want := "{{sshmgr-secret:GOOD_SECRET}}unset\n环境变量 BAD_TOKEN 未注入：读取机密值失败\n"
	if output.Stdout != want {
		t.Fatalf("stdout = %q, want %q", output.Stdout, want)
	}
}

func TestSecretEnvironmentInjectionSkipsFileTransfer(t *testing.T) {
	const secretValue = "file-transfer-secret-value"
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("FILE_TOKEN", "", []byte(secretValue), "FILE_TOKEN"); err != nil {
		t.Fatal(err)
	}
	var commandMu sync.Mutex
	var execStandardInputs [][]byte
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(_ string, stdin io.Reader) (string, string, int) {
			content, err := io.ReadAll(stdin)
			if err != nil {
				return "", err.Error(), 1
			}
			commandMu.Lock()
			execStandardInputs = append(execStandardInputs, content)
			commandMu.Unlock()
			return "", "", 0
		},
	})
	if err := st.AllowSecrets([]string{"FILE_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}

	const fileBody = "uploaded body without injection frame"
	remotePath := filepath.ToSlash(filepath.Join(t.TempDir(), "uploaded.txt"))
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     base64.StdEncoding.EncodeToString([]byte(fileBody)),
			"remote_path": remotePath,
			"encoding":    "base64",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("upload_content failed: %+v", result.Content)
	}

	gotFile, err := os.ReadFile(filepath.FromSlash(remotePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotFile) != fileBody || bytes.Contains(gotFile, []byte(secretValue)) {
		t.Fatalf("uploaded file = %q, want %q and no secret value", gotFile, fileBody)
	}
	commandMu.Lock()
	inputs := append([][]byte(nil), execStandardInputs...)
	commandMu.Unlock()
	for i, input := range inputs {
		if bytes.Contains(input, []byte(secretValue)) {
			t.Fatalf("file transfer exec standard input %d leaked secret value: %q", i, input)
		}
	}
}

func TestSecretEnvironmentInjectionWarningIsNotTruncated(t *testing.T) {
	st := newStore(t)
	if err := st.AddSecretWithEnvironmentName("ZERO_TOKEN", "", []byte("truncation-zero-byte-value\x00"), "ZERO_TOKEN"); err != nil {
		t.Fatal(err)
	}
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(string, io.Reader) (string, string, int) {
			return strings.Repeat("x", int(MaxOutputBytes)), "", 0
		},
	})
	if err := st.AllowSecrets([]string{"ZERO_TOKEN"}, []string{"real"}); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": "printf full"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout    string `json:"stdout"`
		Truncated bool   `json:"truncated"`
	}
	unmarshalToolJSON(t, result, &output)
	warning := "\n环境变量 ZERO_TOKEN 未注入：值含空字节\n"
	wantLength := int(MaxOutputBytes) + len(warning)
	if len(output.Stdout) != wantLength {
		t.Fatalf("stdout length = %d, want %d", len(output.Stdout), wantLength)
	}
	if !strings.HasSuffix(output.Stdout, warning) {
		t.Fatalf("stdout does not end with the untruncated warning: %q", output.Stdout[len(output.Stdout)-100:])
	}
	if output.Truncated {
		t.Fatal("warning must not turn command output into truncated output")
	}
}

func toolResultText(result *mcp.CallToolResult) string {
	var text strings.Builder
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text.WriteString(textContent.Text)
		}
	}
	return text.String()
}
