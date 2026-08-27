package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestE2EIronRule is the capstone: a Profile-scoped MCP client can use its servers
// and is blocked from others, with credentials never crossing the tool boundary.
func TestE2EIronRule(t *testing.T) {
	st := newStore(t)

	// Two real sshd backends: one the agent may use, one it may not.
	allowedAddr, allowedHK, allowedCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "ALLOWED:" + cmd + "\n", "", 0 },
	})
	defer allowedCleanup()
	forbiddenAddr, forbiddenHK, forbiddenCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "FORBIDDEN\n", "", 0 },
	})
	defer forbiddenCleanup()

	allowedID := seedRealServer(t, st, "allowed", allowedAddr, allowedHK, "")
	// forbidden is routed through the "localhost" loopback alias (same listener as
	// 127.0.0.1) so it remains genuinely dialable. The host-key store is keyed by
	// host:port, so even with both loopback sshd instances on the same host string
	// their distinct ports keep the pinned keys separate. Using "localhost" also
	// gives a distinct host string, so if the iron rule ever failed, forbidden
	// would actually connect and return "FORBIDDEN" (failing the test), rather
	// than failing coincidentally on a host-key mismatch.
	forbiddenID := seedServerOnHost(t, st, "forbidden", "localhost", forbiddenAddr, forbiddenHK, "")

	pid, _ := st.AddProfile("agent-profile")
	_ = st.GrantServers(pid, []string{allowedID}) // only allowed in profile

	server, mgr, tasks, _ := NewServer(st, pid, "proj-test")
	defer mgr.CloseAll()
	defer tasks.CloseAll()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, _ := server.Connect(context.Background(), t1, nil)
	defer srvSess.Close()
	cliSess, _ := client.Connect(context.Background(), t2, nil)
	defer cliSess.Close()
	ctx := context.Background()

	// 1. list_servers -> only "allowed"
	res, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
	if res.IsError {
		t.Fatal("list_servers should succeed")
	}
	// (Content is JSON; assert it contains "allowed" and not "forbidden" via the text.)
	if !textContains(res, "allowed") || textContains(res, "forbidden") {
		t.Fatalf("list_servers leaked a forbidden server: %+v", res.Content)
	}

	// 2. exec on allowed -> works
	res2, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"server_id": allowedID, "command": "hi"}})
	if res2.IsError {
		t.Fatalf("allowed exec should succeed: %+v", res2.Content)
	}

	// 3. exec on forbidden -> tool error (iron rule)
	res3, _ := cliSess.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"server_id": forbiddenID, "command": "hi"}})
	if !res3.IsError {
		t.Fatal("forbidden exec must be rejected (iron rule)")
	}
}

func textContains(res *mcp.CallToolResult, want string) bool {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if containsStr(tc.Text, want) {
				return true
			}
		}
	}
	return false
}
func containsStr(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// seedServerOnHost is seedRealServer with an explicit Host string (used to keep
// two same-loopback test sshd backends distinct in the host-key store).
func seedServerOnHost(t *testing.T, st *store.Store, name, host, addr string, hk ssh.PublicKey, sudoPw string) string {
	t.Helper()
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srv := &models.Server{
		Name: name, Host: host, Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: cid,
	}
	if sudoPw != "" {
		sid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(sudoPw)})
		srv.SudoCredentialID = sid
	}
	id, _ := st.AddServer(srv)
	_ = st.SaveHostKey(host, srv.Port, hk.Marshal()) // pre-trust the testsshd host key under this host alias
	return id
}

// unmarshalToolJSON 解码工具的结构化输出 (SDK 把 typed output 序列化成
// TextContent 的 JSON 文本) 到 v——in-memory wire 上的真实形态。
func unmarshalToolJSON(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if err := json.Unmarshal([]byte(tc.Text), v); err != nil {
				t.Fatalf("unmarshal tool output %q into %T: %v", tc.Text, v, err)
			}
			return
		}
	}
	t.Fatalf("tool result has no TextContent to unmarshal: %+v", res.Content)
}

