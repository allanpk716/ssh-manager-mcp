package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestMaskingBackgroundOutputAcrossReadBoundary(t *testing.T) {
	const secretValue = "stream-secret-value-123"
	const secretName = "STREAM_TOKEN"
	prefix := strings.Repeat("x", len(secretValue)+8)
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	var closeFirstOnce sync.Once

	env := newBackgroundMaskingEnv(t, secretName, secretValue, func(_ string, _ io.Reader, stdout, stderr io.Writer) int {
		stdout.Write([]byte(prefix))
		stderr.Write([]byte(prefix))
		stdout.Write([]byte(secretValue[:len(secretValue)/2]))
		stderr.Write([]byte(secretValue[:len(secretValue)/2]))
		closeFirstOnce.Do(func() { close(firstWritten) })
		<-release
		stdout.Write([]byte(secretValue[len(secretValue)/2:]))
		stderr.Write([]byte(secretValue[len(secretValue)/2:]))
		return 0
	})

	start := callBackgroundTool(t, env.client, env.serverID, "split", 5)
	<-firstWritten
	first := readBackgroundOutput(t, env.client, start.TaskID, 0, 0, "text")
	if containsSecretFragment(first.Stdout, secretValue) || containsSecretFragment(first.Stderr, secretValue) {
		t.Fatalf("first read leaked a secret fragment: stdout=%q stderr=%q", first.Stdout, first.Stderr)
	}

	close(release)
	var final BgReadOutput
	final.Status = bgStatusRunning
	var collectedStdout, collectedStderr strings.Builder
	stdoutAfterFirst, stderrAfterFirst := first.NextStdoutOffset, first.NextStderrOffset
	for deadline := time.Now().Add(10 * time.Second); final.Status == bgStatusRunning && time.Now().Before(deadline); {
		final = readBackgroundOutput(t, env.client, start.TaskID, stdoutAfterFirst, stderrAfterFirst, "text")
		collectedStdout.WriteString(final.Stdout)
		collectedStderr.WriteString(final.Stderr)
		stdoutAfterFirst, stderrAfterFirst = final.NextStdoutOffset, final.NextStderrOffset
	}
	if final.Status != bgStatusDone {
		t.Fatalf("final status = %q, want done", final.Status)
	}
	want := prefix + "{{sshmgr-secret:" + secretName + "}}"
	if first.Stdout+collectedStdout.String() != want || first.Stderr+collectedStderr.String() != want {
		t.Fatalf("combined output changed: stdout=%q stderr=%q", first.Stdout+collectedStdout.String(), first.Stderr+collectedStderr.String())
	}
	if final.NextStdoutOffset != final.StdoutBytesTotal || final.NextStderrOffset != final.StderrBytesTotal {
		t.Fatalf("final offsets disagree with totals: stdout %d/%d, stderr %d/%d",
			final.NextStdoutOffset, final.StdoutBytesTotal, final.NextStderrOffset, final.StderrBytesTotal)
	}
	if len(final.Masked) != 1 || final.Masked[0] != secretName {
		t.Fatalf("final masked names = %#v", final.Masked)
	}
}

func TestMaskingBackgroundFlushesTailForTimeoutAndStop(t *testing.T) {
	const secretValue = "terminal-secret-value"
	const secretName = "TERMINAL_TOKEN"
	const prefix = "terminal-prefix:"
	stopRelease := make(chan struct{})
	timeoutRelease := make(chan struct{})
	written := make(chan string, 2)
	env := newBackgroundMaskingEnv(t, secretName, secretValue, func(command string, stdin io.Reader, stdout, stderr io.Writer) int {
		output := []byte(prefix + secretValue)
		stdout.Write(output)
		stderr.Write(output)
		written <- command
		switch command {
		case "stop":
			<-stopRelease
		case "timeout":
			<-timeoutRelease
		}
		return 0
	})

	stopStart := callBackgroundTool(t, env.client, env.serverID, "stop", 10)
	if command := <-written; command != "stop" {
		t.Fatalf("first streamed command = %q, want stop", command)
	}
	stopResult, err := env.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_stop",
		Arguments: map[string]any{"task_id": stopStart.TaskID},
	})
	if err != nil || stopResult.IsError {
		t.Fatalf("exec_stop failed: err=%v result=%+v", err, stopResult.Content)
	}
	close(stopRelease)
	stopOutput := waitBackgroundTerminal(t, env.client, stopStart.TaskID)
	if stopOutput.Status != bgStatusStopped {
		t.Fatalf("stopped task status = %q", stopOutput.Status)
	}
	assertBackgroundTailMasked(t, stopOutput, prefix, secretName, secretValue)

	timeoutStart := callBackgroundTool(t, env.client, env.serverID, "timeout", 1)
	if command := <-written; command != "timeout" {
		t.Fatalf("second streamed command = %q, want timeout", command)
	}
	time.Sleep(1100 * time.Millisecond)
	close(timeoutRelease)
	timeoutOutput := waitBackgroundTerminal(t, env.client, timeoutStart.TaskID)
	if timeoutOutput.Status != bgStatusTimeout {
		t.Fatalf("timeout task status = %q", timeoutOutput.Status)
	}
	assertBackgroundTailMasked(t, timeoutOutput, prefix, secretName, secretValue)
}

