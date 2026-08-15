# Plan 21 设计：卫生清扫 + eval §12/§13 收尾（spec v1）

> 日期：2026-08-16。来源：Plan 20 终审 triage 的 deferred minors + 用户四轮拍板（--clear-credential / T7 记局限不修 / 差分套餐扩展 / CI 零代码手动 dispatch）。
> 与 v0.7.0 发版解耦：发版不等本 plan；本 plan 合并后随下次发版走。

## 背景

Plan 20 合并（`2f44fd4`）后，终审 triage 留下一批「可后置」项；eval 侧 §12/§13 自 Plan 5e/6 后有三个悬置收尾（CI 权威基线、T7 本地命令残余、SFTP/upload-recursive 差分深度）。本 plan 一次收拢。

## Stream A：卫生清扫（3 任务）

### A1 状态统一 + wizard footer 条件化

- **`no_credential` 状态统一**：`internal/mcpserver/core.go` 的 `download_file`（:235 附近）/ `upload_file`（:332 附近）/ `forward_port`（:447 附近）三处 `vault.ErrNoCredential` 错误分支，审计状态从 `auth_error` 改判 `no_credential`（exec 已是此语义，Plan 20 T5）。**错误文案不变**（remedy 提示照旧），只统一状态码。各补一条 status 断言测试（复用 Plan 20 T5 core_test 的无凭据 server 构造模式）。
- **wizard footer 条件化**：`internal/tui/wizard.go` 三处（:759/:769/:813 附近，T3 时确认的三站点）「角色已保存/进度已保存」footer 改为 `saveErr == nil` 才显示——消掉「上横幅报写入失败、下 footer 说已保存」的矛盾。绑定测试：saveErr≠nil 时 View() 不含「已保存」且含错误横幅。

### A2 `--clear-credential` + 小清扫

- **CLI**：`servers edit <name> --clear-credential` → server 置无凭据态（`CredentialID=""`+`AuthMethod=""`，同事务删旧凭据当且仅当两列均无他处引用——复用 `store.UpdateServerWithCredentials` 传「清除哨兵」或新增 `ClearServerCredential(id)`，实现时按 tx.go 现有形状择优）。与 `--password`/`--key` 互斥（三者同时给=报错）。同时剥 `needs-passphrase` 标签（凭据都没了，标签无意义）。
- **TUI**：edit 表单加「清除凭据」勾选（huh Confirm 或 checkbox）；勾选时提交走清除路径并忽略密码/私钥栏。
- **顺手小清扫**：`keyPathField(d)` 抽取并入 T9 的 inputTitle 锁测试（key-path 标题从此有守卫）；`mcpConfigLines` 对空 fieldLayers 做防御（不再可能出尾逗号）；`dropTag` 注释改为准确表述（移除全部出现，非「minus one occurrence」）。

### A3 欠钉测试补齐 + eval harness 对齐

补钉以下已有正确行为但无测试的接缝（全部失败前置可证）：
1. mcpserver `serverInfo` version：注入 `buildinfo.Version="test-x.y.z"` 断言 initialize 响应携带（T12 遗留——cmd 侧已测，server 侧没测）。
2. condKey+空口令提交：TUI supplement 表单对 needs-passphrase 目标只填 role 不填口令 → 提交成功、凭据保留、**标签保留**（T10 评审确认正确但未钉）。
3. cert 失败零孤儿：`SSHMGR_SERVE_CERT` 指坏 PEM → `cache-tokens add` 报错且 cache_tokens 表零新行（T4 评审建议的便宜测试）。
4. TUI dispatch 空列表 e/d no-op 分支（T1 评审遗留）。
5. `internal/eval/broker.go` 三处 argv `--token` 切 env 形态（`env: SSHMGR_TOKEN`，与 T11 生产生成器一致；flag 仍被支持所以测试语义不变）。

## Stream B：eval 收尾（2 任务）

### B1 §13 差分套餐扩展

`internal/conformance/upload_forward_test.go` 的 `TestUploadDifferential`（现平铺单层）扩展为套餐（每项均 vs 真 `scp -r` 零差分断言，Docker gate 内跑）：
- **嵌套子目录递归**（3 层深，含混合文件分布）
- **空目录**：scp -r 保留空目录——断言我们一致
- **空文件**（0 字节）
- **unicode + 空格文件名**（中文/emoji/中间空格）
- **边界尺寸**：刚好超 §6 输出 cap 的文件 → broker 截断行为 vs scp 的完整传输：**显式断言差异被预期**（cap 是我们的安全特性，不是 bug；测试锁「截断发生在边界且差分报告如实」，不是锁「与 scp 相同」）

`TestDownloadDifferential`（SFTP 下载）同构补一套（嵌套/空目录/空文件/unicode；下载无 cap 差异问题）。

### B2 文档收尾（零代码）

- `internal/eval/README.md` 增「已知局限」段：T7 本地命令残余（`--bare` 保留 Bash → agent 可跑宿主机 nvidia-smi 冒充远程；现有 judge+幻觉合取门的覆盖范围；Fable-5 基线 3/5；不修的原因=单任务禁 Bash 曾破坏测量、数字钉死门留作后续选项）。
- CI 权威基线 runbook（README 或 docs/eval 相关文档）：OWNER 步骤 = GitHub repo settings 配 `secrets.ANTHROPIC_API_KEY` → Actions 页手动 dispatch `eval-nightly` → 取 run 产物 summary → 收编为 `baseline-claude-ci.json` + `baselineForModel` 登记。**收编动作届时 controller 协助执行，非本 plan 任务。**

## 依赖与并行

- 两 stream 无相互依赖；A1/A2/A3 内部也无（不同文件）。B1 依赖 Docker（既有 gate 机制）。
- 全部任务可任意串行序；建议 A1→A2→A3→B1→B2。

## 边界与不做

- 不做 T7 代码加固（数字钉死门/单任务禁 Bash——记为未来选项，README 已知局限里点名）。
- 不做 CI 自动 PR 固化（手动 dispatch + 手动收编）。
- 不加深 forward 差分（TestForwardDifferential 已含 half-close 路径，Plan 20 T13 验过）。
- 不动 v0.7.0 发版节奏。

## 验证策略

- A1：三工具 status 断言 + wizard footer 双态断言。
- A2：--clear-credential CLI（清后 GetServer 无凭据 + 旧凭据行删除/共享保留两态）+ TUI 勾选路径 + 互斥报错 + 标签剥离；keyPathField 锁测试含变异确认。
- A3：五项各自失败前置（先证明缺测试时行为无守卫）。
- B1：Docker gate 内套餐全绿；边界尺寸项断言「预期差异」而非零差分。
- 全量 `go build ./... && go vet ./... && go test ./...`；B1 另跑 `SSHMGR_CONFORMANCE=1`。
