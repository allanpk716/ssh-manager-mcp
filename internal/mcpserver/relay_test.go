package mcpserver

// Plan 47 T4: RelayForProfile preflight 分支测试 (spec §2 ①–⑨ / §3 14 行状态表 /
// §8 单测段)。testsshd 的 sftp 子系统服务宿主 FS (core_test.go 同款), 因此
// "远端" 工件 (partial/manifest/真名) 用 os.WriteFile 直栽;"远端" stat 走真
// SFTP。Windows 宿主上 pkg/sftp 的 sftp server 对 statvfs 恒回 ENOTSUP (T1
// relay_test.go 已证), 所以空间不足/投影分支的**真字节**用例沿用 T1 先例标注
// linux CI lane;Windows lane 携带 fail-open (unavailable→照建) 接线。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/vault"
)

// relayProfileChunk 是 RelayForProfile 测试的块网格 (relay_testChunk 的
// mcpserver 侧同位物)。
const relayProfileChunk int64 = 64 << 10

// relaySpaceChunk 是空间分支真字节用例的块大小: 16TiB 稀疏源 ÷ 256MiB = 64 块,
// 远低于 relayMaxChunks 闸, 让块数闸不先于空间分支触发。
const relaySpaceChunk int64 = 256 << 20

// relaySparseSpaceSize 是空间分支的稀疏源体积: 任何真实 FS 的可用空间都远低于
// 它 (不足分支必然拒), 而 Truncate 稀疏扩展零实际占用 (双平台已证)。
const relaySparseSpaceSize int64 = 16 << 40

// ---------- fixtures ----------

type relayEnv struct {
	st     *store.Store
	tm     *TaskManager
	pid    string
	srcID  string // 入 profile 的源服务器行 → testsshd
	dstID  string // 入 profile 的目标服务器行 → 同一 testsshd
	root   string // slash 形式宿主 FS 根 (两个"远端"共享)
	local  string // root 的宿主路径形式 (本机源 fixture 用)
	projID string
	addr   string
	hk     ssh.PublicKey
}

// relayNewEnv 起一个 testsshd, 播两行同址服务器行 (src/dst) 并双双授权,
// 配白盒 TaskManager (CloseAll 挂 Cleanup 收口阻塞 blocker)。
func relayNewEnv(t *testing.T) *relayEnv {
	t.Helper()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	st := newStore(t)
	srcID := seedRealServer(t, st, "rsrc", addr, hk, "")
	dstID := seedRealServer(t, st, "rdst", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srcID, dstID})
	tm := newTestTM(t, 8)
	t.Cleanup(func() { tm.CloseAll() })
	root := toSlash(t.TempDir())
	return &relayEnv{
		st: st, tm: tm, pid: pid, srcID: srcID, dstID: dstID,
		root: root, local: filepath.FromSlash(root),
		projID: "proj-relay", addr: addr, hk: hk,
	}
}

// relayInput 组标准入参 (局部覆盖用)。
func (e *relayEnv) relayInput(fromSrv, fromPath, toPath string, fresh bool) RelayInput {
	return RelayInput{FromServerID: fromSrv, FromPath: fromPath, ToServerID: e.dstID, ToPath: toPath, Fresh: fresh}
}

func (e *relayEnv) relayCall(in RelayInput) (RelayOutput, error) {
	return RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid, in, relayProfileChunk)
}

