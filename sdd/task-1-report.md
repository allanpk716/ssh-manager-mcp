# Task 1 Report: CI Baseline + Release Gate

## Status
DONE

## Commit Hash
7f5abdc

## Changes Made

### Files Created
1. `.github/workflows/ci.yml` (50 lines)
   - Reusable test workflow triggered on push/PR/workflow_call
   - Dual-lane matrix: ubuntu-latest + windows-latest
   - fail-fast: false (both lanes must pass)
   - Concurrency group with cancel-in-progress
   - Test steps: build, vet, gofmt, test
   - Uses go-version-file for Go version consistency

### Files Modified
1. `.github/workflows/release.yml` (22 insertions, 1 deletion)
   - Added `test` job that reuses ci.yml via workflow_call
   - Changed `release` job to depend on `test` (needs: test)
   - Changed from hardcoded `go-version: '1.25'` to `go-version-file: go.mod`
   - Release now only runs after the same dual-lane test matrix passes

## Verification Steps Performed

### Step 3: YAML Syntax Validation
```bash
python -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml', encoding='utf-8')); yaml.safe_load(open('.github/workflows/release.yml', encoding='utf-8')); print('yaml ok')"
```
**Result:** `yaml ok`

### Step 4: Local CI Command Execution
```bash
go build ./...
go vet ./...
gofmt -l .
go test ./... -count=1
```

**Results:**
- `go build ./...`: Success (no output)
- `go vet ./...`: Success (no output)
- `gofmt -l .`: Success (no output = no formatting needed)
- `go test ./... -count=1`: All tests passed (15 packages, ~35s total)

Test packages validated:
- cli, clientops, conformance, eval, importer, mcpserver, paths, roles, sshbroker, store, testsshd, tui, vault, vaultio

### Step 5: Commit
```bash
git add .github/workflows/ci.yml .github/workflows/release.yml
git commit -m "ci: dual-lane test baseline (ubuntu+windows) + release gate reuses it via workflow_call"
```
**Result:** Commit created (2 files changed, 58 insertions(+), 1 deletion(-))

## Self-Check Findings

### Consistency with Brief
- ci.yml: Exact match with specification (50 lines verbatim)
- release.yml: Jobs section replaced exactly as specified
- No extra changes or deviations detected

### Key Design Points
1. **Windows lane is load-bearing**: 4 test files with `//go:build windows` (store/acl, store/dpapi, store/masterkey, tui/istty) will now run on every push/PR
2. **Go version unification**: Both ci.yml and release.yml now use `go-version-file: go.mod` instead of hardcoded versions
3. **Gate enforcement**: Release tags must pass the SAME dual-lane test matrix before publishing

## Concerns
None

## Next Steps (Owner Responsibilities - Not in Scope)
- Push branch to GitHub
- Observe `ci` workflow first run on both lanes (should be green)
- After merge, enable branch protection on master requiring `test` workflow to pass
