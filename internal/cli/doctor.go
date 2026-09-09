package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
)

// `doctor` is a local self-check (Plan 27): it READS local state (env seams,
// role.json, paths, key files) and prints a PASS/WARN/FAIL report with
// remediation hints. It never touches certs/cache, makes no network calls, and
// never prints secret VALUES (paths, sizes, counts, ages, and public
// fingerprints only). One deliberate, narrowly-scoped write exists (Plan 48
// rider 1): the vault-open probe first WAL-checkpoints the production store.db
// through a bare keyless connection (checkpointWALBare) so its byte-copy is
// complete — no key material read, no schema/ACL/row change, store.db never
// created.

// checkStatus is one check row's verdict. INFO = deliberate skip (e.g. no
// vault on a client machine), not a lesser WARN.
type checkStatus string

const (
	statusPass checkStatus = "PASS"
	statusWarn checkStatus = "WARN"
	statusFail checkStatus = "FAIL"
	statusInfo checkStatus = "INFO"
)

// doctorCheck is one line of the report. Detail is a single human-readable
// line WITHOUT any secret; Fix is the remediation and may be empty for
// PASS/INFO rows (the renderer only draws fix lines for WARN/FAIL anyway).
type doctorCheck struct {
	Name   string
	Status checkStatus
	Detail string
	Fix    string
}

// errDoctorFindings is returned (wrapped) when at least one check FAILed.
// WARN alone never changes the exit code.
var errDoctorFindings = errors.New("doctor: FAIL findings detected")

// doctorCheckFuncs is the checks table — T4 (serve cert/service, client
// cache) appends entries here. Every check is self-contained (the single
// deliberate write is checkVaultOpen's pre-copy WAL checkpoint — Plan 48
// rider 1, see checkpointWALBare); order only affects display.
var doctorCheckFuncs = []func() []doctorCheck{
	checkEnv,
	checkRole,
	checkVaultStore,
	checkVaultKey,
	checkVaultOpen,
	checkServeCert,
	checkServeSvc,
	checkClientCache,
}

// doctorEnvSeams is every SSHMGR_* env the CLI honors. Doctor reports which
// ones override defaults BY NAME ONLY — the values may be keys/tokens and
// must never reach the output.
var doctorEnvSeams = []string{
	"SSHMGR_STORE",
	"SSHMGR_FILEKEY_PATH",
	"SSHMGR_CACHE_DIR",
	"SSHMGR_CACHE_DEK",
	"SSHMGR_SERVE_CERT",
	"SSHMGR_SERVE_KEY",
	"SSHMGR_SERVE_MARKER",
	"SSHMGR_SERVE_LOG",
	"SSHMGR_CACHE_URL",
	"SSHMGR_CACHE_TOKEN",
	"SSHMGR_UPDATE_BASE",
	"SSHMGR_MASTERKEY_HEX",
}

// checkEnv reports the SSHMGR_* overrides in effect: the group gets one INFO
// line listing names; a set SSHMGR_MASTERKEY_HEX alone escalates that to a
// WARN — it is a dev/test affordance production must not rely on.
func checkEnv() []doctorCheck {
	var rows []doctorCheck
	if os.Getenv("SSHMGR_MASTERKEY_HEX") != "" {
		rows = append(rows, doctorCheck{
			Name:   "env",
			Status: statusWarn,
			Detail: "SSHMGR_MASTERKEY_HEX is set (dev/test affordance — production should not rely on it)",
			Fix:    "unset SSHMGR_MASTERKEY_HEX and provide the master key via the key file instead",
		})
	}
	var overridden []string
	for _, name := range doctorEnvSeams {
		if name == "SSHMGR_MASTERKEY_HEX" {
			continue // judged above
		}
		if os.Getenv(name) != "" {
			overridden = append(overridden, name)
		}
	}
	switch {
	case len(overridden) > 0:
		rows = append(rows, doctorCheck{
			Name:   "env",
			Status: statusInfo,
			Detail: "SSHMGR_* env overrides in effect: " + strings.Join(overridden, ", ") + " (values not shown)",
		})
	case len(rows) == 0:
		rows = append(rows, doctorCheck{
			Name:   "env",
			Status: statusPass,
			Detail: "no SSHMGR_* environment overrides in effect",
		})
	}
	return rows
}

