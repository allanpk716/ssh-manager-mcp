//go:build windows

package mcpserver

// Plan 47 T7: 最终 rename 失败注入的 e2e (spec §8 e2e 段, rev4 codex#1 锚)——
// windows lane 专属: 注入机制依赖 FILE_SHARE_DELETE 共享语义 (linux 的 rename
// 无条件替换在场的真名, 无等价形态; relayengine_windows_test.go 同款判定)。
//
// 形态: 测试进程独占持有目标"真名"句柄 (共享模式不含 FILE_SHARE_DELETE) →
// 引擎流式搬运正常完成 → 提交点 PosixRename(partial→真名) 报 sharing violation
// → 任务 failed。盘上产物 = §3 r11 重试提交态 (manifest 全完成 + partial + 旧
// 真名)。占用者释放后同参数重跑 → 零块移动 (零进度行) 直奔提交 → 真名替换。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestE2ERelayFileCommitRenameBlockedThenRetryCommit(t *testing.T) {
	e := newRelayE2EEnv(t)
	s := e.openSession(t)
	srcData := strings.Repeat("a", 2*relayBigChunk) // 32 MiB → 2 块
	src := e.root + "/rename/src.bin"
	relayMkFile(t, src, srcData)
	to := e.root + "/rename/f.bin"
	relayMkFile(t, to, "OLD-REAL-NAME-CONTENT") // 旧真名在场 (r14 覆盖语义)

	// 独占占住真名: 读打开但共享模式不含 FILE_SHARE_DELETE → sftp 服务端 (同宿主
	// OS) 的 rename→MoveFileEx(REPLACE_EXISTING) 对它报 sharing violation。
	h, herr := windows.CreateFile(windows.StringToUTF16Ptr(filepath.FromSlash(to)),
		windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if herr != nil {
		t.Fatalf("hold real-name handle: %v", herr)
	}

	// Run 1: 覆盖旧真名的正常传输——搬运全程畅通, 提交点即爆。
	out1 := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	if out1.ResumedChunks != 0 || out1.ChunksTotal != 2 {
		t.Fatalf("run1 = %+v, want 0 resumed of 2", out1)
	}
	stdout1, status, errText := relayE2EPoll(t, s, out1.TaskID, 60*time.Second)
	if status != bgStatusFailed {
		t.Fatalf("run1 terminal = %q, want failed (commit rename must hit the sharing violation)", status)
	}
	if n := strings.Count(stdout1, " ok bytes="); n != 2 {
		t.Fatalf("run1 chunk progress lines = %d, want 2 (all chunks moved before the blocked commit):\n%s", n, stdout1)
	}
	// 引擎错误经任务字段 surfacing (ErrText→exec_output 的 error), 逐字锚提交点。
	if !strings.Contains(errText, "commit rename") {
		t.Fatalf("run1 error = %q, want the commit-rename failure", errText)
	}

	// 提交失败的合法产物 = §3 r11: manifest 全完成 + partial 在 + 真名旧内容。
	m := relayE2EManifest(t, e, to)
	if len(m.Chunks) != 2 {
		t.Fatalf("manifest chunks = %+v, want both chunks recorded (the failure is commit-point only)", m.Chunks)
	}
	if _, serr := os.Stat(filepath.FromSlash(to + relayPartialSuffix)); serr != nil {
		t.Fatalf("partial must survive the failed commit, stat err=%v", serr)
	}
	relayE2EAssertBytes(t, to, "OLD-REAL-NAME-CONTENT")

	// 占用者释放 → Run 2 同参数: 重试提交态 (resumed=2), 零块移动直奔提交。
	windows.CloseHandle(h)
	out2 := relayE2ECallOK(t, s, e.input(e.srcID, src, to, false))
	if out2.ResumedChunks != 2 || out2.ChunksTotal != 2 {
		t.Fatalf("run2 = %+v, want retry-commit shape (2 resumed of 2)", out2)
	}
	stdout2, status, _ := relayE2EPoll(t, s, out2.TaskID, 60*time.Second)
	if status != bgStatusDone {
		t.Fatalf("run2 terminal = %q, want done", status)
	}
	if n := strings.Count(stdout2, " ok bytes="); n != 0 {
		t.Fatalf("run2 chunk progress lines = %d, want 0 (retry-commit moves zero blocks):\n%s", n, stdout2)
	}
	relayE2EAssertBytes(t, to, srcData) // 真名已被提交替换
	relayE2EAssertGone(t, to)
}