// TestE2EBackgroundTrioFullFlow 是后台三件套 (Plan 32 T8) 的全流 capstone:
// 同一条 in-memory MCP 会话跑完整 agent 工作流——
//   - initialize (client.Connect 内完成握手) → tools/list 断言恰 10 工具 (6+3+1)
//     且名称与 BrokerTools 单源核对 (集合相等——SDK featureSet 无序, 协议对
//     tools/list 无序保证)——切片即注册面唯一事实源;
//   - exec_background 起多行输出任务 → exec_output wait 轮询携 next offset
//     推进收集增量至终态 → 终态取尾钉排空锚 (next==total, 零新增);
//   - exec_stop 停第二个 (门控 "睡眠") 任务, 立即回触发时刻 running, 终态
//     stopped 经 exec_output 观察。
//
// 确定性 testsshd 形态: "multi" 立即回多行输出 (快任务); "sleepy" 门控滞留
// handler = 睡眠任务 (tasks_exec_test.go 的 gatedExec 先例)。
func TestE2EBackgroundTrioFullFlow(t *testing.T) {
	st := newStore(t)
	gate := newGatedExec()
	gated := gate.handler("sleepy")
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, r io.Reader) (string, string, int) {
			if cmd == "multi" {
				return "L1\nL2\nL3\n", "", 0
			}
			return gated(cmd, r)
		},
	})
	defer cleanup()
	defer gate.open()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("agent-profile")
	_ = st.GrantServers(pid, []string{srvID})

	server, mgr, tasks, _ := NewServer(st, pid, "proj-test")
	defer mgr.CloseAll()
	defer tasks.CloseAll()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, _ := server.Connect(context.Background(), t1, nil)
	defer srvSess.Close()
	cliSess, _ := client.Connect(context.Background(), t2, nil) // initialize 握手在 Connect 内完成
	defer cliSess.Close()
	ctx := context.Background()

	// 0. tools/list: 恰 10 工具 (6+3+1), 名称与 BrokerTools 单源核对——集合相等
	//    (SDK featureSet 是 map + 按名排序输出, 协议对 tools/list 无序保证,
	//    故断言集合而非注册序)。
	lt, err := cliSess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lt.Tools) != len(BrokerTools) {
		t.Fatalf("tools/list = %d tools (BrokerTools has %d), want exactly %d", len(lt.Tools), len(BrokerTools), len(BrokerTools))
	}
	listed := map[string]bool{}
	for _, tl := range lt.Tools {
		if listed[tl.Name] {
			t.Fatalf("tools/list lists %q twice: %+v", tl.Name, lt.Tools)
		}
		listed[tl.Name] = true
	}
	for i, want := range BrokerTools {
		if !listed[want] {
			t.Fatalf("tools/list is missing BrokerTools[%d] = %q (listed: %v)", i, want, listed)
		}
		delete(listed, want)
	}
	if len(listed) != 0 {
		t.Fatalf("tools/list has names outside BrokerTools: %v", listed)
	}

	// 1. exec_background: 快任务 (多行输出); 缺省超时 → 生产 runCap 24h 回显。
	res, err := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": srvID, "command": "multi"},
	})
	if err != nil || res.IsError {
		t.Fatalf("exec_background: err=%v res=%+v", err, res.Content)
	}
	var start struct {
		TaskID                  string `json:"task_id"`
		EffectiveTimeoutSeconds int    `json:"effective_timeout_seconds"`
		Status                  string `json:"status"`
	}
	unmarshalToolJSON(t, res, &start)
	if start.TaskID == "" || start.Status != bgStatusRunning || start.EffectiveTimeoutSeconds != 86400 {
		t.Fatalf("start output = %+v, want task_id + running + effective 86400 (缺省 runCap 24h)", start)
	}

	// 2. exec_output wait 轮询: 携 next offset 推进收集增量, 至离开 running。
	const multiOut = "L1\nL2\nL3\n"
	collected := ""
	var off, errOff int64
	// #8: 固定 5 轮在慢 CI 上预算不足 (5×2s wait)——deadline 轮询, 每轮仍
	// wait=2 长轮询携游标收集 (偏移推进保证多轮不重复收); 终态即断。
	status := bgStatusRunning
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; status == bgStatusRunning && time.Now().Before(deadline); i++ {
		res2, cerr := cliSess.CallTool(ctx, &mcp.CallToolParams{
			Name: "exec_output",
			Arguments: map[string]any{
				"task_id": start.TaskID, "wait_seconds": 2,
				"stdout_offset": off, "stderr_offset": errOff,
			},
		})
		if cerr != nil || res2.IsError {
			t.Fatalf("exec_output poll %d: err=%v res=%+v", i, cerr, res2.Content)
		}
		var read BgReadOutput
		unmarshalToolJSON(t, res2, &read)
		collected += read.Stdout
		off, errOff = read.NextStdoutOffset, read.NextStderrOffset
		status = read.Status
	}
	if status != bgStatusDone {
		t.Fatalf("task did not reach done within 30s deadline (last status=%q)", status)
	}
	if collected != multiOut {
		t.Fatalf("collected stdout = %q, want %q", collected, multiOut)
	}

	// 3. 终态取尾 (排空锚): offset 已推到 next, 再读零新增且 next==total 不再增长。
	res3, cerr := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name: "exec_output",
		Arguments: map[string]any{
			"task_id":       start.TaskID,
			"stdout_offset": off, "stderr_offset": errOff,
		},
	})
	if cerr != nil || res3.IsError {
		t.Fatalf("tail read: err=%v res=%+v", cerr, res3.Content)
	}
	var tail BgReadOutput
	unmarshalToolJSON(t, res3, &tail)
	if tail.Stdout != "" || tail.Stderr != "" {
		t.Fatalf("drain anchor violated: tail stdout=%q stderr=%q, want empty", tail.Stdout, tail.Stderr)
	}
	if tail.NextStdoutOffset != off || tail.NextStdoutOffset != tail.StdoutBytesTotal {
		t.Fatalf("drain anchor cursors: next=%d total=%d (offset=%d), want next==total==%d",
			tail.NextStdoutOffset, tail.StdoutBytesTotal, off, off)
	}
	if tail.NextStderrOffset != tail.StderrBytesTotal {
		t.Fatalf("stderr cursors: next=%d total=%d, want equal", tail.NextStderrOffset, tail.StderrBytesTotal)
	}

	// 4. exec_background: 第二个 (睡眠) 任务——门控滞留 handler = 恒 running。
	res4, cerr := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_background",
		Arguments: map[string]any{"server_id": srvID, "command": "sleepy"},
	})
	if cerr != nil || res4.IsError {
		t.Fatalf("exec_background(sleepy): err=%v res=%+v", cerr, res4.Content)
	}
	var start2 struct {
		TaskID string `json:"task_id"`
	}
	unmarshalToolJSON(t, res4, &start2)
	gate.waitEntered(t, "sleepy") // 确定性锚: 会话确在运行态, stop 必命中活任务

	// 5. exec_stop: 立即返回触发时刻 running (不阻塞等终态)。
	res5, serr := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec_stop",
		Arguments: map[string]any{"task_id": start2.TaskID},
	})
	if serr != nil || res5.IsError {
		t.Fatalf("exec_stop: err=%v res=%+v", serr, res5.Content)
	}
	if !textContains(res5, `"status":"running"`) {
		t.Fatalf("stop on a running task must answer the trigger-time status running: %+v", res5.Content)
	}

	// 6. exec_output 观察 sleepy 终态 stopped (wait 长轮询真实走一轮)。
	// #8 同款: deadline 轮询至 sleepy 离开 running (每轮 wait=2 长轮询)。
	stopStatus := bgStatusRunning
	stopDeadline := time.Now().Add(30 * time.Second)
	for i := 0; stopStatus == bgStatusRunning && time.Now().Before(stopDeadline); i++ {
		res6, perr := cliSess.CallTool(ctx, &mcp.CallToolParams{
			Name:      "exec_output",
			Arguments: map[string]any{"task_id": start2.TaskID, "wait_seconds": 2},
		})
		if perr != nil || res6.IsError {
			t.Fatalf("exec_output(sleepy) poll %d: err=%v res=%+v", i, perr, res6.Content)
		}
		var read BgReadOutput
		unmarshalToolJSON(t, res6, &read)
		stopStatus = read.Status
	}
	if stopStatus != bgStatusStopped {
		t.Fatalf("sleepy task terminal = %q, want stopped (30s deadline)", stopStatus)
	}
}

