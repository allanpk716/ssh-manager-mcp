# Plan 21 实施计划：卫生清扫 + eval §12/§13 收尾

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 从 spec `docs/superpowers/specs/2026-08-16-plan-21-hygiene-sweep-eval-closure-design.md`（commit bd1ade5）落地 5 个任务：Plan 20 终审遗留清扫（no_credential 状态统一、wizard footer 条件化、--clear-credential、欠钉测试×4、eval harness env token）+ eval 收尾（§13 差分套餐、T7 已知局限与 CI runbook 文档）。

**Architecture:** 两 stream 无依赖；全部是既有代码的收口与测试补强，无新架构面。B1 在既有 Docker conformance gate 内扩展 fixture。

**Tech Stack:** Go 1.25、既有依赖（mcp go-sdk v1.2.0、testsshd、Docker conformance gate）。

## Global Constraints

- 仓库 PUBLIC：任何代码/测试/文档不得出现真实 token、密码、指纹、主机坐标。
- 每任务 TDD：失败测试先行；`go build ./... && go vet ./...` 干净。
- 测试不得触碰生产固定路径——store 相关测试一律 `SSHMGR_STORE`/`SSHMGR_FILEKEY_PATH`/`SSHMGR_SERVE_CERT` 等 env seam。
- 主 worktree 共享：实施在 isolated linked worktree（SDD controller 负责）。
- 既有测试不可弱化；删除/改写断言须在报告逐条说明。
- 提交前缀 `fix:`/`feat:`/`test:`/`docs:` + 英文一行。
- **当前源码是 ground truth**——本计划引用的行号在 fix-wave 后可能漂移，以符号名定位为准；计划代码块与源码冲突时，先报告冲突再按源码现状适配。

---

### Task 1 (A1): no_credential 状态统一 + wizard footer 条件化

**Files:**
- Modify: `internal/mcpserver/core.go`（download ~:235 / upload ~:332 / forward ~:447 三处 AuthForServer 错误分支）
- Test: `internal/mcpserver/core_test.go`
- Modify: `internal/tui/wizard.go`（:759/:769/:783/:786/:793/:813 六处「已保存」footer）
- Test: `internal/tui/wizard_test.go`

**Interfaces:** 无新接口（行为修正）。

- [ ] **Step 1: 失败测试——三工具 no_credential 状态**

先看 `core_test.go` 里 T5 留下的 exec 无凭据测试（`TestExecNoCredential` 或近似名，grep `no_credential`）——它示范了「种无凭据 server + 调工具 + 断言 status」。对 download/upload/forward 各写一个同款：

```go
func TestDownloadNoCredentialStatus(t *testing.T) {
	st, srvID := seedCredentialLessServer(t) // 无凭据 server + profile grant（照抄 exec 测试的种子 helper；若无则就地内联同款逻辑）
	// 直接调 download 路径的 ForProfile 函数（grep core.go 中 AuthForServer :235 所在函数名）
	res, err := DownloadForProfile(st, "prof", srvID, "/tmp/x", t.TempDir()+"/out")
	if err == nil { t.Fatal("want error") }
	if !strings.Contains(fmt.Sprint(err), "no credential") && !strings.Contains(fmt.Sprint(err), "尚未配置凭据") {
		t.Fatalf("err should carry the remedy text: %v", err)
	}
	// 状态断言：该函数若不返回 status，则断言 audit 落行 status（照抄 exec 测试读取 audit 的方式）
	// 期望 status == "no_credential"（现实现为 "auth_error" → 测试红）
}
```

（upload/forward 同款三份；函数名以 core.go 实际为准——`UploadForProfile`/`ForwardPortForProfile` 或闭包内联。若 download/upload/forward 的 status 只存在于 audit 行，则断言走 audit 查询，与 exec 测试同构。）

- [ ] **Step 2: 实现——三处加 errors.Is 分支**

exec 的现成形状（core.go:110-117）：

```go
auth, aerr := vault.AuthForServer(st, srv)
if aerr != nil {
	if errors.Is(aerr, vault.ErrNoCredential) {
		status = "no_credential"
	} else {
		status = "auth_error"
	}
	err = aerr
	return
}
```

把 download/upload/forward 三处（grep `AuthForServer` 的 :235/:332/:447 三个函数）的 `status = "auth_error"` 分支改成同款。**错误文案不动**（ErrNoCredential 自带 remedy 文本）。

- [ ] **Step 3: 失败测试——wizard footer 条件化**

`wizard_test.go` 已有 saveErr+View 断言模式（T3 加的 vaultErr banner 测试）。新增：