// checkRole pins the machine's role.json state. Load() errors (corrupt file /
// invalid role value) are the one FAIL branch — the state roles.Load guides
// to `sshmgr clear` is exactly the broken machine doctor exists to
// catch. Fresh machine is INFO (points at the wizard), not a FAIL.
func checkRole() []doctorCheck {
	c := doctorCheck{Name: "role"}
	vaultP, verr := roles.RolePath(roles.RoleServer) // standalone/server share the vault-dir location
	clientP, cerr := roles.RolePath(roles.RoleClient)
	vaultPresent := verr == nil && roleFilePresent(vaultP)
	clientPresent := cerr == nil && roleFilePresent(clientP)

	st, err := roles.Load()
	switch {
	case err != nil:
		c.Status = statusFail
		c.Detail = fmt.Sprintf("role.json unreadable: %v", err)
		c.Fix = "run `sshmgr clear` (writes a vault safety-net backup first), then re-run the wizard (`sshmgr tui`)"
	case st == nil:
		c.Status = statusInfo
		c.Detail = "no role.json — fresh machine, run the wizard"
	case vaultPresent && clientPresent:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("dual-role residue: role.json at BOTH the vault dir and the user config dir — loaded role=%s setup_complete=%t", st.Role, st.SetupComplete)
		c.Fix = "keep the location for the current role and remove the other, or run `sshmgr clear` and re-run the wizard"
	case !st.SetupComplete:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("role=%s but wizard incomplete (setup_complete=false) — re-run to finish setup", st.Role)
		c.Fix = "run `sshmgr tui` to resume the wizard"
	default:
		c.Status = statusPass
		c.Detail = fmt.Sprintf("role=%s setup_complete=true", st.Role)
	}
	return []doctorCheck{c}
}

// roleFilePresent is fileExists narrowed to a bool for the dual-location
// probe (a stat error other than ErrNotExist counts as absent here — Load's
// own error path reports the real problem).
func roleFilePresent(p string) bool {
	ok, err := fileExists(p)
	return err == nil && ok
}

// vaultHoldingRole reports whether r is a role whose machine owns a local
// vault (store.db + master.key): standalone and server do. Client machines
// may legitimately be cache-only.
func vaultHoldingRole(r roles.Role) bool {
	return r == roles.RoleServer || r == roles.RoleStandalone
}

// doctorRole loads the role for the structural checks. A Load error (corrupt
// role.json) deliberately maps to "no usable role" here — the role check owns
// reporting that failure; the structural checks just fall back to their
// no-role branches.
func doctorRole() *roles.State {
	st, err := roles.Load()
	if err != nil {
		return nil
	}
	return st
}

// checkVaultStore Stats the vault database via the env-aware path. Doctor
// NEVER opens the store — store.Open creates store.db + runs the migration on
// the path it is given, which is exactly the side effect a diagnostic must
// not have; presence + size is the structural signal.
func checkVaultStore() []doctorCheck {
	c := doctorCheck{Name: "store"}
	p, err := paths.StorePath()
	if err != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("vault store path unresolvable: %v", err)
		c.Fix = "check the vault directory (platform vault root could not be resolved)"
		return []doctorCheck{c}
	}
	info, err := os.Stat(p)
	switch {
	case err == nil:
		c.Status = statusPass
		c.Detail = fmt.Sprintf("store.db present (%d bytes)", info.Size())
	case errors.Is(err, fs.ErrNotExist):
		switch st := doctorRole(); {
		case st != nil && vaultHoldingRole(st.Role):
			c.Status = statusFail
			c.Detail = fmt.Sprintf("store.db missing on a vault-holding machine (role=%s)", st.Role)
			c.Fix = "run `sshmgr unlock` or the setup wizard"
		case st != nil: // client
			c.Status = statusInfo
			c.Detail = "store.db absent on a client machine — cache-only is normal"
		default:
			c.Status = statusInfo
			c.Detail = "store.db absent — no usable role on this machine (see the role check)"
		}
	default:
		c.Status = statusFail
		c.Detail = fmt.Sprintf("store.db stat failed: %v", err)
		c.Fix = "check the vault directory permissions"
	}
	return []doctorCheck{c}
}

