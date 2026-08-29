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
| 一屏看全部署形态（单机 / 多机桥姿态 + 管理面）怎么选 | [`docs/deployment-modes.md`](docs/deployment-modes.md) |
| 从零到跑通（安装 / 解锁 / 第一台服务器 / 授权 Claude Code） | [`docs/getting-started.md`](docs/getting-started.md) |
| 新增 / 编辑 / 维护 / 删除服务器 | [`docs/managing-servers.md`](docs/managing-servers.md) |
| 授权 Claude Code / Cursor / 其他 agent；token 轮换与吊销 | [`docs/agent-access.md`](docs/agent-access.md) |
| 多台机器共用一份 vault（`pair` 一条龙入网 + 离线只读缓存） | [`docs/multi-machine.md`](docs/multi-machine.md) |
| **在 broker（serve）主机上也跑 agent**（零距离 client 走桥 + 应急附录） | [`docs/broker-host-agent.md`](docs/broker-host-agent.md) |
| 应用场景与示例（GPU 巡检、读 root 日志、部署、端口转发……） | [`docs/scenarios.md`](docs/scenarios.md) |
| 备份 / 迁移整个 vault（export / import） | [`docs/backup-restore.md`](docs/backup-restore.md) |
| **单机 TUI 教程**（全键盘点选，不想记命令） | [`docs/tui-single-machine.md`](docs/tui-single-machine.md) |
| **联机 TUI 教程**（server 侧主控台 + client 面板 + Pairing 批准页） | [`docs/tui-multi-machine.md`](docs/tui-multi-machine.md) |
| **给 AI agent 的工具手册**（可贴进 CLAUDE.md 的规则模板在内） | [`docs/agent-tools.md`](docs/agent-tools.md) |

---

## What the agent gets (the MCP tools)

The MCP server exposes these tools — **ssh-functional-equivalent for operating a server** (interactive shell is intentionally not provided):

