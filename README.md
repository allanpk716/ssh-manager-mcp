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
| 一屏看全部署形态（单机 / 多机部署 + 管理面）怎么选 | [`docs/deployment-modes.md`](docs/deployment-modes.md) |
| 从零到跑通（安装 / 解锁 / 第一台服务器 / 授权 Claude Code） | [`docs/getting-started.md`](docs/getting-started.md) |
| 新增 / 编辑 / 维护 / 删除服务器 | [`docs/managing-servers.md`](docs/managing-servers.md) |
| 授权 Claude Code / Cursor / 其他 agent；token 轮换与吊销 | [`docs/agent-access.md`](docs/agent-access.md) |
| 多台机器共用一份 vault（`pair` 一条龙入网 + 离线只读缓存） | [`docs/multi-machine.md`](docs/multi-machine.md) |
| **在 broker（serve）主机上也跑 agent**（零距离 client 走桥 + 应急附录） | [`docs/broker-host-agent.md`](docs/broker-host-agent.md) |
| 应用场景与示例（GPU 巡检、读 root 日志、部署、端口转发……） | [`docs/scenarios.md`](docs/scenarios.md) |
| 备份 / 迁移整个 vault（export / import） | [`docs/backup-restore.md`](docs/backup-restore.md) |
| **单机 Web UI 教程**（浏览器表单操作，逐步附命令行等价命令） | [`docs/web-ui-single-machine.md`](docs/web-ui-single-machine.md) |
| **联机 Web UI 教程**（权威端管理页面 + 客户端页面 + Pairing 批准页） | [`docs/web-ui-multi-machine.md`](docs/web-ui-multi-machine.md) |
| **给 AI agent 的工具手册**（可贴进 CLAUDE.md 的规则模板在内） | [`docs/agent-tools.md`](docs/agent-tools.md) |

---

## What the agent gets (the MCP tools)

The MCP server exposes these tools — **ssh-functional-equivalent for operating a server** (interactive shell is intentionally not provided):