// inspectFileACL is the seam over store.InspectFileACL (serveServiceState
// precedent): tests stub it to drive the error branch, which cannot be seeded
// for real — a hardened user mask carries READ_CONTROL, so SD reads succeed.
var inspectFileACL = store.InspectFileACL

// checkVaultKey reads the master key file (env-aware path) and validates it
// STRUCTURALLY — same rationale as vaultStatusString (serve_service.go):
// store.Open is side-effecting, so ValidMasterKeyLen is the lightest faithful
// proxy for "the file is a usable AES-256 key". Unlike serve's LOCKED wording,
// doctor phrases its own remediation. The protection layer then splits by the
// InspectFileACL report, not by runtime.GOOS: on Windows the plaintext key's
// ACL is the layer (L1+ threat model), so the hardened shape is read back —
// an unreadable SD FAILs, a DACL/owner looser than the hardened shape WARNs;
// on other platforms mode bits are the layer, so loose group/world bits
// downgrade an otherwise-valid key to WARN.
func checkVaultKey() []doctorCheck {
	c := doctorCheck{Name: "masterkey"}
	p, err := paths.MasterKeyPath()
	if err != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("master key path unresolvable: %v", err)
		c.Fix = "check the vault directory (platform vault root could not be resolved)"
		return []doctorCheck{c}
	}
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		storePresent := false
		if sp, serr := paths.StorePath(); serr == nil {
			_, serr2 := os.Stat(sp)
			storePresent = serr2 == nil
		}
		switch st := doctorRole(); {
		case st != nil && vaultHoldingRole(st.Role):
			c.Status = statusFail
			c.Detail = fmt.Sprintf("master.key missing on a vault-holding machine (role=%s)", st.Role)
			c.Fix = "run `sshmgr unlock` or the setup wizard"
		case storePresent:
			c.Status = statusFail
			c.Detail = "master.key missing but store.db exists — the vault cannot be decrypted"
			c.Fix = "run `sshmgr unlock` (or restore master.key from backup)"
		case st != nil: // client
			c.Status = statusInfo
			c.Detail = "master.key absent on a client machine — no local vault to unlock"
		default:
			c.Status = statusInfo
			c.Detail = "master.key absent — no usable role on this machine (see the role check)"
		}
	case err != nil:
		c.Status = statusFail
		c.Detail = fmt.Sprintf("master.key unreadable: %v", err)
		c.Fix = "check the master.key file permissions"
	case !store.ValidMasterKeyLen(b):
		c.Status = statusFail
		c.Detail = fmt.Sprintf("master.key is %d bytes, expected 32 — corrupt or wrong file", len(b))
		c.Fix = "restore master.key from backup or re-run `sshmgr unlock`"
	default:
		c.Status = statusPass
		c.Detail = fmt.Sprintf("master.key present (%d bytes)", len(b))
		rep, aerr := inspectFileACL(p)
		switch {
		case aerr != nil:
			// Deep anomaly: hardened users hold READ_CONTROL, so an SD read
			// failure means the ACL was rewritten past legibility.
			c.Status = statusFail
			c.Detail = fmt.Sprintf("master.key ACL unreadable: %v", aerr)
			c.Fix = "inspect the file's security descriptor as admin (icacls <master.key>); restore the key from backup if the SD is corrupt"
		case !rep.Supported:
			// Non-Windows: mode bits are the layer — existing check, moved in.
			if info, serr := os.Stat(p); serr == nil && info.Mode().Perm()&0o077 != 0 {
				c.Status = statusWarn
				c.Detail = fmt.Sprintf("master.key present (%d bytes) but group/world readable (mode %o) — the plaintext key is protected by mode bits alone", len(b), info.Mode().Perm())
				c.Fix = "chmod 600 the master.key file (and 0700 its parent directory)"
			}
		case rep.TooLoose():
			c.Status = statusWarn
			c.Detail = aclLooseDetail(len(b), rep)
			c.Fix = aclLooseFix(rep)
		}
	}
	return []doctorCheck{c}
}

