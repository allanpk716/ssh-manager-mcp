package cli

import (
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

// `doctor` is a side-effect-free local self-check (Plan 27): it READS local
// state (env seams, role.json, paths, key files) and prints a PASS/WARN/FAIL
// report with remediation hints. It never writes the vault/certs/cache, makes
// no network calls, and never prints secret VALUES (paths, sizes, counts,
// ages, and public fingerprints only).

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
// cache) appends entries here. Every check is self-contained and
// side-effect-free; order only affects display.
var doctorCheckFuncs = []func() []doctorCheck{
	checkEnv,
	checkRole,
	checkVaultStore,
	checkVaultKey,
	checkVaultOpen,
	checkServeCert,
	checkServeSvc,
	checkClientCache,
	checkClientCacheInstances,
}

// doctorEnvSeams is every SSHMGR_* env the CLI honors. Doctor reports which
// ones override defaults BY NAME ONLY — the values may be keys/tokens and
// must never reach the output.
var doctorEnvSeams = []string{
	"SSHMGR_STORE",
	"SSHMGR_FILEKEY_PATH",
	"SSHMGR_CACHE_DIR",
	"SSHMGR_CACHE_DEK",
	"SSHMGR_CACHE_DEK_DIR",
	"SSHMGR_CACHE_MAX_OFFLINE",
	"SSHMGR_SERVE_CERT",
	"SSHMGR_SERVE_KEY",
	"SSHMGR_SERVE_MARKER",
	"SSHMGR_SERVE_LOG",
	"SSHMGR_CACHE_URL",
	"SSHMGR_CACHE_TOKEN",
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
// to `ssh-manager clear` is exactly the broken machine doctor exists to
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
		c.Fix = "run `ssh-manager clear` (writes a vault safety-net backup first), then re-run the wizard (`ssh-manager tui`)"
	case st == nil:
		c.Status = statusInfo
		c.Detail = "no role.json — fresh machine, run the wizard"
	case vaultPresent && clientPresent:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("dual-role residue: role.json at BOTH the vault dir and the user config dir — loaded role=%s setup_complete=%t", st.Role, st.SetupComplete)
		c.Fix = "keep the location for the current role and remove the other, or run `ssh-manager clear` and re-run the wizard"
	case !st.SetupComplete:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("role=%s but wizard incomplete (setup_complete=false) — re-run to finish setup", st.Role)
		c.Fix = "run `ssh-manager tui` to resume the wizard"
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
			c.Fix = "run `ssh-manager unlock` or the setup wizard"
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
			c.Fix = "run `ssh-manager unlock` or the setup wizard"
		case storePresent:
			c.Status = statusFail
			c.Detail = "master.key missing but store.db exists — the vault cannot be decrypted"
			c.Fix = "run `ssh-manager unlock` (or restore master.key from backup)"
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
		c.Fix = "restore master.key from backup or re-run `ssh-manager unlock`"
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
// The copy is store.db alone, WITHOUT the WAL sidecars (-wal/-shm): a
// concurrent writer's un-checkpointed frames are simply absent, which reads
// as an older consistent snapshot — in realistic write patterns an
// undercount, not a false verdict; a full re-seal through a long-lived
// un-checkpointed broker connection could transiently mis-verdict. (Copying
// -wal mid-write would risk a torn copy, i.e. exactly the false verdict this
// diagnostic cannot afford.)
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

// checkVaultOpen is the FINDING A detector: the one doctor row that PROVES
// the vault decrypts, not merely that it structurally exists. Skips (INFO)
// when either input is absent or the key fails the structural length check —
// the T2 store/masterkey rows own reporting those — because a probe on an
// empty vault under a wrong-length key derives a different DEK via HKDF but
// has nothing to decrypt, i.e. it would report a misleading PASS.
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
		servers, creds, perr := probeVaultDecrypt(storeP, keyP)
		if perr != nil {
			c.Status = statusFail
			c.Detail = fmt.Sprintf("vault fails to decrypt under the current master key: %v", perr)
			c.Fix = "key/ciphertext mismatch — restore from backup (.sme) or re-unlock + import; see docs/backup-restore.md"
		} else {
			c.Status = statusPass
			c.Detail = fmt.Sprintf("copy-probe decrypted %d servers / %d credentials", servers, creds)
		}
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
		c.Fix = "follow the recovery steps in the detail above, then verify with `ssh-manager serve cert-info`"
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
		c.Fix = "re-run `ssh-manager serve install` (idempotent install + start), or start it via the OS service manager"
	case state == "NOT INSTALLED":
		if st := doctorRole(); st != nil && st.Role == roles.RoleServer {
			c.Status = statusWarn
			c.Detail = "serve service not installed on a server-role machine"
			c.Fix = "run `ssh-manager serve install`"
		} else {
			c.Status = statusInfo
			c.Detail = "serve service not installed (serve not in use)"
		}
	default:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("serve service state indeterminate: %s", state)
		c.Fix = "inspect with `ssh-manager serve status` and the service manager's own tooling"
	}
	return []doctorCheck{c}
}

