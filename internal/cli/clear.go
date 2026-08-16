// Package cli: `ssh-manager clear` — the high-ceremony teardown (Plan 19
// spec §3 v2). The interactive sequence is PINNED by the spec:
//
//	enumerate what exists → show list → typed "DELETE" → (vault roles:
//	verified safety-net export + one-time passphrase + "y" confirm) →
//	idempotent teardown (service → files → legacy timer → role.json).
//
// Anything wrong typed at EITHER prompt = 已取消, exit 0, ZERO mutation. The
// safety net is only made for vault roles (standalone/server); a client
// machine has no secrets to rescue.
//
// Non-TTY stdin is refused outright (「clear 需要交互式终端」) so a script
// piping "DELETE\n" can never wipe a vault.
package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/tui"
	"ssh-manager-mcp/internal/vaultio"
)

// ---------------------------------------------------------------------------
// External-effect seams (test injection only — production defaults are the
// real implementations; every one of them touches something outside the
// process: the TTY, the SCM, schtasks, or the user's home dir).
// ---------------------------------------------------------------------------

// clearStdinIsTTY gates clear on an interactive stdin via the shared
// tui.IsTerminal console check (GetConsoleMode on Windows, char-device stat
// elsewhere). This closed the NUL hole the old inline stat check had
// (Plan 20 A4: `clear < NUL` used to pass as a char device). The typed
// DELETE + y ceremony remains the real guard; this is script-proofing only.
var clearStdinIsTTY = func() bool {
	return tui.IsTerminal(os.Stdin.Fd())
}

// serveInstalledFn drives the ENUMERATION marker only. Execution ALWAYS
// attempts the idempotent uninstall (see runClear) — a probe false-negative
// must never leave a registered service pointing at deleted files.
var serveInstalledFn = serveServiceInstalled

// serveUninstallFn is T4's extracted core, seam'd for tests.
var serveUninstallFn = uninstallServeService

// deleteLegacyTimerFn / legacyTimerPresentFn wrap the platform timer
// functions (schtasks on Windows, no-op on Unix).
var (
	deleteLegacyTimerFn  = deleteLegacyTimer
	legacyTimerPresentFn = legacyTimerPresent
)

// safetyNetHomeDir is where the safety-net backup is written (default: the
// user's home dir per spec §3).
var safetyNetHomeDir = os.UserHomeDir

// serveServiceInstalled reports whether the serve service is registered —
// DISPLAY-ONLY heuristic (drives the "serve:" marker line in the enumerated
// list). Probing is svc.Status(): ErrNotInstalled / no-service-manager / any
// probe error → false. runClear does not gate the actual uninstall on this —
// it always calls the idempotent uninstallServeService, whose "not installed"
// outcome is a printed no-op, so a probe miss can never strand a registered
// service whose vault files were just deleted.
func serveServiceInstalled() bool {
	cfg := &service.Config{Name: serveServiceName, Option: platformServiceOptions()}
	s, err := service.New(&program{}, cfg)
	if err != nil {
		return false
	}
	_, err = s.Status()
	return err == nil
}

// ---------------------------------------------------------------------------
// Target enumeration
// ---------------------------------------------------------------------------

// clearTarget is one enumerated deletion candidate. path=="" marks a
// non-file component (the service registration, the legacy timer) that the
// teardown handles through its own code path.
type clearTarget struct {
	prefix string // vault / serve / client / task / role (display category)
	path   string // file path to delete; "" = non-file marker
	desc   string // the exact line shown to the user ("<prefix>: <path-or-desc>")
}

// clearVaultDir is the directory the vault artifacts live in: the dir of the
// resolved store path (SSHMGR_STORE override-aware) — the same resolution
// roles.vaultRolePath uses, so role.json, store.db and the serve files are
// enumerated from ONE dir in production and tests alike.
func clearVaultDir() string {
	p, err := storePath()
	if err != nil {
		return ""
	}
	return filepath.Dir(p)
}