// probeVaultDecrypt copy-to-scratch-decrypts the vault: reads storePath+keyPath,
// copies both into a fresh scratch dir, store.Open's the COPY, ExportSnapshot()
// (decrypts EVERY credential — any key/ciphertext mismatch surfaces here), and
// removes the scratch. Never touches the originals beyond ReadFile.
// Returns server/credential counts. Error must not leak plaintext.
//
// Why a copy at all (NUC10 FINDING A, incident 2026-08-12): the vault's
// credentials were encrypted under key B while the machine held key A — every
// structural signal (files present, right sizes) was green, but the vault was
// undecryptable. A structural check cannot catch that; only a real decrypt
// can. But store.Open side effects (it CREATES store.db + runs the migration
// on the path it is given) mean the real decrypt must never run against the
// production files — hence the scratch copy.
//
// The copy is store.db alone, WITHOUT the WAL sidecars (-wal/-shm) — and it
// must stay that way: sidecars copied mid-write risk a torn copy, i.e. exactly
// the false verdict this diagnostic cannot afford. But a sidecar-less copy
// misses un-checkpointed frames (reads as an older consistent snapshot — an
// undercount; feedback #5: doctor said 11, `servers ls` said 12). So before
// this copy runs, checkVaultOpen folds the production store's WAL frames into
// store.db via checkpointStoreWAL (Plan 48 rider 1); when that cannot run
// (busy broker, read-only store) the probe degrades to exactly the
// older-snapshot read described above and the row says so in an INFO note.
func probeVaultDecrypt(storePath, keyPath string) (servers, creds int, err error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Name the class ourselves: os.ReadFile's raw message is
			// platform-dependent ("no such file or directory" vs "cannot find
			// the file specified").
			return 0, 0, fmt.Errorf("vault decrypt probe: master.key not found: %w", err)
		}
		return 0, 0, fmt.Errorf("vault decrypt probe: read master.key: %w", err)
	}
	blob, err := os.ReadFile(storePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, fmt.Errorf("vault decrypt probe: store.db not found: %w", err)
		}
		return 0, 0, fmt.Errorf("vault decrypt probe: read store.db: %w", err)
	}
	scratch, err := os.MkdirTemp("", "sshmgr-doctor-*")
	if err != nil {
		return 0, 0, fmt.Errorf("vault decrypt probe: scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)
	copyPath := filepath.Join(scratch, "store.db")
	if err := os.WriteFile(copyPath, blob, 0o600); err != nil {
		return 0, 0, fmt.Errorf("vault decrypt probe: write scratch copy: %w", err)
	}
	st, err := store.Open(copyPath, key)
	if err != nil {
		return 0, 0, fmt.Errorf("vault decrypt probe: %w", err)
	}
	defer st.Close()
	snap, err := st.ExportSnapshot()
	if err != nil {
		// ExportSnapshot wraps per-credential failures as
		// "decrypt credential <id>: <GCM error class>" (store/export.go) —
		// record IDs and cipher error classes only, never decrypted bytes.
		return 0, 0, fmt.Errorf("vault decrypt probe: %w", err)
	}
	return len(snap.Servers), len(snap.Credentials), nil
}

// checkpointWALBare folds the store's un-checkpointed WAL frames into store.db
// so the copy-probe's byte-copy is self-contained (Plan 48 rider 1, spec rev3
// §9.1; feedback #5 — a long-lived broker connection leaves committed rows in
// the -wal sidecar, and a sidecar-less copy read an older snapshot).
//
// Deliberately NOT store.Open: on the production path it creates store.db,
// runs the migration, and rewrites ACLs (see checkVaultStore) — a diagnostic
// must not. A bare database/sql connection needs no key (the vault encrypts
// credential VALUES, not the SQLite file) and runs exactly one PRAGMA. The
// open must be read-write: wal_checkpoint(TRUNCATE) writes folded pages back
// into the main file and truncates the -wal to zero. journal_mode is
// intentionally NOT set here — the production store is already WAL, and the
// pragma would silently CONVERT a non-WAL store. The -wal/-shm sidecars are
// never read or copied (torn-copy risk, see probeVaultDecrypt).
//
// Best-effort with visibility: the caller renders a failure as an INFO note
// and the probe degrades to the pre-rider older-snapshot read (undercount at
// worst, never a false verdict). No -wal sidecar (clean close) → nothing to
// fold, no-op. The busy_timeout matches store.Open so a transiently locked
// broker store waits instead of failing fast. Two closing notes: a blocked
// checkpoint is reported in wal_checkpoint's RESULT ROW (busy=1), not as an
// SQL error — the row is read, not discarded; and when doctor's connection is
// the only one attached, its clean close after the TRUNCATE removes the
// -wal/-shm sidecars entirely — benign, the frames were folded into the main
// file first.
func checkpointWALBare(storePath string) error {
	sidecar := storePath + "-wal"
	if _, err := os.Stat(sidecar); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // no WAL sidecar — no un-checkpointed frames to fold in
		}
		return fmt.Errorf("stat %s: %w", sidecar, err)
	}
	// Re-check the main file: in the window past the caller's existence gate it
	// could have been removed, and a bare open would then lazily create a fresh
	// empty store.db on the production path — doctor must never do that.
	if _, err := os.Stat(storePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", storePath, err)
	}
	db, err := sql.Open("sqlite", storePath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	// wal_checkpoint reports a blocked/partial run in its RESULT ROW (first
	// column), not as an SQL error — Exec would discard the row and a busy
	// broker would take the success path with a silently partial fold-in
	// (checkpointed < log pages, -wal not truncated). Read all three columns
	// and map busy≠0 onto the caller's degrade path.
	var busy, walPages, ckpt int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walPages, &ckpt); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint busy: a reader/writer still holds the wal (%d of %d pages folded)", ckpt, walPages)
	}
	return nil
}

