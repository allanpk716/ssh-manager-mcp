# Plan 29 Task 1 Report — 编辑页纯逻辑层（字段表 + 脏计算 + 预览格式化）

**Status: DONE** — commit `59d475c` on `worktree-plan-29-editpicker`
Files: `internal/tui/editfields.go` (+213), `internal/tui/editfields_test.go` (+304) — both new, nothing else touched.

## 1. API as landed (T2 consumes these exact shapes)

```go
// editfields.go — package tui
type editField struct {
    Key    string                         // 稳定标识 = 脏快照键
    Label  string                         // 列表显示名
    Secret bool                           // true = 值预览掩码（仅 password/keypass/sudopassword）
    Get    func(d *serverDraft) string    // Secret 字段返回状态串，绝不返回内容
    Set    func(d *serverDraft, v string) // 写回；端口做 Atoi+1-65535，非法输入=安全 no-op
    Build  func(d *serverDraft) *huh.Form // 单字段表单 = huh.NewForm(huh.NewGroup(builder))
}

func editFields(editing bool) []editField          // 15 项（编辑）/ 14 项（新增，无清除凭据）；无保存项
func snapshotDraft(d *serverDraft) map[string]string // 15 键原值（含秘密明文，仅用于比较）
func dirtyAgainst(d *serverDraft, snap map[string]string) map[string]bool // 改回原值=净
func fieldPreview(f editField, d *serverDraft, dirty bool) (title, desc string)
```

### 字段表（顺序锁定，测试钉死）

| # | Key | Label | Secret | Build 复用 |
|---|-----|-------|--------|-----------|
| 1 | name | 名称 | – | `huh.NewInput().Title("名称（唯一）").Validate(nonEmpty)` |
| 2 | host | Host | – | `Title("Host / IP") + nonEmpty` |
| 3 | port | 端口 | – | `portField(&d.Port)` |
| 4 | user | SSH 用户 | – | `Title("SSH 用户") + nonEmpty` |
| 5 | password | 密码 | ✔ | `passwordField(d, editing)`（模式感知标题） |
| 6 | keypath | 私钥路径 | ✘（路径非秘密，预览显示原值） | `keyPathField(d)` |
| 7 | keypass | 密钥口令 | ✔ | inline `Title("密钥口令（可选）") + EchoModePassword`（与 newServerForm 同构造） |
| 8 | sudopassword | sudo 密码 | ✔ | `sudoPasswordField(d)` |
| 9 | clearcredential | 清除凭据（**仅编辑态**） | – | `huh.NewConfirm` 同 newServerForm 编辑分支 |
| 10-15 | hardware/location/role/services/caveats/description | 硬件/位置/角色/服务/Caveats/备注 | – | 标题与 `structuredFields` 逐字一致 |

- 保存项不在表内（T2 列表哨兵自行追加）——表最后一项是"备注"。
- **注意顺序差异**：字段选择器序是 端口 在 SSH 用户 **前**（设计决策节），与 newServerForm 的 用户在端口前 不同；结构化六字段序（硬件/位置/角色/服务/Caveats/备注）与 structuredFields 一致。

### Get 的返回值约定（T2 直接渲染）

- 非秘密字符串：原值；端口：`strconv.Itoa`；清除凭据：`"未勾选"`/`"已勾选"`。
- 秘密状态串（`secretStatus`，模式感知，闭包捕获 editing）：
  - 编辑态已填 → `已设（新值）`；新增态已填 → `已设`
  - 编辑态空：密码/sudo → `（留空=保持现有）`；keypass → `（未设）`（keypass 没有可保持的现有值，只随新私钥路径生效）
  - 新增态空 → `（未设）`

### Set 约定

- 字符串直写；端口 TrimSpace+Atoi+1-65535 夹验证，非法输入保持原值（no-op）；清除凭据接受 `"已勾选"` 或 `"true"` → true，其余（含 `"false"`/空）→ false——**snapshot 原值可经 Set 往返**（T2 的 field 态 Esc 恢复可走 `f.Set(d, snap[f.Key])`，秘密字段的快照值是明文原值，Set 收明文，闭环成立）。

### fieldPreview 输出

- title：净 = `<Label>`；脏 = `● <Label>`（无着色——lipgloss 归 T2）。
- desc：`f.Get(d)`；空 → `（空）`；脏 → 追加 `（已改）`。例：净硬件="hw"；脏硬件="● 硬件"/"hw（已改）"；脏空值="（空）（已改）"；秘密脏="已设（新值）（已改）"。
- 秘密掩码机制：Get 本身返回状态串，fieldPreview 无需分支即天然掩码。

