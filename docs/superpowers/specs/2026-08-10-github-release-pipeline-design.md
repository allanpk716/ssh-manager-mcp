# Plan 9 Design: GitHub Release pipeline (GoReleaser, tag-triggered, cross-platform)

**Date:** 2026-08-10
**Status:** Design (brainstormed; pending implementation plan)
**Scope:** A GitHub Actions release pipeline that builds `ssh-manager` for Windows / Linux / macOS and publishes the binaries to a GitHub Release, driven by GoReleaser and triggered by a `v*` tag.
**Nature:** CI/CD only — one small code change (a version-injection variable) + three config files. No broker / MCP logic change. Verified by triggering a real release tag and running the produced Windows binary end-to-end on a Windows host.

---

## 1. Background

`ssh-manager-mcp` ships today only as source: the README Quickstart instructs users to `go build -o ssh-manager ./cmd/ssh-manager` themselves (`README.md:43`). There is no published binary; a Windows user without a Go toolchain cannot use the project. The README already commits to "Cross-platform (Windows / Linux / macOS)" (`README.md:7`) — but nothing produces cross-platform binaries today.

The repo has exactly one workflow (`.github/workflows/eval-nightly.yml`) — an on-demand eval/conformance harness; it does not build or publish artifacts. There is **no** `.goreleaser.yml`, no `Makefile`, no build script (verified — a glob for all three returned nothing).

## 2. Goals

1. **G1 — One-command release.** Pushing a SemVer tag (`v1.0.0`, `v1.0.0-rc1`) builds and publishes a GitHub Release with cross-platform binaries, checksums, and release notes — no manual steps beyond the tag.
2. **G2 — Windows-first cross-platform binaries.** Six archives — `windows/{amd64,arm64}`, `linux/{amd64,arm64}`, `darwin/{amd64,arm64}` — with `windows_amd64` as the headline artifact. A non-Go user on Windows can download, unzip, and run.
3. **G3 — Verifiable version stamp.** `ssh-manager version` prints the release tag the binary was built from (not `"dev"`), via ldflags injection into a package variable.
4. **G4 — Integrity + minimal trust surface.** Every release ships SHA256 checksums; the workflow runs with least privilege (`contents: write` only) and pins the GoReleaser toolchain.

## 3. Non-goals (deferred)

- **Code signing** (GPG / sigstore / cosign) — needs key infrastructure; SHA256 checksums are the v1 integrity floor.
- **Scoop manifest / Homebrew tap** — YAGNI until a user asks.
- **README "latest version" auto-badge** — manual for v1.
- **A pre-release "dev snapshot" channel on every push to `master`** — tag-triggered releases are sufficient; `goreleaser --snapshot` covers local dry-runs.
- **SBOM generation** — deferred.
- **Multi-binary** — `cmd/` has one entry today; re-evaluate if a second appears.

## 4. Confirmed current state (verified against source)

- **Module / Go:** `module ssh-manager-mcp`, `go 1.24.11` (`go.mod:1,3`).
- **Entry point:** `cmd/ssh-manager/main.go` → `cli.NewRootCmd().Execute()` (`main.go:11`).
- **`version` is hardcoded.** `internal/cli/root.go:18-24` defines `versionCmd`; line `:22` prints the literal `"ssh-manager dev"`. There is **no** package-level `Version` variable to receive an ldflags `-X` value — this must be added (§5.1).
- **Dependency stack is pure-Go → `CGO_ENABLED=0` is safe.** `modernc.org/sqlite v1.33.1` (pure-Go SQLite, **not** `mattn/go-sqlite3`), `zalando/go-keyring v0.2.8` (pure-Go OS-keychain wrapper), `danieljoos/wincred v1.2.3` (indirect; Windows Credential Manager, pure-Go), `godbus/dbus/v5` (indirect; Linux Secret Service, pure-Go), `golang.org/x/crypto` (SSH), `modelcontextprotocol/go-sdk v1.2.0`, `spf13/cobra v1.10.2` (`go.mod:5-14,17-19`). No C anywhere → static cross-compilation across all six targets needs no system C toolchain. **This is the single biggest risk of cross-compilation and is therefore the headline acceptance test (§7.5).**
- **`.gitignore` already excludes build output:** `*.exe` (`:1`), `dist/` (`:6`), `build/` (`:7`) — `dist/` is GoReleaser's default output dir, so nothing more is needed.
- **Existing CI conventions** (matched, not reinvented): `actions/checkout@v4`, `actions/setup-go@v5` with `go-version: '1.24'` + `cache: true`, explicit top-level `permissions:` (`eval-nightly.yml:18,35-40`).
- **README cross-platform claim** is a forward promise today (`README.md:7`); this plan is what makes it real.