// relayMkFile 写宿主 FS 文件 (slash 路径直写 — Go 的 os 层双平台都收 /)。
func relayMkFile(t *testing.T, slashPath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.FromSlash(slashPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.FromSlash(slashPath), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// relayMkSource 写 size 字节的确定性源文件, 返回其 slash 路径。
func relayMkSource(t *testing.T, e *relayEnv, rel string, size int64) string {
	t.Helper()
	p := e.root + "/" + rel
	relayMkFile(t, p, strings.Repeat("a", int(size)))
	return p
}

// relayMkSparse 稀疏扩展一个 size 字节的文件 (零实际占用, size 元数据真)。
func relayMkSparse(t *testing.T, slashPath string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.FromSlash(slashPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.FromSlash(slashPath), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// relayStatSource 读源文件的 (size, mtime unix 秒) — manifest fixture 的锚。
func relayStatSource(t *testing.T, slashPath string) (int64, int64) {
	t.Helper()
	fi, err := os.Stat(filepath.FromSlash(slashPath))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size(), fi.ModTime().Unix()
}

func relayHex(i int) string { return fmt.Sprintf("%064x", i) }

// relayManifestFor 组一个结构合法、锚定当前源 stat 的 manifest;idxs 为已完成块
// 下标 (洞允许)。
func relayManifestFor(t *testing.T, srcSlash string, chunkBytes int64, idxs []int) *sshbroker.RelayManifest {
	t.Helper()
	size, mtime := relayStatSource(t, srcSlash)
	chunks := make([]sshbroker.RelayChunkDone, 0, len(idxs))
	for _, i := range idxs {
		chunks = append(chunks, sshbroker.RelayChunkDone{I: i, SHA256: relayHex(i)})
	}
	return &sshbroker.RelayManifest{
		Version: 1, ChunkBytes: chunkBytes, SourceSize: size, SourceMtimeUnix: mtime, Chunks: chunks,
	}
}

func relayPutManifest(t *testing.T, slashPath string, m *sshbroker.RelayManifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	relayMkFile(t, slashPath, string(b))
}

func relayPutRaw(t *testing.T, slashPath, content string) { relayMkFile(t, slashPath, content) }

func relayAuditRows(t *testing.T, st *store.Store, n int) []store.AuditRow {
	t.Helper()
	rows, err := st.AuditRows(n)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return rows
}

// relayAuditFind 找 action+status 匹配的审计行。
func relayAuditFind(t *testing.T, rows []store.AuditRow, action, status string) *store.AuditRow {
	t.Helper()
	for i := range rows {
		if rows[i].Action == action && rows[i].Status == status {
			return &rows[i]
		}
	}
	t.Fatalf("no audit row action=%q status=%q; rows=%+v", action, status, rows)
	return nil
}

// ---------- ① 参数层 ----------

func TestRelayForProfileParamRejections(t *testing.T) {
	e := relayNewEnv(t)

	cases := []struct {
		name    string
		in      RelayInput
		wantSub string
	}{
		{"empty to_server_id", RelayInput{FromServerID: e.dstID, FromPath: "/data/s", ToServerID: "", ToPath: "/data/d"}, "to_server_id"},
		{"relative remote from_path", e.relayInput(e.dstID, "rel/src.bin", "/data/d", false), "absolute path"},
		{"relative to_path", e.relayInput(e.dstID, "/data/s", "rel/d.bin", false), "absolute path"},
		{"empty from_path", e.relayInput(e.dstID, "", "/data/d", false), "absolute path"},
		{"empty to_path", e.relayInput(e.dstID, "/data/s", "", false), "absolute path"},
		{"local source relative path", e.relayInput("", "rel/src.bin", "/data/d", false), "absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.relayCall(tc.in)
			if err == nil {
				t.Fatalf("want param rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}

	// 本机源 `/foo` 形态按 broker 宿主 OS 语义判 (spec §1.1): Windows 上假 → 拒。
	if runtime.GOOS == "windows" {
		t.Run("local /foo not abs on windows broker", func(t *testing.T) {
			_, err := e.relayCall(e.relayInput("", "/foo/src.bin", "/data/d", false))
			if err == nil || !strings.Contains(err.Error(), "absolute path") {
				t.Fatalf("err = %v, want absolute-path rejection", err)
			}
		})
	}
}

// 同源同径: canonical 后比较, `/a/./b`、`/a/f/` 不可绕 (spec §1.1 rev3)。
func TestRelayForProfileSamePathCanonicalRejects(t *testing.T) {
	e := relayNewEnv(t)

	for _, tc := range []struct {
		name, to string
	}{
		{"identical", "/data/f"},
		{"dot-segment bypass", "/data/./f"},
		{"double-slash bypass", "/data//f"},
		{"trailing-slash bypass", "/data/f/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.relayCall(e.relayInput(e.dstID, "/data/f", tc.to, false))
			if err == nil || !strings.Contains(err.Error(), "same source and destination path") {
				t.Fatalf("err = %v, want same-path rejection", err)
			}
		})
	}
}

// own read ∩ own write = ∅ (spec §2①, rev3 codex#2): 远程源路径 ∈ 自身四工件 → 拒。
func TestRelayForProfileOwnReadWriteRejects(t *testing.T) {
	e := relayNewEnv(t)

	for _, tc := range []struct {
		name, from string
	}{
		{"source is own partial", "/data/f.sshmgr-partial"},
		{"source is own manifest", "/data/f.sshmgr-manifest.json"},
		{"source is own manifest tmp", "/data/f.sshmgr-manifest.json.tmp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.relayCall(e.relayInput(e.dstID, tc.from, "/data/f", false))
			if err == nil || !strings.Contains(err.Error(), "own destination artifact") {
				t.Fatalf("err = %v, want own read∩write rejection", err)
			}
		})
	}

	// 反例锚 (rev4 kimi#1 的 ① 侧): 跨服务器同路径工件不是自己的源 → 不拒。
	// (真建任务: 源文件与目标真名物理同址 — preflight 零内容移动, 合法。)
	src := relayMkSource(t, e, "xsrc.bin", 1024)
	out, err := e.relayCall(e.relayInput(e.srcID, src, e.root+"/xonly/f.bin", false))
	if err != nil {
		t.Fatalf("cross-server artifact-shaped source must pass ①: %v", err)
	}
	if out.TaskID == "" {
		t.Fatal("TaskID empty")
	}
}

// ---------- ② gate ----------

func TestRelayForProfileDeniedBothEnds(t *testing.T) {
	e := relayNewEnv(t)
	outsider := seedRealServer(t, e.st, "outsider", e.addr, e.hk, "") // 不授权

	// (a) dest 越权 (源在 profile)。
	_, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		RelayInput{FromServerID: e.srcID, FromPath: "/data/s", ToServerID: "bogus-dest", ToPath: "/data/d"}, relayProfileChunk)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("dest denied: want ErrNotInProfile, got %v", err)
	}
	rows := relayAuditRows(t, e.st, 10)
	row := relayAuditFind(t, rows, "relay-bg-start", "denied")
	if row.ServerID != "bogus-dest" {
		t.Fatalf("dest-denied row ServerID = %q, want bogus-dest", row.ServerID)
	}

	// (b) source 越权 (dest 在 profile)。
	_, err = e.relayCall(e.relayInput(outsider, "/data/s", "/data/d", false))
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("source denied: want ErrNotInProfile, got %v", err)
	}
	rows = relayAuditRows(t, e.st, 10)
	row = relayAuditFind(t, rows, "relay-bg-start", "denied")
	if row.ServerID != outsider {
		t.Fatalf("source-denied row ServerID = %q, want the offending endpoint %q", row.ServerID, outsider)
	}

	// (c) 本机源 + dest 越权。
	_, err = RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		RelayInput{FromPath: e.local + "/s.bin", ToServerID: outsider, ToPath: "/data/d"}, relayProfileChunk)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("local-source dest denied: want ErrNotInProfile, got %v", err)
	}
}

// ---------- ③ 源 stat 词汇表 ----------

func TestRelayForProfileSourceStatBranches(t *testing.T) {
	e := relayNewEnv(t)
	to := e.root + "/srcbranch/f.bin"

	// 远程源不存在。
	_, err := e.relayCall(e.relayInput(e.srcID, e.root+"/nope/src.bin", to, false))
	if err == nil || !strings.Contains(err.Error(), "source stat") {
		t.Fatalf("remote missing: err = %v, want source-stat failure", err)
	}
	// 远程源是目录。
	relayMkFile(t, e.root+"/srcbranch/adir/keep.txt", "x")
	_, err = e.relayCall(e.relayInput(e.srcID, e.root+"/srcbranch/adir", to, false))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("remote dir: err = %v, want not-a-regular-file", err)
	}
	// 本机源不存在 / 目录。
	_, err = e.relayCall(e.relayInput("", filepath.Join(e.local, "nope.bin"), to, false))
	if err == nil || !strings.Contains(err.Error(), "source stat") {
		t.Fatalf("local missing: err = %v, want source-stat failure", err)
	}
	_, err = e.relayCall(e.relayInput("", e.local, to, false))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("local dir: err = %v, want not-a-regular-file", err)
	}

	// no_credential (源端; Plan 20 C0 词汇)。
	bareID, berr := e.st.AddServer(&models.Server{Name: "bare-src", Host: "192.0.2.7", Port: 22, User: "u"})
	if berr != nil {
		t.Fatal(berr)
	}
	_ = e.st.GrantServers(e.pid, []string{bareID})
	_, err = e.relayCall(e.relayInput(bareID, "/data/s", to, false))
	if !errors.Is(err, vault.ErrNoCredential) {
		t.Fatalf("source no_credential: want vault.ErrNoCredential, got %v", err)
	}
	row := relayAuditFind(t, relayAuditRows(t, e.st, 20), "relay-bg-start", "no_credential")
	if row.ServerID != bareID {
		t.Fatalf("no_credential row ServerID = %q, want %q", row.ServerID, bareID)
	}

	// hostkey_mismatch (源端连接分支; 预埋垃圾 host key 真触发)。
	hmID := seedRealServer(t, e.st, "hm-src", e.addr, e.hk, "")
	_ = e.st.GrantServers(e.pid, []string{hmID})
	src := relayMkSource(t, e, "hmsrc.bin", 16)
	_ = e.st.SaveHostKey(hostOfAddr(e.addr), portOfAddr(e.addr), []byte("not-the-real-host-key"))
	_, err = e.relayCall(e.relayInput(hmID, src, to, false))
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("source hostkey: err = %v, want host key mismatch", err)
	}
	relayAuditFind(t, relayAuditRows(t, e.st, 30), "relay-bg-start", "hostkey_mismatch")

	// connect_error (源端; 指向必拒端口)。
	deadID := seedRealServer(t, e.st, "dead-src", "127.0.0.1:1", e.hk, "")
	_ = e.st.GrantServers(e.pid, []string{deadID})
	_, err = e.relayCall(e.relayInput(deadID, "/data/s", to, false))
	if err == nil || !strings.Contains(err.Error(), "ssh dial") {
		t.Fatalf("source connect: err = %v, want ssh dial failure", err)
	}
	relayAuditFind(t, relayAuditRows(t, e.st, 40), "relay-bg-start", "connect_error")

	// cancelled (源端连接分支; 预取消 ctx → Connect 的 ctx.Err() 快速返回)。
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	_, err = RelayForProfile(cctx, e.st, e.tm, e.projID, e.pid,
		e.relayInput(deadID, "/data/s", to, false), relayProfileChunk)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: want context.Canceled, got %v", err)
	}
	relayAuditFind(t, relayAuditRows(t, e.st, 50), "relay-bg-start", "cancelled")
}

