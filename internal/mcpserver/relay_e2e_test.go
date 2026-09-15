package mcpserver

// Plan 47 T7: relay_file e2e 全景 (spec §8 e2e 段)。全部走注册的 relay_file 工具
// (in-memory MCP 传输, TestE2EBackgroundTrioFullFlow 同款), 不直调 RelayForProfile
// ——e2e 的立意就是工具面: 工具调用 → task_id → exec_output 轮询/推进 → 终态。
//
// 取证口径: 目标端是 testsshd 的 sftp 子系统 = 宿主 FS, 因此"远端"工件
// (manifest/partial/真名) 用 os 直读/直栽/直 stat (relayengine_test.go 同款);
// manifest 是 tmp+PosixRename 原子落盘, 读者要么见旧要么见新、无撕裂, 中途直读
// 是协议内安全操作。
//
// 块网格: MCP 工具层的 chunkBytes 来自构造期 env seam (SSHMGR_TRANSFER_CHUNK,
// 钳域下限 16 MiB = relayBigChunk), e2e 固定该网格——与引擎白盒的任意小网格不
// 同, 这里顺带钉住"生产下限网格跑通全流程"。
//
// 中断注入的两种定时形态与确定性论证 (16MiB 块搬运 ≥10ms 对比工具往返 µs-ms):
//   - 块线后停: "chunk 0/N ok" 进度行在 stdout 出现 → exec_stop。清单记账先于
//     进度行落笔 (relay.go 逐块段), 停时 manifest 恰含块 0; 剩余块的整块搬运窗
//     远宽于 stop 往返 (TestRelayEngineResumeStopPreservesManifest 同款论证)。
//   - 双件齐停: 宿主 FS 紧轮询 manifest+partial 双双在场 (引擎 stage 0 落盘点)
//     → exec_stop。双件出现到块 0 记账隔着整块搬运, stop 必落块 0 在途 → chunks=[]。
//   - 直栽残留: "清单后 partial 前"的 µs 级引擎窗口无法定时注入, 直栽 §3 状态表
//     的盘上形态 (T4 状态表同款取证), e2e 补的是真实引擎完成段的证据。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// ---------- e2e fixtures ----------

// relayE2EEnv 是 relay e2e 的环境: 一台 testsshd 播两行同址服务器行 (源/目标),
// profile 双授权, SSHMGR_TRANSFER_CHUNK 钉在 16 MiB 下限 (工具层网格)。
type relayE2EEnv struct {
	st     *store.Store
	pid    string
	srcID  string
	dstID  string
	root   string // slash 形式宿主 FS 根 (testsshd 的 sftp 服务它)
	local  string // root 的宿主路径形式 (本机源 fixture 用)
	projID string
}

func newRelayE2EEnv(t *testing.T) *relayE2EEnv {
	t.Helper()
	t.Setenv("SSHMGR_TRANSFER_CHUNK", "16777216") // 16 MiB = 钳域下限 = relayBigChunk
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	st := newStore(t)
	srcID := seedRealServer(t, st, "rsrc", addr, hk, "")
	dstID := seedRealServer(t, st, "rdst", addr, hk, "")
	pid, _ := st.AddProfile("agent-profile")
	_ = st.GrantServers(pid, []string{srcID, dstID})
	root := toSlash(t.TempDir())
	return &relayE2EEnv{
		st: st, pid: pid, srcID: srcID, dstID: dstID,
		root: root, local: filepath.FromSlash(root), projID: "proj-relay-e2e",
	}
}

func (e *relayE2EEnv) input(fromSrv, fromPath, toPath string, fresh bool) RelayInput {
	return RelayInput{FromServerID: fromSrv, FromPath: fromPath, ToServerID: e.dstID, ToPath: toPath, Fresh: fresh}
}

// relayE2ESession 是一条 in-memory MCP 会话 + 它构造期绑定的双 manager。openSession
// 可对同一 env 开多条 (broker restart 语义模拟: 每条会话一个全新 TaskManager)。
type relayE2ESession struct {
	cli *mcp.ClientSession
	srv *mcp.ServerSession
	mgr *TunnelManager
	tm  *TaskManager
}