| Tool | Like | What it does |
|---|---|---|
| `list_servers` | — | List the servers the agent may use (`id` / `name` / `host` (`"hidden"` by default; owner opts in per server) / `user` / `has_sudo`, plus owner-provided context: role, services, location, hardware, caveats, tags, description). Always call first — the agent learns real server ids here. Never includes credentials. |
| `exec_command` | `ssh host cmd` | Run a shell command on a server. `sudo=true` runs `sudo -S` for you (do **not** prepend `sudo` yourself) — the **whole** line, every `;`/`&&`/`\|` segment, executes elevated (v0.10.1+; before that only the first segment did — see compat-matrix). The result carries `sudo: {outcome, uid}`: elevation is observable, and the did-NOT-run outcomes (`auth-failed` / `sudo-start-failed` / `wrap-failed`) come back as tool errors. |
| `download_file` | `scp host:path .` | Download a remote file (size-capped; truncated output is flagged). |
| `upload_file` | `scp -r . host:path` | Upload a local file **or directory** (recursive) to the server. |
| `upload_content` | — | Write inline content (a string the agent holds) to a remote file — the **cross-machine** upload path (`upload_file` reads the broker's own filesystem; a remote-serve agent pushes its own configs/scripts here). `text` / `base64` (byte-exact) encodings, 8 MiB decoded cap, parent dirs auto-created, existing file overwritten. |
| `exec_context` | — | Capture the exec channel's TRUE context in one round: uid/gid/groups, tty, uid_map, LSM label, SSH provenance, process tree — call it BEFORE hypothesizing about identities or "mystery permission denied" (e.g. when sudo returns uid=0 yet a path stays EACCES). |
| `forward_port` | `ssh -L` | Open a local port forwarding to a remote service — returns `<listen_host>:<port>` (default `127.0.0.1`; non-loopback listen hosts are rejected fail-closed — multi-machine clients are loopback-only by design). |
| `close_port` | — | Close a forward when done (idle tunnels auto-close after ~10 min of no traffic; the owner can emergency-kill any tunnel, and revoke/disable tears open tunnels down within ~15s). |
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
go build -o sshmgr ./cmd/sshmgr        # or: go install ./cmd/sshmgr

# 2. Unlock the vault (writes master key → fixed-path file `master.key.plain`; admin/root needed first time to create the vault dir + set ACL — see `unlock --help`)
sshmgr unlock

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

Drop that snippet into your agent's MCP config (Claude Code: `.mcp.json`; Cursor / other MCP clients: per their setup). The agent now has the ten SSH tools, scoped to the `team-a` profile's servers.

**Other commands:** `servers ls` / `servers rm`, `profiles ls`, `projects ls`, `gc` (find/delete orphan credential rows — dry-run by default), `lock`, `clear` (role teardown — wipes the machine back to first-run), `doctor` (side-effect-free local self-check — prints a PASS/WARN/FAIL report; exit `0` = no FAIL findings, `1` = at least one FAIL), `version`, `pair` (multi-machine one-shot enrollment — LAN discovery → SAS pairing → credentials delivered; see "Multi-machine" below). Tunnel governance (owner, on the machine holding the vault): `tunnels ls` / `tunnels kill <tunnel_id>` / `tunnels kill --project <name>` (emergency stop for live tunnels — teardown within one ~15s control tick; `kill` is surgical and does not revoke the token, `--project` only tears down what exists now — use `projects disable/revoke` to stop re-opening). Audit forensics (owner, on the machine holding the vault): `audit` reads the vault audit log, newest first (owner-only) — `--since 30m|7d|RFC3339|date`, `--server/--project <name|id>`, `--owner`, `--action/--status`, `--limit` (0 = all), `--json` for JSONL.

**Owner access** (you, not the agent) — full access to every server using the stored creds directly:
```bash
sshmgr ssh gpu nvidia-smi          # run ONE command (single, non-interactive)
```
The owner path runs a **single non-interactive command** (connect + exec share one 120-second deadline; output is uncapped; a non-zero remote exit makes the CLI exit non-zero (the code value appears in the error message)). No command → explicit error. Interactive shells are intentionally not provided — for a terminal, use your own SSH client with credentials you already hold or provision separately (they may live only in this vault).

```bash
# Audit forensics — what every agent (and owner) did, newest first
sshmgr audit --since 24h --project my-agent --status error
sshmgr audit --json --limit 0 > audit.jsonl   # nine fields, sidecar-compatible
```

Known filter values: `action` = `exec` / `download` / `upload` / `upload-content` / `forward` / `close-forward` / `exec-bg-start` / `exec-bg-end` + owner-side `project.rotate` / `project.disable` / `project.enable` / `project.revoke` / `project.delete`; `status` = `ok` / `error` / `timeout` / `cancelled` / `denied` / `auth_error` / `hostkey_mismatch` / `connect_error` / `no_credential` / `no_sudo` / `bind_denied`. The sets evolve across versions; unknown filter values silently match nothing (empty result, never an error).

Human-mode output notes: the server / project columns render four ways — the entity's name, `(none)` (no server context, e.g. project-level owner actions), `(owner)` (no project = an owner action), or `id…(deleted)` for a row that has outlived its entity. Dynamic text (commands, entity names) is escape-notated for the terminal: control / invisible characters render as `\n`, `\x1b`, `\u202e`-style sequences, and a literal backslash becomes `\\` (the notation is reversible); seeing a bidi escape such as `\u202e` means the original command carried a direction override — don't trust the visual order of that line. `exit=0` is ambiguous by design: a failure that never produced an exit code (connect-time) and a command that ran and exited 0 both display `exit=0` — read the `status` column to tell them apart. `--limit 0` prints a stderr warning that `audit_log` has no auto-cleanup and the output may be large; when rows are truncated, stderr gets `showing first N rows (more exist) — use --limit 0 for full output` (JSON mode emits rows only, no stderr chatter).

---

## Managing servers & projects (owner CLI)

The owner CLI is how you record servers, group them, and grant an agent access. Each server carries structured metadata (`--role` / `--services` / `--location` / `--hardware` / `--special-handling`) plus free-text `--description` and `--tags` — **all shown to the agent** via `list_servers` so it can act on each box safely. ⚠️ Never put secrets in these fields: they enter the agent's context (and the upstream LLM provider) on every call. As a backstop, every metadata write path (add / edit / import / TUI) prints a **non-blocking advisory warning** when a field matches one of 9 known secret shapes (8 token prefixes like `sk-` / `ghp_` / `AKIA` + a tightened PEM rule) — a hint, not a verdict, and it never echoes the content. See [`docs/managing-servers.md`](docs/managing-servers.md).

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

**Back up / migrate the whole vault:** `sshmgr export` / `import` — a portable, passphrase-encrypted file (backup / migration / disaster recovery). Full guide (中文): [`docs/backup-restore.md`](docs/backup-restore.md).

---

## TUI 主控台（`sshmgr tui`）

一条命令的可视化管理台：在 **broker 机器**上管服务器 / profiles / projects / 设备码 / **配对批准**（Pairing 页，替代手敲 CLI），在 **client 工作机**上查看缓存状态 / 切换实例 / 手动同步。同一个二进制，按本机状态自动选边。各页签 / 设备码 / token 谁是谁的**概念模型图解**（仓库隐喻，中文）：[`docs/concepts.md`](docs/concepts.md)。

空机器第一次运行 `tui` 会进入**角色向导**（单机 / server / client 三选，可中断续配；client 分支 = `sshmgr pair` 入网引导页——连接表单已随 ②a 退役，多机入网一律 pair）；`sshmgr clear` 角色清理——**按实际存在枚举**删除（与 role.json 声明的角色无关）本机 vault / serve / 缓存残留（vault 角色先自动 export 备份 + 输入 `DELETE` 确认），机器回到首次向导状态。

### 启动与模式判定

```bash
sshmgr tui                # 自动判定
sshmgr tui --mode client  # 强制 client 面板
```

> v0.7.0 起 `tui --mode broker` 移除（自动判定覆盖该场景；`--mode client` 保留）。

自动判定规则：

- 本机有**已解锁的 vault** → broker 主控台；
- 本机有**缓存**（`cache.bin`）→ client 面板；
- vault 存在但锁着 / 两者都没有 → **引导性报错**（告诉你该 `unlock` 还是 `--mode client`）——**绝不静默降级到 client**。

Broker 主控台（服务器 / Profiles / Projects / 设备码 / Pairing 5 个页签）与 client 面板（服务器列表只读、零远程写、`[s]` 同步 / `[i]` 实例切换）的操作语义与 owner CLI 完全一致——TUI 只是同一套 vault 操作的另一个入口，做完的事在 `ls` / 审计里看到的一样。各页签键位与典型任务走查见 [docs/tui-single-machine.md](docs/tui-single-machine.md) / [docs/tui-multi-machine.md](docs/tui-multi-machine.md)。

### 终端要求（mintty 注意）

Windows Terminal / cmd 原生可用。**mintty**（Git Bash 默认终端）不是 Windows 控制台，需 `winpty sshmgr tui`；在非 TTY 下启动时程序会**直接报错提示**，不会挂死或乱码。

### 安全面

- **凭据永不回显**：密码 / key / sudo 密码的输入框全程掩码；已设凭据只显示「已设置」，输入新值即更换，不为确认而回显旧值。
- **token / 设备码一次性展示**：全屏显示一次，关闭后不可再查（与 CLI 的 shown-once 语义一致）。
- **client 零远程写**：client 模式的任何按键都不会写 broker。

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
# → owner approves on the broker TUI's Pairing page (or `serve pair approve laptop --profile team-a`)
# → done: agent runs from a local read-only cache
```

- **The serve is narrowed to: authoritative vault + `/snapshot` + `/pair` (+ web admin UI in a later release).** There is **no remote MCP surface** (the old `"type": "http"` + bearer-header direct connection was **removed** in Plan 42 批1 — non-`/snapshot`//`/pair` paths answer 404). Work-machine agents always run **locally from a read-only cache** (`mcp --cache`) and dial target servers themselves; writes (add/edit servers, approvals) belong to the **management plane**: broker TUI / `serve pair` CLI / the future web UI.
- **Auto-TLS + fingerprint pinning (no cert hassle).** On first start `serve` **auto-generates a self-signed ed25519 cert** and forces TLS from then on — no openssl, no CA distribution, no trust-store config. The cert's **SPKI fingerprint** rides the discovery offer and the sealed pairing envelope automatically; `cache pull` pins it too (first-connect verification, zero MITM window — the HPKP/Tailscale model). Pass `--tls-cert`/`--tls-key` only if you want your own cert.
- **Pairing is human-gated.** The client shows `name @ url SAS <6 digits>`; the approver compares the client-screen SAS against the approval row's name@url (broker TUI Pairing page or `serve pair approve`), while the serve **mechanically verifies** that the client's declared target address is actually this machine (mismatch → ⚠ + explicit override required). A direct `--url` pairing without a pin **refuses by default** (`--allow-tofu` is the explicit, unanchored escape hatch — see threat-model R12). No-pin `cache pull` hard-fails the same way; `--allow-plaintext` is the debug-only opt-out.
- **Several agents on ONE machine — named cache instances (Plan 40).** Each agent gets its own offline cache: enroll with `sshmgr pair --instance <name>` (one device code per agent). Inspect every slot with `sshmgr cache status`, switch slots inside the TUI client page with `[i]` (per-session), and persist each instance's offline cap independently with `sshmgr cache config [--instance <name>] --max-offline 24h` (priority env > file > off). Instances live under `instances/<name>/` with a **per-instance DEK** — one instance's leaked material does not decrypt another's cache. Full guide (中文): [`docs/multi-machine.md`](docs/multi-machine.md) 「多实例（同机多 agent）」.
- **Shutdown.** `Ctrl+C` (`SIGINT`) / `SIGTERM` → graceful drain.

> **⚠️ Breaking change / migration order (Plan 42 批1).** The serve no longer accepts MCP-over-HTTP: old `"type": "http"` `.mcp.json` entries answer **404** after the upgrade. Migrate existing setups in **three steps**: ① migrate each legacy machine to the bridge posture **on the old serve** (manual path: `cache-tokens add` + `projects add` + `cache pull` + hand-written `.mcp.json` — clients ≥ v0.10.1 first); ② upgrade serve (precondition: all clients already bridged); ③ from then on every new machine uses `sshmgr pair`. The iron rule stands: **upgrade all clients first, restart serve last.** Full runbooks in [`docs/multi-machine.md`](docs/multi-machine.md) + [`docs/compat-matrix.md`](docs/compat-matrix.md).

---

## Environment variables

目前定义的用户可配环境变量：

| 变量 | 默认 / 语法 | 说明 |
|---|---|---|
| `SSHMGR_CACHE_MAX_OFFLINE` | Go duration（≥1h；unset/`0` 关，**默认关**） | 离线缓存到龄自废：超龄的下次 load/spawn 销毁本地 cache（服务器 Date 锚 + 1h 时钟容差）。优先级 env > `cache.config.json` > 关——v0.11 起可 `cache pull --max-offline 24h` 把上限持久化进每实例的 `cache.config.json`。详见 docs/multi-machine.md |

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

- **`servers import`** — bulk-import `~/.ssh/config` hosts (CLI `--file` / `--dry-run` / `--profile`; TUI server page `i`): vault-conflict skips make it idempotent, identical key files within one batch mint **one** credential row, encrypted keys import as-is with a needs-passphrase ⚠, and the TUI walks a per-server supplement loop after import. See [`docs/managing-servers.md`](docs/managing-servers.md#批量导入servers-import).
- **Credential-less server model** — `servers add` / import may record a server with **no** credential (`--password` / `--key` optional, still mutually exclusive); `exec_command` on such a server fails **before** connecting with an error carrying the `servers edit` fix hint (audit `no_credential`); TUI flags them ⚠ (filter with `!`).
- **`SSHMGR_TOKEN` env channel** — the project token can now ride the environment instead of argv (flag parity: `--token` still works); every `.mcp.json` generator (`projects add` / `rotate`, TUI, wizard) now emits the `env` form. **Snippet shape changed**: previously copied `--token`-in-`args` snippets keep working, but regenerate when you next rotate.
- **`gc`** — find (and with `--apply`, delete) credential rows no server references via either column; dry-run by default; servers, host keys, and cache tokens are never touched.
- **Tunnel half-close** — `forward_port` tunnels propagate a directional EOF (`CloseWrite`) instead of tearing down both directions, so half-closing protocols (e.g. `shutdown(SHUT_WR)`) no longer kill the peer's reply.
- **`SSHMGR_SSH_HOST_KEY_ALGORITHMS` knob** — opt into legacy host-key algorithms (`ssh-rsa`, …) for old boxes; typo = fail-closed before any connect. See [`docs/managing-servers.md`](docs/managing-servers.md#连老机器需要-ssh-rsa).
- **Single build-version source** — CLI `version` and the MCP `serverInfo` now report the same ldflags-injected version.

Design spec: [`docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md`](docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md).
