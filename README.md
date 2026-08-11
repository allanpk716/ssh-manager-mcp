# ssh-manager-mcp

Let an AI agent (Claude Code, Cursor, …) manage your SSH servers — run commands, transfer files, forward ports — **without ever giving it your SSH password or private key.**

Credentials live only in an encrypted vault. The agent's tools go through a broker that holds the credentials and enforces per-project access (the **iron rule**). The agent never sees a password, never has a real `ssh` client with creds, and can only reach the servers you granted it.

Single Go binary. Cross-platform (Windows / Linux / macOS). No daemon — the broker runs in-process inside the MCP server the agent spawns.

---

## Documentation (中文使用文档)

A full operator-facing guide in Chinese — from zero to running, server CRUD, authorizing agents, token lifecycle (rotate / disable / revoke), and worked scenarios. Index: [`docs/README.md`](docs/README.md).

| 我想要…… | 看这篇 |
|---|---|
| 从零到跑通（安装 / 解锁 / 第一台服务器 / 授权 Claude Code） | [`docs/getting-started.md`](docs/getting-started.md) |
| 新增 / 编辑 / 维护 / 删除服务器 | [`docs/managing-servers.md`](docs/managing-servers.md) |
| 授权 Claude Code / Cursor / 其他 agent；token 轮换与吊销 | [`docs/agent-access.md`](docs/agent-access.md) |
| 应用场景与示例（GPU 巡检、读 root 日志、部署、端口转发……） | [`docs/scenarios.md`](docs/scenarios.md) |

---

## What the agent gets (the MCP tools)

The MCP server exposes these tools — **ssh-functional-equivalent for operating a server** (interactive shell is intentionally not provided):

| Tool | Like | What it does |
|---|---|---|
| `list_servers` | — | List the servers the agent may use (`id` / `name` / `host` / `user` / `has_sudo`, plus owner-provided context: role, services, location, hardware, caveats, tags, description). Always call first — the agent learns real server ids here. Never includes credentials. |
| `exec_command` | `ssh host cmd` | Run a shell command on a server. `sudo=true` runs `sudo -S` for you (do **not** prepend `sudo` yourself). |
| `download_file` | `scp host:path .` | Download a remote file (size-capped; truncated output is flagged). |
| `upload_file` | `scp -r . host:path` | Upload a local file **or directory** (recursive) to the server. |
| `forward_port` | `ssh -L` | Open a local port forwarding to a remote service — returns `127.0.0.1:<port>` for the agent to use (e.g. `curl`). |
| `close_port` | — | Close a forward when done (tunnels also auto-close after idle / on exit). |

Every tool is **profile-gated** (the agent only reaches servers you granted its project) and **audited** (each call logged with project, server, action, status). Credential bytes never appear in any tool result.

---

## The security model (the iron rule)

- **Credentials** (passwords / private keys) are stored only in an **encrypted vault** — AES-256-GCM with a per-record key derived (HKDF) from a master key. The master key lives in the **OS keychain** (or a passphrase fallback for headless hosts).
- The agent authenticates to the MCP with a **project token**, not a credential. The MCP server (the *broker*) holds the master key, opens the SSH connections itself, and returns only command output / file bytes / a forwarded port — **never credentials**.
- The agent's own `ssh` (if it even has a shell) **cannot log in** — there are no creds in `~/.ssh` or `ssh-agent` for it to use, so it's forced through the MCP. A residual-key guardrail warns if stray SSH credential files are detected on the host that could undermine this isolation.
- **Profiles** group servers; a **project** (token) is bound to one profile. The agent sees + reaches only its profile's servers — cross-profile access is rejected by the broker (tested adversarially against a top-tier model).

---

## Quickstart

Build + configure once; then point your AI agent at it.

```bash
# 1. Build — or skip this: grab a prebuilt binary from Releases
#          https://github.com/allanpk716/ssh-manager-mcp/releases
go build -o ssh-manager ./cmd/ssh-manager        # or: go install ./cmd/ssh-manager

# 2. Unlock the vault (master key → OS keychain; passphrase fallback available — see `unlock --help`)
ssh-manager unlock

# 3. Add a server + its credential (exactly one of --password / --key; optional sudo)
ssh-manager servers add --name gpu --host 192.0.2.10 --user deploy \
    --password '...'                 # OR: --key ~/.ssh/id_ed25519 [--key-passphrase '...]
    --sudo-password '...'            # optional: enables sudo=true on exec_command

# 4. Create a profile + grant the server to it
ssh-manager profiles add team-a
ssh-manager profiles grant team-a gpu

# 5. Create a project — prints a ONE-TIME token + the .mcp.json snippet
ssh-manager projects add my-agent --profile team-a
#   Token (shown once): <TOKEN>
#   .mcp.json snippet:
#   {"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp","--token","<TOKEN>"]}}}
```

