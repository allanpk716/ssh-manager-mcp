# MCP 引导补全（TUI token 片段 + 双模式 TUI 教程 + agent 工具手册）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 主控台事后发 token 补全双形态 `.mcp.json` 引导、client finish 屏补在线 http 形态、新增三篇分受众文档（TUI 单机/联机教程 + agent 工具手册）并全链路联动。

**Architecture:** 纯 TUI 表现层 + 文档——不改 `internal/cli` / `internal/mcpserver` / store 层。渲染机制零改动：`mcpConfigLines` 只抽共享骨架，真 token/notes 差异全部收敛在新 helper `projectTokenMsg` 的传参里；`mcpHttpConfigLines` 为 http 形态新增；文档片段以 golden fixture + docs 自动比对测试双层锚定。

**Tech Stack:** Go（bubbletea v2 / huh v2 / lipgloss v2）、`encoding/json`（`SetEscapeHTML(false)`）、中文 Markdown 文档。

**Spec:** `docs/superpowers/specs/2026-08-20-mcp-guidance-completion-design.md.rev3.md`（rev3 定稿，已经三轮异构评审闭环）

## Global Constraints

- 文档全部中文；agent-tools.md 直接称 agent「你」。
- **渲染器零改动约束**：`mcpConfigLines` 的**签名**（`func mcpConfigLines(fieldLines []string, notes []string) []string`）与向导既有调用点行为不变——`wizard_test.go` / `wizardserve_test.go` / `wizardsteps_test.go` 既有用例**零改动**全绿是每任务的回归门。
- **值编码（硬要求）**：所有插值（stdio env token、http url、http Bearer）一律经 `jsonValue`（`json.Encoder` + `SetEscapeHTML(false)` 编码完整值串后剥外层引号）；**禁止** `strconv.Quote` 系（产出 `\a` `\v` 等 Go 专用非法 JSON 转义）、**禁止** `json.Marshal` 默认 HTML 转义（会把 `<` `>` `&` 变成 6 字符转义串，摧毁占位符可读性）。
- token 走 env 不走 argv（任何片段里不得出现 `--token`）。
- 不给 secretView/finish 屏加滚动；内容排序保证截断安全（token 首位、JSON 块紧随小节引导行、关键 note 前置）。
- commit 规范：Conventional Commits（`feat(tui):` / `docs:` / `test(tui):`）+ 尾行 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`；每任务一 commit。
- 版本号 bump（v0.8.10）是 **merge 时**动作，不在本计划任务内。
- 工作分支：`worktree-tui-mcp-guidance`（已在隔离 worktree，勿动 master）。

---

### Task 1: 渲染器基建 — `jsonValue` + 共享骨架 + `mcpHttpConfigLines`

**Files:**
- Modify: `internal/tui/wizardsteps.go`（`mcpConfigLines` 区块，约 L183-232）
- Test: `internal/tui/wizardsteps_test.go`（文件末尾追加）

**Interfaces:**
- Consumes: 现有 `mcpConfigLines(fieldLines, notes []string) []string`、`overlay`/`wizStaticView`（本任务不触碰）。
- Produces（后续任务依赖，签名一字不差）:
  - `func jsonValue(s string) string` — JSON 字符串值编码（含两侧引号，HTML 转义关闭）
  - `func stdioEnvLine(token string) string` — 返回 `"env": { "SSHMGR_TOKEN": <jsonValue(token)> }` 形态的 member 行
  - `func mcpHttpConfigLines(urlRef, tokenRef string, notes []string) []string`
  - `func jsonBlockOf(lines []string) string`（测试辅助，实现在 wizardsteps_test.go）

- [ ] **Step 1: 写失败测试**

在 `internal/tui/wizardsteps_test.go` 末尾追加（`json` 已在该文件 import）：

```go
// jsonBlockOf lifts the pretty JSON object out of a builder's rendered lines
// (first standalone "{" line to last "}" line) — the docsync/golden tests
// compare THIS block byte-for-byte (spec §5.5: 比对对象 = 仅 JSON 块).
func jsonBlockOf(lines []string) string {
	start, end := -1, -1
	for i, l := range lines {
		if l == "{" && start < 0 {
			start = i
		}
		if l == "}" {
			end = i
		}
	}
	if start < 0 || end < start {
		panic("no JSON object in lines")
	}
	return strings.Join(lines[start:end+1], "\n")
}

// goldenStdioBlock / goldenHttpBlock are the SPEC-PINNED placeholder goldens
// (spec §5.5): every doc snippet must normalize to one of these (Task 9).
const goldenStdioBlock = `{
  "mcpServers": {
    "ssh": {
      "command": "ssh-manager",
      "args": ["mcp"],
      "env": { "SSHMGR_TOKEN": "<TOKEN>" }
    }
  }
}`

const goldenHttpBlock = `{
  "mcpServers": {
    "ssh": {
      "type": "http",
      "url": "https://192.0.2.5:7878/",
      "headers": { "Authorization": "Bearer <TOKEN>" }
    }
  }
}`

// TestGoldenStdioBlock: mcpConfigLines at the pinned placeholder token must
// render the golden JSON block byte-for-byte.
func TestGoldenStdioBlock(t *testing.T) {
	got := jsonBlockOf(mcpConfigLines(
		[]string{`"args": ["mcp"]`, stdioEnvLine("<TOKEN>")}, nil))
	if got != goldenStdioBlock {
		t.Fatalf("stdio golden drift:\n--- got ---\n%s\n--- want ---\n%s", got, goldenStdioBlock)
	}
}

// TestGoldenHttpBlock: mcpHttpConfigLines at the pinned placeholder URL+token.
func TestGoldenHttpBlock(t *testing.T) {
	got := jsonBlockOf(mcpHttpConfigLines("https://192.0.2.5:7878/", "<TOKEN>", nil))
	if got != goldenHttpBlock {
		t.Fatalf("http golden drift:\n--- got ---\n%s\n--- want ---\n%s", got, goldenHttpBlock)
	}
}

// TestHttpConfigLinesJSONValid: empty-notes and populated-notes both render
// valid JSON with a comma-free last member (same discipline as the stdio
// builder's TestMcpConfigLinesJSONValid).
func TestHttpConfigLinesJSONValid(t *testing.T) {
	for _, notes := range [][]string{nil, {`"type": "http" 必填——漏了会被当 stdio 拒绝。`}} {
		var v any
		if err := json.Unmarshal([]byte(jsonBlockOf(mcpHttpConfigLines("https://h:1/", "tok", notes))), &v); err != nil {
			t.Fatalf("notes=%q: invalid JSON: %v", notes, err)
		}
	}
}

