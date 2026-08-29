package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file implements the transactional self-replacement core (spec
// 2026-08-29-plan-44-self-update-rename §4.3): staged-binary fsync + version
// self-check, the GOOS-splitting replacement with generational .old backups
// and rollback on Windows / atomic rename + directory fsync elsewhere, the
// committed-with-error honesty rule, best-effort backup cleanup and the
// crash-window self-heal detection. It consumes Task 3's
// NormalizeVersionOutput as the staged-check contract.

// ErrRollbackFailed is returned when — after the running binary was renamed
// to its generation backup — the staged binary could not be moved into place
// AND the backup could not be renamed back. The system is left in the
// recoverable-but-broken state "backup exists, canonical missing"; the error
// message carries the manual recovery command (ren <backup> <self>) for the
// CLI to print verbatim.
var ErrRollbackFailed = errors.New("replace: rollback failed — manual recovery required")

// Seams for tests. Package vars solely so tests can inject failures and
// stand-ins; production code must never mutate them.
var (
	osRename     = os.Rename
	osRemove     = os.Remove
	osStat       = os.Stat
	osExecutable = os.Executable
	fileSync     = func(f *os.File) error { return f.Sync() }
	execStaged   = exec.CommandContext
	currentGOOS  = runtime.GOOS // ReplaceBinary's branch dispatch, seam for cross-branch tests
)

// Staged-version self-check guardrails (spec §4.3): 10s budget for the
// staged binary to answer `version`, output capped at 4KiB. Package vars
// solely so tests can shrink them; production code must never mutate them.
var (
	stagedCheckTimeout = 10 * time.Second
	stagedOutputLimit  = int64(4 << 10)
)

// oldGenSep is the separator of the generation backup suffix produced by
// replaceWindows: <self>.old.<unixts>.
const oldGenSep = ".old."

// oldBackup is one generation backup of self.
type oldBackup struct {
	path string
	ts   int64
}

// splitOldGeneration reports whether name has the shape "<stem>.old.<digits>"
// (the exact naming replaceWindows produces) and returns stem and timestamp.
// Non-digit or int64-overflow suffixes are not generations, and a name may
// not START with the separator (a bare ".old.123" hidden file is nothing).
func splitOldGeneration(name string) (stem string, ts int64, ok bool) {
	i := strings.LastIndex(name, oldGenSep)
	if i <= 0 {
		return "", 0, false
	}
	digits := name[i+len(oldGenSep):]
	if digits == "" {
		return "", 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", 0, false
		}
	}
	ts, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name[:i], ts, true
}

// oldGenerations lists self+".old.<ts>" siblings of self, sorted oldest →
// newest by timestamp (newest = last). Entries whose stem does not equal
// self's base name ("sshmgr2.old.123" next to self "sshmgr") are not ours.
func oldGenerations(self string) ([]oldBackup, error) {
	dir := filepath.Dir(self)
	base := filepath.Base(self)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []oldBackup
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), base) {
			continue
		}
		stem, ts, ok := splitOldGeneration(e.Name())
		if !ok || stem != base {
			continue
		}
		out = append(out, oldBackup{path: filepath.Join(dir, e.Name()), ts: ts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts < out[j].ts })
	return out, nil
}

// StagedFSync flushes the staged binary to stable storage. Per spec §4.3 it
// must succeed before any rename is attempted, on both platforms ("rename
// 前置,双平台"). The file is opened O_WRONLY: on Windows FlushFileBuffers
// requires a GENERIC_WRITE handle, so a read-only open would fail with access
// denied (on POSIX fsync accepts either).
func StagedFSync(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("staged fsync %s: %w", path, err)
	}
	defer f.Close()
	if err := fileSync(f); err != nil {
		return fmt.Errorf("staged fsync %s: %w", path, err)
	}
	return nil
}

// ReplaceBinary performs the transactional replacement of the running binary
// (spec §4.3), assuming the staged file already passed checksum verification
// and StagedVersionCheck. Invariants:
//   - any failure before the first successful rename leaves self and staged
//     byte-identical to the call's entry state (替换点之前的失败=零变更) — the
//     staged fsync gate runs here so no caller can order it wrong;
//   - on Windows the swap goes through a generational backup self+".old.<ts>"
//     with rollback on failure (double fault = ErrRollbackFailed);
//   - elsewhere the rename is atomic and a directory-fsync failure afterwards
//     is reported as CommittedWithError — the change IS in effect and is
//     never rolled back or disguised (committed-with-error).
func ReplaceBinary(staged, self string) error {
	// fsync 前置,双平台一致(structurally guaranteed before any rename; T8's
	// separate StagedFSync call for its evidence line merely re-runs it —
	// idempotent and cheap).
	if err := StagedFSync(staged); err != nil {
		return err
	}
	if currentGOOS == "windows" {
		return replaceWindows(staged, self)
	}
	return replaceUnix(staged, self)
}