// ---------- ④⑤ dest 连接/Stat 分支 ----------

func TestRelayForProfileDestBranches(t *testing.T) {
	e := relayNewEnv(t)
	src := relayMkSource(t, e, "dsrc.bin", 16)

	// ⑤: to_path 是目录 → refusal。(必须在 hostkey 污染用例之前——host key 按
	// host:port 存储, 同址的服务器行共享同一条目。)
	relayMkFile(t, e.root+"/dstdir/keep.txt", "x")
	_, err := e.relayCall(e.relayInput(e.srcID, src, e.root+"/dstdir", false))
	if err == nil || !strings.Contains(err.Error(), "is an existing directory") {
		t.Fatalf("dest dir: err = %v, want directory refusal", err)
	}
	relayAuditFind(t, relayAuditRows(t, e.st, 20), "relay-bg-start", "error")

	// dest no_credential。
	bareID, berr := e.st.AddServer(&models.Server{Name: "bare-dst", Host: "192.0.2.7", Port: 22, User: "u"})
	if berr != nil {
		t.Fatal(berr)
	}
	_ = e.st.GrantServers(e.pid, []string{bareID})
	_, err = RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		RelayInput{FromServerID: e.srcID, FromPath: src, ToServerID: bareID, ToPath: "/data/d"}, relayProfileChunk)
	if !errors.Is(err, vault.ErrNoCredential) {
		t.Fatalf("dest no_credential: want vault.ErrNoCredential, got %v", err)
	}
	row := relayAuditFind(t, relayAuditRows(t, e.st, 10), "relay-bg-start", "no_credential")
	if row.ServerID != bareID {
		t.Fatalf("dest no_credential row ServerID = %q, want %q", row.ServerID, bareID)
	}

	// dest hostkey_mismatch (放最后: 覆写同址共享的 host key 条目)。
	hmID := seedRealServer(t, e.st, "hm-dst", e.addr, e.hk, "")
	_ = e.st.GrantServers(e.pid, []string{hmID})
	_ = e.st.SaveHostKey(hostOfAddr(e.addr), portOfAddr(e.addr), []byte("not-the-real-host-key"))
	_, err = RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		RelayInput{FromServerID: e.srcID, FromPath: src, ToServerID: hmID, ToPath: "/data/d"}, relayProfileChunk)
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("dest hostkey: err = %v, want host key mismatch", err)
	}
}

// ---------- ③b 块数闸 (溢出安全) ----------

func TestRelayChunkGateOverflowSafety(t *testing.T) {
	// 溢出安全形态锚: n = size/chunk, 余数进位 — 绝不 (size+chunk-1)/chunk。
	if got := relayCeilDiv(0, 16); got != 0 {
		t.Fatalf("ceilDiv(0,16)=%d, want 0", got)
	}
	if got := relayCeilDiv(32, 16); got != 2 {
		t.Fatalf("ceilDiv(32,16)=%d, want 2", got)
	}
	if got := relayCeilDiv(33, 16); got != 3 {
		t.Fatalf("ceilDiv(33,16)=%d, want 3", got)
	}
	// MaxInt64 级: 不 panic、不求值为负、必拒。
	for _, chunk := range []int64{3, 16 << 20, 1 << 30} {
		n, err := relayChunkGate(1<<63-1, chunk)
		if err == nil {
			t.Fatalf("chunk=%d: MaxInt64 size must be refused", chunk)
		}
		if n < 0 {
			t.Fatalf("chunk=%d: chunk count went negative (%d) — overflow", chunk, n)
		}
	}
	// 边界: 恰 16384 块过闸, 多一字节即拒; 错误文本带两值 + 指引。
	exact := relayMaxChunks * (16 << 20)
	if n, err := relayChunkGate(exact, 16<<20); err != nil || n != relayMaxChunks {
		t.Fatalf("boundary: n=%d err=%v, want %d nil", n, err, relayMaxChunks)
	}
	_, err := relayChunkGate(exact+1, 16<<20)
	if err == nil {
		t.Fatal("one byte over the boundary must be refused")
	}
	for _, frag := range []string{"16384", "raise SSHMGR_TRANSFER_CHUNK"} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("gate error must carry %q: %q", frag, err.Error())
		}
	}
	// 负 size 拒。
	if _, err := relayChunkGate(-1, 16<<20); err == nil {
		t.Fatal("negative size must be refused")
	}
}

