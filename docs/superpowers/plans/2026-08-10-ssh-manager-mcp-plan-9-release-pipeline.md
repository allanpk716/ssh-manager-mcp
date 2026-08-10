# ssh-manager-mcp Plan 9 — GitHub Release pipeline (GoReleaser, tag-triggered, cross-platform)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A GitHub Actions pipeline that, on a pushed `v*` tag, builds `ssh-manager` for Windows / Linux / macOS (6 targets, Windows-first), attaches SHA256 checksums, and publishes a GitHub Release — so a non-Go user (especially on Windows) can download a ready-to-run binary. `ssh-manager version` prints the release tag it was built from.

**Architecture:** GoReleaser v2.17.1 (config in `.goreleaser.yml`) drives a single `goreleaser-action@v6` step inside `.github/workflows/release.yml` (triggered by `push.tags: ['v*']`). GoReleaser cross-compiles `cmd/ssh-manager` with `CGO_ENABLED=0` (the dep stack is pure-Go — `modernc.org/sqlite`, `go-keyring`/`wincred`), injects the version via ldflags (`-X ssh-manager-mcp/internal/cli.Version={{.Version}}`), archives per-OS (zip for Windows, tar.gz elsewhere), generates `checksums.txt`, and creates the Release with GitHub-native notes. A second lightweight workflow `.github/workflows/goreleaser-check.yml` runs `goreleaser check` + a single-target snapshot build on PRs to catch config drift.

**Tech Stack:** Go 1.24; GoReleaser CLI v2.17.1; `goreleaser/goreleaser-action@v6`; GitHub Actions (`actions/checkout@v4`, `actions/setup-go@v5`); the existing `spf13/cobra` CLI.

## Global Constraints

