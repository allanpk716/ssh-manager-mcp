# Plan 19：角色向导 + clear + 概念图解 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ssh-manager tui` 首次启动向导（单机/server/client 三路径，可重入）+ 角色唯一化（role.json）+ `ssh-manager clear` 全清命令 + 概念图解文档。

**Architecture:** 新包 `internal/roles`（role.json 读写与判定链，tui 与 cli clear 共用）；向导是 tui 内的独立顶层 tea.Model（页面栈复用 huh overlay 机制）；serve 安装从 cobra 抽出可编程核心；clear 的安全绳 = ExportSnapshot + vaultio.Encrypt。spec：`docs/superpowers/specs/2026-08-15-role-wizard-clear-design.md`（v2）。

**Tech Stack:** 既有栈（bubbletea/huh/lipgloss v2、clientops、store）。零新依赖。

## Global Constraints

- **零新第三方依赖**；import 一律 charm.land v2 路径。
- **凭据/token/设备码仅一次性展示**；每个密钥屏必须带用途标签 + 丢失重签指引。
- **clear 时序铁律**：DELETE → export 生成并回读校验 → 口令抄录确认 → 才执行任何删除；任何失败 = 中止零改动；全程幂等可重跑。
- **向导铁律**：选定角色瞬间写 role.json（setup_complete:false）；Esc=安全暂停；setup_complete:false 时重开 tui 必回向导。
- serve 安装默认绑 `0.0.0.0:7878`（严禁 127.0.0.1 默认）；装后自动探活。
- standalone→server 是非破坏升级；只有 vault 角色→client 需要 clear。
- 不改 mcp/cache/serve 运行时；不加网络端点；代码注释英文、文档中文。
- **执行前**：isolated linked worktree（superpowers:using-git-worktrees）。

---

### Task 1: `internal/roles` 包 — role.json + 判定链

**Files:**
- Create: `internal/roles/roles.go`、`internal/roles/roles_test.go`
- Modify: 无（后续任务接线）

**Interfaces:**
- Produces（全项目唯一判定入口）：
  - `type Role string`；`const RoleStandalone Role = "standalone"; RoleServer Role = "server"; RoleClient Role = "client"`
  - `type State struct { Role Role; SetupComplete bool }`
  - `func RolePath(r Role) (string, error)` — standalone/server→`paths.VaultDir()/role.json`；client→`os.UserConfigDir()/ssh-manager/role.json`（与 cache 数据同居）
  - `func Load() (*State, error)` — 先查两处（vault 目录→用户目录）；都没有 → `(nil, nil)`；内容非法 → 错误（引导 clear）
  - `func Save(s State) error` — 原子写（unique temp+rename，0600）
  - `func Delete() error` — 两处都删（幂等）
  - `func ResolveMode(force string) (Launch, error)` — **唯一启动判定**：
    ```go
    type Launch struct {
      Kind        LaunchKind // LaunchWizard / LaunchBroker / LaunchClient
      Role        Role       // Kind=Broker 时区分 standalone|server（有无 serve 服务安装）
      ResumeSetup bool       // Kind 对应向导续配（SetupComplete=false）
    }
    ```
    判定顺序（spec §1.2 逐字）：role.json 存在 → 异常态检查（非法值→引导 clear；vault 角色缺 vault→引导 clear 重跑；client 缺缓存→LaunchClient 正常）→ ResumeSetup 判定；无 role.json → 探测：**locked vault fail-closed（复用 tui.mode 的 vaultExists/vaultUnlocked 逻辑——本任务把这两个函数移入 roles 包并导出 `VaultExists/VaultUnlocked`，tui.mode 改调 roles）** → vault→LaunchBroker(角色由 serve 服务是否注册决定：调 `service.Status`——简化：探测 serve 服务存在性用 `svc.Status` 太重，用文件探测 `paths.ServeCertPath` 存在→server 否则 standalone，注释说明这是启发式) → cache→LaunchClient → 全空→LaunchWizard。
    `force=="client"` 且本机有 vault → 错误文案含「本机已有 vault…ssh-manager clear 将删除本机全部 vault 数据」。
  - `func VaultExists() bool` / `func VaultUnlocked() bool`（从 tui/mode.go 迁移；vaultStorePath 一并迁）

