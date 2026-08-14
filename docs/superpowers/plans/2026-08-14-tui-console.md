# Stream B 双端 TUI 主控台 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ssh-manager tui` 单命令双模式全屏主控台——broker 机上做全集运维（服务器/凭据/Profile/Project+token/设备码），client 机上做连接配置+缓存状态+同步。

**Architecture:** bubbletea v2 顶层 App（页签路由+页面栈），多字段操作推入 huh v2 表单页（`Value(&draft)` 指针绑定），lipgloss v2 样式。所有写操作直调 store 层既有 API；client 模式只读 cache + 调 clientops.DoPull。前置 Task 1 把 cache 函数族从 `internal/cli` 抽到 `internal/clientops`（cli 将 import tui，tui 不得反向 import cli）。

**Tech Stack:** `charm.land/bubbletea/v2`、`charm.land/huh/v2`、`charm.land/lipgloss/v2`（MIT，纯 Go 无 CGO）。spec：`docs/superpowers/specs/2026-08-14-tui-console-design.md`。

## Global Constraints

- **import 路径一律 v2**：`tea "charm.land/bubbletea/v2"`、`"charm.land/huh/v2"`、`"charm.land/lipgloss/v2"`（严禁 github.com/charmbracelet 旧路径——v1/v2 类型不互通）。
- **零 CGO**（`CGO_ENABLED=0` 交叉编译不能破坏；收尾用 `goreleaser build --snapshot` 验证）。
- **client 端零远程写**：不得给 serve 加任何新端点/API；tui 的 client 模式只读 cache + DoPull。
- **凭据/token 永不明文回显**：表单输入用 `EchoMode(huh.EchoModePassword)`；已存凭据显示「已设置（输入新值以更换）」；token/设备码只在生成瞬间显示一次。
- **serve/MCP/iron rule 零改动**：不碰 internal/mcpserver、internal/sshbroker、serve 逻辑。
- 每个 TUI 实体操作必须走 store 层既有方法（校验/审计与 CLI 完全同路）。
- 代码注释英文，文档中文；提交信息 `feat(tui):` / `refactor:` / `test:` / `docs:` 前缀。
- **执行前**：isolated linked worktree 开工（superpowers:using-git-worktrees）。

---

### Task 1: `internal/clientops` 抽包（纯迁移）

**Files:**
- Create: `internal/clientops/clientops.go`、`internal/clientops/pin.go`、`internal/clientops/dek.go`（从 cli 迁移；`cache_dek_windows.go`/`cache_dek_unix.go` 同迁）
- Modify: `internal/cli/cache.go`（删除被迁代码，cobra 命令改调 clientops）、`internal/cli/mcp.go`（同）、`internal/cli/cache.go` 系测试文件（拆分迁移）
- Test: `internal/clientops/*_test.go`（随代码迁移的测试）+ 既有 cli 测试全绿

**Interfaces:**
- Produces（tui 与 cli 后续任务依赖的**导出**名，签名逐字）：
  - `func CachePaths() (dir, bin, meta, audit string, err error)`
  - `type CacheCred struct { URL, Token, Pin string }`（json: `url`/`token`/`pin,omitempty`）
  - `func ReadCacheCred() (*CacheCred, error)` / `func WriteCacheCred(cred *CacheCred) error` / `func CacheCredPath() (string, error)`
  - `type PullOpts struct { AllowPlain bool; Timeout time.Duration; StatusOut io.Writer }`
  - `func DoPull(url, token, pin string, o PullOpts) error`
  - `func LoadCacheSnapshot() (*store.Snapshot, error)`
  - `func MaybeLazyPull(maxAge time.Duration) error` + `func ResetLazyPullBackoffForTest()`
  - `type CacheReloader struct{...}` + `func NewCacheReloader(maxAge time.Duration) *CacheReloader` + `func (r *CacheReloader) Check() (*store.Snapshot, bool, error)`
  - `var DekProvider func() store.KeyProvider`（测试 seam；默认现 cli 的 dekProvider 内容）
  - `const LazyPullTimeout = 10 * time.Second`

**迁移规则（逐条执行，零行为变更）：**
1. 从 `internal/cli/cache.go` 迁出（连注释）：`atomicWriteUnique`、`cacheMeta`、`cachePaths`→`CachePaths`（导出）、`cacheCred` 族→`CacheCred` 族（导出）、`loadOrCreateDEK`/`loadDEK`（留私有，改用包内 `DekProvider`）、`pullOpts`/`doPull`→导出、`resolvePin`/`stripEmbeddedPin`/`pinningTransport`（留私有）、`loadCacheSnapshot`→`LoadCacheSnapshot`、`lazyPullTimeout`/`lazyPullBackoff`/`maybeLazyPull`→导出 `MaybeLazyPull`（backoff 留私有 + 导出 `ResetLazyPullBackoffForTest`）、`cacheReloader` 族→`CacheReloader`（导出）。
2. `internal/cli/cache_dek_windows.go`/`cache_dek_unix.go` 整文件迁为 `internal/clientops/dek_windows.go`/`dek_unix.go`，seam 变量改 `DekProvider`（导出 var，包内 `loadOrCreateDEK`/`loadDEK` 调它）。
3. `internal/cli` 内所有残留引用改 `clientops.X`（`cachePullCmd`/`cacheStatusCmd`/`mcp.go` 的 `maybeLazyPull`→`clientops.MaybeLazyPull`、`newCacheReloader`→`clientops.NewCacheReloader`、`rel.check`→`rel.Check` 等）。
4. 测试迁移：`cache_atomic_test.go`、`cache_cred_test.go`、`cache_lazy_test.go`、`cache_reload_test.go` 整体迁为 `internal/clientops/` 包内测试（引用改导出名；`resetBackoff()`→`ResetLazyPullBackoffForTest()`；`withEnv`/`withDEK` 在 clientops 包内重建副本）。`cache_test.go` 与 `cache_pull_integration_test.go`（驱动 cobra 的）留在 cli，其中对被迁私有名的引用改 `clientops.`（`withDEK` 改设 `clientops.DekProvider`）。

- [ ] **Step 1: 迁移前全量基线**

Run: `go build ./... && go test ./internal/cli/ -count=1`
Expected: 全绿（记录测试数）

- [ ] **Step 2: 执行迁移（上述 4 条规则）**

cli 里留下的 `cache.go` 只剩 cobra 命令定义（pull/status）薄封装 + `newCacheCmd`。注意 `mcp_cache_test.go` 里 `loadCacheSnapshot()` 等 → `clientops.LoadCacheSnapshot()`。

- [ ] **Step 3: 全量回归**