func (e *relayE2EEnv) openSession(t *testing.T) *relayE2ESession {
	t.Helper()
	server, mgr, tasks, err := NewServer(e.st, e.pid, e.projID)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := server.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	s := &relayE2ESession{cli: cliSess, srv: srvSess, mgr: mgr, tm: tasks}
	t.Cleanup(s.close)
	return s
}

// close 幂等收口 (TaskManager.CloseAll 的 stopOnce 保证; 会话 Close 幂等)。
func (s *relayE2ESession) close() {
	_ = s.cli.Close()
	_ = s.srv.Close()
	s.mgr.CloseAll()
	s.tm.CloseAll()
}

// relayE2ECall 走注册的 relay_file 工具 (MCP 级)。transport 错误即 Fatal; 工具级
// IsError 原样交还调用方 (out 为零值)——拒绝分支的断言方。
func relayE2ECall(t *testing.T, s *relayE2ESession, in RelayInput) (RelayOutput, *mcp.CallToolResult) {
	t.Helper()
	res, err := s.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id": in.FromServerID, "from_path": in.FromPath,
			"to_server_id": in.ToServerID, "to_path": in.ToPath, "fresh": in.Fresh,
		},
	})
	if err != nil {
		t.Fatalf("relay_file call: %v", err)
	}
	if res.IsError {
		return RelayOutput{}, res
	}
	var out RelayOutput
	unmarshalToolJSON(t, res, &out)
	return out, res
}

// relayE2ECallOK 是成功路径形态: IsError 即 Fatal。
func relayE2ECallOK(t *testing.T, s *relayE2ESession, in RelayInput) RelayOutput {
	t.Helper()
	out, res := relayE2ECall(t, s, in)
	if res.IsError {
		t.Fatalf("relay_file must succeed: %+v", res.Content)
	}
	return out
}

// relayE2EPoll exec_output 长轮询循环 (wait=2 携游标推进, TestE2EBackgroundTrioFullFlow
// 同款) 至任务离开 running; 返回收集的 stdout 全量、终态与失败文本 (引擎错误经
// 任务字段 surfacing, 不入输出流)。
func relayE2EPoll(t *testing.T, s *relayE2ESession, taskID string, budget time.Duration) (string, string, string) {
	t.Helper()
	var collected strings.Builder
	var off, errOff int64
	status := bgStatusRunning
	errText := ""
	deadline := time.Now().Add(budget)
	for status == bgStatusRunning && time.Now().Before(deadline) {
		res, cerr := s.cli.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "exec_output",
			Arguments: map[string]any{
				"task_id": taskID, "wait_seconds": 2,
				"stdout_offset": off, "stderr_offset": errOff,
			},
		})
		if cerr != nil || res.IsError {
			t.Fatalf("exec_output poll: err=%v res=%+v", cerr, res.Content)
		}
		var read BgReadOutput
		unmarshalToolJSON(t, res, &read)
		collected.WriteString(read.Stdout)
		off, errOff = read.NextStdoutOffset, read.NextStderrOffset
		status = read.Status
		errText = read.Error
	}
	if status == bgStatusRunning {
		t.Fatalf("task %s never left running within %v (collected:\n%s)", taskID, budget, collected.String())
	}
	return collected.String(), status, errText
}

