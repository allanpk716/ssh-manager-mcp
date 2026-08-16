# Task 2 实现报告：owner `ssh` 无命令/空命令显式报错

## 状态: DONE

## Commit
- **Hash**: `b7433db1803127b15a8e6aebf0fed39add4dab6f`
- **Message**: `fix(cli): owner ssh refuses host-only/empty/whitespace command args (was: empty-command Exec)`

## 改动清单
1. **新建文件**: `internal/cli/ssh_test.go`（53 行）
   - 添加 `TestOwnerSSHNoCommandErrors` 测试函数
   - 覆盖 3 类无效输入：host-only、空字符串命令、纯空白命令

2. **修改文件**: `internal/cli/ssh.go`（+7 行）
   - 在 `RunE` 函数开头（`openUnlockedStore()` 之前）插入参数校验
   - 校验逻辑：`len(args) == 1 || strings.TrimSpace(strings.Join(args[1:], " ")) == ""`
   - 错误信息：`"no command given: usage ssh-manager ssh <server> <command...> (single non-interactive command; for an interactive terminal use your own ssh client)"`

## 执行流程（按 brief 的 5 个 Step）

### Step 1: 写失败测试 ✅
- 创建 `internal/cli/ssh_test.go`
- 按 brief 逐字抄写测试代码
- 测试覆盖 3 类无效输入：
  - `{"ssh", "t"}` - host only
  - `{"ssh", "t", ""}` - empty string cmd
  - `{"ssh", "t", "   "}` - whitespace cmd

### Step 2: 跑测试确认失败 ✅
```bash
$ go test ./internal/cli/ -run TestOwnerSSHNoCommandErrors -v
=== RUN   TestOwnerSSHNoCommandErrors
=== RUN   TestOwnerSSHNoCommandErrors/host_only
    ssh_test.go:43: args [ssh t]: error "server \"t\" not found" missing 'no command given'
=== RUN   TestOwnerSSHNoCommandErrors/empty_string_cmd
    ssh_test.go:43: args [ssh t ]: error "server \"t\" not found" missing 'no command given'
=== RUN   TestOwnerSSHNoCommandErrors/whitespace_cmd
    ssh_test.go:43: args [ssh t    ]: error "server \"t\" not found" missing 'no command given'
--- FAIL: TestOwnerSSHNoCommandErrors (0.10s)
    --- FAIL: TestOwnerSSHNoCommandErrors/host_only (0.04s)
    --- FAIL: TestOwnerSSHNoCommandErrors/empty_string_cmd (0.03s)
    --- FAIL: TestOwnerSSHNoCommandErrors/whitespace_cmd (0.03s)
FAIL
```
**结果**: 3 个子测试 FAIL，现状先尝试获取 server（报 "server not found"）而不是先检查命令参数 ✅

### Step 3: 最小实现 ✅
- 在 `internal/cli/ssh.go:21-43` 的 `RunE` 开头插入校验块
- 校验位置：在 `openUnlockedStore()` 之前（确保任何 store/连接动作之前返回错误）
- 校验条件：`len(args) == 1`（host-only）或 `strings.TrimSpace(strings.Join(args[1:], " ")) == ""`（空/空白命令）

### Step 4: 跑测试确认通过 + 既有 smoke 不回归 ✅
```bash
$ go test ./internal/cli/ -run 'TestOwnerSSH' -v
=== RUN   TestOwnerSSHExecRunsCommand
--- PASS: TestOwnerSSHExecRunsCommand (0.07s)
=== RUN   TestOwnerSSHNoCommandErrors
=== RUN   TestOwnerSSHNoCommandErrors/host_only
=== RUN   TestOwnerSSHNoCommandErrors/empty_string_cmd
=== RUN   TestOwnerSSHNoCommandErrors/whitespace_cmd
--- PASS: TestOwnerSSHNoCommandErrors (0.07s)
    --- PASS: TestOwnerSSHNoCommandErrors/host_only (0.02s)
    --- PASS: TestOwnerSSHNoCommandErrors/empty_string_cmd (0.02s)
    --- PASS: TestOwnerSSHNoCommandErrors/whitespace_cmd (0.02s)
PASS
ok  	ssh-manager-mcp/internal/cli	1.024s
```
**结果**: 新测试 3 子测试 PASS + `TestOwnerSSHExecRunsCommand` PASS（无回归）✅

### Step 5: Commit ✅
```bash
$ git add internal/cli/ssh.go internal/cli/ssh_test.go
$ git commit -m "fix(cli): owner ssh refuses host-only/empty/whitespace command args (was: empty-command Exec)"
[worktree-plan-25-ci-gate b7433db] fix(cli): owner ssh refuses host-only/empty/whitespace command args (was: empty-command Exec)
 2 files changed, 53 insertions(+)
 create mode 100644 internal/cli/ssh_test.go
```

## 自查结果
1. ✅ **TDD 流程遵守**：红 → 绿 → 提交，严格按 brief 执行
2. ✅ **测试覆盖**：3 类无效输入全部覆盖，均报 "no command given" 错误
3. ✅ **校验位置正确**：在 `openUnlockedStore()` 之前，不产生审计行
4. ✅ **无回归**：`TestOwnerSSHExecRunsCommand` 仍通过
5. ✅ **代码符合 brief**：错误信息与 brief 完全一致
6. ✅ **提交规范**：commit message 与 brief 一致

## 测试摘要
- **新增测试**: `TestOwnerSSHNoCommandErrors`（3 个子测试）
- **既有测试**: `TestOwnerSSHExecRunsCommand`（无回归）
- **总通过数**: 4/4

## Concerns
无
