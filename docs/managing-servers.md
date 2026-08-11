# 管理服务器（新增 / 编辑 / 维护 / 删除）

这篇讲 owner CLI 里 `servers` 子命令的全部用法。先建立心智模型，再逐个命令过。

> 还没装好 / 解锁？先看 [getting-started.md](./getting-started.md)。

---

## 心智模型：Server / Profile / Project

| 概念 | 是什么 | 关键字段 |
|---|---|---|
| **Server** | 一台目标机器 + 它的凭据 | name（唯一）、host、port、user、auth_method（password/privatekey）、credential_id、可选 sudo_credential_id、tags、description |
| **Profile** | server 的分组（多对多） | name；通过 `profiles grant` 把 server 加进来 |
| **Project** | 绑定到一个 profile 的 token（给某个 agent） | name、status、绑定的 profile_id |

凭据本身（密码字节 / 私钥字节）单独存在 `credentials` 表，server 只持有它的 id。所以“换密码”=“发一张新凭据，把 server 指过去”，server 的 id、name、profile 绑定都不变。

---

## 列出所有服务器：`servers ls`

```bash
ssh-manager servers ls
```

输出每行：`name  id  user@host:port  [sudo|-]  (role|-)  · <special-handling 前 ~40 字符>|-`。例如：

```
gpu              <uuid>              deploy@192.0.2.10:22 [sudo] (prod train) · do not reboot 02-03:00; failover is m...
db               <uuid>              pguser@10.0.0.5:22 [-] (prod pg primary) · -
```

- `[sudo]` 表示这台机器给了 sudo 密码（agent 看到 `has_sudo=true`）；`[-]` 表示没有。
- `(role)` 来自 `--role`（这台机器的用途）；空则显示 `-`。
- `·` 后面是 `--special-handling`（Caveats）的截断预览——给 agent 看的操作注意事项，约 40 字符截断；空则 `-`。其余结构化字段（location / hardware / services / description）不在 ls 行上，agent 通过 `list_servers` 看全。
- **密码 / 私钥永远不会出现在任何列表里。**

---

## 新增服务器：`servers add`

```bash
ssh-manager servers add \
  --name <唯一名字> \
  --host <hostname 或 IP> \
  --user <ssh 用户> \
  [--port 22]                       # 默认 22
  --password '<密码>' \              # 二选一，必须恰好一个
  # 或者：
  # --key <私钥文件路径> [--key-passphrase '<私钥口令>']
  [--sudo-password '<sudo 密码>']    # 给了才支持 sudo=true
  [--tags prod,gpu]                 # 可选，逗号分隔
  [--description '<你的备注>']        # 可选，给 agent 看的自由文本
  # 结构化字段（都可选，都会展示给 agent；详见下表）：
  [--location '<部署位置>']           # e.g. dc2 rack14 / us-east-1a
  [--hardware '<硬件配置>']           # e.g. 8x A100 80GB, 1TB RAM
  [--services '<跑了什么>']           # e.g. postgres primary, prometheus
  [--role '<这台机器的用途>']          # e.g. prod pg primary
  [--special-handling '<注意事项>']    # agent 行动前必读（Caveats 字段）
```

### 必填 vs 选填

- **必填**：`--name`、`--host`、`--user`，以及 `--password` 或 `--key` 恰好一个。
- `--password` 和 `--key` **互斥**；两个都给或都不给都会报错。
- `--key` 读的是私钥**文件**的路径（不是把私钥内容贴进来）。加密私钥用 `--key-passphrase` 解密。

### Structured server metadata

Each server carries structured fields, all shown to the agent via `list_servers` so it
understands what each box is and how to act safely:

| Flag | Field | Example |
|---|---|---|
| `--location` | where deployed | `dc2 rack14` / `us-east-1a` |
| `--hardware` | hardware config | `8x A100 80GB, 1TB RAM` |
| `--services` | what's deployed/running | `postgres primary, prometheus` |
| `--role` | this server's purpose | `prod pg primary` |
| `--special-handling` | operational gotchas the agent must heed | `do not reboot 02-03:00; failover is manual` |

`--description` is supplementary free-text; `--tags` is free-form labels. Both are also
shown to the agent.

> ⚠️ **Do not put secrets in any of these fields.** Keys, tokens, and PII entered here
> travel into the agent's context and the upstream LLM provider on every `list_servers`
> call. Use the credential vault (`--password` / `--key`) for secrets, never these fields.

Each field is capped at 4 KB. Edit any field with `ssh-manager servers edit <name> --<flag> ...`;
pass an empty value (`--special-handling ""`) to clear.

### sudo 的真相

`--sudo-password` 只是存了一个"额外的密码"，专门用于 broker 内部执行 `sudo -S`（从 stdin 喂密码）。它**不**改变 agent 登录用的账号密码。给了它 → agent 在 `list_servers` 看到 `has_sudo=true` → 它可以在 `exec_command` 里传 `sudo=true`，broker 自动用 `sudo -S` 跑这条命令。**agent 不应该自己把 `sudo` 拼到命令前面**（否则会出现 `sudo sudo ...`）。

---

## 编辑服务器：`servers edit`（只改你传的字段）

`edit` 是“原地改”：**只更新你显式传了的 flag**，没传的字段（包括 id、profile 绑定）原样保留。这让“只换密码”或“只改 host”变得很安全。

```bash
ssh-manager servers edit <name> [flags...]
```

可用 flags（全部可选，按需传）：

