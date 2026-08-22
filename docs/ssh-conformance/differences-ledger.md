# SSH Client Conformance — Differences Ledger & Acceptance Gate

Implements spec §13.4 (differences ledger) and §13.5 (acceptance gate). This
document draws the explicit boundary of the broker's "consistent with
industry-standard SSH" claim, so the 100% conformance target is honest about
what it does and does not cover.

The broker does not shell out to the `ssh` binary. It speaks SSH in-process via
`golang.org/x/crypto/ssh`. "Consistent with OpenSSH" therefore means: produces
the same observable behavior as the reference `ssh` client against a real
OpenSSH `sshd`, within the scope described below.

## Scope of the conformance claim (what 100% means)

Layer-1 conformance (§13) is deterministic and targets **100%**. The gate is:

```bash
SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v
```

Pass = layer-1 100%. Any differential mismatch is a hard failure (zero
tolerance) — the suite fails the run, the doc does not paper over it.

### Test inventory (the gate, as built)

| Test | §ref | What it proves |
|---|---|---|
| `TestHarnessSmoke` | §13 | docker sshd + `ssh` binary + broker all reach the same real OpenSSH sshd and return matching output. Gates every later test. |
| `TestInteropMatrix` | §13.1 | broker authenticates against real sshd across the MVP auth surface. Subtests: `password`, `bare-rsa`, `bare-ed25519`, `bare-ecdsa`, `encrypted-ed25519`, `wrong-password-rejected`. |
| `TestInteropRealSudo` | §13.1 | `ExecSudo` (`sudo -S`, password on stdin) escalates via REAL sudo (sudoers requires a password); a wrong sudo password does NOT escalate. |
| `TestDifferentialParity` | §13.2 | identical command through the broker and the real `ssh` binary → identical stdout / stderr / exit. Subtests: `normal-exec`, `exit-code-7`, `stderr-only`, `large-output`. |
| `TestDifferentialHostKeyRejection` | §13.2 | BOTH the broker (`ErrHostKeyMismatch`) and the `ssh` binary (`StrictHostKeyChecking=yes`) refuse a server whose host key differs from the trusted one. |
| `TestKnownHostsRoundtrip` | §13.3 | OpenSSH known_hosts lines parse + serialize losslessly, and `ssh-keygen -F` finds entries we write (true format compat, not just self-consistent). |

KEX / cipher / MAC are verified through **default negotiation** — the common
combo both the Go library and OpenSSH converge on when neither side pins
algorithms. The exhaustive KEX×cipher×MAC matrix is deliberately out of scope
(see differences table).

## What IS covered

- **§13.1 interop:** password, bare private key (RSA / Ed25519 / ECDSA), and
  encrypted (passphrase-protected) Ed25519 authenticate against a real OpenSSH
  sshd; wrong credentials are rejected; real `sudo -S` (password required by
  sudoers) escalates correctly and a wrong sudo password does not.
- **§13.2 differential parity:** identical commands through the broker and the
  real `ssh` binary — connected to the same sshd — produce identical stdout,
  stderr, and exit code for: normal exec, exit-code propagation, stderr
  separation, and sub-truncation large output (~9 KiB). A host-key change is
  rejected by **both** paths (broker `ErrHostKeyMismatch`; `ssh` binary strict
  known_hosts).
- **§13.3 known_hosts:** OpenSSH-format known_hosts lines parse and serialize
  losslessly, and `ssh-keygen -F` can find entries we write — proving the
  rendered line is genuinely OpenSSH-readable, not just self-consistent.

## Known differences — NOT in the conformance claim (§13.4)