- **Pure-Go, `CGO_ENABLED=0`.** The dep stack is pure-Go (`go.mod:5-14,17-19`) — every build sets `CGO_ENABLED=0`. No C toolchain needed for any of the 6 cross-compile targets. **§7.5 (run the produced Windows EXE on a real Windows host, Task 6) is the headline proof that static cross-compilation did not silently break `modernc.org/sqlite` + `go-keyring/wincred` — observed, not assumed.**
- **Version pinning (supply chain).** GoReleaser CLI pinned to **`v2.17.1`** (latest stable as of 2026-08; also clears CVE AIKIDO-2026-10332 that affects older v2). `goreleaser-action` pinned to **`@v6`**. `setup-go` pinned to `go-version: '1.24'`. **No `latest` anywhere.**
- **Least-privilege permissions.** `release.yml` declares `permissions: { contents: write }` (workflow-level) — nothing broader. `goreleaser-check.yml` declares `contents: read`. **No `pull_request_target`** (the check workflow uses plain `pull_request` — runs on the PR's own code with the read-only token, avoids the privilege-escalation trap).
- **No new secrets.** Only the built-in `GITHUB_TOKEN`.
- **Go-side hygiene (for the one Go change in T1).** `gofmt -l .` empty; `go vet ./...` clean; `go test ./...` green.
- **`.gitattributes` LF; one logical commit per task; commit messages end `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.**
- **Branch / worktree:** already on `worktree-plan-9-release-pipeline` (base `master` `b1db5be`). All tasks commit here.
- **Design spec:** `docs/superpowers/specs/2026-08-10-github-release-pipeline-design.md` — this plan implements it. **§7 acceptance criteria are the merge gate.**

## Spec coverage map (spec § → task)

- §5.1 version-injection variable → **Task 1**
- §5.2 `.goreleaser.yml` → **Task 2**
- §5.3 `release.yml` → **Task 3**
- §5.4 `goreleaser-check.yml` → **Task 4** (+ §7.7 guard verification)
- §7.1 (green run), §7.2 (6 artifacts), §7.4 (checksums), §7.6 (prerelease marking) → **Task 5**
- §7.3 (version stamp) → Task 1 Step 5 (local) + **Task 6** Step 4 (Windows)
- §7.5 (Windows real-run — headline) → **Task 6**
- §8 T5 (merge + README + first real tag) → **Task 7**

## File Structure

**New:**
- `.goreleaser.yml` — build / archive / checksum / changelog / release config (GoReleaser schema v2).
- `.github/workflows/release.yml` — tag-triggered publish pipeline.
- `.github/workflows/goreleaser-check.yml` — PR/push config-drift guard.
- `internal/cli/root_test.go` — `versionCmd` TDD test for T1.

**Modified:**
- `internal/cli/root.go` — add `var Version = "dev"`; `versionCmd` prints it via `cmd.OutOrStdout()` (was `fmt.Println` to `os.Stdout` — changing it is what makes the command testable, matching the `SetOut` capture pattern already used in `cli_smoke_test.go`).
- `README.md` — one-line "or download a prebuilt binary from Releases" in Quickstart (Task 7).

---

## Task 1: Version-injection variable (`cli.Version` + `versionCmd`) — TDD

**Goal:** `ssh-manager version` prints the `cli.Version` package variable (default `"dev"`, overridable via ldflags at release time). This is the only Go code change in the whole plan; everything else depends on it.

**Files:**
- Modify: `internal/cli/root.go` (the `versionCmd` block at `:18-24`)
- Create: `internal/cli/root_test.go`

**Interfaces:**
- Produces: `var Version = "dev"` in package `cli`; overridable at link time via `-ldflags "-X ssh-manager-mcp/internal/cli.Version=<version>"`. Task 2's `.goreleaser.yml` sets it to `{{.Version}}` (the tag-derived semver).

- [ ] **Step 1: Write the failing test (`internal/cli/root_test.go`)**

```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestVersionCmdPrintsVersionVariable verifies that `ssh-manager version`
// prints the cli.Version package variable — so an ldflags -X override at
// release time is exactly what the user sees. Uses the same SetOut capture
// pattern as cli_smoke_test.go (which is why versionCmd must write to
// cmd.OutOrStdout(), not fmt.Println).
func TestVersionCmdPrintsVersionVariable(t *testing.T) {
	saved := Version
	Version = "9.9.9-test"
	t.Cleanup(func() { Version = saved })

	root := NewRootCmd()
	root.SetArgs([]string{"version"})
	out := &bytes.Buffer{}
	root.SetOut(out)

	if err := root.Execute(); err != nil {
		t.Fatalf("execute version: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "9.9.9-test" {
		t.Fatalf("version output = %q, want %q", got, "9.9.9-test")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestVersionCmdPrintsVersionVariable -v`
Expected: **FAIL / compile error** — `undefined: Version` (the variable does not exist yet), and `versionCmd` still prints the hardcoded `"ssh-manager dev"`.

- [ ] **Step 3: Implement — replace `internal/cli/root.go` with**

```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is the build version. Defaults to "dev" for local `go build` /
// `go install`; overridden at release time via ldflags:
//
//	go build -ldflags "-X ssh-manager-mcp/internal/cli.Version=<version>"
//
// GoReleaser sets this to the tag-derived semver (tag v1.0.0 -> "1.0.0").
var Version = "dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ssh-manager",
		Short: "Encrypted SSH credential vault and broker (MCP)",
	}
	root.AddCommand(versionCmd, newServersCmd(), newProfilesCmd(), newProjectsCmd(), newUnlockCmd(), newLockCmd(), newSSHCmd(), newMCPCmd())
	return root
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), Version)
	},
}
```

(The only changes vs. today: add the `var Version = "dev"` doc+decl; change `fmt.Println("ssh-manager dev")` → `fmt.Fprintln(cmd.OutOrStdout(), Version)`. `NewRootCmd` is unchanged.)

- [ ] **Step 4: Run the test to verify it passes + no regression**

Run: `go test ./internal/cli/ -run TestVersionCmdPrintsVersionVariable -v`
Expected: **PASS**.
Run: `go test ./internal/cli/... -v`
Expected: **all PASS** (the existing `cli_smoke_test.go` etc. unaffected).
Run: `gofmt -l . && go vet ./...`
Expected: empty / clean.

- [ ] **Step 5: Verify ldflags injection end-to-end (the release mechanism, locally)**

Run:
```bash
go build -ldflags "-X ssh-manager-mcp/internal/cli.Version=0.0.0-local" -o /tmp/ssh-manager-ldflags ./cmd/ssh-manager
/tmp/ssh-manager-ldflags version
```
Expected: prints `0.0.0-local` — proves the `-X` wiring GoReleaser will rely on.
Run:
```bash
go build -o /tmp/ssh-manager-plain ./cmd/ssh-manager
/tmp/ssh-manager-plain version
```
Expected: `dev` (the default).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/root.go internal/cli/root_test.go
git commit -m "feat(plan-9): inject build version via ldflags (cli.Version)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `.goreleaser.yml`

**Goal:** The GoReleaser config that builds 6 cross-platform targets, archives them (zip/tar.gz), and emits `checksums.txt`. Validated locally with `goreleaser check` + a full `--snapshot` run before any CI is involved.

**Files:**
- Create: `.goreleaser.yml`

- [ ] **Step 1: Write `.goreleaser.yml`**

```yaml
# GoReleaser config — ssh-manager-mcp (schema v2).
# Pinned toolchain: GoReleaser CLI v2.17.1 (set in .github/workflows/*.yml).
# Docs: https://goreleaser.com
# Local dry-run (no publish):  goreleaser release --snapshot --clean
version: 2

