package conformance

// Plan 47 T7: relay_file 真实 OpenSSH conformance (spec §8 conformance 段)。
// 双重门控 requireConformance 同款 (SSHMGR_CONFORMANCE=1 + docker/ssh/ssh-keygen
// 在 PATH, 本地快速 lane 自动跳过)。
//
// 双服务器形态 = 两个独立 OpenSSH 容器 (A=源, B=目标), 被测通道是 broker 中继
// (A→broker→B)——mcpserver 套件对 in-process testsshd 的全部 relay e2e 在此换成
// 真 OpenSSH 线。入口 = 导出的 mcpserver.RelayForProfile + mcpserver.NewTaskManager:
// relay 没有绕开 preflight 的导出引擎入口, 种最小 vault (双 server 行 + 私钥凭据
// + host key + profile) 是最短生产形态 (后台三件套前例 TestBackgroundLifecycle-
// RealSSH 走 TaskManager seam 不种 vault, 是因为 exec 的引擎入口在 Start 内)。
// 审计行不在此断言 (mcpserver 单测面)。
//
// StatVFS unavailable 分支的 lane 划分 (spec §8): 真 Linux OpenSSH 的 sftp-server
// 实装 statvfs@openssh.com, 任何可达路径都产真字节——unavailable 只能来自扩展
// 缺席 (Windows 目标)。该分支的真线证据在 TestRelayWin32OpenSSHRealTarget
// (owner/CI 门, 缺真实 Win32-OpenSSH 端点即自动跳过); mcpserver 套件的 windows
// lane (pkg/sftp server 对 statvfs 恒 ENOTSUP) 已行为学覆盖 unavailable→照建。
// 本文件的双容器用例断言正向分支 SpaceCheck="ok"。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

const (
	relayConfSrcBytes int64 = 50 << 20 // 50MB 级往返
	relayConfChunk    int64 = 16 << 20 // 生产下限网格 (SSHMGR_TRANSFER_CHUNK 钳域下限)
)

// relayConfFileSHA 从 done 行提取 file_sha256 (mcpserver 侧同款正则的本地副本——
// 包边界, 不引未导出符号)。
var relayConfFileSHA = regexp.MustCompile(`file_sha256=sha256:([0-9a-f]{64})`)

// relayConfSeed 把一台真 OpenSSH 端点播进 store (私钥凭据 + host key), 返回行 id。
func relayConfSeed(t *testing.T, st *store.Store, name, host string, port int, hostKey ssh.PublicKey, privPath string) string {
	t.Helper()
	pem, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: pem})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: name, Host: host, Port: port, User: "sshuser",
		AuthMethod: models.AuthPrivateKey, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveHostKey(host, port, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	return id
}

// relayConfPattern 是确定性模式块 (upload_content conformance 同款配方)。
func relayConfPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// relayConfPatternReader 以 1MiB 模式块循环流式输出 total 字节 (50MiB 级 fixture
// 不整驻内存)。
type relayConfPatternReader struct {
	block []byte
	off   int64
	total int64
}

func newRelayConfPatternReader(total int64) *relayConfPatternReader {
	return &relayConfPatternReader{block: relayConfPattern(1 << 20), total: total}
}

func (r *relayConfPatternReader) Read(p []byte) (int, error) {
	if r.off >= r.total {
		return 0, io.EOF
	}
	written := 0
	for written < len(p) && r.off < r.total {
		pos := int(r.off % int64(len(r.block)))
		n := len(r.block) - pos
		if rem := len(p) - written; n > rem {
			n = rem
		}
		if rem := int(r.total - r.off); n > rem {
			n = rem
		}
		copy(p[written:written+n], r.block[pos:pos+n])
		written += n
		r.off += int64(n)
	}
	return written, nil
}

// relayConfWaitTerminal 经生产 TaskManager.Output 长轮询至任务离开 running,
// 返回收集的 stdout 全量与终态视图。
func relayConfWaitTerminal(t *testing.T, mgr *mcpserver.TaskManager, id string, budget time.Duration) (string, mcpserver.BgView) {
	t.Helper()
	var collected strings.Builder
	var off int64
	deadline := time.Now().Add(budget)
	for {
		v, ok, err := mgr.Output(id, off, 0, 10*time.Second, context.Background())
		if err != nil || !ok {
			t.Fatalf("output poll %s: ok=%v err=%v", id, ok, err)
		}
		collected.Write(v.Stdout)
		off = v.NextStdout
		if v.Status != "running" {
			return collected.String(), v
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never left running within %v:\n%s", id, budget, collected.String())
		}
	}
}

