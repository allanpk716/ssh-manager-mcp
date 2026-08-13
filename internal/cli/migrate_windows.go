//go:build windows

// Package cli: v0.2.0 keychain → DPAPI migration (Windows only; spec §5.7).
//
// Background: v0.2.0 stored the vault master key in the OS keychain on every
// platform (KeyringKeyProvider). On Windows, Credential Manager (wincred)
// cannot be read from sshd / Service / Task-Scheduler sessions — it fails with
// ERROR_NO_SUCH_LOGON_SESSION (1312) (spec §12 spike FINDING 9). Plan 14 moved
// Windows to a DPAPI-encrypted file (DpapiKeyProvider) which works across all
// session types. This file migrates an existing v0.2.0 install: when `unlock`
// first runs and finds no DPAPI master.key, it probes the legacy keychain
// slots (master-key + cache-dek). Readable → prompt → migrate to DPAPI file +
// delete the old slot. Unreadable non-ErrNotFound (the 1312 failure) → clear
// "rerun in an interactive session" prompt. Clean env → first-run generate.
//
// Linux/macOS have no such migration (spec §3.3: they keep KeyringKeyProvider;
// same medium before and after) — see keychain_unix.go, where firstRunMigrator
// stays nil.
//
// Migration is INTERACTIVE-SESSION ONLY by construction: the legacy slot is
// only readable interactively (the 1312 path hard-fails otherwise), and the
// confirm prompt requires a TTY. serve/Service context never sees a readable
// legacy slot, so it never migrates.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// Legacy v0.2.0 keychain slot names (masterkey.go keyringService / keyringUser
// constants; cache-dek is the Plan 12 cache DEK slot). Hard-coded by v0.2.0.
const (
	legacyMasterKeyringUser   = "master-key" // == store.KeyringKeyProvider{}.user() default
	legacyCacheDEKKeyringUser = "cache-dek"
)

// errLegacyKeyringUnreadable is returned by migrateKeyProvider when the legacy
// slot exists but cannot be read in this session (Windows Credential Manager
// returns ERROR_NO_SUCH_LOGON_SESSION 1312 in sshd/Service sessions). The
// caller surfaces a clear "rerun interactively" prompt rather than a raw
// syscall error, and crucially does NOT generate a fresh key (which would
// orphan the old vault behind an unreadable slot).
var errLegacyKeyringUnreadable = errors.New("legacy keychain slot present but unreadable in this session (likely sshd/Service; rerun interactively)")

