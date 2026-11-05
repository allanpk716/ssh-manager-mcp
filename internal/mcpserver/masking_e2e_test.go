package mcpserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestMaskingExecCommandStdoutAndStderr(t *testing.T) {
	const secretValue = "masked-secret-value"
	st := newStore(t)
	if err := st.AddSecret("API_TOKEN", "Deploy token", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(string, io.Reader) (string, string, int) {
			return "stdout has " + secretValue, "stderr has " + secretValue, 0
		},
	})

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": "print"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout string   `json:"stdout"`
		Stderr string   `json:"stderr"`
		Masked []string `json:"masked"`
	}
	unmarshalToolJSON(t, result, &output)
	if output.Stdout != "stdout has {{sshmgr-secret:API_TOKEN}}" {
		t.Fatalf("stdout = %q", output.Stdout)
	}
	if output.Stderr != "stderr has {{sshmgr-secret:API_TOKEN}}" {
		t.Fatalf("stderr = %q", output.Stderr)
	}
	if len(output.Masked) != 1 || output.Masked[0] != "API_TOKEN" {
		t.Fatalf("masked = %#v", output.Masked)
	}
}

func TestMaskingToolErrorText(t *testing.T) {
	const secretValue = "error-secret-value"
	st := newStore(t)
	if err := st.AddSecret("ERROR_TOKEN", "", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "mask-test", Version: "test"}, nil)
	busy := NewBusyTracker(func() int { return 0 }, func() int { return 0 })
	storeFn := func() *store.Store { return st }
	errorText := func() string { return "failed with " + secretValue }
	addTrackedTool(server, busy, storeFn, nil,
		&mcp.Tool{Name: "go_error_probe"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, MaskedOutput, error) {
			return nil, MaskedOutput{}, errors.New(errorText())
		},
	)
	addTrackedTool(server, busy, storeFn, nil,
		&mcp.Tool{Name: "tool_error_probe"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, MaskedOutput, error) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: errorText()}},
			}, MaskedOutput{}, nil
		},
	)
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

	for _, toolName := range []string{"go_error_probe", "tool_error_probe"} {
		result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      toolName,
			Arguments: map[string]any{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError {
			t.Fatalf("%s unexpectedly succeeded: %+v", toolName, result.Content)
		}
		var text string
		for _, content := range result.Content {
			if textContent, ok := content.(*mcp.TextContent); ok {
				text += textContent.Text
			}
		}
		if containsStr(text, secretValue) {
			t.Fatalf("%s leaked secret value: %q", toolName, text)
		}
		if !containsStr(text, "{{sshmgr-secret:ERROR_TOKEN}}") {
			t.Fatalf("%s does not contain placeholder: %q", toolName, text)
		}
		if !containsStr(text, "\nMasked secrets: ERROR_TOKEN") {
			t.Fatalf("%s does not report masked name: %q", toolName, text)
		}
	}
}

func TestMaskingExecCommandTruncatesAfterMasking(t *testing.T) {
	const secretValue = "boundary-secret-value"
	const unifiedSecretValue = "unified-layer-secret-value"
	st := newStore(t)
	if err := st.AddSecret("BOUNDARY_TOKEN", "", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("UNIFIED_TOKEN", "", []byte(unifiedSecretValue)); err != nil {
		t.Fatal(err)
	}
	prefix := strings.Repeat("x", int(MaxOutputBytes)-5)
	stdout := prefix + secretValue + "tail"
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(string, io.Reader) (string, string, int) {
			return stdout, unifiedSecretValue, 0
		},
	})

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": "print"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("exec_command failed: %+v", result.Content)
	}
	var output struct {
		Stdout      string   `json:"stdout"`
		Stderr      string   `json:"stderr"`
		Truncated   bool     `json:"truncated"`
		StdoutBytes int64    `json:"stdout_bytes"`
		StderrBytes int64    `json:"stderr_bytes"`
		Masked      []string `json:"masked"`
	}
	unmarshalToolJSON(t, result, &output)
	if int64(len(output.Stdout)) != MaxOutputBytes {
		t.Fatalf("stdout length = %d, want %d", len(output.Stdout), MaxOutputBytes)
	}
	if !strings.HasSuffix(output.Stdout, prefix+"{{ssh") {
		t.Fatalf("stdout ending = %q", output.Stdout[len(output.Stdout)-40:])
	}
	for i := 1; i <= len(secretValue); i++ {
		if strings.HasSuffix(output.Stdout, secretValue[:i]) {
			t.Fatalf("stdout ends with a %d-byte prefix of the secret value", i)
		}
	}
	if !output.Truncated {
		t.Fatal("truncated = false, want true")
	}
	if output.StdoutBytes != int64(len(stdout)) || output.StderrBytes != int64(len(unifiedSecretValue)) {
		t.Fatalf("byte counts = stdout %d stderr %d, want stdout %d stderr %d", output.StdoutBytes, output.StderrBytes, len(stdout), len(unifiedSecretValue))
	}
	if len(output.Masked) != 2 || output.Masked[0] != "BOUNDARY_TOKEN" || output.Masked[1] != "UNIFIED_TOKEN" {
		t.Fatalf("masked = %#v", output.Masked)
	}
}