// replaceWindows is the windows branch of spec §4.3, pseudocode verbatim.
// Windows allows renaming a running executable (but not deleting it), so the
// running image steps aside to a generation backup and the staged binary
// takes its place; a failed second rename rolls the backup back.
//
// Windows durability note (spec §4.3): no directory sync is attempted — Go
// cannot FlushFileBuffers a directory handle (access denied), so the NTFS
// metadata entry of the final rename is not explicitly flushed. The staged
// file itself is fsync'ed (gate above) and NTFS journaling guarantees a
// power cut leaves either the old or the new entry — both are complete
// binaries. That crash window is documented, not hidden.
func replaceWindows(staged, self string) error {
	// 清残留备份(尽力,失败仅警告;仍被旧进程持有的删不掉,正常)。
	// CleanOldBackups is specified to always return nil, so tolerated failures
	// are silent here by design.
	_ = CleanOldBackups(self)

	// 代际名防单槽被旧进程占死(spec 钉死 unix 秒级时间戳)。同秒重名的实态:
	// rename 到已存在的名字按覆盖语义走,但旧代文件仍被进程持有时该 rename
	// 直接失败 ⇒ 本次替换零变更(自愈可见),次秒重试即成——无需更细粒度命名。
	backup := self + ".old." + strconv.FormatInt(time.Now().Unix(), 10)
	if err := osRename(self, backup); err != nil {
		return fmt.Errorf("replace: rename %s -> %s: %w", self, backup, err)
	}
	if err := osRename(staged, self); err != nil {
		// 回滚"最新代际":backup 由 self 刚改名而来,即最新代际且必然持有
		// 原字节。按 ts 扫描剩余代际反而有风险——预置残留已被起手清理,而
		// 时钟回拨时 ts 扫描可能选中更旧的代际、恢复出更旧的字节。
		if rbErr := osRename(backup, self); rbErr != nil {
			return fmt.Errorf("%w: %s", ErrRollbackFailed, recoverCommand(backup, self))
		}
		return fmt.Errorf("replace: staged rename failed, rolled back to %s: %w", backup, err)
	}
	return nil
}

// CommittedWithError is the committed-with-error state (spec §4.3): the
// replacement rename HAS been performed — self carries the new bytes — but
// the post-rename directory fsync failed, so crash durability is not
// guaranteed. The update is in effect; callers must report it honestly, not
// roll back or pretend zero change. Detect via errors.As.
type CommittedWithError struct {
	Path string // the replaced binary path
	Err  error  // the directory fsync failure
}

func (e *CommittedWithError) Error() string {
	return fmt.Sprintf("replace: %s committed but directory fsync failed (a crash before the metadata flush may lose the change): %v",
		e.Path, e.Err)
}

func (e *CommittedWithError) Unwrap() error { return e.Err }

// replaceUnix is the Linux/darwin branch of spec §4.3: the rename is atomic
// (a running process keeps its old inode; failed renames touch nothing), and
// the directory fsync afterwards upgrades the change from "performed" to
// "durable". Its failure is committed-with-error, never a rollback.
func replaceUnix(staged, self string) error {
	if err := osRename(staged, self); err != nil {
		return fmt.Errorf("replace: rename %s -> %s: %w", staged, self, err)
	}
	if err := fsyncDir(filepath.Dir(self)); err != nil {
		return &CommittedWithError{Path: self, Err: err}
	}
	return nil
}

// fsyncDir flushes a directory entry change to stable storage (POSIX only —
// Go cannot FlushFileBuffers a directory handle on Windows, which is exactly
// why replaceWindows has no directory sync and documents its crash window).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := fileSync(d); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}

// CleanOldBackups removes leftover generation backups self+".old.<ts>"
// (spec §4.3: 尽力,失败仅警告;仍被旧进程持有的删不掉,正常). Per the pinned
// semantics it ALWAYS returns nil — individual removal failures are tolerated
// and not surfaced (they are residue hygiene, never an update outcome), and
// this library layer has no logger to warn through.
func CleanOldBackups(self string) error {
	gens, err := oldGenerations(self)
	if err != nil {
		return nil // cannot even list the directory: nothing best-effort to do
	}
	for _, g := range gens {
		_ = osRemove(g.path) // tolerated by design
	}
	return nil
}