// checkClientCache reports the offline client cache (the DEFAULT instance).
// cache.bin present → cacheSidecarMatrix (DEK/auth/offline-cap sidecar
// matrix). cache.bin missing splits by role: FAIL on a client machine —
// UNLESS a named instance holds the offline cache (Plan 40: a machine whose
// only cache lives in instances/<name>/ must not FAIL "cache missing"; the
// instance rows below carry the diagnosis) — INFO elsewhere.
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
			if n := namedInstancesWithCache(); n > 0 {
				c.Status = statusInfo
				c.Detail = fmt.Sprintf("no default-instance cache.bin — %d named instance(s) hold the offline cache (diagnosed below)", n)
			} else {
				c.Status = statusFail
				c.Detail = "cache.bin missing on a client machine — no offline vault"
				c.Fix = "run `ssh-manager cache pull`"
			}
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
	cacheSidecarMatrix(&c, dir, dekP, "", age)
	return []doctorCheck{c}
}

// cacheSidecarMatrix completes a cache row whose cache.bin exists and statted
// (age known): DEK presence (missing → FAIL, the cache cannot be decrypted —
// the client-side FINDING A class) → auto-refresh credential presence
// (missing → WARN, offline works but never refreshes) → the Plan-37 offline
// cap overlay (a snapshot past its effective cap self-destructs on next use;
// doctor is the only place the state is visible BEFORE it bites). instance is
// "" for the default instance — Detail strings stay byte-identical to the
// pre-Plan-40 wording — or a named instance's device name (label prefix on
// Detail, --instance flags in Fix).
func cacheSidecarMatrix(c *doctorCheck, dir, dekP, instance string, age time.Duration) {
	label, pullFlag := "", ""
	if instance != "" {
		label, pullFlag = "instance "+instance+": ", " --instance "+instance
	}
	dekOK, dok := fileExists(dekP)
	if dok != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("%scache DEK stat failed: %v", label, dok)
		c.Fix = "check the cache DEK file permissions"
		return
	}
	if !dekOK {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("%scache.bin present (age %s) but its cache DEK is missing — cache undecryptable", label, age)
		c.Fix = fmt.Sprintf("re-run the client wizard (`ssh-manager tui`) and `cache pull%s` again to re-establish a decryptable cache", pullFlag)
		return
	}
	authOK, aok := fileExists(filepath.Join(dir, "cache.auth.json"))
	if aok != nil {
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("%scache.bin present (age %s) but cache.auth.json unstatable: %v", label, age, aok)
		c.Fix = "check the cache directory permissions"
		return
	}
	if !authOK {
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("%scache.bin present (age %s) but no auto-refresh credential (manual `cache pull` only)", label, age)
		c.Fix = fmt.Sprintf("run `ssh-manager cache pull%s` to persist cache.auth.json (enables auto-refresh)", pullFlag)
		return
	}
	maxOff, src, merr := clientops.EffectiveMaxOffline(dir)
	switch {
	case merr != nil:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("%scache.bin present (age %s) but the offline cap is unusable: %v", label, age, merr)
		c.Fix = fmt.Sprintf("rewrite it with `ssh-manager cache config --max-offline <dur>%s` (or clear the broken file/env)", pullFlag)
	case maxOff > 0 && age > maxOff:
		c.Status = statusWarn
		c.Detail = fmt.Sprintf("%scache.bin present (age %s) but past its offline cap (max-offline %s from %s) — it will be destroyed on next use", label, age, maxOff, src)
		c.Fix = fmt.Sprintf("re-pull while online (`ssh-manager cache pull%s`) to refresh the snapshot", pullFlag)
	default:
		c.Status = statusPass
		c.Detail = fmt.Sprintf("%scache.bin present (age %s)", label, age)
	}
}

// namedInstancesWithCache counts named instances whose slot holds a cache.bin.
// Zero under a single-slot override (named instances are not diagnosed there,
// so the default row must not downgrade on material it cannot see). Read
// errors count as zero — the instance rows report the failure itself.
func namedInstancesWithCache() int {
	if os.Getenv("SSHMGR_CACHE_DIR") != "" || os.Getenv("SSHMGR_CACHE_DEK") != "" {
		return 0
	}
	names, err := clientops.ListInstances()
	if err != nil {
		return 0
	}
	n := 0
	for _, name := range names {
		if _, bin, _, _, perr := clientops.CachePathsFor(name); perr == nil {
			if ok, ferr := fileExists(bin); ferr == nil && ok {
				n++
			}
		}
	}
	return n
}

