# Plan 29 终审修复报告（final-review fix round）

HEAD 基线 `c38dcd2`（worktree `plan-29-editpicker`）。两项修复落地，两个 commit。

## Finding 1（Important）— Confirm 字段泵门 + 缺失测试

### 库侧事实核对（huh v2.0.3，模块缓存原文）

- `keymap.go:182-183`：`ConfirmKeyMap.Accept = key.NewBinding(key.WithKeys("y", "Y"))`、`Reject = ("n", "N")`。
- `field_confirm.go:213-218`：Accept/Reject 两分支**既 Set 值又 `cmds = append(cmds, NextField)`** —— 终审所述"设值并返回 NextField"属实；泵门只认 Enter/Tab 时，按 `y` 翻值但表单不完成。
- `group.go:222-236` + `form.go:576-589`：`NextField → nextGroup → OnLast → StateCompleted`，`pumpForm` 递归可走完整条链（与 Enter 路径同链）。
- 负例依据（为何必须按字段种类条件化）：Input 字段的字符键 cmd 是 cursor blink 重臂，同步执行每次阻塞 ~530ms（blink context timeout）——不能无条件泵。

### 缝合选择

`editField` 新增 `Confirm bool`（editfields.go:15）而非 `p.field.Key == "clearcredential"` 字符串硬编码：表是权威字段元数据，bool 让未来任何 Confirm 字段（若有）自动进门；`clearCredentialEditField()` 是唯一 `Confirm: true`。

### 门改动（editpage.go）

- 新 helper `confirmAnswer(kp)`：按 `kp.String()` 匹配 `y/Y/n/N` —— 与 huh `key.Matches` 比较的是同一表示形式，`ctrl+y` 之类的组合键不误入（String 为 "ctrl+y"）。
- 门（editpage.go:221-223）：`Enter/Tab || (p.field.Confirm && confirmAnswer(kp))`。`p.field` 仅在 field 态可达（Update 路由保证）。
- 注释改为经核对的清单：Enter/Tab（Input 与 Confirm 的 Next/Submit），外加 Confirm 上的 y/Y/n/N（Accept/Reject 设值并带 NextField）；并写明 y/Y/n/N 臂必须按字段种类门控的原因（Input 字符键 = blink 重臂 = ~530ms 阻塞陷阱）。

### 测试（TDD：先 RED 后 GREEN）

`TestEditPageConfirmSingleKeyCommits`（editpage_test.go:172-214，模式同 `TestEditPageFieldEditMarksDirty`）：
- `y` 于清除凭据（编辑态 index 8，表内唯一 Confirm）→ 断言回 list 态、`ClearCredential=true`、行显示 `● 清除凭据` + `已勾选`；
- `n` 于新开页同字段 → 断言回 list 态、`ClearCredential=false`（干净值完成）；
- 负例：`y` 于备注（index 14，Input）→ 断言留在 field 态且 `Description == "y"`（普通字符，无完成、无 530ms 泵）。

**RED→GREEN 证据**：

```
$ go test ./internal/tui/ -count=1 -run TestEditPageConfirmSingleKeyCommits -v   # 修复前
--- FAIL: TestEditPageConfirmSingleKeyCommits (0.02s)
    editpage_test.go:181: y on the Confirm must complete the form, got state=1   # state=1 = editStateField

$ go test ./internal/tui/ -count=1 -run TestEditPageConfirmSingleKeyCommits -v   # 修复后
--- PASS: TestEditPageConfirmSingleKeyCommits (0.06s)
```

## Finding 2（Important）— backlog #9 措辞（repo 侧）

`docs/backlog.md` item 9 修法原文 `App.Update 把 overlay 返回的 cmd 正确交还 tea 运行时` 不准确：app.go:161-163 **本来就 `return a, cmd`**（cmd 已交还运行时），真正的缺口是 cmd 执行后产生的**消息**（nextFieldMsg/nextGroupMsg 等）回到 App.Update 后只命中 KeyPressMsg 分支、从不路由回 overlay。改为：

> 修法=App 层把 overlay 返回的 cmd 执行后产生的**消息**（nextFieldMsg/nextGroupMsg 等）路由回 overlay（当前只有 KeyPressMsg 被转发；可能需过滤 blink/tick 类），影响面=formOverlay/importflow/wizard。

sdd/ 侧 checklist/report 持久化归 controller，本轮未触碰 sdd/ 既有文件（本报告除外）。

## 验证输出（全跑）

```
$ go test ./internal/tui/ -count=1
ok  	ssh-manager-mcp/internal/tui	2.937s

$ gofmt -l internal/tui/
（空输出）

$ go vet ./internal/tui/
（无输出）
```

backlog.md 非 ASCII 编辑的字节级验证：

```
$ iconv -f UTF-8 -t UTF-8 docs/backlog.md | grep -cF "路由回 overlay"
1
$ grep -cF "正确交还 tea 运行时" docs/backlog.md
0
```

（新短语 UTF-8 往返后恰好 1 处；旧短语全域 0 处。）

## 自审

- 行为变化仅限加宽的泵门；Enter/Tab 路径逐字节未动。
- `confirmAnswer` 用 String 形式匹配 = 与 huh 自身键匹配同构，修饰键组合不误入。
- huh Confirm 的 Toggle（h/l/←/→）只翻值不带 NextField（field_confirm.go:204-208 无 cmds append）——不需要也不应泵，注释清单未列它们，正确。
- 老 long-form（forms.go 三组表单）的同类问题是 backlog #9 的 App 层路由，本轮不动（scope 纪律）。
- 无新依赖；gofmt/vet 干净；工作区仅本轮两 commit 之内的改动 + 本报告。

## Commits

```
f8d9d20 fix(tui): pump Confirm y/n keys + correct gate invariant + clear-credential interactive test (final-review)
07a733c docs: backlog#9 wording — message routing, not cmd return (final-review)
```

（commit 1 含 editpage.go / editfields.go / editpage_test.go；commit 2 仅 docs/backlog.md。本报告文件保持未提交，留 plan 收口。）
