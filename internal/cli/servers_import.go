package cli

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/importer"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/secrethint"
	"ssh-manager-mcp/internal/store"
)

// serversImportCmd: `sshmgr servers import [--file] [--dry-run] [--profile]`.
// Batch-imports literal-host aliases from an OpenSSH client config. Vault
// conflicts are filtered by the pure importer.PlanImport (the same seam the
// TUI import flow reuses); each server insert is one transaction
// (AddServerWithCredentials); identical key files within one batch mint ONE
// credential row (sha256 dedup); an encrypted key imports as-is with a
// needs-passphrase warning AND tag (the same literal the TUI writes — the ⚠
// sort + `!` filter key on it; fix later via TUI or servers edit, both of
// which drop the tag on re-credential).
func serversImportCmd() *cobra.Command {
	var file, profile string
	var dryRun bool
	c := &cobra.Command{
		Use:   "import",
		Short: "Batch-import servers from an OpenSSH client config (~/.ssh/config)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				home, _ := os.UserHomeDir()
				file = filepath.Join(home, ".ssh", "config")
			}
			file = expandTilde(file)
			fallbackUser := currentUserName()
			res, err := importer.Parse(file, fallbackUser)
			if err != nil {
				return err
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			var profID string
			if profile != "" { // precheck BEFORE anything (fail-fast; dry-run included)
				p, err := profileByName(s, profile)
				if err != nil {
					return err
				}
				if p == nil {
					return fmt.Errorf("profile %q not found (create it first: profiles add)", profile)
				}
				profID = p.ID
			}
			return runImport(cmd.OutOrStdout(), cmd.ErrOrStderr(), s, res, filepath.Dir(file), dryRun, profID, profile)
		},
	}
	c.Flags().StringVar(&file, "file", "", "config file (default ~/.ssh/config)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen; write nothing")
	c.Flags().StringVar(&profile, "profile", "", "grant every imported server to this profile (must exist)")
	return c
}