// checkpointStoreWAL is the seam over checkpointWALBare (inspectFileACL
// precedent): tests stub it to drive the checkpoint-failure branch, which
// cannot be seeded portably (a real TRUNCATE-checkpoint busy needs a racing
// reader holding the WAL open).
var checkpointStoreWAL = checkpointWALBare

// checkVaultOpen is the FINDING A detector: the one doctor row that PROVES
// the vault decrypts, not merely that it structurally exists. Skips (INFO)
// when either input is absent or the key fails the structural length check —
// the T2 store/masterkey rows own reporting those — because a probe on an
// empty vault under a wrong-length key derives a different DEK via HKDF but
// has nothing to decrypt, i.e. it would report a misleading PASS.
//
// Plan 48 rider 1: before probing, the production store's un-checkpointed WAL
// frames are folded into store.db (checkpointStoreWAL) so the byte-copy below
// counts every committed row. That fold-in is the row's single deliberate
// write; a failure is best-effort — an INFO note, then the probe reads the
// older snapshot as before the rider.
func checkVaultOpen() []doctorCheck {
	c := doctorCheck{Name: "vault-open"}
	storeP, serr := paths.StorePath()
	keyP, kerr := paths.MasterKeyPath()
	if serr != nil || kerr != nil {
		c.Status = statusInfo
		c.Detail = "skipped — vault paths unresolvable (see the store/masterkey rows)"
		return []doctorCheck{c}
	}
	storePresent, serr2 := fileExists(storeP)
	keyBytes, rerr := os.ReadFile(keyP)
	switch {
	case serr2 != nil:
		// Exists-but-unstatable: T2's store row FAILs the real problem; do not
		// claim "not present" for a file that may well be there.
		c.Status = statusInfo
		c.Detail = "skipped — store.db not statable (see the store row)"
	case !storePresent || errors.Is(rerr, fs.ErrNotExist):
		c.Status = statusInfo
		c.Detail = "skipped — store.db/master.key not both present"
	case rerr != nil:
		// Present but unreadable: T2's masterkey row FAILs the real problem;
		// do not claim "not present" for a file that is there.
		c.Status = statusInfo
		c.Detail = "skipped — master.key unreadable (see the masterkey row)"
	case !store.ValidMasterKeyLen(keyBytes):
		c.Status = statusInfo
		c.Detail = "skipped — master.key not a valid 32-byte key (see the masterkey row)"
	default:
		var rows []doctorCheck
		if cerr := checkpointStoreWAL(storeP); cerr != nil {
			// Visibility, not a verdict: a busy broker or a read-only store
			// leaves the fold-in undone and the probe reads the same
			// older-snapshot it read before the rider — undercount at worst,
			// never a false PASS/FAIL — so the note is INFO.
			rows = append(rows, doctorCheck{
				Name:   "vault-open",
				Status: statusInfo,
				Detail: fmt.Sprintf("pre-copy WAL checkpoint skipped (%v) — copy-probe counts may reflect an older snapshot", cerr),
			})
		}
		servers, creds, perr := probeVaultDecrypt(storeP, keyP)
		if perr != nil {
			c.Status = statusFail
			c.Detail = fmt.Sprintf("vault fails to decrypt under the current master key: %v", perr)
			c.Fix = "key/ciphertext mismatch — restore from backup (.sme) or re-unlock + import; see docs/backup-restore.md"
		} else {
			c.Status = statusPass
			c.Detail = fmt.Sprintf("copy-probe decrypted %d servers / %d credentials", servers, creds)
		}
		rows = append(rows, c)
		return rows
	}
	return []doctorCheck{c}
}