Drop that snippet into your agent's MCP config (Claude Code: `.mcp.json`; Cursor / other MCP clients: per their setup). The agent now has the six SSH tools, scoped to the `team-a` profile's servers.

**Other commands:** `servers ls` / `servers rm`, `profiles ls`, `projects ls`, `lock`, `version`.

**Owner access** (you, not the agent) — full access to every server using the stored creds directly:
```bash
ssh-manager ssh gpu nvidia-smi          # run a command
ssh-manager ssh gpu                     # (your own ssh client; the broker provides creds)
```

---

## Managing servers & projects (owner CLI)

The owner CLI is how you record servers, group them, and grant an agent access. Each server carries structured metadata (`--role` / `--services` / `--location` / `--hardware` / `--special-handling`) plus free-text `--description` and `--tags` — **all shown to the agent** via `list_servers` so it can act on each box safely. ⚠️ Never put secrets in these fields: they enter the agent's context (and the upstream LLM provider) on every call. See [`docs/managing-servers.md`](docs/managing-servers.md).

```bash
# Edit a server in place: pass any subset of fields — the server id + profile bindings are preserved.
ssh-manager servers edit gpu --hardware "8x A100 80GB, CUDA 12"     # structured field (agent-visible)
ssh-manager servers edit gpu --host 192.0.2.20 --port 2222            # re-point host/port
ssh-manager servers edit gpu --password '...'                        # re-credential (or --key)
ssh-manager servers ls                                                # lists name + role + caveats

# See what an agent can reach, and manage its lifecycle.
ssh-manager projects show my-agent        # agent → profile → granted servers (no secrets)
ssh-manager projects rotate my-agent      # re-key the token (old token dies; prints a new one + .mcp.json)
ssh-manager projects disable my-agent     # suspend (token rejected until enable)
ssh-manager projects enable  my-agent     # resume
ssh-manager projects revoke  my-agent     # permanent (token rejected; hidden from ls)
ssh-manager projects ls [--all]           # status column; --all includes revoked
```

**Lifecycle is Lazy:** `rotate` / `disable` / `enable` / `revoke` take effect at the agent's **next `mcp` spawn** (`VerifyToken` admits only `active` projects). A currently-running agent session keeps its access until Claude Code restarts its MCP child — by design (your box, your call). `rotate` keeps the same project id + profile; only the token changes. `revoke` is a soft delete — the token is dead and the project is hidden from `ls`, but the audit row is kept. Every lifecycle action is written to the audit log.

---

## Do I need an LLM API key?

**No.** ssh-manager-mcp is a **credential broker**, not an LLM client — it never calls any LLM API. It only needs the master key (to unlock the vault) + a project token (so the agent authenticates). The LLM key is the **agent's** business (your Claude Code / Cursor already has its own) and is entirely unrelated to this project.

---

## What's NOT supported (deliberate scope)

- **Interactive shell** (`ssh -t`) — the broker is one-command-per-call. Chain commands with `&&` / `;` if you need a sequence. The agent's value is automatable operations, not a terminal session.
- **Recursive directory download** — `download_file` is single-file. For a directory tree, `exec_command` a `tar` on the remote and download the tar.
- **Remote (`-R`) / dynamic (`-D`) port forwarding** — only local (`-L`) is provided.

---

## Proven

- **Layer 1 — SSH-client conformance (spec §13):** the broker's handcrafted SSH client (built on `golang.org/x/crypto/ssh`) is **byte-for-byte equivalent to OpenSSH** across auth methods (password / RSA / Ed25519 / ECDSA / encrypted), sudo (`sudo -S`), host-key handling, and — as of Plan 6 — **file transfer + local port forwarding**. Verified by differential tests against real `openssh` in Docker (`upload` vs `scp -r`; `forward` vs `ssh -L`). See [`docs/ssh-conformance/`](docs/ssh-conformance/).
- **Layer 2 — Agent usability (spec §12):** a real **Claude Code agent** (driven headless via `claude -p`) completes server-operation tasks through the tools, with the iron rule **holding against a top-tier model** (claude-fable-5): zero credential leaks through the broker tool surface, zero cross-profile escapes, across the full task suite (check GPU, install packages, read root-owned logs, transfer files, resist prompt injection). See [`docs/eval/`](docs/eval/).

---

## Status

Plans 1–6 delivered: encrypted vault → in-process SSH broker → MCP server with profile enforcement → §13 SSH conformance → §12 agent-usability eval (judge + regression gate + nightly CI) → SFTP download → **upload (`scp -r`) + local port forward (`-L`)**.

Carry-forwards (deferred, non-blocking): `context.Context` cancellation threaded through the broker; a server-side exec-timeout cap.

Design spec: [`docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md`](docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md).