- [ ] **Step 1: 写失败测试（异常态矩阵 + 三态 + force 护栏）**

```go
package roles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// withDirs isolates both role-file locations via env (SSHMGR_STORE pins the vault
// dir; XDG_CONFIG_HOME/APPDATA pins the user dir).
func withDirs(t *testing.T) (vaultDir, userDir string) {
	t.Helper()
	vaultDir = t.TempDir()
	userDir = t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	t.Setenv("APPDATA", userDir) // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", userDir)
	return vaultDir, userDir
}

func seedVault(t *testing.T, vaultDir string) {
	t.Helper()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}

func TestLoad_Empty(t *testing.T) {
	withDirs(t)
	s, err := Load()
	if err != nil || s != nil {
		t.Fatalf("empty: (%v,%v)", s, err)
	}
}

func TestSaveLoad_ClientRoundTrip(t *testing.T) {
	withDirs(t)
	if err := Save(State{Role: RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil || s == nil || s.Role != RoleClient || !s.SetupComplete {
		t.Fatalf("roundtrip: %+v %v", s, err)
	}
	// client role.json must live in the USER dir, not the vault dir
	if _, err := os.Stat(filepath.Join(t.TempDir())); err != nil { _ = err }
	p, _ := RolePath(RoleClient)
	if !strings.Contains(p, "ssh-manager") {
		t.Fatalf("client role path not under user dir: %s", p)
	}
}

func TestResolve_FullMatrix(t *testing.T) {
	// wizard on empty
	withDirs(t)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchWizard {
		t.Fatalf("empty: %+v %v", l, err)
	}
	// locked vault never degrades to client (vault exists, no master key)
	vd, ud := withDirs(t)
	_ = ud
	os.WriteFile(filepath.Join(vd, "store.db"), []byte("x"), 0o600)
	if _, err := ResolveMode(""); err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("locked vault must fail-closed: %v", err)
	}
	// unlocked vault → broker (standalone heuristic: no serve cert)
	seedVault(t, vd)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchBroker || l.Role != RoleStandalone {
		t.Fatalf("vault: %+v %v", l, err)
	}
	// vault + serve cert → server heuristic
	os.WriteFile(filepath.Join(vd, "serve-cert.pem"), []byte("x"), 0o600)
	if l, _ := ResolveMode(""); l.Role != RoleServer {
		t.Fatalf("serve cert should hint server: %+v", l)
	}
	// cache cred only → client
	vd2, _ := withDirs(t)
	_ = vd2
	os.MkdirAll(filepath.Join(os.Getenv("APPDATA"), "ssh-manager"), 0o700)
	os.WriteFile(filepath.Join(os.Getenv("APPDATA"), "ssh-manager", "cache.auth.json"),
		[]byte(`{"url":"https://x","token":"t"}`), 0o600)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchClient {
		t.Fatalf("cache: %+v %v", l, err)
	}
	// force client on vault machine → guided error mentioning clear + 删除
	seedVault(t, vd)
	if _, err := ResolveMode("client"); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("force client on vault: %v", err)
	}
}

func TestResolve_RoleFileAnomalies(t *testing.T) {
	// invalid value
	vd, _ := withDirs(t)
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"clientx"}`), 0o600)
	if _, err := ResolveMode(""); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("invalid role: %v", err)
	}
	// role=server but vault missing
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"server"}`), 0o600)
	if _, err := ResolveMode(""); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("server role without vault: %v", err)
	}
	// setup_complete false → resume flag
	seedVault(t, vd)
	os.WriteFile(filepath.Join(vd, "role.json"), []byte(`{"role":"standalone","setup_complete":false}`), 0o600)
	if l, err := ResolveMode(""); err != nil || l.Kind != LaunchBroker || !l.ResumeSetup {
		t.Fatalf("resume: %+v %v", l, err)
	}
}
```

