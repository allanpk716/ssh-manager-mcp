package mcpserver

import (
	"strings"

	"ssh-manager-mcp/internal/store"
)

const noAvailableSecretsText = "没有可用的机密项"

// secretNoteLineBreaks keeps one secret on one output line: owner notes may
// contain line breaks, while the tool contract uses one line per secret.
var secretNoteLineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// ListSecretsInput is the list_secrets tool input.
type ListSecretsInput struct {
	Query  string `json:"query,omitempty" jsonschema:"optional case-insensitive substring to match against the secret name or note"`
	Server string `json:"server,omitempty" jsonschema:"optional server id (exact match, preferred) or server name; only secrets that may write to this server are listed"`
}

// ListSecretsTextForProfile returns the agent-visible secret list. The server
// scope is the profile's granted server set, and the text deliberately carries
// only names, notes, environment variable names, and granted server names.
// serverIdentifier accepts a
// server id (exact match first) or a server name, resolved only within the
// granted set; out-of-scope and unknown identifiers are indistinguishable.
func ListSecretsTextForProfile(st *store.Store, profileID, query, serverIdentifier string) (string, error) {
	serverName := serverIdentifier
	if serverIdentifier != "" {
		server, err := st.ResolveGrantedServer(profileID, serverIdentifier)
		if err != nil {
			return "", err
		}
		if server == nil {
			return noAvailableSecretsText, nil
		}
		serverName = server.Name
	}

	serverIDs, err := st.ServersForProfile(profileID)
	if err != nil {
		return "", err
	}
	views, err := st.ListSecretsInScope(store.ScopeToServers(serverIDs...))
	if err != nil {
		return "", err
	}
	views = store.FilterSecretsByServer(views, serverName)
	views = store.FilterSecretsByQuery(views, query)
	lines := make([]string, 0, len(views))
	for _, view := range views {
		note := secretNoteLineBreaks.Replace(view.Note)
		lines = append(lines, view.Name+" | "+note+" | "+view.EnvironmentName+" | "+strings.Join(view.AllowedServers, ","))
	}
	if len(lines) == 0 {
		return noAvailableSecretsText, nil
	}
	return strings.Join(lines, "\n"), nil
}