// 真链路块数闸: 稀疏大源 × 小块 → refusal (双平台; 稀疏 Truncate 零实际占用)。
func TestRelayForProfileChunkGateLive(t *testing.T) {
	e := relayNewEnv(t)
	src := e.root + "/gatesparse/src.bin"
	relayMkSparse(t, src, 300<<30) // 300GiB ÷ 64KiB ≫ 16384

	_, err := e.relayCall(e.relayInput(e.srcID, src, e.root+"/gatesparse/f.bin", false))
	if err == nil || !strings.Contains(err.Error(), "raise SSHMGR_TRANSFER_CHUNK") {
		t.Fatalf("live gate: err = %v, want chunk-count refusal with guidance", err)
	}

	// 反向锚: 300GiB ÷ 32MiB = 9600 块 ≤ 16384 → 过闸建任务。
	out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		e.relayInput(e.srcID, src, e.root+"/gatesparse/ok.bin", false), 32<<20)
	if err != nil {
		t.Fatalf("9600 chunks must pass the gate: %v", err)
	}
	if out.ChunksTotal != 9600 {
		t.Fatalf("ChunksTotal = %d, want 9600", out.ChunksTotal)
	}
}

// ---------- ⑥ manifest 结构防御 ----------

func TestRelayForProfileManifestRejects(t *testing.T) {
	e := relayNewEnv(t)
	src := relayMkSource(t, e, "mreject/src.bin", 2*relayProfileChunk+relayProfileChunk/2) // n=3
	_, mtime := relayStatSource(t, src)

	build := func(m *sshbroker.RelayManifest) string {
		b, _ := json.Marshal(m)
		return string(b)
	}
	badHex := strings.Repeat("z", 64)

	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{"version 2", build(&sshbroker.RelayManifest{Version: 2, ChunkBytes: relayProfileChunk, SourceSize: 1, SourceMtimeUnix: mtime}), "version"},
		{"chunk_bytes mismatch", build(&sshbroker.RelayManifest{Version: 1, ChunkBytes: relayProfileChunk / 2, SourceSize: 1, SourceMtimeUnix: mtime}), "SSHMGR_TRANSFER_CHUNK"},
		{"size mismatch", build(&sshbroker.RelayManifest{Version: 1, ChunkBytes: relayProfileChunk, SourceSize: 99, SourceMtimeUnix: mtime}), "changed"},
		{"mtime mismatch", build(&sshbroker.RelayManifest{Version: 1, ChunkBytes: relayProfileChunk, SourceSize: 2*relayProfileChunk + relayProfileChunk/2, SourceMtimeUnix: mtime + 7}), "changed"},
		{"negative index", build(&sshbroker.RelayManifest{Version: 1, ChunkBytes: relayProfileChunk, SourceSize: 1, SourceMtimeUnix: mtime, Chunks: []sshbroker.RelayChunkDone{{I: -1, SHA256: relayHex(0)}}}), "index"},
		{"duplicate index", build(&sshbroker.RelayManifest{Version: 1, ChunkBytes: relayProfileChunk, SourceSize: 1, SourceMtimeUnix: mtime, Chunks: []sshbroker.RelayChunkDone{{I: 0, SHA256: relayHex(0)}, {I: 0, SHA256: relayHex(0)}}}), "duplicate"},
		{"out-of-range index", build(&sshbroker.RelayManifest{Version: 1, ChunkBytes: relayProfileChunk, SourceSize: 1, SourceMtimeUnix: mtime, Chunks: []sshbroker.RelayChunkDone{{I: 7, SHA256: relayHex(7)}}}), "index"},
		{"bad sha256 hex", build(&sshbroker.RelayManifest{Version: 1, ChunkBytes: relayProfileChunk, SourceSize: 1, SourceMtimeUnix: mtime, Chunks: []sshbroker.RelayChunkDone{{I: 0, SHA256: badHex}}}), "sha256"},
		{"chunks not an array", `{"version":1,"chunk_bytes":` + fmt.Sprint(relayProfileChunk) + `,"source_size":1,"source_mtime_unix":` + fmt.Sprint(mtime) + `,"chunks":{"0":"aa"}}`, "parse manifest"},
		{"unparsable json", "not json at all {", "parse manifest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := e.root + "/mreject/case"
			to := dir + "/f.bin"
			relayPutRaw(t, to+".sshmgr-manifest.json", tc.content)
			_, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
			if err == nil {
				t.Fatalf("want manifest rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}

	// 推导上限拒: n=3 → cap = 3×128B+8KiB; 20KB 垃圾清单远超 → 拒 (解析前)。
	t.Run("over derived cap", func(t *testing.T) {
		to := e.root + "/mreject/cap/f.bin"
		relayPutRaw(t, to+".sshmgr-manifest.json", strings.Repeat("x", 20<<10))
		_, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
		if err == nil || !strings.Contains(err.Error(), "over the derived limit") {
			t.Fatalf("err = %v, want over-cap rejection", err)
		}
	})

	// manifest 落成了目录 → 拒。
	t.Run("manifest is a directory", func(t *testing.T) {
		to := e.root + "/mreject/asdir/f.bin"
		relayMkFile(t, to+".sshmgr-manifest.json/keep.txt", "x")
		_, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want not-a-regular-file rejection", err)
		}
	})
}

// partial 结构校验: 非常规文件拒 / size < 最高完成块末尾拒 (manifest 谎报)。
func TestRelayForProfilePartialStructural(t *testing.T) {
	e := relayNewEnv(t)
	src := relayMkSource(t, e, "pstruct/src.bin", 2*relayProfileChunk+relayProfileChunk/2) // n=3

	t.Run("partial is a directory", func(t *testing.T) {
		to := e.root + "/pstruct/asdir/f.bin"
		relayPutManifest(t, to+".sshmgr-manifest.json", relayManifestFor(t, src, relayProfileChunk, []int{0, 1, 2}))
		relayMkFile(t, to+".sshmgr-partial/keep.txt", "x")
		_, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want partial not-a-regular-file rejection", err)
		}
	})

	t.Run("partial shorter than highest completed chunk end", func(t *testing.T) {
		to := e.root + "/pstruct/short/f.bin"
		relayPutManifest(t, to+".sshmgr-manifest.json", relayManifestFor(t, src, relayProfileChunk, []int{0, 1, 2}))
		relayMkFile(t, to+".sshmgr-partial", strings.Repeat("a", 2*int(relayProfileChunk))) // chunk 2 末尾 = 2.5 chunk > 2 chunk
		_, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
		if err == nil || !strings.Contains(err.Error(), "partial") {
			t.Fatalf("err = %v, want partial-too-short rejection", err)
		}
	})
}

// ---------- ⑥ §3 状态表 14 行逐行 ----------

func TestRelayForProfileStateTable(t *testing.T) {
	e := relayNewEnv(t)
	const srcSize = 2*relayProfileChunk + relayProfileChunk/2 // n=3

	type stateRow struct {
		name        string
		srcSize     int64
		manifest    func(src string) *sshbroker.RelayManifest // nil = 无 manifest
		manifestRaw string                                    // 非空则原样落盘 (覆盖 manifest)
		partial     bool
		real        bool
		wantErr     string // "" = 建任务成功
		wantResumed int64
	}

	rows := []stateRow{
		{name: "r1 first run (M:no P:no R:no)", srcSize: srcSize, wantResumed: 0},
		{name: "r2 partial without manifest (M:no P:yes R:no)", srcSize: srcSize, partial: true, wantErr: "fresh=true"},
		{name: "r3 empty manifest self-heal (M:chunks=[] P:no R:no)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest { return relayManifestFor(t, src, relayProfileChunk, nil) }, wantResumed: 0},
		{name: "r4 incomplete manifest no partial (M:chunks!=[] P:no R:no)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0})
			}, wantErr: "fresh=true"},
		{name: "r5 all complete commit-only (M:all P:no R:no)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0, 1, 2})
			}, wantResumed: 3},
		{name: "r6 resume with partial (M:yes P:yes R:no)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0, 1})
			},
			partial: true, wantResumed: 2},
		{name: "r6b resume with holes", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0, 2})
			},
			partial: true, wantResumed: 2},
		{name: "r7 completed debris (M:all P:no R:yes)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0, 1, 2})
			},
			real: true, wantErr: "final file + manifest from a successful commit"},
		{name: "r8 zero-byte debris (M:chunks=[] size==0 P:no R:yes)", srcSize: 0,
			manifest: func(src string) *sshbroker.RelayManifest { return relayManifestFor(t, src, relayProfileChunk, nil) },
			real:     true, wantErr: "zero-byte commit's manifest"},
		{name: "r9 self-heal over old real name (M:chunks=[] size>0 P:no R:yes)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest { return relayManifestFor(t, src, relayProfileChunk, nil) },
			real:     true, wantResumed: 0},
		{name: "r10 incomplete over real name (M:incomplete P:no R:yes)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0})
			},
			real: true, wantErr: "fresh=true"},
		{name: "r11 retry commit (M:all P:yes R:yes)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0, 1, 2})
			},
			partial: true, real: true, wantResumed: 3},
		{name: "r12 incomplete with partial and real (M:incomplete P:yes R:yes)", srcSize: srcSize,
			manifest: func(src string) *sshbroker.RelayManifest {
				return relayManifestFor(t, src, relayProfileChunk, []int{0, 1})
			},
			partial: true, real: true, wantErr: "fresh=true"},
		{name: "r13 partial and real without manifest (M:no P:yes R:yes)", srcSize: srcSize,
			partial: true, real: true, wantErr: "fresh=true"},
		{name: "r14 fresh overwrite of old real name (M:no P:no R:yes)", srcSize: srcSize,
			real: true, wantResumed: 0},
	}

	for i, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			dir := fmt.Sprintf("%s/state%d", e.root, i)
			var src string
			if r.srcSize == 0 {
				src = dir + "/src.bin"
				relayMkFile(t, src, "")
			} else {
				src = relayMkSource(t, e, fmt.Sprintf("state%d/src.bin", i), r.srcSize)
			}
			to := dir + "/f.bin"
			if r.manifest != nil {
				relayPutManifest(t, to+".sshmgr-manifest.json", r.manifest(src))
			}
			if r.manifestRaw != "" {
				relayPutRaw(t, to+".sshmgr-manifest.json", r.manifestRaw)
			}
			if r.partial {
				relayMkFile(t, to+".sshmgr-partial", strings.Repeat("a", int(r.srcSize)))
			}
			if r.real {
				relayMkFile(t, to, "old-real-name")
			}

			out, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
			if r.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), r.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, r.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("want success, got %v", err)
			}
			if out.ResumedChunks != int(r.wantResumed) {
				t.Fatalf("ResumedChunks = %d, want %d", out.ResumedChunks, r.wantResumed)
			}
			if out.ChunksTotal != 3 {
				t.Fatalf("ChunksTotal = %d, want 3", out.ChunksTotal)
			}
		})
	}
}