// TestValueEncodingAnchor: EVERY interpolation point (stdio env token, http
// url, http Bearer) survives the nasty-value gauntlet — `"`, `\`, control
// chars (\x07, \v), `&`, `<` — as (a) parseable JSON and (b) WITHOUT the
// default-HTML-escape sequences (SetEscapeHTML(false) anchor) and (c) with
// the angle brackets still literally present (copy-paste readability).
func TestValueEncodingAnchor(t *testing.T) {
	nasty := "x\"y\\z\x07\v&<>"
	blocks := []string{
		jsonBlockOf(mcpConfigLines([]string{`"args": ["mcp"]`, stdioEnvLine(nasty)}, nil)),
		jsonBlockOf(mcpHttpConfigLines(nasty, "tok", nil)),
		jsonBlockOf(mcpHttpConfigLines("https://h", nasty, nil)),
	}
	for i, b := range blocks {
		var v any
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			t.Fatalf("block %d not valid JSON: %v\n%s", i, err, b)
		}
		if strings.Contains(b, "\\u003c") || strings.Contains(b, "\\u0026") {
			t.Fatalf("block %d leaked HTML escapes:\n%s", i, b)
		}
		if !strings.Contains(b, "&<>") || !strings.Contains(b, "x\\\"y") {
			t.Fatalf("block %d must keep the literal nasty chars readable:\n%s", i, b)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestGolden|TestHttpConfigLines|TestValueEncodingAnchor' -v`
Expected: 编译失败 `undefined: stdioEnvLine / mcpHttpConfigLines`（golden/http 测试无法编译）。

- [ ] **Step 3: 实现**

`internal/tui/wizardsteps.go`：将 `mcpConfigLines`（L189-211）替换为下面四个函数（**注意**：`mcpConfigLines` 对既有调用方的输出必须逐字节不变——骨架逻辑原样搬进 `mcpSnippetLines`，只是抽出；`mcpConfigScreen` 本任务**不动**）：

```go
// jsonValue encodes s as a complete JSON string VALUE (quotes included) with
// HTML escaping DISABLED — the pinned value-encoding discipline (spec §4.2):
// strconv.Quote-family is forbidden (Go-only \a \v escapes are illegal JSON)
// and json.Marshal's default HTML escaping would turn < > & into 6-char
// escape sequences, wrecking every angle-bracket placeholder.
func jsonValue(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // string encode never fails
	return strings.TrimSuffix(b.String(), "\n")
}

// stdioEnvLine builds the stdio member line carrying the token — the ONLY
// sanctioned way to interpolate SSHMGR_TOKEN (symmetric encoding discipline
// with the http builder's url/Bearer values).
func stdioEnvLine(token string) string {
	return `"env": { "SSHMGR_TOKEN": ` + jsonValue(token) + ` }`
}

// mcpSnippetLines renders the shared snippet skeleton — intro line, the
// pretty-printed mcpServers object with comma-joined members, and the notes
// block. Both builders (stdio mcpConfigLines / http mcpHttpConfigLines) call
// it, so the trailing-comma discipline exists in ONE place.
func mcpSnippetLines(members []string, notes []string) []string {
	lines := []string{
		"把下面的片段写进 agent 项目的 .mcp.json：",
		"",
		"{",
		`  "mcpServers": {`,
		`    "ssh": {`,
	}
	for i, m := range members {
		if i < len(members)-1 {
			m += ","
		}
		lines = append(lines, "      "+m)
	}
	lines = append(lines, `    }`, `  }`, "}", "", "说明：")
	for _, n := range notes {
		lines = append(lines, "- "+n)
	}
	return lines
}

// mcpConfigLines renders the .mcp.json snippet shared by every role's finish
// screen — only the field lines (args / env) and the notes differ per role
// (standalone/server run plain `mcp`, the client role runs `mcp --cache`).
// The "ssh" object's members (command + fieldLines) are collected first and
// comma-joined as a whole, so the LAST member never carries a trailing comma —
// an empty fieldLines list yields valid JSON too.
func mcpConfigLines(fieldLines []string, notes []string) []string {
	members := make([]string, 0, len(fieldLines)+1)
	members = append(members, `"command": "ssh-manager"`)
	members = append(members, fieldLines...)
	return mcpSnippetLines(members, notes)
}

// mcpHttpConfigLines renders the ONLINE (serve/http) .mcp.json snippet —
// sibling of mcpConfigLines (stdio shape), sharing the mcpSnippetLines
// skeleton. VALUE ENCODING (hard requirement, pinned): urlRef and the
// Authorization header are encoded via jsonValue on the COMPLETE value
// string (e.g. "Bearer "+tokenRef) — never per-fragment concatenation.
func mcpHttpConfigLines(urlRef, tokenRef string, notes []string) []string {
	members := []string{
		`"type": "http"`,
		`"url": ` + jsonValue(urlRef),
		`"headers": { "Authorization": ` + jsonValue("Bearer "+tokenRef) + ` }`,
	}
	return mcpSnippetLines(members, notes)
}
```

同时在 import 块加 `"encoding/json"`。

- [ ] **Step 4: 跑测试确认通过 + 既有回归门**

Run: `go test ./internal/tui/ -run 'TestGolden|TestHttpConfigLines|TestValueEncodingAnchor|McpConfig' -v`
Expected: 新测试全 PASS；`TestMcpConfigLinesJSONValid`、`TestMcpConfigScreen_Copy` 等既有用例零改动 PASS。

- [ ] **Step 5: 全包回归**

Run: `go test ./internal/tui/`
Expected: 全绿（wizard 路径零改动约束的第一次验证）。

- [ ] **Step 6: Commit**

```bash
git add internal/tui/wizardsteps.go internal/tui/wizardsteps_test.go
git commit -m "feat(tui): jsonValue 值编码 + 片段共享骨架 + mcpHttpConfigLines 渲染器

SetEscapeHTML(false) 钉死(占位符全角括号可读性); stdio/http 双 golden;
三插值点编码锚(\" \\ 控制字符 & <)。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `tokenIssuedMsg` 扩展 — usage/recovery/snippet + `body()`

**Files:**
- Modify: `internal/tui/tokenview.go`（L7-29 区块）
- Test: `internal/tui/tokenview_test.go`（**新建**）

**Interfaces:**
- Produces: `tokenIssuedMsg{title, token, usage, recovery string; snippet []string}`（新增三字段零值=现状）+ 方法 `func (m tokenIssuedMsg) body() string`（Task 3 的 handler 调用）。

- [ ] **Step 1: 写失败测试**

新建 `internal/tui/tokenview_test.go`：

```go
package tui

import (
	"strings"
	"testing"
)

// TestTokenIssuedMsgBody_ZeroFields: usage/recovery/snippet 全零值时 body()
// 输出 = 现状裸 token（设备码发射点 / 向导发射点的零值兼容锚）。
func TestTokenIssuedMsgBody_ZeroFields(t *testing.T) {
	m := tokenIssuedMsg{title: "设备码 — laptop", token: "DEV-x"}
	if got := m.body(); got != "DEV-x" {
		t.Fatalf("zero-field body must be the bare token, got %q", got)
	}
}

// TestTokenIssuedMsgBody_BlockOrder: 三字段齐备时按固定顺序分块
// token → 用途 → 丢失 → 片段；snippet 非空时含引导语首行与说明块（归属锚）。
func TestTokenIssuedMsgBody_BlockOrder(t *testing.T) {
	m := tokenIssuedMsg{
		title:    "项目 token",
		token:    "TOK-1",
		usage:    "填进 agent 的 .mcp.json",
		recovery: "Projects 页 [e] 轮换换发",
		snippet:  mcpConfigLines([]string{`"args": ["mcp"]`, stdioEnvLine("TOK-1")}, []string{"note-a"}),
	}
	b := m.body()
	iTok := strings.Index(b, "TOK-1")
	iUse := strings.Index(b, "用途：填进")
	iLost := strings.Index(b, "⚠ 仅此一次。丢失 → ")
	iSnip := strings.Index(b, "把下面的片段写进 agent 项目的 .mcp.json：")
	if !(iTok < iUse && iUse < iLost && iLost < iSnip) {
		t.Fatalf("block order must be token→用途→丢失→片段:\n%s", b)
	}
	for _, want := range []string{"说明：", "- note-a", `"SSHMGR_TOKEN": "TOK-1"`} {
		if !strings.Contains(b, want) {
			t.Fatalf("snippet block missing %q:\n%s", want, b)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run TestTokenIssuedMsgBody -v`
Expected: 编译失败 `m.body undefined (type tokenIssuedMsg has no field or method body)`。

- [ ] **Step 3: 实现**

`internal/tui/tokenview.go`：把 L7-10 的 `tokenIssuedMsg` 定义替换为：

```go
// tokenIssuedMsg carries a freshly minted token from a store mutation cmd to
// App.Update, which swaps in a secretView overlay. The plaintext transits this
// one message and then lives only inside the overlay — never in form state.
//
// usage/recovery/snippet are OPTIONAL guidance (zero value = the historical
// bare-token behavior; the device-code emitter puts its whole body in `token`
// and the wizard emits none of these — it owns its own two-screen flow):
//   - usage:    "token 去哪"一行（wizTokenScreen 的用途行同款纪律）
//   - recovery: "丢失→"一行（store 只存 hash，明文不可恢复）
//   - snippet:  mcp.json 引导块 = mcpConfigLines/mcpHttpConfigLines 完整输出
//     按序拼接（引导语 + JSON + 说明块都在其中）；nil 时整块不渲染。
type tokenIssuedMsg struct {
	title, token string
	usage        string
	recovery     string
	snippet      []string
}

// body renders the full secretView body: token first (always), then the
// optional guidance blocks in fixed order (用途 → 丢失 → 片段).
func (m tokenIssuedMsg) body() string {
	var b strings.Builder
	b.WriteString(m.token)
	if m.usage != "" {
		b.WriteString("\n\n用途：" + m.usage)
	}
	if m.recovery != "" {
		b.WriteString("\n⚠ 仅此一次。丢失 → " + m.recovery)
	}
	if len(m.snippet) > 0 {
		b.WriteString("\n\n" + strings.Join(m.snippet, "\n"))
	}
	return b.String()
}
```

文件头 import 块加 `"strings"`。

- [ ] **Step 4: 跑测试确认通过 + 全包回归**

Run: `go test ./internal/tui/ -run TestTokenIssuedMsgBody -v && go test ./internal/tui/`
Expected: 新测试 PASS；全包绿（此时 app.go 尚未用新字段，行为不变）。

- [ ] **Step 5: Commit**

```bash
git add internal/tui/tokenview.go internal/tui/tokenview_test.go
git commit -m "feat(tui): tokenIssuedMsg 加 usage/recovery/snippet 可选引导字段 + body()

零值=现状裸 token(设备码/向导发射点兼容锚有测试钉死)。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `projectTokenMsg` + Projects 页 a/e 接线 + 流程级断言

**Files:**
- Modify: `internal/tui/app.go`（L290-319 两发射点；L458-466 handler）
- Test: `internal/tui/projects_test.go`（末尾追加）

**Interfaces:**
- Consumes: `projectTokenMsg` 用 Task 1 的 `mcpConfigLines`/`stdioEnvLine`/`mcpHttpConfigLines`、Task 2 的 `tokenIssuedMsg` 字段。
- Produces: `func projectTokenMsg(title, token string) tokenIssuedMsg`（app.go 包内函数）。

- [ ] **Step 1: 写失败测试**

`internal/tui/projects_test.go` 末尾追加（`newStore`/`NewBrokerApp` 是包内既有 helper；表单驱动模式抄 `profiles_test.go` 的 drain 循环）：

```go
// driveForm submits a formOverlay via Enter presses, draining huh's cmd chain
// (same loop shape as TestNewGrantFormPreselectsExisting), and returns the
// formDoneMsg the completion produced.
func driveForm(t *testing.T, o *formOverlay, presses int) formDoneMsg {
	t.Helper()
	var cmd tea.Cmd
	_, cmd = o.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for n := 0; n < presses-1; n++ {
		_, cmd = o.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	for steps := 0; cmd != nil && steps < 100; steps++ {
		msg := cmd()
		if done, ok := msg.(formDoneMsg); ok {
			return done
		}
		o2, next := o.Update(msg)
		_ = o2
		cmd = next
	}
	t.Fatal("form never completed")
	return formDoneMsg{}
}

// TestProjectTokenMsg_DualForms: 两条真片段——stdio 与 http 块都代入真 token，
// recovery 指向轮换，http 块是中立 <serve URL> 占位 + 单机忽略引导行。
func TestProjectTokenMsg_DualForms(t *testing.T) {
	m := projectTokenMsg("项目 token", "TOK-real")
	if m.title != "项目 token" || m.token != "TOK-real" {
		t.Fatalf("title/token: %+v", m)
	}
	if !strings.Contains(m.usage, ".mcp.json") || !strings.Contains(m.recovery, "轮换") {
		t.Fatalf("usage/recovery copy: %q / %q", m.usage, m.recovery)
	}
	joined := strings.Join(m.snippet, "\n")
	for _, want := range []string{
		`"args": ["mcp"],`,
		`"SSHMGR_TOKEN": "TOK-real"`,
		`"type": "http",`,
		`"Authorization": "Bearer TOK-real"`,
		"<serve URL>",
		"未部署 serve 可忽略本块",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("snippet missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--token") {
		t.Fatalf("token must ride env, not argv:\n%s", joined)
	}
}

// TestProjectsKeyE_EmitsGuidedTokenMsg — 流程级（spec §4.3）：Projects 页 [e]
// 经真实 keypress → confirm 表单 → action 命令链，返回的必须是 projectTokenMsg
// 形态的 tokenIssuedMsg（防"helper 正确但发射点漏改"）。
func TestProjectsKeyE_EmitsGuidedTokenMsg(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p")
	if _, _, err := st.AddProject("proj", pid); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st) // project exists BEFORE page fetch
	if err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})          // servers → profiles
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})          // profiles → projects
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})   // rotate confirm
	if cmd == nil {
		t.Fatal("[e] must open the rotate confirm form")
	}
	fo, ok := m.(App).overlay.(*formOverlay)
	if !ok {
		t.Fatalf("want formOverlay, got %T", m.(App).overlay)
	}
	done := driveForm(t, fo, 1) // single Confirm: one Enter
	if done.after == nil {
		t.Fatal("rotate submit must carry the mutation cmd")
	}
	msg := done.after()()
	tm, ok := msg.(tokenIssuedMsg)
	if !ok {
		t.Fatalf("rotate must emit tokenIssuedMsg, got %T", msg)
	}
	if tm.title != "项目 token（已轮换）" || !strings.Contains(tm.body(), `"SSHMGR_TOKEN": "`+tm.token) {
		t.Fatalf("rotate msg shape: %q\n%s", tm.title, tm.body())
	}
}

// TestProjectsKeyA_EmitsGuidedTokenMsg — 流程级：Projects 页 [a] 新增表单
// （输入项目名 → 选 profile → 提交）同样必须发射 guided msg。
func TestProjectsKeyA_EmitsGuidedTokenMsg(t *testing.T) {
	st := newStore(t)
	if _, err := st.AddProfile("p"); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if cmd == nil {
		t.Fatal("[a] must open the new-project form")
	}
	fo, ok := m.(App).overlay.(*formOverlay)
	if !ok {
		t.Fatalf("want formOverlay, got %T", m.(App).overlay)
	}
	// 打项目名 "pj"（两个字符键进入 name 输入框），再 Enter×2（跳到 profile
	// 选择 / 提交单选）。presses=2 指 drain 阶段前的额外 Enter 数。
	_, _ = fo.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	_, _ = fo.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	done := driveForm(t, fo, 2)
	if done.after == nil {
		t.Fatal("add submit must carry the mutation cmd")
	}
	msg := done.after()()
	tm, ok := msg.(tokenIssuedMsg)
	if !ok {
		t.Fatalf("add must emit tokenIssuedMsg, got %T", msg)
	}
	if tm.title != "项目 token" || !strings.Contains(tm.body(), `"type": "http",`) {
		t.Fatalf("add msg shape: %q\n%s", tm.title, tm.body())
	}
}
```

（import 块补 `"charm.land/bubbletea/v2"` 作 `tea`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestProjectTokenMsg|TestProjectsKey' -v`
Expected: 编译失败 `undefined: projectTokenMsg`。

- [ ] **Step 3: 实现**

`internal/tui/app.go`：

3a. 在 Projects 页 `case "a"` 区块（约 L278-301）**之前**加 helper：

```go
// projectTokenMsg builds the guidance-complete tokenIssuedMsg for the
// Projects page add/rotate: the REAL token embedded in BOTH snippet forms —
// a single one-time screen, CLI printToken parity (the wizard's placeholder
// approach belongs to its two-screen flow and does not apply here). The http
// block's URL is the neutral "<serve URL>" placeholder on purpose: the serve
// address/port varies with --addr and plaintext deployments exist, so this
// machine cannot know its own outward URL; the token is the copy-paste-
// critical value and must be real.
func projectTokenMsg(title, token string) tokenIssuedMsg {
	stdio := mcpConfigLines(
		[]string{`"args": ["mcp"]`, stdioEnvLine(token)},
		[]string{
			`Windows 建议写绝对路径，如 "command": "C:\\Tools\\ssh-manager.exe"。`,
			".mcp.json 含 token，不要提交进 git。",
		})
	httpBlock := mcpHttpConfigLines("<serve URL>", token, []string{
		`"type": "http" 必填——漏了会被当 stdio 拒绝。`,
		"<serve URL> 按 serve 实际启动地址填（默认形态 https://<主机>:7878/；端口随 --addr 变，明文部署见 docs/multi-machine.md）。",
		".mcp.json 含 token，不要提交进 git。",
	})
	snippet := append([]string{"—— 本机/单机 agent（stdio）——"}, stdio...)
	snippet = append(snippet, "", "—— 联机在线 agent（直连 serve，http；未部署 serve 可忽略本块）——")
	snippet = append(snippet, httpBlock...)
	return tokenIssuedMsg{
		title:    title,
		token:    token,
		usage:    "填进 agent 的 .mcp.json（下方片段已代入此 token，抄完即用）",
		recovery: "Projects 页 [e] 轮换换发（旧 token 立即失效）",
		snippet:  snippet,
	}
}
```

3b. `case "a"` 的发射（L294-300）`return tokenIssuedMsg{title: "项目 token", token: token}` 改为：

```go
							return projectTokenMsg("项目 token", token)
```

3c. `case "e"` 的发射（L311-317）同样改为 `return projectTokenMsg("项目 token（已轮换）", token)`。

3d. handler（L458-466）`case tokenIssuedMsg:` 里 `a.overlay = &secretView{title: m.title, body: m.token}` 改为：

```go
		a.overlay = &secretView{title: m.title, body: m.body()}
```

- [ ] **Step 4: 跑测试确认通过 + 全包回归**

Run: `go test ./internal/tui/ -run 'TestProjectTokenMsg|TestProjectsKey' -v && go test ./internal/tui/`
Expected: 新测试 PASS；全包绿。若 `TestProjectsKeyA` 的按键驱动因 huh 输入框焦点与预期不符而挂，允许微调 press 序列（先跑 `go test -run TestProjectsKeyA -v` 看表单停在哪个字段），但**不得**绕过真实 keypress→formOverlay→action() 链路。

- [ ] **Step 5: 手工冒烟（可选但推荐）**

```bash
SSHMGR_STORE=$(mktemp -d)/s.db SSHMGR_FILEKEY_PATH=$(mktemp -d)/k go run ./cmd/ssh-manager tui
```
空 vault 走向导建 profile+project，或直接在 Projects 页 `a`——确认全屏含双形态真 token 片段。退出后删除临时目录。

- [ ] **Step 6: Commit**

```bash
git add internal/tui/app.go internal/tui/projects_test.go
git commit -m "feat(tui): Projects 页 a/e 发 token 补全双形态引导(stdio+http 真片段)

projectTokenMsg 统一构造; handler 改用 body(); 流程级断言钉死发射点接线。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: `clientFinishScreen(serveURL)` 双形态 + 调用点守卫

**Files:**
- Modify: `internal/tui/clientpage.go`（L194 调用点；L411-431 函数）
- Test: `internal/tui/clientpage_test.go`（`TestClientFinishScreen` 改造 + 两个新测试）

**Interfaces:**
- Consumes: Task 1 的 `mcpHttpConfigLines`/`stdioEnvLine`。
- Produces: `func clientFinishScreen(serveURL string) overlay`（签名变更，包内唯一调用点 L194 同步改）。

- [ ] **Step 1: 写失败测试**

`internal/tui/clientpage_test.go`：把 `TestClientFinishScreen`（L145-159）替换为以下三个测试：

```go
// TestClientFinishScreen_DualForms: 离线 --cache 为主 + 在线 http 为辅；
// http 块 Bearer 是固定占位（client 机从不持有 project token——两道闸门
// 模型），token 走 env 不走 argv。
func TestClientFinishScreen_DualForms(t *testing.T) {
	v := clientFinishScreen("https://192.0.2.5:7878").View().Content
	for _, want := range []string{
		`"args": ["mcp", "--cache"],`,
		`"SSHMGR_TOKEN": "<project token>"`,
		`"type": "http",`,
		`"url": "https://192.0.2.5:7878"`,
		`"Authorization": "Bearer <server 机 Projects 页签发的 token>"`,
		"必填",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("finish screen missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "--token") {
		t.Fatalf("token must ride env, not argv:\n%s", v)
	}
}

// TestClientFinishScreen_EmptyURL: 空 serveURL 渲染 <serve URL> 占位不 panic。
func TestClientFinishScreen_EmptyURL(t *testing.T) {
	v := clientFinishScreen("").View().Content
	if !strings.Contains(v, "<serve URL>") {
		t.Fatalf("empty URL must render the placeholder:\n%s", v)
	}
}

// TestClientWizard_FinishScreenUsesCredURL — 流程级（spec §4.3 调用点锚）：
// pull 成功链路把 m.cred.URL 传进 finish 屏；m.cred == nil 时守卫传空串，
// 渲染占位且不 panic（nil 防御职责在调用点，此处钉死调用点真的判了空）。
func TestClientWizard_FinishScreenUsesCredURL(t *testing.T) {
	m := newClientModel()
	m.wizard = true
	m.cred = &clientops.CacheCred{URL: "https://192.0.2.5:7878"}
	nm, _ := m.Update(pullSucceededMsg{})
	v := nm.(clientModel).overlay.View().Content
	if !strings.Contains(v, `"url": "https://192.0.2.5:7878"`) {
		t.Fatalf("finish screen must carry the connected serve URL:\n%s", v)
	}

	mNil := newClientModel()
	mNil.wizard = true // cred == nil
	nm2, _ := mNil.Update(pullSucceededMsg{})
	v2 := nm2.(clientModel).overlay.View().Content
	if !strings.Contains(v2, "<serve URL>") {
		t.Fatalf("nil cred must fall back to the placeholder (no panic):\n%s", v2)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestClientFinishScreen|TestClientWizard_FinishScreen' -v`
Expected: 编译失败 `not enough arguments in call to clientFinishScreen`。

- [ ] **Step 3: 实现**

3a. `clientpage.go` L194 调用点改为（nil/空防御职责在此，spec §4.2）：

```go
		// nil/空防御职责在调用点（spec rev3 §4.2）：判空后传 ""，杜绝在
		// 传参处解引用 nil cred；函数内只对空串渲染占位。
		serveURL := ""
		if m.cred != nil {
			serveURL = m.cred.URL
		}
		m.overlay = clientFinishScreen(serveURL)
```

3b. `clientFinishScreen`（L411-431）整体替换为：

```go
// clientFinishScreen is the CLIENT role's .mcp.json finish screen (T5): the
// offline --cache form FIRST (the recommended default for the machine that
// just pulled a cache), plus the ONLINE http form for always-on setups — the
// same project token works for both. serveURL is passed AS-IS from the stored
// cred (trailing-slash invariance verified experimentally: the serve handler
// is root-mounted and path-agnostic); an empty value renders "<serve URL>".
// The http block's Bearer is a FIXED placeholder — the client machine never
// holds the project token (the device code in cache.auth.json authorizes
// pulls only; the agent's MCP auth is the project token minted on the server
// machine's Projects page). Token rides env, not argv (ps/proc visibility —
// Plan 20 B2).
func clientFinishScreen(serveURL string) overlay {
	if serveURL == "" {
		serveURL = "<serve URL>"
	}
	offline := mcpConfigLines(
		[]string{
			`"args": ["mcp", "--cache"]`,
			stdioEnvLine("<project token>"),
		},
		[]string{
			"client 角色用 --cache 离线缓存模式启动；SSHMGR_TOKEN 填 server 机 Projects 页签发的 project token（不是设备码——设备码只用于拉取缓存，刚才已保存）。",
			`Windows 建议写绝对路径，如 "command": "C:\\Tools\\ssh-manager.exe"。`,
			".mcp.json 含 token，不要提交进 git。",
		})
	online := mcpHttpConfigLines(serveURL, "<server 机 Projects 页签发的 token>", []string{
		`"type": "http" 必填——漏了会被当 stdio 处理并拒绝该条目。`,
		"两种形态用的是同一个 project token（server 机 Projects 页 [a] 新增 / [e] 轮换签发）。",
		".mcp.json 含 token，不要提交进 git。",
	})
	body := strings.Join(append(append(append([]string{
		"—— 离线为主（默认推荐）——",
	}, offline...), "", "—— 在线为主 ——"), online...), "", "按任意键进入 client 面板", ""), "\n")
	return &wizStaticView{title: "配置 agent 的 .mcp.json（client 模式）", body: body}
}
```

- [ ] **Step 4: 跑测试确认通过 + 全包回归**

Run: `go test ./internal/tui/ -run 'TestClientFinishScreen|TestClientWizard' -v && go test ./internal/tui/`
Expected: 新测试 PASS；全包绿（`TestClientWizard_PullFailureReopensFormWithDraft` 等既有用例不受影响）。

- [ ] **Step 5: Commit**

```bash
git add internal/tui/clientpage.go internal/tui/clientpage_test.go
git commit -m "feat(tui): client finish 屏双形态(离线 --cache 为主 + 在线 http 为辅)

serveURL 由调用点判空传入(流程级锚); Bearer 固定占位指向 server 机 Projects 页。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: `docs/tui-single-machine.md`（人·单机 TUI 教程）

**Files:**
- Create: `docs/tui-single-machine.md`

**Interfaces:**
- Consumes: Task 1-4 落地后的实际行为（键位/流程以此为准）。
- Produces: 文档本体（Task 8 联动、Task 9 docsync 引用）。

- [ ] **Step 1: 事实核对（成文前，spec §4.4-7 纪律）**

对照 `internal/tui` 源码逐条核实以下断言（发现与下文不符处**以代码为准改写教程**，不改代码）：
- 向导首屏问句原文：`internal/tui/wizard.go` newWizard 的「这台电脑要保管所有 SSH 凭据吗？」及两个选项文案；
- 服务器表单密码/密钥互斥与 sudo 可选：`newServerForm`；
- 「继续添加？」循环：`wizard.go` stepServerLoop；
- profile+grant 表单与「未选=agent 暂时看不到任何服务器」提示：`wizProfileGrantForm`；
- 主控台四页签键位：`app.go` 各 `case "a"/"e"/"d"/"i"/"!"/"g"`；
- 4 页签无条件存在（单机也有设备码页）：`app.go` fetchPages；
- mintty/winpty、非 TTY 报错：`README.md` TUI 节既有文案。

- [ ] **Step 2: 成文**

按以下骨架写 `docs/tui-single-machine.md`（全部中文；标题/小节名照抄）：

```markdown
# TUI 教程 · 单机版（ssh-manager tui）

> **读者**：拿到 ssh-manager 单机版 exe、想全程用键盘点选（不想记 CLI 命令）的人。
> 与 [quickstart-single-machine.md](./quickstart-single-machine.md)（CLI 速通）殊途同归——同一套
> vault 操作的两个入口。概念模型图解见 [concepts.md](./concepts.md)。

## 1. 启动
（ssh-manager tui；Windows Terminal/cmd 原生可用；mintty 需 winpty ssh-manager tui；
非 TTY 直接报错不挂死。）

## 2. 首跑向导走查（可中断续配）
（空机进向导 → 问句原文 + 选「是——凭据只存这台机」→ 单机 → 自动建 vault——含
已锁 vault 的引导报错分支（先跑 ssh-manager unlock）→ 录服务器表单（密码/密钥二选一
强制、sudo 密码可选、结构化备注别放机密）→「继续添加？」循环 → Profile 名称 +
授权多选（空格勾选；未选=agent 暂时看不到任何服务器）→ project → token 屏（用途 +
仅此一次丢失→提示）→ .mcp.json 配置屏（args:["mcp"] + env SSHMGR_TOKEN）→ 主控台。
role.json 即存——任何时刻退出都是安全暂停，重跑 tui 回到流程。）

## 3. 主控台四页签
| 页签 | 键 | 动作 |（照 app.go 实测键位填全表；设备码页注明：
**仅 serve 联机部署时有用，单机忽略**——页签无条件存在。）

## 4. 典型任务
### 给第二个 agent 发 token（Projects 页 [a]）
（走查新 secretView：双形态片段——stdio 块真 token 已代入 + http 块 <serve URL>
占位注明「未部署 serve 可忽略本块」；丢失→[e] 轮换。）
### 加服务器 / 批量导入 ssh config
（a 表单；i 导入多选→补全循环→Esc 跳过保留 ⚠；! 只看 ⚠ 待处理。）
### 轮换 token（Projects 页 [e]）

## 5. 排错
（mintty/winpty；vault 锁定→unlock；q/Ctrl+C 退出；表单内 Esc 取消。）

## 6. 安全面
（凭据输入全程掩码；已设凭据只显示「已设置」；token/设备码一次性全屏显示，关闭后不可再查。）

## 相关文档
（getting-started.md / agent-access.md / agent-tools.md / tui-multi-machine.md 链接）
```

- [ ] **Step 3: 自查**

- 事实核对表逐条过了一遍（Step 1 清单）；
- 相对链接目标全部存在；
- 不含机密；JSON 片段（如有）用 Task 1 golden 形态（stdio 多行、token 写 `<TOKEN>`）。

- [ ] **Step 4: Commit**

```bash
git add docs/tui-single-machine.md
git commit -m "docs: TUI 教程·单机版(首跑向导走查+四页签参考+典型任务+排错)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `docs/tui-multi-machine.md`（人·联机 TUI 教程）

**Files:**
- Create: `docs/tui-multi-machine.md`

**Interfaces:**
- Consumes: Task 4 落地后的 clientFinishScreen 双形态；`internal/tui/wizard.go` server 流（T4）双密钥屏。
- Produces: 文档本体（Task 8/9 引用）。

- [ ] **Step 1: 事实核对（成文前）**

对照源码核实：server 向导双密钥屏文案（`wizard.go` tokenIssuedMsg case 的「密钥 1/2」+ 设备码「密钥 2/2」）；serve 安装在向导内非阻断（`wizardData.installErr` 进横幅）；client 面板 `s`（10s 超时、失败保留旧缓存）/`c`（设备码掩码不预填、留空=不变）/`t`（TTL 行文案原文）；页头摘要字段（`clientHeader`：host/pin/服务器数/缓存年龄）。

- [ ] **Step 2: 成文**

骨架（全部中文）：

```markdown
# TUI 教程 · 联机版（serve 多机共享）

> **读者**：server 机（常驻 broker）和工作机（client）两边的操作者。
> 架构/runbook 深水区（TLS 迁移、证书轮换、export/import）在
> [multi-machine.md](./multi-machine.md)——本篇只讲 TUI 怎么点。

## 全景图
（ASCII 图：server 机跑 serve + 主控台；工作机 client 面板 + 本地缓存；
对齐 multi-machine.md 架构图风格，两张视角标注谁在哪台机操作。）

## server 侧走查
（空机 tui → 是→分享给其他机→server → vault/录服务器/grant → 双密钥屏：
「密钥 1/2：project token」用途标注给 client 机 .mcp.json、「密钥 2/2」设备码 →
serve 服务安装（向导内非阻断；结果进横幅）→ 主控台四页签（设备码页此时有用：
[a] 签发/[d] 吊销）。）

## client 侧走查
（空机 tui → 否→client（需先在 server 机完成设置）→ 连接表单（serve 地址/
设备码/pin + 源提示「设备码与服务器指纹在 server 机 TUI『设备码』页签发」）→
首次 pull（失败分类横幅：地址不通/设备码无效/指纹失配/超时，输入保留）→
**finish 屏双形态**：离线为主（--cache，默认推荐，笔记本常出门选这个）/
在线为主（http + <serve URL>；Bearer 是占位——真 token 在 server 机 Projects 页
[a]/[e] 签发）→ client 面板。）

## client 面板参考
（页头连接摘要；s/c/t 键位表 + 零远程写语义。）

## 典型任务
### 新工作机接入全流程
（server 设备码页 [a] → client 向导 → agent 验证 list_servers。）
### 机器失窃处置
（server 机：设备码页 [d] + Projects [e] 轮换；如实注明「已拉下的 cache.bin
仍能被那台机的 DEK 解密——吊销只断拉新，敏感时轮换服务器凭据」。）
### 在线/离线 .mcp.json 互切
（改配置 + 重启客户端；两种形态同一个 project token。）

## 排错
（指纹失配≠泄露（serve 重签证书 vs MITM）；无 pin hard-fail 与 --allow-plaintext
只在 CLI；缓存 TTL/自动保鲜；断连语义四层→agent-access.md。）

## 相关文档
```

- [ ] **Step 3: 自查 + Commit**

同 Task 5 Step 3 清单；commit message：`docs: TUI 教程·联机版(server 侧双密钥屏+client 双形态 finish 屏+典型任务)`。

---

### Task 7: `docs/agent-tools.md`（AI agent 视角工具手册）

**Files:**
- Create: `docs/agent-tools.md`

**Interfaces:**
- Produces: 文档本体 + 文末「行为依据」核对表（Task 8/9 引用；CLAUDE.md 模板被 quickstart 引用）。

- [ ] **Step 1: 事实核对（成文前，spec §4.6 硬要求）**

用以下命令逐条锚定数值/行为断言，核对结果记入文末「行为依据」表（文件:行号 或 文档节名）；**以代码为准，禁止凭记忆写数值**：

```bash
grep -rn "120.*Second\|5.*Minute\|execTimeout\|ExecTimeout" internal/mcpserver internal/sshbroker | head
grep -rn "1.*MiB\|MaxOutput\|OutputCap\|truncated" internal/mcpserver internal/sshbroker | head
grep -rn "10.*Minute\|tunnel.*reclaim\|Reclaim" internal/mcpserver | head
grep -rn "no_credential\|ErrReadOnly\|is not in your profile" internal/m | head
grep -rn "credential files detected\|stray" internal/mcpserver | head
grep -rn "sudo -S" internal/mcpserver internal/sshbroker | head
```

- [ ] **Step 2: 成文**

骨架（中文；直接称「你」；铁律 → 工作流 → 逐工具 → 错误对照 → 三态环境 → 附录模板 → 行为依据）：

```markdown
# 给 AI agent 的 SSH 工具使用手册（docs/agent-tools.md）

> **你是谁**：你是一个拿到 ssh MCP 工具的 AI agent（Claude Code / Cursor / 任何
> MCP 客户端）。这份手册教你安全高效地用它们。

## 铁律
（MCP 工具是**唯一**授权入口；禁裸 ssh/scp/从文件系统找私钥——那是旁路，broker
启动时检测到散落凭据会 stderr 告警。你永远拿不到任何密码/私钥字节——这是设计。）

## 标准工作流
（先 list_servers 拿真实 id——**name ≠ id**，跨工具引用一律用 id；动手前读该机的
caveats/role/services（owner 给的操作须知）；has_sudo=false 别尝试提权。）

## 逐工具语义
### list_servers（字段全解：id/name/host/user/has_sudo + role/services/location/
hardware/caveats/tags/description；永不含凭据）
### exec_command（sudo=true 让 broker 跑 sudo -S——**别自己拼 sudo 前缀**；
连接+执行共享 120s 默认 / 5min 硬上限——长任务拆步或 nohup 后台化；每通道输出
1 MiB 封顶，truncated=true 时 refine（tail/grep/head）而不是硬拉。）
### download_file（单文件、大小帽、截断标志；目录树→先 exec tar 再下。）
### upload_file（本地文件**或目录**递归；preflight 拒绝条件按行为依据表实写。）
### forward_port（返回 127.0.0.1:<port>；只支持本地 -L；创建后 ~10 分钟自动回收，
用完主动 close_port。）
### close_port（401=token 已失效，别重试开新隧道。）

## 错误对照表（每条给"你该做什么"）
| 报错 | 含义 | 你该做 |
|---|---|---|
（token 无效/被轮换/被禁用→报告 owner；server is not in your profile→重新
list_servers 核对 id；no_credential→报告 owner；timeout→拆小命令；truncated→
refine；host key 失配（TOFU fail-closed）→报告 owner 核实，**别**尝试绕过。）

## 三态环境（你通常无需分辨）
单机/stdio（本机 broker，**可写**）｜在线 serve（远程，可写）｜离线 cache（只读）。
遇 ErrReadOnly/read-only → 报告 owner 切在线/本机，**别**重试写操作。

## 附录：贴进你项目的规则模板（CLAUDE.md / AGENTS.md）
```text
# SSH 访问铁律
所有远程服务器操作（执行命令/传文件/端口转发）一律用 mcp__ssh__* 工具，
禁止裸 ssh/scp/寻找私钥（本机没有可用凭据，直连必失败）。
- 先 list_servers 拿真实 id（name ≠ id），动手前读目标机的 caveats/role。
- 提权用 sudo=true 参数，不要自己拼 sudo 前缀。
- 工具报错先查 docs/agent-tools.md 错误对照表；read-only 报错=离线缓存，
  报告 owner，不要重试写操作。
（按需替换工具前缀 mcp__ssh__* 为你的客户端实际命名。）
```

## 行为依据（事实核对表）
| 断言 | 锚点 |
|---|---|
（每条数值/行为一行：120s/5min、1 MiB、~10min 回收、sudo -S、ErrReadOnly、
stderr 告警、preflight 条件——Step 1 的核对结果。）
```

- [ ] **Step 3: 自查 + Commit**

行为依据表无空行；链接目标存在；commit message：`docs: agent 工具手册(铁律+逐工具语义+错误对照+三态环境+CLAUDE.md 模板+行为依据表)`。

---

### Task 8: 文档联动（README / quickstart ×2 / agent-access / multi-machine / docs/README）

**Files:**
- Modify: `README.md`、`docs/quickstart-single-machine.md`、`docs/quickstart-multi-machine.md`、`docs/agent-access.md`、`docs/multi-machine.md`、`docs/README.md`

**Interfaces:**
- Consumes: Task 5-7 的三篇新文档文件名。

- [ ] **Step 1: 六处联动（逐文件小步改，每处一 Edit）**

1. `README.md` Documentation 表（两表合并区）加三行：

```markdown
| **单机 TUI 教程**（全键盘点选，不想记命令） | [`docs/tui-single-machine.md`](docs/tui-single-machine.md) |
| **联机 TUI 教程**（server 侧 + 工作机 client 面板） | [`docs/tui-multi-machine.md`](docs/tui-multi-machine.md) |
| **给 AI agent 的工具手册**（可贴进 CLAUDE.md 的规则模板在内） | [`docs/agent-tools.md`](docs/agent-tools.md) |
```

2. `README.md` TUI 节瘦身：保留「启动与模式判定」「终端要求（mintty 注意）」「安全面」三小节，页签/键位明细段替换为一行：`各页签键位与典型任务走查见 [docs/tui-single-machine.md](docs/tui-single-machine.md) / [docs/tui-multi-machine.md](docs/tui-multi-machine.md)`。
3. `README.md` Multi-machine 节 http 片段（~L209）后补一句：`> client 角色向导的 finish 屏现在会同时展示离线 --cache 与在线 http 两种形态。`
4. `docs/quickstart-single-machine.md` Step 3 的 TUI 提示行改为指向 `tui-single-machine.md`；「接下来」清单加一行 `- TUI 全程点选教程 → tui-single-machine.md；把 agent-tools.md 的规则模板贴进 CLAUDE.md，agent 会更守规矩`。
5. `docs/quickstart-multi-machine.md` 对应位置（Step 2 附近）加两行指针（tui-multi-machine.md / agent-tools.md）。
6. `docs/agent-access.md` 顶部引言后加：`> 发完卡，把 [agent-tools.md](./agent-tools.md)（或其附录规则模板）给 agent 一份——它会更守规矩。`
7. `docs/multi-machine.md`「离线只读缓存」Step 2 双形态片段后加：`> 片段权威源 = 代码渲染器 + golden 测试（internal/tui/wizardsteps*.go）；文档片段如与之不符以代码为准。TUI 操作教程见 [tui-multi-machine.md](./tui-multi-machine.md)。`
8. `docs/README.md`「目录」表加 3 行（同第 1 条措辞）；「两个角色」表 Agent 行「用什么」列补 `；手册：agent-tools.md`。

- [ ] **Step 2: 链接自查**

```bash
grep -rhoP '\]\((\./)?[a-zA-Z0-9./_-]+\.md' README.md docs/*.md | grep -oP '[a-zA-Z0-9./_-]+\.md' | sort -u
```
逐个确认文件存在（新增的 tui-single/tui-multi/agent-tools 必须在列）。

- [ ] **Step 3: Commit**

```bash
git add README.md docs/quickstart-single-machine.md docs/quickstart-multi-machine.md docs/agent-access.md docs/multi-machine.md docs/README.md
git commit -m "docs: 三篇新文档全链路联动(README 表+TUI 节瘦身+quickstart 指针+互链+权威源声明)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: `wizardsteps_docsync_test.go` — 文档片段自动比对

**Files:**
- Create: `internal/tui/wizardsteps_docsync_test.go`

**Interfaces:**
- Consumes: Task 1 的 `mcpConfigLines`/`stdioEnvLine`/`mcpHttpConfigLines`/`jsonBlockOf` + `goldenStdioBlock`。
- Produces: docs 漂移自动报警（R3 第三层缓解）。

**已知文档块清单**（写期望集的依据，Step 1 实测复核）：README L87 单行 stdio / L209 单行 http；quickstart-single L73 单行 stdio；quickstart-multi L80 多行 cache；getting-started L145 单行 stdio / L159 多行 stdio / L269 多行 stdio+MASTERKEY_HEX；agent-access L35 单行 stdio / L54 多行 stdio / L71 多行 stdio+MASTERKEY_HEX；multi-machine L165 多行 http / L344 多行 http / L357 多行 cache。Task 5-8 新增的三篇文档中的片段（如有）一并纳入。

- [ ] **Step 1: 写测试（先写好，文档已在前任务落位，直接应绿；若红，修文档不是修测试）**

```go
package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docNorm canonicalizes a raw .mcp.json snippet block: dynamic values →
// placeholders, then whitespace-compacted — so CLI 单行输出与 TUI 多行渲染
// 两种合法排版在规范化后可比（逐字节口径由 Task 1 的 in-code golden 锁定，
// 本测试锁语义等价 + 占位纪律）。
var (
	reDocToken  = regexp.MustCompile(`("SSHMGR_TOKEN":\s*")[^"]*(")`)
	reDocBearer = regexp.MustCompile(`("Authorization":\s*"Bearer )[^"]*(")`)
	reDocURL    = regexp.MustCompile(`("url":\s*")[^"]*(")`)
	reDocHex    = regexp.MustCompile(`("SSHMGR_MASTERKEY_HEX":\s*")[^"]*(")`)
)

func docNorm(raw string) string {
	s := reDocToken.ReplaceAllString(raw, "${1}<TOKEN>${2}")
	s = reDocBearer.ReplaceAllString(s, "${1}<TOKEN>${2}")
	s = reDocURL.ReplaceAllString(s, "${1}<URL>${2}")
	s = reDocHex.ReplaceAllString(s, "${1}<HEX>${2}")
	var buf strings.Builder
	if err := json.Compact(&buf, []byte(s)); err != nil {
		panic("docNorm: " + err.Error())
	}
	return buf.String()
}

// extractMcpBlocks scans a doc for every {"mcpServers"...} object (balanced
// braces, works for single-line and pretty blocks).
func extractMcpBlocks(t *testing.T, doc string) []string {
	t.Helper()
	var blocks []string
	for i := 0; i < len(doc); i++ {
		if !strings.HasPrefix(doc[i:], `"mcpServers"`) {
			continue
		}
		start := strings.LastIndexByte(doc[:i], '{')
		if start < 0 {
			continue
		}
		depth := 0
		for j := start; j < len(doc); j++ {
			switch doc[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					blocks = append(blocks, doc[start:j+1])
					i = j
					j = len(doc)
				}
			}
		}
	}
	return blocks
}

// TestDocsMcpSnippetsMatchGolden: 每篇文档里的每个 mcpServers 块，规范化后
// 必须命中四条 canonical 之一（stdio / cache / http / stdio+masterkey-hex）。
func TestDocsMcpSnippetsMatchGolden(t *testing.T) {
	canonical := map[string]bool{
		docNorm(jsonBlockOf(mcpConfigLines([]string{`"args": ["mcp"]`, stdioEnvLine("<TOKEN>")}, nil))):                true,
		docNorm(jsonBlockOf(mcpConfigLines([]string{`"args": ["mcp", "--cache"]`, stdioEnvLine("<TOKEN>")}, nil))):     true,
		docNorm(jsonBlockOf(mcpHttpConfigLines("https://192.0.2.5:7878/", "<TOKEN>", nil))):                            true,
		docNorm(`{"mcpServers": {"ssh": {"command": "ssh-manager", "args": ["mcp"], "env": {"SSHMGR_TOKEN": "<TOKEN>", "SSHMGR_MASTERKEY_HEX": "<HEX>"}}}}`): true,
	}
	docs := []string{
		"../../README.md",
		"../../docs/quickstart-single-machine.md",
		"../../docs/quickstart-multi-machine.md",
		"../../docs/getting-started.md",
		"../../docs/agent-access.md",
		"../../docs/multi-machine.md",
		"../../docs/tui-single-machine.md",
		"../../docs/tui-multi-machine.md",
		"../../docs/agent-tools.md",
	}
	for _, p := range docs {
		b, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, blk := range extractMcpBlocks(t, string(b)) {
			if !canonical[docNorm(blk)] {
				t.Errorf("%s: snippet not in canonical forms:\n%s", p, blk)
			}
		}
	}
}
```

（注：若某文档应含片段却一个块都没被抽出——例如写成了非法嵌套——测试不会报；这是已知盲区，由 Task 10 的人工链接检查兜底，不在本测试修。）

- [ ] **Step 2: 跑测试**

Run: `go test ./internal/tui/ -run TestDocsMcpSnippetsMatchGolden -v`
Expected: PASS。若 FAIL：**修文档片段**（把漂移的 JSON 块改成 canonical 形态——值占位 `<TOKEN>`/`<项目token>` 均可，规范化会归一），**不得**改测试或渲染器。

- [ ] **Step 3: 全包回归 + Commit**

Run: `go test ./internal/tui/` → 全绿。

```bash
git add internal/tui/wizardsteps_docsync_test.go
git commit -m "test(tui): docs 片段自动比对(四 canonical 形态,R3 第三层缓解)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

（若 Step 2 修了文档，`git add` 一并带上。）

---

### Task 10: 终验

**Files:**
- 无新增（验证 + 收尾）。

- [ ] **Step 1: 全量构建与测试**

```bash
go build ./... && go test ./...
```
Expected: 全绿（不止 internal/tui——文档测试动了 docs 依赖，全仓跑一遍）。

- [ ] **Step 2: spec 验收清单逐条勾**

对照 spec rev3 §5：
1. ✅ build/test 全绿（Step 1）；
2. ✅ 手工验证已在 Task 3 Step 5（若跳过，补做一次）；
3. ✅ client 路径由 clientpage_test 覆盖（Task 4）；
4. ✅ 三篇新文档存在 + 链接无死链（Task 8 Step 2 的 grep 重跑一遍收尾）；
5. ✅ 片段一致性：golden（Task 1）+ docsync（Task 9）双层自动锚；
6. ✅ agent-tools.md 行为依据表落地（Task 7）；
7. ✅ 两篇 TUI 教程事实核对完成（Task 5/6 Step 1）；
8. 版本 bump 留给 merge 时（不在本计划内）。

- [ ] **Step 3: 收尾 commit（如有零星遗漏修复）**

```bash
git add -A && git commit -m "chore: spec §5 验收清单终验收尾

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```
（工作区已干净则跳过本步。）

---

## Self-Review 记录

- **Spec 覆盖**：§4.1（T2/T3）、§4.2（T1/T4）、§4.3（T1-T4 各测试行逐条对应）、§4.4（T5）、§4.5（T6）、§4.6（T7）、§4.7（T8）、§5.5/5.6/5.7（T1/T9/T7）、§5 其余（T10）——无缺口。
- **占位符扫描**：无 TBD/TODO；代码步骤全量贴码；文档任务给出完整骨架+全部硬事实+golden 片段。
- **类型一致性**：`jsonValue`/`stdioEnvLine`/`mcpHttpConfigLines`/`jsonBlockOf`/`projectTokenMsg`/`clientFinishScreen(serveURL)` 各任务间签名一致（Interfaces 块互锁）。
- **已知裁量**（实施者须知）：① `driveForm` 的按键序列若与 huh 焦点实际不符，允许按 `-v` 输出微调序列，但不得绕过真实链路（Task 3 Step 4 注记）；② docsync 采用「规范化后语义等价」比对而非全文逐字节——逐字节口径由 in-code golden 锁定（Task 9 注记，spec §5.5 的实操化解释）。
