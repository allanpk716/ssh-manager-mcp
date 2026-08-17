// secrethint.go wires the Plan 28 suspected-secret scanner (internal/
// secrethint) into the TUI's two metadata-carrying save paths: the
// servers-page add/edit form (forms.go submitServer) and the import flow's
// supplement submit (importflow.go submitSupplement). The batch insert in
// importflow's startBatch persists NO user free-text (name/host/port/user
// plus the fixed needs-passphrase tag — the same field-flow fact as the CLI
// import), so it has no scan; the supplement submit is the import flow's
// metadata-carrying save point. All hints are advisory: never blocking, no
// new confirm screens, and the rendered lines never echo field content.
// Credential material (password/key bytes) is never passed here — only the
// seven free-text metadata fields are scanned.
package tui

import (
	"encoding/json"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/secrethint"
)

// hintLines renders one advisory ⚠ line per suspected-secret finding across
// the seven free-text metadata fields. Pure (no store, no tea) so the
// warning text is unit-testable without driving the bubbletea loop.
func hintLines(tags, description, location, hardware, services, role, caveats string) []string {
	findings := secrethint.ScanServer(tags, description, location, hardware, services, role, caveats)
	if len(findings) == 0 {
		return nil
	}
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, "⚠ "+secrethint.FormatWarning(f))
	}
	return lines
}

// tagsScanValue renders tags exactly as the store persists them —
// json.Marshal of the slice, the DB's tags TEXT — so the scan sees the same
// bytes list_servers later hands to LLM providers (same discipline as the
// CLI's tagsRawForScan).
func tagsScanValue(tags []string) string {
	b, _ := json.Marshal(tags)
	return string(b)
}

// serverHintLines adapts one server row to hintLines. Call it on the FINAL
// persisted form — after every field adjustment (trim, tag drop) — at the
// save point. The TUI form submits all fields at once, so the edit leg scans
// the whole final row (unlike the CLI's partial-update changed-only scan).
func serverHintLines(srv *models.Server) []string {
	return hintLines(
		tagsScanValue(srv.Tags),
		srv.Description, srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats,
	)
}

// appendHintLines appends advisory hint lines to a status/report string.
// base alone is returned when there is nothing to say.
func appendHintLines(base string, lines []string) string {
	if len(lines) == 0 {
		return base
	}
	return base + "\n" + strings.Join(lines, "\n")
}