// fresh=true: 整段跳过语义解析 — malformed/超限 manifest 也可重启 (rev3 codex#5)。
//
// "preflight 零远端状态变更"的取证 (本测试立意) 在 T5 引擎落地后重锚 (ledger:
// task-5-report「T4 test observation race, quantified」/ progress.md T7 重锚项):
// 原形态在 RelayForProfile 返回后直读盘上工件——返回即引擎 goroutine 起跑,
// stage 0 的 fresh 删除与之赛跑 (after 读可能 ENOENT 或读到引擎新落的空清单),
// 实证绿 (-count=10) 但属结构性潜在 flake。重锚为两个无竞争半边:
//
//	半 1 (admission): fresh 跳过解析 → malformed 照建任务; 等终态后再断言引擎
//	  侧最终形态 (fresh 删除+重传 → done + 真名)——post-terminal 读零竞争。
//	半 2 (non-mutation): 运行中 blocker 经 ReserveRelay 占住同目标四工件写集 →
//	  fresh 调用在 ⑧ 被拒 → 零引擎启动 → 拒绝点在 ⑥ (fresh 跳过段) 之后, 盘上
//	  工件字节级断言零竞争——"preflight 走完 fresh 段仍未删除"的最强形态。
func TestRelayForProfileFreshRestartsMalformed(t *testing.T) {
	e := relayNewEnv(t)
	src := relayMkSource(t, e, "fresh/src.bin", 1000)
	to := e.root + "/fresh/f.bin"

	relayPutRaw(t, to+relayManifestSuffix, "totally malformed {")
	relayMkFile(t, to+relayPartialSuffix, strings.Repeat("p", 500))
	out, err := e.relayCall(e.relayInput(e.srcID, src, to, true))
	if err != nil {
		t.Fatalf("fresh over malformed manifest: %v", err)
	}
	if out.ResumedChunks != 0 {
		t.Fatalf("ResumedChunks = %d, want 0 (fresh)", out.ResumedChunks)
	}
	if s := waitTerminal(t, e.tm, out.TaskID, 10*time.Second); s.status != bgStatusDone {
		t.Fatalf("status = %q (err=%q), want done — fresh discarded the debris and re-transferred", s.status, s.errText)
	}
	if fi, serr := os.Stat(filepath.FromSlash(to)); serr != nil || fi.Size() != 1000 {
		t.Fatalf("fresh restart must land the real name: fi=%v err=%v", fi, serr)
	}

	// 超限 manifest + fresh 同样可重启 (等终态后断言, 同零竞争口径)。
	t.Run("over-cap manifest", func(t *testing.T) {
		to2 := e.root + "/fresh/cap/f.bin"
		relayPutRaw(t, to2+relayManifestSuffix, strings.Repeat("x", 20<<10))
		out2, err := e.relayCall(e.relayInput(e.srcID, src, to2, true))
		if err != nil {
			t.Fatalf("fresh over over-cap manifest: %v", err)
		}
		if s := waitTerminal(t, e.tm, out2.TaskID, 10*time.Second); s.status != bgStatusDone {
			t.Fatalf("status = %q (err=%q), want done", s.status, s.errText)
		}
	})

	t.Run("preflight does not delete on fresh", func(t *testing.T) {
		to3 := e.root + "/fresh/blocked/f.bin"
		relayPutRaw(t, to3+relayManifestSuffix, "totally malformed {")
		relayMkFile(t, to3+relayPartialSuffix, strings.Repeat("p", 500))
		beforeM, merr := os.ReadFile(filepath.FromSlash(to3 + relayManifestSuffix))
		if merr != nil {
			t.Fatal(merr)
		}
		beforeP, perr := os.ReadFile(filepath.FromSlash(to3 + relayPartialSuffix))
		if perr != nil {
			t.Fatal(perr)
		}
		// 运行中 blocker 占住 to3 四工件写集 (TestRelayForProfileConflictMatrix
		// 同款虚拟任务; CloseAll 挂 Cleanup 收口)。
		blocker := BgTaskSpec{
			ProjectID: e.projID, ServerID: e.dstID, Command: "fresh-preflight-blocker", Timeout: time.Hour,
			Run: func(ctx context.Context, _ *bgTask) { <-ctx.Done() },
		}
		writes := []string{
			relayKeyOf(e.dstID, to3),
			relayKeyOf(e.dstID, to3+relayPartialSuffix),
			relayKeyOf(e.dstID, to3+relayManifestSuffix),
			relayKeyOf(e.dstID, to3+relayManifestSuffix+".tmp"),
		}
		if _, _, rerr := e.tm.ReserveRelay(blocker, nil, writes); rerr != nil {
			t.Fatal(rerr)
		}
		if _, err := e.relayCall(e.relayInput(e.srcID, src, to3, true)); err == nil || !strings.Contains(err.Error(), "relay conflict") {
			t.Fatalf("err = %v, want the write-set conflict rejection (zero engine started)", err)
		}
		// 无引擎 goroutine 在场 → 字节级断言零竞争: preflight 走过 ①–⑧ 的 fresh
		// 全程仍零删除 (删除在引擎 stage 0, 以 ⑧ 租约为前提)。
		afterM, merr := os.ReadFile(filepath.FromSlash(to3 + relayManifestSuffix))
		if merr != nil {
			t.Fatal(merr)
		}
		afterP, perr := os.ReadFile(filepath.FromSlash(to3 + relayPartialSuffix))
		if perr != nil {
			t.Fatal(perr)
		}
		if string(beforeM) != string(afterM) || string(beforeP) != string(afterP) {
			t.Fatal("preflight must not mutate the malformed manifest or the partial on any rejection path (deletion belongs to engine stage 0)")
		}
	})
}