```go
func TestWizardFooterHidesSavedWhenSaveErr(t *testing.T) {
	// 对六个 footer 站点各构造一次：saveErr != nil 时 View() 输出：
	//   - 不得包含「已保存」
	//   - 仍含对应步骤的 q/r 键提示（「q 退出」/「r 重试」等，具体措辞按站点）
	// saveErr == nil 时（对照用例）：含「已保存」
}
```

（六个站点分属不同 step 渲染函数——逐一构造对应 step 的 wizard 状态；T3 测试已示范 stepVaultErr 的构造法，其余 step 照 wizard.go 的 render 函数签名推导。若某站点无法单独构造（私有 step 常量+同包测试可直接设），同包测试可直接置字段。）

- [ ] **Step 4: 实现——六处 footer 条件化**

统一模式（以 :759 为例；其余五处同款、保留各自的 r/q 前缀与括号后缀）：

```go
// before
b.WriteString("\n" + footerStyle.Render("q 退出（进度已保存，重开 tui 会继续）") + "\n")
// after
quitHint := "q 退出（进度已保存，重开 tui 会继续）"
if w.saveErr != nil {
	quitHint = "q 退出（role.json 写入失败，进度未保存）"
}
b.WriteString("\n" + footerStyle.Render(quitHint) + "\n")
```

六处各自的原文案保留为 else 分支；saveErr 分支文案按站点语义（:769/:783 的「角色已保存」→「角色未保存」变体）。:786/:793 两处「（进度已保存）」同款。

- [ ] **Step 5: 全量 + 提交**

```bash
go test ./internal/mcpserver/... ./internal/tui/... -count=1
git add -A && git commit -m "fix: no_credential status on download/upload/forward; wizard footers conditional on saveErr"
```

---

### Task 2 (A2): --clear-credential + 小清扫

**Files:**
- Create: store 方法并入 `internal/store/tx.go`（`ClearServerCredential`）
- Modify: `internal/cli/servers.go`（edit 加 flag）
- Test: `internal/cli/servers_test.go`
- Modify: `internal/tui/forms.go`（draft 加勾选 + submitServer 分支）+ `internal/tui/forms_test.go`
- Modify: `internal/tui/wizardsteps.go:186`（mcpConfigLines 尾逗号防御）
- Modify: `internal/tui/servers.go:45`（dropTag 注释）
- Test: `internal/store/tx_test.go`（追加）

**Interfaces:**
- Produces: `func (s *Store) ClearServerCredential(id string) error` —— 置无凭据态 + 剥 `needs-passphrase` 标签 + 级联删独占凭据（两列守卫），单事务。
- CLI flag: `servers edit <name> --clear-credential`（与 `--password`/`--key` 三者互斥）。

- [ ] **Step 1: 失败测试——store 层 ClearServerCredential**

`internal/store/tx_test.go` 追加（复用既有 testStore/key 种子 helper）：

```go
func TestClearServerCredential(t *testing.T) {
	st := newTestStore(t)
	// 种一台带凭据+sudo+needs-passphrase 标签的 server；另一台共享同一登录凭据
	// 1) ClearServerCredential(idA) 后：GetServer(A).CredentialID=="" && AuthMethod==""
	//    && SudoCredentialID=="" && Tags 不含 needs-passphrase
	// 2) 共享的登录凭据行仍在（B 还引用）；A 独占的 sudo 凭据行已删
	// 3) 对不存在的 id：幂等 no-op 不报错（与 DeleteServerCascading 同语义）
}
```

- [ ] **Step 2: 实现——tx.go 追加**

```go
// ClearServerCredential resets the server to the credential-less form in one
// transaction: credential/sudo references cleared, the needs-passphrase tag
// stripped (meaningless without a credential), and exclusively-owned credential
// rows deleted (two-column guard — shared rows survive). Absent id = no-op.
func (s *Store) ClearServerCredential(id string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	srv, err := getServerTx(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // idempotent no-op (DeleteServerCascading semantics)
		}
		return err
	}
	oldCred, oldSudo := srv.CredentialID, srv.SudoCredentialID
	srv.CredentialID, srv.AuthMethod, srv.SudoCredentialID = "", "", ""
	srv.Tags = dropTagFrom(srv.Tags, "needs-passphrase") // 若 store 包已有 stripTag 则复用；否则内联过滤
	if err := updateServerTx(tx, srv); err != nil {
		return err
	}
	for _, cid := range []string{oldCred, oldSudo} {
		if cid == "" {
			continue
		}
		n, err := credentialReferencedElseBy(tx, cid, id)
		if err != nil {
			return err // fail-closed
		}
		if n == 0 {
			if _, err := tx.Exec(`DELETE FROM credentials WHERE id=?`, cid); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
```