// relayConfAssertCommitted 断言真 OpenSSH 目标上提交语义成立: 三工件缺席 +
// 真名在场 (test -e / test -f 逐个, 经真线 exec)。
func relayConfAssertCommitted(t *testing.T, cli *sshbroker.Client, dst string) {
	t.Helper()
	ctx := context.Background()
	for _, artifact := range []string{dst + ".sshmgr-partial", dst + ".sshmgr-manifest.json", dst + ".sshmgr-manifest.json.tmp"} {
		res, err := cli.Exec(ctx, "test -e "+artifact, 30*time.Second, 0)
		if err != nil {
			t.Fatalf("test -e %s: %v", artifact, err)
		}
		if res.ExitCode == 0 {
			t.Fatalf("artifact %s must be gone after the commit", artifact)
		}
	}
	res, err := cli.Exec(ctx, "test -f "+dst, 30*time.Second, 0)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("real name %s must exist after the commit (err=%v exit=%d)", dst, err, res.ExitCode)
	}
}

// relayConfDualServers 起两个独立 OpenSSH 容器并播进同一 vault/profile, 返回
// 预连接好的 A/B 验证客户端 (mustPrivAuth 同一私钥)。
func relayConfDualServers(t *testing.T) (st *store.Store, mgr *mcpserver.TaskManager, rowA, rowB string, cliA, cliB *sshbroker.Client) {
	t.Helper()
	privPath, pub := generateKey(t, "ed25519", "")
	hostA, portA, hkA, _, cleanupA := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	t.Cleanup(cleanupA)
	hostB, portB, hkB, _, cleanupB := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	t.Cleanup(cleanupB)

	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err = store.Open(filepath.Join(t.TempDir(), "store.db"), mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	rowA = relayConfSeed(t, st, "relay-src-a", hostA, portA, hkA, privPath)
	rowB = relayConfSeed(t, st, "relay-dst-b", hostB, portB, hkB, privPath)
	pid, err := st.AddProfile("conf-relay")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(pid, []string{rowA, rowB}); err != nil {
		t.Fatal(err)
	}
	mgr, err = mcpserver.NewTaskManager()
	if err != nil {
		t.Fatalf("new task manager: %v", err)
	}
	t.Cleanup(func() { mgr.CloseAll() })

	ctx := context.Background()
	auth := mustPrivAuth(t, privPath, "")
	cliA, err = sshbroker.Connect(ctx, hostA, portA, "sshuser", auth, ssh.FixedHostKey(hkA))
	if err != nil {
		t.Fatalf("connect A: %v", err)
	}
	t.Cleanup(func() { cliA.Close() })
	cliB, err = sshbroker.Connect(ctx, hostB, portB, "sshuser", auth, ssh.FixedHostKey(hkB))
	if err != nil {
		t.Fatalf("connect B: %v", err)
	}
	t.Cleanup(func() { cliB.Close() })
	return st, mgr, rowA, rowB, cliA, cliB
}

// TestRelayDualServerRealSSH 是双服务器形态 50MB 级往返 (spec §8 conformance):
// 50MiB 确定性模式 seed 到容器 A → RelayForProfile A→B (16MiB×4 块) → 真线轮询
// 至 done → done 行 file_sha256 == 源 sha (seed 时 tee 计算) → B 上真 sha256sum
// 读回同值 → 提交语义 (三工件缺席 + 真名在场)。
func TestRelayDualServerRealSSH(t *testing.T) {
	requireConformance(t)
	st, mgr, rowA, rowB, cliA, cliB := relayConfDualServers(t)
	ctx := context.Background()

	srcPath := "/home/sshuser/plan47-relay-src.bin"
	hasher := sha256.New()
	if err := cliA.WriteFile(ctx, srcPath, io.TeeReader(newRelayConfPatternReader(relayConfSrcBytes), hasher)); err != nil {
		t.Fatalf("seed 50MiB source on A: %v", err)
	}
	t.Cleanup(func() { _, _ = cliA.Exec(ctx, "rm -f "+srcPath, 30*time.Second, 0) })
	srcSHA := hex.EncodeToString(hasher.Sum(nil))

	dstPath := "/home/sshuser/plan47-relay-dst.bin"
	t.Cleanup(func() {
		for _, p := range []string{dstPath, dstPath + ".sshmgr-partial", dstPath + ".sshmgr-manifest.json", dstPath + ".sshmgr-manifest.json.tmp"} {
			_, _ = cliB.Exec(ctx, "rm -f "+p, 30*time.Second, 0)
		}
	})

	out, err := mcpserver.RelayForProfile(ctx, st, mgr, "conf-relay-proj", mustProfile(t, st),
		mcpserver.RelayInput{FromServerID: rowA, FromPath: srcPath, ToServerID: rowB, ToPath: dstPath}, relayConfChunk)
	if err != nil {
		t.Fatalf("relay start: %v", err)
	}
	if out.BytesTotal != relayConfSrcBytes || out.ChunksTotal != 4 || out.ResumedChunks != 0 || out.ChunkBytes != relayConfChunk {
		t.Fatalf("RelayOutput = %+v, want 50MiB/4 chunks/0 resumed/16MiB grid", out)
	}
	if out.SpaceCheck != "ok" {
		t.Fatalf("SpaceCheck = %q, want ok — real Linux OpenSSH sftp-server implements statvfs@openssh.com", out.SpaceCheck)
	}

	stdout, v := relayConfWaitTerminal(t, mgr, out.TaskID, 300*time.Second)
	if v.Status != "done" || v.ExitCode != 0 {
		t.Fatalf("terminal = %q exit=%d err=%q, want done/0", v.Status, v.ExitCode, v.ErrText)
	}
	if n := strings.Count(stdout, " ok bytes="); n != 4 {
		t.Fatalf("chunk progress lines = %d, want 4:\n%s", n, stdout)
	}
	m := relayConfFileSHA.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no file_sha256 in the done line:\n%s", stdout)
	}
	if m[1] != srcSHA {
		t.Fatalf("file_sha256 = %s, want the seeded source sha %s", m[1], srcSHA)
	}
	// B 上真 sha256sum 读回 (conformance 的差异面: 服务端自己算, 不过 SFTP-get)。
	res, err := cliB.Exec(ctx, "sha256sum "+dstPath, 60*time.Second, 4096)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("sha256sum on B: err=%v exit=%d", err, res.ExitCode)
	}
	if !strings.Contains(res.Stdout, srcSHA) {
		t.Fatalf("server sha256 mismatch: out=%q want %q", res.Stdout, srcSHA)
	}
	relayConfAssertCommitted(t, cliB, dstPath)
}

