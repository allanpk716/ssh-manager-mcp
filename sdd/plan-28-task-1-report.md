# Plan 28 Task 1 Report — internal/secrethint 包 + 语料回归

**Status: DONE** — commit `7e108bc` on branch `worktree-plan-28-secrethint` (3 files, +332 lines).

## Corpus provenance

- Source: `C:\WorkSpace\agent\ssh-manager-mcp\.xcheck\20260816-195610\exp\heuristic-corpus.txt` (gitignored, exists).
- Copied byte-for-byte with `cp`, verified identical with `cmp` (silent exit 0) into
  `internal/secrethint/testdata/corpus-legal.txt` (committed — regression lives in the repo).
- **35 non-empty lines** (verified: `awk 'NF' | wc -l` = 35).
- **sha256 pin line present**: `sha256:4DWyXkGY7GpVn/18m8/2n2J1k8WZe6VYv0yVqN0vX7Y` (grep count = 1) — the one
  category that nearly false-positived in the xcheck experiment; the test asserts its presence so future
  corpus edits cannot silently drop it.
- `.gitignore` checked: no rule touches `testdata/*.txt` (`*.pem`/`*.key`/`.xcheck/` do not apply).

## TDD evidence

- **Step 1 (tests first)**: `secrethint_test.go` written before any real implementation. Five tests per the
  brief's list:
  - `TestScanValueTruePositives` — 11 subtests: multi-line OPENSSH PEM, RSA PEM, `sk-ant-api03-…` long
    string, `ghp_` 40-char, `gho_`, `github_pat_`, `AKIA`+16 (`AKIAIOSFODNN7EXAMPLE`), `xoxb-`, `xoxp-`,
    `eyJ` JWT header, and a prefix **embedded in ordinary prose** (surrounded by normal text).
  - `TestScanValueLegalCorpus` — reads testdata line by line, `ScanValue("description", line)`, asserts 0
    findings on every non-empty line; asserts ≥30 lines and sha256-pin presence.
  - `TestScanValuePublicCertNotFlagged` — 4 subtests: public CERTIFICATE, PUBLIC KEY, prose "private key"
    without marker, `-----BEGIN` marker without PRIVATE KEY label (pins the AND-co-occurrence from both
    directions).
  - `TestFormatWarningNoContentEcho` — sentinel non-echo assertion + exact golden one-line output + no
    trailing newline.
  - `TestScanServerAggregates` — tags (`prefix:ghp_`) + caveats (`pem-private-key`) → exactly 2 Findings in
    fixed order tags→…→caveats; all-empty → 0 findings. Non-adjacent fields chosen to pin the order hard.
- **Step 2 (RED)**: stub returning nil/"" → `go test ./internal/secrethint/ -count=1` FAILED with all 11
  true-positive subtests, FormatWarning golden, and ScanServer aggregation red (the two negative tests
  passed trivially against the nil stub, as expected — they are regression nails for the real code).
- **Step 3/4 (implement → GREEN)**: `ok ssh-manager-mcp/internal/secrethint` — all tests pass, `-count=1`.
- Zero dependencies: imports are `strings` only in the implementation; `os`/`strings`/`testing` in tests.
- `gofmt -l internal/secrethint` → clean; `go vet ./internal/secrethint/` → clean;
  `go build ./...` (whole module) → clean.

## Exact API landed (T2/T3 contract)

```go
package secrethint

type Finding struct {
    Field string // "description" / "tags" / …
    Rule  string // "pem-private-key" / "prefix:sk-" / …
}

func ScanValue(field, value string) []Finding
func ScanServer(tags, description, location, hardware, services, role, caveats string) []Finding
func FormatWarning(f Finding) string
```

- `ScanValue` returns one Finding per matched rule — PEM first, then prefixes in table order; nil when
  clean. Deterministic order.
- `ScanServer` field order fixed: tags, description, location, hardware, services, role, caveats.
- `FormatWarning` output (single line, no trailing newline, templated off Finding only, never any field
  value):
  `warning: server metadata may contain a secret — field '<Field>' matched rule '<Rule>' (content not shown; this text would be sent to LLM providers on every list_servers — edit the server to fix, or ignore if intentional)`

## Rule set (binding, from xcheck experiment 3 / rev2 adjudication)

- Prefixes (exact closed set, plain case-sensitive substring): `sk-`, `ghp_`, `gho_`, `github_pat_`,
  `AKIA`, `xoxb-`, `xoxp-`, `eyJ`. Rule names `prefix:sk-` etc.
- PEM rule `pem-private-key`: `strings.Contains(value, "-----BEGIN")` AND
  `strings.Contains(value, "PRIVATE KEY")` — both, case-sensitive, same value.
- **High-entropy rule does not exist.** Grep for "entropy" (case-insensitive) in the package returns
  exactly one hit: the package-doc sentence explaining its deliberate absence with the 2.9% FP
  measurement. No entropy logic, no Shannon computation, no length heuristics.

## Self-review

- API signatures match the brief character-for-character (field order in `ScanServer` params, struct
  field comments included).
- Rule names verified: `pem-private-key`, `prefix:<exact-prefix>`.
- Prefix matching is plain `strings.Contains` — deliberately not word-boundary anchored; the embedded-
  prefix test proves prose-embedded tokens are caught. This is the measured 0-FP behavior; do not
  "tighten" without re-running the corpus regression (noted in a code comment).
- Test PEM bodies were crafted to avoid accidental prefix collisions (e.g. no `AKIA`/`eyJ` inside the
  base64), so the aggregation test's exact-equality assertion is stable.
- `ASIA` (AWS session keys) intentionally NOT in the set — it was not in the measured prefix set; noted
  in a comment on the table.

## Concerns

1. **`eyJ` is broad by design** (3-char substring). It measured 0 FP on the legal corpus, but any base64
   blob or prose containing the literal `eyJ` will warn. That is a warning, not a block — acceptable
   false-positive surface, but T2/T3 copy should present it as advisory.
2. **Single-corpus regression**: the 35-line corpus is the only FP regression net. New prefixes (or
   boundary logic) require extending the corpus first. The comment on `prefixes` says so.
3. T2/T3 must add the trailing newline themselves when printing `FormatWarning` output — pinned by test.
4. The word "entropy" does appear once in a doc comment (coordinator-approved). If plan acceptance
   criterion 3's grep is a naive must-return-zero grep rather than no-implementation check, that one
   comment hit will surface — it is a comment explaining absence, not logic.
