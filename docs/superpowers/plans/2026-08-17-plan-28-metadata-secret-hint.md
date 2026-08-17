# Plan 28：metadata 疑似 secret 提示（rev2 P5 · 收官批）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** metadata 字段（description/role/services/location/hardware/caveats/tags）写入 vault 前的"疑似 secret"提示——前缀规则命中时打警告（不阻塞、不回显内容），防"手滑把私钥/API key 粘进备注→每次 list_servers 外发给 LLM"这一旁路。

**Architecture:** 新纯函数包 `internal/secrethint`（规则集 + 聚合扫描，零依赖）+ 四条写入路各挂一次警告调用（CLI 三路 stderr 警告后继续；TUI 写入已有 report/反馈机制显示 ⚠ 行）。**只提示，永不阻塞，永不回显疑似内容。**

**Tech Stack:** Go 标准库（strings），零新依赖。

## Global Constraints

- **高熵规则不实现**（xcheck 终审附录 A8 写死；实验实测 2.9% FP 唯一误报即合法 sha256 指纹）。
- **规则集 = xcheck 实验 3 的 9 前缀**（实测 0 FP），唯一收紧：`-----BEGIN` 必须与 `PRIVATE KEY` 在同一字段值内**共现**才命中（裸 `-----BEGIN` 会命中公开证书，rev2 明文裁决）。前缀全集：`-----BEGIN`+`PRIVATE KEY` 共现 / `sk-` / `ghp_` / `gho_` / `github_pat_` / `AKIA` / `xoxb-` / `xoxp-` / `eyJ`。
- **不回显疑似内容**：警告只含字段名 + 规则名，绝不含字段值（测试用哨兵串钉住）。
- **不阻塞**：CLI 打警告后命令照常执行（可绕过=没有 gate）；TUI 不加拦截确认屏。
- **铁律不动**：只读 metadata 文本、只写 stderr/report 行；不碰凭据字段、不碰 MCP surface。
- **无新依赖**；测试密闭；gofmt/vet 干净；双 lane CI 必绿。
- **文案与已实现行为一致**。

## 背景（已取证）

1. **威胁**：metadata 是设计上要给 agent 看的——`list_servers` 返回全文进 agent 上下文 → 上游 LLM（docs/managing-servers.md:88 已明示该流向），每次调用重复外发；明文 NAS 备份（Plan 13）与 export 同样携带。凭据字段有铁律，metadata 字段没有——粘进去的 secret 等于主动外发。
2. **实验数据**（`.xcheck/20260816-195610/exp/secretscan/main.go`，本 plan 的规则集来源）：9 前缀对 35 条真实合法语料（hardware/role/services/description 长自由文本 + sha256 pin）**0 误报**；高熵规则 1 误报（2.9%，sha256 指纹 43 字符 4.54 b/c）→ 不实现。
3. **字段面**（SnapshotServer 为权威模型）：自由文本 7 字段 = `tags`(TagsRaw) / `description` / `location` / `hardware` / `services` / `role` / `caveats`。name/host/port/user 是结构字段不扫。
4. **四条写入路**：CLI `servers add`（internal/cli/servers.go）、CLI `servers edit`（同文件，cobra Changed() 部分更新）、CLI `servers import`（servers_import.go + internal/importer；ssh_config 本身几乎不带这些字段，实际 metadata 多由 TUI 导入补全表单填）、TUI（internal/tui 服务器表单 + importflow 补全循环）。

## 设计决策（定死，评审按此判）

- API 形态：

```go
package secrethint

// Finding 是一个字段上的一次命中。不含字段值（不回显约束）。
type Finding struct {
	Field string // "description" / "tags" / …
	Rule  string // "pem-private-key" / "prefix:sk-" / …
}

// ScanValue 对单个字段值跑规则集，返回全部命中（可多个）。
func ScanValue(field, value string) []Finding

// ScanServer 聚合 7 个 metadata 字段（字段名小写固定序：tags, description,
// location, hardware, services, role, caveats）。nil 切片 = 无命中。
func ScanServer(tags, description, location, hardware, services, role, caveats string) []Finding
```

- 警告输出统一形态（CLI，写 stderr）：

```
warning: server metadata may contain a secret — field 'description' matched rule 'pem-private-key' (content not shown; this text would be sent to LLM providers on every list_servers — edit the server to fix, or ignore if intentional)
```

一行一命中；命令随后照常完成（exit code 不受影响）。
- TUI 形态：保存路径上命中 → 在该流程已有的反馈/report 行追加 `⚠ metadata 疑似 secret: <field>(<rule>)，详见 CLI warning 说明`——不加拦截屏。

## 任务间接口

- T1 产出 `secrethint.ScanValue/ScanServer`（上述精确签名）+ `FormatWarning(f Finding) string`（统一警告行文案，CLI/TUI 共用保证一致性）。
- T2/T3 消费这三个函数；T4 文档引用 `FormatWarning` 的实际输出形态。

---

### Task 1: internal/secrethint 包 + 语料回归

**Files:**
- Create: `internal/secrethint/secrethint.go`
- Create: `internal/secrethint/secrethint_test.go`
- Create: `internal/secrethint/testdata/corpus-legal.txt`（35 条合法语料，从 `.xcheck/20260816-195610/exp/heuristic-corpus.txt` 抄录——注意该路径 gitignored，把语料**复制进** testdata 使其入库）

**Interfaces:** 见设计决策节的精确签名。