// ---------- ③b/⑦ 空间 ----------

// 首跑新目录 (父不存在): statvfs 经 RelayAvailable 逐级上溯 — linux lane 得真字节
// (SpaceCheck=ok), windows lane statvfs ENOTSUP → fail-open unavailable; 两 lane
// 都必须照建任务 (rev4 kimi#7 的 ForProfile 侧接线锚)。
func TestRelayForProfileSpaceAncestorParent(t *testing.T) {
	e := relayNewEnv(t)
	src := relayMkSource(t, e, "space/src.bin", 1000)
	to := e.root + "/space/deep/never-created/f.bin"

	out, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
	if err != nil {
		t.Fatalf("task must be created: %v", err)
	}
	if runtime.GOOS == "windows" {
		if out.SpaceCheck != "unavailable" {
			t.Fatalf("windows statvfs stub: SpaceCheck = %q, want unavailable", out.SpaceCheck)
		}
		return
	}
	if out.SpaceCheck != "ok" {
		t.Fatalf("linux ancestor walk: SpaceCheck = %q, want ok", out.SpaceCheck)
	}
}

// 真字节空间分支 (linux CI lane; windows statvfs stub 恒 ENOTSUP 无法产真字节)。
func TestRelayForProfileSpaceRealBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pkg/sftp server statvfs stub returns ENOTSUP on windows hosts — linux CI lane carries the real-bytes space cases")
	}
	e := relayNewEnv(t)

	t.Run("insufficient refuses with evidence", func(t *testing.T) {
		src := e.root + "/space/full/src.bin"
		relayMkSparse(t, src, relaySparseSpaceSize) // 16TiB 稀疏
		_, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
			e.relayInput(e.srcID, src, e.root+"/space/full/f.bin", false), relaySpaceChunk)
		if err == nil {
			t.Fatal("want space refusal")
		}
		for _, frag := range []string{"available", "needs"} {
			if !strings.Contains(err.Error(), frag) {
				t.Fatalf("refusal must carry avail/need evidence (%q): %q", frag, err.Error())
			}
		}
	})

	t.Run("fresh projection counts reclaimable partial", func(t *testing.T) {
		// 同体积稀疏源 (不足必拒) + 同体积稀疏 partial; fresh 投影计入可回收 → 照建
		// (rev4 codex#2 锚: 消"盘满时 fresh 永远到不了 stage 0 删除"死锁)。
		src := e.root + "/space/proj/src.bin"
		relayMkSparse(t, src, relaySparseSpaceSize)
		to := e.root + "/space/proj/f.bin"
		relayMkSparse(t, to+".sshmgr-partial", relaySparseSpaceSize)
		out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
			e.relayInput(e.srcID, src, to, true), relaySpaceChunk)
		if err != nil {
			t.Fatalf("fresh projection must admit: %v", err)
		}
		if out.SpaceCheck != "ok" {
			t.Fatalf("SpaceCheck = %q, want ok", out.SpaceCheck)
		}
	})

	t.Run("resume is gauged on missing bytes", func(t *testing.T) {
		// 16TiB 源 15/16 完成: missing=256MiB (+同值余量) ≪ 可用 → 不靠投影照建
		// (rev3 kimi#6 锚: 续传按 missing 口径, 预置大 partial 不再误拒)。
		src := e.root + "/space/resume/src.bin"
		relayMkSparse(t, src, relaySparseSpaceSize)
		to := e.root + "/space/resume/f.bin"
		idxs := make([]int, 15)
		for i := range idxs {
			idxs[i] = i
		}
		relayPutManifest(t, to+".sshmgr-manifest.json", relayManifestFor(t, src, relaySpaceChunk, idxs))
		relayMkSparse(t, to+".sshmgr-partial", relaySparseSpaceSize)
		out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
			e.relayInput(e.srcID, src, to, false), relaySpaceChunk)
		if err != nil {
			t.Fatalf("resume gauged on missing bytes must admit: %v", err)
		}
		if out.ResumedChunks != 15 {
			t.Fatalf("ResumedChunks = %d, want 15", out.ResumedChunks)
		}
	})
}

