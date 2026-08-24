# ssh-manager-mcp

Let an AI agent (Claude Code, Cursor, …) manage your SSH servers — run commands, transfer files, forward ports — **without ever giving it your SSH password or private key.**

Credentials live only in an encrypted vault. The agent's tools go through a broker that holds the credentials and enforces per-project access (the **iron rule**). The agent never sees a password, never has a real `ssh` client with creds, and can only reach the servers you granted it.

Single Go binary. Cross-platform (Windows / Linux / macOS). No daemon — the broker runs in-process inside the MCP server the agent spawns.

---

## Documentation (中文使用文档)

**最快的入口** — 两篇精简入门（每篇 5 分钟，只讲最少步骤）：

| 我是…… | 看这篇 |
|---|---|
| **单机**用（一台操作机 + 一台目标服务器） | [`docs/quickstart-single-machine.md`](docs/quickstart-single-machine.md) |
| **多机**用（多台机器共用一份 vault） | [`docs/quickstart-multi-machine.md`](docs/quickstart-multi-machine.md) |

**详尽操作指南**（从零到跑通 / 全部可选项 / 排错 / 场景）：

| 我想要…… | 看这篇 |
|---|---|
| 从零到跑通（安装 / 解锁 / 第一台服务器 / 授权 Claude Code） | [`docs/getting-started.md`](docs/getting-started.md) |
| 新增 / 编辑 / 维护 / 删除服务器 | [`docs/managing-servers.md`](docs/managing-servers.md) |
| 授权 Claude Code / Cursor / 其他 agent；token 轮换与吊销 | [`docs/agent-access.md`](docs/agent-access.md) |
| 多台机器共用一份 vault（serve 模式）/ 自动 TLS / 离线只读缓存兜底 | [`docs/multi-machine.md`](docs/multi-machine.md) |
| 应用场景与示例（GPU 巡检、读 root 日志、部署、端口转发……） | [`docs/scenarios.md`](docs/scenarios.md) |
| 备份 / 迁移整个 vault（export / import） | [`docs/backup-restore.md`](docs/backup-restore.md) |
| **单机 TUI 教程**（全键盘点选，不想记命令） | [`docs/tui-single-machine.md`](docs/tui-single-machine.md) |
| **联机 TUI 教程**（server 侧 + 工作机 client 面板） | [`docs/tui-multi-machine.md`](docs/tui-multi-machine.md) |
| **给 AI agent 的工具手册**（可贴进 CLAUDE.md 的规则模板在内） | [`docs/agent-tools.md`](docs/agent-tools.md) |

---

## What the agent gets (the MCP tools)

The MCP server exposes these tools — **ssh-functional-equivalent for operating a server** (interactive shell is intentionally not provided):

