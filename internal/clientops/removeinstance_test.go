package clientops

// Plan 46 T2 — RemoveInstance (双根清理 + 路径安全 + partial 幂等) 与进程内
// 互斥(cacheWriteMu:rm/force 独占 ↔ pull/pair 写盘被拒)的测试。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
)

// removeTestEnv redirects both roots into temp dirs WITHOUT swapping the
// DekProvider seam: RemoveInstance must exercise the REAL FileKeyProvider
// delete path (SSHMGR_CACHE_DEK_DIR relocates the whole DEK tree; see
// paths.CacheDekDirEnv).
func removeTestEnv(t *testing.T) (userDir, dekDir string) {
	t.Helper()
	userDir = redirectUserConfigDir(t) // 清 SSHMGR_CACHE_DIR
	dekDir = t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DEK_DIR": dekDir, "SSHMGR_CACHE_DEK": ""})
	return userDir, dekDir
}

// seedInstanceWithRealDek seeds a full slot (auth/bin/meta/config) AND a real
// DEK file at the resolved path — the exact double-root shape rm must clear.
func seedInstanceWithRealDek(t *testing.T, userDir, name string) (slotDir, dekPath string) {
	t.Helper()
	slotDir = filepath.Join(userDir, "ssh-manager", "instances", name)
	if err := os.MkdirAll(slotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"cache.auth.json", "cache.bin", "cache.meta.json", "cache.config.json"} {
		if err := os.WriteFile(filepath.Join(slotDir, f), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dp, err := paths.CacheDekPathFor(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dp, []byte("dek-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return slotDir, dp
}

func TestRemoveInstance_RemovesBothRoots(t *testing.T) {
	userDir, _ := removeTestEnv(t)
	slotDir, dekPath := seedInstanceWithRealDek(t, userDir, "agentA")

	if err := RemoveInstance("agentA"); err != nil {
		t.Fatalf("RemoveInstance: %v", err)
	}
	for _, p := range []string{slotDir, dekPath} {
		if _, serr := os.Stat(p); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("%s must be gone (stat err=%v)", p, serr)
		}
	}
	// 幂等重跑:两根已空,再删仍 nil(残留物审计为真相源)。
	if err := RemoveInstance("agentA"); err != nil {
		t.Fatalf("idempotent re-run must succeed: %v", err)
	}
}

func TestRemoveInstance_Guards(t *testing.T) {
	userDir, _ := removeTestEnv(t)

	if err := RemoveInstance(""); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("default slot must be refused with a clear pointer: %v", err)
	}
	for _, env := range []string{"SSHMGR_CACHE_DIR", "SSHMGR_CACHE_DEK"} {
		t.Setenv(env, filepath.Join(t.TempDir(), "x"))
		err := RemoveInstance("agentA")
		t.Setenv(env, "")
		if err == nil || !strings.Contains(err.Error(), env) {
			t.Fatalf("%s set must be refused: %v", env, err)
		}
	}
	// 非法名在锁外即拒(traversal/分隔符/保留名/绝对路径),escape 目标不动。
	evil := filepath.Join(userDir, "ssh-manager", "evil")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(evil, "canary.txt")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../evil", "..", ".", "a/b", `a\b`, "CON", "nul.txt", "COM1", `C:\x`, "/abs", "a:b"} {
		if err := RemoveInstance(bad); err == nil {
			t.Fatalf("RemoveInstance(%q) must be refused", bad)
		}
	}
	if _, serr := os.Stat(canary); serr != nil {
		t.Fatalf("escape target must survive every refusal: %v", serr)
	}
	// 合法名但槽不存在 = 幂等成功。
	if err := RemoveInstance("never-was"); err != nil {
		t.Fatalf("absent instance must be idempotent success: %v", err)
	}
}

// TestRemoveInstance_Partial_DirOk_DekFail:目录已删成、DEK 删失败 → 错误含
// 残留物(DEK 路径);解除注入后重跑 rm 幂等清干净。注入是真注入:Windows 上
// 以 share=0 句柄占用 DEK 文件(删除撞 sharing violation),Unix 上剥掉 DEK
// 父目录写权(删除撞 EACCES)。
func TestRemoveInstance_Partial_DirOk_DekFail(t *testing.T) {
	userDir, _ := removeTestEnv(t)
	slotDir, dekPath := seedInstanceWithRealDek(t, userDir, "agentA")

	undo := blockDekDelete(t, dekPath)
	rmErr := RemoveInstance("agentA")
	if rmErr == nil {
		undo()
		t.Fatal("injected DEK delete failure must surface")
	}
	undo()
	if !strings.Contains(rmErr.Error(), dekPath) {
		t.Fatalf("error must carry the DEK residue path:\n%v", rmErr)
	}
	if !strings.Contains(rmErr.Error(), "idempotent") {
		t.Fatalf("error must state the idempotent re-run path:\n%v", rmErr)
	}
	if _, serr := os.Stat(slotDir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("slot dir must already be gone in this direction (stat err=%v)", serr)
	}
	// 重跑:解除注入后,幂等清干净。
	if err := RemoveInstance("agentA"); err != nil {
		t.Fatalf("re-run after undo must finish the cleanup: %v", err)
	}
	if _, serr := os.Stat(dekPath); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("DEK must be gone after the re-run (stat err=%v)", serr)
	}
}