Run: `go build ./... && go vet ./... && go test ./internal/cli/ ./internal/clientops/ ./internal/mcpserver/ -count=1`
Expected: 基线测试数不减、全 PASS（迁移是零行为的）

- [ ] **Step 4: Commit**

```bash
git add -A internal/clientops internal/cli
git commit -m "refactor: extract internal/clientops from cli cache layer (zero-behavior move for tui reuse)"
```

---

### Task 2: Charm v2 依赖 + `tui` 子命令 + 模式判定

**Files:**
- Modify: `go.mod`/`go.sum`（go get 自动）
- Create: `internal/tui/mode.go`、`internal/tui/mode_test.go`
- Modify: `internal/cli/root.go`（注册）、Create `internal/cli/tui.go`

**Interfaces:**
- Produces:
  - `func DetectMode(force string) (Mode, error)`（`internal/tui`）：`ModeBroker`/`ModeClient`；`force ∈ {"", "broker", "client"}`；返回 error 时带用户指引文案
  - `func Run(mode Mode) error`（`internal/tui`）：启动 tea 程序（本任务里是占位 View，Task 3 实装）
  - `type Mode int` + `const (ModeBroker Mode = iota; ModeClient)`

- [ ] **Step 1: 引依赖**

```bash
go get charm.land/bubbletea/v2@latest charm.land/huh/v2@latest charm.land/lipgloss/v2@latest
go mod tidy
```

Run: `go build ./...` — Expected: 零错误（依赖纯 Go）

- [ ] **Step 2: 写模式判定失败测试**

```go
package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// vaultProbe and cacheProbe are injectable for tests (production: real paths).
func TestDetectMode_ForceWins(t *testing.T) {
	for _, c := range []struct{ force string; want Mode }{
		{"broker", ModeBroker}, {"client", ModeClient},
	} {
		got, err := DetectModeWith(c.force, func() bool { return false }, func() bool { return false })
		if err != nil || got != c.want {
			t.Fatalf("force=%q: got (%v,%v)", c.force, got, err)
		}
	}
}

func TestDetectMode_Auto(t *testing.T) {
	// vault present → broker
	if m, err := DetectModeWith("", func() bool { return true }, func() bool { return false }); err != nil || m != ModeBroker {
		t.Fatalf("vault: (%v,%v)", m, err)
	}
	// no vault + cache → client
	if m, err := DetectModeWith("", func() bool { return false }, func() bool { return true }); err != nil || m != ModeClient {
		t.Fatalf("cache: (%v,%v)", m, err)
	}
	// neither → guided error
	if _, err := DetectModeWith("", func() bool { return false }, func() bool { return false }); err == nil {
		t.Fatal("neither vault nor cache must error with guidance")
	}
}
```

- [ ] **Step 3: 跑测试确认失败** — Run: `go test ./internal/tui/ -v` — Expected: FAIL（包不存在符号）

- [ ] **Step 4: 实现 mode.go**

```go
package tui

import (
	"errors"
	"fmt"
	"os"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

type Mode int

const (
	ModeBroker Mode = iota
	ModeClient
)

// vaultPresent reports whether an UNLOCKED vault is reachable on this machine.
// A locked vault is deliberately NOT treated as client-mode (spec §2): probing a
// locked store returns an error we distinguish from "absent".
func vaultPresent() bool {
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return false
	}
	st.Close()
	return true
}

// cachePresent reports whether this machine is an enrolled client.
func cachePresent() bool {
	cred, err := clientops.ReadCacheCred()
	return err == nil && cred != nil
}

// DetectModeWith resolves the run mode: force flag wins, else vault→broker,
// else cache→client, else a guided error.
func DetectModeWith(force string, hasVault, hasCache func() bool) (Mode, error) {
	switch force {
	case "broker":
		return ModeBroker, nil
	case "client":
		return ModeClient, nil
	case "":
	default:
		return 0, fmt.Errorf("invalid --mode %q (want broker|client)", force)
	}
	if hasVault() {
		return ModeBroker, nil
	}
	if hasCache() {
		return ModeClient, nil
	}
	return 0, errors.New("no vault and no cache on this machine: initialize a vault here (broker) or run `cache pull` (client) first")
}

func DetectMode(force string) (Mode, error) {
	return DetectModeWith(force, vaultPresent, cachePresent)
}

// Run starts the console for mode (placeholder view until Task 3).
func Run(mode Mode) error {
	if !isTTY() {
		return errors.New("tui requires a terminal (in mintty run via `winpty ssh-manager tui`, or use Windows Terminal)")
	}
	p := tea.NewProgram(newApp(mode))
	_, err := p.Run()
	return err
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
```

（import 里加 `tea "charm.land/bubbletea/v2"`；`newApp` 本任务先给最小占位：

```go
// app.go (minimal stub for this task; Task 3 replaces it)
type app struct{ mode Mode }

func newApp(m Mode) app { return app{mode: m} }
func (a app) Init() tea.Cmd                                { return nil }
func (a app) Update(msg tea.Msg) (tea.Model, tea.Cmd)      { return a, nil }
func (a app) View() string                                 { return "tui loading… (q to quit)" }
```

q 退出 Task 3 实装；占位期 Ctrl-C 即可。）

- [ ] **Step 5: cli 子命令（internal/cli/tui.go）**

```go
package cli

import (
	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/tui"
)

func newTUICmd() *cobra.Command {
	var mode string
	c := &cobra.Command{
		Use:   "tui",
		Short: "Interactive console (broker: full vault management; client: connection + sync)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := tui.DetectMode(mode)
			if err != nil {
				return err
			}
			return tui.Run(m)
		},
	}
	c.Flags().StringVar(&mode, "mode", "", "force mode: broker|client (default: auto-detect)")
	return c
}
```

root.go 的 `rootCmd.AddCommand(...)` 处加 `newTUICmd()`。

- [ ] **Step 6: 测试过 + build + Commit**

Run: `go test ./internal/tui/ -v && go build ./...`

```bash
git add internal/tui internal/cli/tui.go internal/cli/root.go go.mod go.sum
git commit -m "feat(tui): tui subcommand with vault/cache mode detection; charm v2 deps"
```

---

### Task 3: App 骨架 + broker 服务器页（列表/详情/键位/样式）

**Files:**
- Create: `internal/tui/app.go`（替换占位）、`internal/tui/servers.go`、`internal/tui/style.go`、`internal/tui/app_test.go`、`internal/tui/servers_test.go`