- [ ] **Step 1: 失败测试**：
  - `TestScanValueTruePositives`——每规则至少一真阳：完整 `-----BEGIN OPENSSH PRIVATE KEY-----\nMIIE…`（多行值）、`-----BEGIN RSA PRIVATE KEY-----`、`sk-ant-api03-` 长串、`ghp_` 40 字符、`github_pat_`、`AKIA`+16 位、`xoxb-`、`eyJ` JWT 头、值中**嵌**前缀（前后有普通文字）。
  - `TestScanValueLegalCorpus`——读 testdata 语料逐行 `ScanValue("description", line)`，断言全程零命中（0 FP 钉住）。语料须含 sha256 pin 行（实验里唯一险些命中的类别）。
  - `TestScanValuePublicCertNotFlagged`——`-----BEGIN CERTIFICATE-----\nMIID…` **不得**命中（PEM 收紧的钉子）。
  - `TestFormatWarningNoContentEcho`——`FormatWarning` 输出不含传入字段值（哨兵串断言）。
  - `TestScanServerAggregates`——两字段命中 → 2 条 Finding，字段名正确、固定序。
- [ ] **Step 2: 红** → **Step 3: 实现**（规则表驱动；`-----BEGIN`+`PRIVATE KEY` 用 `strings.Contains` 两次判共现，大小写敏感——PEM 头标准形态大写）→ **Step 4: 绿**（`go test ./internal/secrethint/ -count=1`）。
- [ ] **Step 5: Commit** `feat(secrethint): prefix-rule scanner + legal-corpus 0-FP regression (Plan 28 T1)`。

### Task 2: CLI servers add/edit 接入

**Files:**
- Modify: `internal/cli/servers.go`（add 与 edit 两处 RunE）
- Modify: `internal/cli/servers_test.go` 或新建 `servers_hint_test.go`

- [ ] **Step 1: 失败测试**：`TestServersAddWarnsOnSuspectedSecret`——`servers add` 带 `--caveats "-----BEGIN OPENSSH PRIVATE KEY-----…"` → 服务器**照常入库**（成功路径不变）+ stderr 含 `field 'caveats'` 与 `pem-private-key` + **哨兵串不出现在任何输出**。`TestServersEditWarnsOnSuspectedSecret` 同型（edit 一个既有 server 的 description 为 `sk-…`）。负例：干净 metadata → stderr 无 warning 行。
- [ ] **Step 2: 红 → Step 3: 实现**——add：在写库前对 7 字段 `ScanServer`（tags 用最终 TagsRaw），命中逐条 `fmt.Fprintln(cmd.ErrOrStderr(), secrethint.FormatWarning(f))`；edit：只对 Changed() 的字段 `ScanValue`（部分更新语义）。**不改变任何返回值/退出码。** → **Step 4: 绿**。
- [ ] **Step 5: Commit** `feat(cli): servers add/edit suspected-secret metadata warnings, non-blocking (Plan 28 T2)`。

### Task 3: import 与 TUI 接入

**Files:**
- Modify: `internal/cli/servers_import.go`（批量导入写库前扫每台候选）
- Modify: `internal/tui/`（服务器表单保存路径 + importflow 补全保存路径，grep `AddServer`/`UpdateServer` 调用点定位）
- Test: 对应 `_test.go`

- [ ] **Step 1: 失败测试**：`TestImportWarnsOnSuspectedSecret`——import 一个含 secret 备注（若 importer 路径实际不携带 metadata 字段则造 supplement/直接对聚合函数测——以 grep 实际字段流为准，如实报告）；TUI 侧：保存路径纯函数化（把"扫+格式化"抽成可测小函数 `hintLines(srv) []string`），对它断言命中行与非回显。
- [ ] **Step 2: 红 → Step 3: 实现**——import：每台入库前扫，聚合 warning 打 stderr，导入继续；TUI：保存成功后的反馈/report 行追加 ⚠ 行（不加拦截）。→ **Step 4: 绿**（`go test ./internal/cli/ -count=1` 与 `go test ./internal/tui/ -count=1` 两条**分开的**简单命令）。
- [ ] **Step 5: Commit** `feat: import + TUI suspected-secret metadata warnings (Plan 28 T3)`。

### Task 4: 文档 + 全量验证

**Files:**
- Modify: `docs/managing-servers.md`（metadata 一节加 3-4 行：警告语义、不阻塞、为什么（list_servers→LLM 流向）、修复法）
- Modify: `README.md`（仅当有自然落点——grep `metadata`，无则跳过）
- Modify: `docs/backlog.md`（若 P5 相关项——无独立项则不动；#6-#8 与此无关）

- [ ] **Step 1: 文档**（引用 FormatWarning 实际输出形态；如实写"前缀规则 9 类、高熵不实现、0 FP 语料回归"）。
- [ ] **Step 2: 全量验证**：`go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -count=1`（各一条独立命令）。
- [ ] **Step 3: 手工冒烟**：`go run ./cmd/ssh-manager servers add hint-test 127.0.0.1 --description "sk-test-abcdef0123456789abcdef" --password x`（temp vault——用 SSHMGR_STORE/FILEKEY_PATH 指向 temp 建库；观察 warning 行 + 服务器照常入库 + 随后清理）。
- [ ] **Step 4: Commit** `docs: suspected-secret metadata warning semantics (Plan 28 T4)`。

---

## 验收（整 plan）

1. 四条写入路（add/edit/import/TUI）任一命中前缀规则都会出警告，且**全部不阻塞**、**退出码不变**、**疑似内容零回显**（哨兵断言遍布各测试）。
2. `-----BEGIN CERTIFICATE-----` 不命中；9 前缀对合法语料 0 FP（语料入库 testdata）。
3. 高熵规则不存在于代码中（`grep -ri entropy internal/secrethint` 零命中）。
4. 全量测试双 lane 绿；gofmt/vet 零输出；文档与 FormatWarning 实际输出一致。