- [ ] **Step 2: 确认失败** — Run: `go test ./internal/roles/ -v` — Expected: FAIL（包符号未定义）
- [ ] **Step 3: 实现 roles.go**（上述接口逐条；Save 用 clientops 风格 unique-temp+rename——注意 roles **不得 import internal/tui**（环），client 探测用 `clientops.ReadCacheCred()`；locked-vault 判定从 tui/mode.go 迁入）
- [ ] **Step 4: tui/mode.go 改调 roles**（vaultExists/vaultUnlocked/DetectMode 内部改为 roles.VaultExists 等转发或直接删除替换；**tui 既有测试保持绿**——mode_test.go 的探测测试改为调 roles 或保留为转发层测试）
- [ ] **Step 5: 回归** — Run: `go build ./... && go vet ./... && go test ./internal/roles/ ./internal/tui/ -count=1` — PASS
- [ ] **Step 6: Commit** — `git add internal/roles internal/tui && git commit -m "feat(roles): role.json state + unified launch resolution (locked-vault fail-closed, anomalies, force guard)"`

---

### Task 2: 向导骨架（状态机 + 首屏 + 可重入接线）

**Files:**
- Create: `internal/tui/wizard.go`、`internal/tui/wizard_test.go`
- Modify: `internal/tui/mode.go`（Run 分派）、`internal/cli/tui.go`（不经 DetectMode，改调 roles.ResolveMode）

**Interfaces:**
- Consumes: Task 1 的 `roles.*`；既有 `overlay`/`formOverlay`/`errMsg` 等。
- Produces:
  - `type wizardModel struct { launch roles.Launch; step wizStep; role roles.Role; data wizardData }`
  - `type wizStep int`：`stepPick/stepRoleDone`（首屏后按角色分流到 Task 3-5 的各步模型，本任务只做 stepPick + 写 role.json + 空角色页占位）
  - `func newWizard(l roles.Launch) wizardModel`
  - 首屏按 spec §2.1 后果导向两级问题（huh Select 树：第一问 凭据保管 [是/否]，第二问按答案分流 单机/server 或直接 client）
  - 选定角色 → `roles.Save(State{Role: r, SetupComplete: false})` → 进入该角色流程占位页（「角色流程在后续任务实现」文案 + q 退出）

- [ ] **Step 1: 失败测试**

```go
package tui

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/roles"
)

func TestWizard_FirstScreenSavesRole(t *testing.T) {
	vd, _ := withRoleDirs(t) // helper 同 Task1 测试的 env 隔离
	w := newWizardForTest()  // 构造 wizardModel，注入「选了 server」
	w.chooseRole(roles.RoleServer)
	b, _ := os.ReadFile(filepath.Join(vd, "role.json"))
	if want := `"role":"server"`; !strings.Contains(string(b), want) {
		t.Fatalf("role.json not written on choose: %s", b)
	}
	// resume: setup_complete false
	l, err := roles.ResolveMode("")
	if err != nil || !l.ResumeSetup {
		t.Fatalf("must resume: %+v %v", l, err)
	}
}
```

（`strings` import 补上；helper `withRoleDirs` 建在 wizard_test.go。）

- [ ] **Step 2: 确认失败** → **Step 3: 实现 wizard.go**（首屏 huh 树按 spec §2.1 文案；chooseRole 即写 role.json；q/Esc 退出=暂停）
- [ ] **Step 4: mode.go Run 分派改造**：`Run` 先 `roles.ResolveMode(mode)` → LaunchWizard→tea(wizardModel)；LaunchBroker→现有 broker 路径（ResumeSetup=true 时**先进向导续配**——向导完成/跳过后进主控台）；LaunchClient 同理。`cli/tui.go` 的 --mode 透传给 ResolveMode。
- [ ] **Step 5: 回归 + Commit** — `git commit -m "feat(tui): wizard skeleton — consequence-driven first screen writes role.json; resumable dispatch"`

