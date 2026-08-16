# Plan 25 — CI 测试门禁 + OWNER 路径纠错 + 断连语义四层改写 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地改进计划 rev2 的首批（P0 CI 门禁 → P1 OWNER 纠错 → P2 断连语义改写+回归测试 + P6 兼容矩阵），消除已取证的 7 处代码-文档漂移。

**Architecture:** 纯 CI 配置 + owner CLI 行为修正 + 文案/工具描述对齐 + 语义钉住回归测试，不触碰 agent 侧 broker 凭据面（iron rule 不受影响）。rev2 全文见 `.xcheck/20260816-204442-rev1/proposal.rev2.md`（gitignored），附录 A1–A11 见 `.xcheck/20260816-195610/CLOSE.md`。

**Tech Stack:** Go 1.25（go.mod）、GitHub Actions（reusable workflow）、cobra CLI、既有 testsshd/httptest 测试基建。

## Global Constraints

- Go 版本一律 `go-version-file: go.mod`（不许硬编码）——附录 A2/pi#4。
- 每个任务结束 `gofmt -l .` 必须为空（仓库 .gitattributes 已强制 LF，勿开 autocrlf）。
- 不新增第三方依赖。
- 本批不改 agent 工具行为语义（`forward_port`/`close_port` 逻辑不动，只改描述文案）；`internal/sshbroker`、`internal/store` 不改。
- 提交信息用仓库惯例前缀：`feat:` / `fix:` / `docs:` / `ci:`。
- **范围外**（后续 plan）：P3 欠账分流（junction 语义、clear 双角色、backlog 新增四项——serve 隧道 owner 急停 CLI、Touch 活动刷新、离线 cache 快照失效机制、受控监听地址）、P4 doctor、P5 启发式（高熵不实现）。
- 断连语义的**事实基线**（已三轮取证+实验，写文档时照此，不得回退）：
  1. stdio：生命周期变更下次 `mcp` spawn 生效；在跑会话重启客户端才断。
  2. serve：**逐请求即拒**（revoke 后下一个 HTTP 请求 401，HTTP 实测）。
  3. 既有 forward_port 隧道：revoke **不影响**；close_port 是 MCP 请求会先被 401 挡，stdio 会话/其他 project 的 TunnelManager 是独立进程实例够不到 → **无 owner 急停**，只有重启 broker / 创建后 ~10 分钟回收。
  4. 离线 cache：旧快照不随 revoke 擦除；`cache-tokens revoke` 只断"拉新"；已落盘快照唯一失效手段 = 轮换服务器凭据。
  5. 隧道回收**按创建时间**（Touch 无生产调用方），不是"无活动"。

---

### Task 1: CI 基线 workflow（reusable 双 lane）+ release 内嵌门禁

**Files:**
- Create: `.github/workflows/ci.yml`
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: 无（首个任务）。
- Produces: `.github/workflows/ci.yml` 的 `test` job（`workflow_call` 可复用）——Task 1 的 release.yml 改动 `needs` 它。

- [ ] **Step 1: 写 ci.yml**

```yaml
# Test baseline — every push/PR, both lanes. Also the reusable gate the
# release pipeline calls (release.yml `needs` this workflow's test job), so a
# v* tag is published only after the SAME matrix passes on the tag commit.
# Windows lane is load-bearing: 4 `//go:build windows` test files
# (store/acl, store/dpapi, store/masterkey, tui/istty) never run on ubuntu.
name: ci

on:
  push:
  pull_request:
  workflow_call:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  test:
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Build
        run: go build ./...

      - name: Vet
        run: go vet ./...

      - name: Gofmt (fail on output)
        shell: bash
        run: |
          out="$(gofmt -l .)"
          if [ -n "$out" ]; then
            echo "gofmt needed on:"; echo "$out"; exit 1
          fi

      - name: Test
        run: go test ./... -count=1
```

- [ ] **Step 2: 改 release.yml —— test 门禁内嵌 + go-version-file 统一**

整文件替换 jobs 段（头部 name/on/permissions/concurrency 不动）：

```yaml
jobs:
  # Gate: the SAME dual-lane test matrix ci.yml runs on every push/PR must
  # pass on the tag commit BEFORE anything is published. go-version-file
  # keeps the release toolchain identical to the test toolchain.
  test:
    uses: ./.github/workflows/ci.yml

  release:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # full history — changelog needs it

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          # Pinned GoReleaser CLI version (supply-chain hygiene; v2.17.1 is
          # latest stable 2026-08 and clears CVE AIKIDO-2026-10332). NOT 'latest'.
          version: v2.17.1
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 3: 本地校验 YAML 语法**

Run: `python -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); yaml.safe_load(open('.github/workflows/release.yml')); print('yaml ok')"`
Expected: `yaml ok`（若本机无 python/yaml，用 `go run` 不引入依赖的替代：肉眼对照缩进 + `git diff` 检查）。

- [ ] **Step 4: 本地跑一遍将被 CI 跑的命令（确保 CI 首跑可绿）**

