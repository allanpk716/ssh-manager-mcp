package mcpserver

import (
	"slices"
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
	Server string `json:"server,omitempty" jsonschema:"optional server name; only secrets that may write to this server are listed"`
}

// ListSecretsTextForProfile returns the agent-visible secret list. The server
// scope is the profile's granted server set, and the text deliberately carries
// only names, notes, and granted server names.
func ListSecretsTextForProfile(st *store.Store, profileID, query, serverName string) (string, error) {
	serverIDs, err := st.ServersForProfile(profileID)
	if err != nil {
		return "", err
	}
	if serverName != "" && !profileContainsServerName(st, serverIDs, serverName) {
		return noAvailableSecretsText, nil
	}

	views, err := st.ListSecretsForAuthorizedServers(store.AuthorizeSecretServers(serverIDs...))
	if err != nil {
		return "", err
	}
	needle := strings.ToLower(query)
	lines := make([]string, 0, len(views))
	for _, view := range views {
		if needle != "" &&
			!strings.Contains(strings.ToLower(view.Name), needle) &&
			!strings.Contains(strings.ToLower(view.Note), needle) {
			continue
		}
		if serverName != "" && !slices.Contains(view.AllowedServers, serverName) {
			continue
		}
		note := secretNoteLineBreaks.Replace(view.Note)
		lines = append(lines, view.Name+" | "+note+" | "+strings.Join(view.AllowedServers, ","))
	}
	if len(lines) == 0 {
		return noAvailableSecretsText, nil
	}
	return strings.Join(lines, "\n"), nil
}

func profileContainsServerName(st *store.Store, serverIDs []string, serverName string) bool {
	for _, serverID := range serverIDs {
		server, err := st.GetServer(serverID)
		if err != nil || server == nil {
			continue
		}
		if server.Name == serverName {
			return true
		}
	}
	return false
}