**Interfaces:**
- Consumes: `store.Store`（`ListServers`/`GetServerByName` 等）、models。
- Produces（后续任务在 app 上扩展）:
  - `type App struct` 实现 `tea.Model`；字段：`mode Mode`、`st *store.Store`（client 模式为 nil）、`page page`、`pages [4]listPage`、`overlay overlay`（nil = 无）、`status string`、`err error`
  - `type page int`：`pageServers/pageProfiles/pageProjects/pageTokens`
  - `type listPage interface { Title() string; Rows() []string; Detail() string; Cursor() int; Select(i int) }`（四个实体页共同形状；实现者持 items+cursor）
  - `type overlay interface { tea.Model; Title() string }`（huh 表单页/token 展示页）
  - 键位（全局）：`tab/shift+tab` 切页、`up/down/j/k` 移动、`q/ctrl+c` 退出、`a/e/d` 实体动作（各页自定义，无动作则忽略）
  - `func FetchAll(st *store.Store) ([4]listPage, error)`：一次性装载四页数据（失败原样返回 error 给 App.status）

- [ ] **Step 1: 写失败测试（app 路由 + servers 渲染纯函数）**

```go
package tui

import (
	"testing"

	"github.com/charmbracelet/bubbletea" // DO NOT — use v2:
	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestApp_TabCyclesPages(t *testing.T) {
	a := newTestApp() // seeds store with 1 server/profile/project
	if a.page != pageServers {
		t.Fatalf("start page = %v", a.page)
	}
	m2, _ := a.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m2.(App).page != pageProfiles {
		t.Fatalf("tab: %v", m2.(App).page)
	}
	m3, _ := m2.(App).Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m3.(App).page != pageServers {
		t.Fatalf("shift-tab wrap: %v", m3.(App).page)
	}
}

func TestApp_QuitOnQ(t *testing.T) {
	a := newTestApp()
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q must produce a quit cmd")
	}
}

func TestServersPage_RowsAndDetail(t *testing.T) {
	sp := &serversPage{items: []*models.Server{{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		Hardware: "2x3090", Tags: []string{"gpu"},
	}}}
	if rows := sp.Rows(); len(rows) != 1 || rows[0] != "gpu" {
		t.Fatalf("rows: %v", rows)
	}
	d := sp.Detail()
	for _, want := range []string{"gpu", "192.0.2.10", "2x3090", "gpu"} {
		if !strings.Contains(d, want) {
			t.Fatalf("detail missing %q:\n%s", want, d)
		}
	}
}
```

（`newTestApp` helper：`st := store.Open(t.TempDir()+"/t.db", mk)` 加一台服务器；App{mode: ModeBroker, st: st} + FetchAll。补 `strings` import。）

- [ ] **Step 2: 确认失败** — Run: `go test ./internal/tui/ -v` — Expected: FAIL

- [ ] **Step 3: 实现 style.go / servers.go / app.go**

style.go：

```go
package tui

import "charm.land/lipgloss/v2"

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	selStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	detailStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	footerStyle   = lipgloss.NewStyle().Faint(true)
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	secretStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)
```

servers.go：

```go
package tui

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type serversPage struct {
	items  []*models.Server
	cursor int
}

func (p *serversPage) Title() string  { return "服务器" }
func (p *serversPage) Cursor() int    { return p.cursor }
func (p *serversPage) Select(i int)   { p.cursor = i }
func (p *serversPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, s := range p.items {
		mark := " "
		if i == p.cursor {
			mark = "▶"
		}
		out[i] = fmt.Sprintf("%s %s", mark, s.Name)
	}
	return out
}

func (p *serversPage) Detail() string {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return "(空)"
	}
	s := p.items[p.cursor]
	cred := "已设置（输入新值以更换）"
	if s.CredentialID == "" {
		cred = "未设置"
	}
	return fmt.Sprintf("名称   %s\nHost   %s\n端口   %d\n用户   %s\n凭据   %s（%s）\n硬件   %s\n位置   %s\n角色   %s\n服务   %s\nCaveats %s\n标签   %s\n备注   %s",
		s.Name, s.Host, s.Port, s.User, cred, s.AuthMethod,
		orDash(s.Hardware), orDash(s.Location), orDash(s.Role), orDash(s.Services), orDash(s.Caveats),
		orDash(strings.Join(s.Tags, ",")), orDash(s.Description))
}

func orDash(s string) string { if s == "" { return "-" }; return s }

func (p *serversPage) current() *models.Server {
	if p.cursor < 0 || p.cursor >= len(p.items) { return nil }
	return p.items[p.cursor]
}
```

app.go（骨架版——overlay 与实体动作本任务留 nil/忽略，Task 4-8 填）：

