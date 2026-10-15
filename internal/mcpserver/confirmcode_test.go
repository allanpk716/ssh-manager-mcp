package mcpserver

// 命令策略·需确认与一次性确认码 (ticket #11 / ADR 0007) 的行为测试:
//   - 需确认命令不带码调用:一行说明 (规则编号 + 为什么 + 该怎么做) +
//     一枚确认码,不建立 SSH 连接,审计落「需确认退回」行;
//   - 带正确确认码 + 理由重调:命令照常执行,审计落「带确认码执行」行
//     (含 agent 给的确认理由);
//   - 确认码换命令 (哪怕一个字符)、换服务器、超过 5 分钟、第二次使用:
//     全部失效并重新退回 (附新码);
//   - 只带码不带理由:退回并说明差什么,已签发的码不作废,补理由即可重调。
//
// 测试切入点沿用 policygate_test.go (ticket #10) 的写法:函数级 ForProfile
// + 进程内测试 sshd 连接计数器。exec_command 与 exec_background 经
// confirmHarness 各覆盖一遍。

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// confirmCmd 命中 POSIX-DELETE-TARGET-UNKNOWN 需确认规则的命令
// (递归删除、目标含变量)。
const confirmCmd = "rm -rf $DIR/"

// confirmReason 是测试里 agent 给的确认理由。
const confirmReason = "user approved deleting the deploy dir after checking $DIR is set"

// confirmCodeRe 从需确认错误文本里抠出确认码 (字母表去掉易混淆字符)。
var confirmCodeRe = regexp.MustCompile(`confirmation code ([A-Z2-9]{6}):`)

// extractConfirmCode 断言 err 是需确认错误并返回其中的确认码。
func extractConfirmCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("want confirmation-required error, got nil")
	}
	m := confirmCodeRe.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("error %q carries no confirmation code", err)
	}
	return m[1]
}

// assertConfirmError 钉需确认错误的一行形态:含规则编号、为什么危险、
// 该怎么做、征得用户同意后带码重调的指引,不换行,也不列出其他规则。
func assertConfirmError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want confirmation-required error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "[POSIX-DELETE-TARGET-UNKNOWN]") {
		t.Fatalf("error %q does not name the rule id [POSIX-DELETE-TARGET-UNKNOWN]", msg)
	}
	if !strings.Contains(msg, "ask the user for consent") { // 征得用户同意的重调指引
		t.Fatalf("error %q does not tell the agent to ask the user for consent", msg)
	}
	if !strings.Contains(msg, "confirmation_code") || !strings.Contains(msg, "confirmation_reason") {
		t.Fatalf("error %q does not name the confirmation_code/confirmation_reason fields", msg)
	}
	if confirmCodeRe.FindStringSubmatch(msg) == nil {
		t.Fatalf("error %q carries no confirmation code", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("error must be one line, got %q", msg)
	}
	if strings.Contains(msg, "POSIX-LOCKOUT") { // 不得列出其他规则
		t.Fatalf("error %q lists another rule", msg)
	}
}

// wantConfirmRows 断言该 action 的审计行数,并返回它们 (最新在前)。
func wantConfirmRows(t *testing.T, st *store.Store, action string, n int) []store.AuditRow {
	t.Helper()
	rows := policyRows(t, st, action)
	if len(rows) != n {
		t.Fatalf("%s rows = %d, want %d: %+v", action, len(rows), n, rows)
	}
	return rows
}

// assertConfirmRow 钉「需确认退回」行的字段:Status 存命中规则编号,
// Command 保持命令原文,Sudo/ServerID/ProjectID 全匹配。
func assertConfirmRow(t *testing.T, r store.AuditRow, serverID, projectID string) {
	t.Helper()
	if r.Status != "POSIX-DELETE-TARGET-UNKNOWN" || r.Command != confirmCmd || r.Sudo ||
		r.ServerID != serverID || r.ProjectID != projectID {
		t.Fatalf("row = {Status:%q Command:%q Sudo:%v Server:%q Proj:%q}, want rule id + verbatim command on %s/%s",
			r.Status, r.Command, r.Sudo, r.ServerID, r.ProjectID, serverID, projectID)
	}
}