func TestMaskingDownloadFileTruncatesAfterMasking(t *testing.T) {
	const secretValue = "download-boundary-secret"
	st := newStore(t)
	if err := st.AddSecret("DOWNLOAD_TOKEN", "", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("y", int(MaxOutputBytes)-6) + secretValue + "tail"
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{Password: "pw"})
	remote := filepath.Join(t.TempDir(), "boundary.txt")
	if err := os.WriteFile(remote, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "download_file",
		Arguments: map[string]any{"server_id": serverID, "path": remote},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("download_file failed: %+v", result.Content)
	}
	var output struct {
		Content   string   `json:"content"`
		Bytes     int64    `json:"bytes"`
		Truncated bool     `json:"truncated"`
		Masked    []string `json:"masked"`
	}
	unmarshalToolJSON(t, result, &output)
	if int64(len(output.Content)) != MaxOutputBytes {
		t.Fatalf("content length = %d, want %d", len(output.Content), MaxOutputBytes)
	}
	if !strings.HasSuffix(output.Content, strings.Repeat("y", 20)+"{{sshm") {
		t.Fatalf("content ending = %q", output.Content[len(output.Content)-40:])
	}
	for i := 1; i <= len(secretValue); i++ {
		if strings.HasSuffix(output.Content, secretValue[:i]) {
			t.Fatalf("content ends with a %d-byte prefix of the secret value", i)
		}
	}
	if !output.Truncated {
		t.Fatal("truncated = false, want true")
	}
	if output.Bytes != int64(len(content)) {
		t.Fatalf("bytes = %d, want remote file size %d", output.Bytes, len(content))
	}
	if len(output.Masked) != 1 || output.Masked[0] != "DOWNLOAD_TOKEN" {
		t.Fatalf("masked = %#v", output.Masked)
	}
}

func TestMaskingEmptyValueSetKeepsForegroundAndDownloadUnchanged(t *testing.T) {
	st := newStore(t)
	stdout := strings.Repeat("x", int(MaxOutputBytes)*2)
	stderr := strings.Repeat("e", int(MaxOutputBytes)+1)
	clientSession, serverID := newMaskingSSHToolClient(t, st, testsshd.Options{
		Password: "pw",
		Exec: func(string, io.Reader) (string, string, int) {
			return stdout, stderr, 0
		},
	})

	execResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "exec_command",
		Arguments: map[string]any{"server_id": serverID, "command": "big"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if execResult.IsError {
		t.Fatalf("exec_command failed: %+v", execResult.Content)
	}
	var execOutput struct {
		Stdout      string   `json:"stdout"`
		Stderr      string   `json:"stderr"`
		Truncated   bool     `json:"truncated"`
		StdoutBytes int64    `json:"stdout_bytes"`
		StderrBytes int64    `json:"stderr_bytes"`
		Masked      []string `json:"masked"`
	}
	unmarshalToolJSON(t, execResult, &execOutput)
	if execOutput.Stdout != stdout[:MaxOutputBytes] || execOutput.Stderr != stderr[:MaxOutputBytes] {
		t.Fatal("empty secret set changed retained command output")
	}
	if !execOutput.Truncated || execOutput.StdoutBytes != int64(len(stdout)) || execOutput.StderrBytes != int64(len(stderr)) {
		t.Fatalf("empty secret set changed truncation or byte counts: %+v", execOutput)
	}
	if execOutput.Masked != nil {
		t.Fatalf("empty secret set reported masked names: %#v", execOutput.Masked)
	}

	fileContent := strings.Repeat("z", int(MaxOutputBytes)*2)
	remote := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(remote, []byte(fileContent), 0600); err != nil {
		t.Fatal(err)
	}
	downloadResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "download_file",
		Arguments: map[string]any{"server_id": serverID, "path": remote},
	})
	if err != nil {
		t.Fatal(err)
	}
	if downloadResult.IsError {
		t.Fatalf("download_file failed: %+v", downloadResult.Content)
	}
	var downloadOutput struct {
		Content   string   `json:"content"`
		Bytes     int64    `json:"bytes"`
		Truncated bool     `json:"truncated"`
		Masked    []string `json:"masked"`
	}
	unmarshalToolJSON(t, downloadResult, &downloadOutput)
	if downloadOutput.Content != fileContent[:MaxOutputBytes] || downloadOutput.Bytes != int64(len(fileContent)) || !downloadOutput.Truncated {
		t.Fatalf("empty secret set changed download output: %+v", downloadOutput)
	}
	if downloadOutput.Masked != nil {
		t.Fatalf("empty secret set reported download masked names: %#v", downloadOutput.Masked)
	}
}