```go
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/store"
)

type page int

const (
	pageServers page = iota
	pageProfiles
	pageProjects
	pageTokens
	pageCount
)

type listPage interface {
	Title() string
	Rows() []string
	Detail() string
	Cursor() int
	Select(i int)
}

type overlay interface {
	tea.Model
	Title() string
}

type App struct {
	mode    Mode
	st      *store.Store
	page    page
	pages   [pageCount]listPage
	overlay overlay
	status  string
	err     error
}

func newApp(mode Mode) App { return App{mode: mode} }

// NewBrokerApp builds the broker console over an open store (caller owns Close).
func NewBrokerApp(st *store.Store) (App, error) {
	pages, err := FetchAll(st)
	if err != nil {
		return App{}, err
	}
	return App{mode: ModeBroker, st: st, pages: pages, status: "就绪"}, nil
}

// FetchAll loads the four entity pages in one shot.
func FetchAll(st *store.Store) ([pageCount]listPage, error) {
	var pages [pageCount]listPage
	servers, err := st.ListServers()
	if err != nil { return pages, err }
	profiles, err := st.ListProfiles()
	if err != nil { return pages, err }
	projects, err := st.ListProjects()
	if err != nil { return pages, err }
	tokens, err := st.ListCacheTokens()
	if err != nil { return pages, err }
	pages[pageServers] = &serversPage{items: servers}
	pages[pageProfiles] = &profilesPage{items: profiles, st: st}
	pages[pageProjects] = &projectsPage{items: projects}
	pages[pageTokens] = &cacheTokensPage{items: tokens}
	return pages, nil
}

func (a App) Init() tea.Cmd { return nil }

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyMsg:
		if a.overlay != nil { // overlay owns keys until done (form overlays send formDoneMsg)
			ov, cmd := a.overlay.Update(msg)
			a.overlay, _ = ov.(overlay)
			return a, cmd
		}
		switch m.Type {
		case tea.KeyCtrlC:
			return a, tea.Quit
		case tea.KeyTab:
			a.page = (a.page + 1) % pageCount
			return a, nil
		case tea.KeyShiftTab:
			a.page = (a.page + pageCount - 1) % pageCount
			return a, nil
		case tea.KeyUp:
			a.move(-1)
			return a, nil
		case tea.KeyDown:
			a.move(1)
			return a, nil
		case tea.KeyRunes:
			switch string(m.Runes) {
			case "q":
				return a, tea.Quit
			case "j":
				a.move(1)
			case "k":
				a.move(-1)
			}
		}
	case errMsg:
		a.err = m.err
		a.status = ""
		return a, nil
	case actionDoneMsg:
		a.err = nil
		a.status = m.desc
		pages, err := FetchAll(a.st)
		if err == nil {
			a.pages = pages
		}
		return a, nil
	case formDoneMsg:
		a.overlay = nil
		return a, m.after // run the deferred action (e.g. re-fetch)
	}
	return a, nil
}

func (a *App) move(d int) {
	p := a.pages[a.page]
	if p == nil { return }
	rows := p.Rows()
	if len(rows) == 0 { return }
	c := p.Cursor() + d
	if c < 0 { c = 0 }
	if c >= len(rows) { c = len(rows) - 1 }
	p.Select(c)
}

type errMsg struct{ err error }
type actionDoneMsg struct{ desc string }
type formDoneMsg struct{ after tea.Cmd }

func (a App) View() string {
	if a.overlay != nil {
		return a.overlay.View()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf(" ssh-manager%s ", modeTag(a.mode))) + "\n")
	tabs := make([]string, pageCount)
	for i := page(0); i < pageCount; i++ {
		t := a.pages[i].Title()
		if i == a.page { t = selStyle.Render("["+t+"]") } else { t = "[" + t + "]" }
		tabs[i] = t
	}
	b.WriteString(strings.Join(tabs, " ") + footerStyle.Render("  Tab 切页") + "\n")
	p := a.pages[a.page]
	rows := p.Rows()
	left := "（空）"
	if len(rows) > 0 {
		// re-render with the selected row highlighted
		for i, r := range rows {
			if i == p.Cursor() { rows[i] = selStyle.Render(r) }
		}
		left = strings.Join(rows, "\n")
	}
	b.WriteString(lipColumns(left, p.Detail()))
	b.WriteString("\n")
	if a.err != nil {
		b.WriteString(errStyle.Render("✗ " + a.err.Error()) + "\n")
	} else if a.status != "" {
		b.WriteString(footerStyle.Render("✓ " + a.status) + "\n")
	}
	b.WriteString(footerStyle.Render(a.footer()))
	return b.String()
}

func modeTag(m Mode) string { if m == ModeClient { return " (client)" }; return "" }

func (a App) footer() string {
	if a.mode == ModeClient { return "[s]同步 [c]编辑连接 [t]TTL  q 退出" }
	return "[a]新增 [e]编辑 [d]删除 [g]授权  Tab 切页  q 退出"
}

// lipColumns renders two columns side by side (width-aware lipgloss join).
func lipColumns(left, right string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}
```

（import 加 `"charm.land/lipgloss/v2"`。其余三个页签结构体本任务以**同构骨架**落地（Title/Rows/Detail/Cursor/Select，items 为各自类型）——`profilesPage{items []*models.Profile; st *store.Store}`、`projectsPage{items []*models.Project}`、`cacheTokensPage{items []*models.CacheToken}`，Rows 输出名称，Detail 输出 id/状态/关联信息；Task 5-7 扩充各自动作。）

- [ ] **Step 4: 测试过 + build + 手工冒烟**（`go run ./cmd/ssh-manager tui`，在 NUC10 前跳过——留给 T9 真机）

Run: `go test ./internal/tui/ -v && go build ./...` — Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): app skeleton with tabbed pages, servers list/detail, lipgloss layout"
```

---

### Task 4: broker 服务器 CRUD（huh 表单 + store 动作）

**Files:**
- Create: `internal/tui/forms.go`、`internal/tui/tokenview.go`
- Modify: `internal/tui/app.go`（a/e/d 键位接入）、`internal/tui/servers.go`
- Test: `internal/tui/forms_test.go`、`internal/tui/actions_test.go`

**Interfaces:**
- Consumes: Task 3 的 App/overlay/formDoneMsg；store：`AddServer`/`UpdateServer`/`DeleteServer`/`GetServerByName`/`SetCredential`。
- Produces:
  - `type serverDraft struct { Name, Host, User, Password, KeyPath, KeyPass, SudoPassword, Description, Location, Hardware, Services, Role, Caveats string; Port int }`
  - `func newServerForm(d *serverDraft) *huh.Form`（新增/编辑共用；编辑时预填非敏感字段）
  - `func (a *App) addServerCmd(st *store.Store) tea.Cmd` / `editServerCmd(st *store.Store, cur *models.Server)`：打开 overlay 表单；提交后组装 models.Server + SetCredential → Add/Update → actionDoneMsg
  - `func (a *App) deleteServerCmd(st *store.Store, cur *models.Server) tea.Cmd`：huh Confirm → DeleteServer（按 id）
  - `type secretView struct{...}`（tokenview.go，overlay）：一次性明文展示页——Task 6/7 复用

- [ ] **Step 1: 失败测试（draft→server 组装纯函数 + 删除动作）**

```go
package tui