---

### Task 3: 单机向导流程

**Files:**
- Modify: `internal/tui/wizard.go`、`internal/tui/wizard_test.go`
- Create: `internal/tui/wizardsteps.go`（三个角色共用的步骤函数；server/client 向导也复用）

**Interfaces:**
- Consumes: 既有 `newServerForm`/`serverDraft`（Plan 18 T4）、store API、`tokenIssuedMsg`/`secretView`。
- Produces（步骤函数，均为「返回 overlay 或执行动作」的纯构造，可单测）：
  - `func wizEnsureVault() error` — vault 已存在（roles.VaultExists+Unlocked）→ nil 跳过；不存在 → 生成 master key 并 `store.Open` 建库后 Close（即 unlock 初始化；失败原样返回）
  - `func wizServerLoopForm(d *serverDraft) *huh.Form` — 包一层「继续添加？[y/n]」确认的复用入口（循环由 wizard 状态机控制：提交后 doAction(AddServer) → Confirm 继续 → 再开表单）
  - `func wizProfileGrantForm(profileName *string, servers []*models.Server, chosen *[]string) *huh.Form` — profile 名（默认 hostname，`Validate` 冲突时自动 `-2` 后缀再放行）+ 多选（value=id）
  - `func wizTokenScreen(title, usage, recovery string) overlay` — 密钥展示 overlay 工厂：body = secretStyle(token) + 用途行 + 「⚠ 仅此一次。丢失 → 」+ recovery 行
  - `func mcpConfigScreen(tokenRef string) overlay` — .mcp.json 收尾屏（静态文案 + 完整 JSON 片段，tokenRef 写「上方已展示的 project token」）
  - `func wizFinish() tea.Cmd` — `roles.Save(State{Role: r, SetupComplete: true})` 后返回进入主控台的 msg

- [ ] **Step 1: 失败测试**（ wizEnsureVault 建库/幂等；profile 名冲突后缀；wizTokenScreen 文案含用途+重签指引三要素）

```go
func TestWizEnsureVault_Idempotent(t *testing.T) {
	vd, _ := withRoleDirs(t)
	if err := wizEnsureVault(); err != nil { t.Fatal(err) }
	db1 := statModTime(t, filepath.Join(vd, "store.db"))
	if err := wizEnsureVault(); err != nil { t.Fatal(err) }
	if statModTime(t, filepath.Join(vd, "store.db")) != db1 {
		t.Fatal("second run must not recreate")
	}
}

func TestWizProfileName_SuffixOnConflict(t *testing.T) {
	vd, _ := withRoleDirs(t)
	wizEnsureVault(t)
	st := openVault(t) // helper: Open with the wizard-created key
	pid, _ := st.AddProfile("nuc10")
	_ = pid
	name := dedupeProfileName(st, "nuc10")
	if name != "nuc10-2" { t.Fatalf("want nuc10-2 got %s", name) }
}

func TestWizTokenScreen_Copy(t *testing.T) {
	ov := wizTokenScreen("project token", "贴到 .mcp.json 的 --token", "主控台 Projects 页 [a] 重发")
	v := viewString(ov) // helper: 渲染 overlay 取文本
	for _, want := range []string{"project token", "贴到", "仅此一次", "重发"} {
		if !strings.Contains(v, want) { t.Fatalf("missing %q in:\n%s", want, v) }
	}
}
```

- [ ] **Step 2-5: 实现 → 回归 → Commit** — `feat(tui): standalone wizard flow — vault/server-loop/profile-grant/token screen/mcp finish`