Run: `go build ./... && go vet ./... && test -z "$(gofmt -l .)" && echo fmt-clean && go test ./... -count=1`
Expected: 全部退出 0、`fmt-clean` 打印。

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yml .github/workflows/release.yml
git commit -m "ci: dual-lane test baseline (ubuntu+windows) + release gate reuses it via workflow_call"
```

- [ ] **Step 6（owner gate，代码外）:** 推分支后在 GitHub Actions 观察 `ci` 首跑双 lane 绿；合并后在 repo Settings → Branches 给 master 开 branch protection（require `test` workflow 绿）。此步由 owner 手工做，不进本任务提交。

---

### Task 2: owner `ssh` 无命令/空命令显式报错

**Files:**
- Modify: `internal/cli/ssh.go:21-43`
- Test: `internal/cli/ssh_test.go`（新建）

**Interfaces:**
- Consumes: 既有 `withEnv` helper（cli 包测试内已存在，见 ssh_smoke_test.go:29）。
- Produces: `ssh` 子命令对 host-only / 空串 / 纯空白三类输入返回错误 `"no command given..."`——Task 3 在同一文件继续改。

- [ ] **Step 1: 写失败测试**

```go
package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestOwnerSSHNoCommandErrors pins the arg contract: the owner ssh path is a
// SINGLE non-interactive command — host-only, empty-string, and whitespace-only
// command args all fail fast BEFORE any connection or audit row.
func TestOwnerSSHNoCommandErrors(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	cases := []struct {
		name string
		args []string
	}{
		{"host only", []string{"ssh", "t"}},
		{"empty string cmd", []string{"ssh", "t", ""}},
		{"whitespace cmd", []string{"ssh", "t", "   "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := NewRootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(c.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("args %v: expected error, got nil", c.args)
			}
			if !strings.Contains(err.Error(), "no command given") {
				t.Fatalf("args %v: error %q missing 'no command given'", c.args, err)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestOwnerSSHNoCommandErrors -v`
Expected: 3 个子测试 FAIL（现状：host-only 走空命令 Exec 会先报 connect 错或挂起——错误信息不含 "no command given"）。

- [ ] **Step 3: 最小实现**

`internal/cli/ssh.go` 的 `RunE` 开头（`openUnlockedStore()` 之前）插入：

```go
			// The owner ssh path is a SINGLE non-interactive command (no PTY).
			// Empty/whitespace joins would otherwise run an empty command on
			// the remote host — fail fast instead (Plan 25 / xcheck A4).
			if len(args) == 1 || strings.TrimSpace(strings.Join(args[1:], " ")) == "" {
				return fmt.Errorf("no command given: usage ssh-manager ssh <server> <command...> (single non-interactive command; for an interactive terminal use your own ssh client)")
			}
```

（`strings` 已在 import 里——ssh.go:6 已有。）

- [ ] **Step 4: 跑测试确认通过 + 既有 smoke 不回归**

Run: `go test ./internal/cli/ -run 'TestOwnerSSH' -v`
Expected: 新测试 3 子测试 PASS + `TestOwnerSSHExecRunsCommand` PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/ssh.go internal/cli/ssh_test.go
git commit -m "fix(cli): owner ssh refuses host-only/empty/whitespace command args (was: empty-command Exec)"
```

---

### Task 3: owner `ssh` 共享 120s deadline + 退出码传播

**Files:**
- Modify: `internal/cli/ssh.go`（deadline seam + ctx 贯通 + 退出码）
- Test: `internal/cli/ssh_test.go`（追加两个测试）

**Interfaces:**
- Consumes: Task 2 的报错前置；`sshbroker.Connect(ctx, ...)` 既有 ctx-abandon 语义（client.go:18-26 注释：dial 不可中断，取消即返回 ctx.Err() 并由后台 goroutine 关闭 eventual conn）。
- Produces: 包级 `var ownerSSHDeadline = 120 * time.Second`（测试可覆写）；RunE 全程单一 ctx deadline（connect+exec 共享）。

- [ ] **Step 1: 写失败测试（追加到 ssh_test.go）**

```go
// TestOwnerSSHPropagatesRemoteExitCode pins A4: a remote command exiting
// non-zero must surface as a cobra error (CLI exits non-zero) — output is
// still printed first. Today the exit code is swallowed (return nil).
func TestOwnerSSHPropagatesRemoteExitCode(t *testing.T) {
	addr, hostKey, srvCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			return "partial output\n", "boom\n", 3
		},
	})
	defer srvCleanup()
	host := addr[:bytesIndex(addr, ':')]

	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	st, err := store.Open(filepath.Join(dir, "test.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, _ := st.AddServer(&models.Server{
		Name: "t", Host: host, Port: portOfAddr(addr), User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	_ = st.SaveHostKey(host, portOfAddr(addr), hostKey.Marshal())
	st.Close()
	_ = srvID

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"ssh", "t", "false"})
	err = root.Execute()
	if err == nil {
		t.Fatal("expected error for remote exit code 3, got nil")
	}
	if !strings.Contains(err.Error(), "exited with code 3") {
		t.Fatalf("error %q missing remote exit code", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("partial output")) {
		t.Fatal("remote stdout must still be printed before the error")
	}
}

// TestOwnerSSHConnectDeadlineBounded pins A7: connect shares the command
// deadline — an unreachable host returns within the (shortened) deadline,
// not the OS TCP timeout. ssh.Dial cannot be interrupted; Connect abandons
// the in-flight dial on ctx expiry (client.go contract), so elapsed ≈ deadline.
func TestOwnerSSHConnectDeadlineBounded(t *testing.T) {
	orig := ownerSSHDeadline
	ownerSSHDeadline = 2 * time.Second
	t.Cleanup(func() { ownerSSHDeadline = orig })

	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	st, err := store.Open(filepath.Join(dir, "test.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	_, _ = st.AddServer(&models.Server{
		// RFC5737 TEST-NET-3 — non-routable by definition, safe in any CI lane.
		Name: "t", Host: "203.0.113.1", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	st.Close()

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"ssh", "t", "true"})
	start := time.Now()
	err = root.Execute()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected connect error for unreachable host, got nil")
	}
	// Deadline is 2s; allow generous slack for CI jitter but far below the
	// multi-minute OS TCP timeout this test exists to forbid.
	if elapsed > 15*time.Second {
		t.Fatalf("unreachable-host connect took %v; deadline not shared/bounded", elapsed)
	}
	t.Logf("elapsed=%v err=%v", elapsed, err)
}
```

（文件头 import 追加：`"io"`、`"time"`、`"ssh-manager-mcp/internal/models"`、`"ssh-manager-mcp/internal/testsshd"`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run 'TestOwnerSSHPropagatesRemoteExitCode|TestOwnerSSHConnectDeadlineBounded' -v`
Expected: 第一个 FAIL（err=nil，退出码被吞）；第二个 FAIL 或超长（连接段无 deadline——203.0.113.1 在多数环境挂到 OS 超时，测试本身会拖很久；若 CI 环境立即 RST 则可能"碰巧"过——以代码 diff 为准，两个都要改）。

- [ ] **Step 3: 实现（ssh.go）**

(a) `sshbroker.Connect` 上方（`start := time.Now()` 行之前）加包级 seam 与共享 ctx：

```go
// ownerSSHDeadline bounds the WHOLE owner ssh invocation — connect AND exec
// share it (was: connect used context.Background() and could hang for the OS
// TCP timeout on an unreachable host). ssh.Dial cannot be interrupted;
// sshbroker.Connect abandons the dial at ctx expiry (client.go contract).
var ownerSSHDeadline = 120 * time.Second
```

(b) RunE 内（Task 2 的报错块之后）：

```go
			ctx, cancel := context.WithTimeout(context.Background(), ownerSSHDeadline)
			defer cancel()
```

(c) `sshbroker.Connect(context.Background(), ...)` → `sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)`。

(d) `cli.Exec(context.Background(), commandStr, 120*time.Second, 0)` → `cli.Exec(ctx, commandStr, ownerSSHDeadline, 0) // owner path: unlimited output; shared deadline caps total time`。

(e) 退出码传播——替换结尾：

```go
			out := cmd.OutOrStdout()
			fmt.Fprint(out, res.Stdout)
			fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
			if res.ExitCode != 0 {
				cmd.SilenceErrors = true
				cmd.SilenceUsage = true
				// Output above is already printed; surface the remote exit
				// code as the CLI's own non-zero exit (A4 — was swallowed).
				return fmt.Errorf("remote command exited with code %d", res.ExitCode)
			}
			return nil
```

- [ ] **Step 4: 跑全部 owner ssh 测试确认通过**

Run: `go test ./internal/cli/ -run 'TestOwnerSSH' -v`
Expected: 4 个测试（smoke + 3 新）全 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/ssh.go internal/cli/ssh_test.go
git commit -m "fix(cli): owner ssh shares one 120s connect+exec deadline and propagates remote exit code"
```

---

### Task 4: OWNER 文档纠错（README + quickstart + scenarios）

**Files:**
- Modify: `README.md:94-98`
- Modify: `docs/quickstart-single-machine.md:86-93`
- Modify: `docs/scenarios.md:178-185`

**Interfaces:**
- Consumes: Task 2/3 的行为（无命令报错；共享 120s；退出码传播）。
- Produces: 无代码接口；`docs/getting-started.md` **确认无** owner-ssh 交互段（终审核实），**不改它**。

- [ ] **Step 1: README.md owner 段替换**

原文（94-98 行）：

```markdown
**Owner access** (you, not the agent) — full access to every server using the stored creds directly:
```bash
ssh-manager ssh gpu nvidia-smi          # run a command
ssh-manager ssh gpu                     # (your own ssh client; the broker provides creds)
```
```

替换为：

```markdown
**Owner access** (you, not the agent) — full access to every server using the stored creds directly:
```bash
ssh-manager ssh gpu nvidia-smi          # run ONE command (single, non-interactive)
```
The owner path runs a **single non-interactive command** (connect + exec share one 120-second deadline; output is uncapped; the remote exit code becomes the CLI's exit code). No command → explicit error. Interactive shells are intentionally not provided — for a terminal, use your own SSH client with credentials you already hold or provision separately (they may live only in this vault).
```

- [ ] **Step 2: quickstart-single-machine.md owner 段替换**

原文（88-93 行内代码块）：

```markdown
agent 之外，你本人可以用存储的凭据直接操作任何服务器：

```bash
ssh-manager ssh gpu nvidia-smi         # 直接跑一条命令
ssh-manager ssh gpu                     # 进交互（broker 提供 creds，用你自己的 ssh）
```
```

替换为：

```markdown
agent 之外，你本人可以用存储的凭据直接在服务器上跑**单条命令**（非交互；连接+执行共享 120 秒超时，输出不封顶，远端退出码会传成本地退出码）：

```bash
ssh-manager ssh gpu nvidia-smi         # 直接跑一条命令（不带命令会显式报错）
```

> 这条路**不是交互式终端**。要开终端，用你自己的 ssh 客户端（凭据需自行已有或另行配置——它们可能只存在本 vault 里）。
```

- [ ] **Step 3: scenarios.md owner 段两处修正**

(a) 示例块（178-180 行）删掉第二行误导示例，改为：

```markdown
```bash
ssh-manager ssh gpu nvidia-smi          # 在 gpu 上跑一条命令，输出原样回来
```
```

(b) 184 行要点，原文：

```markdown
- 这条命令**也不是交互式 shell**：后面的 `<command...>` 是要跑的命令（空格分隔会被拼成一行）。它解决的是“owner 用 broker 里存的凭据直接跑一条命令”，不是给你开个 `ssh -t` 终端。要交互式终端，用你自己的 ssh 客户端（凭据你本来就有）。
```

替换为：

```markdown
- 这条命令**也不是交互式 shell**：后面的 `<command...>` 是要跑的命令（空格分隔会被拼成一行；**不带命令 / 空命令会显式报错**）。它解决的是“owner 用 broker 里存的凭据直接跑一条命令”，不是给你开个 `ssh -t` 终端。要交互式终端，用你自己的 ssh 客户端（凭据需自行已有或另行配置——它们可能只存在本 vault 里）。
- 连接+执行**共享 120 秒超时**；输出不封顶；**远端退出码会传播为本地退出码**（脚本里可用 `$?` 判断）。
```

- [ ] **Step 4: 验证无残留错误承诺**

Run: `grep -rn "your own ssh client; the broker provides creds\|进交互" README.md docs/`
Expected: 无输出（0 匹配）。

Run: `grep -n "凭据你本来就有" docs/`
Expected: 无输出。

- [ ] **Step 5: Commit**

```bash
git add README.md docs/quickstart-single-machine.md docs/scenarios.md
git commit -m "docs: owner ssh is single-command only — drop interactive promise, state deadline/exit-code semantics"
```

---

### Task 5: 隧道语义文案对齐（工具描述 + 注释 + scenarios「无活动」）

**Files:**
- Modify: `internal/mcpserver/server.go:133`（forward_port Description）
- Modify: `internal/mcpserver/server.go:151`（close_port Description）
- Modify: `internal/mcpserver/types.go:75-78`（ForwardOutput 注释）
- Modify: `internal/mcpserver/tunnels.go:10-16`（forwardIdleTimeout 注释）
- Modify: `internal/eval/README.md:421-426`
- Modify: `docs/scenarios.md:96`
- Modify: `README.md:43`（工具表 forward_port 行的 idle 措辞——若含）

**Interfaces:**
- Consumes: 事实基线 #3/#5（创建后 ~10 分钟、serve 模式地址仅在 broker 机可达）。
- Produces: agent 可见的两段工具描述（正文变更，逻辑不动）。

- [ ] **Step 1: server.go:133 forward_port Description**

原句尾两段：

```
Returns tunnel_id + local_port: reach the remote service at 127.0.0.1:<local_port> on YOUR machine (e.g. `curl http://127.0.0.1:<local_port>` or pointing your client at it). Out-of-profile server ids are rejected. This holds an SSH connection open in the broker for the tunnel's life — call close_port with tunnel_id when done (tunnels also auto-close after ~10 min idle).
```

替换为：

```
Returns tunnel_id + local_port: the forward listens on 127.0.0.1:<local_port> ON THE MACHINE THE BROKER RUNS ON — with a stdio MCP that is your machine; with a remote serve broker it is the serve host, so reach it from there (e.g. curl on that host) — it is NOT reachable from a different machine. Out-of-profile server ids are rejected. This holds an SSH connection open in the broker for the tunnel's life — call close_port with tunnel_id when done (tunnels auto-close ~10 minutes after creation, not based on activity).
```

- [ ] **Step 2: server.go:151 close_port Description**

原句尾：

```
You SHOULD call this when you are done with a forward rather than waiting for the ~10 min idle timeout.
```

替换为：

```
You SHOULD call this when you are done with a forward rather than waiting for the ~10-minutes-after-creation auto-close.
```

- [ ] **Step 3: 其余四处注释/文档**

(a) `types.go:78` 原文 `close_port when done (tunnels also auto-close after ~10 min idle).` → `close_port when done (tunnels auto-close ~10 min after creation — creation-based, not activity-based).`

(b) `tunnels.go:10-13` 注释原文：

```go
// forwardIdleTimeout is how long a tunnel may live with no re-asserted activity
// before the idle sweeper reaps it. The MVP activity signal is the open time
// (lastActivity = time.Now() in Open); Touch(id) refreshes it for callers that
// want to keep a long-lived tunnel alive. Default 10 min per Plan 6 §T4.
```

替换为：

```go
// forwardIdleTimeout is how long a tunnel lives before the sweeper reaps it.
// NOTE: the signal is CREATION time (lastActivity = time.Now() in Open) —
// Touch(id) exists to refresh it but has NO production caller today, so a
// tunnel dies ~10 min after creation even under continuous traffic. Making
// this activity-aware (wiring Touch) is a tracked backlog item. Default 10
// min per Plan 6 §T4.
```

(c) `internal/eval/README.md:423-424` 原文：

```markdown
- A background **idle-sweeper** auto-closes tunnels idle > ~10 min
  (`forwardIdleTimeout`) — defense-in-depth so a forgetful agent can't leak
  tunnels indefinitely.
```

替换为：

```markdown
- A background sweeper auto-closes tunnels **~10 min after creation**
  (`forwardIdleTimeout`; creation-based — `Touch` exists but has no
  production caller) — defense-in-depth so a forgetful agent can't leak
  tunnels indefinitely.
```

(d) `docs/scenarios.md:96` 原文：

```markdown
- 隧道会占住一条 SSH 连接，**约 10 分钟无活动会被后台自动回收**；你或 agent 也可以随时 `close_port` 主动关。
```

替换为：

```markdown
- 隧道会占住一条 SSH 连接，**创建约 10 分钟后被后台自动回收**（按创建时间计，**不看活动量**——持续有流量也会回收）；你或 agent 也可以随时 `close_port` 主动关。
```

- [ ] **Step 4: 验证全网无 "idle" 残留误导**

Run: `grep -rn "10 min idle\|分钟无活动\|idle > ~10\|~10 min idle\|idle timeout" internal/ docs/ README.md --include="*.go" --include="*.md"`
Expected: 无输出（若 `--include` 语法在本机 grep 报错，改用 `git grep -n -e "10 min idle" -e "分钟无活动" -e "~10 min idle"`，预期无匹配）。

Run: `go build ./... && go test ./internal/mcpserver/ -count=1`
Expected: 编译过、测试全绿（描述是字面量，无行为变更；既有测试若断言旧描述文本需同步更新——搜索 `grep -rn "10 min idle" internal/ --include="*_test.go"` 应为 0）。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/server.go internal/mcpserver/types.go internal/mcpserver/tunnels.go internal/eval/README.md docs/scenarios.md
git commit -m "docs+schema: tunnel auto-close is creation-based (~10 min after open), and forward_port address is broker-host-local in serve mode"
```

---

### Task 6: 断连语义四层改写（agent-access / scenarios / README / multi-machine）

**Files:**
- Modify: `docs/agent-access.md:104-109, 172-175, 197`
- Modify: `docs/scenarios.md:155-169, 208`
- Modify: `README.md:127`
- Modify: `docs/multi-machine.md:570`

**Interfaces:**
- Consumes: 事实基线 #1–#4（Global Constraints）；Task 5 的"创建后 ~10 分钟"措辞。
- Produces: 单一权威小节「断连语义（四层）」在 agent-access.md；其余文档指向它。

- [ ] **Step 1: agent-access.md 104-109 段整体替换**

原文：

```markdown
⚠️ **关键机制——Lazy 生效：** `rotate` / `disable` / `enable` / `revoke` **不是立刻断正在运行的 agent**，而是在 agent **下一次启动 `mcp` 子进程时**生效（token 校验只放行 `status=active` 的 project）。

为什么这样设计？**你的机器你做主**：你重启 Claude Code / 它重启 MCP 子进程时，新策略才接管；当前正在跑的会话保留它的访问直到那一步。这意味着：
- 想立刻掐断某个 agent → 除了 `revoke`/`disable`，还要让客户端重连（重启 Claude Code，或它的 MCP 子进程）。
- `rotate` 保持 project id 和 profile 不变，**只换 token**。
```

替换为：

```markdown
⚠️ **关键机制——断连语义按部署模式分四层**（`rotate` / `disable` / `enable` / `revoke` 的生效范围）：

1. **stdio（本机 MCP 子进程）——Lazy 生效**：token 校验只在 `mcp` 子进程**下次启动**时跑（只放行 `status=active`）。正在跑的会话保留访问直到你重启 Claude Code（或它的 MCP 子进程）。**你的机器你做主**：这是有意的设计。
2. **serve（远程 broker）——逐请求即拒**：broker 对**每一个** HTTP 请求都重新验 token，`revoke`/`disable` 后该 project 的**下一个请求立即 401**——不需要等任何重启。
3. **已建立的 `forward_port` 隧道——不受 revoke 影响，且无 owner 急停**：隧道由 broker 进程持有；被吊销的 project 自己调 `close_port` 会先被第 2 层的 401 挡住；任何 stdio 会话或其他 project 的隧道管理器是**独立进程实例**，够不到它。真实选项只有：**重启 broker**（`serve uninstall`→`install` 或重启机器）/ **等隧道创建后 ~10 分钟自动回收**。（owner 侧急停命令已列 backlog。）
4. **离线 cache——旧快照不随 revoke 擦除**：`cache-tokens revoke` 只断"拉新"（下次 `cache pull` 被拒）；已落盘的 `cache.bin` 里凭据仍在。**失窃/泄露场景下让已缓存凭据失效的唯一手段是轮换服务器凭据**（`servers edit <name> --password/--key`）。

`rotate` 保持 project id 和 profile 不变，**只换 token**（serve 模式下旧 token 同样逐请求即拒）。
```

- [ ] **Step 2: agent-access.md 175 行表格行替换**

原行：

```markdown
| 要立刻断正在跑的会话 | revoke/disable 后，**重启那个客户端**（让它重连 MCP）。 |
```

替换为：

```markdown
| 要立刻断正在跑的会话 | 看模式：serve 远程 agent 下一个请求即拒（无需动作）；stdio 本机会话须重启客户端；既有隧道见「断连语义（四层）」第 3 层（只能重启 broker 或等回收）。 |
```

- [ ] **Step 3: agent-access.md 197 行表格行替换**

原行：

```markdown
| 暂停了 agent 还在跑 | Lazy 机制：disable/revoke 在**下次重连**才接管。重启那个客户端。 |
```

替换为：

```markdown
| 暂停了 agent 还在跑 | stdio：Lazy，下次重连才接管，重启那个客户端；serve：下一请求即拒，无需动作。详见「断连语义（四层）」。 |
```

- [ ] **Step 4: scenarios.md 三处**

(a) 158 行注释行 `# 想立刻断正在跑的会话：重启那个客户端，让它重连 MCP。` → `# serve 模式下一请求即拒；stdio 会话重启客户端；隧道见 agent-access「断连语义（四层）」。`

(b) 169 行引用块原文：

```markdown
> Lazy 机制：disable / enable / revoke / rotate 都在 agent **下次重连 MCP** 时接管，详见 [agent-access.md](./agent-access.md) 的"Project 生命周期"一节。
```

替换为：

```markdown
> 断连语义分四层（stdio=下次重连；serve=逐请求即拒；既有隧道不受 revoke 影响且只能重启 broker/等创建后 ~10 分钟回收；离线 cache 须轮换凭据），详见 [agent-access.md](./agent-access.md) 的「断连语义（四层）」一节。
```

(c) 208 行 `- **出事了** → rotate（换卡）/ disable（暂停）/ revoke（吊销），重启客户端让它立刻接管。` → `- **出事了** → rotate（换卡）/ disable（暂停）/ revoke（吊销）——serve 模式下一请求即拒；stdio 会话重启客户端接管；离线缓存场景须轮换服务器凭据（见 agent-access「断连语义（四层）」）。`

- [ ] **Step 5: README.md:127 段替换**

原文段首句到破折号前：

```markdown
**Lifecycle is Lazy:** `rotate` / `disable` / `enable` / `revoke` take effect at the agent's **next `mcp` spawn** (`VerifyToken` admits only `active` projects). A currently-running agent session keeps its access until Claude Code restarts its MCP child — by design (your box, your call).
```

替换为：

```markdown
**Lifecycle:** `rotate` / `disable` / `enable` / `revoke` take effect **per request on a remote serve broker** (the next request is 401-rejected immediately), and **at the agent's next `mcp` spawn in stdio mode** (`VerifyToken` admits only `active` projects — a currently-running local session keeps access until Claude Code restarts its MCP child, by design). Already-open `forward_port` tunnels survive revocation (no owner emergency-stop; broker restart or the ~10-minutes-after-creation reclaim). Offline caches keep working from their last snapshot — rotate server credentials to invalidate them. Full breakdown: `docs/agent-access.md` 「断连语义（四层）」.
```

- [ ] **Step 6: multi-machine.md:570 行替换**

原行：

```markdown
- [agent-access.md](./agent-access.md)——project token 生命周期（`rotate` / `disable` / `revoke` 的 Lazy 语义）；**serve 模式完全适用**，token 管理在同一台服务器上做。
```

替换为：

```markdown
- [agent-access.md](./agent-access.md)——project token 生命周期；**断连语义分四层**：serve 模式下吊销**逐请求即拒**（远程 agent 无需重启）；stdio/隧道/离线缓存各有不同（见「断连语义（四层）」一节）。token 管理在同一台服务器上做。
```

- [ ] **Step 7: 验证无残留错误表述**

Run: `git grep -n "serve 模式完全适用"`
Expected: 无匹配。
Run: `git grep -n "下次重连才接管\|下次启动.*mcp.*子进程时.*生效" docs/ | grep -v stdio`
Expected: 无匹配（Lazy 表述必须已限定 stdio 或指向四层小节）。

- [ ] **Step 8: Commit**

```bash
git add docs/agent-access.md docs/scenarios.md README.md docs/multi-machine.md
git commit -m "docs: disconnect semantics as four layers (stdio lazy / serve per-request / tunnels survive revoke / offline cache needs credential rotation)"
```

---

### Task 7: revoke 语义回归测试（productize 实验装置）

**Files:**
- Create: `internal/mcpserver/revoke_semantics_test.go`
- Test: 同文件（tests-only 任务，钉住 Task 6 文档所述事实）

**Interfaces:**
- Consumes: 包内既有 helper——`newStore(t)`（core_test.go:23）、`seedRealServer(t, st, name, addr, hk, sudoPw)`（core_test.go:248）、`startEchoListener(t)`（core_test.go:687）、`NewServer(st, profileID, projectID) (*mcp.Server, *TunnelManager, error)`、`ForwardForProfile(ctx, st, projectID, profileID, serverID, remoteHost string, remotePort, localPort int, mgr)`（core.go:427）、`NewServeRunner(st)` / `r.HTTPHandler()`（serve.go）、`st.AddProfile/AddProject/GrantServers/SetProjectStatus/VerifyToken`。
- Produces: 两个钉语义的测试名（后续 CI 永久回归）：`TestRevokedProjectKeepsOpenTunnelForwarding`、`TestServeHTTPRejectsRevokedTokenPerRequest`。

- [ ] **Step 1: 写测试**

```go
package mcpserver

// Disconnect-semantics regression pins (Plan 25). These four facts are
// documented in docs/agent-access.md 「断连语义（四层）」 and were verified
// empirically (xcheck 2026-08-16): (1) VerifyToken rejects a revoked token
// immediately (the serve per-request gate); (2) an ALREADY-OPEN forward tunnel
// keeps forwarding after revocation — the tunnel is held by the broker's
// TunnelManager and nothing tears it down on revoke; (3) via the serve HTTP
// handler, a revoked project's close_port (and any other request) is rejected
// with 401 BEFORE reaching the tool layer.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

// TestRevokedProjectKeepsOpenTunnelForwarding pins layers 1+3: revocation
// kills the token gate immediately, but the broker-held tunnel keeps
// forwarding (no cascade teardown — owner decision, kill CLI is backlog).
func TestRevokedProjectKeepsOpenTunnelForwarding(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	projID, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}

	echoPort := startEchoListener(t)

	_, mgr, err := NewServer(st, pid, projID)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()
	out, err := ForwardForProfile(context.Background(), st, projID, pid, srvID, "127.0.0.1", echoPort, 0, mgr)
	if err != nil {
		t.Fatal(err)
	}

	probe := func(label string) {
		t.Helper()
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", out.LocalPort), 3*time.Second)
		if err != nil {
			t.Fatalf("%s: dial: %v", label, err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = c.Write([]byte("ping-" + label + "\n"))
		buf := make([]byte, 128)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("%s: read through tunnel: %v", label, err)
		}
		t.Logf("%s: tunnel forwarded %q", label, string(buf[:n]))
	}

	probe("before-revoke")
	if p, _ := st.VerifyToken(token); p == nil {
		t.Fatal("sanity: token must verify before revoke")
	}

	if err := st.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}

	// Layer 1: the token gate rejects immediately (this is what serve's
	// per-request verifyToken consults).
	if p, _ := st.VerifyToken(token); p != nil {
		t.Fatal("VerifyToken must reject a revoked token immediately")
	}
	// Layer 3: the already-open tunnel KEEPS forwarding — pin it.
	probe("after-revoke")
}

// TestServeHTTPRejectsRevokedTokenPerRequest pins layer 2 end-to-end at the
// HTTP middleware: post-revoke close_port (and initialize) both 401 — the
// request never reaches the tool layer, so a revoked project cannot even
// close its own tunnel via close_port.
func TestServeHTTPRejectsRevokedTokenPerRequest(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p")
	projID, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}

	r := NewServeRunner(st)
	defer r.Close()
	h := r.HTTPHandler()

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	closeBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"close_port","arguments":{"tunnel_id":"irrelevant"}}}`

	post := func(body, tok string) int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := post(initBody, token); code != http.StatusOK {
		t.Fatalf("sanity: pre-revoke initialize = %d, want 200", code)
	}
	if err := st.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	if code := post(closeBody, token); code != http.StatusUnauthorized {
		t.Fatalf("post-revoke close_port = %d, want 401 (rejected before tool layer)", code)
	}
	if code := post(initBody, token); code != http.StatusUnauthorized {
		t.Fatalf("post-revoke initialize = %d, want 401", code)
	}
}
```

- [ ] **Step 2: 跑测试确认通过（行为已存在，本测试是钉住）**

Run: `go test ./internal/mcpserver/ -run 'TestRevokedProjectKeepsOpenTunnelForwarding|TestServeHTTPRejectsRevokedTokenPerRequest' -v`
Expected: 两个 PASS（若 FAIL——说明文档语义与代码不符，**停下报告**，不许改测试凑绿）。

- [ ] **Step 3: 全包回归**

Run: `go test ./internal/mcpserver/ -count=1`
Expected: 全绿。

- [ ] **Step 4: Commit**

```bash
git add internal/mcpserver/revoke_semantics_test.go
git commit -m "test: pin disconnect semantics — token gate rejects immediately, open tunnels survive revocation, serve HTTP 401s pre-tool"
```

---

### Task 8: client↔serve 版本兼容矩阵

**Files:**
- Create: `docs/compat-matrix.md`
- Modify: `docs/README.md`（目录表加一行）
- Modify: `docs/multi-machine.md:566-575`（相关文档列表加一行）

**Interfaces:**
- Consumes: git tag 历史 + Release notes（数据源）。
- Produces: 兼容矩阵文档，含「已验证组合 / 已知破坏性变更 / 升级顺序铁律」三节。

- [ ] **Step 1: 取证版本事实**

Run: `git tag -l | sort -V && git log --oneline v0.3.1..v0.4.0 -- internal/mcpserver/serve.go internal/cli/cache.go | head -20`
Expected: 拿到全部 tag 列表；确认 TLS-only serve 落地版本（auto-TLS 计划 2026-08-13 合并，预计 v0.4.0——以 git log 实证为准，写入矩阵时标注证据 commit）。

- [ ] **Step 2: 写 docs/compat-matrix.md**

```markdown
# client ↔ serve 版本兼容矩阵

> 维护规则：每次发版后追加一行「已验证组合」。破坏性变更必须在此登记 + 给迁移顺序。

## 已验证组合

| client 版本 | serve 版本 | 在线（HTTP MCP） | 离线（cache pull / mcp --cache） | 验证日期 |
|---|---|---|---|---|
| v0.7.3 | v0.7.3 | ✅（NUC10 权威 broker + 笔记本） | ✅（9/9 服务器） | 2026-08-16 |

（v0.7.3 为当前生产双端；历史组合未逐一回归，旧版本请先看下方破坏性变更。）

## 已知破坏性变更

| 起始版本 | 变更 | 影响 | 迁移 |
|---|---|---|---|
| v0.4.0（以 git log 实证为准） | serve 默认 TLS-only + 自签证书 + SPKI pin；无 pin 客户端默认拒连 | 旧明文 client 无法拉快照/连 MCP | 先升全部工作机 binary + 配 pin，**最后**重启 serve（README「migration order」） |
| v0.7.0 | `tui --mode broker` 移除（自动判定覆盖） | 脚本里写死 `--mode broker` 的调用报错 | 改 `ssh-manager tui` |

## 升级顺序铁律

**先升所有 client（工作机 binary + cache pin），最后重启 serve。** serve 一旦升级到 TLS-only 版本即刻拒绝旧明文 client——顺序反了会把整条缓存链打断。token / snapshot 格式 / tool schema 目前无跨版本不兼容记录；出现时在此登记。

## 相关文档

- [multi-machine.md](./multi-machine.md) —— serve 部署与 TLS 迁移 runbook
- [agent-access.md](./agent-access.md) —— token 生命周期与断连语义（四层）
```

（Step 1 的实证结果如与表中 v0.4.0 不符，以实证改表并注明 commit。）

- [ ] **Step 3: 挂链接**

(a) `docs/README.md` 目录表（「文档 | 解决什么」表格）末尾加一行：

```markdown
| [compat-matrix.md](./compat-matrix.md) | **client↔serve 版本兼容矩阵**：已验证组合 / 破坏性变更 / 升级顺序铁律。升级任何一端之前先看这篇。 |
```

(b) `docs/multi-machine.md:570` 所在「相关文档」列表（Task 6 改过该行）之后加：

```markdown
- [compat-matrix.md](./compat-matrix.md)——client↔serve 版本兼容矩阵（升级任何一端之前先看）。
```

- [ ] **Step 4: Commit**

```bash
git add docs/compat-matrix.md docs/README.md docs/multi-machine.md
git commit -m "docs: client-serve compatibility matrix (verified pairs / breaking changes / upgrade-order rule)"
```

---

## Self-Review 记录

- **Spec 覆盖**：rev2 P0→Task 1；P1（.1 文档→Task 4，.2 行为→Task 2+3，.3 演练→owner gate 不进代码）；P2（四层改写→Task 6，idle 对齐→Task 5，回归测试→Task 7）；P6 兼容矩阵→Task 8。附录映射：A1→T5、A2→T1、A3→T5/T6（文档面）+backlog（范围外）、A4→T2/T3、A5→T4、A6→T6、A7→T3（deadline 测试）、A10→T6（第 4 层措辞）、A11→T3（连接失败 status="error" 维持现状，注释不额外改）。A8/A9 属 P5/P3 范围外。
- **占位符扫描**：无 TBD/TODO；所有代码步骤含完整代码。
- **类型一致性**：`ownerSSHDeadline`（T3 定义/测试覆写）、`ForwardForProfile`/`NewServer`/`NewServeRunner.HTTPHandler` 签名与 core.go:427/server.go:43/serve.go 一致；测试 helper 名与 core_test.go/serve_test.go 一致（newStore/seedRealServer/startEchoListener）。
- **已知风险**：Task 1 的 CI 首跑只能在 GitHub 上验证（本地无 actions runner）——已列为 owner gate；Task 7 若 FAIL 属"文档语义与代码不符"，流程要求停下报告而非改测试。
