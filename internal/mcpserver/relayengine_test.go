package mcpserver

// Plan 47 T5: runRelay 引擎白盒测试 (spec §2 引擎段钉序 / §8 引擎白盒段)。
// 锚点逐条: stage 0 抽读不符→failed+manifest 完好; 续传 stage 0 零 manifest 写
// (stop 保留字节级原样); 探测拒 (逐字文本, 裸 sftp 服务器零扩展对); 进度行落笔
// →长轮询即时唤醒 (非 timer); CloseAll 关双连接; zero-byte start-before-end +
// relay-bg-end; 洞续传只补缺块; commit-only 的 stale 尾巴 truncate 断言; 源
// re-stat 不符拒提交; manifest 删失败→done+警告+真名在。
//
// testsshd 的 sftp 服务宿主 FS (relay_test.go 同款); "远端"工件用 os 直栽/直读。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// relayBigChunk 是引擎时序类测试的块网格: 真实网络搬运毫秒到百毫秒级, 给
// plan 行同步点之后的注入 (Stop / Chtimes) 留出宽裕窗口。
const relayBigChunk = 16 << 20 // 无类型常量: int/int64 双语境直用

// ---------- 引擎测试 fixtures ----------

// relayRealChunkHex 计算源文件第 i 块的真实 sha256 (清单哈希锚)。
func relayRealChunkHex(t *testing.T, srcSlash string, chunkBytes, i int64) string {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(srcSlash))
	if err != nil {
		t.Fatal(err)
	}
	n := relayChunkLen(i, chunkBytes, int64(len(data)))
	sum := sha256.Sum256(data[i*chunkBytes : i*chunkBytes+n])
	return hex.EncodeToString(sum[:])
}

// relayRealManifest 组已完成块哈希为真实值的 manifest (idxs 允许洞)。
func relayRealManifest(t *testing.T, srcSlash string, chunkBytes int64, idxs []int) *sshbroker.RelayManifest {
	t.Helper()
	size, mtime := relayStatSource(t, srcSlash)
	chunks := make([]sshbroker.RelayChunkDone, 0, len(idxs))
	for _, i := range idxs {
		chunks = append(chunks, sshbroker.RelayChunkDone{I: i, SHA256: relayRealChunkHex(t, srcSlash, chunkBytes, int64(i))})
	}
	return &sshbroker.RelayManifest{Version: 1, ChunkBytes: chunkBytes, SourceSize: size, SourceMtimeUnix: mtime, Chunks: chunks}
}

// relayReadOutput 读任务 stdout 全量快照。
func relayReadOutput(t *testing.T, m *TaskManager, id string) string {
	t.Helper()
	v, ok, err := m.Output(id, 0, 0, 0, context.Background())
	if err != nil || !ok {
		t.Fatalf("Output(%s): ok=%v err=%v", id, ok, err)
	}
	return string(v.Stdout)
}

// relayWaitPlanLine 长轮询至引擎 plan 行落笔 (exec_output 等待回路的真实消费
// 形态——唤醒源是 notifyWriter 的代际广播, 非 timer 到点), 返回该帧。
func relayWaitPlanLine(t *testing.T, m *TaskManager, id string, d time.Duration) BgView {
	t.Helper()
	type res struct {
		v  BgView
		ok bool
	}
	ch := make(chan res, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		v, ok, _ := m.Output(id, 0, 0, d, ctx)
		ch <- res{v, ok}
	}()
	select {
	case r := <-ch:
		if !r.ok {
			t.Fatalf("task %s vanished from registry", id)
		}
		if !strings.Contains(string(r.v.Stdout), "relay plan:") {
			t.Fatalf("plan line missing from first frame: %q", r.v.Stdout)
		}
		return r.v
	case <-time.After(d + 10*time.Second):
		t.Fatal("long-poll never returned")
		return BgView{}
	}
}