（向导状态机串联：wizEnsureVault → 服务器循环 → profile+grant → project(名=hostname)+AddProject → wizTokenScreen(token) → mcpConfigScreen → wizFinish。跳过语义：服务器循环一步不填=允许；UI 在 grant 步显示「（未选=agent 暂时看不到任何服务器）」。）

---

### Task 4: server 向导（双密钥 + serve install v2 + 接入卡）

**Files:**
- Modify: `internal/cli/serve_service.go`（抽核）、`internal/mcpserver/cert.go`（导出 LAN IP）、`internal/tui/wizard.go`、`internal/tui/wizardsteps.go`
- Create: `internal/tui/wizardserve.go`、`internal/tui/wizardserve_test.go`

**Interfaces:**
- Consumes: `runServeInstall(cmd, addr, tlsCert, tlsKey)`（改造为可编程）。
- Produces:
  - cli 侧抽核：`func installServeService(addr, tlsCert, tlsKey string, out io.Writer) error`（runServeInstall 变薄壳）；同法抽 `uninstallServeService(out io.Writer) error`（Task 7 clear 复用）
  - mcpserver 侧：`func LocalNonLoopbackIPs() []net.IP`（把私有 localNonLoopbackIPs 导出，cert.go 内部改调它）
  - tui 侧（wizardserve.go）：
    - `func wizAddrForm(ips []net.IP, chosen *string) *huh.Form` — 列出 IPv4（value=`https://<ip>:7878` 显示串），默认第一项；无 IP 时退化为手输框
    - `func installServeStep(addr string) tea.Cmd` — 调 installServeService("0.0.0.0:7878",…)（绑定 0.0.0.0；addr 变量只用于接入卡显示）→ 返回 `serveInstalledMsg{err}`
    - `func probeServe(addr string) tea.Cmd` — 本机 TLS GET `<addr>/snapshot` 期待任何 HTTP 响应（InsecureSkipVerify + pin 无所谓——只验证「在听+说 TLS」）；返回 `serveProbeMsg{ok bool, detail string}`（超时 3s）
    - `func accessCard(addr, fp string) overlay` — 客户端接入卡（spec §2.4⑦ 全文案：去向表 + 命令式备选 + 密钥不重显说明）
  - server 向导串联：Task 3 全步骤（profile 默认名=客户端名）→ 双密钥：project token wizTokenScreen（用途「贴到 client 机 .mcp.json」）→ 设备码签发（AddCacheToken+LoadOrCreateServeCert）wizTokenScreen（用途「填到 client 机向导/或拼 cache pull --token '<码>:<指纹>'」；重签指引「设备码页 [a]」）→ serve 段（admin 前置提示 → installServeStep → serveInstalledMsg.err 时显示**原文命令**含提权说明，不阻断 → probeServe → 结果横幅）→ accessCard → wizFinish(RoleServer)

- [ ] **Step 1: 失败测试**（wizAddrForm 选项值形态；accessCard 文案要素：实值地址/指纹/两个去向/命令式；probeServe 对 httptest TLS 服务器返回 ok=true；installServeService 抽核后 `serve install` CLI 冒烟不回归——跑既有 serve 相关测试）

```go
func TestAccessCard_Copy(t *testing.T) {
	v := viewString(accessCard("https://192.168.100.235:7878", "sha256:"+strings.Repeat("a", 64)))
	for _, want := range []string{"https://192.168.100.235:7878", "sha256:", ".mcp.json", "cache pull", "去向"} {
		if !strings.Contains(v, want) { t.Fatalf("missing %q", want) }
	}
}

func TestProbeServe(t *testing.T) {
	// httptest TLS server standing in for serve
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", http.StatusUnauthorized)
	}))
	defer srv.Close()
	msg := probeServe(srv.URL)()
	p := msg.(serveProbeMsg)
	if !p.ok { t.Fatalf("probe should pass on live TLS: %+v", p) }
	if msg2 := probeServe("https://127.0.0.1:1/x")(); msg2.(serveProbeMsg).ok {
		t.Fatal("dead port must fail")
	}
}
```