// relayE2EWaitLine 紧轮询 (wait=0 快照 + 2ms 间隔) 至 stdout 出现 frag 且任务仍
// running; frag 出现前任务已离开 running 即 Fatal (注入窗被整体跳过 = 测试前提
// 崩塌, 不得静默降级)。返回已收集 stdout。
func relayE2EWaitLine(t *testing.T, s *relayE2ESession, taskID, frag string, budget time.Duration) string {
	t.Helper()
	var collected strings.Builder
	var off int64
	deadline := time.Now().Add(budget)
	for {
		res, cerr := s.cli.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "exec_output",
			Arguments: map[string]any{"task_id": taskID, "stdout_offset": off},
		})
		if cerr != nil || res.IsError {
			t.Fatalf("exec_output snapshot: err=%v res=%+v", cerr, res.Content)
		}
		var read BgReadOutput
		unmarshalToolJSON(t, res, &read)
		collected.WriteString(read.Stdout)
		off = read.NextStdoutOffset
		if strings.Contains(collected.String(), frag) {
			if read.Status != bgStatusRunning {
				t.Fatalf("frag %q observed but task already %s — injection window missed", frag, read.Status)
			}
			return collected.String()
		}
		if read.Status != bgStatusRunning {
			t.Fatalf("task reached %s before %q appeared — injection window missed:\n%s", read.Status, frag, collected.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("frag %q never appeared within %v:\n%s", frag, budget, collected.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// relayE2EStopNow exec_stop (断言触发时刻 running——终态即注入窗已失) 并长轮询至
// 终态 stopped。
func relayE2EStopNow(t *testing.T, s *relayE2ESession, taskID string, budget time.Duration) {
	t.Helper()
	res, serr := s.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_stop", Arguments: map[string]any{"task_id": taskID},
	})
	if serr != nil || res.IsError {
		t.Fatalf("exec_stop: err=%v res=%+v", serr, res.Content)
	}
	var stop BgStopOutput
	unmarshalToolJSON(t, res, &stop)
	if stop.Status != bgStatusRunning {
		t.Fatalf("exec_stop trigger-time status = %q, want running (the injection window was missed)", stop.Status)
	}
	_, status, _ := relayE2EPoll(t, s, taskID, budget)
	if status != bgStatusStopped {
		t.Fatalf("terminal = %q, want stopped", status)
	}
}

// relayE2EWaitArtifacts 宿主 FS 紧轮询至 manifest 与 partial 双双在场 (引擎
// stage 0 的落盘点), deadline 内不成即 Fatal。
func relayE2EWaitArtifacts(t *testing.T, e *relayE2EEnv, to string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		_, merr := os.Stat(filepath.FromSlash(to + relayManifestSuffix))
		_, perr := os.Stat(filepath.FromSlash(to + relayPartialSuffix))
		if merr == nil && perr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manifest+partial never both appeared within %v (manifest err=%v, partial err=%v)", d, merr, perr)
		}
		time.Sleep(time.Millisecond)
	}
}

// relayE2EManifest 直读宿主 FS 上的目标 manifest 并解析 (原子写保证无撕裂读)。
func relayE2EManifest(t *testing.T, e *relayE2EEnv, to string) *sshbroker.RelayManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(to + relayManifestSuffix))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m sshbroker.RelayManifest
	if jerr := json.Unmarshal(b, &m); jerr != nil {
		t.Fatalf("parse manifest %s: %v", b, jerr)
	}
	return &m
}

// relayE2EAssertGone 提交完成后四工件全部缺席 (manifest/partial/.tmp/——真名由
// 调用方另行断言在)。
func relayE2EAssertGone(t *testing.T, to string) {
	t.Helper()
	for _, art := range []string{to + relayManifestSuffix, to + relayPartialSuffix, to + relayManifestSuffix + ".tmp"} {
		if _, serr := os.Stat(filepath.FromSlash(art)); !os.IsNotExist(serr) {
			t.Fatalf("%s must be gone after commit, stat err=%v", art, serr)
		}
	}
}

// relayE2EFileSHA 从 done 行解析 file_sha256 (§1.2 验证配方的提取端)。
var relayDoneFileSHA = regexp.MustCompile(`file_sha256=sha256:([0-9a-f]{64})`)

func relayE2EFileSHA(t *testing.T, stdout string) string {
	t.Helper()
	m := relayDoneFileSHA.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no file_sha256 in the done line:\n%s", stdout)
	}
	return m[1]
}