project_name: ssh-manager

builds:
  - id: ssh-manager
    main: ./cmd/ssh-manager
    binary: ssh-manager
    env:
      - CGO_ENABLED=0          # pure-Go dep stack -> static cross-compile
    flags:
      - -trimpath              # reproducible: strip local paths
    ldflags:
      # -s -w: drop symbol/DWARF tables (smaller binary)
      # -X:   inject release version (tag v1.0.0 -> Version=1.0.0)
      - -s -w -X ssh-manager-mcp/internal/cli.Version={{.Version}}
    goos: [windows, linux, darwin]
    goarch: [amd64, arm64]

archives:
  - id: default
    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    formats: [tar.gz]          # v2 plural form (v2.6+)
    format_overrides:
      - goos: windows
        formats: [zip]

checksum:
  name_template: checksums.txt
  algorithm: sha256

changelog:
  use: github-native           # GitHub auto release notes (groups by PR)

release:
  name_template: "ssh-manager {{ .Tag }}"
  prerelease: auto             # v1.0.0-rc1 -> auto-marked Pre-release
  draft: false

gomod:
  proxy: true
  tidy: false

# (No `snapshot:` block — `goreleaser release --snapshot --clean` enables
#  snapshot mode via the flag and uses GoReleaser's default version template.)
```

- [ ] **Step 2: Install the pinned GoReleaser CLI (if not already on PATH)**

Run: `go install github.com/goreleaser/goreleaser/v2@v2.17.1`
Verify: `goreleaser --version`
Expected: reports `Version: 2.17.1` (± commit metadata).

- [ ] **Step 3: Validate the config**

Run: `goreleaser check`
Expected: `config is valid` (exit 0). If it flags a field, fix per the message — v2 uses `formats` (plural, list), not `format`.

- [ ] **Step 4: Single-target snapshot build (fast — proves the matrix compiles)**

Run: `goreleaser build --snapshot --single-target --clean`
Expected: exit 0; one binary written under `dist/` (the host target).

- [ ] **Step 5: Full local snapshot release (no publish — proves the whole pipeline)**

Run: `goreleaser release --snapshot --clean`
Expected: exit 0; `dist/` contains 6 archives + `checksums.txt`. (Snapshot mode skips the GitHub publish — no token needed.)
Verify:
```bash
ls dist/
```
Expected files: `ssh-manager_*_windows_amd64.zip`, `_windows_arm64.zip`, `_linux_amd64.tar.gz`, `_linux_arm64.tar.gz`, `_darwin_amd64.tar.gz`, `_darwin_arm64.tar.gz`, plus `checksums.txt`.

- [ ] **Step 6: Verify the version stamp survived a real GoReleaser build**

(The Windows `.exe` can't execute on Linux — verify the Linux archive's binary instead; it carries the same ldflags path.)
```bash
tar -xzf dist/ssh-manager_*_linux_amd64.tar.gz -C /tmp/lcheck
/tmp/lcheck/ssh-manager version
```
Expected: a SNAPSHOT version string (e.g. `0.0.0-next-SNAPSHOT-<shorthash>` — GoReleaser's default snapshot template), **not** `dev`. This proves `{{.Version}}` ldflags injection works end-to-end through GoReleaser. (Executing the Windows `.exe` itself is §7.5 / Task 6.)

- [ ] **Step 7: Clean `dist/` + commit**

```bash
rm -rf dist/                    # gitignored (.gitignore:6), but stay tidy
git add .goreleaser.yml
git commit -m "feat(plan-9): add GoReleaser config (6 cross-platform targets, checksums)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: `release.yml` — tag-triggered publish pipeline