## 2. TDD evidence

- **红**（实现前）：`undefined: editField / editFields / snapshotDraft / dirtyAgainst …`（build failed，10+ 符号未定义）。
- **绿**：`go test ./internal/tui/ -count=1` → `ok ssh-manager-mcp/internal/tui 2.517s`；新增 8 个测试全过：
  - `TestEditFieldsTableShape` — 15/14 项、标签精确顺序、键唯一、Get/Set/Build 非空、Build 非 nil（双模式）。
  - `TestEditFieldsSecretFlags` — 三个秘密字段 true；keypath 显式 false。
  - `TestEditFieldsKeysMatchSnapshot` — snapshot 恰 15 键=表键集合；**每个字段的 Set 恰好只弄脏自己的键**（双模式逐一验证）。
  - `TestEditFieldsGetSetRoundTrip` — 10 个字符串字段往返；端口 Atoi 失败路径（`not-a-number`/`0`/`70000`/`-1` 均 no-op）+ TrimSpace；清除凭据双形态解析。
  - `TestSecretGetNeverReturnsContent` — Get 永不含内容。
  - `TestSecretStatusStrings` — 状态串精确文案锁定（上表）。
  - `TestDirtyAgainstComputation` — 全净起点；单改仅单脏；**改回原值→净**；勾选清除凭据→脏。
  - `TestFieldPreviewMasksSecrets` — **哨兵断言**：Password/KeyPass/SudoPassword 各塞 SENTINEL-*，preview（净/脏×编辑/新增四组合）均不含 SENTINEL；keypath 预览显示原路径（非秘密）。
  - `TestFieldPreviewFormat` — ● 前缀/（已改）后缀/（空）/端口数字/未勾选。
- `gofmt -l internal/tui/` 零输出；`go vet ./internal/tui/` 干净；`go build ./...` 通过。

## 3. Self-review

- **提交语义零变化**：未动 newServerForm/submitServer/prefill/serverDraft；仅新增两文件。Build 全部复用 forms.go 构造器或逐字同标题 inline（keypass/清除凭据的 inline 构造与 newServerForm 字节级同文案——keypass 标题在 forms.go 本就是 inline，无处可复用）。
- **秘密掩码深度**：三层——Get 状态串（源头）、fieldPreview 无明文路径（消费端无分支需求）、测试哨兵锁行为。snapshotDraft 含明文但注释+设计上仅用于比较、永不渲染（与铁律无冲突：不落盘不出 TUI 视图）。
- **Key↔snapshot 双源**：editFields 的 Key 与 snapshotDraft 的字面键是两处来源，`TestEditFieldsKeysMatchSnapshot` 锁定对应（漏键/错键立即红）。
- **端口 no-op 决策**：Set 无法返回错误（接口无 error）；huh 表单自身已验证，Set 见到非法输入只可能是编程错误，选保持原值而非写 0/panic。

## 4. Concerns（给 T2/T3）

1. **field 态 Esc 恢复**：Build 是指针直绑（`Value(&d.X)`），huh 输入过程可能已写穿到 draft。T2 进 field 态前须存 `snapshotDraft(d)[f.Key]`，Esc 时 `f.Set(d, 存值)` 恢复（Set 已保证快照值可往返，含端口/布尔/秘密明文）。**不要**用 `f.Get` 存值再恢复——秘密字段的 Get 是状态串，会把状态串写进 draft。
2. **编辑态秘密"已设"的歧义**：Get 的状态串只反映 draft（prefill 后为空=保持现有），不知道原服务器是否真有凭据。若 T2 想显示"原服务器有无凭据"，需从 orig（AuthMethod/CredentialID）另行推导，勿改 Get。
3. **端口 Get 在零值时**：`strconv.Itoa(0)` = "0"。编辑页 draft 必经 prefill（Port=真实值），不会出现 0；add 模式若 T2 未来复用本表，portField 的字符串镜像在 0 时默认显示 "22"，但 Get 仍返回 "0"——预览与表单默认不一致，目前无消费者，挂账即可。
4. **Build 标题无深度断言**：huh 内部不可检（仓内先例 TestNewServerFormComposesConstructors 同样只 smoke）；标题文案正确性靠"逐字复制+文案评审"，非测试锁定。

## 5. 验证命令记录

```
go test ./internal/tui/ -count=1        → ok 2.517s（含既有全量）
go test ./internal/tui/ -count=1 -run 'EditField|Secret|Dirty|FieldPreview' -v → 全 PASS
gofmt -l internal/tui/                  → 空
go vet ./internal/tui/                  → 干净
go build ./...                          → 通过
```