| Flag | 作用 |
|---|---|
| `--name <新名字>` | 重命名（保持 id 不变） |
| `--host <新地址>` | 改 IP / 域名（机器迁移） |
| `--port <端口>` | 改端口 |
| `--user <用户>` | 改登录用户 |
| `--description '<备注>'` | 改 / 加备注 |
| `--tags a,b,c` | **替换**整组 tags（不是追加） |
| `--location '<部署位置>'` | 改 / 加 location（结构化字段，展示给 agent） |
| `--hardware '<硬件配置>'` | 改 / 加 hardware |
| `--services '<跑了什么>'` | 改 / 加 services |
| `--role '<用途>'` | 改 / 加 role |
| `--special-handling '<注意事项>'` | 改 / 加 Caveats；传 `--special-handling ""` 清空 |
| `--password '<新密码>'` | 切换到 / 替换密码认证（会发新凭据） |
| `--key <私钥路径> [--key-passphrase '<口令>']` | 切换到 / 替换密钥认证 |
| `--sudo-password '<新sudo密码>'` | 设 / 换 sudo 密码 |

> `--password` 和 `--key` 在 `edit` 里同样**互斥**。可以一次同时改多个无关字段（如 `--host` + `--description`）。

### 常见维护操作（直接抄）

```bash
# 轮换密码（安全周期性操作）
ssh-manager servers edit gpu --password '<新密码>'

# 从密码登录切换到密钥登录
ssh-manager servers edit gpu --key ~/.ssh/id_ed25519

# 机器换了 IP / 端口
ssh-manager servers edit gpu --host 192.0.2.20 --port 2222

# 补一个 sudo 密码（之前忘了给）
ssh-manager servers edit gpu --sudo-password '<sudo密码>'

# 更新你的备注
ssh-manager servers edit gpu --description '8x A100 80GB, CUDA 12, 已扩容到 2TB NVMe'

# 重命名
ssh-manager servers edit gpu --name gpu-a100
```

注意：

- **改名后**，后续命令要用新名字引用（id 不变，用 id 也行）。
- **换密钥/密码不影响 profile 绑定**——这台 server 仍然在原来那些 profile 里，绑定的 agent 不用重新授权。
- `--tags` 是**替换语义**：`edit --tags a,b` 会把原有 tags 整个换成 `a,b`，不是追加。

---

## 删除服务器：`servers rm`

```bash
ssh-manager servers rm <name-or-id>
```

- 可以传 **name 或 id**（name 会自动解析成 id）。
- **级联清理**：删除一台 server，会自动把它从所有 grant 了它的 profile 里摘掉（数据库层 `ON DELETE CASCADE`，开了外键）。所以你**不需要**先手动挨个 profile 取消授权。
- 删了就没了，且这操作**没有二次确认**——删前确认名字对。建议先 `servers ls` 看一眼。
- 凭据字节（`credentials` 表里那条）不一定会被级联删掉，但它们仍是加密的、且已不被任何 server 引用——不影响安全。

> 目前 **没有** “把某台 server 从某个 profile 单独摘掉”的 CLI（`profiles` 只有 `add` / `ls` / `grant`）。如果你需要“某 agent 不再能访问某台机器”，最干净的做法是：
> - 要么把那台机器单独 grant 给一个新 profile，把 agent 换绑过去；
> - 要么直接 `servers rm`（如果整台机器都不用了）；
> - 要么对该 agent 的 project 做 `rotate` / `revoke`（见 [agent-access.md](./agent-access.md)）。

---

## Profile 的增 / 查 / 授权

```bash
ssh-manager profiles add <名字>                      # 建一个空 profile
ssh-manager profiles ls                              # 每个 profile 有几台 server
ssh-manager profiles grant <profile> <server1> [server2 ...]   # 把 server 加进 profile（可多个）
```

- `grant` 是**追加**且**幂等**（重复 grant 同一台不会报错，忽略）。
- 授权一台不存在的 server 会 fail-closed（整批授权回滚）。

典型编排（dev / staging / prod 三套，给三个不同 agent）：

```bash
ssh-manager profiles add dev     && ssh-manager profiles grant dev dev-web dev-db
ssh-manager profiles add staging && ssh-manager profiles grant staging stg-web stg-db
ssh-manager profiles add prod    && ssh-manager profiles grant prod prod-web prod-db
# 然后每个 agent 建一个 project 绑对应 profile（见 agent-access.md）
```

---

## Host key 与 TOFU

broker 连一台新机器时采用 **TOFU（trust on first use）**：第一次连接自动记下对方的 host key（存进保险柜的 `host_keys` 表），之后再次连接会核对——**对不上就拒绝**（防中间人攻击）。所以：

- 第一次连一台机器：自动信任并记录，无需你操作。
- 那台机器**重装系统 / 换了 host key**：再连会被拒（报 host key 不匹配）。你需要清掉旧记录后重连。处理方式：
  - 用 `ssh-keygen -R <host>` 不适用于本项目（broker 用自己的存储）。目前最直接的办法是连同一 `host:port` 的机器确实换了，重新建立信任——这通常意味着清掉库里那条 `host_keys` 记录。
  - 日常你很少会遇到，除非重装了服务器的 SSH。

---

## 一句话总结

- **加**：`servers add`（密码或密钥二选一；可选 sudo / tags / description）。
- **改**：`servers edit`（只传要改的字段；id 和 profile 绑定保留）。
- **查**：`servers ls` / `profiles ls`。
- **删**：`servers rm`（自动从所有 profile 摘除）。
- **分组**：`profiles add` + `profiles grant`。
- **给 agent 用**：去 [agent-access.md](./agent-access.md) 建 project 拿 token。
- **真实怎么用**：去 [scenarios.md](./scenarios.md) 看场景。
