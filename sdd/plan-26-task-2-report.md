# Plan 26 Task 2 Report — upload 嵌套符号目录显式拒绝（cap 无关）+ 语义文档

**Status: DONE_WITH_CONCERNS** (one minimal, evidence-forced deviation from the brief's verbatim Step 3 code — details below; semantics, error text, tests, and docs are exactly per brief)

Commit: `31f5e9d` `feat(upload): nested symlinked dirs refused by name (cap-independent); root follows link — semantics documented (Plan 26 T2)`

## What was implemented

1. **Tests (Step 1)** — the brief's 3 tests appended verbatim to `internal/sshbroker/upload_test.go` (after the shared `makeDirLink` helper, not redefined):
   - `TestUploadDirNestedSymlinkedDirRefused` — armed cap, refuses naming `z-link`, `a.txt` remains uploaded (Plan 23 "already-completed files remain" contract).
   - `TestUploadDirNestedSymlinkedDirRefusedNoCap` — cap==0 still refuses (check not under the cap-armed branch).
   - `TestUploadJunctionNestedRefused_windows` — Windows-only, junction via `makeDirLink` (mklink /J, privilege-free; genuinely ran on this host).

2. **Implementation (Step 3)** — replaced the Plan 24 walk-callback block in `internal/sshbroker/upload.go` with the brief's cap-independent three-part structure: follow re-stat → dir-target refusal (exact error text verbatim: `symlinked directory not uploaded: %s — upload the target directory directly (following directory links recursively is not supported)`) → file-target followed size into the cap gate → uploadFile.

3. **Comment accuracy** — rewrote the walk-callback comment and updated both stale Plan 24 doc-comment passages (`Upload`'s per-file pre-flight bullet, `uploadDir`'s walk description) so nothing survives claiming the re-stat is cap-gated or that dir targets fall into the file branch.

4. **Docs (Step 5)**:
   - `internal/mcpserver/server.go` upload_file Description: brief's sentence inserted verbatim right after "a directory is uploaded recursively, preserving relative paths".
   - `docs/scenarios.md` 场景 3 要点: one bullet stating the three states (root follows link / nested symlink→dir refused with the exact error shape / nested symlink→file follows, cap judged on target size per Plan 24).
   - `docs/managing-servers.md`: **skipped** — `grep -i upload` over the file returns zero matches (brief says skip when none; grep evidence, not memory).

## TDD evidence

**RED** (Step 2, before implementation):

```
go test ./internal/sshbroker/ -count=1 -run 'TestUploadDirNestedSymlinkedDirRefused|TestUploadJunctionNestedRefused' -v
--- FAIL: TestUploadDirNestedSymlinkedDirRefused
    upload_test.go:552: want named refusal naming z-link, got: read C:\...\z-link: Incorrect function.
--- FAIL: TestUploadDirNestedSymlinkedDirRefusedNoCap
    upload_test.go:574: cap==0 must still refuse, got: read C:\...\z-link: Incorrect function.
--- FAIL: TestUploadJunctionNestedRefused_windows
    upload_test.go:595: junction must be refused like a dir symlink, got: read C:\...\z-junc: Incorrect function.
```

Exactly the misleading platform-dependent open/read error the plan's background predicted (ERROR_INVALID_FUNCTION reading a directory handle on Windows).

**GREEN** (same command after implementation): all 3 PASS. Output pristine (no logs beyond pass lines).

**Full package (Step 4)**: `go test ./internal/sshbroker/ -count=1` → `ok ssh-manager-mcp/internal/sshbroker 5.078s` (T1 + 3 new + all Plan 23/24 existing). Re-ran after comment refinement: green again. `gofmt -l internal/sshbroker internal/mcpserver` and `go vet` on both packages: zero output.

## Files changed

- `internal/sshbroker/upload.go` — walk-callback symlink block + 2 doc-comment updates
- `internal/sshbroker/upload_test.go` — +3 tests (verbatim from brief)
- `internal/mcpserver/server.go` — upload_file Description +1 sentence (verbatim)
- `docs/scenarios.md` — +1 bullet in upload 要点

## Deviation from brief (the one concern) — READ THIS

The brief's Step 3 snippet gates the re-stat on `info.Mode()&os.ModeSymlink != 0` only. **That condition cannot pass the brief's own tests on Windows**, because all three tests create dir links via `makeDirLink`, which on Windows is a junction (`mklink /J`), and on this toolchain junctions do NOT lstat as ModeSymlink:

- Repo pins `go 1.25.8` (go.mod), no `godebug` overrides.
- Go 1.25.8 `src/os/types_windows.go` `fileStat.mode()`: name-surrogate reparse points (symlink AND mount point) get NO ModeDir; only tag `IO_REPARSE_TAG_SYMLINK` sets ModeSymlink; mount points (junctions) fall to `default: m |= ModeIrregular`. (The pre-1.23 ModeSymlink-for-junctions behavior exists only behind `godebug winsymlink=0`.)
- T1's landed root loop already encodes exactly this: `fi.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0` as its break condition, with a comment saying junctions Lstat as ModeIrregular.
- RED evidence agrees: with the current (symlink-only) check the junction fell straight through to uploadFile.

**Change made**: gate condition is `info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0` — i.e. the brief's code verbatim except the junction bit added, mirroring T1's root loop. Everything else (structure, exact error text, cap-independence, comment intent) is the brief's. Blast radius of including ModeIrregular:

- Non-link files are unaffected (regular files never lstat Irregular on any platform here).
- A non-surrogate Irregular reparse file (e.g. non-hydrated cloud placeholder) now re-stats: Stat returns the file itself, IsDir false, size = followed size — the cap gate becomes MORE accurate; transfer behavior unchanged.
- Broken junction now errors at the re-stat naming the path — same contract as broken symlink (Plan 24).

If the reviewer wants the literal snippet instead, tests 1-3 fail on Windows — the deviation is required to satisfy the brief's own Step 4 acceptance. Everything else in the brief was followed verbatim.

## Self-review notes / minor observations (no action taken)

- `internal/mcpserver/types.go:52` (UploadInput.LocalPath jsonschema description) and `internal/eval/README.md:387` also carry the "a directory is uploaded recursively, preserving relative paths" wording but are NOT in the brief's file list — left untouched. If tool-description parity is desired, they'd need a follow-up (types.go is also touched by T5 separately).
- Commit stages only the brief's 4 files; a pre-existing unstaged deletion of `sdd/task-1-report.md` (orchestrator's doing) was left alone.
- No new deps; iron rule untouched (no credential-path code near this change).