// scanClearTargets enumerates what EXISTS (Stat-gated). Role is carried per
// the task contract but deliberately does NOT gate the scan: a server machine
// may hold client residue (and vice versa), and clear must catch every
// leftover regardless of what role.json claims. The caller uses the role for
// the header label and the safety-net decision.
func scanClearTargets(_ roles.Role) []clearTarget {
	var ts []clearTarget
	seen := map[string]bool{}
	add := func(prefix, p string) {
		if p == "" || seen[p] {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		seen[p] = true
		ts = append(ts, clearTarget{prefix: prefix, path: p, desc: prefix + ": " + p})
	}

	vd := clearVaultDir()
	if vd != "" {
		// vault data (sqlite store + master key)
		add("vault", filepath.Join(vd, "store.db"))
		add("vault", filepath.Join(vd, "store.db-wal"))
		add("vault", filepath.Join(vd, "store.db-shm"))
		// store.db.meta.json: passphrase salt sidecar written by unlock.
		// If leftover from an old vault and reused with the same passphrase,
		// the same salt would derive the same master key — cross-generation
		// key reuse. clear removes it to prevent this.
		add("vault", filepath.Join(vd, "store.db.meta.json"))
		// client cache DEK: paths.CacheDekPath pins it to the PROGRAM-FIXED
		// vault dir (no SSHMGR_STORE coupling), so it may differ from vd on a
		// migrated machine — enumerate BOTH resolutions, deduped.
		add("client", filepath.Join(vd, paths.CacheDekFilename))
		if dek, err := paths.CacheDekPath(); err == nil {
			add("client", dek)
		}
		// serve artifacts: env overrides honored first (same discipline as
		// roles.serveCertPresent), else next to the vault.
		for _, c := range []struct{ env, file string }{
			{"SSHMGR_SERVE_CERT", paths.ServeCertFilename},
			{"SSHMGR_SERVE_KEY", paths.ServeKeyFilename},
			{"SSHMGR_SERVE_MARKER", paths.ServeCertMarkerFilename},
		} {
			if v := os.Getenv(c.env); v != "" {
				add("serve", v)
			} else {
				add("serve", filepath.Join(vd, c.file))
			}
		}
		// serve log + its rotated generation (Plan 22 T4: a >5MiB serve.log is
		// rotated to serve.log.1, one generation kept). Like the cache DEK,
		// paths.ServeLogPath pins to the PROGRAM-FIXED vault dir unless
		// SSHMGR_SERVE_LOG is set, so it may differ from vd on a relocated
		// machine — enumerate BOTH resolutions, deduped.
		add("serve", filepath.Join(vd, paths.ServeLogFilename))
		add("serve", filepath.Join(vd, paths.ServeLogFilename+".1"))
		if lp, err := paths.ServeLogPath(); err == nil {
			add("serve", lp)
			add("serve", lp+".1")
		}
	}
	// master.key: env-aware (SSHMGR_FILEKEY_PATH), may live outside the vault dir.
	if mk, err := paths.MasterKeyPath(); err == nil {
		add("vault", mk)
	}
	// client cache dir (SSHMGR_CACHE_DIR / UserConfigDir → ssh-manager)
	if dir, _, _, _, err := clientops.CachePaths(); err == nil {
		add("client", filepath.Join(dir, "cache.bin"))
		add("client", filepath.Join(dir, "cache.auth.json"))
		add("client", filepath.Join(dir, "cache.meta.json"))
		add("client", filepath.Join(dir, "cache-audit.log"))
	}
	// role.json — BOTH locations (a machine can hold residue in each).
	if p, err := roles.RolePath(roles.RoleServer); err == nil {
		add("role", p)
	}
	if p, err := roles.RolePath(roles.RoleClient); err == nil {
		add("role", p)
	}
	// non-file markers
	if serveInstalledFn() {
		ts = append(ts, clearTarget{
			prefix: "serve",
			desc:   "serve: 服务「" + serveServiceName + "」（停止+卸载）",
		})
	}
	if legacyTimerPresentFn() {
		ts = append(ts, clearTarget{
			prefix: "task",
			desc:   "task: 计划任务 " + legacyTimerName,
		})
	}
	return ts
}

// enumClearTargets returns the display lines of the enumerated deletion
// candidates (Stat-gated; only what actually exists appears).
func enumClearTargets(role roles.Role) []string {
	ts := scanClearTargets(role)
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.desc
	}
	return out
}

// ---------------------------------------------------------------------------
// Safety net
// ---------------------------------------------------------------------------