// relayE2ESHAFile 宿主 FS 文件的 sha256 (sha256sum 式读回的本地实现——testsshd
// 的 Exec 是回调不是真 shell, 引擎白盒同款直读口径)。
func relayE2ESHAFile(t *testing.T, slashPath string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(slashPath))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// relayE2EAssertBytes 真名字节精确断言。
func relayE2EAssertBytes(t *testing.T, slashPath, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.FromSlash(slashPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("real name %s: %d bytes, want byte-exact %d", slashPath, len(got), len(want))
	}
}

// ---------- 锚 1: 全流程 (小网格多块 → 轮询至 done → 目标读回对 file_sha256) ----------

func TestE2ERelayFileFullFlow(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	srcData := strings.Repeat("a", 3*relayBigChunk) // 48 MiB → 3 块
	src := e.root + "/full/src.bin"
	relayMkFile(t, src, srcData)
	to := e.root + "/full/dst/deep/f.bin" // 父目录不存在 → 引擎 stage 0 MkdirAll 真走

	out := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	if out.TaskID == "" {
		t.Fatal("TaskID empty")
	}
	if out.BytesTotal != int64(len(srcData)) || out.ChunksTotal != 3 || out.ResumedChunks != 0 || out.ChunkBytes != relayBigChunk {
		t.Fatalf("RelayOutput = %+v, want 48MiB/3 chunks/0 resumed/16MiB grid", out)
	}
	switch runtime.GOOS {
	case "windows":
		if out.SpaceCheck != "unavailable" {
			t.Fatalf("SpaceCheck = %q, want unavailable (windows statvfs stub)", out.SpaceCheck)
		}
	default:
		if out.SpaceCheck != "ok" {
			t.Fatalf("SpaceCheck = %q, want ok", out.SpaceCheck)
		}
	}

	stdout, status, _ := relayE2EPoll(t, s, out.TaskID, 90*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}
	wantPlan := fmt.Sprintf("relay plan: %d bytes, 3 chunks (resumed 0), chunk=%d", len(srcData), relayBigChunk)
	if !strings.Contains(stdout, wantPlan) {
		t.Fatalf("plan line mismatch:\n%s", stdout)
	}
	if n := strings.Count(stdout, " ok bytes="); n != 3 {
		t.Fatalf("chunk progress lines = %d, want 3:\n%s", n, stdout)
	}
	// file_sha256 == 源 sha (§1.2: fresh 全程任务的验证凭据), 且目标真名读回
	// (sha256sum 式) 与之一致 + 字节精确。
	fileSHA := relayE2EFileSHA(t, stdout)
	if fileSHA != relayE2ESHAFile(t, src) {
		t.Fatalf("file_sha256 = %s, want the source sha %s", fileSHA, relayE2ESHAFile(t, src))
	}
	relayE2EAssertBytes(t, to, srcData)
	if got := relayE2ESHAFile(t, to); got != fileSHA {
		t.Fatalf("destination readback sha = %s, want file_sha256 %s", got, fileSHA)
	}
	relayE2EAssertGone(t, to)
}

// ---------- 锚 2: 续传全流程 (Stop→盘上残留→重跑→只补缺块→字节精确+rename+清单删) ----------

