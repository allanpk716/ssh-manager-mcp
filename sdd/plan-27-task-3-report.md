# Plan 27 Task 3 Report — 副本解密探针（NUC10 FINDING A 检测器）

**Status: DONE** — commit `94465f2` on `worktree-plan-27-doctor`
**Files changed:** `internal/cli/doctor.go` (+117/−3), `internal/cli/doctor_test.go` (+247)

## What was implemented

### `probeVaultDecrypt(storePath, keyPath string) (servers, creds int, err error)` (doctor.go:315)

Exact signature per the interface spec. Flow: `os.ReadFile` master.key → `os.ReadFile` store.db → `os.MkdirTemp("", "sshmgr-doctor-*")` → write the store bytes as `<scratch>/store.db` (0600) → `store.Open(copy, key)` → `ExportSnapshot()` → `len(snap.Servers)` / `len(snap.Credentials)` → deferred `st.Close()` + `os.RemoveAll(scratch)`. The originals are never touched beyond ReadFile.

Error wrapping is `fmt.Errorf("vault decrypt probe: %w", err)` per spec. Two deliberate refinements:
- **Missing-input errors name the class themselves** ("vault decrypt probe: store.db not found: …"): `os.ReadFile`'s raw message is platform-dependent ("no such file or directory" vs "cannot find the file specified"), so the brief's cross-platform "not found" assertion needed the probe to say it.
- Verified the leak contract at the source: `store/export.go:150-160` wraps per-credential failures as `decrypt credential %s: %w` — record ID + GCM error class only. The tests pin that the seeded sentinel plaintext never appears in any error.

### `checkVaultOpen` → the `vault-open` doctor row (doctor.go:363), registered in `doctorCheckFuncs`

- Either input missing → INFO `skipped — store.db/master.key not both present` (exact wording per spec; T2 rows own the FAIL/INFO verdict for absence).
- Probe error → FAIL, Detail embeds the probe error (IDs + class only), Fix = `key/ciphertext mismatch — restore from backup (.sme) or re-unlock + import; see docs/backup-restore.md` (exact).
- Success → PASS, Detail = `copy-probe decrypted N servers / M credentials`.
- **Extension beyond the literal brief (documented in code):** also skips INFO when `!ValidMasterKeyLen(keyBytes)`. Rationale discovered during implementation: `crypto.go` derives the DEK via HKDF — **any** master-key length yields a valid 32-byte DEK, so a wrong-length key never errors in `aes.NewCipher`; on an EMPTY vault there is nothing to decrypt and the probe would report a misleading PASS ("decrypted 0/0" under a garbage key). The skip defers to the T2 masterkey row, which FAILs wrong-length keys — same ownership pattern T2 already established. The T2 test's expected count (`0 WARN, 1 FAIL` for the 17-byte-key case) stays green under this skip.
- Skip Detail wording precision: stat-error and unreadable-key cases get their own INFO wording ("not statable" / "unreadable") instead of the false "not both present", since those files exist; the T2 rows FAIL the underlying problem.

### Tests (doctor_test.go)