// makeSafetyNet exports the vault to a passphrase-encrypted file in the home
// dir (~/ssh-manager-backup-<UTC时间戳>.sme), VERIFIES it by decrypting the
// just-written file and unmarshaling into store.Snapshot, and returns the
// path + the one-time passphrase. Any failure = error, ZERO mutation of the
// vault (the caller must abort). The store is closed before returning — on
// Windows an open sqlite handle would block the teardown's os.Remove.
func makeSafetyNet() (path, passphrase string, err error) {
	st, err := openUnlockedStore()
	if err != nil {
		return "", "", fmt.Errorf("vault 未解锁或不可读：请先 `ssh-manager unlock`（clear 不提供无备份删除）: %w", err)
	}
	defer st.Close()

	snap, err := st.ExportSnapshot()
	if err != nil {
		return "", "", fmt.Errorf("导出快照失败（未做任何改动）: %w", err)
	}
	blob, err := json.Marshal(snap)
	if err != nil {
		return "", "", fmt.Errorf("序列化快照失败（未做任何改动）: %w", err)
	}
	passphrase, err = store.GenerateToken()
	if err != nil {
		return "", "", fmt.Errorf("生成口令失败（未做任何改动）: %w", err)
	}
	enc, err := vaultio.Encrypt([]byte(passphrase), blob)
	if err != nil {
		return "", "", fmt.Errorf("加密快照失败（未做任何改动）: %w", err)
	}
	home, err := safetyNetHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("定位用户主目录失败（未做任何改动）: %w", err)
	}
	path = filepath.Join(home, "ssh-manager-backup-"+time.Now().UTC().Format("20060102T150405Z")+".sme")

	// unique temp + rename (same atomicity discipline as every other secret
	// write in this repo; a torn safety net is worse than no safety net).
	tmp, err := os.CreateTemp(home, ".ssh-manager-backup-*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("写入安全网失败（未做任何改动）: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("写入安全网失败（未做任何改动）: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("写入安全网失败（未做任何改动）: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("写入安全网失败（未做任何改动）: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", "", fmt.Errorf("写入安全网失败（未做任何改动）: %w", err)
	}

	// 回读校验: a safety net that cannot be decrypted/unmarshaled is worse
	// than useless — it would lull the user into deleting the real vault.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("安全网回读失败（未做任何改动；请手动检查 %s）: %w", path, err)
	}
	plain, err := vaultio.Decrypt([]byte(passphrase), onDisk)
	if err != nil {
		return "", "", fmt.Errorf("安全网回读校验失败（未做任何改动；请手动删除 %s 后重试）: %w", path, err)
	}
	var back store.Snapshot
	if err := json.Unmarshal(plain, &back); err != nil {
		return "", "", fmt.Errorf("安全网回读校验失败（未做任何改动；请手动删除 %s 后重试）: %w", path, err)
	}
	return path, passphrase, nil
}

// ---------------------------------------------------------------------------
// Role resolution for clear
// ---------------------------------------------------------------------------

// clearResolveRole resolves the machine's role for the clear flow. Unlike the
// launcher, clear must keep working when role.json is the BROKEN state (an
// invalid role.json is one of the anomalies clear exists to fix) — so a
// ResolveMode error falls back to the filesystem probe: vault present →
// vault role (safety net needed), else client-like (nothing to export).
func clearResolveRole() roles.Role {
	l, err := roles.ResolveMode("")
	if err == nil {
		if l.Role != "" {
			return l.Role
		}
		// LaunchWizard on an empty machine — no vault, nothing to export.
		return roles.RoleClient
	}
	if roles.VaultExists() {
		return roles.RoleServer
	}
	return roles.RoleClient
}

// ---------------------------------------------------------------------------
// The command
// ---------------------------------------------------------------------------

func newClearCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "clear",
		Short: "Permanently remove ALL ssh-manager data on this machine (typed DELETE + verified safety net)",
		Long: `Remove every ssh-manager artifact on this machine: the vault (store.db +
master.key), serve service + certificates, the offline client cache, the
legacy cache-refresh scheduled task, and role.json. Afterwards the machine is
back to the first-run wizard state.

For vault machines (standalone/server) an encrypted safety-net export is
written to ~/ssh-manager-backup-<timestamp>.sme BEFORE anything is deleted,
and its one-time passphrase is shown exactly once. Requires an interactive
terminal: type DELETE to confirm, then y to confirm the passphrase was
copied down. Any other answer cancels with zero changes. The whole teardown
is idempotent — re-running clear after a mid-way failure skips what is
already done.`,
		Args: cobra.NoArgs,
		RunE: runClear,
	}
	return c
}