## 5. Design

### 5.1 Version-injection variable (the only code change)

`internal/cli/root.go`:

```go
// Version is the build version. Overridden at release time via ldflags:
//   -ldflags "-X ssh-manager-mcp/internal/cli.Version=<version>"
// Defaults to "dev" for local `go build` / `go install`.
var Version = "dev"

var versionCmd = &cobra.Command{
    Use:   "version",
    Short: "Print build version",
    Run: func(cmd *cobra.Command, args []string) {
        fmt.Println(Version)
    },
}
```

`main.go` is unchanged. The GoReleaser ldflags `-X` target is `ssh-manager-mcp/internal/cli.Version={{.Version}}`.

### 5.2 `.goreleaser.yml` (new)

Schema v2. Key decisions:

- `version: 2`; `project_name: ssh-manager`.
- **builds:** `main: ./cmd/ssh-manager`, `binary: ssh-manager`, `env: [CGO_ENABLED=0]`, `flags: [-trimpath]`, `goos: [windows, linux, darwin]`, `goarch: [amd64, arm64]` (6 targets).
- **ldflags** (strip symbols + version stamp):
  ```yaml
  ldflags:
    - -s -w -X ssh-manager-mcp/internal/cli.Version={{.Version}}
  ```
  `-trimpath` strips local paths (reproducible build); `-s -w` drops the symbol / DWARF tables (smaller binary). `{{.Version}}` is the SemVer tag stripped of the `v` prefix (tag `v1.0.0` → `1.0.0`).
