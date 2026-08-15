// Package roles is the single authority for the machine's role state
// (role.json) and the unified launch-resolution chain consumed by the CLI/TUI
// entrypoint (Plan 19, spec §1.2): role.json first, anomaly checks
// (invalid value / vault-role-without-vault → guide `clear`; client without
// cache → normal), then the v0.6.0-era filesystem probe (locked vault
// fail-closed → vault → broker, cache → client, empty → wizard).
package roles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// Role identifies the machine's deployment role. The string values are the
// on-disk JSON values of role.json's "role" field — never rename them.
type Role string

const (
	RoleStandalone Role = "standalone"
	RoleServer     Role = "server"
	RoleClient     Role = "client"
)

func validRole(r Role) bool {
	return r == RoleStandalone || r == RoleServer || r == RoleClient
}

// State is the parsed role.json. Both fields are always written by Save.
type State struct {
	Role          Role `json:"role"`
	SetupComplete bool `json:"setup_complete"`
}

// LaunchKind selects what the entrypoint should start.
type LaunchKind int

const (
	LaunchWizard LaunchKind = iota
	LaunchBroker
	LaunchClient
)

// Launch is the result of ResolveMode.
type Launch struct {
	Kind        LaunchKind // LaunchWizard / LaunchBroker / LaunchClient
	Role        Role       // Kind=Broker 时区分 standalone|server（有无 serve 服务安装）
	ResumeSetup bool       // Kind 对应向导续配（SetupComplete=false）
}

// ---------------------------------------------------------------------------
// role.json locations
// ---------------------------------------------------------------------------

// vaultStorePath mirrors cli's storePath (env override > default) WITHOUT
// importing cli. Returns "" only when the default path itself is unresolvable
// (no vault could exist there anyway).
func vaultStorePath() string {
	if p := os.Getenv("SSHMGR_STORE"); p != "" {
		return p
	}
	p, err := store.DefaultStorePath()
	if err != nil {
		return ""
	}
	return p
}

// vaultRolePath is role.json for the standalone/server roles. It lives in the
// vault dir, derived from the resolved STORE path (not paths.VaultDir directly)
// so a SSHMGR_STORE pin relocates role.json together with store.db; with no
// env override both resolve to the same program-fixed dir (spec §3.1).
func vaultRolePath() string {
	p := vaultStorePath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "role.json")
}

// clientRolePath is role.json for the client role: os.UserConfigDir()/
// ssh-manager/role.json, colocated with the client cache data (clientops
// cache.bin / cache.auth.json live in the same directory).
func clientRolePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ssh-manager", "role.json"), nil
}

// RolePath returns where role.json lives for the given role:
// standalone/server → the vault dir; client → the user config dir.
func RolePath(r Role) (string, error) {
	switch r {
	case RoleStandalone, RoleServer:
		p := vaultRolePath()
		if p == "" {
			return "", errors.New("vault dir unresolvable (cannot locate role.json)")
		}
		return p, nil
	case RoleClient:
		return clientRolePath()
	default:
		return "", fmt.Errorf("unknown role %q", r)
	}
}

// ---------------------------------------------------------------------------
// Load / Save / Delete
// ---------------------------------------------------------------------------

// Load reads role.json, vault location first, user-config location second.
// Neither present → (nil, nil) (fresh machine → wizard). A present-but-invalid
// file is an error guiding `ssh-manager clear` — never silently ignored.
func Load() (*State, error) {
	if p := vaultRolePath(); p != "" {
		s, err := readRoleFile(p)
		if err != nil || s != nil {
			return s, err
		}
	}
	if p, err := clientRolePath(); err == nil {
		return readRoleFile(p)
	}
	return nil, nil
}

func readRoleFile(p string) (*State, error) {
	blob, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(blob, &s); err != nil {
		return nil, fmt.Errorf("role.json (%s) 内容非法: %w — 运行 `ssh-manager clear` 后重新初始化", p, err)
	}
	if !validRole(s.Role) {
		return nil, fmt.Errorf("role.json (%s) role 值非法 %q — 运行 `ssh-manager clear` 后重新初始化", p, s.Role)
	}
	return &s, nil
}

// Save atomically writes role.json for s.Role (unique temp + rename, 0600,
// ACL-hardened on Windows). Both JSON fields are always written.
func Save(s State) error {
	if !validRole(s.Role) {
		return fmt.Errorf("cannot save unknown role %q", s.Role)
	}
	p, err := RolePath(s.Role)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := atomicWriteUnique(p, blob); err != nil {
		return err
	}
	return store.HardenACL(p)
}

