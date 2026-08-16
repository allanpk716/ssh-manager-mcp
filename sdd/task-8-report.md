# Task 8 报告：client↔serve 版本兼容矩阵

**状态：完成** | commit `128576f56d1a8e8b078f5bbfb9b6525adaf51aa0`（分支 worktree-plan-25-ci-gate）
3 files changed, 30 insertions(+), 1 deletion(-)

## Step 1: git 取证输出

### tag 列表（`git tag -l | sort -V`）

```
v0.1.0 v0.2.0 v0.3.0 v0.3.1 v0.4.0 v0.5.0 v0.6.0 v0.7.0 v0.7.1 v0.7.2 v0.7.3
```

### `git log --oneline v0.3.1..v0.4.0 -- internal/mcpserver/serve.go internal/cli/cache.go`

```
73d80b2 fix(cli): scheme-error hint says clear-pin + --allow-plaintext (xT2 review Minor)
7fcae77 feat(cli): no-pin pull hard-fails unless --allow-plaintext; embed pin as single token (xcheck F4/F7/F11)
8f95d17 fix(cli): hard-fail when pin set but URL is not https (xcheck F8)
5b14546 docs(mcpserver): correct defensive else-branch comment in RunServe
d48523a feat(mcpserver): RunServe auto-generates cert + forces TLS when none given
0d56bb0 feat(cli): cache pull uses pinned TLS, plaintext fallback when no pin
7ad1b0d feat(cli): pinning TLS transport + pin resolution for cache pull
```

### TLS-only 版本结论

**实证版本 = v0.4.0，占位符正确，无需改版本号。**

- 证据 commit：`d48523a`（全 hash `d48523aae2cfa104a9adc471bd8e265f1f3f3742`，2026-08-13，"feat(mcpserver): RunServe auto-generates cert + forces TLS when none given"）
- `git tag --contains d48523a | sort -V | head -3` → `v0.4.0 v0.5.0 v0.6.0`（最早含此 commit 的 tag 是 **v0.4.0**）
- `git describe --tags d48523a` → `v0.3.0-9-gd48523a`（位于 v0.3.0 之后、v0.4.0 打 tag 之前，与 tag --contains 一致）
- 表格处理：把占位「（以 git log 实证为准）」替换为实证标注「（实证：commit `d48523a`，2026-08-13「RunServe auto-generates cert + forces TLS when none given」）」——满足 brief Step 1 Expected 的"写入矩阵时标注证据 commit"。

### 附带核实

- v0.7.0 `tui --mode broker` 移除：`068edef`（"--mode broker release note"）落在 v0.6.0..v0.7.0 范围内，且 `9491e50`（v0.7.0 release notes）同范围——表行成立。
- 矩阵引用的 README「migration order」锚点真实存在：根 README.md:214「**⚠️ Breaking change / migration order.** New `serve` is TLS-only. ...」（含先升 client 后重启 serve 的同一铁律表述）。

## Step 2: 新建 docs/compat-matrix.md

照 brief 模板全文写入，唯一改动 = 上述 TLS-only 行的实证标注替换。结构：维护规则引言 / 已验证组合（v0.7.3×v0.7.3，2026-08-16，在线 NUC10+笔记本、离线 9/9）/ 已知破坏性变更（v0.4.0 TLS-only + v0.7.0 mode-broker 移除）/ 升级顺序铁律 / 相关文档（multi-machine.md + agent-access.md 四层断连）。

关键行（破坏性变更表 TLS-only 行）：

```
| v0.4.0（实证：commit `d48523a`，2026-08-13「RunServe auto-generates cert + forces TLS when none given」） | serve 默认 TLS-only + 自签证书 + SPKI pin；无 pin 客户端默认拒连 | 旧明文 client 无法拉快照/连 MCP | 先升全部工作机 binary + 配 pin，**最后**重启 serve（README「migration order」） |
```

## Step 3: 挂链（2 处）+ 控制器追加项（1 处）

### docs/README.md（2 处改动）

(a) 目录表末尾（backup-restore.md 行后）加：

```
| [compat-matrix.md](./compat-matrix.md) | **client↔serve 版本兼容矩阵**：已验证组合 / 破坏性变更 / 升级顺序铁律。升级任何一端之前先看这篇。 |
```

(b) 控制器追加（T6 评审遗留，:28 agent-access.md 行）：

```diff
-| [agent-access.md](./agent-access.md) | **授权 AI agent**：project token 怎么生成、`.mcp.json` 怎么配进 Claude Code / Cursor、token 轮换 / 暂停 / 吊销的 Lazy 语义、多 agent 隔离、紧急处置。 |
+| [agent-access.md](./agent-access.md) | **授权 AI agent**：project token 怎么生成、`.mcp.json` 怎么配进 Claude Code / Cursor、token 轮换 / 暂停 / 吊销的断连语义（四层）、多 agent 隔离、紧急处置。 |
```

### docs/multi-machine.md（1 处改动）

「相关文档」列表末尾（仓库根 README 行后）加：

```diff
+- [compat-matrix.md](./compat-matrix.md)——client↔serve 版本兼容矩阵（升级任何一端之前先看）。
```

## 操作纪律自查

- 中文弯引号逐字节检查（python 计数）：compat-matrix.md 全文 U+201C=0、U+201D=0（模板用「」直角引号，无弯引号，天然无配对问题）；README.md / multi-machine.md 的 diff 行均不含新增弯引号。
- 只动了 3 个目标文件（git show --stat 核实：compat-matrix.md 新建 28 行、README.md +2/-1、multi-machine.md +1）。
- `git diff` 已核对，与 brief Step 3 逐字一致（README 表行、multi-machine 列表行均照抄 brief 文本）。

## Step 4: Commit

```
128576f56d1a8e8b078f5bbfb9b6525adaf51aa0
docs: client-serve compatibility matrix (verified pairs / breaking changes / upgrade-order rule) + index wording
```

## Concerns

无。TLS-only 版本实证为 v0.4.0（占位符即正确），证据 commit d48523a 已按 brief 要求写进矩阵表。