// TestRemoveInstance_Partial_DekOk_DirFail:反向注入 —— DEK 已删、目录删失败
// → 错误含残留物(槽目录);解除后重跑清干净。
func TestRemoveInstance_Partial_DekOk_DirFail(t *testing.T) {
	userDir, _ := removeTestEnv(t)
	slotDir, dekPath := seedInstanceWithRealDek(t, userDir, "agentA")

	undo := blockDirRemove(t, slotDir)
	rmErr := RemoveInstance("agentA")
	undo()
	if rmErr == nil {
		t.Fatal("injected dir removal failure must surface")
	}
	if !strings.Contains(rmErr.Error(), slotDir) {
		t.Fatalf("error must carry the slot-dir residue path:\n%v", rmErr)
	}
	if _, serr := os.Stat(dekPath); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("DEK must already be gone in this direction (stat err=%v)", serr)
	}
	if err := RemoveInstance("agentA"); err != nil {
		t.Fatalf("re-run after undo must finish the cleanup: %v", err)
	}
	if _, serr := os.Stat(slotDir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("slot dir must be gone after the re-run (stat err=%v)", serr)
	}
}

// blockingDEK 把 DekProvider 的 Delete 卡在测试控制的通道上 —— RemoveInstance
// 被钉在临界区内(独占持锁),供互斥断言在确定时点探测。path 非空时 Delete
// 真删该文件(模拟一次成功的删除,残留物审计才能如实在场)。
type blockingDEK struct {
	store.MemKeyProvider
	path    string
	entered chan struct{}
	release chan struct{}
}

func (b *blockingDEK) Delete() error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	if b.path == "" {
		return nil
	}
	return os.Remove(b.path)
}