func TestE2ERelayFileResumeAfterStop(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	srcData := strings.Repeat("a", 2*relayBigChunk) // 32 MiB → 2 块
	src := e.root + "/resume/src.bin"
	relayMkFile(t, src, srcData)
	to := e.root + "/resume/f.bin"

	out1 := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	if out1.ResumedChunks != 0 || out1.ChunksTotal != 2 {
		t.Fatalf("first run = %+v, want 0 resumed of 2", out1)
	}
	// 注入点 = 首块进度行 (引擎 1-based: "chunk 1/2 ok")。清单记账先于进度行
	// 落笔, 停时 manifest 恰含块 0; 块 2 的整块搬运 (~250ms 实测) 远宽于 stop
	// 的工具往返。实测网格: 16MiB ≈ 65-95 MiB/s (loopback sftp)。
	relayE2EWaitLine(t, s, out1.TaskID, "chunk 1/2 ok", 60*time.Second)
	relayE2EStopNow(t, s, out1.TaskID, 30*time.Second)

	// 盘上残留: manifest 恰含块 0 (记账先于进度行), partial 在, 真名不在。
	m := relayE2EManifest(t, e, to)
	if len(m.Chunks) != 1 || m.Chunks[0].I != 0 {
		t.Fatalf("manifest chunks = %+v, want exactly chunk 0", m.Chunks)
	}
	if fi, serr := os.Stat(filepath.FromSlash(to + relayPartialSuffix)); serr != nil || fi.Size() < relayBigChunk {
		t.Fatalf("partial: fi=%v err=%v", fi, serr)
	}
	if _, serr := os.Stat(filepath.FromSlash(to)); !os.IsNotExist(serr) {
		t.Fatalf("real name must be absent after the stop, stat err=%v", serr)
	}

	// 重跑同参数 → manifest 驱动续传。
	out2 := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	if out2.ResumedChunks != 1 || out2.ChunksTotal != 2 {
		t.Fatalf("resume run = %+v, want 1 resumed of 2", out2)
	}
	stdout, status, _ := relayE2EPoll(t, s, out2.TaskID, 60*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}
	// 源读字节计数 = 缺块 (行为学断言, 零引擎改动): 每个被搬运的块恰落一行进度
	// (清单记账之后落笔), 续传路径其余唯一源读是 stage 0 抽读复核 (最高完成块)
	// 与 1 字节 EOF 确认——都不落进度行。1 行 = 恰块 1 被搬运。
	if n := strings.Count(stdout, " ok bytes="); n != 1 {
		t.Fatalf("chunk progress lines = %d, want 1 (only the missing chunk moves):\n%s", n, stdout)
	}
	wantPlan := fmt.Sprintf("relay plan: %d bytes, 2 chunks (resumed 1), chunk=%d", len(srcData), relayBigChunk)
	if !strings.Contains(stdout, wantPlan) {
		t.Fatalf("resume plan line mismatch:\n%s", stdout)
	}
	// 续传任务 (resumed>0) 的 done 行只有 merkle 根, 无 file_sha256 段 (§1.2)。
	if !strings.Contains(stdout, "relay done: root=sha256:") || strings.Contains(stdout, "file_sha256") {
		t.Fatalf("resumed done line must be root-only:\n%s", stdout)
	}
	relayE2EAssertBytes(t, to, srcData)
	relayE2EAssertGone(t, to)
}

// ---------- 锚 3: 首块前中断 (双件齐) → 可续传 ----------

func TestE2ERelayFilePreChunkStopBothArtifactsResumable(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	srcData := strings.Repeat("a", 2*relayBigChunk)
	src := e.root + "/prechunk/src.bin"
	relayMkFile(t, src, srcData)
	to := e.root + "/prechunk/f.bin"

	out1 := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	// 双件齐 (引擎 stage 0 落盘点) 即停: 双件出现到块 0 记账隔着整块 16MiB 的搬运
	// (≥10ms), 工具往返 µs-ms——stop 必落块 0 在途, 清单零记账。
	relayE2EWaitArtifacts(t, e, to, 30*time.Second)
	relayE2EStopNow(t, s, out1.TaskID, 30*time.Second)

	m := relayE2EManifest(t, e, to)
	if len(m.Chunks) != 0 {
		t.Fatalf("manifest chunks = %+v, want zero recorded (the stop landed inside chunk 0's transfer window)", m.Chunks)
	}
	if _, serr := os.Stat(filepath.FromSlash(to + relayPartialSuffix)); serr != nil {
		t.Fatalf("partial must exist (双件齐), stat err=%v", serr)
	}
	if _, serr := os.Stat(filepath.FromSlash(to)); !os.IsNotExist(serr) {
		t.Fatalf("real name must be absent, stat err=%v", serr)
	}

	// 双件齐 (manifest 空 + partial 在) → 重跑续传直走。
	out2 := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	if out2.ResumedChunks != 0 || out2.ChunksTotal != 2 {
		t.Fatalf("resume run = %+v, want 0 resumed (nothing recorded) of 2", out2)
	}
	stdout, status, _ := relayE2EPoll(t, s, out2.TaskID, 60*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}
	if n := strings.Count(stdout, " ok bytes="); n != 2 {
		t.Fatalf("chunk progress lines = %d, want 2 (all chunks were missing):\n%s", n, stdout)
	}
	// resumed==0 → byte0→EOF 全程 → file_sha256 在 (§2 判据 = 续传块数, 非 fresh 参数)。
	if fileSHA := relayE2EFileSHA(t, stdout); fileSHA != relayE2ESHAFile(t, src) {
		t.Fatalf("file_sha256 = %s, want the source sha", fileSHA)
	}
	relayE2EAssertBytes(t, to, srcData)
	relayE2EAssertGone(t, to)
}