- `TestProbeVaultDecrypt` — ① seeded real vault (1 server + 1 password credential, `SetCredential`→`AddServer`, export_import_smoke_test.go's minimal pattern) + correct key → counts 1/1, err nil, **both store.db and master.key size+mtime identical** before/after; ② same vault + fresh `GenerateMasterKey` in a second file → err contains "decrypt", never the sentinel plaintext; ③ missing store.db → err contains "not found", never plaintext.
- `TestDoctorVaultOpen` — end-to-end doctor row: healthy vault → `vault-open:  PASS` + `copy-probe decrypted 1 servers / 1 credentials`; **FINDING A simulation** (master.key replaced with a different valid 32-byte key while store/masterkey rows stay PASS — the exact incident signature) → `vault-open:  FAIL` + backup-restore fix, `overall: 0 WARN, 1 FAIL`; missing vault → INFO skip with T2 rows carrying the FAILs.
- New helpers: `seedDoctorVaultWithData` (real vault WITH ciphertext to decrypt — T2's `seedDoctorVault` is empty, and an empty vault cannot exercise the decrypt path at all), `probeTestSecret` sentinel (every error/report string asserted not to contain it), `mustStat`, `doctorScratchCount`, `pinScratchTemp`.

## Scratch-cleanup assertion approach + why

**Chosen: pin TMP/TEMP (Windows) + TMPDIR (Unix) at a private empty `t.TempDir()`, then assert `doctorScratchCount(root) == 0` after each probe call.**

The brief offered three options (temp-dir glob before/after, refactor cleanup into a directly-tested function, count-and-tolerate). I started with the before/after glob of the real `os.TempDir()` and measured it: **~0.9 s per glob on this machine, spiking to ~11 s under antivirus** (the first full run of `TestProbeVaultDecrypt` took 35.9 s — probes themselves are 7 ms each). The baseline-delta approach was also inherently perturbable by unrelated processes writing `sshmgr-doctor-*` (none do today, but the prefix is guessable).

Pinning the temp env solves both at once: the probe's scratch lands in a directory nobody else can write, so "count == 0" is exact (not a delta), and the glob is a listing of ~3 entries. Deterministic without relying on absence of `t.Parallel()`, immune to crashed-run leftovers, and the test went 35.9 s → 0.03 s. `t.Setenv` auto-restores the env. The alternative of returning the scratch path was ruled out because the signature is pinned by the task.

## TDD evidence

1. **RED** (before implementation):
   ```
   internal\cli\doctor_test.go:436:26: undefined: probeVaultDecrypt
   internal\cli\doctor_test.go:468:25: undefined: probeVaultDecrypt
   internal\cli\doctor_test.go:483:15: undefined: probeVaultDecrypt
   FAIL ssh-manager-mcp/internal/cli [build failed]
   ```
2. **GREEN** (targeted): `TestProbeVaultDecrypt` PASS, `TestDoctorVaultOpen` PASS.
3. **Full package**: `go test ./internal/cli/ -count=1` → `ok ssh-manager-mcp/internal/cli 9.938s` (after the final wording refinement; earlier run 8.3 s). All T1/T2 doctor tests still green with the new row in the table — their exact `overall:` counts were re-verified against the new INFO-skip semantics.
4. `gofmt -l internal/cli/` clean; `go vet ./internal/cli/` clean; no new dependencies (only stdlib `path/filepath` added).

## Self-review

- Probe never Opens, Stats-and-writes, or migrates the production paths — only ReadFile; pinned by the mtime/size assertions on BOTH originals.
- Errors carry record IDs + GCM classes only; sentinel-plaintext absence asserted in every error path and in the full doctor report (healthy, FINDING A).
- Doctor output for the row carries counts only on PASS; on FAIL the embedded error is the store's own ID+class wrapping.
- The FINDING A test reproduces the incident exactly: both files present, right sizes, different key — structural rows PASS, only vault-open FAILs.
- Committed on the worktree branch (`worktree-plan-27-doctor`), never master. T2's untracked `sdd/plan-27-task-2-report.md` left alone.

## Concerns

1. **WAL undercount (benign direction), documented in code:** the scratch copy is store.db alone, no `-wal`/`-shm`. A concurrently running broker with un-checkpointed WAL frames yields an older consistent snapshot — worst case an undercounted PASS Detail, never a false FAIL. Copying `-wal` mid-write would risk a torn copy (false FAIL), which is the direction a diagnostic must never err in.
2. **Key double-read TOCTOU:** `checkVaultOpen` reads the key for the length check, then the probe re-reads it. A key swapped between the reads fails closed (probe error → FAIL). Acceptable.
3. **HKDF key-length tolerance** is why the `ValidMasterKeyLen` skip exists (see above). If a future format ever makes key length cryptographically binding, that skip's rationale comment should be revisited.
4. Timing-noise investigation artifacts (temporary `time`-import + `t.Logf` instrumentation) were fully removed before commit; the committed test file is gofmt-clean.

## Report-back summary

Status: DONE. Commit `94465f2` `feat(cli): doctor copy-to-scratch decrypt probe (Plan 27 T3)`. Full `internal/cli` package green (`-count=1`), gofmt/vet clean.