// relayWaitSlots 轮询至引擎已挂双连接槽, 返回 (dest, source) 两条连接。
func relayWaitSlots(t *testing.T, m *TaskManager, id string) (dst, src *sshbroker.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		tk, ok := m.tasks[id]
		if ok && tk.client != nil {
			dst, src = tk.client, tk.auxClient
			m.mu.Unlock()
			return dst, src
		}
		m.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("client slot never set within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------- stage 0 抽读复核: 不符 → failed, 原 manifest 完好 ----------

func TestRelayEngineSpotRecheckFailsManifestIntact(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = 64 << 10
	src := relayMkSource(t, e, "spot/src.bin", 3*chunk) // 'a' × 3 块
	to := e.root + "/spot/f.bin"

	m := relayRealManifest(t, src, chunk, []int{0, 2})
	// 最大 index (2) 的清单哈希与源不符——源没变、清单谎报 (或源曾在窗口内变更):
	// 引擎必须 failed, 零块移动, 原清单字节级完好。
	bad := sha256.Sum256([]byte(strings.Repeat("b", chunk)))
	m.Chunks[1].SHA256 = hex.EncodeToString(bad[:])
	manifestSlash := to + relayManifestSuffix
	relayPutManifest(t, manifestSlash, m)
	relayMkFile(t, to+relayPartialSuffix, strings.Repeat("a", 3*chunk))
	before, err := os.ReadFile(filepath.FromSlash(manifestSlash))
	if err != nil {
		t.Fatal(err)
	}

	out, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
	if err != nil {
		t.Fatalf("task must be created: %v", err)
	}
	if out.ResumedChunks != 2 {
		t.Fatalf("ResumedChunks = %d, want 2", out.ResumedChunks)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 10*time.Second)
	if s.status != bgStatusFailed {
		t.Fatalf("status = %q errText=%q, want failed", s.status, s.errText)
	}
	if !strings.Contains(s.errText, "source file changed since the interrupted transfer") {
		t.Fatalf("errText = %q, want pinned spot-re-check failure text", s.errText)
	}
	after, err := os.ReadFile(filepath.FromSlash(manifestSlash))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("stage 0 must leave the original manifest byte-intact (resume never writes at stage 0):\nbefore %s\nafter  %s", before, after)
	}
	if _, serr := os.Stat(filepath.FromSlash(to + relayPartialSuffix)); serr != nil {
		t.Fatalf("partial must remain untouched: %v", serr)
	}
	if _, serr := os.Stat(filepath.FromSlash(to)); !os.IsNotExist(serr) {
		t.Fatalf("real name must not appear on a stage-0 failure, stat err=%v", serr)
	}
}

// ---------- 续传 + stop: stage 0 零 manifest 写, 已完成块保留 ----------

func TestRelayEngineResumeStopPreservesManifest(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = relayBigChunk
	src := relayMkSource(t, e, "stopres/src.bin", 2*chunk) // 32MiB
	to := e.root + "/stopres/f.bin"

	relayPutManifest(t, to+relayManifestSuffix, relayRealManifest(t, src, chunk, []int{0}))
	relayMkFile(t, to+relayPartialSuffix, strings.Repeat("a", chunk))
	manifestBefore, err := os.ReadFile(filepath.FromSlash(to + relayManifestSuffix))
	if err != nil {
		t.Fatal(err)
	}

	// relayCall 的块网格是 relayProfileChunk——这里用 16MiB 大块网格直接调
	// RelayForProfile (⑥ 的 chunk_bytes 闸要求清单网格与调用网格一致)。
	out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		e.relayInput(e.srcID, src, to, false), chunk)
	if err != nil {
		t.Fatalf("task must be created: %v", err)
	}
	if out.ResumedChunks != 1 {
		t.Fatalf("ResumedChunks = %d, want 1", out.ResumedChunks)
	}
	if v := relayWaitPlanLine(t, e.tm, out.TaskID, 30*time.Second); v.Status != bgStatusRunning {
		t.Fatalf("status at plan line = %q, want running", v.Status)
	}
	// plan 行即同步点; 其后引擎要抽读复核 16MiB (读+哈希) 才进循环——Stop 必然
	// 落在 stage 0 或首块在途, 16MiB 块不可能在 ~1ms 内完成。
	if status, ok := e.tm.Stop(out.TaskID); !ok || status != bgStatusRunning {
		t.Fatalf("Stop = (%q,%v), want (running,true)", status, ok)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 10*time.Second)
	if s.status != bgStatusStopped {
		t.Fatalf("status = %q (err=%q), want stopped", s.status, s.errText)
	}
	after, err := os.ReadFile(filepath.FromSlash(to + relayManifestSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBefore, after) {
		t.Fatalf("manifest must be byte-identical after the stop (stage 0 never writes on resume; the in-flight chunk never records):\nbefore %s\nafter  %s", manifestBefore, after)
	}
	var got sshbroker.RelayManifest
	if jerr := json.Unmarshal(after, &got); jerr != nil || len(got.Chunks) != 1 || got.Chunks[0].I != 0 {
		t.Fatalf("manifest chunks = %+v (err=%v), want exactly chunk 0", got.Chunks, jerr)
	}
	if _, serr := os.Stat(filepath.FromSlash(to)); !os.IsNotExist(serr) {
		t.Fatalf("real name must not appear, stat err=%v", serr)
	}
	if fi, serr := os.Stat(filepath.FromSlash(to + relayPartialSuffix)); serr != nil || fi.Size() < chunk {
		t.Fatalf("partial must survive the stop: fi=%v err=%v", fi, serr)
	}
}

// ---------- 进度行落笔 → 长轮询即时唤醒 (非 timer) ----------

func TestRelayEngineProgressWakesLongPollImmediately(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = relayBigChunk
	src := relayMkSource(t, e, "wake/src.bin", 2*chunk)
	to := e.root + "/wake/f.bin"

	out, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	v := relayWaitPlanLine(t, e.tm, out.TaskID, 30*time.Second)
	wake := time.Since(start)
	// 长轮询预算 30s: 若 <10s 即返回, 只能是 notifyWriter 落笔广播唤醒——既非
	// timer 到点, 也非终态唤醒 (32MiB 传输还在途, 状态仍 running)。
	if wake > 10*time.Second {
		t.Fatalf("long-poll woke at %v — that is the timer, not the notifyWriter", wake)
	}
	if v.Status != bgStatusRunning {
		t.Fatalf("status = %q at wake, want running (transfer still in flight)", v.Status)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 30*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("status = %q err=%q, want done", s.status, s.errText)
	}
	got, err := os.ReadFile(filepath.FromSlash(to))
	if err != nil || int64(len(got)) != 2*chunk {
		t.Fatalf("readback len=%d err=%v, want %d", len(got), err, 2*chunk)
	}
	if !bytes.Equal(got, []byte(strings.Repeat("a", 2*chunk))) {
		t.Fatal("readback content mismatch")
	}
}

// ---------- CloseAll 双连接可达即关 ----------

func TestRelayEngineCloseAllClosesBothConnections(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = relayBigChunk
	src := relayMkSource(t, e, "closeboth/src.bin", 2*chunk)
	out, err := e.relayCall(e.relayInput(e.srcID, src, e.root+"/closeboth/f.bin", false))
	if err != nil {
		t.Fatal(err)
	}
	dstCli, srcCli := relayWaitSlots(t, e.tm, out.TaskID)
	relayWaitPlanLine(t, e.tm, out.TaskID, 30*time.Second) // 传输确已起跑 (32MiB 在途)
	e.tm.CloseAll()                                        // 引擎可能仍在途——CloseAll 双槽即关 + wg.Wait 收口
	waitClientClosed(t, dstCli)
	waitClientClosed(t, srcCli)
}

// ---------- zero-byte 全程: start-before-end + relay-bg-end + 双空摘要 ----------

func TestRelayEngineZeroByteStartBeforeEndAudit(t *testing.T) {
	e := relayNewEnv(t)
	localSrc := filepath.Join(e.local, "zb", "src.bin")
	relayMkFile(t, filepath.ToSlash(localSrc), "")
	to := e.root + "/zb/f.bin"

	out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		RelayInput{FromPath: localSrc, ToServerID: e.dstID, ToPath: to}, relayProfileChunk)
	if err != nil {
		t.Fatal(err)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 10*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("status = %q err=%q, want done (zero-byte transfer)", s.status, s.errText)
	}
	if fi, serr := os.Stat(filepath.FromSlash(to)); serr != nil || fi.Size() != 0 {
		t.Fatalf("zero-byte real name: fi=%v err=%v", fi, serr)
	}
	if _, serr := os.Stat(filepath.FromSlash(to + relayManifestSuffix)); !os.IsNotExist(serr) {
		t.Fatalf("manifest must be gone after commit, stat err=%v", serr)
	}
	stdout := relayReadOutput(t, e.tm, out.TaskID)
	if !strings.Contains(stdout, "relay plan: 0 bytes, 0 chunks (resumed 0), chunk=") {
		t.Fatalf("plan line mismatch:\n%s", stdout)
	}
	// 空哈希 = sha256(空串) — root 恒有; zero-byte 恒为 byte0→EOF 全程 → file_sha256 在。
	empty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, frag := range []string{
		"relay done: root=sha256:" + empty,
		"file_sha256=sha256:" + empty + "(total=0)",
		"renamed -> " + to,
	} {
		if !strings.Contains(stdout, frag) {
			t.Fatalf("done line missing %q:\n%s", frag, stdout)
		}
	}

	// 审计: start(ok) 锁内先落 (start-before-end, 秒级失败/完成也不倒挂);
	// end 行 action=relay-bg-end, Command=taskID。
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := relayAuditRows(t, e.st, 30)
		var startRow, endRow *store.AuditRow
		for i := range rows {
			if rows[i].Action == "relay-bg-start" && rows[i].Status == "ok" {
				startRow = &rows[i]
			}
			if rows[i].Action == "relay-bg-end" && rows[i].Status == "ok" {
				endRow = &rows[i]
			}
		}
		if startRow != nil && endRow != nil {
			if endRow.Command != out.TaskID {
				t.Fatalf("end Command = %q, want taskID %q", endRow.Command, out.TaskID)
			}
			if endRow.TS.Before(startRow.TS) {
				t.Fatalf("end row (%v) predates start row (%v) — start must be written in the insert lock first", endRow.TS, startRow.TS)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit rows never settled: %+v", rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------- 洞续传: 只补缺块, 字节精确 ----------

func TestRelayEngineResumeHolesCompletesOnlyMissing(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = 64 << 10
	srcData := strings.Repeat("a", chunk) + strings.Repeat("b", chunk) + strings.Repeat("c", chunk)
	src := e.root + "/holes/src.bin"
	relayMkFile(t, src, srcData)
	to := e.root + "/holes/f.bin"

	relayPutManifest(t, to+relayManifestSuffix, relayRealManifest(t, src, chunk, []int{0, 2}))
	partialData := strings.Repeat("a", chunk) + strings.Repeat("\x00", chunk) + strings.Repeat("c", chunk)
	relayMkFile(t, to+relayPartialSuffix, partialData)

	out, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
	if err != nil {
		t.Fatal(err)
	}
	if out.ResumedChunks != 2 {
		t.Fatalf("ResumedChunks = %d, want 2", out.ResumedChunks)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 10*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("status = %q err=%q, want done", s.status, s.errText)
	}
	got, err := os.ReadFile(filepath.FromSlash(to))
	if err != nil || string(got) != srcData {
		t.Fatalf("readback mismatch: len=%d err=%v", len(got), err)
	}
	if _, serr := os.Stat(filepath.FromSlash(to + relayManifestSuffix)); !os.IsNotExist(serr) {
		t.Fatal("manifest must be gone after commit")
	}
	stdout := relayReadOutput(t, e.tm, out.TaskID)
	if n := strings.Count(stdout, " ok bytes="); n != 1 {
		t.Fatalf("exactly one chunk line (only the missing chunk moves), got %d:\n%s", n, stdout)
	}
	wantPlan := fmt.Sprintf("relay plan: %d bytes, 3 chunks (resumed 2), chunk=%d", 3*chunk, chunk)
	if !strings.Contains(stdout, wantPlan) {
		t.Fatalf("plan line mismatch:\n%s", stdout)
	}
}

// ---------- commit-only 直奔提交: stale 尾巴 stage 0 truncate 断言 ----------

func TestRelayEngineCommitOnlyTruncatesStaleTail(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = 64 << 10
	src := relayMkSource(t, e, "trunc/src.bin", 3*chunk)
	to := e.root + "/trunc/f.bin"

	relayPutManifest(t, to+relayManifestSuffix, relayRealManifest(t, src, chunk, []int{0, 1, 2}))
	// partial = 全部正确内容 + 4096 字节 stale 尾巴: §3 "全完成=直奔提交" 行要求
	// 尺寸收敛——未 truncate 的 rename 会把尾巴带进真名 (rename 保留文件全长)。
	relayMkFile(t, to+relayPartialSuffix, strings.Repeat("a", 3*chunk)+strings.Repeat("J", 4096))

	out, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
	if err != nil {
		t.Fatal(err)
	}
	if out.ResumedChunks != 3 {
		t.Fatalf("ResumedChunks = %d, want 3 (retry-commit shape)", out.ResumedChunks)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 10*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("status = %q err=%q, want done", s.status, s.errText)
	}
	fi, serr := os.Stat(filepath.FromSlash(to))
	if serr != nil {
		t.Fatal(serr)
	}
	if fi.Size() != 3*chunk {
		t.Fatalf("real name size = %d, want exactly %d — the stale tail must be truncated away in stage 0 before the commit rename", fi.Size(), 3*chunk)
	}
	got, err := os.ReadFile(filepath.FromSlash(to))
	if err != nil || string(got) != strings.Repeat("a", 3*chunk) {
		t.Fatalf("real name content mismatch (len=%d err=%v)", len(got), err)
	}
	for _, artifact := range []string{to + relayManifestSuffix, to + relayPartialSuffix, to + relayManifestSuffix + ".tmp"} {
		if _, serr := os.Stat(filepath.FromSlash(artifact)); !os.IsNotExist(serr) {
			t.Fatalf("%s must be gone after commit, stat err=%v", artifact, serr)
		}
	}
	// 续传任务 (resumed>0) 的 done 行只有 merkle 根, 无 file_sha256 段; 根 = 升序
	// 串接三块 32B 摘要的 sha256。
	stdout := relayReadOutput(t, e.tm, out.TaskID)
	if !strings.Contains(stdout, "relay done: root=sha256:") || strings.Contains(stdout, "file_sha256") {
		t.Fatalf("done line must be root-only for a resumed task:\n%s", stdout)
	}
	root := sha256.New()
	for i := 0; i < 3; i++ {
		d, _ := hex.DecodeString(relayRealChunkHex(t, src, chunk, int64(i)))
		root.Write(d)
	}
	if !strings.Contains(stdout, fmt.Sprintf("root=sha256:%x", root.Sum(nil))) {
		t.Fatalf("merkle root mismatch:\n%s", stdout)
	}
}

// ---------- 源 re-stat 不符 → 拒提交 (真名不出场, manifest 完好) ----------

func TestRelayEngineReStatMismatchRefusesCommit(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = relayBigChunk
	srcSlash := relayMkSource(t, e, "restat/src.bin", 2*chunk) // 32MiB
	srcLocal := filepath.Join(e.local, "restat", "src.bin")
	to := e.root + "/restat/f.bin"

	relayPutManifest(t, to+relayManifestSuffix, relayRealManifest(t, srcSlash, chunk, []int{0, 1}))
	relayMkFile(t, to+relayPartialSuffix, strings.Repeat("a", 2*chunk))

	out, err := RelayForProfile(context.Background(), e.st, e.tm, e.projID, e.pid,
		RelayInput{FromPath: srcLocal, ToServerID: e.dstID, ToPath: to}, chunk)
	if err != nil {
		t.Fatal(err)
	}
	relayWaitPlanLine(t, e.tm, out.TaskID, 30*time.Second)
	// plan 行 = 引擎起跑同步点; 其后抽读复核 16MiB (读+哈希) 给出充裕窗口, mtime
	// 突变必然落在完成段 re-stat 之前。
	fi, err := os.Stat(srcLocal)
	if err != nil {
		t.Fatal(err)
	}
	mutated := fi.ModTime().Add(5 * time.Second)
	if cerr := os.Chtimes(srcLocal, mutated, mutated); cerr != nil {
		t.Fatal(cerr)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 15*time.Second)
	if s.status != bgStatusFailed {
		t.Fatalf("status = %q err=%q, want failed", s.status, s.errText)
	}
	if !strings.Contains(s.errText, "refusing to commit") {
		t.Fatalf("errText = %q, want re-stat refusal", s.errText)
	}
	if _, serr := os.Stat(filepath.FromSlash(to)); !os.IsNotExist(serr) {
		t.Fatalf("commit must be refused — real name must not appear, stat err=%v", serr)
	}
	if _, serr := os.Stat(filepath.FromSlash(to + relayManifestSuffix)); serr != nil {
		t.Fatalf("manifest intact on refusal: %v", serr)
	}
	if _, serr := os.Stat(filepath.FromSlash(to + relayPartialSuffix)); serr != nil {
		t.Fatalf("partial intact on refusal: %v", serr)
	}
}

// ---------- posix-rename 探测拒 (裸 sftp 服务器, 零扩展对) ----------

// startBareSFTPSSHD 起一个 sftp 子系统只回 SSH_FXP_VERSION (零扩展对) 的裸 SSH
// 服务器 (testsshd.Start 的 host key/password 骨架): 专供引擎 stage 0 第 1 步
// 探测拒绝分支的白盒驱动。探测是零 IO 内存查找, 不支持即终态——服务器无需实现
// 任何其他报文 (pkg/sftp 服务端恒广告 posix-rename, testsshd 触发不了本分支)。
func startBareSFTPSSHD(t *testing.T) (string, ssh.PublicKey, func()) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
		if string(pass) == "pw" {
			return nil, nil
		}
		return nil, io.EOF
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				close(done)
				return
			}
			go serveBareSFTP(c, cfg)
		}
	}()
	return ln.Addr().String(), signer.PublicKey(), func() {
		ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func serveBareSFTP(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		go func(nc ssh.NewChannel) {
			ch, creqs, aerr := nc.Accept()
			if aerr != nil {
				return
			}
			defer ch.Close()
			for req := range creqs {
				if req.Type != "subsystem" {
					req.Reply(false, nil)
					continue
				}
				var sub struct{ Subsystem string }
				if uerr := ssh.Unmarshal(req.Payload, &sub); uerr != nil || sub.Subsystem != "sftp" {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				// SSH_FXP_VERSION (type 2), version 3, 零扩展对——HasExtension 的
				// 内存查找因此为空, 探测即拒。之后停车至客户端断开。
				pkt := []byte{2, 0, 0, 0, 3}
				hdr := make([]byte, 4)
				binary.BigEndian.PutUint32(hdr, uint32(len(pkt)))
				if _, werr := ch.Write(append(hdr, pkt...)); werr != nil {
					return
				}
				_, _ = io.Copy(io.Discard, ch)
				return
			}
		}(newChan)
	}
}

func TestRelayEnginePosixRenameProbeRejected(t *testing.T) {
	addr, _, cleanup := startBareSFTPSSHD(t)
	defer cleanup()
	cli, cerr := sshbroker.ConnectKeepAlive(context.Background(),
		hostOfAddr(addr), portOfAddr(addr), "u", ssh.Password("pw"), ssh.InsecureIgnoreHostKey())
	if cerr != nil {
		t.Fatalf("connect: %v", cerr)
	}
	t.Cleanup(func() { cli.Close() })

	localSrc := filepath.Join(t.TempDir(), "src.bin")
	if werr := os.WriteFile(localSrc, []byte("hello relay"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	fi, serr := os.Stat(localSrc)
	if serr != nil {
		t.Fatal(serr)
	}

	// 首跑 (fresh) 路径: plan 行先落笔, 探测即拒 (逐字文本)。
	buf := &bytes.Buffer{}
	code, timedOut, rerr := relayEngineRun(context.Background(), &RelayTaskSpec{
		SrcServerID: "", DstServerID: "dst",
		FromPath: localSrc, ToPath: "/data/f.bin",
		Size: fi.Size(), Mtime: fi.ModTime().Unix(),
		ChunkBytes: 64 << 10, ChunksTotal: 1,
		Fresh: true, Resumed: 0,
		Manifest: nil, SrcCli: nil, DstCli: cli,
	}, buf)
	if rerr == nil || !strings.Contains(rerr.Error(), "lacks posix-rename@openssh.com") {
		t.Fatalf("err = %v, want pinned posix-rename rejection text", rerr)
	}
	if timedOut || code != 0 {
		t.Fatalf("code=%d timedOut=%v, want 0/false", code, timedOut)
	}
	if !strings.HasPrefix(buf.String(), "relay plan:") {
		t.Fatalf("plan line must precede the probe: %q", buf.String())
	}

	// 续传路径同样先行探测 (rev3 kimi#2: 探测无条件先行, 不绑空清单落盘)。
	buf2 := &bytes.Buffer{}
	_, _, rerr = relayEngineRun(context.Background(), &RelayTaskSpec{
		SrcServerID: "", DstServerID: "dst",
		FromPath: localSrc, ToPath: "/data/f.bin",
		Size: fi.Size(), Mtime: fi.ModTime().Unix(),
		ChunkBytes: 64 << 10, ChunksTotal: 1,
		Fresh: false, Resumed: 1,
		Manifest: &sshbroker.RelayManifest{
			Version: 1, ChunkBytes: 64 << 10, SourceSize: fi.Size(), SourceMtimeUnix: fi.ModTime().Unix(),
			Chunks: []sshbroker.RelayChunkDone{{I: 0, SHA256: strings.Repeat("ab", 32)}},
		},
		SrcCli: nil, DstCli: cli,
	}, buf2)
	if rerr == nil || !strings.Contains(rerr.Error(), "lacks posix-rename@openssh.com") {
		t.Fatalf("resume-path probe: err = %v, want the same pinned rejection", rerr)
	}
}