// ---------- 锚 4: 清单后 partial 前中断 → 自愈态直走 ----------

// 中断形态 = §3 r3 盘上残留 (manifest(chunks=[]) 在, partial 无) 直栽——该窗口在
// 引擎内只有 µs 级 (两次 sftp 小包), 无法定时注入; admission 半边由 T4 状态表
// r3 行 (relay_test.go TestRelayForProfileStateTable) 钉死, 这里补真实引擎完成段:
// 自愈态直走 = 补建 partial → 全块搬运 → 提交。
func TestE2ERelayFileSelfHealManifestNoPartial(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	srcData := strings.Repeat("a", 2*relayBigChunk)
	src := e.root + "/selfheal/src.bin"
	relayMkFile(t, src, srcData)
	to := e.root + "/selfheal/f.bin"
	relayPutManifest(t, to+relayManifestSuffix, relayRealManifest(t, src, relayBigChunk, nil)) // chunks=[]

	out := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	if out.ResumedChunks != 0 || out.ChunksTotal != 2 {
		t.Fatalf("out = %+v, want 0 resumed of 2 (self-heal shape)", out)
	}
	stdout, status, _ := relayE2EPoll(t, s, out.TaskID, 60*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}
	// 自愈态 resumed==0 → byte0→EOF 全程 → file_sha256 在 (§2 kimi#9 两可判据)。
	if fileSHA := relayE2EFileSHA(t, stdout); fileSHA != relayE2ESHAFile(t, src) {
		t.Fatalf("file_sha256 = %s, want the source sha", fileSHA)
	}
	relayE2EAssertBytes(t, to, srcData)
	relayE2EAssertGone(t, to)
}

// ---------- 锚 6: zero-byte 端到端 (MCP 级) ----------

func TestE2ERelayFileZeroByte(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	srcLocal := filepath.Join(e.local, "zb", "src.bin")
	relayMkFile(t, filepath.ToSlash(srcLocal), "")
	to := e.root + "/zb/f.bin"

	out := relayE2ECallOK(t, s, e.input("", srcLocal, to, false))
	if out.BytesTotal != 0 || out.ChunksTotal != 0 || out.ResumedChunks != 0 {
		t.Fatalf("out = %+v, want all-zero plan", out)
	}
	stdout, status, _ := relayE2EPoll(t, s, out.TaskID, 30*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done (zero-byte transfer)", status)
	}
	empty := sha256.Sum256(nil)
	emptyHex := hex.EncodeToString(empty[:])
	for _, frag := range []string{
		"relay plan: 0 bytes, 0 chunks (resumed 0), chunk=16777216",
		"relay done: root=sha256:" + emptyHex,
		"file_sha256=sha256:" + emptyHex + "(total=0)",
		"renamed -> " + to,
	} {
		if !strings.Contains(stdout, frag) {
			t.Fatalf("zero-byte stdout missing %q:\n%s", frag, stdout)
		}
	}
	if fi, serr := os.Stat(filepath.FromSlash(to)); serr != nil || fi.Size() != 0 {
		t.Fatalf("zero-byte real name: fi=%v err=%v", fi, serr)
	}
	relayE2EAssertGone(t, to)
}

