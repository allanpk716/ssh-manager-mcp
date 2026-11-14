package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/knownhosts"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// servers pin-hostkey（票 #58）：owner 的带外锚通道。对象是服务器条目：
// 一个条目一把锚，条目的全部候选地址共用。五种形态：
//
//	pin-hostkey <name>                       显示该条目的锚
//	pin-hostkey --list                       按条目列出全部锚与握手地址
//	pin-hostkey <name> --fingerprint SHA256:… [--force]
//	pin-hostkey <name> --from-keyscan <file|-> [--force]
//	pin-hostkey <name> --clear               删除该条目的锚（幂等、写审计）
//
// 所有变更走存储层的单事务原语（UpsertManualEntryPin 系列 / ClearEntryPin），
// --force 是唯一覆盖入口。
func serversPinHostkeyCmd() *cobra.Command {
	var (
		list        bool
		fingerprint string
		keyscan     string
		keyType     string
		clear       bool
		force       bool
	)
	c := &cobra.Command{
		Use:   "pin-hostkey [name]",
		Short: "Show, pin, or clear a server entry's host-key anchor",
		Long: `Show, pin, or clear a server entry's host-key anchor (spec #88).

The anchor follows the server entry: one anchor per entry, shared by all of
the entry's candidate SSH addresses. Changing the entry's host or port keeps
the anchor; deleting the entry removes it.

Forms:
  pin-hostkey <name>                     show fingerprint/key-type/format/source/device/handshake/created
  pin-hostkey --list                     list every entry anchor with key type and handshake address
  pin-hostkey <name> --fingerprint SHA256:<base64> [--key-type ed25519|ecdsa|rsa] [--force]
  pin-hostkey <name> --from-keyscan <file|-> [--force]
  pin-hostkey <name> --clear             remove the entry's anchor (idempotent, audited)

--clear is mutually exclusive with --force/--fingerprint/--from-keyscan/--key-type.
--force is the ONLY way to replace an existing pin.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			fingerprintSet := fl.Changed("fingerprint")
			keyscanSet := fl.Changed("from-keyscan")
			keyTypeSet := fl.Changed("key-type")

			// 参数组合检查在打开 vault 之前判定：错误调用绝不触库、绝不写审计。
			if clear {
				if force || fingerprintSet || keyscanSet || keyTypeSet {
					return fmt.Errorf("--clear is mutually exclusive with --force/--fingerprint/--from-keyscan/--key-type")
				}
				if list {
					return fmt.Errorf("--clear is mutually exclusive with --list")
				}
				if len(args) != 1 {
					return fmt.Errorf("--clear needs a server name (the anchor follows the entry)")
				}
			} else if list {
				if len(args) > 0 || force || fingerprintSet || keyscanSet || keyTypeSet {
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
				if keyTypeSet && !fingerprintSet {
					return fmt.Errorf("--key-type requires --fingerprint")
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
				srv, err := pinResolve(s, args[0])
				if err != nil {
					return err
				}
				return runPinClear(cmd, s, srv)
			case fingerprintSet:
				srv, err := pinResolve(s, args[0])
				if err != nil {
					return err
				}
				blob, fp, err := parseFingerprintFlag(fingerprint)
				if err != nil {
					return err
				}
				keyAlgorithm, err := store.ParsePinKeyAlgorithm(keyType)
				if err != nil {
					return err
				}
				// 指纹锚没有握手地址可学，握手地址记空（展示为 "-"）。
				return runPinManual(cmd, s, srv, "", blob, store.PinFormatFingerprint,
					fp, "fingerprint", "", keyAlgorithm, force)
			case keyscanSet:
				srv, err := pinResolve(s, args[0])
				if err != nil {
					return err
				}
				m, err := readKeyscan(keyscan, srv.Host, srv.Port)
				if err != nil {
					return err
				}
				note := ""
				if m.skipHashed > 0 || m.skipMarker > 0 {
					note = fmt.Sprintf("skipped %d hashed-hostname (|1|) and %d marker (@) lines\n", m.skipHashed, m.skipMarker)
				}
				return runPinManual(cmd, s, srv, store.EntryFallbackAddr(srv), m.blob,
					store.PinFormatBlob, m.fp, "keyscan", note, "", force)
			default:
				srv, err := pinResolve(s, args[0])
				if err != nil {
					return err
				}
				return runPinShow(cmd, s, srv)
			}
		},
	}
	c.Flags().BoolVar(&list, "list", false, "list every entry anchor with its handshake address")
	c.Flags().StringVar(&fingerprint, "fingerprint", "", "pin by OpenSSH fingerprint SHA256:<base64, unpadded> (must decode to exactly 32 bytes)")
	c.Flags().StringVar(&keyscan, "from-keyscan", "", "pin from a known_hosts / ssh-keyscan file (\"-\" = stdin)")
	c.Flags().StringVar(&keyType, "key-type", "", "host-key family for --fingerprint: ed25519, ecdsa, or rsa (omit to learn it on first connect)")
	c.Flags().BoolVar(&clear, "clear", false, "remove the entry's anchor (idempotent; audited)")
	c.Flags().BoolVar(&force, "force", false, "replace an existing pin (the only override channel)")
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

// runPinShow 实现显示形态：指纹、算法、格式、来源、转发设备、握手地址与时间。
func runPinShow(cmd *cobra.Command, s *store.Store, srv *models.Server) error {
	m, err := s.GetEntryPinMeta(srv.ID)
	if err != nil {
		return err
	}
	if m == nil {
		fmt.Fprintf(cmd.OutOrStdout(), "no pin for %s (unanchored — first trust or pin forwarding will create it)\n", srv.Name)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "entry=%s host=%s fingerprint=%s (key-type=%s, format=%s, source=%s, device=%s, handshake=%s, created=%s)\n",
		srv.Name, store.EntryFallbackAddr(srv), store.PinFingerprint(m.Blob, m.Format),
		m.DisplayKeyAlgorithm(), m.Format, m.Source, dash(m.Device), dash(m.HandshakeAddr), pinTime(m.CreatedAt))
	return nil
}

// runPinList 实现 --list：按条目一行，显示每把锚的算法与学习地址。
func runPinList(cmd *cobra.Command, s *store.Store) error {
	pins, err := s.ListEntryPins()
	if err != nil {
		return err
	}
	servers, err := s.ListServers()
	if err != nil {
		return err
	}
	byID := make(map[string]*models.Server, len(servers))
	for _, srv := range servers {
		byID[srv.ID] = srv
	}
	out := cmd.OutOrStdout()
	if len(pins) == 0 {
		fmt.Fprintln(out, "no pins recorded")
		return nil
	}
	for _, p := range pins {
		srv := byID[p.ServerID]
		name, hostPort := p.ServerID, p.HandshakeAddr
		if srv != nil {
			name, hostPort = srv.Name, store.EntryFallbackAddr(srv)
		}
		fmt.Fprintf(out, "entry=%s host=%s fingerprint=%s (key-type=%s, format=%s, source=%s, device=%s, handshake=%s, created=%s)\n",
			name, hostPort, store.PinFingerprint(p.Blob, p.Format), p.DisplayKeyAlgorithm(),
			p.Format, p.Source, dash(p.Device), dash(p.HandshakeAddr), pinTime(p.CreatedAt))
	}
	return nil
}

// runPinManual 落 --fingerprint / --from-keyscan 锚：单事务原语，审计行在
// 这里构造、在事务里写入，was= 指纹是事务内读到的旧锚。
func runPinManual(
	cmd *cobra.Command, s *store.Store, srv *models.Server, handshakeAddr string,
	blob []byte, format, fp, via, note, keyAlgorithm string, force bool,
) error {
	hostPort := store.EntryFallbackAddr(srv)
	auditFor := func(old *store.PinMeta) store.AuditRow {
		command := fmt.Sprintf("entry=%s host=%s fingerprint=%s via=%s forced=%t", srv.Name, hostPort, fp, via, force)
		if force && old != nil {
			command += " was=" + store.PinFingerprint(old.Blob, old.Format)
		}
		return store.AuditRow{
			TS:       time.Now(),
			ServerID: srv.ID,
			Action:   "pin-manual",
			Command:  command,
			Status:   "ok",
		}
	}
	old, err := s.UpsertManualEntryPinWithAlgorithm(
		srv, handshakeAddr, blob, format, keyAlgorithm, force, auditFor,
	)
	if err != nil {
		if errors.Is(err, store.ErrPinExists) {
			return fmt.Errorf("refusing to replace the existing pin for %s: fingerprint=%s (source=%s) — pass --force to replace it",
				srv.Name, store.PinFingerprint(old.Blob, old.Format), old.Source)
		}
		return err
	}
	out := cmd.OutOrStdout()
	if note != "" {
		fmt.Fprint(out, note)
	}
	if old != nil {
		fmt.Fprintf(out, "replacing existing pin fingerprint=%s (source=%s)\n", store.PinFingerprint(old.Blob, old.Format), old.Source)
	}
	fmt.Fprintf(out, "pinned %s host=%s fingerprint=%s (format=%s, source=manual, handshake=%s, forced=%t)\n",
		srv.Name, hostPort, fp, format, dash(handshakeAddr), force)
	if keyAlgorithm != "" {
		fmt.Fprintf(out, "key-type=%s\n", keyAlgorithm)
	}
	return nil
}

// runPinClear 按条目删除锚：单事务原语，输出与审计带条目、指纹、来源与
// 握手地址；无锚时幂等成功且不写审计。
func runPinClear(cmd *cobra.Command, s *store.Store, srv *models.Server) error {
	auditFor := func(old store.PinMeta) store.AuditRow {
		return store.AuditRow{
			TS:       time.Now(),
			ServerID: srv.ID,
			Action:   "pin-clear",
			Command: fmt.Sprintf("entry=%s host=%s fingerprint=%s source=%s handshake=%s",
				srv.Name, store.EntryFallbackAddr(srv), store.PinFingerprint(old.Blob, old.Format), old.Source, dash(old.HandshakeAddr)),
			Status: "ok",
		}
	}
	old, err := s.ClearEntryPin(srv.ID, auditFor)
	if err != nil {
		return err
	}
	if old == nil {
		fmt.Fprintf(cmd.OutOrStdout(), "no pin for %s (nothing cleared)\n", srv.Name)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "unpinned %s host=%s (was fingerprint=%s, source=%s, handshake=%s)\n",
		srv.Name, store.EntryFallbackAddr(srv), store.PinFingerprint(old.Blob, old.Format), old.Source, dash(old.HandshakeAddr))
	return nil
}

// parseFingerprintFlag enforces the strict double check via the shared store
// validator so the command line and the Web UI accept the same fingerprints.
func parseFingerprintFlag(in string) ([]byte, string, error) {
	return store.ParseFingerprintPin(in)
}

// knownHostsForm renders the entry address the way OpenSSH known_hosts spells
// it: bare host on port 22, "[host]:port" otherwise.
func knownHostsForm(host string, port int) string {
	if port == 22 {
		return host
	}
	return fmt.Sprintf("[%s]:%d", host, port)
}

// keyscanMatch is readKeyscan's outcome bundle.
type keyscanMatch struct {
	blob       []byte   // the matched key's SSH wire bytes
	fp         string   // its SHA256 fingerprint
	skipHashed int      // |1|-hashed-hostname lines (unsupported, never mis-matched)
	skipMarker int      // @cert-authority/@revoked marker lines (ditto)
	seenForms  []string // distinct pattern elements in matchable lines (no-match diagnostics)
}

// readKeyscan reads <file|-> and resolves the ONE distinct key matching the
// entry's known_hosts rendering. Pattern elements are comma-split and compared
// by literal equality to the expected form; hashed and marker lines are skipped
// and counted; zero matches and multiple DISTINCT matches are both refusals.
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
		return nil, fmt.Errorf("%d different keys match %s in %s (%s) — ssh-keyscan emits one line per algorithm and the entry keeps ONE pin, so guessing the algorithm bets on which key the server presents next. Self-heal: make a first connection from a machine that can reach the target (the pin is then forwarded automatically), or pick ONE algorithm from the scan output and pass it via --fingerprint; if that connection reports presented != pinned fingerprints (both are named in the error), --force replaces the pin",
			len(matched), expected, path, strings.Join(fps, ", "))
	}
}