// mustProfile 取回 relayConfDualServers 建的 "conf-relay" profile id。
func mustProfile(t *testing.T, st *store.Store) string {
	t.Helper()
	pids, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pids {
		if p.Name == "conf-relay" {
			return p.ID
		}
	}
	t.Fatal("profile conf-relay not found")
	return ""
}

// TestRelayResumeDrillRealSSH 是双服务器形态续传演练 (spec §8 conformance):
// 48MiB (16MiB×3) A→B, 首块进度行出现即 Stop (清单记账先于进度行落笔; 剩余块
// 的整块搬运窗远宽于 Stop 往返) → B 上真线 cat 清单核记账户数 → 同参数重跑 →
// 只补缺块 (进度行数 = 3−k) → sha256sum 对上 → 提交语义。
func TestRelayResumeDrillRealSSH(t *testing.T) {
	requireConformance(t)
	st, mgr, rowA, rowB, cliA, cliB := relayConfDualServers(t)
	ctx := context.Background()

	const srcBytes = int64(48 << 20) // 恰 3×16MiB
	srcPath := "/home/sshuser/plan47-relay-resume-src.bin"
	hasher := sha256.New()
	if err := cliA.WriteFile(ctx, srcPath, io.TeeReader(newRelayConfPatternReader(srcBytes), hasher)); err != nil {
		t.Fatalf("seed source on A: %v", err)
	}
	t.Cleanup(func() { _, _ = cliA.Exec(ctx, "rm -f "+srcPath, 30*time.Second, 0) })
	srcSHA := hex.EncodeToString(hasher.Sum(nil))

	dstPath := "/home/sshuser/plan47-relay-resume-dst.bin"
	t.Cleanup(func() {
		for _, p := range []string{dstPath, dstPath + ".sshmgr-partial", dstPath + ".sshmgr-manifest.json", dstPath + ".sshmgr-manifest.json.tmp"} {
			_, _ = cliB.Exec(ctx, "rm -f "+p, 30*time.Second, 0)
		}
	})

	in := mcpserver.RelayInput{FromServerID: rowA, FromPath: srcPath, ToServerID: rowB, ToPath: dstPath}
	out, err := mcpserver.RelayForProfile(ctx, st, mgr, "conf-relay-proj", mustProfile(t, st), in, relayConfChunk)
	if err != nil {
		t.Fatalf("relay start: %v", err)
	}

	// 注入: 紧轮询 (wait=0 快照 + 50ms) 至首块进度行即 Stop。首块记账先于进度行
	// 落笔 (relay 引擎逐块段), docker 线上 16MiB 块搬运 ≥ 数百 ms, 注入窗稳固。
	var collected strings.Builder
	var off int64
	injectDeadline := time.Now().Add(120 * time.Second)
	for {
		v, ok, oerr := mgr.Output(out.TaskID, off, 0, 0, ctx)
		if oerr != nil || !ok {
			t.Fatalf("output snapshot: ok=%v err=%v", ok, oerr)
		}
		collected.Write(v.Stdout)
		off = v.NextStdout
		if strings.Contains(collected.String(), "chunk 1/3 ok") {
			break
		}
		if v.Status != "running" {
			t.Fatalf("transfer reached %s before the injection point:\n%s", v.Status, collected.String())
		}
		if time.Now().After(injectDeadline) {
			t.Fatalf("first chunk line never appeared within 120s:\n%s", collected.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if trigger, ok := mgr.Stop(out.TaskID); !ok || trigger != "running" {
		t.Fatalf("Stop = (%q, %v), want (running, true)", trigger, ok)
	}
	_, v := relayConfWaitTerminal(t, mgr, out.TaskID, 60*time.Second)
	if v.Status != "stopped" {
		t.Fatalf("terminal = %q (err %q), want stopped", v.Status, v.ErrText)
	}

	// B 端 manifest 演练: 真线读取 + 解析, 记账户数 k ∈ [1, 3)。
	res, err := cliB.Exec(ctx, "cat "+dstPath+".sshmgr-manifest.json", 30*time.Second, 64<<10)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("manifest must survive the stop on B: err=%v exit=%d", err, res.ExitCode)
	}
	var mf sshbroker.RelayManifest
	if jerr := json.Unmarshal([]byte(res.Stdout), &mf); jerr != nil {
		t.Fatalf("parse remote manifest %q: %v", res.Stdout, jerr)
	}
	k := len(mf.Chunks)
	if k < 1 || k >= 3 {
		t.Fatalf("manifest records %d chunks, want 1..2 (first chunk recorded, transfer unfinished)", k)
	}
	res, err = cliB.Exec(ctx, "test -e "+dstPath+".sshmgr-partial", 30*time.Second, 0)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("partial must survive the stop on B (err=%v exit=%d)", err, res.ExitCode)
	}

	// 同参数重跑 → manifest 驱动续传, 只补缺块。
	out2, err := mcpserver.RelayForProfile(ctx, st, mgr, "conf-relay-proj", mustProfile(t, st), in, relayConfChunk)
	if err != nil {
		t.Fatalf("resume start: %v", err)
	}
	if out2.ResumedChunks != k {
		t.Fatalf("ResumedChunks = %d, want %d (the recorded count)", out2.ResumedChunks, k)
	}
	stdout, v := relayConfWaitTerminal(t, mgr, out2.TaskID, 300*time.Second)
	if v.Status != "done" || v.ExitCode != 0 {
		t.Fatalf("resume terminal = %q exit=%d err=%q, want done/0", v.Status, v.ExitCode, v.ErrText)
	}
	if n := strings.Count(stdout, " ok bytes="); n != 3-k {
		t.Fatalf("resume chunk progress lines = %d, want %d (only the missing chunks move):\n%s", n, 3-k, stdout)
	}
	res, err = cliB.Exec(ctx, "sha256sum "+dstPath, 60*time.Second, 4096)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("sha256sum on B after resume: err=%v exit=%d", err, res.ExitCode)
	}
	if !strings.Contains(res.Stdout, srcSHA) {
		t.Fatalf("server sha256 mismatch after resume: out=%q want %q", res.Stdout, srcSHA)
	}
	relayConfAssertCommitted(t, cliB, dstPath)
}