import (
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestDraftToServer_Add(t *testing.T) {
	st := newStore(t) // helper: temp store with master key (same as mcpserver tests pattern)
	d := &serverDraft{Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22, Password: "pw", Hardware: "2x3090"}
	srv, err := d.toServer(st)
	if err != nil { t.Fatal(err) }
	if _, err := st.AddServer(srv); err != nil { t.Fatal(err) }
	got, _ := st.GetServerByName("gpu")
	if got == nil || got.Host != "192.0.2.10" || got.AuthMethod != models.AuthPassword {
		t.Fatalf("roundtrip: %+v", got)
	}
}

func TestDraftToServer_PasswordKeyMutex(t *testing.T) {
	st := newStore(t)
	d := &serverDraft{Name: "x", Host: "h", User: "u", Password: "p", KeyPath: "k"}
	if _, err := d.toServer(st); err == nil {
		t.Fatal("password+key must be rejected (CLI parity)")
	}
}

func TestDeleteServer_Action(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	id, _ := st.AddServer(&models.Server{Name: "tmp", Host: "h", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err := st.DeleteServer(id); err != nil { t.Fatal(err) }
	if g, _ := st.GetServerByName("tmp"); g != nil { t.Fatal("still present") }
}
```

- [ ] **Step 2: 确认失败** — Run: `go test ./internal/tui/ -run 'TestDraft|TestDeleteServer' -v` — Expected: FAIL

- [ ] **Step 3: 实现 forms.go（关键代码）**

```go
package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/huh/v2"
	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type serverDraft struct {
	Name, Host, User   string
	Port               int
	Password, KeyPath, KeyPass, SudoPassword string
	Description, Location, Hardware, Services, Role, Caveats string
}

// newServerForm builds the add/edit form bound to d by pointer. Secret fields are
// masked and OPTIONAL in edit mode (empty = keep existing credential).
func newServerForm(d *serverDraft, editing bool) *huh.Form {
	credTitle := "密码（留空=保持不变）"
	if !editing {
		credTitle = "密码（与密钥二选一）"
	}
	f := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("名称（唯一）").Value(&d.Name).Validate(func(s string) error {
				if strings.TrimSpace(s) == "" { return errors.New("必填") }
				return nil
			}),
			huh.NewInput().Title("Host / IP").Value(&d.Host).Validate(nonEmpty),
			huh.NewInput().Title("SSH 用户").Value(&d.User).Validate(nonEmpty),
			huh.NewInput().Title("端口").Value(intStr(&d.Port, 22)),
		),
		huh.NewGroup(
			huh.NewInput().Title(credTitle).Value(&d.Password).EchoMode(huh.EchoModePassword),
			huh.NewInput().Title("私钥路径（与密码二选一；编辑时留空=不变）").Value(&d.KeyPath),
			huh.NewInput().Title("密钥口令（可选）").Value(&d.KeyPass).EchoMode(huh.EchoModePassword),
			huh.NewInput().Title("sudo 密码（可选）").Value(&d.SudoPassword).EchoMode(huh.EchoModePassword),
		),
		huh.NewGroup(
			huh.NewInput().Title("硬件").Value(&d.Hardware),
			huh.NewInput().Title("位置").Value(&d.Location),
			huh.NewInput().Title("角色").Value(&d.Role),
			huh.NewInput().Title("服务").Value(&d.Services),
			huh.NewInput().Title("Caveats（agent 行动前必读）").Value(&d.Caveats),
			huh.NewInput().Title("备注").Value(&d.Description),
		),
	)
	return f
}

func nonEmpty(s string) error {
	if strings.TrimSpace(s) == "" { return errors.New("必填") }
	return nil
}

// intStr binds an int field via a string mirror (huh Input is string-bound).
func intStr(p *int, def int) *string {
	s := fmt.Sprintf("%d", def)
	go func() {}() // no-op; see bindInt below
	return &s // NOTE: real binding below via custom Validate — implement bindInt
}
```

**实现者注意（intStr 陷阱）**：huh 的 Input 绑定 string。正确做法：用 `huh.NewInput().Value(&s)` + 在提交后 `strconv.Atoi(strings.TrimSpace(s))` 写回 `*p`；失败给字段校验错误。**删除上面的占位 intStr**，改为 `portField(p *int) *huh.Input`：内部持 `s := strconv.Itoa(*p)`，`Validate` 里 `strconv.Atoi` 校验并写回 `*p`。本步收尾必须 `go vet` 干净。

`toServer`（与 CLI `serversAddCmd` 同路）：

```go
// toServer assembles a models.Server from the draft, minting credentials via st
// when secret fields are filled. Password/key are mutually exclusive (CLI parity).
func (d *serverDraft) toServer(st *store.Store) (*models.Server, error) {
	if d.Password != "" && d.KeyPath != "" {
		return nil, errors.New("密码与私钥互斥：二选一")
	}
	srv := &models.Server{
		Name: strings.TrimSpace(d.Name), Host: strings.TrimSpace(d.Host),
		Port: d.Port, User: strings.TrimSpace(d.User),
		Description: strings.TrimSpace(d.Description), Location: strings.TrimSpace(d.Location),
		Hardware: strings.TrimSpace(d.Hardware), Services: strings.TrimSpace(d.Services),
		Role: strings.TrimSpace(d.Role), Caveats: strings.TrimSpace(d.Caveats),
	}
	switch {
	case d.Password != "":
		cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(d.Password)})
		if err != nil { return nil, err }
		srv.CredentialID, srv.AuthMethod = cid, models.AuthPassword
	case d.KeyPath != "":
		keyBytes, err := os.ReadFile(d.KeyPath)
		if err != nil { return nil, err }
		cid, err := st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: []byte(d.KeyPass)})
		if err != nil { return nil, err }
		srv.CredentialID, srv.AuthMethod = cid, models.AuthPrivateKey
	}
	if d.SudoPassword != "" {
		sid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(d.SudoPassword)})
		if err != nil { return nil, err }
		srv.SudoCredentialID = sid
	}
	return srv, nil
}
```

**edit 路径**：`editServerCmd` 预填 `d` 的非敏感字段（从 cur 拷贝）+ `d.Port=cur.Port`；提交时若 Password/KeyPath 均空 → 沿用 `cur.CredentialID/AuthMethod`；否则 toServer 重铸凭据后 `srv.ID = cur.ID`，`st.UpdateServer(srv)`（全行写、id 保留——CLI `serversEditCmd` 同语义）。

**App 接线**（app.go Update 的 KeyRunes 分支，仅 `a.mode == ModeBroker`）：

```go
case "a", "e", "d":
	if a.page == pageServers {
		sp := a.pages[pageServers].(*serversPage)
		switch k {
		case "a":
			draft := &serverDraft{}
			a.overlay = newFormOverlay("新增服务器", newServerForm(draft, false), func() tea.Cmd {
				return submitServer(a.st, nil, draft)
			})
		case "e":
			if cur := sp.current(); cur != nil {
				draft := prefill(cur)
				a.overlay = newFormOverlay("编辑服务器", newServerForm(draft, true), func() tea.Cmd {
					return submitServer(a.st, cur, draft)
				})
			}
		case "d":
			if cur := sp.current(); cur != nil {
				confirm := false
				form := huh.NewForm(huh.NewGroup(huh.NewConfirm().
					Title(fmt.Sprintf("删除服务器 %q？（profile 授权一并失效）", cur.Name)).Value(&confirm)))
				a.overlay = newFormOverlay("删除服务器", form, func() tea.Cmd {
					if !confirm { return nil }
					return doAction(a.st, func() (string, error) {
						return "已删除 " + cur.Name, a.st.DeleteServer(cur.ID)
					})
				})
			}
		}
	}