// checkServeCert reports the serve TLS cert via mcpserver.ReadServeCertFingerprint
// — the READ-ONLY twin of LoadOrCreateServeCert, so the doctor path itself can
// never generate. Both cert and marker absent → INFO "serve not in use" (a
// machine that never ran serve is healthy, not broken); otherwise the twin
// resolves the fingerprint: PASS carries it (public info — every client
// receives it on connect anyway), an error FAILs with the twin's text as
// Detail (covers the corrupt-keypair refusal AND the F10 out-of-band refusal,
// both of which already embed their recovery steps).
func checkServeCert() []doctorCheck {
	c := doctorCheck{Name: "serve-cert"}
	certP, err := paths.ServeCertPath()
	if err != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("serve cert path unresolvable: %v", err)
		c.Fix = "check the vault directory (platform vault root could not be resolved)"
		return []doctorCheck{c}
	}
	markerP, merr := paths.ServeCertMarkerPath()
	if merr != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("serve cert marker path unresolvable: %v", merr)
		c.Fix = "check the vault directory (platform vault root could not be resolved)"
		return []doctorCheck{c}
	}
	certOK, cerr := fileExists(certP)
	markerOK, merr2 := fileExists(markerP)
	if cerr != nil || merr2 != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("serve cert stat failed: cert=%v marker=%v", cerr, merr2)
		c.Fix = "check the serve cert directory permissions"
		return []doctorCheck{c}
	}
	if !certOK && !markerOK {
		c.Status = statusInfo
		c.Detail = "serve not in use (no serve cert and no init marker)"
		return []doctorCheck{c}
	}
	_, _, fp, rerr := mcpserver.ReadServeCertFingerprint()
	if rerr != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("serve cert problem: %v", rerr)
		c.Fix = "follow the recovery steps in the detail above, then verify with `sshmgr serve cert-info`"
		return []doctorCheck{c}
	}
	c.Status = statusPass
	c.Detail = fmt.Sprintf("serve cert present (fingerprint %s)", fp)
	return []doctorCheck{c}
}