（`dropTagFrom`：store 包没有 tag helper 就写包内 3 行过滤函数；`errors`/`sql` 已在文件 import。）

- [ ] **Step 3: 失败测试——CLI flag + 互斥**

`internal/cli/servers_test.go` 追加（env seam 建 tmp store；照 `TestServersEditNeedsPassphraseTag` 的驱动方式）：

```go
func TestServersEditClearCredential(t *testing.T) {
	// 种带凭据 server → edit <name> --clear-credential → GetServerByName 后
	//   CredentialID=="" && AuthMethod==""；旧凭据行数-1（独占时）
	// 互斥：edit --clear-credential --password x → 报错含 "mutually exclusive"，库无变化
	// 同轮再验：edit --clear-credential --key k → 同款互斥报错
}
```

- [ ] **Step 4: 实现——CLI**

`serversEditCmd`：加 `var clearCred bool` + `c.Flags().BoolVar(&clearCred, "clear-credential", false, "remove the server's credentials (back to credential-less)")`；re-credential 段前加：

```go
if clearCred && (pwSet || keySet) {
	return fmt.Errorf("--clear-credential is mutually exclusive with --password/--key")
}
if clearCred {
	if err := s.ClearServerCredential(srv.ID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "cleared credentials for %s\n", srv.Name)
	return nil
}
```

（放在 sudo-password 处理之前——清除语义下 sudo 一并清，不单独设置。）

- [ ] **Step 5: TUI 勾选**

`serverDraft` 加 `ClearCredential bool`；edit 表单（`newServerForm` editing 分支的凭据组）加：

```go
huh.NewConfirm().Title("清除凭据（回到无凭据态）").Value(&d.ClearCredential).Affirmative("清除").Negative("保留"),
```

（仅 editing 模式渲染该栏。）`submitServer` edit 分支开头：

```go
if cur != nil && d.ClearCredential {
	return doAction(st, func() (string, error) {
		if err := st.ClearServerCredential(cur.ID); err != nil {
			return "", err
		}
		return "已清除凭据 " + cur.Name, nil
	})
}
```

forms_test 追加：勾选路径 → ClearServerCredential 被走（断言库内无凭据）；勾选+密码同填 → 清除优先（按上面实现即忽略密码栏——测试钉死此优先级）。

- [ ] **Step 6: 小清扫三件**

1. `keyPathField`：把 `newServerForm` 里 inline 的私钥路径输入抽成 `func keyPathField(d *serverDraft) *huh.Input`（title 与现值逐字节一致：`私钥路径（可选，与密码互斥；编辑时留空=不变）`）；`TestNewServerFormFieldTitles` 加断言。
2. `mcpConfigLines`（wizardsteps.go:186）：读现实现，把「command 行硬编码尾逗号 + fieldLines join」重写为「收集 middle lines 后统一 join、最后一行无逗号」的形状——空 fieldLines 不再可能产出非法 JSON；加一条空列表用例（返回的行拼起来是合法 JSON 片段，用 `json.Unmarshal` 于完整包裹后验证）。
3. `dropTag` 注释（servers.go:45）：`minus one occurrence` → `removes all occurrences`。

- [ ] **Step 7: 全量 + 提交**

```bash
go test ./internal/store/... ./internal/cli/... ./internal/tui/... -count=1
git add -A && git commit -m "feat: servers edit --clear-credential (+TUI toggle); keyPathField; mcpConfigLines guard; comment fix"
```

---

### Task 3 (A3): 欠钉测试补齐 + eval harness env token

**Files:**
- Test: `internal/mcpserver/server_test.go`（serverInfo version）
- Test: `internal/tui/importflow_test.go`（condKey+空口令）
- Test: `internal/cli/cachetokens_test.go` 或新文件（cert 失败零孤儿）
- Test: `internal/tui/app_test.go`（dispatch 空列表）
- Modify + Test: `internal/eval/broker.go`（3 处 argv → env）+ `internal/eval/*_test.go`

**Interfaces:** 无新生产接口。

- [ ] **Step 1: serverInfo version 注入测试**

先探 SDK 访问器：`grep -rn "ServerInfo\|InitializeResult" "$(go env GOMODCACHE)/github.com/modelcontextprotocol/go-sdk@v1.2.0/" --include="*.go" | grep -i "func.*Session\|func.*Client"` ——若 session 暴露 initialize 结果访问器，用 server_test 的 in-memory client 模式：