func waitBackgroundTerminal(t *testing.T, session *mcp.ClientSession, taskID string) BgReadOutput {
	t.Helper()
	var output BgReadOutput
	output.Status = bgStatusRunning
	var stdoutOffset, stderrOffset int64
	var stdout, stderr strings.Builder
	deadline := time.Now().Add(10 * time.Second)
	for output.Status == bgStatusRunning && time.Now().Before(deadline) {
		output = readBackgroundOutput(t, session, taskID, stdoutOffset, stderrOffset, "text")
		stdout.WriteString(output.Stdout)
		stderr.WriteString(output.Stderr)
		stdoutOffset, stderrOffset = output.NextStdoutOffset, output.NextStderrOffset
	}
	if output.Status == bgStatusRunning {
		t.Fatalf("task %s did not leave running within 10s", taskID)
	}
	output.Stdout, output.Stderr = stdout.String(), stderr.String()
	return output
}

func assertBackgroundTailMasked(t *testing.T, output BgReadOutput, prefix, secretName, secretValue string) {
	t.Helper()
	want := prefix + "{{sshmgr-secret:" + secretName + "}}"
	if output.Stdout != want || output.Stderr != want {
		t.Fatalf("terminal output = stdout %q stderr %q, want %q", output.Stdout, output.Stderr, want)
	}
	if len(output.Masked) != 1 || output.Masked[0] != secretName {
		t.Fatalf("terminal masked names = %#v", output.Masked)
	}
}

func TestMaskingBackgroundBase64Output(t *testing.T) {
	const secretValue = "base64-secret-value"
	const secretName = "BASE64_TOKEN"
	const prefix = "base64-prefix:"
	env := newBackgroundMaskingEnv(t, secretName, secretValue, func(_ string, _ io.Reader, stdout, stderr io.Writer) int {
		stdout.Write([]byte(prefix + secretValue))
		stderr.Write([]byte(prefix + secretValue))
		return 0
	})
	start := callBackgroundTool(t, env.client, env.serverID, "emit", 5)
	output := waitBackgroundTerminal(t, env.client, start.TaskID)
	if output.Status != bgStatusDone {
		t.Fatalf("base64 task status = %q", output.Status)
	}

	encoded := readBackgroundOutput(t, env.client, start.TaskID, 0, 0, "base64")
	stdout, err := base64.StdEncoding.DecodeString(encoded.Stdout)
	if err != nil {
		t.Fatalf("stdout is not standard base64: %q", encoded.Stdout)
	}
	stderr, err := base64.StdEncoding.DecodeString(encoded.Stderr)
	if err != nil {
		t.Fatalf("stderr is not standard base64: %q", encoded.Stderr)
	}
	want := []byte(prefix + "{{sshmgr-secret:" + secretName + "}}")
	if !bytes.Equal(stdout, want) || !bytes.Equal(stderr, want) {
		t.Fatalf("base64-decoded output: stdout=%q stderr=%q, want %q", stdout, stderr, want)
	}
	if encoded.NextStdoutOffset != int64(len(want)) || encoded.NextStderrOffset != int64(len(want)) {
		t.Fatalf("base64 offsets = %d/%d, want %d", encoded.NextStdoutOffset, encoded.NextStderrOffset, len(want))
	}
	if len(encoded.Masked) != 1 || encoded.Masked[0] != secretName {
		t.Fatalf("base64 masked names = %#v", encoded.Masked)
	}
}

