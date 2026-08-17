# Plan 26 Task 1 Report: upload 根目录跟链解析（junction/symlink root）

> NOTE ON LOCATION: the orchestrator-specified paths (`.git/worktrees/plan-26-arrears/sdd/task-1-report.md`, primary and fallback) are both unwritable from this isolated session (worktree guard blocks writes outside the worktree; the primary path also fails ENOTDIR because `.git` inside a worktree is a pointer file). This copy lives at the worktree mirror `sdd/task-1-report.md` — the coordinator should relocate/copy it next to the brief if the pipeline reads it from the .git path.

## Status: DONE_WITH_CONCERNS (one justified deviation from the brief's verbatim implementation — see below)

## What I implemented

- `internal/sshbroker/upload_test.go`: appended `TestUploadDirSymlinkRootResolved` + the `makeDirLink` helper (exact name, reused by Task 2) — both verbatim from the brief. Imports `os/exec` + `runtime` added per the brief's gofmt-grouped note.
- `internal/sshbroker/upload.go`: at `uploadDir` entry, the brief's verbatim root-resolution block (`filepath.EvalSymlinks` + `localRoot = resolved`) plus — necessarily — a Readlink follow-through loop for Windows junctions (see Deviation). One sentence appended to the `uploadDir` doc comment (existing wording untouched), updated to name both mechanisms accurately.

## Deviation from the brief (the concern)

The brief's Step 3 code alone (`filepath.EvalSymlinks(localRoot)`) is a **silent no-op for junctions on go1.25.8/windows** — verified with a standalone probe before extending:

```
mklink /J diag26-link -> diag26-real          : ok
os.Lstat(junction)  -> mode=?rw-rw-rw- IsDir=false   (? = ModeIrregular)
os.Stat(junction)   -> mode=drwxrwxrwx IsDir=true
filepath.EvalSymlinks(junction) -> returns the LINK path UNCHANGED, err=nil
os.Readlink(junction)           -> returns the target, err=nil
```

EvalSymlinks only follows `ModeSymlink` entries; a junction Lstats as `ModeIrregular`, so the brief's verbatim insertion left the Windows junction lane of the brief's own test RED (`read ...link-root: Incorrect function.`) — and that lane is the test's stated privilege-free Windows mechanism (`mklink /J`). The brief's test (must pass) and its implementation snippet (insufficient) conflict on this host; junction resolution is the task's core, so I kept the brief's block verbatim and appended the minimal fixpoint loop: Lstat → if `ModeSymlink|ModeIrregular` → Readlink (relative targets joined against the link's dir) → repeat, capped at 64 iterations (ELOOP-style bound; symlink cycles terminate with `too many levels of symbolic links` instead of hanging). Errors from EvalSymlinks/Lstat/Readlink propagate unchanged, matching the brief's `return rerr` shape.

Consequences checked:
- Regular-dir roots: loop breaks on first Lstat — one extra syscall, no behavior change.
- Unix symlink roots: EvalSymlinks already resolved them; loop breaks immediately.
- Nonexistent root: still caught by Upload's entry `os.Stat` (unchanged); the new error paths are TOCTOU guards.
- Nested entries: untouched (lstat semantics preserved) — Task 2 stacks its nested-subdir refusal on top as planned.
- Unix root that Lstats as ModeIrregular (rare): now errors at Readlink instead of later at file read — was already an error case either way; behavior class unchanged.

## TDD Evidence

**RED** (test added, no fix yet):
```
$ go test ./internal/sshbroker/ -count=1 -run TestUploadDirSymlinkRootResolved -v
--- FAIL: TestUploadDirSymlinkRootResolved (0.07s)
    upload_test.go:506: symlink-root Upload: read C:\...\link-root: Incorrect function.
```
Exactly the brief's predicted mode: Walk lstats the junction root, misclassifies it as a file, io.Copy reads a directory handle → raw Windows error, no upload semantics.

**RED #2** (brief's verbatim EvalSymlinks insertion only — proves the deviation was necessary):
```
$ go test ./internal/sshbroker/ -count=1 -run TestUploadDirSymlinkRootResolved -v
--- FAIL: TestUploadDirSymlinkRootResolved (0.12s)
    upload_test.go:506: symlink-root Upload: read C:\...\link-root: Incorrect function.
```
EvalSymlinks returned the junction unchanged (nil error), so the walk still started at the link.

**GREEN** (with junction follow-through):
```
$ go test ./internal/sshbroker/ -count=1 -run TestUploadDirSymlinkRootResolved -v
--- PASS: TestUploadDirSymlinkRootResolved (0.14s)
```

**Regression**:
```
$ go test ./internal/sshbroker/ -count=1
ok  	ssh-manager-mcp/internal/sshbroker	4.110s        (incl. Plan 24 symlink cap-gate + broken-link tests)
$ go test ./... -count=1
all packages ok (cli, clientops, conformance, eval, importer, mcpserver, paths, roles, sshbroker, store, testsshd, tui, vault, vaultio)
$ gofmt -l internal/sshbroker
(no output — clean)
```

## Files changed

- `internal/sshbroker/upload.go` — root resolution at uploadDir entry + doc sentence
- `internal/sshbroker/upload_test.go` — new test + makeDirLink helper + 2 imports

## Commit

`eb1d2d8` feat(upload): symlink/junction upload root resolved via EvalSymlinks (Plan 26 T1)
(body documents the junction follow-through rationale; Co-Authored-By trailer)

## Self-review findings

- Completeness: brief's test/helper/implementation structure all in place; nonexistent-root path unchanged (Upload entry os.Stat) with TOCTOU guards added; no brief items skipped.
- Quality: `makeDirLink` exact name; comments document the empirical junction finding (ModeIrregular vs ModeSymlink) so the next reader knows why the loop exists; cycle cap present.
- Discipline: only the two plan files touched; imports limited to `os/exec`, `runtime` (+ existing `fmt` reused); diagnostic probe artifacts removed from OS temp; nothing beyond the brief beyond the necessary junction fix.
- Testing: RED captured before GREEN (twice — including after the verbatim-only attempt, which is the evidence justifying the deviation); full package + full repo + gofmt all clean.