```go
func TestServerInfoCarriesBuildinfoVersion(t *testing.T) {
	buildinfo.Version = "test-9.9.9"
	defer func() { buildinfo.Version = "dev" }()
	// NewServer + in-memory transports（照 server_test.go:30 模式）
	// 从 cliSession 的 initialize 结果断言 ServerInfo.Version == "test-9.9.9"
}
```

若 SDK 无访问器：fallback —— 把 `mcp.NewServer(&mcp.Implementation{...})` 的实参抽成 `func serverImplementation() mcp.Implementation { return mcp.Implementation{Name: "ssh-manager", Version: buildinfo.Version} }`，NewServer 改调它，测试断言 helper 返回值 + `grep -n "serverImplementation()" server.go` 确认接线行存在（接线确认写进测试注释与报告）。**两条路选一，报告里说明选了哪条及原因。**

- [ ] **Step 2: condKey+空口令提交**

`importflow_test.go` 追加（照既有 supplement 测试的直接方法调用模式）：

```go
func TestSupplementCondKeyEmptyPassphraseKeepsTag(t *testing.T) {
	// 种 needs-passphrase 标签 server（凭据在）
	// supplement 提交：只填 Role，KeyPass 留空
	// 断言：UpdateServerWithCredentials 走 nil-cred 路径 →
	//   GetServerByName 后 CredentialID 不变（凭据保留）
	//   Tags 仍含 needs-passphrase（⚠ 不消失——缺口令事实未变）
}
```

- [ ] **Step 3: cert 失败零孤儿**

照 cachetokens_test 的 seam 模式（SSHMGR_SERVE_CERT/SSHMGR_SERVE_KEY/SSHMGR_SERVE_MARKER 指 t.TempDir()）：

```go
func TestCacheTokenAddCertFailZeroOrphans(t *testing.T) {
	// 坏 PEM 写入 cert 路径；key 同理坏
	// 驱动 cache-tokens add RunE → 期望 error 含 cert 相关文本
	// 打开 store 断言 cache_tokens 表 count == 0（零孤儿——T4 重排的正确性钉）
}
```

（驱动方式照 cachetokens_test 既有用例：cobra 命令构造 + env seam store。）

- [ ] **Step 4: dispatch 空列表 no-op**

`app_test.go` 的 dispatch 表驱动测试（T1 建的）追加两个用例：servers 页空列表（`items` 为空）下按 `e` / `d` → 返回 `(model, nil)` 且 overlay 仍 nil、无 panic。

- [ ] **Step 5: eval broker 切 env token**

`internal/eval/broker.go` 三处（:163/:317/:475）：

```go
// before
"args": []string{"mcp", "--token", plaintextToken},
// after
"args": []string{"mcp"},
"env":  map[string]string{"SSHMGR_TOKEN": plaintextToken},
```

（确认该配置 struct 的字段名——若 args 所在的是匿名 mcpServers JSON 映射，按其形状加 env 键。）跑 `SSHMGR_AGENT_EVAL=1` gate 的既有测试若环境不可用则至少跑 eval 包非 gate 测试 + 全量；报告说明 gate 是否跑了。

- [ ] **Step 6: 全量 + 提交**

```bash
go test ./... -count=1
git add -A && git commit -m "test: pin serverInfo version, condKey empty-passphrase, cert-fail zero-orphans, empty-list dispatch; eval harness env token"
```

---

### Task 4 (B1): §13 差分套餐扩展

**Files:**
- Modify: `internal/conformance/upload_forward_test.go`（TestUploadDifferential 扩套餐）
- Modify: 同文件或新 `download_differential_test.go`（TestDownloadDifferential——若已存在则扩展，grep `DownloadDifferential` 确认）

**Interfaces:** 无（Docker gate 内测试扩展）。

- [ ] **Step 1: 构造套餐 fixture（本地树）**

在 TestUploadDifferential 内把现单层 fixture 换成套餐树（t.TempDir() 本地构造）：

```go
// pkg/ 套餐树：
// pkg/root.txt                      （普通文件）
// pkg/a/one.txt
// pkg/a/b/two.txt
// pkg/a/b/c/three.txt               （3 层嵌套）
// pkg/empty-dir/                    （空目录——scp -r 保留，我们必须一致）
// pkg/zero-byte.txt                 （0 字节文件）
// pkg/中文名-测试.txt               （unicode 文件名）
// pkg/with space.txt                （空格文件名）
// pkg/boundary.bin                  （§6 cap+1 字节——见 Step 3）
```