- **archives:** `name_template: ssh-manager_{{.Version}}_{{.Os}}_{{.Arch}}`; `format_overrides` → `windows: zip`, else `tar.gz`.
- **checksum:** `algorithm: sha256`, `name_template: checksums.txt`.
- **changelog:** `use: github-native` (GitHub's auto-generated release notes — more accurate than commit scraping, groups by PR).
- **release:** `prerelease: auto` (tags matching a pre-release SemVer are auto-marked, e.g. `v1.0.0-rc1`); `draft: false`; `name_template: "ssh-manager {{.Tag}}"`.
- **snapshot:** enabled (default template) — lets `goreleaser release --snapshot --clean` build + archive locally **without** a tag and **without** publishing. GoReleaser's default snapshot version template (derived from the latest tag + short commit) is sufficient; no custom `version_template` is needed (a snapshot run has no tag, so `{{.Tag}}` would be empty).
- **gomod:** `proxy: true`, `tidy: false`.

### 5.3 `.github/workflows/release.yml` (new — the publish pipeline)

- **Trigger:** `on.push.tags: ['v*']` — only a SemVer tag starts a release.
- **Permissions:** `contents: write` (explicit, least-privilege — needed to create the Release and upload assets; no `packages`, no `id-token`).
- **Single job `release`** on `ubuntu-latest`:
  1. `actions/checkout@v4` with `fetch-depth: 0` (full history — `changelog` needs it).
  2. `actions/setup-go@v5` — `go-version: '1.24'`, `cache: true` (matches existing CI).
  3. `goreleaser/goreleaser-action@v6` — `version: <pinned>` (pin to the latest stable GoReleaser at implementation time, **not** `latest` — supply-chain hygiene; exact version recorded in the plan), `args: release --clean`, `env.GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}`.

No separate upload step — GoReleaser creates the Release and uploads all assets in one invocation.

### 5.4 `.github/workflows/goreleaser-check.yml` (new — config-drift guard)

- **Trigger:** `on.push` and `on.pull_request` for `paths: ['.goreleaser.yml', 'cmd/**', '.github/workflows/goreleaser-check.yml']`.
- **Permissions:** `contents: read`.
- **Job:** `goreleaser check` (validates the YAML) + `goreleaser build --snapshot --single-target --clean` (builds one target — proves the cross-compile matrix actually compiles, without publishing or building all six). Fast feedback that a change didn't break the release config.

### 5.5 Release flow (operator SOP)

```bash
git tag v1.0.0
git push origin v1.0.0
# → Actions runs release.yml → Release page gains 6 archives + checksums.txt + release notes
```

Pre-release: `git tag v1.0.0-rc1` → same flow, Release auto-marked "Pre-release".

## 6. Security / supply-chain considerations

1. **Least privilege.** `permissions: contents: write` at workflow level — the default broad `GITHUB_TOKEN` is not used. No other scopes.
2. **Integrity floor.** `checksums.txt` (SHA256) attached to every release; any user can `sha256sum -c checksums.txt --ignore-missing` to verify a download.
3. **Pinned toolchain.** `goreleaser-action` and the GoReleaser binary version are pinned (not `latest`) — a moving tag could ship a tampered binary; a pin makes the build reproducible and auditable. `setup-go` is version-pinned to `1.24` already.
4. **No extra secrets.** Only the built-in `GITHUB_TOKEN`; no new repository secrets to manage or leak.
5. **No `pull_request_target`.** The check workflow uses plain `pull_request` (runs on the PR's own code with the read-only token) — avoids the standard privilege-escalation trap.
6. **Reproducibility.** `-trimpath` + pinned Go + tag-locked source → any third party rebuilding the same tag gets a bit-identical binary modulo the Go toolchain build.

## 7. Acceptance criteria (measurable — must pass before merge)

1. **Green run.** A test tag (e.g. `v0.0.0-rc1`) triggers `release.yml` and the job succeeds.
2. **Six artifacts.** The Release lists: `ssh-manager_*_windows_amd64.zip`, `_windows_arm64.zip`, `_linux_amd64.tar.gz`, `_linux_arm64.tar.gz`, `_darwin_amd64.tar.gz`, `_darwin_arm64.tar.gz` + `checksums.txt` + GitHub-native notes.
3. **Version stamp injected.** Unzip `windows_amd64` on a Windows host; `.\ssh-manager.exe version` prints `0.0.0-rc1` (not `dev`) — proves the `-X` ldflags wiring (§5.1) is correct.
4. **Checksums verify.** `sha256sum -c checksums.txt --ignore-missing` passes for the downloaded archive(s).
5. **Headline cross-compile proof — run on real Windows.** On a Windows machine: `ssh-manager.exe unlock` initializes the vault (writes to Windows Credential Manager via `go-keyring`/`wincred`), `ssh-manager.exe servers add …` persists (opens `modernc.org/sqlite`), and `ssh-manager.exe mcp --token <t>` starts the MCP server over stdio. This is the **critical** check: it proves `CGO_ENABLED=0` cross-compilation did not silently break the two native-bridge deps (keychain + SQLite) — the largest risk of static cross-compilation, which must be **observed, not assumed**.
6. **Pre-release marking.** A `-rc` tag produces a Release auto-flagged "Pre-release" (not a stable release).
7. **Check workflow guards.** A PR that breaks `.goreleaser.yml` fails `goreleaser-check.yml`.

## 8. Implementation task split (high level — the plan doc details)

- **T1 — version variable.** Add `var Version = "dev"` to `internal/cli/root.go`; wire `versionCmd` to print it. Verify locally: `go build -o ssh-manager ./cmd/ssh-manager && ./ssh-manager version` → `dev`; then with `-ldflags "-X ssh-manager-mcp/internal/cli.Version=test"` → `test`.
- **T2 — `.goreleaser.yml`.** Author per §5.2. Local dry-run: `goreleaser release --snapshot --clean` → `dist/` contains 6 archives + `checksums.txt`; the `ssh-manager.exe` from the snapshot prints its version.
- **T3 — workflows.** Author `release.yml` (§5.3) and `goreleaser-check.yml` (§5.4). Pin the GoReleaser version after checking the latest stable at implementation time.
- **T4 — end-to-end validation.** Push `v0.0.0-rc1` on a non-`master` branch (or a personal fork) → confirm acceptance §7.1–§7.4 + §7.6. Then run §7.5 on a real Windows host. Delete the test release/tag once green.
- **T5 — merge + first real tag + README line.** Merge to `master`; tag the first real release (`v1.0.0`) from `master`; confirm the Release. Add a one-line "or download a prebuilt binary from Releases" line to the README Quickstart (small enough to include in this plan).

## 9. Pointers

- Entry: `cmd/ssh-manager/main.go`; `versionCmd`: `internal/cli/root.go:18-24` (hardcoded literal at `:22`).
- Module/Go: `go.mod:1,3`.
- Pure-Go deps (the CGO-free proof + the cross-compile risk): `go.mod:5-14` (direct) + `:17-19` (the keyring native bridges, indirect).
- `.gitignore` already covers `dist/`: `.gitignore:6`.
- Existing CI conventions (matched): `.github/workflows/eval-nightly.yml:18,35-40`.
- README cross-platform claim (this plan fulfils it): `README.md:7`; Quickstart (extended in T5): `README.md:37-64`.
- GoReleaser docs: https://goreleaser.com (schema v2, customization, ldflags).