func TestBackgroundOutputWithoutSecretsKeepsCurrentBehavior(t *testing.T) {
	env := newBackgroundMaskingEnv(t, "", "", func(_ string, _ io.Reader, stdout, stderr io.Writer) int {
		stdout.Write([]byte("plain stdout\n"))
		stderr.Write([]byte("plain stderr\n"))
		return 0
	})
	start := callBackgroundTool(t, env.client, env.serverID, "plain", 5)
	output := waitBackgroundTerminal(t, env.client, start.TaskID)
	if output.Status != bgStatusDone {
		t.Fatalf("plain task status = %q", output.Status)
	}
	if output.Stdout != "plain stdout\n" || output.Stderr != "plain stderr\n" {
		t.Fatalf("plain output changed: stdout=%q stderr=%q", output.Stdout, output.Stderr)
	}
	if output.Masked != nil {
		t.Fatalf("plain output reported masked names: %#v", output.Masked)
	}
	if output.NextStdoutOffset != int64(len(output.Stdout)) || output.NextStderrOffset != int64(len(output.Stderr)) {
		t.Fatalf("plain offsets = %d/%d", output.NextStdoutOffset, output.NextStderrOffset)
	}
}

func TestMaskingBackgroundUsesNewValueOnlyForLaterWrites(t *testing.T) {
	const oldValue = "same-store-old-secret"
	const newValue = "same-store-new-secret"
	const secretName = "CHANGING_TOKEN"
	prefix := strings.Repeat("y", len(oldValue)+len(newValue)+8)
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	env := newBackgroundMaskingEnv(t, secretName, oldValue, func(_ string, _ io.Reader, stdout, _ io.Writer) int {
		stdout.Write([]byte(prefix))
		stdout.Write([]byte(oldValue))
		close(firstWritten)
		<-release
		stdout.Write([]byte(newValue))
		return 0
	})
	start := callBackgroundTool(t, env.client, env.serverID, "change", 5)
	<-firstWritten
	first := readBackgroundOutput(t, env.client, start.TaskID, 0, 0, "text")
	placeholder := "{{sshmgr-secret:" + secretName + "}}"
	if first.Stdout != prefix+placeholder {
		t.Fatalf("first read = %q, want %q", first.Stdout, prefix+placeholder)
	}
	if err := env.st.SetSecretValue(secretName, []byte(newValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	close(release)
	final := waitBackgroundTerminal(t, env.client, start.TaskID)
	want := prefix + placeholder + placeholder
	if final.Stdout != want {
		t.Fatalf("changed-value output = %q, want %q", final.Stdout, want)
	}
	if len(final.Masked) != 1 || final.Masked[0] != secretName {
		t.Fatalf("changed-value masked names = %#v", final.Masked)
	}
}

func TestMaskingBackgroundDeletionDoesNotRescanBufferedOutput(t *testing.T) {
	const secretValue = "deleted-secret-value"
	const secretName = "DELETED_TOKEN"
	const suffix = "written-after-delete"
	prefix := strings.Repeat("d", len(secretValue)+8)
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	env := newBackgroundMaskingEnv(t, secretName, secretValue, func(_ string, _ io.Reader, stdout, _ io.Writer) int {
		stdout.Write([]byte(prefix))
		stdout.Write([]byte(secretValue))
		close(firstWritten)
		<-release
		stdout.Write([]byte(suffix))
		return 0
	})
	start := callBackgroundTool(t, env.client, env.serverID, "delete", 5)
	<-firstWritten
	first := readBackgroundOutput(t, env.client, start.TaskID, 0, 0, "text")
	if strings.Contains(first.Stdout, secretValue) {
		t.Fatalf("first read leaked deleted value: %q", first.Stdout)
	}
	if err := env.st.DeleteSecret(secretName, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	close(release)
	final := waitBackgroundTerminal(t, env.client, start.TaskID)
	want := prefix + "{{sshmgr-secret:" + secretName + "}}" + suffix
	if final.Stdout != want {
		t.Fatalf("output after delete = %q, want %q", final.Stdout, want)
	}
	if len(final.Masked) != 1 || final.Masked[0] != secretName {
		t.Fatalf("masked names after delete = %#v", final.Masked)
	}
}

func TestMaskingBackgroundChangesStoreForLaterWrites(t *testing.T) {
	const storeAValue = "store-a-secret-value"
	const storeBValue = "store-b-secret-value"
	prefix := strings.Repeat("z", len(storeAValue)+len(storeBValue)+8)
	firstWritten := make(chan struct{})
	release := make(chan struct{})

	storeA := newStore(t)
	storeB := newStore(t)
	if err := storeA.AddSecret("STORE_A", "", []byte(storeAValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := storeB.AddSecret("STORE_B", "", []byte(storeBValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		ExecStream: func(_ string, _ io.Reader, stdout, _ io.Writer) int {
			stdout.Write([]byte(prefix))
			stdout.Write([]byte(storeAValue))
			close(firstWritten)
			<-release
			stdout.Write([]byte(storeBValue))
			return 0
		},
	})
	defer cleanup()
	serverID := seedRealServer(t, storeA, "real", addr, hostKey, "")
	profileID, err := storeA.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := storeA.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	current := storeA
	server, tunnels, tasks, err := NewServerFromSource(func() *store.Store { return current }, profileID, "project-test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnels.CloseAll()
	defer tasks.CloseAll()
	clientSession := connectInMemoryMCPClient(t, server)

	start := callBackgroundTool(t, clientSession, serverID, "swap", 5)
	<-firstWritten
	first := readBackgroundOutput(t, clientSession, start.TaskID, 0, 0, "text")
	if strings.Contains(first.Stdout, storeBValue) {
		t.Fatalf("first read used the replacement store: %q", first.Stdout)
	}
	current = storeB
	close(release)
	final := waitBackgroundTerminal(t, clientSession, start.TaskID)
	want := prefix + "{{sshmgr-secret:STORE_A}}{{sshmgr-secret:STORE_B}}"
	if final.Stdout != want {
		t.Fatalf("store-swap output = %q, want %q", final.Stdout, want)
	}
	if len(final.Masked) != 2 || final.Masked[0] != "STORE_A" || final.Masked[1] != "STORE_B" {
		t.Fatalf("store-swap masked names = %#v", final.Masked)
	}
}

type backgroundStartOutput struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

type backgroundMaskingEnv struct {
	st       *store.Store
	serverID string
	client   *mcp.ClientSession
}

func newBackgroundMaskingEnv(t *testing.T, secretName, secretValue string, execStream func(string, io.Reader, io.Writer, io.Writer) int) *backgroundMaskingEnv {
	t.Helper()
	st := newStore(t)
	if secretName != "" {
		if err := st.AddSecret(secretName, "", []byte(secretValue), store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", ExecStream: execStream})
	t.Cleanup(cleanup)
	serverID := seedRealServer(t, st, "real", addr, hostKey, "")
	profileID, err := st.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(st, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return &backgroundMaskingEnv{st: st, serverID: serverID, client: clientSession}
}

func connectInMemoryMCPClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callBackgroundTool(t *testing.T, session *mcp.ClientSession, serverID, command string, timeoutSeconds int) backgroundStartOutput {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": serverID, "command": command, "timeout_seconds": timeoutSeconds},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_background failed: err=%v result=%+v", err, result.Content)
	}
	var output backgroundStartOutput
	unmarshalToolJSON(t, result, &output)
	if output.TaskID == "" || output.Status != bgStatusRunning {
		t.Fatalf("exec_background output = %+v", output)
	}
	return output
}

func readBackgroundOutput(t *testing.T, session *mcp.ClientSession, taskID string, stdoutOffset, stderrOffset int64, encoding string) BgReadOutput {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_output",
		Arguments: map[string]any{
			"task_id": taskID, "wait_seconds": 2, "encoding": encoding,
			"stdout_offset": stdoutOffset, "stderr_offset": stderrOffset,
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("exec_output failed: err=%v result=%+v", err, result.Content)
	}
	var output BgReadOutput
	unmarshalToolJSON(t, result, &output)
	return output
}

func containsSecretFragment(text, secret string) bool {
	for start := 0; start < len(secret); start++ {
		for end := start + 1; end <= len(secret); end++ {
			if strings.Contains(text, secret[start:end]) {
				return true
			}
		}
	}
	return false
}