// TestRemoveInstance_ExcludesConcurrentPull(互斥断言,rm 侧持锁):rm 卡在
// Delete 临界区内时,并发 DoPull 的写盘段被【拒】(明确错误,非等待非交错);
// rm 完成后同参数 pull 正常 —— 锁无泄漏。拒绝而非排队的 plan 定案由此钉死。
func TestRemoveInstance_ExcludesConcurrentPull(t *testing.T) {
	userDir, _ := removeTestEnv(t)
	// 只种槽目录(DEK 走 blockingDEK seam,不种真文件)。
	slotDir := filepath.Join(userDir, "ssh-manager", "instances", "agentA")
	if err := os.MkdirAll(slotDir, 0o700); err != nil {
		t.Fatal(err)
	}

	name := "agentA"
	b := &blockingDEK{entered: make(chan struct{}, 1), release: make(chan struct{})}
	prev := DekProvider
	DekProvider = func(string) store.KeyProvider { return b }
	t.Cleanup(func() { DekProvider = prev })

	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(&name))

	errCh := make(chan error, 1)
	go func() { errCh <- RemoveInstance("agentA") }()
	<-b.entered // rm 已进临界区(槽目录此刻已删,卡在 DEK Delete 上)

	if _, pullErr := DoPull(url, "code", pin, PullOpts{Instance: "agentA"}); pullErr == nil ||
		!(strings.Contains(pullErr.Error(), "refused") && strings.Contains(pullErr.Error(), "rm")) {
		t.Fatalf("pull during rm must be REFUSED with the rm hint, got: %v", pullErr)
	}
	close(b.release)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RemoveInstance must complete after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveInstance did not return after release (deadlock?)")
	}
	// 锁已释放:同参数 pull 正常(拒绝语义不留死锁/泄漏)。
	if _, pullErr := DoPull(url, "code", pin, PullOpts{Instance: "agentA"}); pullErr != nil {
		t.Fatalf("pull after rm finished must succeed: %v", pullErr)
	}
}

// TestWriteGate_DirectLock(锁共享钉):直接独占 cacheWriteMu(等价于 rm/force
// 持锁)时 DoPull 被拒;解锁后恢复 —— 钉「DoPull 检查的是 rm 所持同一把锁」。
func TestWriteGate_DirectLock(t *testing.T) {
	removeTestEnv(t)
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(nil))

	cacheWriteMu.Lock()
	if _, err := DoPull(url, "code", pin, PullOpts{}); err == nil || !strings.Contains(err.Error(), "refused") {
		cacheWriteMu.Unlock()
		t.Fatalf("DoPull under an exclusive write gate must be refused: %v", err)
	}
	cacheWriteMu.Unlock()
	if _, err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("DoPull after unlock must succeed: %v", err)
	}
}

// TestWriteAndPull_Gate(pair 侧互斥断言):rm(等价地,独占持锁)进行中,
// WriteAndPull 的写盘段被拒;解锁后全链照常成功。
func TestWriteAndPull_Gate(t *testing.T) {
	srv := newPairingServer(t)
	userDir, _ := removeTestEnv(t)
	ctx := context.Background()

	s, err := NewPairSession(sessionOpts(srv.url, srv.spki, "gate-laptop"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enroll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitApproval(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx); err != nil {
		t.Fatal(err)
	}

	cacheWriteMu.Lock()
	_, werr := s.WriteAndPull(ctx)
	cacheWriteMu.Unlock()
	if werr == nil || !strings.Contains(werr.Error(), "refused") {
		t.Fatalf("WriteAndPull under an exclusive write gate must be refused: %v", werr)
	}
	slotDir := filepath.Join(userDir, "ssh-manager", "instances", "gate-laptop")
	if _, serr := os.Stat(slotDir); serr == nil {
		t.Fatal("the refused WriteAndPull must not have written anything")
	}
	// 解锁后全链成功(gate 不破坏既有语义)。
	res, err := s.WriteAndPull(ctx)
	if err != nil {
		t.Fatalf("WriteAndPull after unlock: %v", err)
	}
	if res.Instance != "gate-laptop" {
		t.Fatalf("res.Instance = %q", res.Instance)
	}
}

// TestDoPull_NoGateWhenNoRm(无误伤钉):没有 rm 在途时,连续两次 pull 都成功
// —— TryRLock 语义不得让普通并发 pull 莫名被拒。
func TestDoPull_NoGateWhenNoRm(t *testing.T) {
	removeTestEnv(t)
	url, pin := newPinnedTLSServer(t, deviceSnapshotHandler(nil))
	for i := 0; i < 2; i++ {
		if _, err := DoPull(url, "code", pin, PullOpts{}); err != nil {
			t.Fatalf("pull #%d without any rm in flight must succeed: %v", i+1, err)
		}
	}
}