（文件内容含可校验随机字节；`boundary.bin` 单独最后处理。）

- [ ] **Step 2: 差分主体（不含 boundary）**

沿用现 helper：`scp -r` 上传到远端 `A/`，broker `Upload` 到远端 `B/`，`remoteDiff(t, true, A, B)` 零差分。**空目录若差分失败 = 发现真 bug**（Upload 的 filepath.Walk 是否 Mkdir 空目录）——修 broker 而不是删用例；修法与理由写报告。

- [ ] **Step 3: 边界尺寸（预期差异断言）**

先读 `internal/sshbroker/upload.go` 的 cap 常量名与截断返回形状（`UploadResult` 的 `Truncated`/`Bytes` 字段）。构造 `cap+1` 字节文件：

```go
// broker Upload 单独传 boundary.bin：
//   断言 A: 返回结果如实报告截断（Truncated==true 或等价字段，按实际形状）
//   断言 B: 远端 B/boundary.bin 尺寸 == cap（截断发生在边界，非任意处）
//   断言 C: scp -r 的 A/boundary.bin 尺寸 == cap+1（对照组完整）
// 三条合起来 = 「截断发生在边界且差分报告如实」（spec B1 边界项的验收语义）
// 用例注释明标：cap 是安全特性，与 scp 的差异是预期的，本测试锁的是边界精确与如实报告
```

- [ ] **Step 4: Download 同构套餐**

`TestDownloadDifferential`（grep 确认存在与否；不存在则照 Upload 差分的 helper 反向新建：远端 scp 放树 → broker `Download` 拉回 → 本地 `diff -r` 零差分）：嵌套/空目录/空文件/unicode 四项（下载无 cap 差异问题，不含 boundary 项）。Windows 本地 diff：用 `filepath.Walk` + 逐文件 `os.ReadFile` 比较替代 `diff -r`（跨平台）。

- [ ] **Step 5: gate 内验证 + 提交**

```bash
SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run 'TestUploadDifferential|TestDownloadDifferential' -v   # 全绿
go test ./... -count=1   # 默认 gate 自跳过
git add -A && git commit -m "test: upload/download differential suite — nesting, empty dir/file, unicode, boundary cap"
```

---

### Task 5 (B2): 文档收尾（零代码）

**Files:**
- Modify: `internal/eval/README.md`（已知局限段）
- Modify: `internal/eval/README.md` 或 `docs/`（CI 权威基线 runbook）

**Interfaces:** 无。

- [ ] **Step 1: 已知局限段**

`internal/eval/README.md` 追加「已知局限（Known Limitations）」小节，收录（措辞与文档风格一致）：

- **T7 本地命令残余**：`--bare` 保留 Bash → agent 可跑宿主机命令（如本地 `nvidia-smi`）冒充远程检查。现有防线 = judge+幻觉合取门（`score.go` 的 hallucination gate）；Fable-5 基线 T7=3/5。不修的原因：全局禁 Bash 曾破坏测量（agent 无工具不行动，Plan 5e 教训）；单任务禁用与数字钉死门（容器 fixture 唯一数字 vs 报告数字）记为未来选项（Plan 21 spec §B2 点名）。
- 顺带收录既有如实声明（隔离模型 = 工具面无泄漏非文件系统不可见——README 已有则不重复）。

- [ ] **Step 2: CI 权威基线 runbook**

同 README（或 docs/ 下 eval 相关文档）追加「CI 权威基线」小节，OWNER 步骤：

1. GitHub repo → Settings → Secrets and variables → Actions → 新增 `ANTHROPIC_API_KEY`（真实 key；workflow `.github/workflows/eval-nightly.yml:30` 已引用）。
2. Actions 页选 `eval-nightly` → Run workflow 手动 dispatch 一次。
3. 取 run 产物里的 summary JSON → 收编为 `internal/eval/baseline-claude-ci.json` + `baselineForModel` 登记模型 tag（收编动作可让 controller 协助——非本 plan 任务）。
4. 后续刷新 = 再 dispatch + 重收编（无自动化，YAGNI 决策见 Plan 21 spec）。

- [ ] **Step 3: 提交**

```bash
git add -A && git commit -m "docs: eval known-limitations (T7 local-command residual), CI authoritative baseline runbook"
```

---

## 任务依赖图

```
T1(A1) T2(A2) T3(A3) T4(B1) T5(B2)   [全独立，任意串行序；建议按号]
```

串行执行顺序（subagent-driven 默认）：T1→T2→T3→T4→T5。
