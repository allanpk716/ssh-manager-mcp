# Task 2 Report — 向导骨架（状态机 + 首屏 + 可重入接线）

**Status: COMPLETE** — commit `bdf30d1` on `feat/plan19-role-wizard`.

## What was built

### `internal/tui/wizard.go` (new)
- `type wizStep int`：`stepPick` / `stepRoleDone`（Task 3-5 从 stepRoleDone 分流到各角色步骤模型）。
- `type wizardData struct{}` — 预留的流程答案容器（Task 3-5 扩展）。
- `wizAnswers{keep, share}` — 首屏 huh 绑定，**堆分配一次**、模型持指针：wizardModel 按值穿过 Update，值字段取址会在第一次拷贝后变悬垂。
- `wizardModel{launch, step, role, data, askShare, ans, form, residualClient, saveErr}`。
- `newWizard(l)` — spec §2.1 后果导向两级问题，**两张连续 huh Select**（非单表条件显隐）：q1「这台电脑要保管所有 SSH 凭据吗？」[是——凭据只存这台机（其他机器不能用了它就都用不了）｜否——…→ client]；答「是」才开 q2「这台机器上的 agent 需要连别的电脑吗？」[只有本机用→单机｜要给其他机器共享→server]；答「否」直达 `chooseRole(RoleClient)`。构造时探测 `roles.Load()` 残留 client role.json → 底部提示「检测到本机曾有 client 配置，可运行 ssh-manager clear 清理」（spec §1.3 client→vault 转换场景）。
- `newWizardForRole(l)` — 续配入口：跳过首屏直达 stepRoleDone（role 已在盘上，不重写 role.json）。
- `chooseRole(r)`（指针接收者）— **选定即写盘**：`roles.Save(State{Role:r, SetupComplete:false})`，失败存 `saveErr` 并在占位页红字显示。这就是防死态不变量。
- 首屏底部恒显「概念图解：docs/concepts.md（或 --help）」；stepRoleDone 占位页文案「<角色> 角色流程将在下一步实现 / q 退出（进度已保存，重开 tui 会继续）」，角色名带中文标签（单机/服务器/客户端）。
- 按键：`q` / `Ctrl+C` / `Esc` = 退出（Esc 即暂停退出，状态已落盘）；在传给 huh **之前**拦截，"q" 不会漏进表单键处理；huh StateAborted 兜底退出。
- v2 API：`tea.KeyPressMsg` + `k.Key()`；`View() tea.NewView`；huh form 走 `form.Update` → `StateCompleted` 驱动状态机（与既有 formOverlay 同款模式）。

### `internal/tui/mode.go` (modified)
- `launchTarget(l roles.Launch) string` — 纯分派表（可单测）：`ResumeSetup && Kind!=LaunchClient → "wizard"`；否则按 Kind → wizard/broker/client。**client+ResumeSetup 本任务仍回 client 面板**（Task 5 才给 client 向导表单入口——任务指令明示）。
- `Run(modeFlag string)` — 签名从 `Run(Mode)` 改为收 `--mode` 原串；先 TTY 检查 → `roles.ResolveMode(modeFlag)` → 按 `launchTarget` 分派：wizard（ResumeSetup 且角色为 standalone/server 时 `newWizardForRole` 预选进入）/ client（`newClientModel` 原路径）/ broker（`vault.OpenStore`+`NewBrokerApp` 原路径，逐字保留）。
- `DetectMode`/`DetectModeWith`/`Mode` 保留——mode_test.go 仍直接测它们（探测护栏 stat-first 行为不回归），除测试外已无生产引用，属过渡态。

### `internal/cli/tui.go` (modified)
`RunE` 不再调 `DetectMode`，`--mode` 原串透传 `tui.Run(mode)`；flag 帮助文案更新为「force mode: client (default: resolve via role.json + machine probes)」（与 ResolveMode 只接受 client 护栏一致）。

