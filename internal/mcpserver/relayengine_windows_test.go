//go:build windows

package mcpserver

// Plan 47 T5: manifest 删除失败注入 (windows lane 专属——注入机制依赖
// FILE_SHARE_DELETE 共享语义, linux 无等价形态; os.Remove 自 Go 1.21 起也
// 能删只读文件, 该注入不可用)。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// 注入: 测试进程持有清单句柄且共享模式不含 FILE_SHARE_DELETE——sftp 服务端
// (同宿主 OS) 的 os.Remove→DeleteFileW 报 sharing violation, 完成段的
// best-effort 删除必失败。断言: 任务仍 done + 警告行 + 真名在 + 清单残留。
func TestRelayEngineManifestDeleteFailureWarnsDone(t *testing.T) {
	e := relayNewEnv(t)
	const chunk = 64 << 10
	src := relayMkSource(t, e, "stale/src.bin", 3*chunk)
	to := e.root + "/stale/f.bin"
	manifestSlash := to + relayManifestSuffix

	relayPutManifest(t, manifestSlash, relayRealManifest(t, src, chunk, []int{0, 1, 2}))
	relayMkFile(t, to+relayPartialSuffix, strings.Repeat("a", 3*chunk))

	h, herr := windows.CreateFile(windows.StringToUTF16Ptr(filepath.FromSlash(manifestSlash)),
		windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if herr != nil {
		t.Fatalf("hold manifest handle: %v", herr)
	}
	defer windows.CloseHandle(h)

	out, err := e.relayCall(e.relayInput(e.srcID, src, to, false))
	if err != nil {
		t.Fatal(err)
	}
	s := waitTerminal(t, e.tm, out.TaskID, 10*time.Second)
	if s.status != bgStatusDone {
		t.Fatalf("status = %q err=%q, want done (manifest-delete failure must not fail the task)", s.status, s.errText)
	}
	if fi, serr := os.Stat(filepath.FromSlash(to)); serr != nil || fi.Size() != 3*chunk {
		t.Fatalf("real name: fi=%v err=%v", fi, serr)
	}
	if _, serr := os.Stat(filepath.FromSlash(manifestSlash)); serr != nil {
		t.Fatalf("stale manifest must still be present (Remove failed): %v", serr)
	}
	if stdout := relayReadOutput(t, e.tm, out.TaskID); !strings.Contains(stdout, "stale manifest left behind — harmless, remove at leisure") {
		t.Fatalf("warning line missing:\n%s", stdout)
	}
}