// serveServiceState is the seam over the kardianos service-manager query:
// runServeStatus's svc.Status() five-state mapping reduced to one string
// ("Running" / "Stopped" / "NOT INSTALLED" / "Unknown (...)" / "Unknown").
// ErrNoServiceSystemDetected maps to NOT INSTALLED too — simpler for doctor,
// which only distinguishes installed-vs-not, and the status command keeps the
// finer no-service-manager wording. A var so tests stub the SCM (a diagnostic
// test must not touch the host's real service manager).
var serveServiceState = func() string {
	cfg := &service.Config{Name: serveServiceName, Option: platformServiceOptions()}
	s, err := service.New(&program{}, cfg)
	if err != nil {
		if errors.Is(err, service.ErrNoServiceSystemDetected) {
			return "NOT INSTALLED"
		}
		return "Unknown (" + err.Error() + ")"
	}
	status, serr := s.Status()
	switch {
	case serr != nil && errors.Is(serr, service.ErrNotInstalled):
		return "NOT INSTALLED"
	case serr != nil:
		return "Unknown (" + serr.Error() + ")"
	case status == service.StatusRunning:
		return "Running"
	case status == service.StatusStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

// checkServeSvc reports the registered serve service's state. Running → PASS;
// Stopped → WARN (the broker machine's MCP endpoint is down); NOT INSTALLED
// splits by role — INFO off-server (standalone/client machines never promised
// a serve service) but WARN on a server-role machine, whose whole purpose is
// the broker; anything indeterminate → WARN rather than a guess.
func checkServeSvc() []doctorCheck {
	c := doctorCheck{Name: "serve-svc"}
	switch state := serveServiceState(); {
	case state == "Running":
		c.Status = statusPass
		c.Detail = "serve service running"
	case state == "Stopped":
		c.Status = statusWarn
		c.Detail = "serve service installed but stopped"
		c.Fix = "re-run `sshmgr serve install` (idempotent install + start), or start it via the OS service manager"
	case state == "NOT INSTALLED":
		if st := doctorRole(); st != nil && st.Role == roles.RoleServer {
			c.Status = statusWarn
			c.Detail = "serve service not installed on a server-role machine"
			c.Fix = "run `sshmgr serve install`"
		} else {
			c.Status = statusInfo
			c.Detail = "serve service not installed (serve not in use)"
		}
	default:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("serve service state indeterminate: %s", state)
		c.Fix = "inspect with `sshmgr serve status` and the service manager's own tooling"
	}
	return []doctorCheck{c}
}

// checkClientCache reports the offline client cache. cache.bin present → the
// sidecar matrix: DEK missing → FAIL (the cache cannot be decrypted — the
// client-side FINDING A class), cache.auth.json missing → WARN (cache works
// offline but never auto-refreshes), else PASS with the snapshot's age.
// cache.bin missing splits by role: FAIL on a client machine (the cache IS
// its vault), INFO elsewhere.
func checkClientCache() []doctorCheck {
	c := doctorCheck{Name: "client-cache"}
	dir, bin, _, _, err := clientops.CachePaths()
	if err != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("cache dir unresolvable: %v", err)
		c.Fix = "check the user config dir (os.UserConfigDir could not be resolved)"
		return []doctorCheck{c}
	}
	info, serr := os.Stat(bin)
	switch {
	case errors.Is(serr, fs.ErrNotExist):
		if st := doctorRole(); st != nil && st.Role == roles.RoleClient {
			c.Status = statusFail
			c.Detail = "cache.bin missing on a client machine — no offline vault"
			c.Fix = "run `sshmgr cache pull`"
		} else {
			c.Status = statusInfo
			c.Detail = "cache.bin absent (no offline cache on this machine)"
		}
		return []doctorCheck{c}
	case serr != nil:
		c.Status = statusFail
		c.Detail = fmt.Sprintf("cache.bin stat failed: %v", serr)
		c.Fix = "check the cache directory permissions"
		return []doctorCheck{c}
	}
	age := time.Since(info.ModTime()).Round(time.Minute)

	dekP, derr := paths.CacheDekPath()
	if derr != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("cache DEK path unresolvable: %v", derr)
		c.Fix = "check the vault directory (platform vault root could not be resolved)"
		return []doctorCheck{c}
	}
	dekOK, dok := fileExists(dekP)
	if dok != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("cache DEK stat failed: %v", dok)
		c.Fix = "check the cache DEK file permissions"
		return []doctorCheck{c}
	}
	if !dekOK {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("cache.bin present (age %s) but its cache DEK is missing — cache undecryptable", age)
		c.Fix = "re-run the client wizard (`sshmgr tui`) and `cache pull` again to re-establish a decryptable cache"
		return []doctorCheck{c}
	}

	authOK, aok := fileExists(filepath.Join(dir, "cache.auth.json"))
	if aok != nil {
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("cache.bin present (age %s) but cache.auth.json unstatable: %v", age, aok)
		c.Fix = "check the cache directory permissions"
		return []doctorCheck{c}
	}
	if !authOK {
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("cache.bin present (age %s) but no auto-refresh credential (manual `cache pull` only)", age)
		c.Fix = "run `sshmgr cache pull` to persist cache.auth.json (enables auto-refresh)"
		return []doctorCheck{c}
	}
	c.Status = statusPass
	c.Detail = fmt.Sprintf("cache.bin present (age %s)", age)
	return []doctorCheck{c}
}

