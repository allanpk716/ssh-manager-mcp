package mcpserver

// Disconnect-semantics regression pins (Plan 35 contract). The three facts
// documented in docs/agent-access.md 「断连语义（四层）」: (1) VerifyToken
// rejects a revoked token immediately (the serve per-request gate); (2) via
// the serve HTTP handler, a revoked project's close_port (and any other
// request) is rejected with 401 BEFORE reaching the tool layer; (3) an
// ALREADY-OPEN forward is torn down within one control tick (~15s; tests
// drive runControlTick directly for determinism) — this flips the Plan-25
// "keeps forwarding" pin (owner decision then, `tunnels kill` CLI was
// backlog); the owner now has the emergency stop: revoke/disable cascade,
// `tunnels kill <id>` / `--project`, and bind-whitelist shrink all close
// tunnels within a tick (Plan 35 spec §1). Background tasks survive
// revocation unchanged (Plan 32 pin, TestRevokedProjectKeepsBackgroundTaskRunning).

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestRevokedProjectTunnelsTornByControlTick flips the OLD Plan-25 pin
// ("revoked project's tunnel keeps forwarding — owner decision, kill CLI was
// backlog"). Plan 35 contract (spec §1): revoke cascades into tunnel teardown
// within one control tick (~15s; tests drive runControlTick directly for
// determinism). The HTTP per-request 401 layer above is unchanged (see
// TestServeHTTPRejectsRevokedTokenPerRequest); background-task survival is
// unchanged (TestRevokedProjectKeepsBackgroundTaskRunning, Plan 32 pin).
func TestRevokedProjectTunnelsTornByControlTick(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	projID, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	echoPort := startEchoListener(t)
	mgr := NewTunnelManager()
	mgr.AttachStore(func() *store.Store { return st }, projID)
	defer mgr.CloseAll()

	out, err := ForwardForProfile(context.Background(), st, projID, pid, srvID, "127.0.0.1", echoPort, 0, "", mgr)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := st.VerifyToken(token); p == nil {
		t.Fatal("sanity: token must verify before revoke")
	}
	if err := st.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.VerifyToken(token); p != nil {
		t.Fatal("layer 1: VerifyToken must reject a revoked token immediately")
	}
	mgr.runControlTick() // deterministic stand-in for the ≤15s tick
	// layer 3 (flipped): the tunnel is TORN DOWN — port unreachable + mirror row gone
	if _, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", out.LocalPort), 500*time.Millisecond); derr == nil {
		t.Fatal("port must be unreachable after revoke+tick")
	}
	if has, _ := st.HasTunnelRegistryRow(out.TunnelID); has {
		t.Fatal("mirror row must be gone after cascade teardown")
	}
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

	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
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

// TestRevokedProjectKeepsBackgroundTaskRunning 钉住上面 tunnel 先例的后台
// 任务版: revoke 立即杀死 token 门 (serve 层 401), 但一个 RUNNING 中的后台
// 任务继续跑——任务由 broker 的 TaskManager 持有, revoke 路径没有任何
// CloseAll 连带 (owner 决策, 与 open tunnel 同契约)。反向钉住意图: 若未来
// revoke 实现加了 TaskManager.CloseAll 连带 (或任何 revoke 时点的任务拆除),
// 本测试必须红——契约变更须经由改测试显式声明, 不得静默发生。
func TestRevokedProjectKeepsBackgroundTaskRunning(t *testing.T) {
	st := newStore(t)
	gate := newGatedExec()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: gate.handler("longrun")})
	defer cleanup()
	defer gate.open()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	projID, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}

	_, mgr, tasks, err := NewServer(st, pid, projID)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()
	defer tasks.CloseAll()

	// 长跑任务: 门控滞留 handler → 恒 running (waitEntered = 会话确在途的确定性锚)。
	startOut, err := ExecBackgroundForProfile(context.Background(), st, projID, pid, srvID, "longrun", false, 60, tasks)
	if err != nil {
		t.Fatal(err)
	}
	gate.waitEntered(t, "longrun")

	// sanity: revoke 前 Output 可读且 running。
	before, err := ExecOutputForProfile(context.Background(), st, projID, startOut.TaskID, 0, 0, 0, "text", tasks)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != bgStatusRunning {
		t.Fatalf("sanity: status before revoke = %q, want running", before.Status)
	}
	if p, _ := st.VerifyToken(token); p == nil {
		t.Fatal("sanity: token must verify before revoke")
	}

	if err := st.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	// Layer 1 (与 tunnel 先例同一断言): token 门立即拒绝。
	if p, _ := st.VerifyToken(token); p != nil {
		t.Fatal("VerifyToken must reject a revoked token immediately")
	}

	// 反向钉住主体: revoke 无 CloseAll 连带——任务仍 running 且 Output 可读
	// (ExecOutputForProfile 不触 store, 任务记录仍在 TaskManager)。
	after, err := ExecOutputForProfile(context.Background(), st, projID, startOut.TaskID, 0, 0, 0, "text", tasks)
	if err != nil {
		t.Fatalf("post-revoke Output: %v (task record must survive revoke)", err)
	}
	if after.Status != bgStatusRunning {
		t.Fatalf("post-revoke status = %q, want still running — revoke must not cascade a TaskManager teardown", after.Status)
	}

	// 放行 → 引擎照常落终态 done 且全量可读 (任务生命周期独立于 token 状态)。
	gate.open()
	if s := waitTerminal(t, tasks, startOut.TaskID, 5*time.Second); s.status != bgStatusDone {
		t.Fatalf("after release: status = %q, want done", s.status)
	}
	final, err := ExecOutputForProfile(context.Background(), st, projID, startOut.TaskID, 0, 0, 0, "text", tasks)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != bgStatusDone || final.Stdout != gatedOut {
		t.Fatalf("final Output = {status:%q stdout:%q}, want done + %q", final.Status, final.Stdout, gatedOut)
	}
}