// profileByName finds a profile row by name; nil (not an error) when absent —
// the store has no name-keyed lookup, so this scans like profiles.go does.
func profileByName(s *store.Store, name string) (*models.Profile, error) {
	profs, err := s.ListProfiles()
	if err != nil {
		return nil, err
	}
	for _, p := range profs {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, nil
}

// importReport is one output line: the alias plus what happened to it.
type importReport struct{ name, note string }

// importHint pairs one imported server's scan findings with its alias — the
// findings alone don't say WHICH server, and an import is a batch.
type importHint struct {
	name     string
	findings []secrethint.Finding
}

// scanImportServer is the aggregate suspected-secret scan the import runs on
// each server right before the insert (Plan 28 T3): the same
// secrethint.ScanServer the T2 add path uses, over the metadata in the exact
// form this import persists. Today the CLI import path carries ZERO
// user-controlled metadata — importer.Candidate comes from ssh_config, which
// supplies only HostName/Port/User/IdentityFile (internal/importer/importer.go),
// and runImport fills Name/Host/Port/User plus at most the fixed
// "needs-passphrase" tag literal; Description/Location/Hardware/Services/
// Role/Caveats are always "". This scan is therefore DEFENSIVE: it can only
// fire once the importer starts populating free-text fields. The real
// metadata-carrying import leg is the TUI supplement form
// (internal/tui/importflow.go submitSupplement), which runs its own scan at
// its save point. The key file CONTENT importer.PickKey reads is a
// credential, never metadata — deliberately not scanned.
func scanImportServer(srv *models.Server) []secrethint.Finding {
	return secrethint.ScanServer(
		tagsRawForScan(srv.Tags),
		srv.Description, srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats,
	)
}

// printImportHints writes the aggregated per-server advisory warnings to the
// warning stream (stderr). Advisory only: the import loop never branches on
// it, and the warning never echoes field content.
func printImportHints(w io.Writer, hints []importHint) {
	for _, h := range hints {
		for _, f := range h.findings {
			fmt.Fprintf(w, "⚠ %s: %s\n", h.name, secrethint.FormatWarning(f))
		}
	}
}

// runImport walks the parsed candidates: vault-conflict filtering
// (importer.PlanImport), first-readable-key resolution with batch-level
// credential dedup and needs-passphrase detection, one transactional insert
// per server, then the optional profile grant. dry-run computes and prints
// everything but writes nothing. warn receives the aggregated Plan 28 T3
// suspected-secret warnings (stderr in production); out stays the report
// table.
func runImport(out, warn io.Writer, s *store.Store, res *importer.Result, configDir string, dryRun bool, profID, profName string) error {
	if res.MatchWarning {
		fmt.Fprintln(out, "⚠ config 含 Match 块：相关继承值由库按 Host 模式近似求值，可能与真 ssh 不一致")
	}
	existing, err := s.ListServers()
	if err != nil {
		return err
	}
	ex := make([]importer.ExistingServer, len(existing))
	for i, e := range existing {
		ex[i] = importer.ExistingServer{Name: e.Name, Host: e.Host, Port: e.Port, User: e.User}
	}
	// Vault-conflict filtering is the pure PlanImport the TUI import flow
	// reuses. runImport walks res.Candidates rather than PlanImport's
	// toImport so skip-existing lines interleave with imports in config
	// order; toImport is redundant here because the two outputs partition
	// the candidate list.
	_, vaultSkips := importer.PlanImport(res.Candidates, ex)
	skipReason := make(map[string]string, len(vaultSkips))
	for _, sk := range vaultSkips {
		skipReason[sk.Alias] = sk.Reason
	}
	keyIDs := map[[32]byte]string{} // sha256(key content) -> credential id (batch dedup)
	var report []importReport
	var importedIDs []string
	var hints []importHint // Plan 28 T3 suspected-secret findings, dumped after the report
	wouldGrant := 0        // dry-run inserts nothing; the grant line still reports the would-grant count
	for _, cand := range res.Candidates {
		if reason, skip := skipReason[cand.Name]; skip {
			report = append(report, importReport{cand.Name, renderVaultSkip(reason)})
			continue
		}
		// First-readable-key resolution, batch dedup and needs-passphrase
		// detection live in importer.PickKey — the seam shared with the TUI
		// import flow (T10). The reuse path (cred.ID set) still reports
		// NeedsPass: a second host sharing an encrypted key lacks the
		// passphrase just as much as the first.
		pick := importer.PickKey(cand, configDir, keyIDs)
		cred := pick.Cred
		note := "" // plain key: nothing to annotate
		if cred == nil {
			note = "needs-credential" // no IdentityFile resolved to a readable key
		} else if pick.NeedsPass {
			note = "needs-passphrase ⚠（连接会失败；TUI 补全或 servers edit --key-passphrase）"
		}
		if dryRun {
			report = append(report, importReport{cand.Name, "will-import (" + noteOrCred(note, cred) + ")"})
			wouldGrant++
			continue
		}
		srv := &models.Server{
			Name: cand.Name, Host: cand.Host, Port: cand.Port, User: cand.User,
		}
		if pick.NeedsPass {
			// Same tag the TUI import flow writes: serverNeedsAttention (⚠
			// sort, `!` filter) keys on it, and every re-credential path
			// (TUI edit, servers edit) drops it once the passphrase lands.
			srv.Tags = []string{"needs-passphrase"}
		}
		// Plan 28 T3: advisory scan of the metadata this insert is about to
		// persist — see scanImportServer for why it cannot fire today (the
		// import carries no user free-text). Collected and printed after the
		// report; the insert ALWAYS proceeds.
		if fs := scanImportServer(srv); len(fs) > 0 {
			hints = append(hints, importHint{name: cand.Name, findings: fs})
		}
		id, err := s.AddServerWithCredentials(srv, cred, nil)
		if err != nil {
			report = append(report, importReport{cand.Name, "FAILED: " + err.Error()})
			continue
		}
		if pick.Minted && cred.ID != "" {
			// insertCredentialTx backfilled cred.ID — later candidates whose
			// key file hashes to pick.Sum reuse this row instead of minting.
			keyIDs[pick.Sum] = cred.ID
		}
		importedIDs = append(importedIDs, id)
		report = append(report, importReport{cand.Name, "imported " + noteOrCred(note, cred)})
	}
	// parse-phase skips (wildcard-pattern / bad-port / internal-duplicate)
	for _, sk := range res.Skipped {
		report = append(report, importReport{sk.Alias, "skip: " + sk.Reason})
	}
	for _, r := range report {
		fmt.Fprintf(out, "%-20s %s\n", r.name, r.note)
	}
	// Aggregated advisory block (Plan 28 T3): suspected-secret metadata
	// warnings go to the WARNING stream; the stdout report table and every
	// exit path below are untouched.
	printImportHints(warn, hints)
	// --profile grant (dry-run only prints)
	if profID != "" {
		if dryRun || len(importedIDs) == 0 {
			suffix := ""
			n := len(importedIDs)
			if dryRun {
				suffix = " (dry-run, not granted)"
				n = wouldGrant
			}
			fmt.Fprintf(out, "grant: %d server(s) -> %s%s\n", n, profName, suffix)
			return nil
		}
		if err := s.GrantServers(profID, importedIDs); err != nil {
			return fmt.Errorf("imported but grant failed: %w", err)
		}
		fmt.Fprintf(out, "granted %d server(s) -> %s\n", len(importedIDs), profName)
	}
	if dryRun {
		return nil // the closing line describes post-import state; nothing was written
	}
	fmt.Fprintln(out, "提示：原私钥文件仍在盘上（vault 另存一份）；结构化字段可进 TUI 逐台补全或 servers edit。")
	return nil
}

// renderVaultSkip maps a PlanImport skip reason to its report wording.
func renderVaultSkip(reason string) string {
	switch reason {
	case "existing-name":
		return "skip-existing (name)"
	case "existing-endpoint":
		return "skip-existing (host:port:user)"
	default:
		return "skip-existing (" + reason + ")"
	}
}

// noteOrCred renders the per-candidate credential annotation: a needs-*
// note when present, else "key"/"no-key" for the import result lines.
func noteOrCred(note string, cred *models.Credential) string {
	if note != "" {
		return note
	}
	if cred != nil {
		return "key"
	}
	return "no-key"
}

// expandTilde expands a bare "~" or "~/"-prefixed path to the user home dir.
// "~user/..." forms are left untouched (mirroring importer.ResolveKeyPaths).
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// currentUserName is the ssh_config User fallback: the OS account name via
// user.Current, with env fallbacks (USERNAME on Windows, USER on POSIX).
func currentUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	return os.Getenv("USER")
}