// checkClientCacheInstances diagnoses every NAMED cache instance (Plan 40:
// instances/<name>/) with the same sidecar matrix as the default row. Under a
// single-slot override (SSHMGR_CACHE_DIR redirects the default slot;
// SSHMGR_CACHE_DEK swallows the per-instance DEK suffix) named-instance paths
// no longer resolve to the operator's real material — the whole group is
// skipped with one audible INFO row instead of silently mis-diagnosing. No
// named instances → no rows (single-instance machines keep byte-identical
// output).
func checkClientCacheInstances() []doctorCheck {
	if os.Getenv("SSHMGR_CACHE_DIR") != "" || os.Getenv("SSHMGR_CACHE_DEK") != "" {
		return []doctorCheck{{
			Name:   "client-cache-instances",
			Status: statusInfo,
			Detail: "skipped — single-slot cache override in effect (SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK); named instances not diagnosed",
		}}
	}
	names, err := clientops.ListInstances()
	if err != nil {
		return []doctorCheck{{
			Name:   "client-cache-instances",
			Status: statusWarn,
			Detail: fmt.Sprintf("instances dir unreadable: %v", err),
			Fix:    "check the instances directory permissions",
		}}
	}
	var rows []doctorCheck
	for _, name := range names {
		rows = append(rows, namedInstanceCacheRow(name))
	}
	return rows
}

// namedInstanceCacheRow is one named instance's row. cache.bin missing splits
// three ways: empty slot → INFO debris; client machine → WARN with the
// --instance pull fix (the machine-level "no cache anywhere" FAIL stays with
// the default row, which only downgrades when some instance HOLDS a bin);
// non-client → INFO. The bin-present matrix is role-independent — broken
// cache material is broken wherever it sits.
func namedInstanceCacheRow(name string) doctorCheck {
	c := doctorCheck{Name: "client-cache[" + name + "]"}
	dir, bin, _, _, err := clientops.CachePathsFor(name)
	if err != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("instance %s: cache paths unresolvable: %v", name, err)
		c.Fix = "check the instance name and the user config dir"
		return c
	}
	info, serr := os.Stat(bin)
	role := doctorRole()
	switch {
	case errors.Is(serr, fs.ErrNotExist):
		switch {
		case emptyInstanceSlot(dir):
			c.Status = statusInfo
			c.Detail = fmt.Sprintf("instance %s: empty slot (no cache material)", name)
		case role != nil && role.Role == roles.RoleClient:
			c.Status = statusWarn
			c.Detail = fmt.Sprintf("instance %s has no cache.bin", name)
			c.Fix = fmt.Sprintf("run `ssh-manager cache pull --instance %s`", name)
		default:
			c.Status = statusInfo
			c.Detail = fmt.Sprintf("instance %s has no cache.bin (no offline cache expected on this machine)", name)
		}
	case serr != nil:
		c.Status = statusFail
		c.Detail = fmt.Sprintf("instance %s: cache.bin stat failed: %v", name, serr)
		c.Fix = "check the cache directory permissions"
	default:
		dekP, derr := paths.CacheDekPathFor(name)
		if derr != nil {
			c.Status = statusFail
			c.Detail = fmt.Sprintf("instance %s: cache DEK path unresolvable: %v", name, derr)
			c.Fix = "check the vault directory (platform vault root could not be resolved)"
			return c
		}
		cacheSidecarMatrix(&c, dir, dekP, name, time.Since(info.ModTime()).Round(time.Minute))
	}
	return c
}

// emptyInstanceSlot reports whether an instance directory carries no cache
// material at all. Any of the five known files present means NOT empty — a
// destroyed/quarantined cache keeps meta/audit residue and must not read as
// clean debris.
func emptyInstanceSlot(dir string) bool {
	for _, f := range []string{"cache.bin", "cache.meta.json", "cache.auth.json", "cache.config.json", "cache-audit.log"} {
		if ok, err := fileExists(filepath.Join(dir, f)); err == nil && ok {
			return false
		}
	}
	return true
}

// runDoctor executes every check, renders the report, and returns an error
// when FAIL findings are detected (wrapped errDoctorFindings).
func runDoctor(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "ssh-manager doctor (%s)\n", buildinfo.Version)
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
		Short: "Side-effect-free local self-check with PASS/WARN/FAIL findings",
		Long: `Run a local self-check and print a PASS/WARN/FAIL report with remediation
hints. Checks are read-only: doctor never writes the vault, certificates, or
client cache, makes no network calls, and never prints secret values —
environment overrides are reported by name only.

Exit codes (stable, for scripts): 0 = no FAIL findings (warnings allowed),
1 = at least one FAIL finding.`,
		Args: cobra.NoArgs,
		RunE: runDoctor,
	}
	return c
}
