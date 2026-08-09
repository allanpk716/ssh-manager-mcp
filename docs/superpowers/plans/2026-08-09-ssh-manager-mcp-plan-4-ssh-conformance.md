# ssh-manager-mcp Plan 4 — §13 SSH Client Conformance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove the broker's hand-rolled SSH client (`golang.org/x/crypto/ssh`, not OpenSSH) behaves identically to industry-standard SSH — interop with real OpenSSH sshd, zero differential vs the real `ssh` binary, and OpenSSH known_hosts format compatibility — plus land the carry-forward fix that rekeys `host_keys` by `host:port`.

**Architecture:** Two halves. (1) A surgical, docker-free carry-forward: rekey the `host_keys` table from host-only to `host:port` so same-host-different-port servers stop colliding (unblocks multi-port conformance). (2) A new `internal/conformance` package of gated integration tests that spin up real OpenSSH sshd in Docker, drive it through both the broker's Go SSH client and the real `ssh` binary, and assert parity. Tests self-skip unless `SSHMGR_CONFORMANCE=1` so the default fast-lane `go test ./...` stays cheap, deterministic, and docker-free.

**Tech Stack:** Go 1.24, `golang.org/x/crypto/ssh`, `modernc.org/sqlite` (pure-Go), `linuxserver/openssh-server`-style Alpine image (built locally via a committed Dockerfile), real `ssh` + `ssh-keygen` binaries (OpenSSH client, present on the dev Windows box at `C:\Windows\System32\OpenSSH\`).

## Global Constraints

Copied verbatim from the spec's project-wide rules; every task's requirements implicitly include these:

- **Single Go binary, `golang.org/x/crypto/ssh` in-process** — never shell to the `ssh` binary from production code paths. (The `ssh` binary is used ONLY inside the `internal/conformance` test package as a reference oracle; this is dev tooling and does NOT violate the agent iron rule, which governs the agent runtime, not the test process.)
- **Pure-Go SQLite** (`modernc.org/sqlite`), WAL + `busy_timeout(5000)` + `MaxOpenConns(1)`. Schema changes go through `migrate()` before `initSchema()`.
- **Pre-release migration is lossy-but-honest for `host_keys`:** legacy host-only rows are port-ambiguous and are NOT secrets (regenerated via TOFU on next connect), so the migration drops and rebuilds the table. Document this in the commit message.
- **Conformance tests MUST self-skip by default.** Gate on `SSHMGR_CONFORMANCE=1`; additionally `t.Skip` if `docker`, `ssh`, or `ssh-keygen` are absent from PATH. Default `go test ./...` (fast-lane, §12.4) must remain green, deterministic, and docker-free.
- **Layer-1 = deterministic, target 100%.** A differential mismatch is a hard CI failure (zero tolerance, per §13.5). No flakes: poll-and-wait for sshd readiness, use loopback, reuse one built image.
- **`.gitattributes` enforces LF** (`core.autocrlf=false` for the repo) — `gofmt -l .` must report nothing on Windows. Match surrounding code style (`slices.Contains`/`strings.Contains` over hand-rolled loops where idiomatic).
- **No `~/.ssh` touching at runtime.** known_hosts work (§13.3) uses throwaway files under `t.TempDir()`; the broker's runtime store stays the `host_keys` table. The iron rule is preserved.
- **Commits:** one logical commit per task (or per step where the task is large); conventional-commit messages; tests green before each commit.

---

## Scope Decisions (interpretation calls within §13 — surfaced for review)

These resolve ambiguity in the already-approved spec §13. They narrow scope to keep Plan 4 bounded and non-flaky. Flag any you disagree with at the plan-review step.

1. **"SSH 证书" auth (§13.1) is EXCLUDED.** §13.1 lists "SSH 证书" among auth methods, but §1 (非目标), §9, and §11 all explicitly defer/exclude SSH CA short-lived client certificates as L3/non-goals. The three exclusion statements govern; the matrix therefore covers **password / bare private key / encrypted private key (passphrase)** only. SSH-CA support is recorded as out-of-scope in the §13.4 differences ledger.
2. **KEX / cipher / MAC is tested as *default negotiation*, not an exhaustive Cartesian product.** §13.1 says "常见组合 → 每组合均能". A full KEX×cipher×MAC matrix is a known flake/烂尾 source (real sshd advertises a negotiated set; forcing rare combos is environment-fragile). Instead: every interop test exercises the broker's default `ssh.ClientConfig` (lib defaults = the most common combo OpenSSH also defaults to), proving the broker negotiates successfully. The set of algorithms `x/crypto/ssh` enables vs OpenSSH's defaults is documented in the §13.4 ledger. This keeps "100% consistent" honest and bounded.
3. **Differential parity is asserted only where OpenSSH parity is meaningful.** Per-command timeout-kill (§6 broker feature) and >1 MiB output truncation (§6 broker feature) have NO counterpart in the OpenSSH `ssh` CLI, so they are NOT differential-tested (broker-specific; covered by unit tests elsewhere). The differential suite covers: normal exec, exit-code propagation, stderr separation, sub-truncation large output, and host-key-change rejection — the cases where "identical to ssh" is a real, falsifiable claim.
4. **NOPASSWD sudo is NOT separately conformance-tested.** The NOPASSWD path is plain `Exec("sudo …")` plus server-side sudoers config — no broker-specific behavior beyond what the interop exec tests already cover. The high-value sudo test is **`ExecSudo` (sudo -S with password) against REAL `sudo`**, which closes the Plan-2 gap where `testsshd` did not strictly check the sudo password. Recorded in the ledger.

---

## File Structure

**Modified (carry-forward, T1):**
- `internal/store/store.go` — add `migrate(db)` (drops legacy host-only `host_keys`), call before `initSchema`; change `host_keys` schema PK `host` → `host_port`.
- `internal/store/hostkeys.go` — `GetHostKey`/`SaveHostKey` take `(host string, port int)`; add unexported `hostKeyID`.
- `internal/sshbroker/hostkey.go` — `HostKeyStore` interface + `HostKeyTOFU` take `(host string, port int)`.
- Caller edits (mechanical, port threaded through): `internal/mcpserver/core.go:81`, `internal/cli/ssh.go:37`, and test seeders `internal/mcpserver/core_test.go:162`, `internal/mcpserver/e2e_test.go:113`, `internal/cli/ssh_smoke_test.go:45`.

**Modified tests (signatures, T1):** `internal/store/hostkeys_test.go`, `internal/sshbroker/hostkey_test.go`.

**New (conformance package, T2–T5):**
- `internal/conformance/Dockerfile` — Alpine + openssh + sudo, user `sshuser` (pw `testpw123`), password-required sudoers, `StrictModes no`, host keys generated at start.
- `internal/conformance/docker.go` — `requireConformance(t)`, `ensureImage(t)`, `startOpenSSH(t, opts)` → `(host, port, hostKey, containerID, cleanup)`.
- `internal/conformance/sshbin.go` — `runSSHBinary(t, args…) (stdout, stderr string, exitCode int)`, `generateKey(t, keyType, passphrase) (privPath, pubLine string)`, `sshBinaryKeyAuthArgs(host, port, user, privPath) []string`.
- `internal/conformance/knownhosts.go` — `ParseKnownHostsLine(line) (patterns, keyType string, key ssh.PublicKey, err error)`, `FormatKnownHostsLine(patterns string, key ssh.PublicKey) string`.
- `internal/conformance/harness_test.go` — skip-guard smoke test (broker + ssh binary both reach sshd).
- `internal/conformance/interop_test.go` — §13.1 matrix (password / RSA / Ed25519 / ECDSA / encrypted-key / real sudo).
- `internal/conformance/differential_test.go` — §13.2 parity suite.
- `internal/conformance/knownhosts_test.go` — §13.3 roundtrip + `ssh-keygen -F` cross-check (needs ssh-keygen, NOT docker).

**New doc (T6):** `docs/ssh-conformance/differences-ledger.md` — §13.4 ledger + §13.5 acceptance-gate instructions.

---

## Task 1: Carry-forward — rekey `host_keys` by `host:port`

**Why first:** docker-free, deterministic, fast win; explicitly flagged "file before Plan 4 §13"; unblocks multi-port host-key tests in T4.

**Files:**
- Modify: `internal/store/store.go` (add `migrate`, wire into `Open`, change `host_keys` schema)
- Modify: `internal/store/hostkeys.go` (signatures + `hostKeyID`)
- Modify: `internal/sshbroker/hostkey.go` (interface + `HostKeyTOFU`)
- Modify: `internal/mcpserver/core.go:81`, `internal/cli/ssh.go:37` (pass `srv.Port`)
- Modify: test seeders `internal/mcpserver/core_test.go:162`, `internal/mcpserver/e2e_test.go:113`, `internal/cli/ssh_smoke_test.go:45`
- Modify: `internal/store/hostkeys_test.go`, `internal/sshbroker/hostkey_test.go`
- Test: `internal/store/hostkeys_test.go` (add collision regression), `internal/sshbroker/hostkey_test.go` (signatures)

**Interfaces:**
- Produces: `Store.GetHostKey(host string, port int) ([]byte, error)`, `Store.SaveHostKey(host string, port int, marshaledKey []byte) error`, `sshbroker.HostKeyTOFU(st HostKeyStore, host string, port int) (ssh.HostKeyCallback, error)`.

- [ ] **Step 1: Write the failing collision regression test**

Append to `internal/store/hostkeys_test.go` (reuse existing `newTestStore(t)` and `net.SplitHostPort` for port parsing — add `"net"` to imports):

```go
func TestHostKeysKeyedByHostPort(t *testing.T) {
	// Two testsshd instances on different ports get distinct host keys (testsshd
	// generates a fresh key per Start). Storing both against the SAME host must not
	// clobber — this proves host:port keying (legacy host-only keying would collide).
	addr1, hk1, cleanup1 := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup1()
	addr2, hk2, cleanup2 := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup2()

	h1, p1, err := net.SplitHostPort(addr1)
	if err != nil {
		t.Fatal(err)
	}
	h2, p2, err := net.SplitHostPort(addr2)
	if err != nil {
		t.Fatal(err)
	}
	if hk1.Marshal() == nil || bytes.Equal(hk1.Marshal(), hk2.Marshal()) {
		t.Fatal("test servers must have distinct host keys for this test")
	}

	s := newTestStore(t)
	port1, _ := strconv.Atoi(p1)
	port2, _ := strconv.Atoi(p2)
	if err := s.SaveHostKey(h1, port1, hk1.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHostKey(h2, port2, hk2.Marshal()); err != nil {
		t.Fatal(err)
	}

	got1, err := s.GetHostKey(h1, port1)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetHostKey(h2, port2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, hk1.Marshal()) || !bytes.Equal(got2, hk2.Marshal()) {
		t.Fatal("host keys clobbered across ports — keying is not host:port")
	}
}
```

Add imports `"net"`, `"strconv"` to `hostkeys_test.go`; add `"ssh-manager-mcp/internal/testsshd"` (the store test package legitimately depends on testsshd for a real host-key source — test-only import).

Also update the existing `TestHostKeySaveGetRoundTrip` to the new signature (add a port, e.g. `s.GetHostKey("gpu.example", 22)` / `s.SaveHostKey("gpu.example", 22, blob)`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL — `s.SaveHostKey takes 3 arguments` / `GetHostKey takes 3` (signature not yet changed), plus compile errors in sshbroker + callers.

- [ ] **Step 3: Change the `host_keys` schema + add migration**

In `internal/store/store.go`, replace the `host_keys` block in `schemaSQL`:

```go
CREATE TABLE IF NOT EXISTS host_keys (
  host_port TEXT PRIMARY KEY,
  key_blob BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
```

Add `migrate` and call it from `Open` BEFORE `initSchema`:

```go
// migrate evolves the schema from prior pre-release shapes. host_keys was keyed by
// host only (PRIMARY KEY host); it is now keyed by host:port so same-host-different-
// port servers (host sshd:22 + container:2222) don't collide/clobber. Legacy host-only
// rows are port-ambiguous and are NOT secrets (regenerated via TOFU on next connect),
// so we drop and let CREATE rebuild. Idempotent: no-op on fresh and already-migrated DBs.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(host_keys)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasHostCol := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "host" {
			hasHostCol = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasHostCol {
		if _, err := db.Exec(`DROP TABLE host_keys`); err != nil {
			return err
		}
	}
	return nil
}
```

In `Open`, insert the call:

```go
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := initSchema(db); err != nil {
```

- [ ] **Step 4: Update `hostkeys.go` signatures**

Replace `internal/store/hostkeys.go` contents:

```go
package store

import (
	"database/sql"
	"fmt"
)

// hostKeyID is the storage key for a host's pinned public key. Always host:port
// (unconditional, even for :22) so same-host-different-port servers never collide.
// OpenSSH known_hosts uses bare "host" for :22 and "[host]:port" otherwise; that
// format-specific rendering lives in the known_hosts serializer, not here.
func hostKeyID(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// GetHostKey returns the stored marshaled host key for host:port, or (nil, nil) if absent.
func (s *Store) GetHostKey(host string, port int) ([]byte, error) {
	var blob []byte
	err := s.db.QueryRow(`SELECT key_blob FROM host_keys WHERE host_port=?`, hostKeyID(host, port)).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// SaveHostKey records (trusts on first use) a marshaled host key for host:port.
func (s *Store) SaveHostKey(host string, port int, marshaledKey []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO host_keys (host_port, key_blob, created_at) VALUES (?,?,?)
		 ON CONFLICT(host_port) DO UPDATE SET key_blob=excluded.key_blob`,
		hostKeyID(host, port), marshaledKey, now(),
	)
	return err
}
```

- [ ] **Step 5: Update `HostKeyStore` interface + `HostKeyTOFU`**

In `internal/sshbroker/hostkey.go`, change the interface and function to thread `port`:

```go
// HostKeyStore is the subset of *store.Store that HostKeyTOFU needs (also faked in tests).
type HostKeyStore interface {
	GetHostKey(host string, port int) ([]byte, error)
	SaveHostKey(host string, port int, marshaledKey []byte) error
}

// HostKeyTOFU returns a trust-on-first-use host-key callback bound to st and host:port.
// First connection: records the key. Subsequent: must match, else rejected.
func HostKeyTOFU(st HostKeyStore, host string, port int) (ssh.HostKeyCallback, error) {
	return func(_ string, _ net.Addr, remote ssh.PublicKey) error {
		marshaled := remote.Marshal()
		stored, err := st.GetHostKey(host, port)
		if err != nil {
			return err
		}
		if stored == nil {
			if err := st.SaveHostKey(host, port, marshaled); err != nil {
				return fmt.Errorf("save host key: %w", err)
			}
			return nil // trust on first use
		}
		if !bytes.Equal(marshaled, stored) {
			return ErrHostKeyMismatch
		}
		return nil
	}, nil
}
```

- [ ] **Step 6: Update the two production callers (thread `srv.Port`)**

`internal/mcpserver/core.go:81` — change `hkCb, herr := sshbroker.HostKeyTOFU(st, srv.Host)` to:

```go
	hkCb, herr := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
```

`internal/cli/ssh.go:37` — change `hkCb, err := sshbroker.HostKeyTOFU(st, srv.Host)` to:

```go
	hkCb, err := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
```

- [ ] **Step 7: Update the three test seeders + the two host-key test files**

Each `st.SaveHostKey(<host>, <blob>)` becomes `st.SaveHostKey(<host>, <port>, <blob>)`:
- `internal/mcpserver/core_test.go:162` — pass `srv.Port`.
- `internal/mcpserver/e2e_test.go:113` — pass the port in scope (the loopback port the testsshd bound).
- `internal/cli/ssh_smoke_test.go:45` — pass the port in scope.

`internal/sshbroker/hostkey_test.go` — update `fakeHostKeyStore` to key by `host:port` and all `HostKeyTOFU(st, "h")` calls to `HostKeyTOFU(st, "h", portOf(addr))`:

```go
type fakeHostKeyStore struct {
	keys map[string][]byte // keyed by host:port
}

func (f *fakeHostKeyStore) GetHostKey(host string, port int) ([]byte, error) {
	return f.keys[fmt.Sprintf("%s:%d", host, port)], nil
}
func (f *fakeHostKeyStore) SaveHostKey(host string, port int, k []byte) error {
	if f.keys == nil {
		f.keys = map[string][]byte{}
	}
	f.keys[fmt.Sprintf("%s:%d", host, port)] = k
	return nil
}
```

In `TestHostKeyTOFURecordsThenVerifies` and `TestHostKeyMismatchRejected`, replace `HostKeyTOFU(st, "h")` with `HostKeyTOFU(st, "h", portOf(addr))` and the pre-seed key `"h"` with the same `host:port` key (e.g. `keys: map[string][]byte{fmt.Sprintf("h:%d", portOf(addr)): ...}` — recompute inside the test since `addr` is known). Add `"fmt"` to imports if missing. The existing `hostOf`/`portOf` helpers in the sshbroker test package are reused.

- [ ] **Step 8: Run the full suite; verify green**

Run: `go test ./...`
Expected: PASS (store, sshbroker, mcpserver, cli all green; new collision regression passes).

- [ ] **Step 9: Verify the migration is idempotent + LF-clean**

Run:
```bash
go test ./internal/store/ -run TestHostKeys -v   # new test green
gofmt -l .                                        # empty
```
Then manually confirm migration: the test store helper opens a fresh DB each time (new schema, no legacy rows → migrate no-ops). To exercise the legacy-drop path, the implementer adds a throwaway check (do NOT commit): open a DB, hand-create `host_keys(host TEXT PRIMARY KEY, ...)`, insert a row, call `Open` again, confirm table is rebuilt with `host_port` column and the legacy row is gone. Report this manual check in the task report; remove the throwaway before commit.

- [ ] **Step 10: Commit**

```bash
git add internal/store internal/sshbroker internal/mcpserver internal/cli
git commit -m "refactor(hostkeys): rekey host_keys by host:port

Pre-release carry-forward before §13 conformance. Legacy host-only keying
collided for same-host-different-port servers (sshd:22 + container:2222).
Migration drops legacy host-only rows (port-ambiguous, non-secret, TOFU-
regenerated) and rebuilds the table with a host_port TEXT PRIMARY KEY.
HostKeyStore interface + HostKeyTOFU thread port through to callers."
```

---

## Task 2: Conformance harness — real OpenSSH sshd + `ssh` binary helpers (gated)

**Files:**
- Create: `internal/conformance/Dockerfile`
- Create: `internal/conformance/docker.go`
- Create: `internal/conformance/sshbin.go`
- Create: `internal/conformance/harness_test.go`

**Interfaces:**
- Produces: `requireConformance(t *testing.T)`, `startOpenSSH(t, opts OpenSSHOpts) (host string, port int, hostKey ssh.PublicKey, containerID string, cleanup func())`, `runSSHBinary(t, args) (stdout, stderr string, exitCode int)`, `generateKey(t, keyType, passphrase) (privPath, pubLine string)`, `sshBinaryKeyAuthArgs(host string, port int, user, privPath string) []string`.

- [ ] **Step 1: Write the Dockerfile**

`internal/conformance/Dockerfile`:

```dockerfile
# Conformance sshd for §13 tests. NOT shipped; built locally by the test harness.
FROM alpine:3.20
RUN apk add --no-cache openssh sudo
RUN addgroup -S ssh && adduser -S -G ssh -h /home/sshuser -s /bin/sh sshuser
# Login password (used by broker password-auth and as the sudo password).
RUN echo 'sshuser:testpw123' | chpasswd
# Password-required sudo so ExecSudo's `sudo -S` path is exercised against REAL sudo.
RUN echo 'sshuser ALL=(ALL) ALL' > /etc/sudoers.d/sshuser && chmod 0440 /etc/sudoers.d/sshuser
RUN mkdir -p /home/sshuser/.ssh && chown sshuser:ssh /home/sshuser/.ssh && chmod 700 /home/sshuser/.ssh
RUN ssh-keygen -A
# StrictModes no lets us bind-mount authorized_keys (root-owned, read-only) without perm churn.
RUN sed -i -E \
    -e 's/^#?PasswordAuthentication.*/PasswordAuthentication yes/' \
    -e 's/^#?PubkeyAuthentication.*/PubkeyAuthentication yes/' \
    -e 's/^#?PermitRootLogin.*/PermitRootLogin no/' \
    -e 's/^#?StrictModes.*/StrictModes no/' \
    /etc/ssh/sshd_config
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
```

- [ ] **Step 2: Write the docker + skip-guard helper**

`internal/conformance/docker.go`:

```go
package conformance

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const imageTag = "sshmgr-conformance-sshd:local"

// requireConformance skips unless SSHMGR_CONFORMANCE=1 and docker/ssh/ssh-keygen are on PATH.
// This keeps the default fast-lane `go test ./...` docker-free (spec §12.4).
func requireConformance(t *testing.T) {
	t.Helper()
	if os.Getenv("SSHMGR_CONFORMANCE") != "1" {
		t.Skip("set SSHMGR_CONFORMANCE=1 to run OpenSSH conformance tests (needs docker + ssh + ssh-keygen)")
	}
	for _, bin := range []string{"docker", "ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("conformance needs %q on PATH: %v", bin, err)
		}
	}
}

// OpenSSHOpts configures the conformance sshd container.
type OpenSSHOpts struct {
	AuthorizedPubKey string // OpenSSH-format public key line to authorize; "" = password-only
}

// ensureImage builds the conformance sshd image (idempotent; docker caches).
func ensureImage(t *testing.T) {
	t.Helper()
	dir, _ := packageDir()
	cmd := exec.Command("docker", "build", "-q", "-t", imageTag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", dir, err, out)
	}
}

// startOpenSSH launches a real OpenSSH sshd in Docker on a random loopback port.
// Returns host, port, the container's ed25519 host public key, its id, and a cleanup func.
func startOpenSSH(t *testing.T, opts OpenSSHOpts) (host string, port int, hostKey ssh.PublicKey, containerID string, cleanup func()) {
	t.Helper()
	ensureImage(t)

	args := []string{"run", "-d", "--rm", "-p", "127.0.0.1::22"}
	var authFile string
	if opts.AuthorizedPubKey != "" {
		dir := t.TempDir()
		authFile = dir + "/authorized_keys"
		if err := os.WriteFile(authFile, []byte(strings.TrimSpace(opts.AuthorizedPubKey)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-v", authFile+":/home/sshuser/.ssh/authorized_keys:ro")
	}
	args = append(args, imageTag)

	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	containerID = strings.TrimSpace(string(out))

	// Resolve the random host port.
	portOut, err := exec.Command("docker", "port", containerID, "22").Output()
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("docker port: %v", err)
	}
	_, p, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(portOut), "\n")[0]))
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("parse port %q: %v", portOut, err)
	}
	fmt.Sscanf(p, "%d", &port)
	host = "127.0.0.1"

	// Wait for sshd to accept TCP connections (container start is async).
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 500*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Retrieve the container's ed25519 host public key.
	pubOut, err := exec.Command("docker", "exec", containerID, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").Output()
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("cat host key: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(pubOut)))
	if len(fields) < 2 {
		dockerKill(containerID)
		t.Fatalf("unexpected host key line: %q", pubOut)
	}
	hostKey, err = ssh.ParsePublicKey([]byte(fields[0] + " " + fields[1]))
	if err != nil {
		dockerKill(containerID)
		t.Fatalf("parse host key: %v", err)
	}

	cleanup = func() { dockerKill(containerID) }
	return host, port, hostKey, containerID, cleanup
}

func dockerKill(id string) {
	_ = exec.Command("docker", "rm", "-f", id).Run()
}

// packageDir returns this package's directory (Dockerfile lives alongside).
func packageDir() (string, error) {
	// Determined at runtime from the test binary; avoids os.Getwd fragility.
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Fall back to CWD; tests are always run from the package dir or repo root.
	if wd, err := os.Getwd(); err == nil {
		// Prefer the dir that actually contains the Dockerfile.
		if _, err := os.Stat(wd + "/Dockerfile"); err == nil {
			return wd, nil
		}
	}
	_ = path
	return os.Getwd()
}
```

Note: `packageDir` prefers CWD when it contains the Dockerfile (the normal case: `go test ./internal/conformance/` runs with CWD = the package dir). The `os.Executable` branch is a defensive fallback the implementer may simplify — the requirement is only "locate the Dockerfile dir"; a passing test in Step 5 is the proof.

- [ ] **Step 3: Write the ssh-binary helpers**

`internal/conformance/sshbin.go`:

```go
package conformance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSSHBinary invokes the real `ssh` binary and returns separated stdout/stderr + exit code.
func runSSHBinary(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command("ssh", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("ssh binary failed to start: %v", err)
	} // err == nil → exitCode stays 0
	return out.String(), errb.String(), exitCode
}

// generateKey creates an OpenSSH-format keypair via ssh-keygen in a temp dir.
// keyType ∈ {rsa, ed25519, ecdsa}; passphrase may be "" for an unencrypted key.
// Returns the path to the private key file and the public key line (authorized_keys format).
func generateKey(t *testing.T, keyType, passphrase string) (privPath, pubLine string) {
	t.Helper()
	dir := t.TempDir()
	privPath = filepath.Join(dir, "id")
	args := []string{"-q", "-t", keyType, "-f", privPath, "-N", passphrase, "-C", "conformance"}
	if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -t %s: %v\n%s", keyType, err, out)
	}
	pub, err := os.ReadFile(privPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return privPath, strings.TrimSpace(string(pub))
}

// sshBinaryKeyAuthArgs assembles the common ssh-binary args for key-auth against the
// conformance sshd: batch mode, identity pinned, no host-key prompts, quiet stderr.
func sshBinaryKeyAuthArgs(host string, port int, user, privPath string) []string {
	return []string{
		"-p", itoa(port),
		"-i", privPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		user + "@" + host,
	}
}

func itoa(n int) string {
	// avoid pulling strconv just for this; small helper
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
```

(The implementer may replace `itoa` with `strconv.Itoa` + the import if preferred — functionally identical; `strconv` is the more idiomatic choice, use it.)

- [ ] **Step 4: Write the harness smoke test**

`internal/conformance/harness_test.go`:

```go
package conformance

import (
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestHarnessSmoke proves the docker sshd + ssh-binary + broker client all reach the
// same real OpenSSH sshd and return matching output. Gates every later conformance test.
func TestHarnessSmoke(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	// Broker path (Go SSH), trusting the known host key on first use via FixedHostKey.
	cb := ssh.FixedHostKey(hostKey)
	cli, err := sshbroker.Connect(host, port, "sshuser", mustPrivAuth(t, privPath, ""), cb)
	if err != nil {
		t.Fatalf("broker connect: %v", err)
	}
	defer cli.Close()
	res, err := cli.Exec("printf %s hi-broker", 0)
	if err != nil {
		t.Fatalf("broker exec: %v", err)
	}
	if res.Stdout != "hi-broker" {
		t.Fatalf("broker stdout = %q, want hi-broker", res.Stdout)
	}

	// ssh-binary path.
	out, _, code := runSSHBinary(t, append(sshBinaryKeyAuthArgs(host, port, "sshuser", privPath), "printf %s hi-bin")...)
	if code != 0 || out != "hi-bin" {
		t.Fatalf("ssh binary: code=%d stdout=%q", code, out)
	}
}
```

Add a tiny helper `mustPrivAuth` (in `sshbin.go` or a `helpers_test.go`) that reads the private key file and returns `ssh.PublicKeys(signer)`:

```go
func mustPrivAuth(t *testing.T, privPath, passphrase string) ssh.AuthMethod {
	t.Helper()
	keyPEM, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := sshbroker.PrivateKeyAuth(keyPEM, []byte(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	return auth
}
```

- [ ] **Step 5: Run the smoke test (needs Docker running locally)**

Run: `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run TestHarnessSmoke -v`
Expected: PASS (image builds once, container starts, broker + ssh binary both print their markers).

If it fails on `docker build` (image pull / network), retry once; if `docker port` parsing fails, inspect `docker port <id> 22` output format and adjust the `SplitHostPort` line. Report any environment quirks.

- [ ] **Step 6: Verify the default fast-lane still skips cleanly**

Run: `go test ./internal/conformance/`
Expected: PASS with `ok` and `--- SKIP` lines (no docker invoked, no failure). This is the fast-lane guarantee.

- [ ] **Step 7: Commit**

```bash
git add internal/conformance
git commit -m "test(conformance): docker OpenSSH sshd + ssh-binary harness (§13)

Gated by SSHMGR_CONFORMANCE=1 + PATH checks; default go test ./... skips.
Dockerfile: Alpine openssh + sudo, sshuser/testpw123, password-required
sudo (exercises real sudo -S), StrictModes no for bind-mount authorized_keys.
Helpers: startOpenSSH (random loopback port, readiness poll, host-key fetch),
runSSHBinary, generateKey (ssh-keygen), sshBinaryKeyAuthArgs. Smoke test
proves broker + ssh binary both reach the same real sshd."
```

---

## Task 3: §13.1 Interoperability matrix — broker ↔ real OpenSSH sshd

**Files:**
- Create: `internal/conformance/interop_test.go`

**Interfaces:** Consumes T2 helpers. Produces: table-driven interop coverage.

**Coverage (per scope decisions 1 & 2):** password auth · bare key (RSA / Ed25519 / ECDSA) · encrypted key (passphrase) · real `sudo -S`. KEX/cipher/MAC via default negotiation (implicit in every connect).

- [ ] **Step 1: Write the interop matrix test**

`internal/conformance/interop_test.go`:

```go
package conformance

import (
	"bytes"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestInteropMatrix proves the broker's Go SSH client authenticates against real
// OpenSSH sshd across the MVP auth surface (scope: no SSH-CA; KEX via defaults).
func TestInteropMatrix(t *testing.T) {
	requireConformance(t)

	// Pre-generate one key per type so the matrix can authorize them all up front.
	rsaPriv, rsaPub := generateKey(t, "rsa", "")
	edPriv, edPub := generateKey(t, "ed25519", "")
	ecdsaPriv, ecdsaPub := generateKey(t, "ecdsa", "")
	encPriv, encPub := generateKey(t, "ed25519", "secret-pass")
	allKeys := strings.Join([]string{rsaPub, edPub, ecdsaPub, encPub}, "\n")

	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: allKeys})
	defer cleanup

	type cas struct {
		name     string
		auth     ssh.AuthMethod
		marker   string
		exitCode int
	}
	cases := []cas{
		{"password", sshbroker.PasswordAuth("testpw123"), "pw-ok", 0},
		{"bare-rsa", mustPrivAuth(t, rsaPriv, ""), "rsa-ok", 0},
		{"bare-ed25519", mustPrivAuth(t, edPriv, ""), "ed-ok", 0},
		{"bare-ecdsa", mustPrivAuth(t, ecdsaPriv, ""), "ecdsa-ok", 0},
		{"encrypted-ed25519", mustPrivAuth(t, encPriv, "secret-pass"), "enc-ok", 0},
		{"wrong-password-rejected", sshbroker.PasswordAuth("nope"), "", 255}, // connect fails
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cb := ssh.FixedHostKey(hostKey) // trust the known host key
			cli, err := sshbroker.Connect(host, port, "sshuser", c.auth, cb)
			if c.marker == "" {
				// Expect auth failure.
				if err == nil {
					cli.Close()
					t.Fatal("expected connect to fail, succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer cli.Close()

			res, err := cli.Exec("printf %s "+c.marker, 0)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if res.ExitCode != c.exitCode {
				t.Fatalf("exit = %d, want %d", res.ExitCode, c.exitCode)
			}
			if res.Stdout != c.marker {
				t.Fatalf("stdout = %q, want %q", res.Stdout, c.marker)
			}
		})
	}
}

// TestInteropRealSudo proves ExecSudo (sudo -S, password on stdin) runs a privileged
// command against REAL sudo (closes the Plan-2 gap where testsshd did not strictly
// check the sudo password). The conformance user requires a password for sudo.
func TestInteropRealSudo(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	cli, err := sshbroker.Connect(host, port, "sshuser", mustPrivAuth(t, privPath, ""), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	// Real sudo here requires a password; broker feeds it via sudo -S.
	res, err := cli.ExecSudo("whoami", []byte("testpw123"), 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "root" {
		t.Fatalf("sudo whoami stdout = %q, want root", res.Stdout)
	}
	// A wrong sudo password must NOT escalate.
	resBad, _ := cli.ExecSudo("whoami", []byte("wrong-sudo-pw"), 0)
	if bytes.Contains([]byte(resBad.Stdout), []byte("root")) {
		t.Fatalf("wrong sudo password escalated; stdout=%q", resBad.Stdout)
	}
}
```

- [ ] **Step 2: Run the matrix**

Run: `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run 'TestInterop' -v`
Expected: all subtests PASS — password, all three bare key types, encrypted key, wrong-password-rejected, and real sudo returns root (wrong sudo pw does not).

If `ecdsa` auth fails (some Alpine openssh builds disable ecdsa key types by default), check `/etc/ssh/sshd_config` `PubkeyAcceptedAlgorithms`; if needed add `PubkeyAcceptedAlgorithms +ecdsa-sha2-nistp256` to the Dockerfile and document. Report the resolution.

- [ ] **Step 3: Commit**

```bash
git add internal/conformance/interop_test.go
git commit -m "test(conformance): §13.1 interop matrix vs real OpenSSH sshd

Auth: password, bare RSA/Ed25519/ECDSA, encrypted (passphrase), wrong-pw
reject. KEX/cipher/MAC via default negotiation (scope decision 2). Real sudo
test proves ExecSudo sudo -S works against actual sudo with a required
password (closes Plan-2 testsshd rigor gap)."
```

---

## Task 4: §13.2 Differential parity — broker vs real `ssh` binary (zero diff)

**Files:**
- Create: `internal/conformance/differential_test.go`

**Coverage (scope decision 3):** normal exec · exit-code propagation · stderr separation · sub-truncation large output · host-key-change rejection. Timeouts and >1MiB truncation are broker-specific (no ssh-binary counterpart) — excluded from diff, documented in the ledger.

- [ ] **Step 1: Write the differential parity suite**

`internal/conformance/differential_test.go`:

```go
package conformance

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestDifferentialParity runs identical commands through the broker's Go SSH client
// and the real `ssh` binary against the same sshd, asserting stdout/stderr/exit match.
// Zero differential = the broker is consistent with the industry-standard client (§13.2).
func TestDifferentialParity(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	brokerAuth := mustPrivAuth(t, privPath, "")
	sshArgs := append(sshBinaryKeyAuthArgs(host, port, "sshuser", privPath))

	type scenario struct {
		name string
		cmd  string // remote command, identical for both paths
	}
	scenarios := []scenario{
		{"normal-exec", "printf %s out123"},
		{"exit-code-7", "sh -c 'exit 7'"},
		{"stderr-only", "printf %s err-on-stderr 1>&2"},
		{"large-output", "seq 1 2000"}, // ~9 KiB, well under the 1 MiB truncation threshold
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			// Broker path.
			cli, err := sshbroker.Connect(host, port, "sshuser", brokerAuth, ssh.FixedHostKey(hostKey))
			if err != nil {
				t.Fatalf("broker connect: %v", err)
			}
			defer cli.Close()
			bRes, err := cli.Exec(sc.cmd, 0)
			if err != nil {
				t.Fatalf("broker exec: %v", err)
			}
			// ssh-binary path.
			sOut, sErr, sCode := runSSHBinary(t, append(append([]string{}, sshArgs...), sc.cmd)...)

			if bRes.Stdout != sOut {
				t.Errorf("stdout diff:\nbroker=%q\nssh   =%q", bRes.Stdout, sOut)
			}
			if bRes.Stderr != sErr {
				t.Errorf("stderr diff:\nbroker=%q\nssh   =%q", bRes.Stderr, sErr)
			}
			if bRes.ExitCode != sCode {
				t.Errorf("exit diff: broker=%d ssh=%d", bRes.ExitCode, sCode)
			}
		})
	}
}

// TestDifferentialHostKeyRejection asserts BOTH the broker and the real `ssh` binary
// refuse a server whose host key differs from the trusted one (TOFU/strict semantics).
func TestDifferentialHostKeyRejection(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")

	// Trust container A's host key, then connect to container B (different key) on a
	// different port — both the broker (ErrHostKeyMismatch) and ssh binary must reject.
	hostA, portA, keyA, _, cleanupA := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanupA()
	hostB, portB, _, _, cleanupB := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanupB()
	if bytes.Equal(keyA.Marshal(), mustKeyB(t, hostB, portB, pub).Marshal()) {
		// sanity: distinct containers must have distinct host keys
	}

	// Broker: pre-trust keyA under hostB:portB, then connect to B → mismatch.
	st := newFakeStore(map[string][]byte{fmtHostPort(hostB, portB): keyA.Marshal()})
	cb, _ := sshbroker.HostKeyTOFU(st, hostB, portB)
	_, err := sshbroker.Connect(hostB, portB, "sshuser", mustPrivAuth(t, privPath, ""), cb)
	if err == nil {
		t.Fatal("broker accepted a mismatched host key")
	}

	// ssh binary: known_hosts with keyA under [hostB]:portB, strict checking → reject.
	kh := t.TempDir() + "/known_hosts"
	line := sshBinaryKnownHostsLine(hostB, portB, keyA)
	if err := os.WriteFile(kh, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-p", itoa(portB), "-i", privPath,
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + kh,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "LogLevel=ERROR",
		"sshuser@" + hostB, "true",
	}
	_, _, code := runSSHBinary(t, args...)
	if code == 0 {
		t.Fatal("ssh binary accepted a mismatched host key")
	}
}
```

Supporting helpers (add to `internal/conformance/sshbin.go` or a new `internal/conformance/diff_helpers_test.go`):

```go
func fmtHostPort(host string, port int) string { return host + ":" + itoa(port) }

// mustKeyB dials container B just to fetch its host key for the sanity check
// (kept minimal; could reuse startOpenSSH's returned key instead).
func mustKeyB(t *testing.T, host string, port int, pub string) ssh.PublicKey {
	// Re-fetch via the ssh binary scanning the live server.
	t.Helper()
	out, _, _ := runSSHBinary(t, "-p", itoa(port),
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-i", "NOSUCHKEY", "sshuser@"+host, "true")
	_ = out // unused; the real comparison uses keyA vs a fresh startOpenSSH below
	return nil
}

// sshBinaryKnownHostsLine renders an OpenSSH known_hosts entry: [host]:port type base64.
func sshBinaryKnownHostsLine(host string, port int, key ssh.PublicKey) string {
	return "[" + host + "]:" + itoa(port) + " " + key.Type() + " " +
		base64Std(key.Marshal())
}

func base64Std(b []byte) string {
	return strings.TrimSpace(string((bytesBuffer(b))))
}
```

NOTE for the implementer: the `mustKeyB`/`base64Std`/`bytesBuffer` shims above are deliberately sketched — REPLACE them with clean implementations:
- The host-key *distinctness* sanity check: simply compare `keyA` against the key returned by `startOpenSSH` for B (you already have both — `keyA` and B's key from its `startOpenSSH` return). Capture B's returned host key in a variable instead of re-fetching. Drop `mustKeyB` entirely.
- `base64Std`: use `encoding/base64` — `base64.StdEncoding.EncodeToString(key.Marshal())`.
Use `strconv.Itoa` over the `itoa` helper. The goal is correct, idiomatic code; the sketch's job is to pin the *behavior* (known_hosts line format `[host]:port type b64`, broker pre-trust under `host:port`, both reject). Clean it up before commit.

- [ ] **Step 2: Run the differential suite**

Run: `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run 'TestDifferential' -v`
Expected: all subtests PASS with zero diffs; both reject paths return nonzero. This is the §13.5 "零差分" gate for these scenarios.

- [ ] **Step 3: Commit**

```bash
git add internal/conformance/differential_test.go
git commit -m "test(conformance): §13.2 differential parity vs ssh binary (zero diff)

Identical commands through broker Go SSH and real ssh binary against the same
sshd; assert stdout/stderr/exit match. Scenarios: normal exec, exit-code
propagation, stderr separation, sub-truncation large output, host-key-change
rejection (broker ErrHostKeyMismatch AND ssh binary strict-known-hosts both
refuse). Timeouts + >1MiB truncation are broker-specific (documented, not
diffed) per scope decision 3."
```

---

## Task 5: §13.3 known_hosts OpenSSH-format compatibility

**Files:**
- Create: `internal/conformance/knownhosts.go`
- Create: `internal/conformance/knownhosts_test.go`

**Note:** This task needs `ssh-keygen` but NOT docker. It is gated by `requireConformance` (which checks ssh-keygen) — fine. The broker never touches `~/.ssh` at runtime; this is a parse/serialize compatibility proof using throwaway files.

- [ ] **Step 1: Write the failing roundtrip test**

`internal/conformance/knownhosts_test.go`:

```go
package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestKnownHostsRoundtrip proves we parse and re-render an OpenSSH known_hosts line
// without loss, and that the real ssh-keygen can FIND an entry we wrote (true format
// compatibility, §13.3).
func TestKnownHostsRoundtrip(t *testing.T) {
	requireConformance(t)

	// Generate a host key via ssh-keygen, read its public line.
	_, pub := generateHostKey(t) // ssh-keygen -t ed25519, returns authorized_keys-style pub line
	patterns := "[example.com]:2222"
	_, key := parsePubLine(t, pub)

	formatted := FormatKnownHostsLine(patterns, key)
	gotPatterns, gotType, gotKey, err := ParseKnownHostsLine(formatted)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gotPatterns != patterns || gotType != key.Type() {
		t.Fatalf("roundtrip lost data: patterns=%q type=%q", gotPatterns, gotType)
	}
	if string(gotKey.Marshal()) != string(key.Marshal()) {
		t.Fatal("roundtrip lost key bytes")
	}

	// Cross-check: write our line, have ssh-keygen -F find it.
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(formatted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found := sshKeygenFind(t, kh, "example.com", 2222); !strings.Contains(found, "example.com") {
		t.Fatalf("ssh-keygen -F did not find our entry:\n%s", found)
	}
}

// parsePubLine parses an authorized_keys/pub line "type b64 [comment]" into ssh.PublicKey.
func parsePubLine(t *testing.T, line string) (string, ssh.PublicKey) {
	t.Helper()
	f := strings.Fields(line)
	if len(f) < 2 {
		t.Fatalf("bad pub line %q", line)
	}
	key, err := ssh.ParsePublicKey([]byte(f[0] + " " + f[1]))
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	return f[0], key
}
```

(`generateHostKey` and `sshKeygenFind` are helpers added in Step 2 alongside the impl; sketches below.)

- [ ] **Step 2: Implement the parser/serializer + helpers**

`internal/conformance/knownhosts.go`:

```go
package conformance

import (
	"encoding/base64"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// ParseKnownHostsLine parses one OpenSSH known_hosts line:
//
//	[patterns] type base64-key   (or   patterns type base64-key)
//
// Returns the host patterns, the key type string, and the parsed public key.
func ParseKnownHostsLine(line string) (patterns string, keyType string, key ssh.PublicKey, err error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", "", nil, errors.New("known_hosts line needs patterns, type, key")
	}
	patterns, keyType, b64 := fields[0], fields[1], fields[2]
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", "", nil, err
	}
	key, err = ssh.ParsePublicKey(blob)
	if err != nil {
		return "", "", nil, err
	}
	return patterns, keyType, key, nil
}

// FormatKnownHostsLine renders an OpenSSH known_hosts line for patterns + key.
func FormatKnownHostsLine(patterns string, key ssh.PublicKey) string {
	return patterns + " " + key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
}
```

Helpers in `knownhosts_test.go` (or `sshbin.go`):

```go
// generateHostKey creates an ed25519 host key via ssh-keygen and returns its pub line.
func generateHostKey(t *testing.T) (privPath, pubLine string) {
	t.Helper()
	return generateKey(t, "ed25519", "") // reuse: same shape, "host" vs "client" is just usage
}

// sshKeygenFind runs `ssh-keygen -F host -p port -f file` and returns its stdout.
func sshKeygenFind(t *testing.T, file, host string, port int) string {
	t.Helper()
	out, err := exec.Command("ssh-keygen", "-F", host, "-p", strconvItoa(port), "-f", file).Output()
	if err != nil {
		// ssh-keygen -F exits nonzero when not found; that is itself signal.
		return string(out)
	}
	return string(out)
}
```

(Use `strconv.Itoa`; `strconvItoa` is a stand-in name to avoid clashing with the T2 `itoa` — the implementer consolidates to `strconv.Itoa` everywhere and deletes the hand-rolled `itoa`.)

- [ ] **Step 3: Run the known_hosts test**

Run: `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run TestKnownHostsRoundtrip -v`
Expected: PASS — roundtrip preserves data and `ssh-keygen -F` finds the entry we wrote (true OpenSSH-format compatibility).

- [ ] **Step 4: Commit**

```bash
git add internal/conformance/knownhosts.go internal/conformance/knownhosts_test.go
git commit -m "test(conformance): §13.3 OpenSSH known_hosts format compatibility

ParseKnownHostsLine / FormatKnownHostsLine (parse + serialize, base64 blob via
ssh.ParsePublicKey). Roundtrip preserves patterns/type/bytes; ssh-keygen -F
cross-check proves the rendered line is genuinely OpenSSH-readable. Broker
runtime store stays host_keys (iron rule intact); this is an import/compat path."
```

---

## Task 6: §13.4 differences ledger + §13.5 acceptance-gate documentation

**Files:**
- Create: `docs/ssh-conformance/differences-ledger.md`

This task documents the boundaries of the "consistent with OpenSSH" claim and records how to run the §13.5 gate. No code; the test suite IS the gate.

- [ ] **Step 1: Write the differences ledger**

`docs/ssh-conformance/differences-ledger.md`:

````markdown
# SSH Client Conformance — Differences Ledger & Acceptance Gate

Implements spec §13.4 (differences ledger) and §13.5 (acceptance gate). This document
draws the explicit boundary of the broker's "consistent with industry-standard SSH"
claim, so the 100% conformance target is honest about what it does and does not cover.

## Scope of the conformance claim (what 100% means)

Layer-1 conformance (§13) is deterministic and targets **100%**. The gate is:

```bash
SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v
```

Pass = layer-1 100%. Any differential mismatch is a hard failure (zero tolerance).

## What IS covered

- **§13.1 interop:** password, bare private key (RSA / Ed25519 / ECDSA), encrypted
  private key (passphrase) authenticate against real OpenSSH sshd; wrong credentials
  rejected; real `sudo -S` (password required) escalates correctly. KEX / cipher / MAC
  verified through default negotiation (the common combo both the Go lib and OpenSSH
  default to).
- **§13.2 differential parity:** identical commands through the broker and the real
  `ssh` binary produce identical stdout / stderr / exit for: normal exec, exit-code
  propagation, stderr separation, sub-truncation large output. Host-key change is
  rejected by BOTH (broker `ErrHostKeyMismatch`; ssh binary strict known_hosts).
- **§13.3 known_hosts:** OpenSSH-format known_hosts lines parse and serialize
  losslessly, and `ssh-keygen -F` can find entries we write.

## Known differences — NOT in the conformance claim (§13.4)

| Area | `golang.org/x/crypto/ssh` / broker | OpenSSH | In scope? |
|---|---|---|---|
| ProxyJump / bastion | not natively supported | `-J` | No — MVP non-goal (§1, §11) |
| `~/.ssh/config` parsing (`Match`, `IdentityFile` precedence) | broker self-manages config; never reads `~/.ssh` (iron rule) | full parser | No — by design |
| SSH CA short-lived client certs | not supported | `ssh -i cert` | No — L3 / non-goal (§1, §9, §11) |
| 2FA / keyboard-interactive / TOTP | not supported | supported | No — non-automation (§1) |
| Interactive PTY | exec + `sudo -S` only | full PTY | No — MVP scope (§1) |
| Per-command timeout kill | broker feature (`timeout_seconds` → SIGKILL + partial output) | no native per-cmd timeout | Broker-specific — unit-tested, not diffed |
| Output truncation (> 1 MiB) | broker feature (`truncated`) | no truncation | Broker-specific — §6, not diffed |
| Exhaustive KEX×cipher×MAC matrix | default negotiation tested | — | Out of scope (flake risk); see ledger intro |
| host-key storage keying | broker stores `host:port` (unconditional) | known_hosts uses bare `host` for :22, `[host]:port` otherwise | Documented; semantic parity (per-port isolation) holds |

These differences are reflected in the MCP tool descriptions so "consistent with ssh"
carries an explicit boundary.

## Running

- **Fast lane (default, every PR):** `go test ./...` — conformance tests self-skip
  (no Docker, no cost). Covers unit + broker logic.
- **Full conformance (nightly / on-demand / release):**
  `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v` — needs Docker + the
  OpenSSH client (`ssh`, `ssh-keygen`) on PATH. Builds the local sshd image once.
````

- [ ] **Step 2: Sanity-run the full gate once**

Run: `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v`
Expected: all green (T2 smoke, T3 matrix, T4 differential, T5 known_hosts). This run
is the empirical proof that layer-1 = 100% as of this commit.

- [ ] **Step 3: Final repo-wide check + commit**

Run:
```bash
go test ./...                                  # fast lane still green & skipping
gofmt -l .                                     # empty
go vet ./...                                   # clean
```
Commit:
```bash
git add docs/ssh-conformance/differences-ledger.md
git commit -m "docs(conformance): §13.4 differences ledger + §13.5 acceptance gate

Records the boundary of the 'consistent with OpenSSH' claim (ProxyJump,
~/.ssh/config, SSH-CA, PTY, per-cmd timeout, truncation, KEX matrix, host-key
keying) and how to run the 100% gate (SSHMGR_CONFORMANCE=1)."
```

---

## Self-Review (run before handoff)

1. **Spec coverage (§13):** §13.1 → Task 3 (auth matrix) + KEX-via-default documented (Task 6). §13.2 → Task 4 (differential, zero diff). §13.3 → Task 5 (known_hosts parse/serialize + ssh-keygen cross-check). §13.4 → Task 6 (ledger). §13.5 → Task 6 (gate instructions) + the green full run in T6 Step 2. Carry-forward (host:port) → Task 1. All sections covered.
2. **Placeholder scan:** T4 explicitly flags the sketched helpers (`mustKeyB`, `base64Std`, `strconvItoa`) as "replace with clean idiomatic code" — these are intentional behavior-pins, not unfinished work, and the instruction names the correct replacements (`strconv.Itoa`, `base64.StdEncoding`, capture B's returned key directly). No other TBDs.
3. **Type consistency:** `GetHostKey(host, port)` / `SaveHostKey(host, port, blob)` / `HostKeyTOFU(st, host, port)` — signatures consistent across T1 store, sshbroker, callers, and tests. `ExecResult{Stdout,Stderr,ExitCode,TimedOut}` matches existing code. `runSSHBinary` / `generateKey` / `startOpenSSH` signatures consistent across T2–T5.
4. **Scope:** 6 tasks, each independently testable + committable. T1 is docker-free (guaranteed win). T2–T5 share one harness; T6 is docs. CI wiring (GitHub Actions workflow) is deliberately OUT of scope — Plan 4 delivers the test suite + gate; wiring it into CI is an ops follow-up.

---

## Execution Handoff

Two options:

1. **Subagent-Driven (recommended)** — fresh implementer per task, review between tasks (especially the docker-dependent T2 harness, which is the riskiest), final whole-branch review. The SDD ledger should note that T2–T5 require `SSHMGR_CONFORMANCE=1` + Docker for full verification; task reviewers must run the gated tests, not just the fast lane.
2. **Inline Execution** — batch execution with checkpoints, in this session.

Which approach?
