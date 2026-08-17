# Plan 29 Task 4 Report — 文档 + 全量验证 + owner gate 移交

**Commit:** `c38dcd2 docs: field-picker edit page usage + backlog overlay-routing item (Plan 29 T4)`（branch `worktree-plan-29-editpicker`，2 files, +9 lines，working tree clean）

---

## 1. Doc placement

**File:** `C:\WorkSpace\agent\ssh-manager-mcp\docs\managing-servers.md`
**Position:** 新增子节 `### TUI 等价：服务器页 \`e\` 字段选择器编辑页`，插在「编辑服务器：`servers edit`」一章的互斥提示（`> \`--password\` 和 \`--key\` 在 \`edit\` 里同样**互斥**…`）之后、`### 清除凭据：--clear-credential（独占动作）` 之前（现约 L213-221）。

**Why here:** grep 全 docs/ 后确认 TUI `e` 编辑的既有描述全部集中在 managing-servers.md（L154 / L178 / L179 / L224），且该章已有同款「TUI 等价流程（服务器页 `i`）」子节先例——新子节与 CLI `servers edit` 主体描述并列、紧邻清除凭据小节（保存语义里的「清除凭据走独占路径」正好回指它），是最自然的落点。

**Content（与实际行为逐条核对过代码后落笔）：**
- `e` 打开「编辑服务器」字段选择页：每行一个字段 + 末行 `✓ 保存并退出` 哨兵项；页脚实时 `第 X/Y 页`；`↑↓` 跨页选到每一个字段（`←`/`→`/pgup/pgdn 直接翻页，来自 `rebindListKeys`，panels.go:39-40）；`Enter` 进单字段编辑、再 `Enter` 确认回列表。
- 脏标记：`●` 前缀 + 亮色（warnStyle）+「（已改）」+ 新值；未编辑字段原值原样（空值显示（空））；秘密字段只显状态文案（`已设（新值）`、`（留空=保持现有）` 等），全程无明文（editfields.go `secretStatus`）。
- 撤销：单字段 `Esc` 放弃本字段（回到**进入本字段时**的值——多次进出时基线是上一次会话值，非进页原值，措辞如实）；列表态 `Esc` 整体放弃、零落库（editpage.go:143-145）。
- 保存：末行 `Enter` 一次性提交；未动字段净效果不变、秘密留空=保持现有凭据（forms.go `toParts` nil=keep + `prefill` 只拷非秘密字段）；勾选清除凭据走独占清除路径（forms.go:301-308）。

## 2. Backlog item 9（docs/backlog.md 末尾追加）

> 9. **App overlay 消息路由只转发 KeyPressMsg**——现状：`App.Update` 给 overlay 只送 `tea.KeyPressMsg`，huh 的 `nextFieldMsg`/`nextGroupMsg`（及其他命令消息）在路由层被丢弃 → 嵌入表单（新增 / 向导 / importflow 补全）在真终端无法前进字段/组；编辑页（Plan 29）靠内部泵 `pumpForm` 规避，其余 overlay 未规避。修法=App.Update 把 overlay 返回的 cmd 正确交还 tea 运行时（可能需过滤 blink/tick 类），影响面=formOverlay/importflow/wizard。来源：Plan 29 T2 实证（`internal/tui/app.go:150-163`）。

（现状+修法按任务指定文本落笔；代码锚点核对过——app.go:158-164 的 switch 只有 `case tea.KeyPressMsg` 会转发给 overlay，huh 命令消息无 case 可命中。）

## 3. Full verification（每条独立简单命令，均在工作树最终态=提交态上跑）

| Command | Result |
|---|---|
| `go build ./...` | 零输出，通过 |
| `go vet ./...` | 零输出，通过 |
| `gofmt -l .` | 空（无文件需格式化） |
| `go test ./... -count=1` | 全绿：16 个有测试包全部 `ok`（含 `internal/tui` 4.931s、`internal/cli` 13.015s、`internal/sshbroker` 6.748s），无 FAIL 无 skip 异常 |

## 4. Byte-review of edited Chinese lines

- 两文件整体：valid UTF-8、无 U+FFFD、无 mojibake 标记（Ã / â€ 等）。
- 新增 5 行（managing-servers.md 4 行 + backlog 第 9 条）逐字符 codepoint 扫描：非 CJK 汉字符符仅为预期集合（`，。；：（）「」·——、` + `●✓↑↓←→`），`ALL CLEAN`。（首跑的 traceback 是 GBK 控制台打印 ✓ 失败，非文件问题；换 UTF-8 stdout 复跑全过。）

## 5. OWNER-GATE SMOKE CHECKLIST（用户真终端执行 — Plan 29 验收第 6 条）

> 前置：worktree 合回 master 后在本机真终端（非测试 harness）起 TUI，服务器页选一台**真实服务器**。逐项打勾，任何一项不过 = gate 不过。

- [ ] **1. 进出无残留**：`e` 打开编辑页 → `Esc` 退出 → 列表页渲染完好无残影/无半截表单；再次 `e` 正常打开。
- [ ] **2. ↑↓ 跨页覆盖每个字段**：从「名称」行开始 `↓` 到末行「✓ 保存并退出」，光标可走过全部 15 个字段 + 保存项（跨页时光标连续，不跳字段）；`↑` 反向同样走全。
- [ ] **3. 页码可见且随翻页变**：页脚 `第 X/Y 页` 始终显示；光标跨页 / `→`pgdn 翻页时 X 递增，`←`pgup 递减，Y 与实际页数一致（16 行 ≈ 3 页）。
- [ ] **4. Enter 编辑 + 脏标记**：选「备注」行 `Enter` 进单字段表单 → 改值 → `Enter` 确认回列表 → 该行 `● 备注` 前缀 + 亮色 +「（已改）」+ 新值；未动字段仍显示原值。
- [ ] **5. field-Esc 撤销单字段**：对同一字段再 `Enter` → 改成另一个值 → `Esc` → 行值回到上一步的值（不是新改的值）。
- [ ] **6. 保存落库（真实往返）**：把这台服务器的 description 改成 `plan29-smoke` → `✓ 保存并退出` `Enter` → 状态行报「已更新 …」；退出 TUI 用 `servers ls`/`servers edit --description` 或 agent 侧确认落库 → **再 `e` 把 description 改回原值保存**，恢复现场。
- [ ] **7. list-Esc 不落库**：改两三个字段（● 亮起）→ 列表态 `Esc` 整体退出 → `servers ls` 确认值未变。
- [ ] **8. 窄终端不破碎**：终端缩到 60 列再 `e`：列表行/页脚被截断成 `…` 而非折行错乱；单字段表单宽度收窄不溢出。
- [ ] **9. 秘密字段只掩码**：「密码 / 密钥口令 / sudo 密码」行预览只见 `已设（新值）`/`（留空=保持现有）` 类状态文案，从进页到保存全程无明文（含改完回到列表的行预览）。

## 6. Concerns

- **嵌入表单路由 bug 仍开放**（backlog #9）：新增 / 向导 / importflow 补全的多字段 huh 表单在真终端仍无法用 Enter/Tab 前进字段——本 plan 只修了编辑页。owner 冒烟时注意别把旧表单的问题误记到编辑页头上。
- 编辑页固定高 20 行（`editPageHeight`）：超 ~6 字段/页 → 3 页。字段表未来变长只会页数变多，不破坏交互。
- 冒烟第 6 项要求改回原值（description 往返），清单已内嵌提醒。