func newMaskingSSHToolClient(t *testing.T, st *store.Store, options testsshd.Options) (*mcp.ClientSession, string) {
	t.Helper()
	addr, hostKey, cleanup := testsshd.Start(t, options)
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
	return clientSession, serverID
}

type testMetadataEditor struct{}

func (testMetadataEditor) EditMeta(string, int64, map[string]*string) (MetadataEditOutcome, error) {
	return MetadataEditOutcome{}, nil
}

func TestAllRegisteredToolsDeclareMaskedOutput(t *testing.T) {
	snapshot, projectToken, _ := seedSnap(t, 1)
	server, tunnels, tasks, cleanup, err := NewCacheBroker(
		projectToken,
		snapshot,
		t.TempDir()+"/audit.log",
		nil,
		nil,
		"laptop",
		testMetadataEditor{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer tunnels.CloseAll()
	defer tasks.CloseAll()
	registerReloadSelfTool(server, NewBusyTracker(func() int { return 0 }, func() int { return 0 }), nil)

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

	toolList, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := make(map[string]bool, len(toolList.Tools))
	for _, tool := range toolList.Tools {
		registered[tool.Name] = true
		schema, ok := tool.OutputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s has no object output schema: %#v", tool.Name, tool.OutputSchema)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s output schema has no properties: %#v", tool.Name, schema)
		}
		if _, ok := properties["masked"]; !ok {
			t.Fatalf("%s output schema has no masked field: %#v", tool.Name, schema)
		}
		if !containsStr(tool.Description, "Only a {{sshmgr-secret:NAME}} placeholder whose NAME appears in the masked field is a masking result.") {
			t.Fatalf("%s description does not explain masked placeholders: %s", tool.Name, tool.Description)
		}
	}
	for _, toolName := range BrokerTools {
		if !registered[toolName] {
			t.Fatalf("tool %s is not registered", toolName)
		}
	}
}

type maskingProbeInput struct {
	ServerID string `json:"server_id,omitempty"`
}

type maskingProbeNested struct {
	Text   string
	Values []string
}

type maskingProbeOutput struct {
	MaskedOutput
	Text            string              `json:"text"`
	Nested          *maskingProbeNested `json:"nested"`
	Slice           []string            `json:"slice"`
	Map             map[string]string   `json:"map"`
	Any             any                 `json:"any"`
	ContextServerID string              `json:"context_server_id"`
}

func TestMaskingLifecycleAndFieldShapes(t *testing.T) {
	const (
		firstValue  = "first-secret-value"
		secondValue = "second-secret-value"
		thirdValue  = "third-secret-value"
	)
	st := newStore(t)
	if err := st.AddSecret("API_TOKEN", "", []byte(firstValue)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecretValue("API_TOKEN", []byte(secondValue)); err != nil {
		t.Fatal(err)
	}

	values := []string{firstValue, secondValue}
	clientSession := newMaskingProbeServer(t, st, func() maskingProbeOutput {
		return maskingProbeOutput{
			Text:   "literal {{sshmgr-secret:API_TOKEN}}",
			Nested: &maskingProbeNested{Text: values[0], Values: []string{values[1]}},
			Slice:  append([]string(nil), values...),
			Map:    map[string]string{values[0]: values[1]},
			Any:    maskingProbeNested{Text: values[1]},
		}
	})
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "masking_probe",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("masking_probe: %v", err)
	}
	if result.IsError {
		t.Fatalf("masking_probe: err=%v result=%+v", err, result.Content)
	}
	var first maskingProbeOutput
	unmarshalToolJSON(t, result, &first)
	placeholder := "{{sshmgr-secret:API_TOKEN}}"
	if first.Text != "literal {{sshmgr-secret:API_TOKEN}}" {
		t.Fatalf("literal placeholder changed: %q", first.Text)
	}
	if first.Nested.Text != placeholder || first.Nested.Values[0] != placeholder {
		t.Fatalf("nested strings not masked: %#v", first.Nested)
	}
	if first.Slice[0] != placeholder || first.Slice[1] != placeholder {
		t.Fatalf("slice strings not masked: %#v", first.Slice)
	}
	if first.Map[placeholder] != placeholder {
		t.Fatalf("map key/value not masked: %#v", first.Map)
	}
	if anyText, _ := first.Any.(map[string]any); anyText["Text"] != placeholder {
		t.Fatalf("interface string not masked: %#v", first.Any)
	}
	if len(first.Masked) != 1 || first.Masked[0] != "API_TOKEN" {
		t.Fatalf("masked = %#v", first.Masked)
	}

	if err := st.SetSecretValue("API_TOKEN", []byte(thirdValue)); err != nil {
		t.Fatal(err)
	}
	values = []string{firstValue, secondValue, thirdValue}
	result, err = clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "masking_probe",
		Arguments: map[string]any{},
	})
	if err != nil || result.IsError {
		t.Fatalf("masking_probe after value change: err=%v result=%+v", err, result.Content)
	}
	var second maskingProbeOutput
	unmarshalToolJSON(t, result, &second)
	if containsStr(second.Text, firstValue) {
		t.Fatalf("retired values remain active: %+v", second)
	}
	if second.Slice[1] != placeholder || second.Slice[2] != placeholder || second.Slice[0] != firstValue {
		t.Fatalf("current and previous values not selected correctly: %#v", second.Slice)
	}

	if err := st.DeleteSecret("API_TOKEN"); err != nil {
		t.Fatal(err)
	}
	result, err = clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "masking_probe",
		Arguments: map[string]any{},
	})
	if err != nil || result.IsError {
		t.Fatalf("masking_probe after delete: err=%v result=%+v", err, result.Content)
	}
	var deleted maskingProbeOutput
	unmarshalToolJSON(t, result, &deleted)
	if deleted.Slice[1] != secondValue || deleted.Slice[2] != thirdValue {
		t.Fatalf("deleted secret still masks values: %#v", deleted.Slice)
	}
	if deleted.Masked != nil {
		t.Fatalf("deleted secret still reports masked names: %#v", deleted.Masked)
	}
}

