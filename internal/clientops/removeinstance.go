// Package clientops: Plan 46 T2 — instance removal as a first-class operation
// (`sshmgr cache instances rm`), plus the PROCESS-LEVEL write gate shared by
// every instance-write section (RemoveInstance / forceCleanInstance take it
// exclusively; DoPull / WriteAndPull take the shared side, non-blocking).
//
// 并发边界(plan 定案,如实):进程内 rm/force 进行中,并发 pull 与 pair 写盘
// 被【拒绝】——返回明确错误,不排队、不交错;跨进程并发不由任何文件锁拦截,
// 由 atomicWriteUnique 原子写 + rm 幂等可重跑兜底(文档职责,见 docs)。
package clientops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/paths"
)

// cacheWriteMu is the package-level instance-write mutex. RemoveInstance /
// forceCleanInstance hold it EXCLUSIVELY for the duration of their disk
// mutations; DoPull / WriteAndPull take the shared side via beginCacheWrite
// with TryRLock semantics (never blocks, never queues). Deliberate
// consequence (plan 定案:拒绝而非排队): the moment a removal is in progress —
// or even merely WAITING for an in-flight pull's write section to drain — new
// pulls/pair writes in this process are REJECTED with a clear error. With no
// removal around, TryRLock succeeds immediately and the existing concurrent-
// pull posture (atomicWriteUnique) is unchanged.
var cacheWriteMu sync.RWMutex

// beginCacheWrite takes the shared side of cacheWriteMu (non-blocking) and
// returns the release func on success. Failure means a removal/force cleanup
// is in progress in this process: the caller surfaces the refusal, never
// retries inline, never waits.
func beginCacheWrite(instance string) (func(), error) {
	if !cacheWriteMu.TryRLock() {
		return nil, fmt.Errorf("instance %q write refused: a concurrent `sshmgr cache instances rm` (or force cleanup) is in progress in this process — retry after it finishes", instance)
	}
	return cacheWriteMu.RUnlock, nil
}

// RemoveInstance deletes one NAMED instance's local state at BOTH roots: the
// slot directory (instances/<name>/ — auth/bin/meta/config, the pair artifact
// and the quarantine subtree all live inside it) and the per-instance DEK file
// (cache-dek-<name>.key under the vault dir / SSHMGR_CACHE_DEK_DIR).
//
// Path safety (plan 定案,三层defense):
//  1. the single-slot env overrides are refused outright — with either set the
//     paths below would NOT resolve to the named instance (same mutex the CLI
//     layer enforces via checkInstanceFlag; repeated here because this is an
//     exported entry point);
//  2. instname.Valid — the whitelist already bans separators, traversal
//     shapes, and Windows reserved device names (CON/NUL/COM1…, first
//     dot-segment), so the Join can never leave the root;
//  3. canonicalize + filepath.Rel — belt-and-braces: both targets are
//     Abs+Clean'ed and refused unless they resolve INSIDE their root.
//     The user-supplied name never reaches os.RemoveAll unvalidated.
//
// Idempotence: an already-absent slot dir or DEK counts as success, so a
// re-run after a partial failure finishes the cleanup. Anything still present
// after both steps is reported in the error (残留物清单).
//
// 并发(进程内): holds cacheWriteMu exclusively — concurrent DoPull /
// WriteAndPull write sections in THIS process are refused while it runs.
// The broker-side device code is untouched (client has no revoke power; the
// CLI prints the `cache-tokens revoke` companion hint).
func RemoveInstance(instance string) error {
	if instance == "" {
		return errors.New("refusing to remove the DEFAULT instance slot (it has no name) — `sshmgr clear` removes all local sshmgr data")
	}
	for _, env := range []string{"SSHMGR_CACHE_DIR", "SSHMGR_CACHE_DEK"} {
		if os.Getenv(env) != "" {
			return fmt.Errorf("%s is set — it fully overrides the cache path/DEK resolution, so this removal would target the wrong slot; unset it and re-run", env)
		}
	}
	if verr := instname.Valid(instance); verr != nil {
		return verr
	}
	root, err := InstancesRoot()
	if err != nil {
		return err
	}
	slotDir, err := canonicalUnderRoot(root, filepath.Join(root, instance))
	if err != nil {
		return err
	}
	dekPath, err := paths.CacheDekPathFor(instance)
	if err != nil {
		return err
	}
	dekPath, err = canonicalUnderRoot(filepath.Dir(dekPath), dekPath)
	if err != nil {
		return err
	}

	cacheWriteMu.Lock()
	defer cacheWriteMu.Unlock()

	dirErr := os.RemoveAll(slotDir)
	var dekErr error
	if d, ok := DekProvider(instance).(interface{ Delete() error }); ok {
		dekErr = d.Delete()
	}

	// Residue audit is the source of truth: a now-absent target is the
	// idempotent success; anything still present (or un-stat-able) is residue.
	var residue []string
	for _, p := range []string{slotDir, dekPath} {
		switch _, serr := os.Stat(p); {
		case serr == nil:
			residue = append(residue, p)
		case !errors.Is(serr, fs.ErrNotExist):
			residue = append(residue, fmt.Sprintf("%s (stat: %v)", p, serr))
		}
	}
	if len(residue) > 0 {
		var causes []string
		if dirErr != nil {
			causes = append(causes, fmt.Sprintf("slot dir: %v", dirErr))
		}
		if dekErr != nil {
			causes = append(causes, fmt.Sprintf("dek: %v", dekErr))
		}
		return fmt.Errorf("instance %q removal incomplete — leftovers remain: %s (%s); the command is idempotent — re-run the same `sshmgr cache instances rm %s` to finish the cleanup",
			instance, strings.Join(residue, "; "), strings.Join(causes, "; "), instance)
	}
	return nil
}

// canonicalUnderRoot Abs+Clean's target and refuses any escape from root —
// the second belt after instname.Valid (which already bans separators, so a
// straight Join cannot leave the root; this closes any future path-shape
// drift). Rel across drives errors → refused.
func canonicalUnderRoot(root, target string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, rerr := filepath.Rel(rootAbs, targetAbs)
	if rerr != nil {
		return "", fmt.Errorf("path %q does not resolve under root %q: %v", targetAbs, rootAbs, rerr)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes root %q — refusing traversal", targetAbs, rootAbs)
	}
	return targetAbs, nil
}