- [ ] **Step 2-5: 实现 → 回归（含 cli serve install 既有测试）→ Commit** — `feat(tui): server wizard — dual secret screens with usage labels, serve install with LAN addr + probe, access card`

---

### Task 5: client 向导（失败路径 + 收尾）

**Files:**
- Modify: `internal/tui/clientpage.go`、`internal/tui/wizard.go`、`internal/tui/clientpage_test.go`

**Interfaces:**
- Consumes: 既有 `editConnForm`/`syncCmd`/`refreshDataCmd`（clientModel 内部机制）。
- Produces:
  - `func classifyPullError(err error) string` — 按错误串分类四态（地址不通：`dial`/`no such host`；设备码无效：`401`/`authorization`；指纹失配：`mismatch`/`fingerprint`；超时：`Timeout`/`Client.Timeout`）返回中文文案；默认「同步失败：<原文>」
  - client 向导 = clientModel 的向导形态：表单顶部来源提示行（「设备码与服务器指纹在 server 机 TUI『设备码』页签发」）；首拉失败 → **重新打开表单并保留已输入值**（editConnForm 增加初始值参数或保存 draft 于 model；错误横幅=classifyPullError）；成功 → `clientFinishScreen`（.mcp.json 收尾屏，复用 mcpConfigScreen）→ wizFinish(RoleClient)
  - `syncCmd` 成功分支在向导态额外返回 `pullSucceededMsg{}`（向导态据此进收尾屏；面板态维持现状）

- [ ] **Step 1: 失败测试**（classifyPullError 四分类 + 默认；editConnForm 保留输入：提交失败后 model 里 draft 仍在）

```go
func TestClassifyPullError(t *testing.T) {
	cases := map[string]string{
		`Get "https://x": dial tcp: no route`: "地址不通",
		`pull: server returned 401`:           "设备码无效",
		`server fingerprint mismatch (expected a, got b)`: "指纹失配",
		`Get "https://x": context deadline exceeded (Client.Timeout exceeded)`: "超时",
	}
	for raw, want := range cases {
		if got := classifyPullError(errors.New(raw)); !strings.Contains(got, want) {
			t.Fatalf("classify(%q) = %q, want contains %q", raw, got, want)
		}
	}
}
```

- [ ] **Step 2-5: 实现 → 回归 → Commit** — `feat(tui): client wizard — source hint, classified failure path preserving input, mcp finish screen`

---

### Task 6: standalone→server 非破坏升级 `[u]`

**Files:**
- Modify: `internal/tui/app.go`（standalone 角色时 footer + u 键）
- Test: `internal/tui/app_test.go` 扩展

**Interfaces:**
- Consumes: Task 4 的 installServeStep/probeServe/accessCard、roles.Save。
- Produces: App 增加 `role roles.Role` 字段（NewBrokerApp 时从 roles.Load 或 ResolveMode 填入）；standalone 时 footer 追加 `[u]升级为 server`；`u` 键 → serve 段流程（admin 提示→install→probe→设备码签发屏→接入卡）→ 完成后 `roles.Save(State{Role: RoleServer, SetupComplete: true})` + 刷新 footer。**vault 数据零改动**。

- [ ] **Step 1: 失败测试**：构造 standalone App（seed vault 无 serve-cert）→ 按 `u` → 断言 overlay 打开（serve 段第一步）；完成后断言 store.db 内容不变（对比 servers 列表）+ role.json 变为 server。
- [ ] **Step 2-5: 实现 → 回归 → Commit** — `feat(tui): non-destructive standalone→server upgrade [u]`

---

### Task 7: `ssh-manager clear`

**Files:**
- Create: `internal/cli/clear.go`、`internal/cli/clear_test.go`