func TestMaskingUsesCurrentServerForTieBreakAndMasksUnauthorizedValues(t *testing.T) {
	const sharedValue = "shared-secret-value"
	st := newStore(t)
	if err := st.AddSecret("AAA_SAME", "", []byte(sharedValue)); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("ZZZ_SAME", "", []byte(sharedValue)); err != nil {
		t.Fatal(err)
	}
	serverID, err := st.AddServer(&models.Server{Name: "target", Host: "192.0.2.10", Port: 22, User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"ZZZ_SAME"}, []string{"target"}); err != nil {
		t.Fatal(err)
	}
	call := func(serverID string) string {
		clientSession := newMaskingProbeServer(t, st, func() maskingProbeOutput {
			return maskingProbeOutput{Text: sharedValue, Slice: []string{}, Map: map[string]string{}}
		})
		defer clientSession.Close()
		result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "masking_probe",
			Arguments: map[string]any{"server_id": serverID},
		})
		if err != nil {
			t.Fatalf("masking_probe: %v", err)
		}
		if result.IsError {
			t.Fatalf("masking_probe failed: %+v", result.Content)
		}
		var output maskingProbeOutput
		unmarshalToolJSON(t, result, &output)
		return output.Text
	}
	if got := call(serverID); got != "{{sshmgr-secret:ZZZ_SAME}}" {
		t.Fatalf("current server tie-break text = %q", got)
	}
	if got := call(""); got != "{{sshmgr-secret:AAA_SAME}}" {
		t.Fatalf("without current server tie-break text = %q", got)
	}
}

