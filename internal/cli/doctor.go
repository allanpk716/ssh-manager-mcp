package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"

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

// doctorExitCode is the stable exit-code convention (scripts rely on it):
// 0 = no FAIL, 1 = ≥1 FAIL (errDoctorFindings, wrapped included),
// 2 = doctor internal error.
func doctorExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errDoctorFindings):
		return 1
	default:
		return 2
	}
}

// doctorCheckFuncs is the checks table — T3 (copy decrypt probe), T4 (serve
// cert/service, client cache) append entries here. Every check is
// self-contained and side-effect-free; order only affects display.
var doctorCheckFuncs = []func() []doctorCheck{
	checkEnv,
	checkRole,
	checkVaultStore,
	checkVaultKey,
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
		c.Fix = "check the vault directory (platform root could not be resolved — see spec §3.1)"
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
			c.Detail = "store.db absent — no role.json on this machine (see the role check)"
		}
	default:
		c.Status = statusFail
		c.Detail = fmt.Sprintf("store.db stat failed: %v", err)
		c.Fix = "check the vault directory permissions"
	}
	return []doctorCheck{c}
}

// checkVaultKey reads the master key file (env-aware path) and validates it
// STRUCTURALLY — same rationale as vaultStatusString (serve_service.go):
// store.Open is side-effecting, so ValidMasterKeyLen is the lightest faithful
// proxy for "the file is a usable AES-256 key". Unlike serve's LOCKED wording,
// doctor phrases its own remediation. On Unix the plaintext key's protection
// is mode bits alone (L1+ threat model), so loose group/world bits downgrade
// an otherwise-valid key to WARN; on Windows protection is ACLs, not mode
// bits, and the branch is skipped (runtime guard keeps one test file).
func checkVaultKey() []doctorCheck {
	c := doctorCheck{Name: "masterkey"}
	p, err := paths.MasterKeyPath()
	if err != nil {
		c.Status = statusFail
		c.Detail = fmt.Sprintf("master key path unresolvable: %v", err)
		c.Fix = "check the vault directory (platform root could not be resolved — see spec §3.1)"
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
			c.Detail = "master.key absent — no role.json on this machine (see the role check)"
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
		if runtime.GOOS != "windows" {
			if info, serr := os.Stat(p); serr == nil && info.Mode().Perm()&0o077 != 0 {
				c.Status = statusWarn
				c.Detail = fmt.Sprintf("master.key present (%d bytes) but group/world readable (mode %o) — the plaintext key is protected by mode bits alone", len(b), info.Mode().Perm())
				c.Fix = "chmod 600 the master.key file (and 0700 its parent directory)"
			}
		}
	}
	return []doctorCheck{c}
}

// runDoctor executes every check, renders the report, and returns the error
// the exit-code convention is read from: nil = 0 (no FAIL),
// errDoctorFindings = 1 (≥1 FAIL), any other error = 2 (internal error).
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
		return fmt.Errorf("%w (%d) — see the report above", errDoctorFindings, fail)
	}
	return nil
}

func newDoctorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Side-effect-free local self-check with PASS/WARN/FAIL findings",
		Long: `Run a local self-check and print a PASS/WARN/FAIL report with remediation
hints. Checks are read-only: doctor never writes the vault, certificates, or
client cache, makes no network calls, and never prints secret values —
environment overrides are reported by name only.

Exit codes (stable, for scripts): 0 = no FAIL (WARN does not change it),
1 = at least one FAIL, 2 = doctor internal error.`,
		Args: cobra.NoArgs,
		RunE: runDoctor,
	}
	return c
}