func runClear(cmd *cobra.Command, _ []string) error {
	if !clearStdinIsTTY() {
		return errors.New("clear 需要交互式终端（stdin 不是 TTY）——为防止脚本误删，拒绝执行")
	}
	out := cmd.OutOrStdout()
	in := bufio.NewReader(cmd.InOrStdin())

	role := clearResolveRole()
	targets := scanClearTargets(role)
	fmt.Fprintf(out, "本机角色：%s\n", role)
	fmt.Fprintln(out, "以下文件/组件将被永久删除（按实际存在枚举）：")
	if len(targets) == 0 {
		fmt.Fprintln(out, "  （本机没有可清理的 ssh-manager 数据）")
	}
	for _, t := range targets {
		fmt.Fprintf(out, "  ▸ %s\n", t.desc)
	}
	fmt.Fprintln(out, "（exe 不会删除；clear 后本机回到首次向导状态）")

	// Prompt 1: typed DELETE. Wrong word / empty / EOF = cancel, zero changes.
	fmt.Fprint(out, "输入 DELETE 确认：")
	line, _ := in.ReadString('\n')
	if strings.TrimSpace(line) != "DELETE" {
		fmt.Fprintln(out, "\n已取消，未做任何改动。")
		return nil
	}

	// Vault present → verified safety net + one-time passphrase + y confirm.
	// Gated on the FILESYSTEM (store.db actually exists among the enumerated
	// targets), not the resolved role: a tampered/hand-edited role.json=client
	// on a machine that still holds a real vault must not dodge the export
	// safety net (role only drives the display label above).
	if roles.VaultExists() {
		backupPath, passphrase, err := makeSafetyNet()
		if err != nil {
			return err // zero mutation by contract
		}
		fmt.Fprintf(out, "安全网已写入：%s\n", backupPath)
		fmt.Fprintf(out, "口令：%s\n", passphrase)
		fmt.Fprintln(out, "⚠ 此口令仅显示一次 —— 请立即抄录；没有它备份文件无法解开。")
		// Prompt 2: y confirms the passphrase was copied down.
		fmt.Fprint(out, "按 y 确认已抄录口令：")
		confirm, _ := in.ReadString('\n')
		if strings.TrimSpace(confirm) != "y" {
			fmt.Fprintln(out, "\n已取消，未做任何改动。")
			return nil
		}
	}

	// Teardown (idempotent — every step tolerates its work already being done).
	// 1. Service FIRST: a running serve holds store.db open (Windows delete
	//    lock) and a registered service whose files are gone would crash-loop
	//    at boot. Always attempted, not gated on the display probe.
	if err := serveUninstallFn(out); err != nil {
		return fmt.Errorf("卸载 serve 服务失败: %w — 请以管理员/root 身份重新运行 ssh-manager clear（已完成步骤会自动跳过）", err)
	}
	// 2. Delete every enumerated file (ENOENT = already gone = fine).
	for _, t := range targets {
		if t.path == "" {
			continue // service / timer markers are handled above / below
		}
		if err := os.Remove(t.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("删除 %s 失败: %w（重跑 clear 将跳过已完成步骤）", t.path, err)
		}
	}
	// 3. Legacy timer (client artifact; Windows-only, best-effort).
	if err := deleteLegacyTimerFn(); err != nil {
		fmt.Fprintf(out, "warning: 删除遗留计划任务失败（不影响其余清理；可手动执行 schtasks /Delete /TN %s /F）: %v\n", legacyTimerName, err)
	}
	// 4. role.json — both locations; roles.Delete is itself ENOENT-tolerant.
	if err := roles.Delete(); err != nil {
		return fmt.Errorf("删除 role.json 失败: %w（重跑 clear 将跳过已完成步骤）", err)
	}

	fmt.Fprintln(out, "已清理。下次 ssh-manager tui 将重新进入首次向导。")
	return nil
}
