package cli

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/knownhosts"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// servers pin-hostkey (Plan 48 §3): the owner out-of-band channel for the
// host-key anchors that pin forwarding (§1) and TOFU create. Six forms:
//
//	pin-hostkey <name>                       show the anchor at the entry's address
//	pin-hostkey --list                       every anchor, orphans flagged [orphan]
//	pin-hostkey <name> --fingerprint SHA256:… [--force]
//	pin-hostkey <name> --from-keyscan <file|-> [--force]
//	pin-hostkey <name> --clear               delete the anchor (idempotent)
//	pin-hostkey --clear --hostport <host>:<port>   orphan-anchor direct clear
//
// Every mutation runs through the single-transaction store primitives
// (UpsertManualPin / ClearPin) so the audit row is always an in-tx truth, and
// --force is the only override channel.
func serversPinHostkeyCmd() *cobra.Command {
	var (
		list        bool
		fingerprint string
		keyscan     string
		clear       bool
		force       bool
		hostport    string
	)
	c := &cobra.Command{
		Use:   "pin-hostkey [name]",
		Short: "Show, pin, or clear a server's host-key anchor",
		Long: `Show, pin, or clear a server's host-key anchor (Plan 48).

Forms:
  pin-hostkey <name>                     show fingerprint/format/source/device/created
  pin-hostkey --list                     list every anchor; anchors no entry points at are [orphan]
  pin-hostkey <name> --fingerprint SHA256:<base64> [--force]
  pin-hostkey <name> --from-keyscan <file|-> [--force]
  pin-hostkey <name> --clear             remove the anchor (idempotent, audited)
  pin-hostkey --clear --hostport <host>:<port>
                                         clear an anchor whose entry is gone or moved

--clear is mutually exclusive with --force/--fingerprint/--from-keyscan.
--force is the ONLY way to replace an existing pin.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			fingerprintSet := fl.Changed("fingerprint")
			keyscanSet := fl.Changed("from-keyscan")
			hostportSet := fl.Changed("hostport")

			// Flag discipline is decided BEFORE the vault opens — a malformed
			// invocation must never touch the store, let alone audit it.
			if clear {
				if force || fingerprintSet || keyscanSet {
					return fmt.Errorf("--clear is mutually exclusive with --force/--fingerprint/--from-keyscan")
				}
				if list {
					return fmt.Errorf("--clear is mutually exclusive with --list")
				}
				if hostportSet {
					if len(args) > 0 {
						return fmt.Errorf("--clear --hostport takes no positional argument")
					}
					host, port, err := parseHostPort(hostport)
					if err != nil {
						return err
					}
					s, err := openUnlockedStore()
					if err != nil {
						return err
					}
					defer s.Close()
					return runPinClear(cmd, s, host, port, "")
				}
				if len(args) != 1 {
					return fmt.Errorf("--clear needs a server name, or --clear --hostport <host>:<port> for an orphan anchor")
				}
			} else {
				if hostportSet {
					return fmt.Errorf("--hostport is only valid with --clear")
				}
				if list {
					if len(args) > 0 || force || fingerprintSet || keyscanSet {
						return fmt.Errorf("--list takes no other arguments")
					}
				} else {
					if len(args) == 0 {
						return fmt.Errorf("specify a server name to show its anchor, or use --list / --fingerprint / --from-keyscan / --clear")
					}
					if len(args) > 1 {
						return fmt.Errorf("exactly one server name is required, got %d", len(args))
					}
					if force && !fingerprintSet && !keyscanSet {
						return fmt.Errorf("--force requires --fingerprint or --from-keyscan")
					}
					if fingerprintSet && keyscanSet {
						return fmt.Errorf("--fingerprint and --from-keyscan are mutually exclusive — pick one input channel")
					}
				}
			}

			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()

			switch {
			case list:
				return runPinList(cmd, s)
			case clear:
				srv, err := s.GetServerByName(args[0])
				if err != nil {
					return err
				}
				if srv == nil {
					return fmt.Errorf("server %q not found", args[0])
				}
				return runPinClear(cmd, s, srv.Host, srv.Port, srv.ID)
			case fingerprintSet:
				srv, err := pinResolve(s, args[0])
				if err != nil {
					return err
				}
				blob, fp, err := parseFingerprintFlag(fingerprint)
				if err != nil {
					return err
				}
				// The stored value of a fingerprint anchor IS the canonical
				// fingerprint string (§5) — which is exactly `blob`.
				return runPinManual(cmd, s, srv, blob, store.PinFormatFingerprint, fp, "fingerprint", force)
			case keyscanSet:
				srv, err := pinResolve(s, args[0])
				if err != nil {
					return err
				}
				m, err := readKeyscan(keyscan, srv.Host, srv.Port)
				if err != nil {
					return err
				}
				if m.skipHashed > 0 || m.skipMarker > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "skipped %d hashed-hostname (|1|) and %d marker (@) lines (unsupported, not matched)\n",
						m.skipHashed, m.skipMarker)
				}
				return runPinManual(cmd, s, srv, m.blob, store.PinFormatBlob, m.fp, "keyscan", force)
			default:
				srv, err := pinResolve(s, args[0])
				if err != nil {
					return err
				}
				return runPinShow(cmd, s, srv)
			}
		},
	}
	c.Flags().BoolVar(&list, "list", false, "list every host-key anchor (anchors with no pointing entry are flagged [orphan])")
	c.Flags().StringVar(&fingerprint, "fingerprint", "", "pin by OpenSSH fingerprint SHA256:<base64, unpadded> (must decode to exactly 32 bytes)")
	c.Flags().StringVar(&keyscan, "from-keyscan", "", "pin from a known_hosts / ssh-keyscan file (\"-\" = stdin)")
	c.Flags().BoolVar(&clear, "clear", false, "remove the anchor at the address (idempotent; audited)")
	c.Flags().BoolVar(&force, "force", false, "replace an existing pin (the only override channel)")
	c.Flags().StringVar(&hostport, "hostport", "", "with --clear: the host:port address itself (orphan anchors, no entry needed)")
	return c
}

// pinResolve looks up the named server (shared first step of every name-form).
func pinResolve(s *store.Store, name string) (*models.Server, error) {
	srv, err := s.GetServerByName(name)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, fmt.Errorf("server %q not found", name)
	}
	return srv, nil
}

// pinHostPort is the anchor's storage key rendering for a server entry.
func pinHostPort(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// pinFingerprint renders an anchor's fingerprint for display and audit text: a
// fingerprint anchor IS the string; a blob anchor is parsed to compute it. A
// blob that no longer parses (hand-edited DB — no writer produces one)
// degrades to the same non-material placeholder the mismatch text uses.
func pinFingerprint(blob []byte, format string) string {
	if format == store.PinFormatFingerprint {
		return string(blob)
	}
	if pub, err := ssh.ParsePublicKey(blob); err == nil {
		return ssh.FingerprintSHA256(pub)
	}
	return "unparseable"
}

// dash renders an empty provenance field as "-".
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pinTime renders an anchor's registration time like the audit listing does.
func pinTime(unix int64) string {
	return time.Unix(unix, 0).Local().Format("2006-01-02 15:04:05-07:00")
}

// runPinShow implements the display form: fingerprint + format + source +
// forwarding device + registration time; an unpinned address is stated plainly.
func runPinShow(cmd *cobra.Command, s *store.Store, srv *models.Server) error {
	m, err := findPinMeta(s, srv.Host, srv.Port)
	if err != nil {
		return err
	}
	hostPort := pinHostPort(srv.Host, srv.Port)
	if m == nil {
		fmt.Fprintf(cmd.OutOrStdout(), "no pin at %s (unanchored — first trust or pin forwarding will create it)\n", hostPort)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "host=%s fp=%s (format=%s, source=%s, device=%s, created=%s)\n",
		hostPort, pinFingerprint(m.KeyBlob, m.PinFormat), m.PinFormat, m.PinSource, dash(m.PinDevice), pinTime(m.CreatedAt))
	return nil
}

// findPinMeta returns the anchor at host:port with its provenance (nil when
// unpinned). ListHostKeys is the provenance-carrying read; the point-read
// GetHostKey carries no source/device, so the CLI filters the dump to the one
// address (CLI-scale n, one indexed query).
func findPinMeta(s *store.Store, host string, port int) (*store.SnapshotHostKey, error) {
	want := pinHostPort(host, port)
	keys, err := s.ListHostKeys()
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].HostPort == want {
			return &keys[i], nil
		}
	}
	return nil, nil
}

// runPinList implements --list: every anchor, one line each, [orphan] on any
// anchor that no server row points at (cross-referenced against ListServers —
// the deleted/moved-entry poison anchors become discoverable, §3).
func runPinList(cmd *cobra.Command, s *store.Store) error {
	keys, err := s.ListHostKeys()
	if err != nil {
		return err
	}
	servers, err := s.ListServers()
	if err != nil {
		return err
	}
	pointed := make(map[string]bool, len(servers))
	for _, srv := range servers {
		pointed[pinHostPort(srv.Host, srv.Port)] = true
	}
	out := cmd.OutOrStdout()
	if len(keys) == 0 {
		fmt.Fprintln(out, "no pins recorded")
		return nil
	}
	for _, k := range keys {
		line := fmt.Sprintf("%s fp=%s (format=%s, source=%s, device=%s, created=%s)",
			k.HostPort, pinFingerprint(k.KeyBlob, k.PinFormat), k.PinFormat, k.PinSource, dash(k.PinDevice), pinTime(k.CreatedAt))
		if !pointed[k.HostPort] {
			line += " [orphan]"
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

// runPinManual lands --fingerprint / --from-keyscan pins through the
// single-transaction UpsertManualPin. The audit row is BUILT here (wording is
// a CLI contract) but WRITTEN by the store primitive inside the tx, so the
// was= fingerprint is the in-tx read and can never lie about what was replaced.
func runPinManual(cmd *cobra.Command, s *store.Store, srv *models.Server, blob []byte, format, fp, via string, force bool) error {
	hostPort := pinHostPort(srv.Host, srv.Port)
	auditFor := func(old *store.PinMeta) store.AuditRow {
		command := fmt.Sprintf("host=%s:%d fp=%s via=%s forced=%t", srv.Host, srv.Port, fp, via, force)
		if force && old != nil {
			command += " was=" + pinFingerprint(old.Blob, old.Format)
		}
		return store.AuditRow{
			TS:       time.Now(),
			ServerID: srv.ID,
			Action:   "pin-manual",
			Command:  command,
			Status:   "ok",
		}
	}
	old, err := s.UpsertManualPin(srv.Host, srv.Port, blob, format, force, auditFor)
	if err != nil {
		if errors.Is(err, store.ErrPinExists) {
			return fmt.Errorf("refusing to replace the existing pin for %s: fp=%s (source=%s) — pass --force to replace it",
				hostPort, pinFingerprint(old.Blob, old.Format), old.Source)
		}
		return err
	}
	out := cmd.OutOrStdout()
	if old != nil {
		fmt.Fprintf(out, "replacing existing pin fp=%s (source=%s)\n", pinFingerprint(old.Blob, old.Format), old.Source)
	}
	fmt.Fprintf(out, "pinned %s host=%s fp=%s (format=%s, source=manual, forced=%t)\n", srv.Name, hostPort, fp, format, force)
	return nil
}

// affectedNames is the owner full-vault view of "who shares this address"
// (ListServers — the same walk the pin-forward audit does, §1.2 ⑤): every
// entry whose host:port equals the anchor key, in ListServers (name) order.
func affectedNames(s *store.Store, host string, port int) ([]string, error) {
	all, err := s.ListServers()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, srv := range all {
		if srv.Host == host && srv.Port == port {
			names = append(names, srv.Name)
		}
	}
	return names, nil
}

// affectsAudit renders the audit-line affects fragment (key=value form,
// symmetric with the pin-forward audit's `affects=%d entries: %s`).
func affectsAudit(n int, names []string) string {
	if n == 0 {
		return "affects=0 entries"
	}
	return fmt.Sprintf("affects=%d entries: %s", n, strings.Join(names, ", "))
}

// runPinClear implements both clear forms (named entry and --hostport orphan):
// the global anchor row is removed through the single-transaction ClearPin;
// the deleted anchor's fingerprint/source and the affected-entry list go into
// BOTH the output line and the audit row (owner full view). Absent anchors are
// an idempotent success with no audit row.
func runPinClear(cmd *cobra.Command, s *store.Store, host string, port int, serverID string) error {
	affected, err := affectedNames(s, host, port)
	if err != nil {
		return err
	}
	auditFor := func(old store.PinMeta) store.AuditRow {
		return store.AuditRow{
			TS:       time.Now(),
			ServerID: serverID,
			Action:   "pin-clear",
			Command: fmt.Sprintf("host=%s:%d fp=%s source=%s %s",
				host, port, pinFingerprint(old.Blob, old.Format), old.Source, affectsAudit(len(affected), affected)),
			Status: "ok",
		}
	}
	old, err := s.ClearPin(host, port, auditFor)
	if err != nil {
		return err
	}
	hostPort := pinHostPort(host, port)
	if old == nil {
		fmt.Fprintf(cmd.OutOrStdout(), "no pin present at %s (nothing cleared)\n", hostPort)
		return nil
	}
	// The affected list is a clear-time snapshot (§3: entries may drift
	// afterwards; clear never touches entry rows, so the drift is bounded).
	// An empty list renders bare "affects 0 entries" (the orphan form's
	// expected shape — no dangling colon, no placeholder dash).
	suffix := fmt.Sprintf("affects %d entries", len(affected))
	if len(affected) > 0 {
		suffix = fmt.Sprintf("affects %d entries: %s", len(affected), strings.Join(affected, ", "))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "unpinned host=%s (was fp=%s, source=%s) — %s\n",
		hostPort, pinFingerprint(old.Blob, old.Format), old.Source, suffix)
	return nil
}

// parseFingerprintFlag enforces §3's strict double check: the OpenSSH form
// SHA256:<base64, unpadded> (whitespace trimmed) AND a decode to EXACTLY 32
// bytes (a SHA-256 digest) — a mistyped fingerprint is refused here instead of
// becoming a never-matching pin whose 409 recovery loop never converges
// (rev3). Returns the bytes to store (the canonical string itself) and the
// display string.
func parseFingerprintFlag(in string) ([]byte, string, error) {
	fp := strings.TrimSpace(in)
	rest, ok := strings.CutPrefix(fp, "SHA256:")
	if !ok {
		return nil, "", fmt.Errorf("--fingerprint must be the OpenSSH form SHA256:<base64, unpadded> (got %q)", fp)
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return nil, "", fmt.Errorf("--fingerprint base64 is invalid (%v) — expected the unpadded standard-alphabet form ssh-keygen -lf prints", err)
	}
	if len(raw) != 32 {
		return nil, "", fmt.Errorf("--fingerprint decodes to %d bytes, want exactly 32 (a SHA-256 digest) — is the string truncated or padded?", len(raw))
	}
	if base64.RawStdEncoding.EncodeToString(raw) != rest {
		return nil, "", fmt.Errorf("--fingerprint is not in canonical SHA256:<base64, unpadded> form — re-check for stray characters")
	}
	return []byte(fp), fp, nil
}

// parseHostPort splits the --hostport literal — the anchor key's own rendering
// (hostKeyID's host:port, port required).
func parseHostPort(in string) (string, int, error) {
	i := strings.LastIndex(in, ":")
	if i <= 0 || i == len(in)-1 {
		return "", 0, fmt.Errorf("--hostport must be <host>:<port>, got %q", in)
	}
	port, err := strconv.Atoi(in[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("--hostport must be <host>:<port> with a valid port, got %q", in)
	}
	return in[:i], port, nil
}

// knownHostsForm renders the entry address the way OpenSSH known_hosts spells
// it (§3 matching rule): bare host on port 22, "[host]:port" otherwise.
func knownHostsForm(host string, port int) string {
	if port == 22 {
		return host
	}
	return fmt.Sprintf("[%s]:%d", host, port)
}

// keyscanMatch is readKeyscan's outcome bundle.
type keyscanMatch struct {
	blob       []byte   // the matched key's SSH wire bytes (what a TOFU callback would see)
	fp         string   // its SHA256 fingerprint
	skipHashed int      // |1|-hashed-hostname lines (unsupported, never mis-matched)
	skipMarker int      // @cert-authority/@revoked marker lines (ditto)
	seenForms  []string // distinct pattern elements in matchable lines (no-match diagnostics)
}

// readKeyscan reads <file|-> and resolves the ONE distinct key matching the
// entry's known_hosts rendering. §3's pinned rule, verbatim: pattern elements
// are comma-split and compared by literal equality to the expected form; hashed
// and marker lines are skipped and counted; zero matches and multiple DISTINCT
// matches are both refusals (duplicate lines of the same key are one key).
func readKeyscan(path, host string, port int) (*keyscanMatch, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	expected := knownHostsForm(host, port)
	m := &keyscanMatch{}
	var matched []ssh.PublicKey // distinct, in file order
	seenForm := map[string]bool{}

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		first := line
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			first = line[:i]
		}
		// Marker and hashed lines put the marker/hash in the FIRST field
		// (before patterns) — detect them there, before the 3-field parser
		// would misread the marker as the patterns field.
		switch {
		case strings.HasPrefix(first, "@"):
			m.skipMarker++
			continue
		case strings.HasPrefix(first, "|1|"):
			m.skipHashed++
			continue
		}
		patterns, _, key, err := knownhosts.ParseKnownHostsLine(line)
		if err != nil {
			return nil, fmt.Errorf("malformed known_hosts line in %s: %v", path, err)
		}
		for _, el := range strings.Split(patterns, ",") {
			if el == "" {
				continue
			}
			if !seenForm[el] {
				seenForm[el] = true
				m.seenForms = append(m.seenForms, el)
			}
			if el != expected {
				continue
			}
			dup := false
			for _, mk := range matched {
				if bytes.Equal(mk.Marshal(), key.Marshal()) {
					dup = true
					break
				}
			}
			if !dup {
				matched = append(matched, key)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	switch len(matched) {
	case 0:
		return nil, fmt.Errorf("no known_hosts line matches %s in %s — forms found: [%s]; expected form: %s (bare host for port 22, [host]:port otherwise)",
			expected, path, strings.Join(m.seenForms, ", "), expected)
	case 1:
		m.blob = matched[0].Marshal()
		m.fp = ssh.FingerprintSHA256(matched[0])
		return m, nil
	default:
		fps := make([]string, 0, len(matched))
		for _, k := range matched {
			fps = append(fps, ssh.FingerprintSHA256(k))
		}
		// §3: guessing the algorithm bets on which key the server presents
		// next — wrong bet = a phantom mismatch. Point at the self-healing
		// loop instead (first-connect forwarding, or one chosen --fingerprint;
		// a real presented≠pinned mismatch names both fingerprints, --force
		// is the remedy).
		return nil, fmt.Errorf("%d different keys match %s in %s (%s) — ssh-keyscan emits one line per algorithm and the store keeps ONE pin per host:port, so guessing the algorithm bets on which key the server presents next. Self-heal: make a first connection from a machine that can reach the target (the pin is then forwarded automatically), or pick ONE algorithm from the scan output and pass it via --fingerprint; if that connection reports presented != pinned fingerprints (both are named in the error), --force replaces the pin",
			len(matched), expected, path, strings.Join(fps, ", "))
	}
}