// DetectHeal reports the crash-window states of §4.3's start-up self-heal.
// Two entries:
//  1. the canonical binary is missing while a generation backup
//     self+".old.<ts>" exists (crash between the two renames);
//  2. the running executable's own path carries the ".old.<ts>" generation
//     suffix and the canonical path is missing — the process IS the backup.
//
// The returned hint names both paths plus the manual recovery command for the
// CLI layer's interactive confirmation (自愈确认 = 交互式 only,--yes 不豁免 —
// enforcing that policy is the CLI's job, not this function's).
func DetectHeal() (healHint string, ok bool) {
	exe, err := osExecutable()
	if err != nil {
		return "", false
	}
	// Entry 2 first: we are executing FROM a generation backup; renaming
	// ourselves back to the canonical path is the recovery.
	if stem, _, isGen := splitOldGeneration(filepath.Base(exe)); isGen {
		canonical := filepath.Join(filepath.Dir(exe), stem)
		if fileExists(canonical) {
			return "", false // canonical present: mid-update normal, nothing to heal
		}
		return fmt.Sprintf("running from backup %s; canonical %s missing (crash between the two renames); recover with: %s",
			exe, canonical, recoverCommand(exe, canonical)), true
	}
	// Canonical self, as far as symlinks resolve. A missing target cannot be
	// resolved — fall back to the raw path, whose absence is the very
	// condition being tested below.
	self := exe
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		self = resolved
	}
	if fileExists(self) {
		return "", false
	}
	gens, err := oldGenerations(self)
	if err != nil || len(gens) == 0 {
		return "", false
	}
	newest := gens[len(gens)-1] // oldest→newest sorted; the freshest image wins
	return fmt.Sprintf("%s missing but backup %s exists (crash between the two renames); recover with: %s",
		self, newest.path, recoverCommand(newest.path, self)), true
}

// recoverCommand renders the manual recovery command for the current
// platform, executable verbatim. Windows: `ren` does NOT accept fully
// qualified paths as its destination argument (cmd /c 实测 "命令语法不正确")
// — `move /y` accepts both sides fully qualified and overwrites without
// prompting. POSIX: plain mv accepts both.
func recoverCommand(backup, self string) string {
	if currentGOOS == "windows" {
		return fmt.Sprintf(`move /y "%s" "%s"`, backup, self)
	}
	return fmt.Sprintf("mv %s %s", backup, self)
}

// fileExists reports whether path is present. Stat errors that are NOT
// NotExist (e.g. permission denied on a present file) count as "exists" —
// conservative: an unreadable canonical must never trigger a destructive
// recovery recommendation.
func fileExists(path string) bool {
	_, err := osStat(path)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// StagedVersionCheck executes the staged binary's `version` subcommand and
// compares its output against wantVersion under the §4.3 normalization
// contract (NormalizeVersionOutput on both sides). Guardrails: a 10s context
// kills a hung binary; stdout is capped at 4KiB (a flood is not a version).
// Any failure — unstartable, timeout, oversized output, mismatch — rejects
// the replacement (保留原文件,清 staged 由调用方处理).
//
// Returns the staged binary's normalized version, also on mismatch (for
// evidence lines). The target version is the caller's decision (spec: the
// GitHub/--version path uses the tag; the --file path uses the staged output
// itself as the baseline) — this function only compares.
func StagedVersionCheck(staged, wantVersion string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stagedCheckTimeout)
	defer cancel()
	cmd := execStaged(ctx, staged, "version")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("staged check: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("staged check: cannot run %s: %w", filepath.Base(staged), err)
	}
	out, readErr := readLimitedDrain(stdout, stagedOutputLimit)
	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("staged check: timed out after %s", stagedCheckTimeout)
	}
	if readErr != nil {
		return "", fmt.Errorf("staged check: %w", readErr)
	}
	if waitErr != nil {
		return "", fmt.Errorf("staged check: %s version exited with error: %w", filepath.Base(staged), waitErr)
	}
	got := NormalizeVersionOutput(string(out))
	if got == "" {
		// An empty version report is a broken binary under every mode —
		// never let it masquerade as the target version (want=="" would
		// otherwise compare equal).
		return "", errors.New("staged check: staged binary printed empty version")
	}
	if got != NormalizeVersionOutput(wantVersion) {
		return got, fmt.Errorf("staged check: version mismatch: staged reports %q, want %q", got, wantVersion)
	}
	return got, nil
}

// readLimitedDrain reads at most limit bytes; limit+1 bytes available is a
// failure. Unlike transport.go's readLimited (which reads a closed HTTP body
// and needs no drain), the staged check reads a live child's pipe: on
// overflow the rest is drained (up to 1MiB) so a well-behaved child never
// blocks on a full pipe — a pathological flood beyond that is killed by the
// context deadline instead.
func readLimitedDrain(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 1<<20))
		return nil, fmt.Errorf("output exceeds %d byte limit", limit)
	}
	return b, nil
}