// ---------- ⑧ 冲突集四矩阵 (ForProfile 侧: 键构造 + canonical 贯通) ----------

func TestRelayForProfileConflictMatrix(t *testing.T) {
	e := relayNewEnv(t)

	// 运行中 blocker (白盒直种, Run 阻塞至 cancel): 写 dstID:<root>/data/f 四工件,
	// 读 dstID:<root>/data/other-src。键全部锚在物理存在的路径上——probe 要过
	// ①–⑦ 才到 ⑧, 源 stat 与 ⑤ Stat 需要真文件。CloseAll (Cleanup) 收口。
	target := e.root + "/data/f"
	otherSrc := e.root + "/data/other-src"
	blocker := BgTaskSpec{
		ProjectID: e.projID, ServerID: e.dstID, Command: "blocker", Timeout: time.Hour,
		Run: func(ctx context.Context, _ *bgTask) { <-ctx.Done() },
	}
	blockerReads := []string{relayKeyOf(e.dstID, otherSrc)}
	blockerWrites := []string{
		relayKeyOf(e.dstID, target),
		relayKeyOf(e.dstID, target+relayPartialSuffix),
		relayKeyOf(e.dstID, target+relayManifestSuffix),
		relayKeyOf(e.dstID, target+relayManifestSuffix+".tmp"),
	}
	if _, _, err := e.tm.ReserveRelay(blocker, blockerReads, blockerWrites); err != nil {
		t.Fatal(err)
	}

	// 供 stat 用的真文件 (blocker 是虚拟任务, 盘上文件随意在)。
	relayMkFile(t, target, "blocker-target-shape")
	p1 := relayMkSource(t, e, "conf/p1.bin", 16)
	relayMkFile(t, otherSrc, "blocker-source-shape")

	before := e.tm.Len()

	t.Run("canonical collision with running write set", func(t *testing.T) {
		// <root>/data/./f canonical 后即 blocker 的真名键——① 贯通的 canonical
		// 值进 ⑧ 键集, 不可绕。
		_, err := e.relayCall(e.relayInput(e.srcID, p1, e.root+"/data/./f", false))
		if err == nil || !strings.Contains(err.Error(), "relay conflict") {
			t.Fatalf("err = %v, want relay conflict", err)
		}
	})

	t.Run("manifest artifact collision", func(t *testing.T) {
		_, err := e.relayCall(e.relayInput(e.srcID, p1, target+relayManifestSuffix, false))
		if err == nil || !strings.Contains(err.Error(), "relay conflict") {
			t.Fatalf("err = %v, want relay conflict", err)
		}
	})

	t.Run("own write vs running read", func(t *testing.T) {
		_, err := e.relayCall(e.relayInput(e.srcID, p1, otherSrc, false))
		if err == nil || !strings.Contains(err.Error(), "relay conflict") {
			t.Fatalf("err = %v, want relay conflict", err)
		}
	})

	t.Run("own read vs running write", func(t *testing.T) {
		// 源 = blocker 的目标工件 (同 endpoint): own read ∩ running writes ≠ ∅ → 拒
		// (rev3 codex#2 锚)。
		_, err := e.relayCall(e.relayInput(e.dstID, target, e.root+"/conf/fresh-target.bin", false))
		if err == nil || !strings.Contains(err.Error(), "relay conflict") {
			t.Fatalf("err = %v, want relay conflict", err)
		}
	})

	t.Run("read-read allowed", func(t *testing.T) {
		// 同源同键 read∩read 允许 (读键 = blocker 的读键, 写集全新)。
		out, err := e.relayCall(e.relayInput(e.dstID, otherSrc, e.root+"/conf/rr-target.bin", false))
		if err != nil {
			t.Fatalf("read∩read must be allowed: %v", err)
		}
		if out.TaskID == "" {
			t.Fatal("TaskID empty")
		}
	})

	t.Run("distinct to_server_id same path not mutually exclusive", func(t *testing.T) {
		// rev4 kimi#1 锚: 同路径、不同 to_server_id (srcID 行) → 键空间不同构, 不互斥。
		out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
			RelayInput{FromServerID: e.srcID, FromPath: p1, ToServerID: e.srcID, ToPath: target}, relayProfileChunk)
		if err != nil {
			t.Fatalf("rev4 kimi#1: distinct endpoint same path must run: %v", err)
		}
		if out.TaskID == "" {
			t.Fatal("TaskID empty")
		}
	})

	// spec §6: ⑧ 拒绝同落 relay-bg-start 审计行 (status=error)——共享 defer 落笔
	// (ReserveRelay 失败路径), 四笔冲突恰好四行, 归因端点均为目标侧。
	errStarts := 0
	for _, r := range relayAuditRows(t, e.st, 20) {
		if r.Action == "relay-bg-start" && r.Status == "error" {
			if r.ServerID != e.dstID {
				t.Fatalf("conflict start row ServerID = %q, want %q", r.ServerID, e.dstID)
			}
			errStarts++
		}
	}
	if errStarts != 4 {
		t.Fatalf("relay-bg-start/error rows = %d, want 4 (one per conflict rejection)", errStarts)
	}

	if got := e.tm.Len(); got != before+2 { // 恰两笔成功 (read-read + distinct-endpoint)
		t.Fatalf("Len = %d, want %d (conflicts must not create tasks)", got, before+2)
	}
}

