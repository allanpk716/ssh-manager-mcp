package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestListSecretsStandaloneTools calls the read-only discovery surface over
// the in-memory MCP wire: profile scoping, both filters, their intersection,
// indistinguishable missing/out-of-profile servers, and the empty result.
func TestListSecretsStandaloneTools(t *testing.T) {
	st := newStore(t)
	alpha := mustAddServer(t, st, "alpha")
	mustAddServer(t, st, "beta")
	profileID, _ := st.AddProfile("agent-profile")
	if err := st.GrantServers(profileID, []string{alpha}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("API_TOKEN", "Deploy token", []byte("api-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("NOTE_MATCH", "maintenance phrase", []byte("note-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("BETA_ONLY", "outside profile", []byte("beta-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"API_TOKEN"}, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"NOTE_MATCH"}, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"BETA_ONLY"}, []string{"beta"}); err != nil {
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

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "all", args: map[string]any{}, want: "API_TOKEN | Deploy token | alpha\nNOTE_MATCH | maintenance phrase | alpha"},
		{name: "query matches name case-insensitively", args: map[string]any{"query": "api"}, want: "API_TOKEN | Deploy token | alpha"},
		{name: "query matches note", args: map[string]any{"query": "MAINTENANCE"}, want: "NOTE_MATCH | maintenance phrase | alpha"},
		{name: "server filters", args: map[string]any{"server": "alpha"}, want: "API_TOKEN | Deploy token | alpha\nNOTE_MATCH | maintenance phrase | alpha"},
		{name: "query and server intersect", args: map[string]any{"query": "maintenance", "server": "alpha"}, want: "NOTE_MATCH | maintenance phrase | alpha"},
		{name: "out-of-profile server", args: map[string]any{"server": "beta"}, want: noAvailableSecretsText},
		{name: "missing server", args: map[string]any{"server": "missing"}, want: noAvailableSecretsText},
		{name: "no secret matches", args: map[string]any{"query": "nothing"}, want: noAvailableSecretsText},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, isError := callListSecrets(t, clientSession, testCase.args)
			if isError {
				t.Fatalf("list_secrets failed: %s", got)
			}
			if got != testCase.want {
				t.Fatalf("list_secrets = %q, want %q", got, testCase.want)
			}
			for _, value := range []string{"api-secret-value", "note-secret-value", "beta-secret-value", "beta", "BETA_ONLY"} {
				if strings.Contains(got, value) {
					t.Fatalf("list_secrets leaked %q: %s", value, got)
				}
			}
		})
	}
}

// TestListSecretsKeepsEachSecretOnOneLine pins the compact-output contract
// when an owner note itself contains line breaks.
func TestListSecretsKeepsEachSecretOnOneLine(t *testing.T) {
	st := newStore(t)
	alpha := mustAddServer(t, st, "alpha")
	profileID, _ := st.AddProfile("agent-profile")
	if err := st.GrantServers(profileID, []string{alpha}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("MULTI_NOTE", "第一行\n第二行", []byte("multi-note-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"MULTI_NOTE"}, []string{"alpha"}); err != nil {
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
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	got, isError := callListSecrets(t, session, map[string]any{})
	if isError {
		t.Fatalf("list_secrets failed: %s", got)
	}
	if want := "MULTI_NOTE | 第一行 第二行 | alpha"; got != want {
		t.Fatalf("list_secrets = %q, want %q", got, want)
	}
}

func mustAddServer(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	id, err := st.AddServer(&models.Server{Name: name, Host: name, Port: 22, User: "u", AuthMethod: models.AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func callListSecrets(t *testing.T, session *mcp.ClientSession, args map[string]any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_secrets",
		Arguments: args,
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
	return text, result.IsError
}