**Goal:** The workflow that fires on a `v*` tag and runs GoReleaser to build + publish the Release.

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Step 1: Write `.github/workflows/release.yml`**

```yaml
# Release pipeline — on a pushed v* tag, builds ssh-manager for
# Windows/Linux/macOS (6 targets) and publishes a GitHub Release with
# SHA256 checksums. See docs/superpowers/specs/2026-08-10-github-release-pipeline-design.md.
name: release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write   # goreleaser creates the Release + uploads assets; nothing broader

concurrency:
  group: release-${{ github.ref }}
  cancel-in-progress: false

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # full history — changelog needs it

      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
          cache: true

      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          # Pinned GoReleaser CLI version (supply-chain hygiene; v2.17.1 is
          # latest stable 2026-08 and clears CVE AIKIDO-2026-10332). NOT 'latest'.
          version: v2.17.1
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 2: (optional, if available) lint the workflow syntax**

Run: `actionlint .github/workflows/release.yml || true`
(not a gate — the authoritative validation is the real Actions run in Task 5.)

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "feat(plan-9): tag-triggered release workflow (goreleaser-action v6, pinned v2.17.1)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: `goreleaser-check.yml` — config-drift guard + §7.7

**Goal:** A fast PR/push workflow that runs `goreleaser check` + a single-target snapshot build, so a change that breaks the release config fails CI before merge.

**Files:**
- Create: `.github/workflows/goreleaser-check.yml`

- [ ] **Step 1: Write `.github/workflows/goreleaser-check.yml`**

```yaml
# Config-drift guard — on PRs/pushes touching the release config or the CLI
# entry, validate .goreleaser.yml and prove one target still compiles.
# Does NOT publish.
name: goreleaser-check

on:
  push:
    branches: [master]
    paths:
      - '.goreleaser.yml'
      - 'cmd/**'
      - '.github/workflows/goreleaser-check.yml'
  pull_request:
    paths:
      - '.goreleaser.yml'
      - 'cmd/**'
      - '.github/workflows/goreleaser-check.yml'

permissions:
  contents: read

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
          cache: true

      - name: goreleaser check
        uses: goreleaser/goreleaser-action@v6
        with:
          version: v2.17.1
          args: check

      - name: single-target snapshot build
        uses: goreleaser/goreleaser-action@v6
        with:
          version: v2.17.1
          args: build --snapshot --single-target --clean
```

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/goreleaser-check.yml
git commit -m "feat(plan-9): goreleaser-check workflow (config-drift guard on PRs)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 3 (§7.7 — verify the guard actually catches a broken config).** This is best done as part of Task 5's branch push. After the branch is pushed (Task 5 Step 1), open a throwaway PR with a deliberately broken `.goreleaser.yml` (e.g. rename `builds:` → `build:`), confirm the `goreleaser-check` job fails on it, then close the PR without merging. (If skipped, §7.7 is still satisfied *by construction* — `goreleaser check` non-zero exits on invalid config — but observing the failure is the rigorous bar.)

---

## Task 5: End-to-end validation with a test tag (operator step — needs GitHub)

**Goal:** Push the branch + a `-rc1` test tag and validate the real GitHub Actions run against §7.1 (green), §7.2 (6 artifacts), §7.4 (checksums), §7.6 (pre-release). §7.3 version-stamp and §7.5 Windows run are Task 6.

> **Note:** This task needs write access to `origin` (or a personal fork). All prior tasks (T1–T4) are committed on `worktree-plan-9-release-pipeline` before starting.

- [ ] **Step 1: Push the worktree branch to origin**

```bash
git push -u origin worktree-plan-9-release-pipeline
```
(If you lack write access to the upstream repo, push to your fork and use that remote instead.)

- [ ] **Step 2: Create + push a test release tag (RC, so it auto-marks pre-release — covers §7.6)**

```bash
git tag v0.0.0-rc1
git push origin v0.0.0-rc1
```

- [ ] **Step 3: Watch the Actions run**

Open `https://github.com/<OWNER>/ssh-manager-mcp/actions` → the **`release`** workflow triggered by tag `v0.0.0-rc1`. Wait for green.
Expected: the `release` job succeeds (§7.1).

- [ ] **Step 4: Validate the Release page (§7.2, §7.4, §7.6)**