func TestMaskingReReadsStoreForEachCall(t *testing.T) {
	storeA := newStore(t)
	storeB := newStore(t)
	if err := storeA.AddSecret("STORE_A", "", []byte("store-a-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := storeB.AddSecret("STORE_B", "", []byte("store-b-secret-value")); err != nil {
		t.Fatal(err)
	}
	current := storeA
	server, tunnels, tasks, err := NewServerFromSource(func() *store.Store { return current }, "profile", "project", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnels.CloseAll()
	defer tasks.CloseAll()
	currentValue := func() string {
		values, err := current.SecretValuesForMasking()
		if err != nil || len(values) != 1 {
			return "store probe could not read its current store: " + err.Error()
		}
		return string(values[0].Current)
	}
	addTrackedTool(server, NewBusyTracker(func() int { return 0 }, func() int { return 0 }),
		func() *store.Store { return current }, nil,
		&mcp.Tool{Name: "store_probe"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, maskingProbeOutput, error) {
			return nil, maskingProbeOutput{
				Text:  currentValue(),
				Slice: []string{},
				Map:   map[string]string{},
			}, nil
		},
	)
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
	callStoreProbe := func() string {
		result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "store_probe",
			Arguments: map[string]any{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("store_probe failed: %+v", result.Content)
		}
		var output maskingProbeOutput
		unmarshalToolJSON(t, result, &output)
		if len(output.Masked) != 1 {
			t.Fatalf("masked = %#v", output.Masked)
		}
		return output.Text
	}
	if got := callStoreProbe(); got != "{{sshmgr-secret:STORE_A}}" {
		t.Fatalf("first store text = %q", got)
	}
	current = storeB
	if got := callStoreProbe(); got != "{{sshmgr-secret:STORE_B}}" {
		t.Fatalf("second store text = %q", got)
	}
}

func TestMaskingReadFailureDoesNotReturnHandlerOutput(t *testing.T) {
	const secretValue = "closed-store-secret-value"
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir()+"/closed.db", masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("CLOSED_TOKEN", "", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "mask-closed", Version: "test"}, nil)
	addTrackedTool(server, NewBusyTracker(func() int { return 0 }, func() int { return 0 }),
		func() *store.Store { return st }, nil,
		&mcp.Tool{Name: "closed_store_probe"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, MaskedOutput, error) {
			return nil, MaskedOutput{}, errors.New("handler returned " + secretValue)
		},
	)
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

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "closed_store_probe",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text += textContent.Text
		}
	}
	if !result.IsError || containsStr(text, secretValue) {
		t.Fatalf("closed store result must fail without the original text: isError=%v text=%q", result.IsError, text)
	}
	if !containsStr(text, "prepare output masking") {
		t.Fatalf("closed store result does not report masking preparation failure: %q", text)
	}
}

func TestMaskingEmptyValueSetKeepsResultUnchangedAndExposesContext(t *testing.T) {
	st := newStore(t)
	clientSession := newMaskingProbeServer(t, st, func() maskingProbeOutput {
		return maskingProbeOutput{
			Text:  "literal {{sshmgr-secret:NOT_REGISTERED}}",
			Slice: []string{},
			Map:   map[string]string{},
		}
	})
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "masking_probe",
		Arguments: map[string]any{"server_id": "server-for-context"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("masking_probe failed: %+v", result.Content)
	}
	var output maskingProbeOutput
	unmarshalToolJSON(t, result, &output)
	if output.Text != "literal {{sshmgr-secret:NOT_REGISTERED}}" {
		t.Fatalf("text changed with empty secret set: %q", output.Text)
	}
	if output.Masked != nil {
		t.Fatalf("masked reported with empty secret set: %#v", output.Masked)
	}
	if output.ContextServerID != "server-for-context" {
		t.Fatalf("context server id = %q", output.ContextServerID)
	}
}

func newMaskingProbeServer(t *testing.T, st *store.Store, output func() maskingProbeOutput) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "mask-probe", Version: "test"}, nil)
	addTrackedTool(server, NewBusyTracker(func() int { return 0 }, func() int { return 0 }),
		func() *store.Store { return st }, nil,
		&mcp.Tool{Name: "masking_probe"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ maskingProbeInput) (*mcp.CallToolResult, maskingProbeOutput, error) {
			out := output()
			_, contextServerID, contextOK := MaskingCallFromContext(ctx)
			if !contextOK {
				out.ContextServerID = "masking-context-missing"
			} else {
				out.ContextServerID = contextServerID
			}
			return nil, out, nil
		},
	)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	return clientSession
}