```

支撑函数（forms.go）：

```go
// formOverlay wraps a huh form as an App overlay; on completion it emits
// formDoneMsg{after: action}, so the action runs AFTER the overlay closes.
type formOverlay struct {
	title  string
	form   *huh.Form
	action func() tea.Cmd
}
func newFormOverlay(title string, f *huh.Form, action func() tea.Cmd) *formOverlay {
	return &formOverlay{title: title, form: f, action: action}
}
func (o *formOverlay) Title() string { return o.title }
func (o *formOverlay) Init() tea.Cmd { return o.form.Init() }
func (o *formOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	f, cmd := o.form.Update(msg)
	if nf, ok := f.(*huh.Form); ok { o.form = nf }
	if o.form.State == huh.StateCompleted {
		return o, formDoneMsg{after: o.action()}
	}
	if o.form.State == huh.StateAborted {
		return o, formDoneMsg{}
	}
	return o, cmd
}
func (o *formOverlay) View() string {
	return titleStyle.Render(" "+o.title+" ") + "\n（Esc 取消）\n" + o.form.View()
}

// doAction runs a store mutation off the UI loop and reports via messages.
func doAction(st *store.Store, fn func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		desc, err := fn()
		if err != nil { return errMsg{err} }
		return actionDoneMsg{desc}
	}
}

func submitServer(st *store.Store, cur *models.Server, d *serverDraft) tea.Cmd {
	return doAction(st, func() (string, error) {
		srv, err := d.toServer(st)
		if err != nil { return "", err }
		if cur == nil {
			_, err := st.AddServer(srv)
			return "已新增 " + srv.Name, err
		}
		if d.Password == "" && d.KeyPath == "" { // keep existing credential
			srv.CredentialID, srv.AuthMethod = cur.CredentialID, cur.AuthMethod
		}
		if d.SudoPassword == "" { srv.SudoCredentialID = cur.SudoCredentialID }
		srv.ID = cur.ID
		return "已更新 " + srv.Name, st.UpdateServer(srv)
	})
}
```

- [ ] **Step 4: 测试过 + 全量回归**

Run: `go test ./internal/tui/ -v && go build ./...` — Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): server add/edit/delete via huh forms with masked credentials"
```

---

### Task 5: Profiles 页签（新增 + 授权多选）

**Files:**
- Modify: `internal/tui/profiles.go`（扩 Detail：服务器数/成员名）、`internal/tui/app.go`（g 键）、`internal/tui/forms.go`（profile 表单）
- Test: `internal/tui/profiles_test.go`

**Interfaces:**
- Consumes: `st.AddProfile(name) (string, error)`、`st.GrantServers(profileID, []string) error`、`st.ListServers()`、`st.ServersForProfile(profileID)`。
- Produces: `func newGrantForm(profiles []string, servers []*models.Server, chosen *[]string) *huh.Form`（huh.NewMultiSelect[ string ].Options(服务器名)…）；`a` 键（profiles 页）= 新建 profile。

- [ ] **Step 1: 失败测试**

```go
package tui

import "testing"

func TestGrantAction(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p1")
	cid, _ := stSetCred(st)
	s1, _ := st.AddServer(srv("a", cid)); s2, _ := st.AddServer(srv("b", cid))
	if err := st.GrantServers(pid, []string{s1, s2}); err != nil { t.Fatal(err) }
	ids, _ := st.ServersForProfile(pid)
	if len(ids) != 2 { t.Fatalf("granted %d", len(ids)) }
	// idempotent re-grant (INSERT OR IGNORE)
	_ = st.GrantServers(pid, []string{s1})
	if ids, _ = st.ServersForProfile(pid); len(ids) != 2 { t.Fatalf("dup grant leaked: %d", len(ids)) }
}
```

（`srv`/`stSetCred` 为包内测试 helper，模式同 T4。）

- [ ] **Step 2: 确认失败** → **Step 3: 实现**

grant 流程（app.go，profiles 页 + `g` 键）：`st.ListServers()` → huh MultiSelect（Options 带 server id value + name label）→ `GrantServers(profile.Name 为查 id …)`——**注意**：MultiSelect value 用 **server id**（GrantServers 要 id，且同名不可靠；`huh.NewOption(name, id)` label 显示名、value 是 id）。profile 定位用 `profilesPage.current().ID`。新建 profile：单字段表单 `huh.NewInput().Value(&name)` → `AddProfile`。profiles 页 Detail 扩充：`ServersForProfile` 数量 + 成员名列表（`GetServer` 按需或用 ListServers 建 id→name 映射）。

- [ ] **Step 4: 测试 + 回归** → Run: `go test ./internal/tui/ -v && go build ./...`

- [ ] **Step 5: Commit** — `git commit -m "feat(tui): profiles page — create profile, multi-select grant"`

---

### Task 6: Projects 页签（token 发放一次性展示 + 生命周期）

**Files:**
- Create: `internal/tui/tokenview.go`（一次性明文 overlay，Task 4 已声明接口本任务实装）
- Modify: `internal/tui/projects.go`、`internal/tui/app.go`、`internal/tui/forms.go`
- Test: `internal/tui/projects_test.go`

**Interfaces:**
- Consumes: `st.AddProject(name, profileID) (id, token, error)`、`st.RotateProject(id) (token, error)`、`st.SetProjectStatus(id, models.ProjectActive/Disabled/Revoked)`、`st.ListProfiles()`。
- Produces: `type secretView struct{ title, body string }` 实现 overlay：`View()` 全屏渲染 `secretStyle` 高亮的明文 + 「⚠ 仅此一次显示，任意键返回」；任意键 → `formDoneMsg{}`。

- [ ] **Step 1: 失败测试**（AddProject 返回值 + secretView 渲染）

```go
package tui

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestProjectTokenFlow(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p")
	id, tok, err := st.AddProject("proj", pid)
	if err != nil || tok == "" || id == "" { t.Fatalf("(%q,%q,%v)", id, tok, err) }
	newTok, err := st.RotateProject(id)
	if err != nil || newTok == tok { t.Fatalf("rotate: %q %v", newTok, err) }
	if err := st.SetProjectStatus(id, models.ProjectDisabled); err != nil { t.Fatal(err) }
}

func TestSecretView_RendersOnceNotice(t *testing.T) {
	sv := &secretView{title: "项目 token", body: "TOK-xyz"}
	v := sv.View()
	if !strings.Contains(v, "TOK-xyz") || !strings.Contains(v, "仅此一次") {
		t.Fatalf("view: %s", v)
	}
}
```

- [ ] **Step 2: 确认失败** → **Step 3: 实现**

tokenview.go：

```go
package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// secretView shows a one-time secret full-screen. Any key dismisses it.
type secretView struct{ title, body string }

func (s *secretView) Title() string { return s.title }
func (s *secretView) Init() tea.Cmd { return nil }
func (s *secretView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyMsg); ok {
		return s, formDoneMsg{}
	}
	return s, nil
}
func (s *secretView) View() string {
	return titleStyle.Render(" "+s.title+" ") + "\n\n" +
		secretStyle.Render(s.body) +
		"\n\n⚠ 仅此一次显示（关闭后不可再看）。按任意键返回。\n"
}
```