Open the `v0.0.0-rc1` Release. Confirm:
- **6 archives** present: `ssh-manager_0.0.0-rc1_windows_amd64.zip`, `_windows_arm64.zip`, `_linux_amd64.tar.gz`, `_linux_arm64.tar.gz`, `_darwin_amd64.tar.gz`, `_darwin_arm64.tar.gz` (§7.2).
- **`checksums.txt`** attached (§7.4).
- Release marked **Pre-release** (§7.6 — because the tag matches a pre-release semver).
- GitHub-native release notes present.

- [ ] **Step 5: Verify a checksum locally (§7.4)**

```bash
curl -LO https://github.com/<OWNER>/ssh-manager-mcp/releases/download/v0.0.0-rc1/checksums.txt
curl -LO https://github.com/<OWNER>/ssh-manager-mcp/releases/download/v0.0.0-rc1/ssh-manager_0.0.0-rc1_linux_amd64.tar.gz
sha256sum -c --ignore-missing checksums.txt
```
Expected: `ssh-manager_0.0.0-rc1_linux_amd64.tar.gz: OK` (exit 0).

- [ ] **Step 6: If anything failed — debug loop**

- Workflow didn't trigger → confirm the tag matches `v*` and `release.yml` is on the tagged commit.
- GoReleaser error → read the Actions log; reproduce locally with `goreleaser check` / `goreleaser release --snapshot --clean`.
- Wrong artifact count → check `goos`/`goarch` matrices in `.goreleaser.yml`.

Iterate by deleting the tag, fixing, committing, re-tagging:
```bash
git push origin :refs/tags/v0.0.0-rc1 && git tag -d v0.0.0-rc1
# ... fix + commit ...
git tag v0.0.0-rc1 && git push origin v0.0.0-rc1
```

- [ ] **Step 7: Leave the RC release up** — Task 6 consumes its Windows artifact. **Do not merge to master or tag `v1.0.0` yet** — that's Task 7, gated on Task 6.

---

## Task 6: Windows real-run validation (§7.5 — the headline acceptance gate)

**Goal:** On a real Windows host, run the cross-compiled `ssh-manager.exe` through the three operations that exercise the native-bridge deps — vault unlock (`go-keyring`/`wincred` → Windows Credential Manager), server persistence (`modernc.org/sqlite`), and MCP server boot (`go-sdk` + broker init). This is the proof that `CGO_ENABLED=0` cross-compilation did not silently break those deps. **Must be observed, not assumed. This is the merge gate.**

> Run on a Windows 10/11 machine. PowerShell commands shown; adapt for cmd if needed. `<OWNER>` = the real GitHub owner/org.

- [ ] **Step 1: Fetch the Windows artifact + checksums**

```powershell
curl -LO https://github.com/<OWNER>/ssh-manager-mcp/releases/download/v0.0.0-rc1/checksums.txt
curl -LO https://github.com/<OWNER>/ssh-manager-mcp/releases/download/v0.0.0-rc1/ssh-manager_0.0.0-rc1_windows_amd64.zip
```

- [ ] **Step 2: Verify integrity (§7.4 full)**

```powershell
Get-FileHash ssh-manager_0.0.0-rc1_windows_amd64.zip -Algorithm SHA256
# compare to the matching line in checksums.txt
```
Expected: hash matches the `ssh-manager_0.0.0-rc1_windows_amd64.zip` line in `checksums.txt`.

- [ ] **Step 3: Unzip**

```powershell
Expand-Archive ssh-manager_0.0.0-rc1_windows_amd64.zip -DestinationPath .
```

- [ ] **Step 4: Version stamp (§7.3)**

```powershell
.\ssh-manager.exe version
```
Expected: `0.0.0-rc1` (**not** `dev`) — proves the `-X ssh-manager-mcp/internal/cli.Version` ldflags injection survived cross-compilation.

- [ ] **Step 5: Vault unlock → proves `go-keyring`/`wincred` (Windows Credential Manager)**

```powershell
.\ssh-manager.exe unlock
```
Expected: completes without error; the master key is stored in Windows Credential Manager. Verify: open **Credential Manager → Windows Credentials** → an `ssh-manager-mcp` entry appears. (On a normal desktop session it should NOT fall back to the passphrase prompt.)

- [ ] **Step 6: Add a server → proves `modernc.org/sqlite` under CGO=0**