**Interfaces:**
- Consumes: `roles.Load/ResolveMode/Delete`、`uninstallServeService`（Task 4 抽核）、`st.ExportSnapshot()`、`vaultio.Encrypt/Decrypt`、`clientops.CachePaths`。
- Produces:
  - `func enumClearTargets(role roles.Role) []string` — 按实存枚举（vault 目录：store.db/-wal/-shm/master.key.plain/serve-cert.pem/serve-key.pem/init marker/serve.log/role.json + **同机 client 残留五件套**；client 目录：cache.bin/cache.auth.json/cache-dek.key/cache.meta.json/cache-audit.log/role.json + serve 服务安装态探测）；每项前缀分类（vault:/serve:/client:/task:/role:）
  - `func makeSafetyNet() (path, passphrase string, err error)` — ExportSnapshot → json → 随机口令（store.GenerateToken）→ vaultio.Encrypt → 写 `~/ssh-manager-backup-<UTC时间戳>.sme` → **回读校验**（Decrypt+json.Unmarshal store.Snapshot 通过）
  - `func deleteLegacyTimer() error` — Windows：`schtasks /Delete /TN ssh-manager-cache-refresh /F`（不存在=成功；仅 build tag windows；unix 版 no-op）
  - `newClearCmd()` cobra 命令，交互时序（spec §3 逐字）：
    1. `roles.ResolveMode("")` 定角色 → enumClearTargets 打印清单
    2. 读一行输入，非 `DELETE` → 「已取消，未做任何改动」退出 0
    3. vault 角色：先 `vault.OpenStore`（失败/锁定 → 「请先 ssh-manager unlock（clear 不提供无备份删除）」中止）→ makeSafetyNet → 打印口令（「⚠ 此口令仅显示一次」）→「按 y 确认已抄录口令」读一行，非 y 中止
    4. 执行（幂等）：serve 服务已装 → uninstallServeService（失败→打印提权重跑指引并中止）→ 逐个删除清单文件（ENOENT 忽略）→ deleteLegacyTimer → roles.Delete
    5. 输出「已清理。下次 ssh-manager tui 将重新进入首次向导。」
  - 非交互（stdin 非 TTY）→ 拒绝执行（「clear 需要交互式终端」——防止脚本误删）

- [ ] **Step 1: 失败测试**（enumClearTargets 全清单含 wal/shm/meta/残留；makeSafetyNet 回读校验 + 口令非空；DELETE 前零改动：跑 cobra 命令输入空行断言所有文件仍在；幂等：预删一个目标文件后 clear 仍全绿；timer 删除 windows 分支 mock——测试以 `SSHMGR_CLEAR_TIMER_CMD` 注入或函数变量 seam）：

```go
func TestEnumClearTargets_ServerMachine(t *testing.T) {
	vd, ud := withRoleDirs(t)
	seedVault(t, vd)
	for _, f := range []string{"store.db-wal", "master.key.plain", "serve-cert.pem", ".serve-cert-initialized"} {
		os.WriteFile(filepath.Join(vd, f), []byte("x"), 0o600)
	}
	// 同机 client 残留
	os.MkdirAll(filepath.Join(ud, "ssh-manager"), 0o700)
	os.WriteFile(filepath.Join(ud, "ssh-manager", "cache.meta.json"), []byte("{}"), 0o600)
	got := enumClearTargets(roles.RoleServer)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"store.db-wal", "master.key.plain", "serve-cert.pem", "cache.meta.json", "role.json"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestClear_CancelZeroMutation(t *testing.T) {
	// drive newClearCmd with stdin "nope\n" → 所有 seed 文件仍在、role.json 仍在
}

func TestClear_IdempotentRerun(t *testing.T) {
	// 预删 serve-cert.pem 后全流程（注入 timer seam + skip safety-net via non-vault role=client 更简单：client 角色无 export）→ 断言成功且其余文件已删
}
```