// Delete removes role.json from BOTH locations (idempotent — a missing file is
// not an error). Used by `ssh-manager clear` and by wizard re-runs.
func Delete() error {
	var firstErr error
	paths := []string{}
	if p := vaultRolePath(); p != "" {
		paths = append(paths, p)
	}
	if p, err := clientRolePath(); err == nil {
		paths = append(paths, p)
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// atomicWriteUnique atomically replaces path with blob via a UNIQUE temp file +
// rename. Copied verbatim from clientops (Plan 19 T1: roles must not depend on
// clientops' unexported helper, and exporting it from clientops just for this
// would widen that package's API — a ~20-line local copy is the lesser evil).
// Unlike a fixed ".tmp" name, concurrent writers never interleave on the same
// temp file, so a torn blob can never be renamed into place (xcheck
// 2026-08-14). os.CreateTemp creates the temp 0600, matching role.json's
// protection. On Windows, concurrent readers can hold the target open, so the
// rename is retried briefly on fs.ErrPermission.
func atomicWriteUnique(path string, blob []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	var lastErr error
	for i := 0; i < 50; i++ {
		err := os.Rename(tmpPath, path)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
		time.Sleep(time.Duration(10+(i*2)) * time.Millisecond)
	}
	return lastErr
}

// ---------------------------------------------------------------------------
// Vault probes (migrated verbatim from internal/tui/mode.go — v0.6.0 behavior)
// ---------------------------------------------------------------------------

// VaultExists reports whether a store.db file exists at the vault location.
// Stat-first so probing a machine with NO vault never triggers OpenStore's
// create-on-open side effect (a fresh empty store.db).
func VaultExists() bool {
	p := vaultStorePath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// VaultUnlocked reports whether an UNLOCKED vault is reachable. A vault that
// EXISTS but cannot be opened (locked / key unreadable) is distinguished by the
// caller via VaultExists so detection never silently degrades a locked broker
// machine into client mode (spec §6).
func VaultUnlocked() bool {
	if !VaultExists() {
		return false
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return false
	}
	st.Close()
	return true
}

// cachePresent reports whether this machine is an enrolled client
// (cache.auth.json readable; nil,nil = never enrolled).
func cachePresent() bool {
	cred, err := clientops.ReadCacheCred()
	return err == nil && cred != nil
}

// serveCertPresent is the server-vs-standalone heuristic when no role.json
// exists yet: serve-cert.pem present in the vault dir → this machine once ran
// `serve install` (RoleServer), else RoleStandalone. This is a FILE probe, not
// a service-manager query (svc.Status is too heavy for every launch and is
// Windows-specific); a stale cert on a machine whose serve service was
// uninstalled merely picks the "server" label until role.json is written by
// the wizard, which is then authoritative. SSHMGR_SERVE_CERT (test) is honored
// first; otherwise the cert is looked up next to the resolved store path,
// which equals paths.ServeCertPath()'s default dir in production.
func serveCertPresent() bool {
	if v := os.Getenv("SSHMGR_SERVE_CERT"); v != "" {
		_, err := os.Stat(v)
		return err == nil
	}
	p := vaultStorePath()
	if p == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(p), "serve-cert.pem"))
	return err == nil
}

// ---------------------------------------------------------------------------
// ResolveMode — the single launch decision (spec §1.2)
// ---------------------------------------------------------------------------

// ResolveMode decides what `ssh-manager` should launch. Order:
//
//  1. force guard: force=="client" on a machine WITH a vault is refused
//     (accidental client mode against its own vault); any other non-empty
//     force value is invalid.
//  2. role.json present → anomaly checks (invalid value / vault-role without
//     vault → error guiding `ssh-manager clear`; client without cache is
//     NORMAL — first pull happens after launch) → Launch with ResumeSetup
//     when SetupComplete=false.
//  3. no role.json → v0.6.0 probe: locked vault fails closed (never degrades
//     to client) → unlocked vault → LaunchBroker (standalone|server via the
//     serve-cert heuristic) → cache → LaunchClient → empty → LaunchWizard.
func ResolveMode(force string) (Launch, error) {
	switch force {
	case "", "client":
	default:
		return Launch{}, fmt.Errorf("invalid --force %q (want client)", force)
	}
	if force == "client" {
		if VaultExists() {
			return Launch{}, errors.New("本机已有 vault，无法以 client 角色启动：`ssh-manager clear` 将删除本机全部 vault 数据（含全部凭据），确认无误后再运行")
		}
		return Launch{Kind: LaunchClient, Role: RoleClient}, nil
	}

	if st, err := Load(); err != nil {
		return Launch{}, err
	} else if st != nil {
		return resolveFromState(st)
	}

	// No role.json: probe (pre-wizard / v0.6.0 machines).
	if VaultExists() && !VaultUnlocked() {
		return Launch{}, errors.New("本机 vault 存在但锁定或不可读：先运行 `ssh-manager unlock`（不会降级为 client 模式）")
	}
	if VaultUnlocked() {
		r := RoleStandalone
		if serveCertPresent() {
			r = RoleServer
		}
		return Launch{Kind: LaunchBroker, Role: r}, nil
	}
	if cachePresent() {
		return Launch{Kind: LaunchClient, Role: RoleClient}, nil
	}
	return Launch{Kind: LaunchWizard}, nil
}

func resolveFromState(st *State) (Launch, error) {
	switch st.Role {
	case RoleStandalone, RoleServer:
		if !VaultExists() {
			if !st.SetupComplete {
				// User picked a role but hasn't built the vault yet — wizard resumes
				return Launch{Kind: LaunchWizard, Role: st.Role, ResumeSetup: true}, nil
			}
			return Launch{}, fmt.Errorf("role.json 声明本机为 %s，但 vault 不存在：运行 `ssh-manager clear` 清除残留状态后重新运行向导", st.Role)
		}
		return Launch{Kind: LaunchBroker, Role: st.Role, ResumeSetup: !st.SetupComplete}, nil
	case RoleClient:
		// Missing cache is NOT an anomaly: a client machine before its first
		// pull (or after an explicit cache wipe) still launches as client.
		return Launch{Kind: LaunchClient, Role: RoleClient, ResumeSetup: !st.SetupComplete}, nil
	default:
		// Unreachable: Load rejects invalid roles. Kept for exhaustiveness.
		return Launch{}, fmt.Errorf("role.json role 值非法 %q — 运行 `ssh-manager clear` 后重新初始化", st.Role)
	}
}