// ---------- 锚 7: 本机源全流程 + 中断续传 + fresh 不触碰本机源 ----------

func TestE2ERelayFileLocalSourceFullFlowAndFreshUntouched(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	srcLocal := filepath.Join(e.local, "localsrc", "src.bin")
	srcSlash := filepath.ToSlash(srcLocal)
	srcData := strings.Repeat("a", 2*relayBigChunk)
	relayMkFile(t, srcSlash, srcData)
	to1 := e.root + "/locsrc/one/f.bin"
	to2 := e.root + "/locsrc/two/f.bin"

	// 全流程: 本机源 → 远程 (upload_file 1 MiB cap 债的根因销项路径), done +
	// file_sha256 + 字节精确。
	out1 := relayE2ECallOK(t, s, e.input("", srcLocal, to1, false))
	if out1.BytesTotal != int64(len(srcData)) || out1.ChunksTotal != 2 {
		t.Fatalf("out1 = %+v", out1)
	}
	stdout1, status, _ := relayE2EPoll(t, s, out1.TaskID, 60*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}
	if fileSHA := relayE2EFileSHA(t, stdout1); fileSHA != relayE2ESHAFile(t, srcSlash) {
		t.Fatalf("file_sha256 = %s, want the source sha", fileSHA)
	}
	relayE2EAssertBytes(t, to1, srcData)
	relayE2EAssertGone(t, to1)

	// 中断: 第二目标 chunk-0 后停 → manifest 恰含块 0。
	out2 := relayE2ECallOK(t, s, e.input("", srcLocal, to2, false))
	relayE2EWaitLine(t, s, out2.TaskID, "chunk 1/2 ok", 60*time.Second) // 1-based 首块进度行
	relayE2EStopNow(t, s, out2.TaskID, 30*time.Second)
	if m := relayE2EManifest(t, e, to2); len(m.Chunks) != 1 || m.Chunks[0].I != 0 {
		t.Fatalf("manifest chunks = %+v, want exactly chunk 0", m.Chunks)
	}

	// 续传 (非 fresh) 完成: 只补缺块。
	out3 := relayE2ECallOK(t, s, e.input("", srcLocal, to2, false))
	if out3.ResumedChunks != 1 {
		t.Fatalf("ResumedChunks = %d, want 1", out3.ResumedChunks)
	}
	stdout3, status, _ := relayE2EPoll(t, s, out3.TaskID, 60*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}
	if n := strings.Count(stdout3, " ok bytes="); n != 1 {
		t.Fatalf("chunk progress lines = %d, want 1 (only the missing chunk moves):\n%s", n, stdout3)
	}
	relayE2EAssertBytes(t, to2, srcData)
	relayE2EAssertGone(t, to2)

	// fresh 重跑同目标: 引擎的删除全部发生在远端, 本机源只读——传输前后本机源
	// 字节级比对 (rev4 kimi#6 锚: 第二大卖点"本机源不被 fresh 触碰")。
	out4 := relayE2ECallOK(t, s, e.input("", srcLocal, to2, true))
	if out4.ResumedChunks != 0 {
		t.Fatalf("fresh run ResumedChunks = %d, want 0", out4.ResumedChunks)
	}
	if _, status, _ := relayE2EPoll(t, s, out4.TaskID, 60*time.Second); status != bgStatusDone {
		t.Fatalf("fresh terminal = %q, want done", status)
	}
	relayE2EAssertBytes(t, srcSlash, srcData) // fresh 未触碰本机源
	relayE2EAssertBytes(t, to2, srcData)      // fresh 全量重传后目标仍字节精确
	relayE2EAssertGone(t, to2)
}

// ---------- 锚 9: broker restart 语义模拟 (任务表清空 → manifest 驱动续传完成) ----------