projects 页动作（app.go）：`a` = 新建 project 表单（name + Select profile）→ `AddProject` → **成功后 overlay = &secretView{title:"项目 token", body: token}**（token 明文只在此时出现）；`e` = RotateProject → secretView（新 token）；`d` = Confirm → `SetProjectStatus(id, models.ProjectRevoked)`。Detail 显示：name/status/profile 名/token_prefix/created。

- [ ] **Step 4: 测试 + 回归** → Run: `go test ./internal/tui/ -v && go build ./...`

- [ ] **Step 5: Commit** — `git commit -m "feat(tui): projects page — token issuance via one-time secret view, rotate, revoke"`

---

### Task 7: 设备码页签（签发含指纹 + 吊销）

**Files:**
- Modify: `internal/tui/app.go`、`internal/tui/forms.go`；`internal/cli/tui.go` 不动
- Test: `internal/tui/cachetokens_test.go`

**Interfaces:**
- Consumes: `st.AddCacheToken(name) (id, code, error)`、`st.RevokeCacheToken(name) error`、`st.ListCacheTokens()`、**指纹来源：`mcpserver.LoadOrCreateServeCert()`（internal/mcpserver，cli 已用它打印指纹——tui 可 import mcpserver，无环）**。
- Produces: 设备码 secretView 的 body 格式 = `<设备码>` + 换行 + `指纹 sha256:…` + 换行 + `cache pull --url <serve-url> --token '<码>:<指纹>'` 使用提示（url 从 cacheMeta/cred 读不到 broker 地址——**用输入项**：签发表单多一个「serve 地址」字段，仅用于拼提示文案，不落盘）。

- [ ] **Step 1: 失败测试**

```go
package tui

import (
	"strings"
	"testing"
	"time"
)

func TestCacheTokenIssueFlow(t *testing.T) {
	st := newStore(t)
	id, code, err := st.AddCacheToken("laptop")
	if err != nil || code == "" || id == "" { t.Fatalf("(%q,%q,%v)", id, code, err) }
	if err := st.RevokeCacheToken("laptop"); err != nil { t.Fatal(err) }
}

func TestDeviceCodeSecretView_Body(t *testing.T) {
	sv := deviceCodeView("https://192.0.2.5:7878", "CODE123", "sha256:"+"a"*64)
	v := sv.View()
	for _, want := range []string{"CODE123", strings.Repeat("a", 64), "cache pull", "--allow-plaintext 之外"} {
		_ = want // asserts below
	}
	if !strings.Contains(v, "CODE123") || !strings.Contains(v, "sha256:") || !strings.Contains(v, "cache pull") {
		t.Fatalf("view missing parts:\n%s", v)
	}
}
```

（末段 `strings.Repeat("a", 64)` 与 `time` 若未用则删 import——保持 vet 干净。）

- [ ] **Step 2: 确认失败** → **Step 3: 实现**

`a` = 签发表单（name + serve 地址提示字段）→ `AddCacheToken` + `LoadOrCreateServeCert()` 拿指纹 → overlay = 设备码 secretView（`deviceCodeView(url, code, fp) *secretView`，body 按上述格式）。`d` = Confirm → `RevokeCacheToken(current().Name)`。Detail：name/status/last_pull/prefix。

- [ ] **Step 4: 测试 + 回归** → Run: `go test ./internal/tui/ -v && go build ./...`

- [ ] **Step 5: Commit** — `git commit -m "feat(tui): cache-token page — issue with fingerprint one-time view, revoke"`

---

### Task 8: client 模式面板

**Files:**
- Create: `internal/tui/clientpage.go`
- Modify: `internal/tui/app.go`（client 模式路由：无页签，单一 clientView；`s/c/t` 键位）、`internal/tui/mode.go`（`newApp` client 分支装载 clientView）
- Test: `internal/tui/clientpage_test.go`

**Interfaces:**
- Consumes: `clientops.ReadCacheCred/WriteCacheCred/LoadCacheSnapshot/DoPull`、`os.Stat(cache.bin)`。
- Produces: `type clientModel struct` 实现 tea.Model：字段 `cred *clientops.CacheCred`、`snap *store.Snapshot`、`cacheAge time.Duration`、`status string`、`err error`、`busy bool`；cmds：`refreshDataCmd()`（重读 cred+snapshot+stat）、`syncCmd(cred)`（异步 DoPull，10s 超时，返回 syncDoneMsg{err}）。

- [ ] **Step 1: 失败测试（数据组装纯函数）**

```go
package tui

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

func TestClientHeader(t *testing.T) {
	h := clientHeader(&clientops.CacheCred{URL: "https://192.0.2.5:7878", Pin: "sha256:" + strings.Repeat("a", 64)}, 3, 2*time.Minute)
	for _, want := range []string{"192.0.2.5", "sha256", "3 服务器", "2m"} {
		if !strings.Contains(h, want) {
			t.Fatalf("header missing %q:\n%s", want, h)
		}
	}
}

func TestClientServerList(t *testing.T) {
	snap := &store.Snapshot{Servers: []store.SnapshotServer{{Name: "gpu", Host: "192.0.2.10", User: "u"}}}
	rows := clientServerRows(snap)
	if len(rows) != 1 || !strings.Contains(rows[0], "gpu") || !strings.Contains(rows[0], "192.0.2.10") {
		t.Fatalf("rows: %v", rows)
	}
}
```

（`2m` 由 `age.String()` 截断而来——`clientHeader` 内 `age.Round(time.Minute).String()` 若含 `2m0s`，断言改 `2m0s`；以实现为准调断言一次。）

- [ ] **Step 2: 确认失败** → **Step 3: 实现**

clientpage.go 骨架：