| Area | `golang.org/x/crypto/ssh` / broker | OpenSSH | Status |
|---|---|---|---|
| ProxyJump / bastion | not natively supported by the Go library; broker has no jump API | `-J` / `ProxyJump` | **Out of scope** — MVP non-goal (§1, §11). |
| `~/.ssh/config` parsing (`Match`, `IdentityFile` precedence, host wildcards) | broker self-manages its config in its own vault and **never reads `~/.ssh`** (iron rule, §3/§5) | full parser | **Out of scope by design** — touching `~/.ssh` would violate the broker's isolation guarantee. |
| SSH CA short-lived client certs | not supported (broker keys are long-lived vault secrets) | `ssh -i cert.pem`, `CertificateFile` | **Out of scope** — L3 / non-goal (§1, §9, §11). |
| 2FA / keyboard-interactive / TOTP | not supported | supported | **Out of scope** — non-automation (§1). The broker exists to drive unattended agents. |
| Interactive PTY | exec + `sudo -S` only; no PTY allocation | full PTY (`ssh -t`) | **Out of scope** — MVP scope (§1). |
| Per-command timeout kill | broker feature: `Exec(cmd, timeout)` → on deadline sends `SIGKILL` and closes the session, sets `ExecResult.TimedOut`, returns partial stdout/stderr (MCP field: `timeout_seconds` → `timed_out`) | no native per-command timeout | **Broker-specific** — unit-tested in `internal/sshbroker`, deliberately **excluded from the §13.2 differential** (no `ssh`-binary counterpart for an apples-to-apples comparison). |
| Output truncation (> 1 MiB) | broker caps each output channel at 1 MiB (spec §6): retains the prefix, counts the rest, returns `truncated=true` + `stdout_bytes`/`stderr_bytes` so the agent learns the true size and can refine its command | no truncation (ssh streams everything) | **Broker-specific** — deliberately **excluded from the §13.2 differential** (no `ssh`-binary counterpart). The §13.2 `large-output` subtest uses ~9 KiB, under the cap. |
| Exhaustive KEX×cipher×MAC matrix | default negotiation only (`ssh.ClientConfig` sets no `Config.Ciphers`/`KeyExchanges`/`MACs`) | full algorithm menu | **Out of scope** — flake risk and version drift; default negotiation is what both sides converge on in practice. |
| Multi-algorithm host-key negotiation | broker exposes no `HostKeyAlgorithms` knob on `Connect`; the client accepts whichever host key the server offers first (pinned via `FixedHostKey` or `HostKeyTOFU` after the fact) | client can request a preferred host-key algorithm (`HostKeyAlgorithms`, `HostbasedAcceptedAlgorithms`) | **Out of the conformance claim.** The conformance sshd (`internal/conformance/Dockerfile`) pins `HostKey` to ed25519-only so the negotiated host key is deterministic; this is a test-harness pin, not a broker capability. Multi-algorithm host-key negotiation is not tested and not claimed. |
| host-key storage keying | runtime store keys host keys by `host:port` unconditionally (even `:22`), so same-host-different-port servers never collide | known_hosts uses bare `host` for `:22` and `[host]:port` otherwise | **Documented micro-difference** — semantic parity (per-port isolation) holds; only the `:22` rendering differs from OpenSSH's bare-host convention. The `knownhosts.go` serializer renders `[host]:port` for the known_hosts *file format*; the runtime store uses `host:port` internally. |
| Background task trio (`exec_background` / `exec_output` / `exec_stop`) | broker feature: per-project in-process task table (32-task cap incl. in-flight reservations, 24h run cap, ~1h post-exit retention, 1 MiB rolling tail per channel, byte-offset incremental polling) | no `ssh`-binary counterpart — backgrounding is a remote-side idiom (`nohup` / `tmux`), not a client capability | **Broker-specific** — deliberately **excluded from the §13.2 differential** (no `ssh`-binary counterpart for an apples-to-apples comparison). |
| `exec_stop` kill semantics | stop closes the task's SSH session → the remote process receives SIGHUP; processes started with `nohup`/`setsid` survive; no signal ladder is attempted (OpenSSH's sshd ignores `signal` requests) | killing the `ssh` client delivers SIGHUP the same way; graceful per-signal kills need a PTY (not provided) | **Documented parity** — same kill-the-session semantics as the real `ssh` client; no differential (the trio has no counterpart, row above). |

These differences are bounded in this document. The broker deliberately does
**not** claim "ssh-consistent" anywhere in its agent-facing tool descriptions —
agents see only the `list_servers` / `exec_command` surface with no SSH-conformance
nuance, so there is no boundary to state to them (surfacing it would be scope
creep). "Consistent with ssh" is a developer-facing claim, bounded here.

### Note on the T2 harness pin (load-bearing)

The conformance sshd pins `HostKey /etc/ssh/ssh_host_ed25519_key` and
**regenerates** that key at every container start. Two reasons, both
load-bearing for the suite:

1. **Deterministic negotiation.** `sshbroker.Connect` does not expose a
   `HostKeyAlgorithms` knob, so the Go client takes whatever host key the
   server offers first. With the default sshd (which also presents ECDSA/RSA),
   a harness that captured an ed25519 fingerprint up front and then pinned it
   via `FixedHostKey` could mismatch when the server chose a different
   algorithm. Pinning the server to ed25519-only makes the negotiated key
   deterministic. This is a test-harness constraint, **not** a broker
   capability — see the differences table row "Multi-algorithm host-key
   negotiation".
2. **Independent keys per container.** `ssh-keygen -A` in the image bake step
   would give every container the same host key, which would make
   `TestDifferentialHostKeyRejection` (two containers, expecting distinct
   keys) meaningless. The start-time `rm` + `ssh-keygen` gives each container
   a unique key.

## Running

- **Fast lane (default, every PR):** `go test ./...` — conformance tests
  self-skip when `SSHMGR_CONFORMANCE` is unset or Docker / `ssh` / `ssh-keygen`
  is not on PATH (see `requireConformance`). Covers unit + broker logic; docker
  -free, free, minute-scale.
- **Full conformance (nightly / on-demand / release):**
  `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v` — needs Docker
  Desktop running and the OpenSSH client (`ssh`, `ssh-keygen`) on PATH. Builds
  the local sshd image once (cached thereafter). This run is the empirical
  proof that layer-1 = 100% as of this commit.

The gate is the green full run. If it is not all-green, the 100% claim does
not hold for that commit — fix the suite, do not relax the doc.
