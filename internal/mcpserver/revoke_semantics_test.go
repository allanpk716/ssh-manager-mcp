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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

// TestRevokedProjectKeepsOpenTunnelForwarding pins layers 1+3: revocation
// kills the token gate immediately, but the broker-held tunnel keeps
// forwarding (no cascade teardown — owner decision, kill CLI is backlog
// (see docs/backlog.md)).
func TestRevokedProjectKeepsOpenTunnelForwarding(t *testing.T) {
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

	_, mgr, tasks, err := NewServer(st, pid, projID)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()
	defer tasks.CloseAll()
	out, err := ForwardForProfile(context.Background(), st, projID, pid, srvID, "127.0.0.1", echoPort, 0, "", mgr)
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