// TestRelayForProfileSaturatedTableStartAuditRow: maxTasks 满员拒绝在 ForProfile
// 层同样落 relay-bg-start status=error 审计行 (spec §6)——满员形态照
// TestReserveRelayAdmissionEvictionAndLimit (b) 上移一层 (maxTasks=1 全 running)。
func TestRelayForProfileSaturatedTableStartAuditRow(t *testing.T) {
	e := relayNewEnv(t)
	sat := newTestTM(t, 1)
	t.Cleanup(func() { sat.CloseAll() })
	if _, _, err := sat.ReserveRelay(BgTaskSpec{
		ProjectID: e.projID, ServerID: e.dstID, Command: "filler", Timeout: time.Hour,
		Run: func(ctx context.Context, _ *bgTask) { <-ctx.Done() },
	}, nil, nil); err != nil {
		t.Fatal(err)
	}

	src := relayMkSource(t, e, "sat/src.bin", 16)
	_, err := RelayForProfile(context.Background(), e.st, sat, e.projID, e.pid,
		e.relayInput(e.srcID, src, e.root+"/sat/f.bin", false), relayProfileChunk)
	if !errors.Is(err, ErrBgTaskLimit) {
		t.Fatalf("saturated table must refuse with ErrBgTaskLimit, got %v", err)
	}
	row := relayAuditFind(t, relayAuditRows(t, e.st, 10), "relay-bg-start", "error")
	if row.ServerID != e.dstID {
		t.Fatalf("start row ServerID = %q, want %q", row.ServerID, e.dstID)
	}
	if got := sat.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 (refusal must not create a task)", got)
	}
}

// ---------- ⑨ 返回值 + 审计 ----------

func TestRelayForProfileReturnValueAndAudit(t *testing.T) {
	e := relayNewEnv(t)

	// 本机源 → 远程 dest: 全返回字段 + 双审计行 + start-before-end。
	localSrc := filepath.Join(e.local, "retval", "src.bin")
	relayMkFile(t, filepath.ToSlash(localSrc), strings.Repeat("b", 1000))
	to := e.root + "/retval/f.bin"

	out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		RelayInput{FromPath: localSrc, ToServerID: e.dstID, ToPath: to}, relayProfileChunk)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if out.TaskID == "" {
		t.Fatal("TaskID empty")
	}
	if out.BytesTotal != 1000 || out.ChunksTotal != 1 || out.ResumedChunks != 0 || out.ChunkBytes != relayProfileChunk {
		t.Fatalf("out = %+v", out)
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

	s := waitTerminal(t, e.tm, out.TaskID, 5*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("relay engine: status = %q, want done", s.status)
	}

	wantStart := fmt.Sprintf("relay local:%s -> %s:%s (1000 bytes, 1 chunks, resumed 0)",
		filepath.Clean(localSrc), e.dstID, to)
	rows := relayAuditRows(t, e.st, 30)
	start := relayAuditFind(t, rows, "relay-bg-start", "ok")
	if start.Command != wantStart {
		t.Fatalf("start Command = %q, want %q", start.Command, wantStart)
	}
	end := relayAuditFind(t, rows, "relay-bg-end", "ok")
	if end.Command != out.TaskID {
		t.Fatalf("end Command = %q, want taskID %q", end.Command, out.TaskID)
	}
	if end.TS.Before(start.TS) {
		t.Fatalf("end row (%v) predates start row (%v) — start must be written inside the insert lock first", end.TS, start.TS)
	}

	// 远程 → 远程: 摘要渲染 from 侧用服务器 id; 2.5 块源 → ChunksTotal=3。
	srcR := relayMkSource(t, e, "retvalr/src.bin", 2*relayProfileChunk+relayProfileChunk/2)
	out2, err := e.relayCall(e.relayInput(e.srcID, srcR, e.root+"/retvalr/f.bin", false))
	if err != nil {
		t.Fatalf("remote relay: %v", err)
	}
	if out2.ChunksTotal != 3 {
		t.Fatalf("ChunksTotal = %d, want 3", out2.ChunksTotal)
	}
	wantStart2 := fmt.Sprintf("relay %s:%s -> %s:%s (%d bytes, 3 chunks, resumed 0)",
		e.srcID, srcR, e.dstID, e.root+"/retvalr/f.bin", 2*relayProfileChunk+relayProfileChunk/2)
	rows = relayAuditRows(t, e.st, 30)
	start2 := relayAuditFind(t, rows, "relay-bg-start", "ok")
	if start2.Command != wantStart2 {
		t.Fatalf("start2 Command = %q, want %q", start2.Command, wantStart2)
	}
}