// confirmMigrate is the yes/no prompt for migration confirmation. Tests
// override it to drive the [y/N] branch deterministically without touching the
// real TTY. Returns true only on an explicit affirmative (y/yes). The label is
// captured by the override so tests can assert which key was being confirmed.
//
// w is the command's stderr writer (cmd.ErrOrStderr()) so prompt text is
// captured by the test's stderr buffer.
var confirmMigrate = func(w interface{ Write([]byte) (int, error) }, label string) bool {
	fmt.Fprintf(w, "Detected %s in the legacy v0.2.0 keychain slot. Migrate to a DPAPI-encrypted file (recommended)? [y/N]: ", label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// migrateSources returns the legacy slot + DPAPI destination pairs for the
// master key and the cache DEK. Tests override it to point at isolated test
// service/slot names + temp paths so the host's real ssh-manager /
// ssh-manager-eval keychain entries and DPAPI files are never touched.
//
// Default (production): legacy slots are SSHMGR_KEYRING_SERVICE-scoped (same
// service the cache + eval use) so the migration picks up the SAME entries the
// running install would read. DPAPI destinations live in the same dir as
// master.key (%AppData%\ssh-manager\), disjoint filenames for master vs cache.
var migrateSources = func() (master, dek migrateSource) {
	dir := dpapiBaseDir()
	return migrateSource{
			old: store.KeyringKeyProvider{Service: keyringService(), User: legacyMasterKeyringUser},
			new: store.DpapiKeyProvider{Path: filepath.Join(dir, "master.key")},
		}, migrateSource{
			old: store.KeyringKeyProvider{Service: keyringService(), User: legacyCacheDEKKeyringUser},
			new: store.DpapiKeyProvider{Path: dpapiCacheDekPath()},
		}
}

// migrateSource pairs one legacy keychain slot with its DPAPI destination.
type migrateSource struct {
	old store.KeyProvider // legacy v0.2.0 keychain slot (Get/Set/Delete)
	new store.KeyProvider // DPAPI-encrypted file destination (Get/Set)
}

// keyringService returns the keychain service name to probe for legacy slots.
// Mirrors envKeyringKeyProvider (keychain_unix.go): SSHMGR_KEYRING_SERVICE on
// every call (empty → production default "ssh-manager"). The eval harness sets
// this to "ssh-manager-eval" via mcp.json, so a migration run inside the eval
// sees the eval-only slot.
func keyringService() string { return os.Getenv("SSHMGR_KEYRING_SERVICE") }

// dpapiBaseDir resolves the directory holding master.key / cache-dek.key
// (parent of DpapiKeyProvider's default path). Empty/unset → "ssh-manager" in
// the current dir (best-effort; matches DpapiKeyProvider.path's fallback shape
// — tests always override migrateSources with a temp dir).
func dpapiBaseDir() string {
	appData := os.Getenv("AppData")
	if appData == "" {
		return filepath.Join("ssh-manager")
	}
	return filepath.Join(appData, "ssh-manager")
}

// dpapiCacheDekPath is the SINGLE source of truth for the cache-DEK DPAPI file
// path. Both the migration writer (migrateSources → DpapiKeyProvider{Path: …})
// and the cache DEK reader (dekProvider in cache_dek_windows.go) MUST resolve
// the same path — otherwise the migrated cache-dek.key is written to A and read
// from B, the reader hits ErrNotFound, generates a fresh DEK, and the migrated
// file is orphaned (Plan 14 T5 review F1). Centralizing the path here keeps the
// write + read sides locked together; migrateSources and the dekProvider seam
// both call THIS function, never filepath.Join(dir, "cache-dek.key") inline.
const cacheDekFileName = "cache-dek.key"

func dpapiCacheDekPath() string {
	return filepath.Join(dpapiBaseDir(), cacheDekFileName)
}

func init() {
	// Wire the migration into `unlock`'s ErrNotFound branch. keychain_unix.go
	// leaves firstRunMigrator nil, so Unix unlock skips this entirely.
	firstRunMigrator = migrateOnFirstRun
	// Plan 15 T3: wire the user-scope → machine-scope re-protect hook into
	// `unlock`'s Get-succeeded branch. Nil on Unix (no migrate_windows.go).
	postGetMigrator = migrateDpapiScope
}

// migrateDpapiScope (Plan 15 T3) re-protects a legacy user-scope master.key to
// machine-scope. Triggered by unlock's postGetMigrator hook (Get succeeded →
// key was read → maybe it's a legacy user-scope blob needing re-protect so
// boot auto-start via Task Scheduler can read it without an interactive
// logon). Fixes codex #1: firstRunMigrator is ErrNotFound-gated and never
// fires here (dual-scope Get returns success for a user-scope blob).
//
// === SPIKE-2 CAVEAT (load-bearing, read before changing the detection rule) ===
// Plan 15 spike 2 (TestDpapi_CrossScopeInteroperable) proved DPAPI's scope flag
// is a HINT, not a hard gate: a blob self-describes its scope and BOTH flags
// decrypt it. Verified symmetric in this task (user-blob + machine-flag also
// succeeds). Therefore MachineUnprotectForMigrate succeeding does NOT prove the
// blob is already machine-scope — it succeeds on user-scope blobs too. The
// brief's proposed detection ("machine-unprotect OK ⇒ already machine-scope,
// skip") would have short-circuited on the very user-scope blobs we must
// migrate, recreating codex #1 under a different name. This test was confirmed
// empirically (see task-3-report.md). Since DPAPI exposes NO way to recover the
// flag a blob was protected with, scope detection from the blob alone is
// impossible. We instead ALWAYS offer the re-protect:
//   - On a legacy user-scope blob: migrates to machine-scope (the goal).
//   - On an already-machine-scope blob: idempotent re-protect (Set re-protects
//     with machine flag; master key VALUE unchanged because Get already read
//     it and we pass mk back to Set). Cost: one Set + icacls per interactive
//     unlock. UX cost: a [y/N] prompt on EVERY interactive unlock, even when
//     the file is already machine-scope. Both are accepted as the price of
//     spike-2; a non-invasive scope probe does not exist. See task-3-report.md
//     CONCERNS for follow-up options (sentinel file, etc.).
//
// Flow:
//   - Locate master.key via migrateSources().master.new (DpapiKeyProvider).
//     Non-DPAPI provider / no path / unreadable file ⇒ nothing to do
//     (return false, nil; firstRunMigrator handles the truly-absent case).
//   - Sanity-probe the blob (machine then user unprotect): if BOTH fail the
//     file is corrupt / admin-reset ⇒ print a recovery path, return (false,
//     nil) without orphaning the vault. (Get already succeeded to reach here,
//     so this branch is defensive; under spike-2 at least one always succeeds.)
//   - confirmMigrate [y/N]. N ⇒ guidance ("re-run unlock, accept prompt"),
//     return (false, nil). Caller prints mk anyway.
//   - Re-protect mk via master.new.Set(mk) (machine-scope by T2; ACL contract
//     preserved by DpapiKeyProvider.Set). Return (true, nil) on success.
//   - cache-DEK piggybacks: if a legacy keyring slot exists it is migrated
//     too (migrateKeyProvider reads old slot, writes DPAPI file, deletes
//     slot); best-effort, skips if absent. Reuses v0.2.0 "master succeeds ⇒
//     DEK migrates" consistency. This does NOT cover a user-scope cache-dek
//     FILE (only the legacy keyring slot); a user-scope cache-dek.key file
//     is out of scope for T3 (cache-dek.key is created post-T5 by the DPAPI
//     writer, always machine-scope; there is no user-scope cache-dek.key in
//     the wild).
//
// mk is the key already read by Get (caller passes it); we only re-protect
// it, value unchanged. Must be an INTERACTIVE session (the confirm prompt
// reads stdin; sshd sessions get N implicitly via EOF, no harm done).
func migrateDpapiScope(w interface{ Write([]byte) (int, error) }, mk []byte) (bool, error) {
	master, dek := migrateSources() // master.key + cache-dek.key providers

	masterProv, ok := master.new.(store.DpapiKeyProvider)
	if !ok {
		return false, nil // 非 Windows DPAPI provider(Windows-only build 不应发生)
	}
	masterPath, err := masterProv.PathOrEmpty()
	if err != nil || masterPath == "" {
		return false, nil // 无路径 → 无可迁移(master.key 不存在由 firstRunMigrator 处理)
	}
	blob, rErr := os.ReadFile(masterPath)
	if rErr != nil {
		return false, nil // master.key 不读 / 不存在 → 无可迁移(firstRunMigrator 管 ErrNotFound 路径)
	}
	// blob 必须是 Get 能读出的(spin-2 下任一 flag 都能解)——能被 Get 读出说明
	// 文件有效(非损坏/admin 重置)。若是损坏 blob,Get 早已失败,不会走到这里。
	// 双 scope 都试一次确认 blob 至少能解(防御性;spike-2 下通常都能解)。
	if _, mErr := masterProv.MachineUnprotectForMigrate(blob); mErr != nil {
		if _, uErr := masterProv.UserUnprotectForMigrate(blob); uErr != nil {
			// 两个 scope 都读不出:损坏 / admin 密码重置。不生成新 key(会 orphan vault),
			// 不重 protect(可能掩盖更深的 vault 损坏)。给用户一条恢复路径。
			fmt.Fprintln(w, "\nmaster.key could not be read under either DPAPI scope (possibly corrupt, or admin password reset). To recover, restore from a backup export (see docs/backup-restore.md).")
			return false, nil
		}
	}

	// 确认迁移(复用 confirmMigrate 的 [y/N] 机制)。prompt label 说明意图:
	// spike-2 使 scope 检测不可行,我们总是确保 machine-scope(对已是 machine-scope
	// 的 blob 是幂等 re-protect)。label 文案告诉用户"这是 user-scope → machine-scope"。
	if !confirmMigrate(w, "user-scope master.key (migrate to machine-scope)") {
		fmt.Fprintln(w, "migration declined; master.key left as-is. serve auto-start at boot needs machine-scope DPAPI; re-run `unlock` and accept the prompt (interactive session).")
		return false, nil
	}

	// 重 protect mk 到 machine-scope(值不变 —— Get 读出的 mk,原样回 Set;
	// T2 的 Set 用 dpapiProtect(mk, true) 即 machine-scope)。ACL 契约由 Set 保证
	// (temp 在 protectedDir 内,继承 allan716-only ACL)。
	if err := master.new.Set(mk); err != nil {
		return false, fmt.Errorf("re-protect master.key to machine-scope: %w", err)
	}

	// cache-dek 搭车(如果 legacy keyring slot 存在)。复用 v0.2.0 "master 成功才迁 dek"
	// 一致性。best-effort:ErrNotFound(无 slot)和 errLegacyKeyringUnreadable(sshd 1312)
	// 都不是硬错;其他错仅记录。
	if _, _, dErr := migrateKeyProvider(w, dek, "cache DEK"); dErr != nil && !errors.Is(dErr, errLegacyKeyringUnreadable) {
		fmt.Fprintf(w, "cache DEK scope migration skipped: %v\n", dErr)
	}

	fmt.Fprintln(w, "master.key re-protected: user-scope to machine-scope DPAPI (idempotent if it already was machine-scope).")
	return true, nil
}

// migrateOnFirstRun is the unlock ErrNotFound hook (Windows). It probes the
// legacy master-key + cache-dek keychain slots and migrates whichever are
// readable interactively. Returns (mk, outcome, err):
//   - firstRunMigrated: master key migrated (persisted to DPAPI file) + mk
//     returned for the caller to print.
//   - firstRunSkip: clean env (no legacy slot) → caller first-run generates.
//   - firstRunStop: abort without generating. Covers (a) the user declining the
//     prompt, and (b) the legacy slot being present but unreadable (sshd/1312)
//     — generating a fresh key in either case would orphan the old vault.
//     Guidance is already printed to w; err is nil.
//   - err non-nil: hard failure mid-migrate (DPAPI persist error, etc.).
//
// A legacy slot that errors with errLegacyKeyringUnreadable (the 1312 path) is
// reported to w with a clear "rerun in an interactive session" message and
// routes to firstRunStop (NOT firstRunSkip) so unlock does NOT auto-generate.
func migrateOnFirstRun(w interface{ Write([]byte) (int, error) }) ([]byte, firstRunOutcome, error) {
	master, dek := migrateSources()

	migratedMK, mkOutcome, err := migrateKeyProvider(w, master, "master key")
	// errLegacyKeyringUnreadable is NOT a hard error — it's the sshd/1312 path
	// that we surface as guidance + firstRunStop. Any OTHER error (DPAPI
	// persist failure, missing Delete method) is hard.
	if err != nil && !errors.Is(err, errLegacyKeyringUnreadable) {
		return nil, firstRunStop, err
	}

	// Migrate the cache DEK ONLY when the master outcome is a green light
	// (migrated, or absent). If the user declined the master prompt, or the
	// master slot is unreadable (sshd/1312), don't prompt again for the DEK —
	// one declination/refusal governs both, and a partial migration (DEK moved
	// but master left) would be confusing. The DEK is never load-bearing
	// (cache.bin is a refreshable copy), so skipping its migration is safe.
	if mkOutcome == migrateOutcomeDone || mkOutcome == migrateOutcomeAbsent {
		if _, _, dErr := migrateKeyProvider(w, dek, "cache DEK"); dErr != nil && !errors.Is(dErr, errLegacyKeyringUnreadable) {
			fmt.Fprintf(w, "cache DEK migration skipped: %v\n", dErr)
		}
	}

	switch mkOutcome {
	case migrateOutcomeDone:
		return migratedMK, firstRunMigrated, nil
	case migrateOutcomeDeclined:
		// User chose not to migrate the master key. Do NOT first-run generate
		// (would mask the legacy slot behind a fresh DPAPI file) — surface a
		// clear remediation path.
		fmt.Fprintln(w, "migration declined; master.key not created. To use DPAPI, re-run `unlock` and accept the migration prompt (or remove the legacy keychain slot manually).")
		return nil, firstRunStop, nil
	case migrateOutcomeUnreadable:
		// sshd/Service can't read the legacy slot. Print guidance + STOP (do
		// NOT generate — the old vault may still be behind the unreadable slot).
		fmt.Fprintln(w, "\nA legacy v0.2.0 keychain master-key slot appears to exist but could not be read in this session (Windows Credential Manager is unavailable in sshd/Service sessions — error 1312). To migrate, re-run `unlock` from an interactive session (local terminal or RDP), or reset the vault (see docs/backup-restore.md).")
		return nil, firstRunStop, nil
	case migrateOutcomeAbsent:
		// Clean env (no legacy slot) → caller first-run generates.
		return nil, firstRunSkip, nil
	}
	return nil, firstRunStop, nil
}

// migrateOutcome enumerates the result of probing one legacy slot.
type migrateOutcome int

const (
	migrateOutcomeAbsent     migrateOutcome = iota // no legacy slot (ErrNotFound) — clean
	migrateOutcomeUnreadable                       // slot present but unreadable (sshd 1312)
	migrateOutcomeDone                             // migrated + old slot deleted
	migrateOutcomeDeclined                         // user answered No at the prompt
)

// migrateKeyProvider probes oldKp; if a legacy key is present and readable it
// prompts, writes it to newKp, and deletes oldKp. It is the shared body for
// both the master-key and cache-DEK migrations. The label is used in the
// prompt + error messages. All messages route through w (cmd.ErrOrStderr()).
//
// Errors:
//   - oldKp.Get() == ErrNotFound → (nil, migrateOutcomeAbsent, nil).
//   - oldKp.Get() non-ErrNotFound error → (nil, migrateOutcomeUnreadable,
//     errLegacyKeyringUnreadable wrapping it). NOT a hard error: the caller
//     decides whether to surface the "rerun interactively" guidance.
//   - prompt declines → (nil, migrateOutcomeDeclined, nil).
//   - newKp.Set / oldKp.Delete failure → (nil, _, err): hard error.
func migrateKeyProvider(w interface{ Write([]byte) (int, error) }, src migrateSource, label string) ([]byte, migrateOutcome, error) {
	oldKp, ok := src.old.(keyProviderWithDelete)
	if !ok {
		// KeyringKeyProvider always satisfies keyProviderWithDelete; this guard
		// is defensive against a future KeyProvider that lacks Delete.
		return nil, migrateOutcomeAbsent, fmt.Errorf("migration source for %s lacks Delete (cannot remove legacy slot); refusing to migrate", label)
	}
	legacy, err := src.old.Get()
	if errors.Is(err, store.ErrNotFound) {
		return nil, migrateOutcomeAbsent, nil
	}
	if err != nil {
		// The sshd / non-interactive-session 1312 failure lands here. Wrap so
		// the caller can errors.Is(err, errLegacyKeyringUnreadable).
		return nil, migrateOutcomeUnreadable, fmt.Errorf("%w: %v", errLegacyKeyringUnreadable, err)
	}
	if !confirmMigrate(w, label) {
		return nil, migrateOutcomeDeclined, nil
	}
	if err := src.new.Set(legacy); err != nil {
		return nil, migrateOutcomeDeclined, fmt.Errorf("persist %s to DPAPI file: %w", label, err)
	}
	// Legacy slot cleanup is best-effort: ErrNotFound after a successful Set is
	// fine (slot raced away); a real delete failure is logged but does not roll
	// back — the new DPAPI file is already authoritative, and a stale legacy
	// slot is harmless (re-running unlock sees the DPAPI file first).
	if dErr := oldKp.Delete(); dErr != nil && !errors.Is(dErr, store.ErrNotFound) {
		fmt.Fprintf(w, "warning: %s migrated to DPAPI file, but could not delete the legacy keychain slot: %v (remove it manually if desired)\n", label, dErr)
	}
	fmt.Fprintf(w, "%s migrated from keychain to DPAPI file.\n", label)
	return legacy, migrateOutcomeDone, nil
}

// keyProviderWithDelete is the slice of KeyProvider + Delete used by the
// migration (the production KeyProvider interface only exposes Get/Set; Delete
// lives on the concrete providers). KeyringKeyProvider satisfies it.
type keyProviderWithDelete interface {
	store.KeyProvider
	Delete() error
}