// wantConfirmedRow 断言恰一行「带确认码执行」审计行:Status 存命中规则
// 编号,Command 是含命令原文与确认理由的 JSON 摘要。
func wantConfirmedRow(t *testing.T, st *store.Store, action, serverID, projectID, reason string) {
	t.Helper()
	rows := wantConfirmRows(t, st, action, 1)
	r := rows[0]
	if r.Status != "POSIX-DELETE-TARGET-UNKNOWN" || r.Sudo || r.ServerID != serverID || r.ProjectID != projectID {
		t.Fatalf("row = {Status:%q Sudo:%v Server:%q Proj:%q}, want rule id on %s/%s", r.Status, r.Sudo, r.ServerID, r.ProjectID, serverID, projectID)
	}
	var summary map[string]string
	if err := json.Unmarshal([]byte(r.Command), &summary); err != nil {
		t.Fatalf("confirmed row Command is not a JSON summary: %v (%q)", err, r.Command)
	}
	if summary["command"] != confirmCmd {
		t.Fatalf("summary command = %q, want %q", summary["command"], confirmCmd)
	}
	if summary["reason"] != reason {
		t.Fatalf("summary reason = %q, want %q", summary["reason"], reason)
	}
}

// confirmHarness 把两个执行工具包成同一张确认门测试面:call 以给定
// 服务器/命令/确认码/理由调一次工具;started 额外断言「命令真的执行了」。
type confirmHarness struct {
	name      string
	st        *store.Store
	srvID     string
	pid       string
	conns     *atomic.Int64
	call      func(serverID, command, code, reason string) error
	confirm   string // 需确认退回动作名
	confirmed string // 带确认码执行动作名
	start     string // 正常执行行动作名
	started   func(t *testing.T)
}

// newConfirmHarness 装配一个工具的测试面 (bg=true 是 exec_background,
// 否则 exec_command):进程内测试 sshd (带连接计数)、单服务器、profile
// 授权齐全。第二台服务器由用例按需追加 seed。
func newConfirmHarness(t *testing.T, bg bool) *confirmHarness {
	t.Helper()
	conns := &atomic.Int64{}
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:    "pw",
		Connections: conns,
		Exec:        func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	t.Cleanup(cleanup)
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	ctx := context.Background()
	const proj = "proj-confirm"
	if bg {
		m := newTestTM(t, 4)
		t.Cleanup(m.CloseAll)
		return &confirmHarness{
			name: "exec_background", st: st, srvID: srvID, pid: pid, conns: conns,
			call: func(serverID, command, code, reason string) error {
				_, err := ExecBackgroundForProfile(ctx, st, proj, pid, serverID, command, false, 0, m, code, reason)
				return err
			},
			confirm:   "exec-bg-confirm-required",
			confirmed: "exec-bg-confirmed",
			start:     "exec-bg-start",
			started: func(t *testing.T) {
				t.Helper()
				if m.Len() != 1 {
					t.Fatalf("registry len = %d, want 1 running task", m.Len())
				}
			},
		}
	}
	return &confirmHarness{
		name: "exec_command", st: st, srvID: srvID, pid: pid, conns: conns,
		call: func(serverID, command, code, reason string) error {
			_, err := ExecCommandForProfile(ctx, st, proj, pid, serverID, command, false, 5*time.Second, code, reason)
			return err
		},
		confirm:   "exec-confirm-required",
		confirmed: "exec-confirmed",
		start:     "exec",
		started:   func(t *testing.T) { t.Helper() }, // 前台同步返回,连接数与 start 行已够
	}
}

// wantConns 断言测试 sshd 至今收到的连接数。
func (h *confirmHarness) wantConns(t *testing.T, n int64) {
	t.Helper()
	if got := h.conns.Load(); got != n {
		t.Fatalf("[%s] test sshd received %d connections, want %d", h.name, got, n)
	}
}

// TestExecConfirmRequiredIssuesCode:需确认命令不带码调用——一行说明 +
// 确认码,测试 sshd 零连接 (一个字节都没到服务器),审计恰一行「需确认
// 退回」(Status=规则编号,Command=命令原文)。两个工具各验一遍。
func TestExecConfirmRequiredIssuesCode(t *testing.T) {
	for _, bg := range []bool{false, true} {
		h := newConfirmHarness(t, bg)
		t.Run(h.name, func(t *testing.T) {
			err := h.call(h.srvID, confirmCmd, "", "")
			assertConfirmError(t, err)
			h.wantConns(t, 0)
			rows := wantConfirmRows(t, h.st, h.confirm, 1)
			assertConfirmRow(t, rows[0], h.srvID, "proj-confirm")
		})
	}
}