func TestE2ERelayFileBrokerRestartResumeCompletes(t *testing.T) {
	e := newRelayE2EEnv(t)
	s1 := e.openSession(t)
	srcData := strings.Repeat("a", 2*relayBigChunk)
	src := e.root + "/restart/src.bin"
	relayMkFile(t, src, srcData)
	to := e.root + "/restart/f.bin"

	out1 := relayE2ECallOK(t, s1, e.input(e.srcID, src, to, false))
	relayE2EWaitLine(t, s1, out1.TaskID, "chunk 1/2 ok", 60*time.Second) // 1-based 首块进度行
	relayE2EStopNow(t, s1, out1.TaskID, 30*time.Second)
	if m := relayE2EManifest(t, e, to); len(m.Chunks) != 1 || m.Chunks[0].I != 0 {
		t.Fatalf("manifest chunks = %+v, want exactly chunk 0", m.Chunks)
	}

	// "旧 broker 进程"退场: 会话关停 + 任务表 (TM1) CloseAll。重启语义的模拟点:
	// 任务表纯进程内, 新进程 = 新 TM = 空表; 唯一续传事实源回到 B 端 manifest。
	oldTaskID := out1.TaskID
	s1.close()
	s2 := e.openSession(t)

	// 旧 task_id 在新表 → unknown 三因文案 (逐字点名 broker restarted)。
	res, err := s2.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_output", Arguments: map[string]any{"task_id": oldTaskID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !textContains(res, "broker restarted") {
		t.Fatalf("stale task_id via the restarted broker must answer the unknown-task error naming the restart: %+v", res.Content)
	}

	// 同参数重跑 → manifest 驱动续传完成。
	out2 := relayE2ECallOK(t, s2, e.input(e.srcID, src, to, false))
	if out2.ResumedChunks != 1 {
		t.Fatalf("ResumedChunks = %d, want 1 (source read count = missing chunks)", out2.ResumedChunks)
	}
	stdout, status, _ := relayE2EPoll(t, s2, out2.TaskID, 60*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}
	if n := strings.Count(stdout, " ok bytes="); n != 1 {
		t.Fatalf("chunk progress lines = %d, want 1 (only the missing chunk moves):\n%s", n, stdout)
	}
	relayE2EAssertBytes(t, to, srcData)
	relayE2EAssertGone(t, to)
}

// ---------- 锚 10: no-leak 三面反向断言 ----------

func TestE2ERelayFileNoLeakThreeFaces(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	secret := "sk-live-S3CRIT-9f34ce2a1b7d4e5f8092aabbccdd"
	content := "-----BEGIN E2E FIXTURE-----\ntoken: " + secret + "\nregion: cn-fix-1\n-----END-----\n"
	srcLocal := filepath.Join(e.local, "noleak", "creds.txt")
	relayMkFile(t, filepath.ToSlash(srcLocal), content)
	to := e.root + "/noleak/f.bin"

	// 保留原始工具返回 (spec §6: 工具返回只回元数据)。
	out, res := relayE2ECall(t, s, e.input("", srcLocal, to, false))
	if res.IsError {
		t.Fatalf("relay must succeed: %+v", res.Content)
	}
	stdout, status, _ := relayE2EPoll(t, s, out.TaskID, 30*time.Second)
	if status != bgStatusDone {
		t.Fatalf("terminal = %q, want done", status)
	}

	var raw strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			raw.WriteString(tc.Text)
		}
	}
	for _, face := range []struct{ name, text string }{
		{"tool result JSON", raw.String()},
		{"exec_output stdout", stdout},
	} {
		if strings.Contains(face.text, secret) {
			t.Fatalf("%s leaks the fixture secret fragment", face.name)
		}
	}
	rows := relayAuditRows(t, e.st, 100)
	for _, row := range rows {
		if strings.Contains(fmt.Sprintf("%+v", row), secret) {
			t.Fatalf("audit row leaks the fixture secret fragment: %+v", row)
		}
	}
	// 正向对照: 传输确实完成 (反向断言不建立在空跑上)。
	relayE2EAssertBytes(t, to, content)
	relayE2EAssertGone(t, to)
}
