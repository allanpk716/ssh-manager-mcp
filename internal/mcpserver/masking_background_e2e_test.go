package mcpserver

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"ssh-manager-mcp/internal/testsshd"
)

func TestMaskingBackgroundOutputAcrossReadBoundary(t *testing.T) {
	const secretValue = "stream-secret-value-123"
	const secretName = "STREAM_TOKEN"
	prefix := strings.Repeat("x", len(secretValue)+8)
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	var closeFirstOnce sync.Once

	st := newStore(t)
	if err := st.AddSecret(secretName, "", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		ExecStream: func(_ string, _ io.Reader, stdout, stderr io.Writer) int {
			stdout.Write([]byte(prefix))
			stderr.Write([]byte(prefix))
			stdout.Write([]byte(secretValue[:len(secretValue)/2]))
			stderr.Write([]byte(secretValue[:len(secretValue)/2]))
			closeFirstOnce.Do(func() { close(firstWritten) })
			<-release
			stdout.Write([]byte(secretValue[len(secretValue)/2:]))
			stderr.Write([]byte(secretValue[len(secretValue)/2:]))
			return 0
		},
	})
	defer cleanup()

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
	defer tunnels.CloseAll()
	defer tasks.CloseAll()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	start := callBackgroundTool(t, clientSession, serverID, "split")
	<-firstWritten
	first := readBackgroundOutput(t, clientSession, start.TaskID, 0, 0, "text")
	if containsSecretFragment(first.Stdout, secretValue) || containsSecretFragment(first.Stderr, secretValue) {
		t.Fatalf("first read leaked a secret fragment: stdout=%q stderr=%q", first.Stdout, first.Stderr)
	}

	close(release)
	var final BgReadOutput
	final.Status = bgStatusRunning
	stdoutAfterFirst, stderrAfterFirst := first.NextStdoutOffset, first.NextStderrOffset
	for deadline := time.Now().Add(10 * time.Second); final.Status == bgStatusRunning && time.Now().Before(deadline); {
		final = readBackgroundOutput(t, clientSession, start.TaskID, stdoutAfterFirst, stderrAfterFirst, "text")
		stdoutAfterFirst, stderrAfterFirst = final.NextStdoutOffset, final.NextStderrOffset
	}
	if final.Status != bgStatusDone {
		t.Fatalf("final status = %q, want done", final.Status)
	}
	want := prefix + "{{sshmgr-secret:" + secretName + "}}"
	if first.Stdout+final.Stdout != want || first.Stderr+final.Stderr != want {
		t.Fatalf("combined output changed: stdout=%q stderr=%q", first.Stdout+final.Stdout, first.Stderr+final.Stderr)
	}
	if final.NextStdoutOffset != final.StdoutBytesTotal || final.NextStderrOffset != final.StderrBytesTotal {
		t.Fatalf("final offsets disagree with totals: stdout %d/%d, stderr %d/%d",
			final.NextStdoutOffset, final.StdoutBytesTotal, final.NextStderrOffset, final.StderrBytesTotal)
	}
	if len(final.Masked) != 1 || final.Masked[0] != secretName {
		t.Fatalf("final masked names = %#v", final.Masked)
	}
}

type backgroundStartOutput struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

func callBackgroundTool(t *testing.T, session *mcp.ClientSession, serverID, command string) backgroundStartOutput {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": serverID, "command": command, "timeout_seconds": 5},
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
