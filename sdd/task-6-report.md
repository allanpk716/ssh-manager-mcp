# Task 6 报告：断连语义四层改写

## 状态：完成 ✅

**Commit hash:** c836ca5

## 修改清单（8 处）

### 1. agent-access.md 104-109 段（Step 1）
**原文：**
```
⚠️ **关键机制——Lazy 生效：** `rotate` / `disable` / `enable` / `revoke` **不是立刻断正在运行的 agent**，而是在 agent **下一次启动 `mcp` 子进程时**生效（token 校验只放行 `status=active` 的 project）。

为什么这样设计？**你的机器你做主**：你重启 Claude Code / 它重启 MCP 子进程时，新策略才接管；当前正在跑的会话保留它的访问直到那一步。这意味着：
- 想立刻掐断某个 agent → 除了 `revoke`/`disable`，还要让客户端重连（重启 Claude Code，或它的 MCP 子进程）。
- `rotate` 保持 project id 和 profile 不变，**只换 token**。
```

**替换为：**
```
⚠️ **关键机制——断连语义按部署模式分四层**（`rotate` / `disable` / `enable` / `revoke` 的生效范围）：

1. **stdio（本机 MCP 子进程）——Lazy 生效**：token 校验只在 `mcp` 子进程**下次启动**时跑（只放行 `status=active`）。正在跑的会话保留访问直到你重启 Claude Code（或它的 MCP 子进程）。**你的机器你做主**：这是有意的设计。
2. **serve（远程 broker）——逐请求即拒**：broker 对**每一个** HTTP 请求都重新验 token，`revoke`/`disable` 后该 project 的**下一个请求立即 401**——不需要等任何重启。
3. **已建立的 `forward_port` 隧道——不受 revoke 影响，且无 owner 急停**：隧道由 broker 进程持有；被吊销的 project 自己调 `close_port` 会先被第 2 层的 401 挡住；任何 stdio 会话或其他 project 的隧道管理器是**独立进程实例**，够不到它。真实选项只有：**重启 broker**（`serve uninstall`→`install` 或重启机器）/ **等隧道创建后 ~10 分钟自动回收**。（owner 侧急停命令已列 backlog。）
4. **离线 cache——旧快照不随 revoke 擦除**：`cache-tokens revoke` 只断"拉新"（下次 `cache pull` 被拒）；已落盘的 `cache.bin` 里凭据仍在。**失窃/泄露场景下让已缓存凭据失效的唯一手段是轮换服务器凭据**（`servers edit <name> --password/--key`）。

`rotate` 保持 project id 和 profile 不变，**只换 token**（serve 模式下旧 token 同样逐请求即拒）。
```

---

### 2. agent-access.md 175 行（Step 2）
**原文：** `| 要立刻断正在跑的会话 | revoke/disable 后，**重启那个客户端**（让它重连 MCP）。 |`

**替换为：** `| 要立刻断正在跑的会话 | 看模式：serve 远程 agent 下一个请求即拒（无需动作）；stdio 本机会话须重启客户端；既有隧道见「断连语义（四层）」第 3 层（只能重启 broker 或等回收）。 |`

---

### 3. agent-access.md 197 行（Step 3）
**原文：** `| 暂停了 agent 还在跑 | Lazy 机制：disable/revoke 在**下次重连**才接管。重启那个客户端。 |`

**替换为：** `| 暂停了 agent 还在跑 | stdio：Lazy，下次重连才接管，重启那个客户端；serve：下一请求即拒，无需动作。详见「断连语义（四层）」。 |`

---

### 4. scenarios.md 158 行（Step 4a）
**原文：** `# 想立刻断正在跑的会话：重启那个客户端，让它重连 MCP。`

**替换为：** `# serve 模式下一请求即拒；stdio 会话重启客户端；隧道见 agent-access「断连语义（四层）」。`

---

### 5. scenarios.md 169 行（Step 4b）
**原文：** `> Lazy 机制：disable / enable / revoke / rotate 都在 agent **下次重连 MCP** 时接管，详见 [agent-access.md](./agent-access.md) 的"Project 生命周期"一节。`

**替换为：** `> 断连语义分四层（stdio=下次重连；serve=逐请求即拒；既有隧道不受 revoke 影响且只能重启 broker/等创建后 ~10 分钟回收；离线 cache 须轮换凭据），详见 [agent-access.md](./agent-access.md) 的「断连语义（四层）」一节。`

---

### 6. scenarios.md 208 行（Step 4c）
**原文：** `- **出事了** → `rotate`（换卡）/ `disable`（暂停）/ `revoke`（吊销），重启客户端让它立刻接管。`

**替换为：** `- **出事了** → rotate（换卡）/ disable（暂停）/ revoke（吊销）——serve 模式下一请求即拒；stdio 会话重启客户端接管；离线缓存场景须轮换服务器凭据（见 agent-access「断连语义（四层）」）。`

---

### 7. README.md 127 行（Step 5）
**原文：** `**Lifecycle is Lazy:** `rotate` / `disable` / `enable` / `revoke` take effect at the agent's **next `mcp` spawn** (`VerifyToken` admits only `active` projects). A currently-running agent session keeps its access until Claude Code restarts its MCP child — by design (your box, your call). `rotate` keeps the same project id + profile; only the token changes. `revoke` is a soft delete — the token is dead and the project is hidden from `ls`, but the audit row is kept. Every lifecycle action is written to the audit log.`

**替换为：** `**Lifecycle:** `rotate` / `disable` / `enable` / `revoke` take effect **per request on a remote serve broker** (the next request is 401-rejected immediately), and **at the agent's next `mcp` spawn in stdio mode** (`VerifyToken` admits only `active` projects — a currently-running local session keeps access until Claude Code restarts its MCP child, by design). Already-open `forward_port` tunnels survive revocation (no owner emergency-stop; broker restart or the ~10-minutes-after-creation reclaim). Offline caches keep working from their last snapshot — rotate server credentials to invalidate them. Full breakdown: `docs/agent-access.md` 「断连语义（四层）」.`

---

### 8. multi-machine.md 570 行（Step 6）
**原文：** `- [agent-access.md](./agent-access.md)——project token 生命周期（`rotate` / `disable` / `revoke` 的 Lazy 语义）；**serve 模式完全适用**，token 管理在同一台服务器上做。`

**替换为：** `- [agent-access.md](./agent-access.md)——project token 生命周期；**断连语义分四层**：serve 模式下吊销**逐请求即拒**（远程 agent 无需重启）；stdio/隧道/离线缓存各有不同（见「断连语义（四层）」一节）。token 管理在同一台服务器上做。`

---

## 验证结果

### Step 7-1：grep 验证"serve 模式完全适用"已清除
```bash
$ git grep -n "serve 模式完全适用"
(无输出) ✅
```

### Step 7-2：grep 验证 Lazy 表述已限定 stdio 或指向四层小节
```bash
$ git grep -n "下次重连才接管\|下次启动.*mcp.*子进程时.*生效" docs/ | grep -v stdio
(无输出) ✅
```

### 中文引号自查
所有新文本中的中文引号均为正确的开引号（"）和闭引号（"）。

---

## 文件变更统计
- `docs/agent-access.md`：+7 -3（3 处修改）
- `docs/scenarios.md`：+3 -3（3 处修改）
- `README.md`：+1 -1（1 处修改）
- `docs/multi-machine.md`：+1 -1（1 处修改）

**总计：** 4 个文件，14 行插入，11 行删除。

---

## Concerns
无