| Tool | Like | What it does |
|---|---|---|
| `list_servers` | — | List the servers the agent may use (`id` / `name` / `host` (`"hidden"` by default; owner opts in per server) / `user` / `has_sudo`, plus owner-provided context: role, services, location, hardware, caveats, tags, description). Always call first — the agent learns real server ids here. Never includes credentials. |
| `exec_command` | `ssh host cmd` | Run a shell command on a server. `sudo=true` runs `sudo -S` for you (do **not** prepend `sudo` yourself). |
| `download_file` | `scp host:path .` | Download a remote file (size-capped; truncated output is flagged). |
| `upload_file` | `scp -r . host:path` | Upload a local file **or directory** (recursive) to the server. |
| `upload_content` | — | Write inline content (a string the agent holds) to a remote file — the **cross-machine** upload path (`upload_file` reads the broker's own filesystem; a remote-serve agent pushes its own configs/scripts here). `text` / `base64` (byte-exact) encodings, 8 MiB decoded cap, parent dirs auto-created, existing file overwritten. |
| `forward_port` | `ssh -L` | Open a local port forwarding to a remote service — returns `127.0.0.1:<port>` for the agent to use (e.g. `curl`). |
| `close_port` | — | Close a forward when done (tunnels auto-close ~10 min after creation). |
| `exec_background` | — | Start a long-running command (builds, training, log tails) in the background and get a `task_id` immediately — 24h run cap, 32 tasks per project, records live only in the broker process (a broker restart loses them all). |
| `exec_output` | — | Poll a background task's incremental output (absolute byte-offset cursors per channel, long-poll `wait_seconds`, text/base64 encoding — use base64 for GBK/non-UTF-8 logs). |
| `exec_stop` | — | Stop a background task (returns immediately; kill = session close → remote SIGHUP, so `nohup`'d remote processes survive). |

Every server-touching tool is **profile-gated** (the agent only reaches servers you granted its project) and **audited** (each call logged with project, server, action, status); `exec_output` / `exec_stop` are in-process task operations (no server access, no audit row — stop 触发的终态仍由任务侧落 exec-bg-end 生命周期行). Credential bytes never appear in any tool result.

> **v0.9.0 破坏性变更**：`list_servers` 的 host 默认掩码为 `"hidden"`（`servers edit <name> --expose-host` 逐台放开）；错误文本不再包含主机地址。详见 [docs/compat-matrix.md](docs/compat-matrix.md)。

> **v0.10.0 纯增量**：新增后台任务三件套 `exec_background` / `exec_output` / `exec_stop`（长活命令后台跑、按 offset 轮询增量、停止；任务表在 broker 进程内，重启即失）；前台 `exec_command` 新增 `effective_timeout_seconds` 回显字段（超时钳制从静默改为响式回显）。无破坏性变更——详见 [docs/compat-matrix.md](docs/compat-matrix.md)。

---

## The security model (the iron rule)

- **Credentials** (passwords / private keys) are stored only in an **encrypted vault** — AES-256-GCM with a per-record key derived (HKDF) from a master key. The master key lives in a **fixed-path plaintext file with hardened ACL** (`master.key.plain`, Win `C:\ProgramData\ssh-manager\` / Unix `/var/lib/ssh-manager/`). This is the **L1+ threat model** (Plan 16): protects against same-machine non-privileged process accidental read; does **not** protect against admin/root or offline disk access. Applicable only for single-user, trusted-machine deployments. See [docs/threat-model.md](./docs/threat-model.md).
- The agent authenticates to the MCP with a **project token**, not a credential. The MCP server (the *broker*) holds the master key, opens the SSH connections itself, and returns only command output / file bytes / a forwarded port — **never credentials**.
- The agent's own `ssh` (if it even has a shell) **cannot log in** — there are no creds in `~/.ssh` or `ssh-agent` for it to use, so it's forced through the MCP. A residual-key guardrail warns if stray SSH credential files are detected on the host that could undermine this isolation.
- **Profiles** group servers; a **project** (token) is bound to one profile. The agent sees + reaches only its profile's servers — cross-profile access is rejected by the broker (tested adversarially against a top-tier model).

---

## Quickstart

> Fastest path: the two quickstart docs — **[single-machine](docs/quickstart-single-machine.md)** / **[multi-machine](docs/quickstart-multi-machine.md)**. The inline steps below are the single-machine stdio path.

Build + configure once; then point your AI agent at it.

```bash
# 1. Build — or skip this: grab a prebuilt binary from Releases
#          https://github.com/allanpk716/ssh-manager-mcp/releases
go build -o ssh-manager ./cmd/ssh-manager        # or: go install ./cmd/ssh-manager

# 2. Unlock the vault (writes master key → fixed-path file `master.key.plain`; admin/root needed first time to create the vault dir + set ACL — see `unlock --help`)
ssh-manager unlock

# 3. Add a server + its credential (one of --password / --key, mutually exclusive —
#    or neither for a credential-less server; optional sudo)
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
#   {"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp"],"env":{"SSHMGR_TOKEN":"<TOKEN>"}}}}
```

Drop that snippet into your agent's MCP config (Claude Code: `.mcp.json`; Cursor / other MCP clients: per their setup). The agent now has the ten SSH tools, scoped to the `team-a` profile's servers.

**Other commands:** `servers ls` / `servers rm`, `profiles ls`, `projects ls`, `gc` (find/delete orphan credential rows — dry-run by default), `lock`, `clear` (role teardown — wipes the machine back to first-run), `doctor` (side-effect-free local self-check — prints a PASS/WARN/FAIL report; exit `0` = no FAIL findings, `1` = at least one FAIL), `version`.

**Owner access** (you, not the agent) — full access to every server using the stored creds directly:
```bash
ssh-manager ssh gpu nvidia-smi          # run ONE command (single, non-interactive)
```
The owner path runs a **single non-interactive command** (connect + exec share one 120-second deadline; output is uncapped; a non-zero remote exit makes the CLI exit non-zero (the code value appears in the error message)). No command → explicit error. Interactive shells are intentionally not provided — for a terminal, use your own SSH client with credentials you already hold or provision separately (they may live only in this vault).

---

## Managing servers & projects (owner CLI)

The owner CLI is how you record servers, group them, and grant an agent access. Each server carries structured metadata (`--role` / `--services` / `--location` / `--hardware` / `--special-handling`) plus free-text `--description` and `--tags` — **all shown to the agent** via `list_servers` so it can act on each box safely. ⚠️ Never put secrets in these fields: they enter the agent's context (and the upstream LLM provider) on every call. As a backstop, every metadata write path (add / edit / import / TUI) prints a **non-blocking advisory warning** when a field matches one of 9 known secret shapes (8 token prefixes like `sk-` / `ghp_` / `AKIA` + a tightened PEM rule) — a hint, not a verdict, and it never echoes the content. See [`docs/managing-servers.md`](docs/managing-servers.md).

```bash
# Edit a server in place: pass any subset of fields — the server id + profile bindings are preserved.
ssh-manager servers edit gpu --hardware "8x A100 80GB, CUDA 12"     # structured field (agent-visible)
ssh-manager servers edit gpu --host 192.0.2.20 --port 2222            # re-point host/port
ssh-manager servers edit gpu --password '...'                        # re-credential (or --key)
ssh-manager servers edit gpu --clear-credential                     # strip credentials → credential-less (exclusive action)
ssh-manager servers ls                                                # lists name + role + caveats

# Bulk-import your existing ~/.ssh/config instead of typing servers one by one
ssh-manager servers import --dry-run            # preview: what would be imported / skipped
ssh-manager servers import --profile team-a     # import + grant every imported server in one go

# See what an agent can reach, and manage its lifecycle.
ssh-manager projects show my-agent        # agent → profile → granted servers (no secrets)
ssh-manager projects rotate my-agent      # re-key the token (old token dies; prints a new one + .mcp.json)
ssh-manager projects disable my-agent     # suspend (token rejected until enable)
ssh-manager projects enable  my-agent     # resume
ssh-manager projects revoke  my-agent     # permanent (token rejected; hidden from ls)
ssh-manager projects ls [--all]           # status column; --all includes revoked
```

**Lifecycle:** `rotate` / `disable` / `enable` / `revoke` take effect **per request on a remote serve broker** (the next request is 401-rejected immediately), and **at the agent's next `mcp` spawn in stdio mode** (`VerifyToken` admits only `active` projects — a currently-running local session keeps access until Claude Code restarts its MCP child, by design). Already-open `forward_port` tunnels survive revocation (no owner emergency-stop; broker restart or the ~10-minutes-after-creation reclaim). Offline caches: once a device code is revoked, the device's next pull (≤30min lazy cadence, pinned-401) **destroys its local cache in place** (Plan 34 — DEK + device code + snapshot + meta, quarantine trace left behind); a device that never comes back online can only be cut by rotating the server credentials themselves. Full breakdown: `docs/agent-access.md` 「断连语义（四层）」. `rotate` keeps the same project id + profile; only the token changes. `revoke` is a soft delete — the token is dead and the project is hidden from `ls`, but the audit row is kept. Every lifecycle action is written to the audit log.

**Back up / migrate the whole vault:** `ssh-manager export` / `import` — a portable, passphrase-encrypted file (backup / migration / disaster recovery). Full guide (中文): [`docs/backup-restore.md`](docs/backup-restore.md).

---

## TUI 主控台（`ssh-manager tui`）

一条命令的可视化管理台：在 **broker 机器**上管服务器 / profiles / projects / 设备码（替代手敲 CLI），在 **client 工作机**上可视化配置连接 + 手动同步缓存。同一个二进制，按本机状态自动选边。各页签 / 设备码 / token 谁是谁的**概念模型图解**（仓库隐喻，中文）：[`docs/concepts.md`](docs/concepts.md)。

空机器第一次运行 `tui` 会进入**角色向导**（单机 / server / client 三选，可中断续配）；`ssh-manager clear` 角色清理——**按实际存在枚举**删除（与 role.json 声明的角色无关）本机 vault / serve / 缓存残留（vault 角色先自动 export 备份 + 输入 `DELETE` 确认），机器回到首次向导状态。

### 启动与模式判定

```bash
ssh-manager tui                # 自动判定
ssh-manager tui --mode client  # 强制 client 面板
```

> v0.7.0 起 `tui --mode broker` 移除（自动判定覆盖该场景；`--mode client` 保留）。

自动判定规则：

- 本机有**已解锁的 vault** → broker 主控台；
- 本机有**缓存**（`cache.bin`）→ client 面板；
- vault 存在但锁着 / 两者都没有 → **引导性报错**（告诉你该 `unlock` 还是 `--mode client`）——**绝不静默降级到 client**。

Broker 主控台（服务器 / Profiles / Projects / 设备码 4 个页签）与 client 面板（服务器列表只读、零远程写）的操作语义与 owner CLI 完全一致——TUI 只是同一套 vault 操作的另一个入口，做完的事在 `ls` / 审计里看到的一样。各页签键位与典型任务走查见 [docs/tui-single-machine.md](docs/tui-single-machine.md) / [docs/tui-multi-machine.md](docs/tui-multi-machine.md)。

### 终端要求（mintty 注意）

Windows Terminal / cmd 原生可用。**mintty**（Git Bash 默认终端）不是 Windows 控制台，需 `winpty ssh-manager tui`；在非 TTY 下启动时程序会**直接报错提示**，不会挂死或乱码。

### 安全面

- **凭据永不回显**：密码 / key / sudo 密码的输入框全程掩码；已设凭据只显示「已设置」，输入新值即更换，不为确认而回显旧值。
- **token / 设备码一次性展示**：全屏显示一次，关闭后不可再查（与 CLI 的 shown-once 语义一致）。
- **client 零远程写**：client 模式的任何按键都不会写 broker。

---

## Multi-machine: `serve` mode (remote agents on a VLAN)

> **Quickstart:** [`docs/quickstart-multi-machine.md`](docs/quickstart-multi-machine.md) · **Full guide (中文):** [`docs/multi-machine.md`](docs/multi-machine.md)

By default the broker runs **in-process** inside the MCP server the agent spawns (no daemon). For **several machines sharing one authoritative vault** — e.g. you work across multiple boxes on a home/VLAN network — run the broker as a small HTTP server on one trusted host and point the other machines' agents at it.

```bash
# On the trusted VLAN host (the authoritative broker):
ssh-manager serve --addr 0.0.0.0:7878
# → ssh-manager serve: listening on 0.0.0.0:7878 (tls=auto)
# → auto-TLS cert (self-signed). client pin: sha256:...
```

- **Auto-TLS + fingerprint pinning (no cert hassle).** On first start `serve` **auto-generates a self-signed ed25519 cert** and forces TLS from then on — no openssl, no CA distribution. `cache-tokens add` prints the cert's **SPKI fingerprint** alongside the device code; `cache pull` **pins** it (first-connect verification, zero MITM window — the HPKP/Tailscale model). This is the default; pass `--tls-cert`/`--tls-key` only if you want your own cert.
- **No-pin clients refuse by default.** A `cache pull` without a pin **hard-fails** (was: silent plaintext fallback — a fail-open risk, now closed). Opt into plaintext explicitly with `--allow-plaintext` (debugging / talking to an old plaintext serve only). A pin set with a non-`https://` URL also hard-fails.
- **Auth — same gate as stdio.** Every request carries `Authorization: Bearer <project-token>`. The server resolves it per request with `VerifyToken` (`active` projects only); the iron rule (per-call `serverID ∈ profileID`) applies identically.
- **Point the remote agent at it** — streamable-HTTP endpoint `https://<host>:7878/` with the bearer header. Claude Code `.mcp.json` (online live mode):
  ```json
  {"mcpServers":{"ssh":{"type":"http","url":"https://192.0.2.5:7878/","headers":{"Authorization":"Bearer <TOKEN>"}}}}
  ```
  Or run the broker from a local **read-only cache** (`mcp --cache`) so the agent keeps working offline — see the quickstart.
  > client 角色向导的 finish 屏现在会同时展示离线 --cache 与在线 http 两种形态。
- **Shutdown.** `Ctrl+C` (`SIGINT`) / `SIGTERM` → graceful drain and every open `forward_port` tunnel torn down.

> **⚠️ Breaking change / migration order.** New `serve` is TLS-only. When upgrading an already-deployed plaintext setup: **upgrade all work-machine binaries + configure their pin FIRST, restart `serve` LAST** — the moment `serve` upgrades it rejects old plaintext clients. Full migration + key-rotation runbooks in [`docs/multi-machine.md`](docs/multi-machine.md).

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

**Plan 10 — `serve` mode:** run the broker as an authenticated HTTP MCP server so agents on other VLAN machines share one authoritative vault. Phase 1 of multi-machine support (remote live access). **Plan 12 — offline read-only cache:** each work machine pulls an encrypted read-only snapshot of the vault (`cache pull`) and can run the broker from it (`mcp --cache`) when the server is unreachable. Vault replication and Synology backup are later phases. See "Multi-machine: `serve` mode" above and [`docs/multi-machine.md`](docs/multi-machine.md#离线只读缓存plan-12).

**v0.7.0 (Plan 20) — ssh-config import & hygiene hardening:**

- **`servers import`** — bulk-import `~/.ssh/config` hosts (CLI `--file` / `--dry-run` / `--profile`; TUI server page `i`): vault-conflict skips make it idempotent, identical key files within one batch mint **one** credential row, encrypted keys import as-is with a needs-passphrase ⚠, and the TUI walks a per-server supplement loop after import. See [`docs/managing-servers.md`](docs/managing-servers.md#批量导入servers-import).
- **Credential-less server model** — `servers add` / import may record a server with **no** credential (`--password` / `--key` optional, still mutually exclusive); `exec_command` on such a server fails **before** connecting with an error carrying the `servers edit` fix hint (audit `no_credential`); TUI flags them ⚠ (filter with `!`).
- **`SSHMGR_TOKEN` env channel** — the project token can now ride the environment instead of argv (flag parity: `--token` still works); every `.mcp.json` generator (`projects add` / `rotate`, TUI, wizard) now emits the `env` form. **Snippet shape changed**: previously copied `--token`-in-`args` snippets keep working, but regenerate when you next rotate.
- **`gc`** — find (and with `--apply`, delete) credential rows no server references via either column; dry-run by default; servers, host keys, and cache tokens are never touched.
- **Tunnel half-close** — `forward_port` tunnels propagate a directional EOF (`CloseWrite`) instead of tearing down both directions, so half-closing protocols (e.g. `shutdown(SHUT_WR)`) no longer kill the peer's reply.
- **`SSHMGR_SSH_HOST_KEY_ALGORITHMS` knob** — opt into legacy host-key algorithms (`ssh-rsa`, …) for old boxes; typo = fail-closed before any connect. See [`docs/managing-servers.md`](docs/managing-servers.md#连老机器需要-ssh-rsa).
- **Single build-version source** — CLI `version` and the MCP `serverInfo` now report the same ldflags-injected version.

Design spec: [`docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md`](docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md).