```go
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

type clientModel struct {
	cred     *clientops.CacheCred
	snap     *store.Snapshot
	cacheAge time.Duration
	status   string
	err      error
	busy     bool
	overlay  overlay // connection-edit form
}

func newClientModel() clientModel { return clientModel{} }

type syncDoneMsg struct{ err error }

func (m clientModel) Init() tea.Cmd { return refreshDataCmd }

func refreshDataCmd() tea.Msg {
	cred, err := clientops.ReadCacheCred()
	if err != nil || cred == nil {
		return errMsg{fmt.Errorf("读取连接配置失败: %w", err)}
	}
	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		return errMsg{err}
	}
	_, bin, _, _, err := clientops.CachePaths()
	if err != nil { return errMsg{err} }
	var age time.Duration
	if fi, err := os.Stat(bin); err == nil { age = time.Since(fi.ModTime()) }
	return dataReadyMsg{cred: cred, snap: snap, age: age}
}

type dataReadyMsg struct {
	cred *clientops.CacheCred
	snap *store.Snapshot
	age  time.Duration
}

func syncCmd(cred *clientops.CacheCred) tea.Cmd {
	return func() tea.Msg {
		pin := cred.Pin
		code := cred.Token
		if pin == "" {
			return syncDoneMsg{fmt.Errorf("连接配置缺 pin（自动拉取永不走明文）——请 [c] 编辑连接补上")}
		}
		err := clientops.DoPull(cred.URL, code, pin, clientops.PullOpts{Timeout: clientops.LazyPullTimeout})
		return syncDoneMsg{err}
	}
}

func (m clientModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case dataReadyMsg:
		m.cred, m.snap, m.cacheAge = t.cred, t.snap, t.age
		return m, nil
	case syncDoneMsg:
		m.busy = false
		if t.err != nil { m.err = t.err; m.status = "" } else { m.err = nil; m.status = "同步完成" }
		return m, refreshDataCmd
	case errMsg:
		m.err = t.err
		return m, nil
	case formDoneMsg:
		m.overlay = nil
		return m, refreshDataCmd
	case tea.KeyMsg:
		if m.overlay != nil {
			ov, cmd := m.overlay.Update(msg)
			m.overlay, _ = ov.(overlay)
			return m, cmd
		}
		if k := string(t.Runes); k == "q" || t.Type == tea.KeyCtrlC {
			return m, tea.Quit
		} else if k == "s" && !m.busy {
			m.busy = true
			return m, syncCmd(m.cred)
		} else if k == "c" {
			m.overlay = m.editConnForm()
			return m, m.overlay.Init()
		} else if k == "t" {
			m.status = "TTL 由 .mcp.json 的 --cache-max-age 控制（默认 30m；0=关闭自动拉取）"
		}
	case tea.WindowSizeMsg:
		return m, nil
	}
	return m, nil
}
```

`editConnForm`：huh 表单（URL / 设备码 `EchoModePassword` / 指纹 pin）→ `clientops.WriteCacheCred`（失败 errMsg）。View()：`clientHeader(cred, len(snap.Servers), age)` + 左侧 `clientServerRows(snap)` 右侧选中详情（复用 orDash 模式）+ footer `[s]同步 [c]编辑连接 [t]TTL  q退出` + status/err 横幅 + busy 时「同步中…」。app.go 的 `newApp(ModeClient)` 返回包装 clientModel 的 App 变体——**实现选择**：`Run(ModeClient)` 直接 `tea.NewProgram(newClientModel())`（client 模式不走 App 结构，独立顶层模型；`Run` 里按 mode 分派）。

- [ ] **Step 4: 测试 + 回归** → Run: `go test ./internal/tui/ -v && go build ./...`

- [ ] **Step 5: Commit** — `git commit -m "feat(tui): client panel — connection config, cache status, read-only list, manual sync"`

---

### Task 9: 终端检测收尾 + 文档 + 交叉编译验证

**Files:**
- Modify: `README.md`（tui 章节）、`docs/getting-started.md`（提一笔）或 `docs/multi-machine.md`（client enroll 提及 `tui --client`）
- Test: 手工验证清单

- [ ] **Step 1: README 增加「TUI 主控台」章节**（中文，要点：单命令双模式、键位表、mintty 需 winpty/Windows Terminal、凭据永不回显、token 一次性展示、client 端零远程写）

- [ ] **Step 2: goreleaser 交叉编译验证**（Charm 依赖不得破坏 6 目标）

```bash
goreleaser build --snapshot --clean
```

Expected: 6 目标全过（windows/linux/darwin × amd64/arm64），无 CGO 报错。

- [ ] **Step 3: 手工冒烟**（本机，Windows Terminal）：`go run ./cmd/ssh-manager tui`——vault 在本机开发机没有 → 应引导报错；`--mode client` → client 面板（读本机真实 cache）。

- [ ] **Step 4: Commit** — `git commit -m "docs: tui console chapter; verify goreleaser cross-compile with charm deps"`

---

### Task 10: 全量验证 + 真机验收

- [ ] **Step 1: 机器验证** — Run: `go build ./... && go vet ./... && go test ./... -count=1` — Expected: 全绿。

- [ ] **Step 2: 真机验收（controller 执行，SSH 到 NUC10 + 本机）**
  1. NUC10：部署新 exe → `ssh-manager tui`（broker 模式自动判定）→ 键盘完成：新增服务器（掩码凭据）→ profiles 页授权 → projects 页发 token（一次性展示）→ 设备码页签发（含指纹）→ 删除新增的测试服务器。
  2. 笔记本：`tui --mode client` → `[c]` 改连接 → `[s]` 立即同步 → 清单刷新。
  3. 回归：`mcp --cache` 路径不受影响（跑一次现有会话工具调用）。

- [ ] **Step 3: 收尾** — superpowers:finishing-a-development-branch（合并 push；push 前 secret scan 覆盖本分支全部新增输出文案）。

---

## Self-Review 记录

- **Spec 覆盖**：spec §2 模式判定→T2；§3.1 主控台四页签+token 一次性页→T3/T4/T5/T6/T7；§3.2 client 面板→T8；§4 组件/数据流（clientops 迁移在 T1，tui 不 import cli）→T1；§5 敏感面（掩码/不回显/一次性）→T4/T6/T7/T8 内嵌；§6 错误处理（锁定引导/mintty/错误横幅）→T2/T3/T8；§7 测试→各任务+T10；§8 依赖与交叉编译→T2/T9；§9 边界（零远程写/零 serve 改动）→Global Constraints。**缺口：spec §3.1 说「授权 [g]」在 profiles 页执行——T5 实现时注意 g 键在 profiles 页 = 对当前 profile 授权（多选服务器），servers 页的 g 提示移除或同样路由到 profiles 页（实现时统一为 profiles 页专属，footer 文案相应调整）。**
- **占位符**：无 TBD；T4 的 intStr 陷阱带明确修正指令；T8 的 2m 断言带适配指令。
- **类型一致性**：`listPage`/`overlay`/`formDoneMsg`/`errMsg`/`actionDoneMsg` 在 T3 定义、T4-T8 消费一致；`clientops` 导出名 T1 定义、T2/T8 消费一致；`secretView` T4 声明、T6 实装、T7 复用（`deviceCodeView` 返回 `*secretView`）。