// TestRelayWin32OpenSSHRealTarget 是 owner/CI 门专属用例: 真 Win32-OpenSSH 目标
// (docker 起不了 windows 容器)。门控 = requireConformance + 四个端点 env, 缺任
// 一即 skip (本地零门自动跳)。锚 (spec §8 conformance):
//  1. posix-rename@openssh.com 真探测 = true——硬依赖的兼容面承诺 (spec §2:
//     "OpenSSH 全系含 Win32 端口支持该扩展") 的真线证据, 假了即 compat-matrix 需回改;
//  2. StatVFS 分支真形态: Windows 目标按预期报 unavailable → fail-open 照建并
//     完成; 若某版 Win32-OpenSSH 实装了 statvfs 则如实为 ok——两态都必须完成
//     传输 (fail-open 的真线证据, 分支形态记录在案不硬钉);
//  3. 本机源 → Win32 目标全流程字节精确 (真线 Download 回读比对)。
//
// env: SSHMGR_CONFORMANCE_WINSSH_ADDR=host:port, SSHMGR_CONFORMANCE_WINSSH_USER,
// SSHMGR_CONFORMANCE_WINSSH_KEY=path | SSHMGR_CONFORMANCE_WINSSH_PASSWORD=pw,
// 可选 SSHMGR_CONFORMANCE_WINSSH_DEST (默认 C:/Users/Public/plan47-relay-dst.bin)。
// host key 走 TOFU 首连即钉 (生产同款, 不预埋)。
func TestRelayWin32OpenSSHRealTarget(t *testing.T) {
	requireConformance(t)
	addr := os.Getenv("SSHMGR_CONFORMANCE_WINSSH_ADDR")
	user := os.Getenv("SSHMGR_CONFORMANCE_WINSSH_USER")
	keyPath := os.Getenv("SSHMGR_CONFORMANCE_WINSSH_KEY")
	password := os.Getenv("SSHMGR_CONFORMANCE_WINSSH_PASSWORD")
	if addr == "" || user == "" || (keyPath == "" && password == "") {
		t.Skip("real Win32-OpenSSH target not provisioned — set SSHMGR_CONFORMANCE_WINSSH_ADDR=host:port, SSHMGR_CONFORMANCE_WINSSH_USER, and either SSHMGR_CONFORMANCE_WINSSH_KEY=path or SSHMGR_CONFORMANCE_WINSSH_PASSWORD=pw (plus SSHMGR_CONFORMANCE=1)")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SSHMGR_CONFORMANCE_WINSSH_ADDR %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("SSHMGR_CONFORMANCE_WINSSH_ADDR %q: %v", addr, err)
	}
	dstPath := os.Getenv("SSHMGR_CONFORMANCE_WINSSH_DEST")
	if dstPath == "" {
		dstPath = "C:/Users/Public/plan47-relay-dst.bin"
	}

	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var cred *models.Credential
	var authMethod models.AuthMethod
	var auth ssh.AuthMethod
	switch {
	case keyPath != "":
		pem, rerr := os.ReadFile(keyPath)
		if rerr != nil {
			t.Fatal(rerr)
		}
		cred = &models.Credential{Type: models.CredPrivateKey, Secret: pem}
		authMethod = models.AuthPrivateKey
		auth, err = sshbroker.PrivateKeyAuth(pem, nil)
		if err != nil {
			t.Fatal(err)
		}
	default:
		cred = &models.Credential{Type: models.CredPassword, Secret: []byte(password)}
		authMethod = models.AuthPassword
		auth = sshbroker.PasswordAuth(password)
	}
	cid, err := st.SetCredential(cred)
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.AddServer(&models.Server{
		Name: "winssh-target", Host: host, Port: port, User: user,
		AuthMethod: authMethod, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.AddProfile("conf-winssh")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(pid, []string{row}); err != nil {
		t.Fatal(err)
	}
	hkCb, err := sshbroker.HostKeyTOFU(st, host, port)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := mcpserver.NewTaskManager()
	if err != nil {
		t.Fatalf("new task manager: %v", err)
	}
	defer mgr.CloseAll()
	ctx := context.Background()

	// ① posix-rename 真探测 (零 IO 内存查找, 对 SSH_FXP_VERSION 的扩展对表)。
	probeCli, err := sshbroker.Connect(ctx, host, port, user, auth, hkCb)
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	defer probeCli.Close()
	sc, err := probeCli.RelaySFTP()
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if !probeCli.RelayPosixRenameOK(sc) {
		t.Fatalf("real Win32-OpenSSH target %s does NOT advertise posix-rename@openssh.com — relay's hard dependency (spec §2: the OpenSSH port family supports it) is violated; the compat-matrix row needs revisiting", addr)
	}
	if _, availOK, verr := probeCli.RelayAvailable(sc, filepath.ToSlash(filepath.Dir(dstPath))); verr != nil || !availOK {
		t.Logf("StatVFS on the real Win32 target: ok=%v err=%v (expect unavailable on most Win32-OpenSSH builds)", availOK, verr)
	} else {
		t.Logf("StatVFS on the real Win32 target: available (this build implements statvfs@openssh.com)")
	}

	// ② 本机源 (1 MiB 确定性模式) → Win32 目标全流程。
	payload := relayConfPattern(1 << 20)
	localSrc := filepath.Join(t.TempDir(), "win-relay-src.bin")
	if werr := os.WriteFile(localSrc, payload, 0o644); werr != nil {
		t.Fatal(werr)
	}
	sum := sha256.Sum256(payload)
	srcSHA := hex.EncodeToString(sum[:])

	out, err := mcpserver.RelayForProfile(ctx, st, mgr, "conf-winssh", pid,
		mcpserver.RelayInput{FromPath: localSrc, ToServerID: row, ToPath: dstPath}, relayConfChunk)
	if err != nil {
		t.Fatalf("relay start: %v", err)
	}
	// StatVFS 分支形态记录 (不硬钉——见头注 2); 照建本身即 fail-open 的证据。
	t.Logf("SpaceCheck reported by RelayForProfile against the real Win32 target: %q", out.SpaceCheck)
	if out.SpaceCheck != "ok" && out.SpaceCheck != "unavailable" {
		t.Fatalf("SpaceCheck = %q, want ok|unavailable", out.SpaceCheck)
	}
	stdout, v := relayConfWaitTerminal(t, mgr, out.TaskID, 120*time.Second)
	if v.Status != "done" || v.ExitCode != 0 {
		t.Fatalf("terminal = %q exit=%d err=%q, want done/0", v.Status, v.ExitCode, v.ErrText)
	}
	if !bytes.Contains([]byte(stdout), []byte("file_sha256=sha256:"+srcSHA)) {
		t.Fatalf("done line must carry the source file_sha256 %s:\n%s", srcSHA, stdout)
	}

	// ③ 真线 Download 回读字节精确 + 目标侧工件清扫 (best-effort)。
	verifyCli, err := sshbroker.Connect(ctx, host, port, user, auth, hkCb)
	if err != nil {
		t.Fatalf("verify connect: %v", err)
	}
	defer verifyCli.Close()
	dl, err := verifyCli.Download(ctx, dstPath, 0)
	if err != nil {
		t.Fatalf("download back %s: %v", dstPath, err)
	}
	if dl.Truncated || dl.Bytes != int64(len(payload)) || !bytes.Equal([]byte(dl.Content), payload) {
		t.Fatalf("round trip mismatch: bytes=%d truncated=%v", dl.Bytes, dl.Truncated)
	}
	t.Cleanup(func() {
		vsc, serr := verifyCli.RelaySFTP()
		if serr != nil {
			return
		}
		defer vsc.Close()
		for _, p := range []string{dstPath, dstPath + ".sshmgr-partial", dstPath + ".sshmgr-manifest.json", dstPath + ".sshmgr-manifest.json.tmp"} {
			_ = vsc.Remove(p)
		}
	})
}