```powershell
.\ssh-manager.exe servers add --name gpu --host 192.168.1.10 --user deploy --password dummy
.\ssh-manager.exe servers ls
```
Expected: `servers add` succeeds (the store DB file was created — `modernc.org/sqlite` opened/initialized under pure-Go); `servers ls` lists `gpu`.

- [ ] **Step 7: Create a profile/project → token (still `modernc.org/sqlite` + token gen)**

```powershell
.\ssh-manager.exe profiles add team-a
.\ssh-manager.exe profiles grant team-a gpu
.\ssh-manager.exe projects add my-agent --profile team-a
```
Expected: `projects add` prints a one-time token + the `.mcp.json` snippet (per README Quickstart). Capture the token.

- [ ] **Step 8: MCP server boots on stdio → proves `go-sdk` + broker initialize**

```powershell
$env:SSHMGR_TOKEN = "<token-from-step-7>"
'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' | .\ssh-manager.exe mcp --token $env:SSHMGR_TOKEN
```
Expected: a JSON-RPC `initialize` **response** is printed (the server booted + spoke MCP over stdio). It will then block waiting on stdin — Ctrl-C to exit. (Any panic / crash / silent exit here fails §7.5.)

- [ ] **Step 9: Record the result**

If Steps 4–8 all green → **§7.5 PASSED**. Note any anomaly (e.g. keyring passphrase-fallback behavior on a locked session) in the PR description. Do not proceed to Task 7 until §7.5 is observed green.

- [ ] **Step 10 (cleanup — optional): delete the test release + tag**

```bash
gh release delete v0.0.0-rc1 --yes
git push origin :refs/tags/v0.0.0-rc1
git tag -d v0.0.0-rc1
```
(Skip if you'd rather keep the RC visible.)

---

## Task 7: Merge to master + README line + first real release (§8 T5)

**Goal:** Land the pipeline on `master`, point the README at prebuilt binaries, and cut the first stable `v1.0.0`.

- [ ] **Step 1: Add the README Quickstart line**

In `README.md`, find the existing Quickstart build line (around `README.md:42-43`):

```
# 1. Build
go build -o ssh-manager ./cmd/ssh-manager        # or: go install ./cmd/ssh-manager
```

Replace with (substitute `<OWNER>`):

```
# 1. Build — or skip this step: grab a prebuilt binary from Releases
#    https://github.com/<OWNER>/ssh-manager-mcp/releases
go build -o ssh-manager ./cmd/ssh-manager        # or: go install ./cmd/ssh-manager
```

- [ ] **Step 2: Commit README**

```bash
git add README.md
git commit -m "docs(plan-9): point Quickstart at prebuilt Releases binaries" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 3: Push branch + open PR + merge**

```bash
git push origin worktree-plan-9-release-pipeline
gh pr create --base master \
  --title "Plan 9: GitHub Release pipeline (GoReleaser, tag-triggered)" \
  --body "Implements docs/superpowers/specs/2026-08-10-github-release-pipeline-design.md. Acceptance §7.1–§7.6 verified (test tag v0.0.0-rc1 + real Windows run)."
```
Review, then merge **`--no-ff`** to `master` (project convention).

- [ ] **Step 4: First real release tag from master**

(From the main checkout — leave the worktree via `ExitWorktree` action `keep`, or run in a fresh clone.)
```bash
git checkout master
git pull
git tag v1.0.0
git push origin v1.0.0
```
Expected: `release.yml` fires; a `v1.0.0` Release appears with 6 archives + `checksums.txt`, **not** marked pre-release.

- [ ] **Step 5: Final verification of the stable release**

- Actions run green.
- `ssh-manager_1.0.0_windows_amd64.zip` present.
- `checksums.txt` present.
- Release **not** marked pre-release.
- Release notes present (GitHub-native).

- [ ] **Step 6: Update project memory**

Update the project-status line (the "Plans 1–N delivered" note) to record Plan 9 delivered; note the new operator SOP (`git tag vX.Y.Z && git push origin vX.Y.Z` → Release).

---

## Self-review notes (plan vs spec)

- Every spec section maps to a task (see coverage map).
- No `latest` anywhere — GoReleaser CLI `v2.17.1`, action `@v6`, Go `1.24`, all pinned (Global Constraints).
- The headline risk (cross-compile breaking native-bridge deps) is Task 6's entire purpose — observed on real Windows, not assumed.
- `<OWNER>` is a stand-in for the real GitHub owner/org, substituted in Tasks 5/6/7 — it is the only external unknown in the plan.