### `internal/tui/wizard_test.go` (new)
- `withRoleDirs` — 逐字复制 roles 包 `withDirs` 的全量环境钉死（SSHMGR_STORE/FILEKEY_PATH/MASTERKEY_HEX/CACHE_DIR/SERVE_CERT/APPDATA/XDG_CONFIG_HOME）。
- `seedWizardVault` — roles 包 `seedVault` 镜像（删占位 store.db → 建库 → 写 master key 到钉死路径）。
- `newWizardForTest()` — `newWizard(Launch{Kind:LaunchWizard})`。
- `TestWizard_FirstScreenSavesRole`（brief 逐字 + 两处必要修正，见下）：chooseRole(server) → role.json 内容含 `"role":"server"` 与 `"setup_complete":false` → `ResolveMode("")` 返回 ResumeSetup+RoleServer。
- `TestWizard_ResumeSkipsFirstScreen` — newWizardForRole 直接 stepRoleDone、View 含角色名。
- `TestLaunchTarget` — 7 行分派矩阵（wizard/broker/client × ResumeSetup，含 client-resume 特例）。

## Deviations from the brief

1. **brief 测试必须 seed vault 才能过 resume 断言**：Task 1 落地的 `resolveFromState` 对「role.json=standalone/server 但 vault 缺失」**硬错误**（引导 clear，Task 1 测试钉死）。brief 测试只 chooseRole 不建 vault → `ResolveMode("")` 必然报错而非 ResumeSetup。修正：断言前 `seedWizardVault`（role.json 写 vault 目录，与 store.db 无冲突）。附带加了 `"setup_complete":false` 内容断言。
2. **`w.View().String()` → `w.View().Content`**：bubbletea v2.0.8 的 `tea.View` 无 String() 方法，导出字段是 `Content`。
3. 首屏「两级 Select 树」实现为**两张连续表单**（q1 完成后按答案决定开 q2 或直达 client）——huh 无条件显隐字段，连续表单是既有 formOverlay 模式的自然延伸，且答案分流逻辑显式可测。

## Implementation notes for downstream tasks (Task 3-5)

- **进入各角色流程的挂点**：`stepRoleDone`（wizard.go Update 里该分支目前只吞按键）。Task 3-5 把占位 View 换成各角色步骤模型/页面栈；`wizardData` 从空壳开始长字段。
- **`ans` 指针模式必须延续**：任何新 huh 绑定一律挂堆分配结构（`ans` 或新容器），不要对 wizardModel 值字段取址。
- **续配语义**：`newWizardForRole` 不重写 role.json（盘上已是 `setup_complete:false`）；Task 3 的 `wizEnsureVault` 是续配幂等的关键（已存在 vault 跳过重建）。
- **client 续配占位**：`launchTarget` 里 `ResumeSetup && Kind==LaunchClient → "client"` 是本任务的显式特例，Task 5 接手后应改回 wizard 入口。

## Verification

- TDD：先跑测试确认编译失败（`undefined: wizardModel/newWizard/newWizardForRole/stepRoleDone/launchTarget`）→ 实现 → 绿。
- `go build ./...` ✓ `go vet ./...` ✓
- `go test ./internal/tui/ ./internal/roles/ -count=1` → ok（tui 5.3s / roles 2.3s）
- `go test ./... -count=1` 全仓 14 包全 ok（含 cli——`tui.Run` 签名变更的调用方）。
- commit `bdf30d1`：4 files changed, 356 insertions(+), 22 deletions(-)。

## Concerns

1. **⚠ spec 内在矛盾（上报，未擅改）**：向导在选定角色的瞬间写 role.json，而 standalone/server 的 vault 在角色流程步骤②才创建——若用户在这两步之间退出，下次启动 `ResolveMode` 会按「vault 角色缺 vault」硬错误引导 `clear`，与 spec §2.2「无死态/可重入」相抵触。本任务测试用 seed vault 规避（证明 vault 建立后的 resume 正确）。建议 Task 3 二选一：(a) `resolveFromState` 对 `SetupComplete=false` 的 vault 角色缺 vault 改为 LaunchWizard 续配而非报错（更符合向导语义）；(b) chooseRole 对 standalone/server 同步预建 vault。**倾向 (a)**——那是「向导进行中」而非「数据被外部破坏」。
2. `DetectMode`/`DetectModeWith`/`Mode` 现仅剩测试引用（生产路径已全走 `roles.ResolveMode`）。可在 Task 5/9 收尾时删除并迁移 mode_test 的护栏断言到 roles 包，避免长期双轨。
3. 向导完成后的「进主控台」链路（brief Step 4 括号语义）要等 Task 3-5 各角色流程落地 setup_complete:true 后才真正闭环；本任务 quit 后重开即 resume，行为正确但尚无「完成」出口。