// TestE2EUploadContentFullFlow drives upload_content end-to-end over the SDK
// in-memory transport (Plan 33 T4): base64 binary lands byte-exact with the
// parent created, and the tool DESCRIPTION embeds the resolved cap (the env
// seam's dynamic-description pin, spec rev3 §1.2).
func TestE2EUploadContentFullFlow(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("agent-profile")
	_ = st.GrantServers(pid, []string{srvID})

	server, mgr, tasks, _ := NewServer(st, pid, "proj-test")
	defer mgr.CloseAll()
	defer tasks.CloseAll()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, _ := server.Connect(context.Background(), t1, nil)
	defer srvSess.Close()
	cliSess, _ := client.Connect(context.Background(), t2, nil)
	defer cliSess.Close()
	ctx := context.Background()

	// description embeds the resolved cap (default 8 MiB in this test env).
	lt, err := cliSess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	capStr := fmt.Sprint(8 << 20)
	descOK := false
	for _, tl := range lt.Tools {
		if tl.Name == "upload_content" && strings.Contains(tl.Description, "Capped at "+capStr+" bytes decoded") {
			descOK = true
		}
	}
	if !descOK {
		t.Fatalf("upload_content description does not embed the resolved cap %q", capStr)
	}

	// base64 binary upload → byte-exact landing with parent creation.
	bin := []byte{0x00, 0xFF, 0x7F, 0xD6, 0xD0, 0x0A}
	target := toSlash(filepath.Join(t.TempDir(), "e2e-uc", "sub", "blob.bin"))
	res, err := cliSess.CallTool(ctx, &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   srvID,
			"content":     base64.StdEncoding.EncodeToString(bin),
			"remote_path": target,
			"encoding":    "base64",
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("upload_content: err=%v res=%+v", err, res.Content)
	}
	var out UploadContentOutput
	unmarshalToolJSON(t, res, &out)
	if out.Bytes != int64(len(bin)) {
		t.Fatalf("Bytes = %d, want %d", out.Bytes, len(bin))
	}
	if got, _ := os.ReadFile(filepath.FromSlash(target)); !bytes.Equal(got, bin) {
		t.Fatalf("e2e bytes = %x, want %x", got, bin)
	}
}