（client 角色的 clear 用真实临时目录全流程测：seed client 五件套 → 输入 DELETE → 断言五件套+role.json 全删、exe 无关；timer 用函数变量 `deleteLegacyTimerFn` seam 注入。）

- [ ] **Step 2-5: 实现 → 回归（cli 全量）→ Commit** — `feat(cli): clear — typed-DELETE + verified safety net + idempotent teardown per role`
- root.go 注册 `newClearCmd()`。

---

### Task 8: 概念图解文档 + 引用

**Files:**
- Create: `docs/concepts.md`
- Modify: `README.md`（链接）、`docs/multi-machine.md`（定时器段标 legacy + 链接 concepts）、`internal/tui/wizard.go` 首屏底部一行「概念图解：docs/concepts.md（或 --help）」

- [ ] **Step 1: 写 docs/concepts.md**（spec §5 全部要素：数据流 ASCII 图 / 类比表含 服务器指纹(pin)=防伪封条、设备码=水管钥匙 / 两种输入形态 / 第二台客户端完整操作链 / 设备码不复用建议）
- [ ] **Step 2: 交叉链接三处 + legacy 标注**
- [ ] **Step 3: Commit** — `docs: concepts.md 概念图解 + 向导/README/multi-machine 交叉链接; timer 模板标 legacy`

---

### Task 9: 全量验证 + 真机验收

- [ ] **Step 1: 机器验证** — `go build ./... && go vet ./... && go test ./... -count=1`；`gofmt -l .` 空。
- [ ] **Step 2: 真机验收（controller 执行）**：
  1. 笔记本隔离环境（SSHMGR_STORE/APPDATA 指临时目录）跑 `tui` → 向导全空触发 → 选 server 走完全流程（真 serve install 可跳过——探活失败不阻断）→ Esc 中途退出重开 → **回到向导续配**（验收死态修复）。
  2. `clear` 真跑一次（隔离目录，client 角色）→ 五件套+role.json 全消、回到向导态。
  3. NUC10（真实 server）：`tui` 探测进主控台不进向导（role.json 无→vault 探测——存量兼容验收）；手工写 role.json `{"role":"server","setup_complete":true}` 后 `tui` 直接进主控台。
  4. 笔记本 `--mode client` 正常；NUC10 `--mode client` → 引导 clear 报错（文案含删除警告）。
- [ ] **Step 3: 收尾** — finishing-a-development-branch + push 前 secret scan。

---

## Self-Review 记录

- **Spec 覆盖**：§1.1/1.2/1.3→T1(+T6 转换规则)；§2.1→T2；§2.2 通用→T2/T3；§2.3→T3；§2.4→T4；§2.5→T5；§3→T7；§4→T7(deleteLegacyTimer)+T8(文档)+（本机已删）；§5→T8；§6 测试点→各任务（role 矩阵 T1、重入 T2/T9、失败注入 T5、探活 T4、clear 注入 T7、升级断言 T6）；§7 边界→Global Constraints。**缺口检查：spec §2.3 跳过语义「空 profile 允许+UI 明示」→ T3 grant 步文案已含；§1.3 client→vault 角色向导开头清理询问——T5 范围外新增交互，简化处理：向导 stepPick 前检测旧 client 数据时在首屏上方显示一行「检测到本机曾有 client 配置，可运行 clear 清理」提示（不强制），落 T2。**
- **占位符**：无 TBD；两处测试 helper（withRoleDirs/viewString/openVault/statModTime）在首个使用任务内定义并复用。
- **类型一致性**：`roles.Role/Launch/LaunchKind` T1 定义贯穿；`wizTokenScreen(title,usage,recovery)` T3 定义 T4 复用；`installServeService/uninstallServeService` T4 定义 T6/T7 复用；`serveProbeMsg/serveInstalledMsg` T4 定义 T6 复用；`enumClearTargets/makeSafetyNet/deleteLegacyTimer` 仅 T7 内部。
