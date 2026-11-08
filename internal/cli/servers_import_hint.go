package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/secrethint"
)

// importHint pairs one imported server's scan findings with its alias — the
// findings alone don't say WHICH server, and an import is a batch.
type importHint struct {
	name     string
	findings []secrethint.Finding
}

// scanImportServer is the aggregate suspected-secret scan the import runs on
// each server right after the insert: the same secrethint.ScanServer the add
// path uses, over the metadata in the exact form this import persists. Today
// the import path carries ZERO user-controlled
// metadata — importer.Candidate comes from ssh_config, which supplies only
// HostName/Port/User/IdentityFile, and the server management module fills
// Name/Host/Port/User plus at most the fixed 待补口令标签 — so this scan is
// DEFENSIVE: it can only fire once the importer starts populating free-text
// fields. The key file CONTENT the module reads is a credential, never
// metadata — deliberately not scanned.
func scanImportServer(srv *models.Server) []secrethint.Finding {
	return secrethint.ScanServer(
		tagsRawForScan(srv.Tags),
		srv.Description, srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats,
	)
}

// printImportHints writes the aggregated per-server advisory warnings to the
// warning stream (stderr in production). Advisory only: the import loop never
// branches on it, and the warning never echoes field content.
func printImportHints(w io.Writer, hints []importHint) {
	for _, h := range hints {
		for _, f := range h.findings {
			fmt.Fprintf(w, "⚠ %s: %s\n", h.name, secrethint.FormatWarning(f))
		}
	}
}

// tagsRawForScan renders tags exactly as the store persists them —
// json.Marshal of the slice, the DB's tags TEXT (a JSON array string) — so
// the scan sees the same bytes list_servers later hands to LLM providers.
func tagsRawForScan(tags []string) string {
	b, _ := json.Marshal(tags)
	return string(b)
}