| Tool | Like | What it does |
|---|---|---|
| `list_servers` | — | List the servers the agent may use (`id` / `name` / `host` (`"hidden"` by default; owner opts in per server) / `user` / `has_sudo`, plus owner-provided context: role, services, location, hardware, caveats, tags, description, and topology registration fields `device` / `site` / `networks`). Topology fields are registration only and do not mean a server is currently reachable. Always call first — the agent learns real server ids here. Never includes credentials or topology addresses. |
| `query_topology` | — | Read-only neighborhood view from one registered origin. `from` accepts `self` (the device of the current sshmgr instance) or an authorized server id. The result lists the origin's networks, then neighbors in physical local-area networks, virtual-local-area-network-only neighbors, unreachable devices, and unregistered authorized servers. Each reachable line also shows the latest locally stored probe observation; the query itself never probes, opens SSH, writes audit records, or changes the snapshot revision. Alternatively, `from` may be an authorized server id and `to` exactly one SMB shared address (`\\server\share\path` or `smb://server/share/path`); the tool returns that server's agreed local path and whether it may mount the shared storage, without exposing mount addresses or credentials. |
| `list_secrets` | — | Read-only discovery of secrets the agent may reference. Each compact line carries only the secret name, note, and the server names it may write to within the agent's authorization; values, value metadata, timestamps, and out-of-scope servers never appear. Optional `query` and `server` filters are combined when both are given. The future reference syntax is `{{sshmgr-secret:NAME}}`; this version does not provide writing secret values. |
| `list_shares` | — | Read-only list of the registered SMB shared storages visible in the session, with the agreed per-server mount points (PowerShell/cmd servers use the fixed drive letter like `Z:`, others use `/mnt/sshmgr/<share name>`). Each share carries its name, provider device, owner note, directory agreements, drive letter, registration version, change note, and the in-scope servers that may mount it. By default, mount points are registration agreements only and the tool performs no network or SSH action; optional `live_check` runs read-only Linux/macOS mount checks (Windows is reported as not checked), and `check_server` narrows that check to one server. It never includes mount passwords, and no agent tool can register or modify shared storages. |
| `mount_share` | — | Persistently mount one registered SMB shared storage on a Linux or macOS server. sshmgr selects a candidate address (physical local-area-network candidates first), writes the persistent configuration and mount password through its privileged fixed command path, and verifies the mount. The call is idempotent and never returns addresses or credentials. If a revocation marker is at least as new as the snapshot's shared-storage version, the call refuses to remount; after reauthorization, a successful mount removes that marker. The tool never unmounts, revokes, or reselects an address — those are owner CLI operations. Windows servers are rejected because command-session drive injection already covers them. |
| `exec_command` | `ssh host cmd` | Run a shell command on a server. On a POSIX, PowerShell, or cmd server's normal shell layer, owner-authorized secret environment variables are exported before your command, so you can reference `$NAME`, `$env:NAME`, or `%NAME%` respectively; values never enter the command text, policy input, audit text, or errors. `sudo=true` runs `sudo -S` for you (do **not** prepend `sudo` yourself) — the **whole** line, every `;`/`&&`/`\|` segment, executes elevated (v0.10.1+; before that only the first segment did — see compat-matrix). The result carries `sudo: {outcome, uid}`: elevation is observable, and the did-NOT-run outcomes (`auth-failed` / `sudo-start-failed` / `wrap-failed`) come back as tool errors. Command text is never rewritten; use `query_topology` to translate a shared address to the server-local path first. |
| `download_file` | `scp host:path .` | Download a remote file (size-capped; truncated output is flagged). |
| `upload_file` | `scp -r . host:path` | Upload a local file **or directory** (recursive) to the server. |
| `upload_content` | — | Write inline content (a string the agent holds) to a remote file — the **cross-machine** upload path (`upload_file` reads the broker's own filesystem; a remote-serve agent pushes its own configs/scripts here). `text` / `base64` (byte-exact) encodings, 8 MiB decoded cap, parent dirs auto-created, existing file overwritten. |
| `exec_context` | — | Capture the exec channel's TRUE context in one round: uid/gid/groups, tty, uid_map, LSM label, SSH provenance, process tree — call it BEFORE hypothesizing about identities or "mystery permission denied" (e.g. when sudo returns uid=0 yet a path stays EACCES). |
| `query_software_inventory` | — | Read-only query of the software inventory: per device, software items with exact versions, listening ports with exposure levels, source, collection time, and the device's collection status (including whether its inventory is stale). Five optional filters: device, name (case-insensitive substring), minimum exposure level, category, include system packages (default false — package lists can be huge). Visibility follows your profile; unmanaged devices never appear. On a cache client this queries the authority online and never falls back to local data. |
| `draft_software_declarations` | — | Submit one or more software declaration drafts. A draft may add software the collector cannot see, claim an observed item or unidentified process with `claimed_observed_key`, and provide declaration-only exposure levels. The reply returns one draft identifier per input; nothing affects queries or follow-ups until the owner confirms it. No tool can confirm, reject, or edit effective declarations. On a cache client this is an auditable forwarded write to the authority. |
| `collect_software_inventory` | — | Request immediate collection for one profile-visible device. If the authority (or standalone instance) is the collector, it runs the fixed read-only command set and returns the report; if a client instance is the collector, it only registers the immediate-collection request for that instance. On a cache client the tool forwards through the authority and fails closed when the authority is unreachable. |
| `forward_port` | `ssh -L` | Open a local port forwarding to a remote service — returns `<listen_host>:<port>` (default `127.0.0.1`; non-loopback listen hosts are rejected fail-closed — multi-machine clients are loopback-only by design). |
| `close_port` | — | Close a forward when done (idle tunnels auto-close after ~10 min of no traffic; the owner can emergency-kill any tunnel, and revoke/disable tears open tunnels down within ~15s). |
| `exec_background` | — | Start a long-running command (builds, training, log tails) in the background and get a `task_id` immediately — 24h run cap, 32 tasks per project, records live only in the broker process (a broker restart loses them all). Command text is never rewritten; use `query_topology` to translate a shared address to the server-local path first. |
| `exec_output` | — | Poll a background task's incremental output (absolute byte-offset cursors per channel, long-poll `wait_seconds`, text/base64 encoding — use base64 for GBK/non-UTF-8 logs). |
| `exec_stop` | — | Stop a background task (returns immediately; kill = session close → remote SIGHUP, so `nohup`'d remote processes survive). |
| `relay_file` | — (Broker-specific — `scp serverA:file serverB:` approximates the shape but is two direct connections with credentials on both ends; the broker proxies with vault-held credentials the agent never sees) | Relay a LARGE file server-to-server through the broker, or from the broker's own disk to a server — the zero-context big-file path (file bytes stream through the broker's memory only; results carry metadata only). Resumable background transfer: returns `task_id` for the `exec_output`/`exec_stop` trio; an interrupted transfer leaves a partial + manifest on the destination and re-running with the same paths completes only the missing chunks; `fresh=true` restarts. Digest verification recipes in the tool description; destination sftp must be OpenSSH-family (`posix-rename@openssh.com` hard dependency). |
| `reload_self` | — (bridge self-upgrade, not a server tool) | The stdio bridge's own hot-upgrade: reports this bridge process's version status (current / on-disk generation written by `sshmgr update` / latest GitHub release — one live query per call) and, when the disk holds a newer generation (upgrade or downgrade alike — the trigger is the generation counter, never a version comparison) and the bridge is idle, hot-swaps into the new binary without dropping the MCP session (busy → declined with the active list; successor fails to start → old bridge keeps serving, reason returned). Also fires on its own every ~30 s when idle. stdio bridge faces only (`sshmgr mcp` / `mcp --cache`) — the remote serve face does not carry it. Behavior details: [docs/agent-access.md](docs/agent-access.md#桥热升级reload_self). |

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

Build once; then point your AI agent at it. Prefer a browser? `sshmgr ui` opens the
first-run wizard (choose this machine's role, create the vault) and then the regular
management pages — every wizard step has the command-line equivalent shown below.

```bash
# 1. Build — or skip this: grab a prebuilt binary from Releases
#          https://github.com/allanpk716/ssh-manager-mcp/releases
go build -o sshmgr ./cmd/sshmgr        # or: go install ./cmd/sshmgr

# 2. Initialize this machine as standalone (writes role.json + master key + vault; also reachable via `sshmgr ui` → 单机)
sshmgr init --role standalone

# 3. Add a server + its credential (one of --password / --key, mutually exclusive —
#    or neither for a credential-less server; optional sudo)
sshmgr servers add --name gpu --host 192.0.2.10 --user deploy \
    --password '...'                 # OR: --key ~/.ssh/id_ed25519 [--key-passphrase '...]
    --sudo-password '...'            # optional: enables sudo=true on exec_command

# 4. Create a profile + grant the server to it
sshmgr profiles add team-a
sshmgr profiles grant team-a gpu

# 5. Create a project — prints a ONE-TIME token + the .mcp.json snippet
sshmgr projects add my-agent --profile team-a
#   Token (shown once): <TOKEN>
#   .mcp.json snippet:
#   {"mcpServers":{"ssh":{"command":"sshmgr","args":["mcp"],"env":{"SSHMGR_TOKEN":"<TOKEN>"}}}}
```

Drop that snippet into your agent's MCP config (Claude Code: `.mcp.json`; Cursor / other MCP clients: per their setup). The agent now has the thirteen SSH tools plus the bridge self-upgrade tool `reload_self` (see [docs/agent-access.md](docs/agent-access.md)), scoped to the `team-a` profile's servers.

**Settings and role inspection:** `settings ls` / `settings get <key>` / `settings set <key> <value>` / `settings unset <key>` manage the known vault settings with the same validation as the Web UI settings page; `role show` displays this machine's role, setup state, and `role.json` path. Every settings change writes a `setting.set` or `setting.unset` audit row.

**Other commands:** `servers ls` / `servers rm`, `profiles ls`, `projects ls`, `gc` (find/delete orphan credential rows — dry-run by default), `lock`, `clear` (role teardown — wipes the machine back to first-run), `doctor` (local self-check — prints a PASS/WARN/FAIL report; the vault decrypt probe first WAL-checkpoints the store via a bare keyless connection so its byte-copy is complete; exit `0` = no FAIL findings, `1` = at least one FAIL), `policy` (command policy — the [ADR 0007](docs/adr/0007-command-policy-client-enforced-guardrail.md) guardrail that judges every agent-issued command *before* any connection is made (block / confirmation-required / allow; a backstop against catastrophic agent mistakes, not a security boundary): `policy ls` lists the rules in effect, `add` / `rm` manage custom rules, `disable` / `enable` toggle built-in default rules by id, `policy test --shell posix "…"` dry-runs the judgement — full-function on the authoritative machine, read-only listing/testing on cache clients, with `--instance <name>` to pick a named cache instance), `version`, `pair` (multi-machine one-shot enrollment — LAN discovery → SAS pairing → credentials delivered; see "Multi-machine" below). Tunnel governance (owner, on the machine holding the vault): `tunnels ls` / `tunnels kill <tunnel_id>` / `tunnels kill --project <name>` (emergency stop for live tunnels — teardown within one ~15s control tick; `kill` is surgical and does not revoke the token, `--project` only tears down what exists now — use `projects disable/revoke` to stop re-opening). Audit forensics (owner, on the machine holding the vault): `audit` reads the vault audit log, newest first (owner-only) — `--since 30m|7d|RFC3339|date`, `--server/--project <name|id>`, `--owner`, `--action/--status`, `--source/--source-ip`, `--before <audit id>`, `--limit` (0 = all), `--json` for JSONL.

**Owner access** (you, not the agent) — full access to every server using the stored creds directly:
```bash
sshmgr ssh gpu nvidia-smi          # run ONE command (single, non-interactive)
```
The owner path runs a **single non-interactive command** (connect + exec share one 120-second deadline; output is uncapped; a non-zero remote exit makes the CLI exit non-zero (the code value appears in the error message)). No command → explicit error. Interactive shells are intentionally not provided — for a terminal, use your own SSH client with credentials you already hold or provision separately (they may live only in this vault).

```bash
# Audit forensics — what every agent (and owner) did, newest first
sshmgr audit --since 24h --project my-agent --status error
sshmgr audit --json --limit 0 > audit.jsonl   # twelve fields, sidecar-compatible
# On a client machine: the local audit sidecar next to one cache instance
sshmgr cache audit --instance laptop           # newest first; --limit / --json
```

Known filter values: `action` = `exec` / `download` / `upload` / `upload-content` / `forward` / `close-forward` / `exec-bg-start` / `exec-bg-end` / `relay-bg-start` / `relay-bg-end` / `exec-blocked` / `exec-confirm-required` / `exec-confirmed` / `exec-bg-blocked` / `exec-bg-confirm-required` / `exec-bg-confirmed` (command-policy verdicts on agent-issued commands) + owner-side `project.add` / `project.rotate` / `project.disable` / `project.enable` / `project.revoke` / `project.delete` / `policy-rule.add` / `policy-rule.rm` / `policy-rule.disable` / `policy-rule.enable`; `status` = `ok` / `error` / `timeout` / `cancelled` / `denied` / `auth_error` / `hostkey_mismatch` / `connect_error` / `no_credential` / `no_sudo` / `bind_denied` (command-policy rows `exec-blocked` / `exec-confirm-required` / `exec-confirmed` and their `exec-bg-*` twins carry the hit **rule id** in the `status` column instead). The sets evolve across versions; unknown filter values silently match nothing (empty result, never an error).

Human-mode output notes: the server / project columns render four ways — the entity's name, `(none)` (no server context, e.g. project-level owner actions), `(owner)` (no project = an owner action), or `id…(deleted)` for a row that has outlived its entity. Dynamic text (commands, entity names) is escape-notated for the terminal: control / invisible characters render as `\n`, `\x1b`, `\u202e`-style sequences, and a literal backslash becomes `\\` (the notation is reversible); seeing a bidi escape such as `\u202e` means the original command carried a direction override — don't trust the visual order of that line. `exit=0` is ambiguous by design: a failure that never produced an exit code (connect-time) and a command that ran and exited 0 both display `exit=0` — read the `status` column to tell them apart. `--limit 0` prints a stderr warning that `audit_log` has no auto-cleanup and the output may be large; when rows are truncated, stderr gets `showing first N rows (more exist) — use --limit 0 for full output` (JSON mode emits rows only, no stderr chatter).

---

## Managing servers & projects (owner CLI)

The owner CLI is how you record servers, group them, and grant an agent access. Each server carries structured metadata (`--role` / `--services` / `--location` / `--hardware` / `--special-handling`) plus free-text `--description` and `--tags` — **all shown to the agent** via `list_servers` so it can act on each box safely. ⚠️ Never put secrets in these fields: they enter the agent's context (and the upstream LLM provider) on every call. As a backstop, every metadata write path (add / edit / import / Web UI) prints a **non-blocking advisory warning** when a field matches one of 9 known secret shapes (8 token prefixes like `sk-` / `ghp_` / `AKIA` + a tightened PEM rule) — a hint, not a verdict, and it never echoes the content. See [`docs/managing-servers.md`](docs/managing-servers.md).

```bash
# Edit a server in place: pass any subset of fields — the server id + profile bindings are preserved.
sshmgr servers edit gpu --hardware "8x A100 80GB, CUDA 12"     # structured field (agent-visible)
sshmgr servers edit gpu --host 192.0.2.20 --port 2222            # re-point host/port
sshmgr servers edit gpu --password '...'                        # re-credential (or --key)
sshmgr servers edit gpu --clear-credential                     # strip credentials → credential-less (exclusive action)
sshmgr servers ls                                                # lists name + role + caveats

# Bulk-import your existing ~/.ssh/config instead of typing servers one by one
sshmgr servers import --dry-run            # preview: what would be imported / skipped
sshmgr servers import --profile team-a     # import + grant every imported server in one go

# See what an agent can reach, and manage its lifecycle.
sshmgr projects show my-agent        # agent → profile → granted servers (no secrets)
sshmgr projects rotate my-agent      # re-key the token (old token dies; prints a new one + .mcp.json)
sshmgr projects disable my-agent     # suspend (token rejected until enable)
sshmgr projects enable  my-agent     # resume
sshmgr projects revoke  my-agent     # permanent (token rejected; hidden from ls)
sshmgr projects ls [--all]           # status column; --all includes revoked
```

**Lifecycle:** `rotate` / `disable` / `enable` / `revoke` take effect **at the agent's next `mcp` spawn in stdio mode** (`VerifyToken` admits only `active` projects — a currently-running local session keeps access until Claude Code restarts its MCP child, by design); on multi-machine clients a revoked project disappears with the next snapshot refresh (≤30min) and a revoked device code triggers local cache destruction on the next pull (pinned-401 quarantine). Already-open `forward_port` tunnels are torn down within ~15s of `disable`/`revoke` (one control tick — Plan 35; the owner also has the `tunnels kill` emergency stop). Offline caches: once a device code is revoked, the device's next pull (≤30min lazy cadence, pinned-401) **destroys its local cache in place** (Plan 34 — DEK + device code + snapshot + meta, quarantine trace left behind); a device that never comes back online is cut by the `max_offline` hard cap (pair-issued default 24h) and can only be fully cut by rotating the server credentials themselves. Full breakdown: `docs/agent-access.md` 断连语义 + `docs/agent-tools.md` 吊销三路径. `rotate` keeps the same project id + profile; only the token changes. `revoke` is a soft delete — the token is dead and the project is hidden from `ls`, but the audit row is kept. Every lifecycle action is written to the audit log.

### Shared storages (`shares` owner CLI)

Shared-storage mounts are owner-managed. The mount tool available to agents can mount or update a mount, but cannot unmount it, revoke permission, or reselect its address. The commands below require this machine to hold the unlocked writable vault (standalone or the authoritative machine); a cache client is rejected.

```bash
sshmgr shares unmount datasets linux-web --yes            # remove persistent config, then unmount
sshmgr shares revoke datasets linux-web --unmount --yes   # revoke permission, clean config, unmount
sshmgr shares revoke datasets linux-web --unmount \
  --new-password-from-file <path-to-new-password-file> --yes
sshmgr shares reselect datasets linux-web --yes           # prefer a reachable physical local-area-network address again
sshmgr shares residue datasets                            # read-only leftover check for one share
sshmgr shares residue                                     # read-only leftover check for every share
```

- `unmount` affects Linux and macOS only. It deletes the sshmgr credential/configuration entry first. On macOS it reloads autofs immediately after deleting the map entry, then attempts to unmount `/mnt/sshmgr/<share>`; on Linux it unmounts after deleting `/etc/fstab` and the credential file. If no configuration entry exists, it still cleans a leftover credential file, configuration entry, or mount for that share; only an entry, credential, and mount that are all absent are reported as not managed. If processes keep the mount busy, their process IDs, users, and command names are listed and the mount is preserved; add `--force` to force unmounting. If the server is still in the share's mountable range, an agent may mount it again — revoke permission for a durable removal. If the server is out of range or the registration was deleted, sshmgr writes a secret-free revocation marker.
- `revoke` first removes the server from the mount password secret's writable-server list in one transaction. After that commit it attempts to remove Linux/macOS persistent configuration and write the revocation marker; a server that was never mounted also receives the marker. `--unmount` additionally unmounts (again with busy-process reporting and optional `--force`). Connection failure is reported immediately with a structured failed result and a non-zero exit status, but the registration change remains and the command still prints the `shares residue` / `shares unmount` follow-up and all password-change reminders. A new password may be supplied only from standard input (`--new-password-stdin`) or a file (`--new-password-from-file`); changing it in the same transaction bumps the shared-storage version once and prints which remaining Linux/macOS servers need an agent-driven `mount_share` update.
- Revoking on Windows makes no SSH connection. Clients stop injecting the registered drive letter after snapshot synchronization (online clients by the next default refresh within five minutes; an executing command keeps its injected drive until it finishes; a long-offline client keeps injecting until it synchronizes). Stopping injection is a guardrail, not a security boundary — changing the shared-storage mount password is the real revocation action, and manually added desktop connections also need the new password.
- `reselect` works only on a Linux/macOS server that remains mountable. It evaluates candidates exactly as `mount_share` does (physical local-area network first, registration order for ties, Server Message Block TCP port 445 reachability from the target). It reports “best address unchanged” when no change is needed, preserves the old address/configuration/mount when the mount is busy, and uses `--force` to unmount forcibly and mount again.
- `residue` is read-only and covers every Linux/macOS server entry in the vault (at most four servers concurrently). It reports credentials, configuration, mounts, and revocation markers for shares that are unregistered or no longer mountable on that server, and prints the full `shares unmount` cleanup command for each actual leftover. A marker-only result is the expected state after revocation; unreachable servers are listed as unchecked, and Windows servers are skipped because sshmgr leaves nothing persistent there.
- `shares rm` removes only the registration and does not contact servers. Afterward, use `shares residue` and `shares unmount` to find and clean leftovers. Deleting the provider device through `devices rm` performs the remote revocation step for listed Linux/macOS mountable servers whose entries survive the deletion (without forcing); Windows servers stop injection after snapshot synchronization. A server entry deleted together with the provider device is marked “not cleaned” because it can no longer be dialed — the preview tells the owner to run `shares unmount <share> <server>` first if cleanup is needed. Remote failures do not roll back the registration deletion.

**Back up / migrate the whole vault:** `sshmgr export` / `import` — a portable, passphrase-encrypted file (backup / migration / disaster recovery). Full guide (中文): [`docs/backup-restore.md`](docs/backup-restore.md).

---

## Web 管理界面（`sshmgr ui`）

一条命令的浏览器管理界面：初始化向导、服务器 / 访问授权 / 机密项 / 运维页面、权威端**配对批准**都在里面。同一个二进制，按本机状态自动决定去向；每一步人工操作都有命令行等价面。各页面 / 设备码 / token 谁是谁的**概念模型图解**（仓库隐喻，中文）：[`docs/concepts.md`](docs/concepts.md)。

### 首次初始化（空机器）

```bash
sshmgr ui
```

空机器运行后，会在本机回环地址的随机端口启动**初始化向导**（终端打印带一次性启动令牌的网址并自动打开浏览器；只监听 `127.0.0.1`，命令退出即停）。第一步选角色：**单机**（一台机器自持 vault） / **权威端**（多机部署里常驻权威 vault） / **客户端**（只持本地只读快照）。

- 选**单机**：向导只建 vault（随机主密钥加密钥文件），显示主密钥文件位置与备份提醒，然后**直接进入单机正式页面**；首页给「录服务器 → 建 Profile 并授权 → 建项目并取令牌」下一步引导，全部完成后消失。
- 选**权威端**：向导按「建 vault → 设管理员口令 → 安装 serve」三步走。安装页按本机操作系统显示**完整提权命令**（含 sshmgr 可执行文件绝对路径）和复制按钮；页面每 2 秒自动检查 serve，检测到运行后显示正式 Web UI 全部地址，`sshmgr ui` 随即退出并在终端打印这些地址。
- 选**客户端**：页面显示 `sshmgr pair` 的命令写法与「到权威端 Web UI 的配对页批准」说明，随后 `sshmgr ui` 退出（不在向导里做配对、不写 `role.json`）。

每步的命令行等价命令（无显示器机器 / agent 用）：

| 向导步骤 | 命令行 |
|---|---|
| 选角色并建 vault（单机 / 权威端；对已有 vault 的单机运行 `--role server` 即升级为权威端，vault 内容不动） | `sshmgr init --role standalone` / `sshmgr init --role server` |
| 设管理员口令 | `sshmgr serve admin set` |
| 安装 serve | Linux / macOS：`sudo <sshmgr 绝对路径> serve install --addr 0.0.0.0:7878`；Windows：在管理员 PowerShell 中运行 `& "<sshmgr 绝对路径>" serve install --addr 0.0.0.0:7878` |
| 客户端入网 | `sshmgr pair --instance <名字>`（在权威端 Web UI 配对页批准） |

中途关掉浏览器是安全暂停：再次运行 `sshmgr ui` 从未完成的步骤继续。单机正式页面的运维区另有「升级为权威端」入口（等价于 `sshmgr init --role server`，之后按提示设管理员口令并安装 serve）。

### 已初始化机器的去向

- **单机 / 客户端**：`sshmgr ui` 在本机回环地址的随机端口启动正式页面（一次性启动令牌换取会话；命令退出即停）。单机全功能可写；客户端只读查看本地快照（含连接状态、快照修订号与推荐版本），外加立即同步、切换缓存实例，修改 vault 一律到权威端 Web UI 做。
- **权威端**：正式 Web UI 由 `serve` 进程的独立 HTTPS 监听提供（管理员口令登录；尚未设口令时登录页提示在权威端主机运行 `sshmgr serve admin set`）。已初始化的权威端运行 `sshmgr ui` 不另起监听，只打印 Web UI 地址与状态。

### 旧命令 `sshmgr tui`

TUI 已删除。`sshmgr tui` 保留为隐藏命令：不论带什么参数，只打印「TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`」并以退出码 1 结束，不打开浏览器、不监听端口。

### 安全面

- **回环模式只监听 `127.0.0.1`**：单机与客户端的页面（含初始化向导）不出本机，入口网址带一次性启动令牌，并沿用 Host 检查、来源检查与 CSRF 校验。
- **凭据永不回显**：密码 / key / sudo 密码的输入框全程掩码；已设凭据只显示「已设置」，输入新值即更换，不为确认而回显旧值。
- **token / 设备码一次性展示**：显示一次，关闭后不可再查（与 CLI 的 shown-once 语义一致）。
- **客户端零远程写**：客户端页面的任何操作都不会写权威端。

页面走查与典型任务见 [docs/web-ui-single-machine.md](docs/web-ui-single-machine.md) / [docs/web-ui-multi-machine.md](docs/web-ui-multi-machine.md)。

---

## Multi-machine: bridge posture (on a VLAN)

> **Quickstart:** [`docs/quickstart-multi-machine.md`](docs/quickstart-multi-machine.md) · **Full guide (中文):** [`docs/multi-machine.md`](docs/multi-machine.md) · **Mode overview:** [`docs/deployment-modes.md`](docs/deployment-modes.md)

By default the broker runs **in-process** inside the MCP server the agent spawns (no daemon). For **several machines sharing one authoritative vault** — e.g. you work across multiple boxes on a home/VLAN network — run the authoritative vault as a resident service on one trusted host and enroll each work machine with **`sshmgr pair`** (one command: LAN discovery → SAS pairing → credentials delivered → first pull → ready-to-paste `.mcp.json` artifact).

```bash
# On the trusted VLAN host (the authoritative vault):
sshmgr serve --addr 0.0.0.0:7878
# → sshmgr serve: listening on 0.0.0.0:7878 (tls=auto)
# → auto-TLS cert (self-signed). client pin: sha256:...
# → sshmgr serve: discovery: udp/7878 (on)

# On each work machine (after installing the binary):
sshmgr pair --instance laptop
# → owner approves in the authoritative Web UI Pairing page (or `serve pair approve laptop --profile team-a`)
# → done: agent runs from a local read-only cache
```

- **The serve is narrowed to: authoritative vault + `/snapshot` + `/pair` + the Web UI.** There is **no remote MCP surface** (the old `"type": "http"` + bearer-header direct connection was **removed** in Plan 42 批1 — non-`/snapshot`//`/pair` paths answer 404). Work-machine agents always run **locally from a read-only cache** (`mcp --cache`) and dial target servers themselves; writes (add/edit servers, approvals) belong to the **management plane**: Web UI / `serve pair` command line.
- **Auto-TLS + fingerprint pinning (no cert hassle).** On first start `serve` **auto-generates a self-signed ed25519 cert** and forces TLS from then on — no openssl, no CA distribution, no trust-store config. The cert's **SPKI fingerprint** rides the discovery offer and the sealed pairing envelope automatically; `cache pull` pins it too (first-connect verification, zero MITM window — the HPKP/Tailscale model). Pass `--tls-cert`/`--tls-key` only if you want your own cert.
- **Pairing is human-gated.** The client shows `name @ url SAS <6 digits>`; the approver compares the client-screen SAS against the approval row's name@url in the Web UI Pairing page (or with `serve pair approve`), while the serve **mechanically verifies** that the client's declared target address is actually this machine (mismatch → ⚠ + explicit override required). A direct `--url` pairing without a pin **refuses by default** (`--allow-tofu` is the explicit, unanchored escape hatch — see threat-model R12). No-pin `cache pull` hard-fails the same way; `--allow-plaintext` is the debug-only opt-out.
- **Several agents on ONE machine — named cache instances (Plan 40; lifecycle management Plan 46).** Each agent gets its own offline cache: enroll with `sshmgr pair --instance <name>` (one device code per agent). Inspect every slot with `sshmgr cache status` or `sshmgr cache instances ls` (per-row artifact/DEK existence + age, half-state and DEK-orphan slots flagged), switch slots in the client Web UI (per session, `sshmgr ui --instance <name>`), **remove an instance** with `sshmgr cache instances rm <name>` (double-root cleanup: slot dir + per-instance DEK; typed-name confirm; idempotent — the broker-side device code revoke stays a separate, manual step), and persist each instance's offline cap independently with `sshmgr cache config [--instance <name>] --max-offline 24h` (priority env > file > off). Instances live under `instances/<name>/` with a **per-instance DEK** — one instance's leaked material does not decrypt another's cache. Full guide (中文): [`docs/multi-machine.md`](docs/multi-machine.md) 「多实例（同机多 agent）」.
- **First trust for broker-unreachable targets (Plan 48).** A target only a work machine can reach (the broker has no route to it) no longer deadlocks on first connect: the cache client **forwards** the presented host key to the broker (`POST /pin-hostkey` — same device-code gate, insert-only, audited), the anchor lands in the authoritative vault, and the handshake completes on the first try. With the broker unreachable it fails closed with the presented fingerprint and two remedies (retry while online, or the owner pins out-of-band: `sshmgr servers pin-hostkey <name> --fingerprint SHA256:…`). Owner forensics: `servers pin-hostkey --list` (orphan anchors flagged) / `--clear` / `--clear --hostport`. Mixed-version note: an old (< v0.15.0) client reports a legitimate fingerprint anchor as "possible MITM" — upgrading fixes it. Full guide (中文): [`docs/multi-machine.md`](docs/multi-machine.md) 「主机密钥锚定」.
- **Shutdown.** `Ctrl+C` (`SIGINT`) / `SIGTERM` → graceful drain.

> **⚠️ Breaking change / migration order (Plan 42 批1).** The serve no longer accepts MCP-over-HTTP: old `"type": "http"` `.mcp.json` entries answer **404** after the upgrade. Migrate existing setups in **three steps**: ① migrate each legacy machine to the bridge posture **on the old serve** (manual path: `cache-tokens add` + `projects add` + `cache pull` + hand-written `.mcp.json` — clients ≥ v0.10.1 first); ② upgrade serve (precondition: all clients already bridged); ③ from then on every new machine uses `sshmgr pair`. The iron rule stands: **upgrade all clients first, restart serve last.** Full runbooks in [`docs/multi-machine.md`](docs/multi-machine.md) + [`docs/compat-matrix.md`](docs/compat-matrix.md).

---

## Environment variables

目前定义的用户可配环境变量：

| 变量 | 默认 / 语法 | 说明 |
|---|---|---|
| `SSHMGR_CACHE_MAX_OFFLINE` | Go duration（≥1h；unset/`0` 关，**默认关**） | 离线缓存到龄自废：超龄的下次 load/spawn 销毁本地 cache（服务器 Date 锚 + 1h 时钟容差）。优先级 env > `cache.config.json` > 关——v0.11 起可 `cache pull --max-offline 24h` 把上限持久化进每实例的 `cache.config.json`。详见 docs/multi-machine.md |
| `SSHMGR_UPDATE_BASE` | 默认 `https://api.github.com`；自定义基址 = 换信任白名单（镜像须照搬 GitHub 的 URL 路径布局）；非环回强制 https，环回 http 仅测试假源 | 自更新基址（`sshmgr update`，Plan 44）：镜像/内网换源 seam；生效 base 恒在 update 证据行显示，非默认时醒目标记 |
| `SSHMGR_TRANSFER_CHUNK` | 缺省 `268435456`（256 MiB）；可解析十进制字节值，合法域 `[16777216, 1073741824]`（[16 MiB, 1 GiB]） | relay_file 块大小（Plan 47）：非法/越界 → **broker 拒绝启动**（fail-closed）。续传两端块网格必须一致——改过此值的重跑会被拒（指引还原或 `fresh=true`） |
| `SSHMGR_TRANSFER_PARALLEL` | 只接受缺省/unset 或 `1` | relay_file 并行块传输 seam（Plan 47）：**v1 单流，非 `1` → broker 拒绝启动**（"reserved for a future version"）；名称与 v2 终态钳域 `[1,8]` 已冻结，放开零迁移 |

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

**Plan 10 — `serve` mode:** run the broker as an authenticated HTTP MCP server so agents on other VLAN machines share one authoritative vault. Phase 1 of multi-machine support (remote live access). **Plan 12 — offline read-only cache:** each work machine pulls an encrypted read-only snapshot of its bound profile's authorized servers (`cache pull`, scoped per device code since Plan 39) and can run the broker from it (`mcp --cache`) when the server is unreachable. Vault replication and Synology backup are later phases. See "Multi-machine: `serve` mode" above and [`docs/multi-machine.md`](docs/multi-machine.md#离线只读缓存plan-12).

**v0.7.0 (Plan 20) — ssh-config import & hygiene hardening:**

- **`servers import`** — bulk-import `~/.ssh/config` hosts (CLI `--file` / `--dry-run` / `--profile`; Web UI 服务器页「导入 SSH 配置」): vault-conflict skips make it idempotent, identical key files within one batch mint **one** credential row, encrypted keys import as-is with a needs-passphrase ⚠, and the Web UI walks a per-server supplement loop after import. See [`docs/managing-servers.md`](docs/managing-servers.md#批量导入servers-import).
- **Credential-less server model** — `servers add` / import may record a server with **no** credential (`--password` / `--key` optional, still mutually exclusive); `exec_command` on such a server fails **before** connecting with an error carrying the `servers edit` fix hint (audit `no_credential`); the Web UI flags them 「需要注意」 (filter with 「只看需要注意」).
- **`SSHMGR_TOKEN` env channel** — the project token can now ride the environment instead of argv (flag parity: `--token` still works); every `.mcp.json` generator (`projects add` / `rotate`, Web UI, wizard) now emits the `env` form. **Snippet shape changed**: previously copied `--token`-in-`args` snippets keep working, but regenerate when you next rotate.
- **`gc`** — find (and with `--apply`, delete) credential rows no server references via either column; dry-run by default; servers, host keys, and cache tokens are never touched.
- **Tunnel half-close** — `forward_port` tunnels propagate a directional EOF (`CloseWrite`) instead of tearing down both directions, so half-closing protocols (e.g. `shutdown(SHUT_WR)`) no longer kill the peer's reply.
- **`SSHMGR_SSH_HOST_KEY_ALGORITHMS` knob** — opt into legacy host-key algorithms (`ssh-rsa`, …) for old boxes; typo = fail-closed before any connect. See [`docs/managing-servers.md`](docs/managing-servers.md#连老机器需要-ssh-rsa).
- **Single build-version source** — CLI `version` and the MCP `serverInfo` now report the same ldflags-injected version.

Design spec: [`docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md`](docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md).
