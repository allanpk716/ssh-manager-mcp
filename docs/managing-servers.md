# 管理服务器（新增 / 编辑 / 维护 / 删除）

这篇讲 owner CLI 里 `servers` 子命令的全部用法。先建立心智模型，再逐个命令过。

> 还没装好 / 解锁？先看 [getting-started.md](./getting-started.md)。

---

## 心智模型：Server / Profile / Project

| 概念 | 是什么 | 关键字段 |
|---|---|---|
| **Server** | 一台目标机器 + 它的凭据 | name（唯一）、host、port、user、auth_method（password/privatekey）、credential_id（**可空** = 无凭据服务器，见下文专节）、可选 sudo_credential_id、tags、description |
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
  [--password '<密码>'] \             # 与 --key 互斥；两个都不给 = 无凭据服务器
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

- **必填**：`--name`、`--host`、`--user`。
- `--password` 和 `--key` **互斥**（最多给一个）；也可以**都不给**——先录机器后补凭据（[无凭据服务器](#无凭据服务器)，exec 时 agent 会收到明确的补凭据提示）。
- `--key` 读的是私钥**文件**的路径（不是把私钥内容贴进来）。加密私钥用 `--key-passphrase` 解密。

> 💡 **手里已经有一份 `~/.ssh/config`？** 别逐台抄——[`servers import`](#批量导入servers-import) 一条命令批量导入（dry-run 预览、冲突自动跳过、同密钥自动去重）。

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

## 批量导入：`servers import`

已经有一份 OpenSSH 客户端配置（`~/.ssh/config`）的话，不用逐台 `servers add`：

```bash
ssh-manager servers import --dry-run            # 默认读 ~/.ssh/config，先预览
ssh-manager servers import --profile team-a     # 真导入，并把导入的机器全部 grant 进 team-a
ssh-manager servers import --file /path/to/config   # 指定别的配置文件
```

### Flags

| Flag | 语义 |
|---|---|
| `--file <路径>` | 要读的 ssh config 文件；默认 `~/.ssh/config`。相对 IdentityFile 以**这个文件所在目录**解析（见下文「相对路径规则」） |
| `--dry-run` | 只打印将要发生什么，**一个字节都不写库**（含 `--profile` 时也只打印 grant 行）。建议每次先跑一遍 |
| `--profile <名字>` | 导入完成后把所有新导入的机器 grant 进这个 profile。**profile 必须已存在**——不存在则整批 fail-fast（dry-run 也一样报错），不会先导入一半 |

### 导入什么、跳过什么

每个 Host 块取 `HostName`（缺省用别名本身）、`Port`（缺省 22）、`User`（缺省用当前 OS 账户名）、`IdentityFile`（可多条，取**第一个能读到的**）。跳过规则分两层：

- **解析期**：通配 / 取反模式（`Host *`、`!alias`）跳过；Port 非法跳过；同一批里后面又指向已见 host:port:user 的别名跳过。
- **对库冲突**（让命令**幂等**——反复跑不会重复导入）：别名与库中已有 server 同名 → `skip-existing (name)`；host:port:user 三元组已存在 → `skip-existing (host:port:user)`。

### 密钥与凭据的四种结果

| 每台机器的报告行 | 含义 | 后续动作 |
|---|---|---|
| `imported key` | 找到可读私钥，已入 vault | 无 |
| `imported needs-passphrase ⚠（连接会失败；TUI 补全或 servers edit --key-passphrase）` | 私钥**有口令加密**，按原样导入（口令没有进 vault） | 补口令（见下） |
| `imported needs-credential` | 没有任何可读的 IdentityFile → 导入成**无凭据服务器** | 补凭据（见「无凭据服务器」） |
| `skip-existing ...` / `skip: ...` | 冲突或解析期跳过 | 无 |

- **批内密钥去重**：同一批里多台机器引用**同一个私钥文件**（内容相同），vault 只存**一份**凭据行——不会因为 20 台机器共用一个 key 就存 20 份。
- **needs-passphrase 的补法**（连接会一直失败，直到补上）：
  - **TUI**：主控台服务器页 `i` 导入流程的补全表单里有「密钥口令（补全加密私钥）」一栏，当场补（不重读盘上文件）；事后也可以 `!` 过滤出 ⚠ 机器，`e` 编辑补。
  - **CLI**：`ssh-manager servers edit <名字> --key <私钥路径> --key-passphrase '<口令>'`（重发凭据，换掉那份没口令的）。
- **原私钥文件不动**：import 只是把内容**复制**进 vault 加密存储，`~/.ssh` 下的原文件原样保留。

### Match 警示

config 里含 `Match` 块时，命令开头会打一条 ⚠：库按 `Host` 模式近似求值继承值（first-obtained-wins，含 `Host *`），**Match 条件**（`exec` / `host` 判定等）不参与——相关机器的 Port / User / IdentityFile 可能与真 `ssh` 实际用的不一致。导入后照 `servers ls` / `servers edit` 核对一遍即可。

### 相对路径规则（有意偏离 OpenSSH）

IdentityFile 写**相对路径**时，本工具按 **config 文件所在目录**解析；真 OpenSSH 是按 `ssh` 进程的工作目录（CWD）解析的。**这是有意的偏离**——CWD 在"哪条命令、哪个目录跑的"下漂移不定，config 目录才是这份文件自带的稳定基准（`~` / `~/...` 照常展开成家目录；`~user/...` 不展开，读不到就落到 needs-credential）。不想踩差异就把 config 里的路径写成绝对路径或 `~/` 开头。

### TUI 等价流程（服务器页 `i`）

主控台「服务器」页按 `i` 是同一套语义的可视化版：路径表单（预填 `~/.ssh/config`）→ 候选多选（vault 冲突已自动排除，全选预勾）→ 静默批量导入 → **逐台补全表单**（结构化字段 + sudo + 按需出现的密码 / 密钥口令栏；`Esc` 跳过这台保留 ⚠，`q` 结束补全循环）→ 结果页（导入 N / 跳过 N / 待补 N）。带口令的机器导入时会被打上 `needs-passphrase` 标签，补全表单里填了口令就自动摘掉。

---

## 无凭据服务器

「先录机器、凭据后补」是合法状态：`servers add`（或 import 的 `needs-credential` 结果）可以**不带任何凭据**。要点：

- **agent 一侧的语义**：对无凭据机器调 `exec_command` / 下载 / 上传 / 转发，会在**发起连接之前**被拒，错误信息自带补法——`server has no credential configured (set one with: ssh-manager servers edit <name> --password ... / --key ...)`（审计里统一记 `no_credential`，不是 auth_error，agent 不会误判成密码错了）。
- **TUI 一侧**：无凭据（以及未填 role、带 needs-passphrase 标签）的机器在列表里以 ⚠ 前缀**置顶**；按 `!` 只看这些待处理机器。
- **补凭据**：`servers edit <名字> --password '<密码>'` 或 `--key <私钥路径> [--key-passphrase '<口令>']`（TUI 里 `e` 编辑，密码栏填新值）。补上即正常连接，server 的 id / name / profile 绑定全程不变。
- **反向操作（退回无凭据态）**：`servers edit <名字> --clear-credential`——清除这台机器的登录 + sudo 凭据引用（独占动作、与 `--password` / `--key` 互斥，详见[编辑服务器](#编辑服务器servers-edit只改你传的字段)一节）；TUI 里是 `e` 编辑表单的「清除凭据」开关。

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
| `--clear-credential` | **清除**这台服务器的全部凭据引用，退回[无凭据态](#无凭据服务器)（独占动作，见下节） |

> `--password` 和 `--key` 在 `edit` 里同样**互斥**。可以一次同时改多个无关字段（如 `--host` + `--description`）。

### 清除凭据：`--clear-credential`（独占动作）

「换密码 / 换密钥」的反向操作——把一台已有凭据的机器**退回无凭据态**，一个事务完成：

```bash
ssh-manager servers edit gpu --clear-credential
```

- **独占动作**：`--clear-credential` 不是字段编辑。传了它，这条命令就**只做清除这一件事**——同一条命令里传的其他字段 flag（如 `--host`、`--name`）**不会生效**（想改字段请清除后另跑一条 `edit`）。确认输出打印的是**当前库名**。
- **互斥**：与 `--password` / `--key` 互斥，同传直接报错——「清除」和「换发凭据」是两个方向的操作，混在同一条命令里语义不清。
- **清除的范围**：登录凭据 + sudo 凭据的引用一并解除、`auth_method` 清空、`needs-passphrase` 标签摘除（无凭据时这个标签没有意义）；只有这台机器**独占**的凭据行会被级联删除，被其他 server 共用的不动。
- **TUI 对应**：服务器页 `e` 编辑表单里的「清除凭据（回到无凭据态）」确认开关，勾选提交即走同一条清除路径（也是同样的独占语义）。
- 清除后机器进入[无凭据服务器](#无凭据服务器)状态（列表 ⚠ 置顶、`!` 可过滤，agent 调用会收到带补法的 `no_credential` 提示）；server 的 id / name / profile 绑定**全程不变**，之后 `--password` / `--key` 随时可以补回。

> 💡 传错了想清除却给了空值？`edit --password ""` 这类**显式空值会被拒绝**（报错文案会指回 `--clear-credential`）——空串是引号 / 笔误的常见形态，静默铸一条空凭据会让 agent 之后连接时莫名失败。清除只有 `--clear-credential` 这一条路，意图明确才动手。

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

# 退回无凭据态（清除登录 + sudo 凭据引用；独占动作，别和其他字段混传）
ssh-manager servers edit gpu --clear-credential
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

## 连老机器（需要 ssh-rsa）

很老的 SSH 服务器（CentOS 7 时代的机器、老交换机 / 嵌入式设备）可能只支持 `ssh-rsa`（SHA-1 签名）或需要显式点名 RSA 签名算法，新客户端默认不提供 → 握手直接失败。broker 端给了一个环境变量旋钮：

```bash
export SSHMGR_SSH_HOST_KEY_ALGORITHMS="ssh-rsa,rsa-sha2-512"   # 逗号分隔
```

- **允许值（白名单）**：`ssh-ed25519`、`ecdsa-sha2-nistp256`、`ecdsa-sha2-nistp384`、`ecdsa-sha2-nistp521`、`rsa-sha2-256`、`rsa-sha2-512`、`ssh-rsa`。
- **写错 = fail-closed**：值不在白名单里，连接在**发起之前**就报错（错误信息会点名这个环境变量和允许列表），绝不会静默回退到默认值去连。
- 不设 / 留空 = 用默认算法集。
- 这是给 **broker 进程**（`serve` 端）设的环境变量，改完要重启 broker 才生效。

> ⚠️ **开旋钮后注意 TOFU**：如果这台服务器有多个 host key（比如同时有 ed25519 和 RSA），默认连接时记录下的可能是 ed25519 那把；开了旋钮后对方可能改呈 RSA host key——与已记录的对不上，会被 host key mismatch 拒掉。这时需要清掉这台机器的 host key 记录（见上文「Host key 与 TOFU」），重走一次 TOFU。
>
> 另外提醒：`ssh-rsa` 依赖 SHA-1，属于弱签名算法。只在确实连不上老机器时临时开，用完关掉。

---

## 清理孤儿凭据：`gc`

历史操作可能在 `credentials` 表里留下**没有任何 server 引用**（既不是 `credential_id` 也不是 `sudo_credential_id`）的孤儿行——它们仍是加密的、无害，只是占地方。`gc` 专门清这个：

```bash
ssh-manager gc           # 默认 dry-run：只数一遍、打印数量，不动任何东西
ssh-manager gc --apply   # 真删：删掉的恰好是上面数出来的那批孤儿行
```

- **永不碰**：`servers`、`host_keys`（TOFU 记录）、cache tokens——`--apply` 的 WHERE 条件就是上面那个两列引用检查，删的只可能是无引用凭据行。
- 什么时候会有孤儿：老版本 `edit` 换凭据换出来的旧行之类。Plan 20 起 add / edit / import 的 server+凭据写入都是单事务的，正常操作不再产生新孤儿——`gc` 更像一次性打扫，不是日常命令。
- 只读模式（`mcp --cache`）下 `--apply` 会被拒绝（store 只读）。

---

## 一句话总结

- **加**：`servers add`（凭据可选——密码或密钥最多一个；可选 sudo / tags / description）。
- **批量加**：`servers import`（ssh config → vault；dry-run 预览、冲突跳过、同密钥去重、`--profile` 顺手授权）。
- **改**：`servers edit`（只传要改的字段；id 和 profile 绑定保留；也是补凭据 / 补私钥口令 / 清除凭据（`--clear-credential`）的路）。
- **查**：`servers ls` / `profiles ls`。
- **删**：`servers rm`（自动从所有 profile 摘除）。
- **扫**：`gc`（孤儿凭据；dry-run 默认，`--apply` 才删）。
- **分组**：`profiles add` + `profiles grant`。
- **给 agent 用**：去 [agent-access.md](./agent-access.md) 建 project 拿 token。
- **真实怎么用**：去 [scenarios.md](./scenarios.md) 看场景。