// runDoctor executes every check, renders the report, and returns an error
// when FAIL findings are detected (wrapped errDoctorFindings).
func runDoctor(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "sshmgr doctor (%s)\n", buildinfo.Version)
	var warn, fail int
	for _, check := range doctorCheckFuncs {
		for _, c := range check() {
			fmt.Fprintf(out, "%s:  %s  %s\n", c.Name, c.Status, c.Detail)
			if c.Fix != "" && (c.Status == statusWarn || c.Status == statusFail) {
				fmt.Fprintf(out, "       fix: %s\n", c.Fix)
			}
			switch c.Status {
			case statusWarn:
				warn++
			case statusFail:
				fail++
			}
		}
	}
	fmt.Fprintf(out, "overall: %d WARN, %d FAIL\n", warn, fail)
	if fail > 0 {
		return NewExitCodeError(1, fmt.Errorf("%w (%d) — see the report above", errDoctorFindings, fail))
	}
	return nil
}

// aclLooseDetail renders the WARN Detail per the frozen clause table (spec
// rev3 §2.1): one clause per triggered signal, semicolon-joined, common tail,
// advisory parenthetical when inheritance is live.
func aclLooseDetail(keyBytes int, rep store.FileACLReport) string {
	var parts []string
	if rep.DaclNull {
		parts = append(parts, fmt.Sprintf(
			"master.key present (%d bytes) but it has no DACL — every principal is allowed", keyBytes))
	}
	if len(rep.UnexpectedReadGrantors) > 0 {
		parts = append(parts, fmt.Sprintf(
			"master.key present (%d bytes) but its DACL grants access to unexpected principals: %s",
			keyBytes, strings.Join(rep.UnexpectedReadGrantors, ", ")))
	}
	if rep.OwnerUnexpected {
		parts = append(parts, fmt.Sprintf(
			"master.key present (%d bytes) but the file owner is %s — the owner can typically rewrite the DACL",
			keyBytes, rep.OwnerSID))
	}
	detail := strings.Join(parts, "; ") + " — the plaintext key is protected by this ACL alone"
	// Invariant: Protected is never populated on DaclNull reports
	// (InspectFileACL early-returns before reaching the Protected read), so
	// this parenthetical only ever renders on grantor/owner WARNs — a null
	// DACL has no parent-ACE inheritance question. Intentional; do not "fix".
	if !rep.Protected {
		detail += " (inheritance also enabled)"
	}
	return detail
}

// aclLooseFix renders the WARN Fix per the frozen clause table (spec rev3
// §2.1): icacls segments joined in owner→inheritance/grants order, all
// asterisk-SID form (account names localize on non-English Windows).
func aclLooseFix(rep store.FileACLReport) string {
	var segs []string
	if rep.OwnerUnexpected {
		segs = append(segs, "/setowner *S-1-5-32-544")
	}
	if len(rep.UnexpectedReadGrantors) > 0 {
		segs = append(segs, "/inheritance:r", "/remove:g <SIDs...>")
	}
	if rep.DaclNull || len(rep.UnexpectedReadGrantors) > 0 {
		segs = append(segs, "/grant:r *S-1-5-18:(F) *S-1-5-32-544:(F) *<you-SID>:(RC,R,W,D)")
	}
	return "icacls <master.key> " + strings.Join(segs, " ") +
		" — replace <SIDs...> with the principals listed above (asterisk-prefixed SID form, e.g. *S-1-1-0) and <you-SID> with your own SID (`whoami /user`)"
}

func newDoctorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Local self-check with PASS/WARN/FAIL findings",
		Long: `Run a local self-check and print a PASS/WARN/FAIL report with remediation
hints. Checks are read-only with one narrow carve-out: the vault decrypt probe
first WAL-checkpoints the production store.db (bare, keyless connection — no
key read, no schema or ACL change) so its byte-copy counts every committed row.
Doctor never creates the vault, never touches certificates or the client
cache, makes no network calls, and never prints secret values — environment
overrides are reported by name only.

Exit codes (stable, for scripts): 0 = no FAIL findings (warnings allowed),
1 = at least one FAIL finding.`,
		Args: cobra.NoArgs,
		RunE: runDoctor,
	}
	return c
}