// TestExecConfirmCodeRunsCommand:带正确确认码 + 理由重调——命令照常
// 执行 (连接数 +1、任务表进任务),审计落「带确认码执行」行 (含理由)
// 与正常执行行;同一枚码第二次使用即作废,重新退回附新码。
func TestExecConfirmCodeRunsCommand(t *testing.T) {
	for _, bg := range []bool{false, true} {
		h := newConfirmHarness(t, bg)
		t.Run(h.name, func(t *testing.T) {
			code := extractConfirmCode(t, h.call(h.srvID, confirmCmd, "", ""))

			// 正确码 + 理由:执行成功。
			if err := h.call(h.srvID, confirmCmd, code, confirmReason); err != nil {
				t.Fatalf("confirmed run failed: %v", err)
			}
			h.wantConns(t, 1)
			h.started(t)
			wantConfirmedRow(t, h.st, h.confirmed, h.srvID, "proj-confirm", confirmReason)
			// 正常执行行也在 (前台 exec(ok)/后台 exec-bg-start(ok)),Command 保持原文。
			startRows := policyRows(t, h.st, h.start)
			found := false
			for _, r := range startRows {
				if r.Command == confirmCmd && r.Status == "ok" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no %s(ok) row for %q: %+v", h.start, confirmCmd, startRows)
			}

			// 同一枚码第二次使用:失效,重新退回 (新码),不再执行。
			err := h.call(h.srvID, confirmCmd, code, confirmReason)
			assertConfirmError(t, err)
			if code2 := extractConfirmCode(t, err); code2 == code {
				t.Fatalf("reissued code %q must differ from the consumed one", code2)
			}
			h.wantConns(t, 1)
			rows := wantConfirmRows(t, h.st, h.confirm, 2)
			assertConfirmRow(t, rows[0], h.srvID, "proj-confirm")
		})
	}
}

// TestExecConfirmCodeInvalidScopes:确认码换命令 (哪怕一个字符)、换服务
// 器、超过 5 分钟,全部失效并重新退回;测试 sshd 全程零连接。
func TestExecConfirmCodeInvalidScopes(t *testing.T) {
	for _, bg := range []bool{false, true} {
		h := newConfirmHarness(t, bg)
		t.Run(h.name, func(t *testing.T) {
			t.Run("changed command", func(t *testing.T) {
				code := extractConfirmCode(t, h.call(h.srvID, confirmCmd, "", ""))
				// 命令改一个字符 (多一个斜杠),同码重调:失效重新退回。
				err := h.call(h.srvID, "rm -rf $DIR//", code, confirmReason)
				assertConfirmError(t, err)
				extractConfirmCode(t, err)
			})

			t.Run("other server", func(t *testing.T) {
				// 第二台服务器条目:地址不可拨也无妨——核销先于取凭据,
				// 码对不上时根本走不到连接那一步。
				srvB, _ := h.st.AddServer(&models.Server{Name: "other", Host: "vault-confirm.example.internal", Port: 22, User: "u"})
				_ = h.st.GrantServers(h.pid, []string{srvB})
				code := extractConfirmCode(t, h.call(h.srvID, confirmCmd, "", ""))
				err := h.call(srvB, confirmCmd, code, confirmReason)
				assertConfirmError(t, err)
				extractConfirmCode(t, err)
			})

			t.Run("expired after 5 minutes", func(t *testing.T) {
				fake := time.Now()
				prev := execConfirmations.now
				execConfirmations.now = func() time.Time { return fake }
				defer func() { execConfirmations.now = prev }()
				code := extractConfirmCode(t, h.call(h.srvID, confirmCmd, "", ""))
				fake = fake.Add(5*time.Minute + time.Second)
				err := h.call(h.srvID, confirmCmd, code, confirmReason)
				assertConfirmError(t, err)
				extractConfirmCode(t, err)
			})

			h.wantConns(t, 0)
		})
	}
}

// TestExecConfirmCodeNeedsReason:只带码不带理由——退回并说明差什么,
// 已签发的码不作废;补上理由用同一枚码即可执行。
func TestExecConfirmCodeNeedsReason(t *testing.T) {
	for _, bg := range []bool{false, true} {
		h := newConfirmHarness(t, bg)
		t.Run(h.name, func(t *testing.T) {
			code := extractConfirmCode(t, h.call(h.srvID, confirmCmd, "", ""))

			err := h.call(h.srvID, confirmCmd, code, "")
			if err == nil || !strings.Contains(err.Error(), "confirmation_reason") {
				t.Fatalf("code without reason must be rejected with a one-line hint, got %v", err)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Fatalf("error must be one line, got %q", err)
			}
			h.wantConns(t, 0)
			// 退回也算一次「需确认退回」审计 (第二行)。
			wantConfirmRows(t, h.st, h.confirm, 2)

			// 同一枚码补理由:执行成功——那枚码没有被白耗掉。
			if err := h.call(h.srvID, confirmCmd, code, confirmReason); err != nil {
				t.Fatalf("code must survive the reason-missing retry: %v", err)
			}
			h.wantConns(t, 1)
			wantConfirmedRow(t, h.st, h.confirmed, h.srvID, "proj-confirm", confirmReason)
		})
	}
}
