package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ageRevisionCache 把 cache.bin 与 cache.meta.json 的最后联系时间拨回到
// maxAge 之前,并清掉进程内的自动拉取退避,让下一次 MaybeLazyPullFor 一定
// 走到期保鲜路径。
func ageRevisionCache(t *testing.T, dir string, maxAge time.Duration) {
	t.Helper()
	old := time.Now().Add(-2 * maxAge)
	if err := os.Chtimes(filepath.Join(dir, "cache.bin"), old, old); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, "cache.meta.json")
	blob, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(blob, &meta); err != nil {
		t.Fatal(err)
	}
	meta["pulled_at"] = old.Unix()
	blob, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	clientops.ResetLazyPullBackoffForTest()
}

// deviceCodeState 读取权威端设备码表的 owner 可见状态,用来证明修订号查询
// 不会改变 `cache-tokens ls` 的输出。
func deviceCodeState(t *testing.T, st *store.Store) (time.Time, string) {
	t.Helper()
	tokens, err := st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 {
		t.Fatalf("设备码数量 = %d,应为 1", len(tokens))
	}
	return tokens[0].LastPullAt, tokens[0].ClientVersion
}

// TestCacheRevisionE2E_ExpiryChecksVersionFirst: 在真实权威端 + 真实加密缓存
// 的端到端路径上验证到期保鲜:修订号不变时只查询、不下载;修订号变化才
// 下载;旧权威端 404 时回退到全量拉取;查询不污染设备码表。
func TestCacheRevisionE2E_ExpiryChecksVersionFirst(t *testing.T) {
	fx := newCacheFwdFixture(t)
	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	mem := &store.MemKeyProvider{}
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	prevDek := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prevDek })

	if err := clientops.WriteCacheCred(&fx.cred); err != nil {
		t.Fatal(err)
	}
	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{}); err != nil {
		t.Fatalf("初次拉取:%v", err)
	}
	if got := fx.snapshotCount(); got != 1 {
		t.Fatalf("初次全量快照请求 = %d,应为 1", got)
	}
	pullTime, clientVersion := deviceCodeState(t, fx.serve)
	if pullTime.IsZero() {
		t.Fatal("初次拉取后设备码应有最后拉取时间")
	}

	// 修订号未变化:只查询修订号,推进最后联系时间,不下载。
	ageRevisionCache(t, cacheDir, time.Minute)
	if err := clientops.MaybeLazyPullFor("", time.Minute); err != nil {
		t.Fatalf("到期保鲜:%v", err)
	}
	if got := fx.versionCount(); got != 1 {
		t.Fatalf("修订号查询次数 = %d,应为 1", got)
	}
	if got := fx.snapshotCount(); got != 1 {
		t.Fatalf("修订号未变化时的全量快照请求 = %d,应为 1", got)
	}
	if got := fx.seenClientVersion("/snapshot/version"); got != buildinfo.Version {
		t.Fatalf("修订号查询的客户端版本请求头 = %q,应为 %q", got, buildinfo.Version)
	}
	afterPullTime, afterClientVersion := deviceCodeState(t, fx.serve)
	if !afterPullTime.Equal(pullTime) || afterClientVersion != clientVersion {
		t.Fatalf("修订号查询改变了设备码状态:last_pull %v->%v client_version %q->%q",
			pullTime, afterPullTime, clientVersion, afterClientVersion)
	}
	loaded, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	wantRevision, err := fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SnapshotRevision != wantRevision {
		t.Fatalf("缓存快照修订号 = %d,应为 %d", loaded.SnapshotRevision, wantRevision)
	}

	// 权威端写入配置后:修订号变化,才再次全量拉取。
	if _, err := fx.serve.AddServer(&models.Server{Name: "new-box", Host: "192.0.2.50", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	ageRevisionCache(t, cacheDir, time.Minute)
	if err := clientops.MaybeLazyPullFor("", time.Minute); err != nil {
		t.Fatalf("修订号变化后的到期保鲜:%v", err)
	}
	if got := fx.versionCount(); got != 2 {
		t.Fatalf("修订号查询次数 = %d,应为 2", got)
	}
	if got := fx.snapshotCount(); got != 2 {
		t.Fatalf("修订号变化后的全量快照请求 = %d,应为 2", got)
	}
	loaded, err = clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	wantRevision, err = fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SnapshotRevision != wantRevision {
		t.Fatalf("更新后的缓存快照修订号 = %d,应为 %d", loaded.SnapshotRevision, wantRevision)
	}

	// 旧权威端返回 404:停止修订号比较,回到到期直接全量拉取。
	fx.versionNotFound.Store(true)
	ageRevisionCache(t, cacheDir, time.Minute)
	if err := clientops.MaybeLazyPullFor("", time.Minute); err != nil {
		t.Fatalf("404 回退:%v", err)
	}
	if got := fx.versionCount(); got != 3 {
		t.Fatalf("修订号查询次数 = %d,应为 3", got)
	}
	if got := fx.snapshotCount(); got != 3 {
		t.Fatalf("404 回退后的全量快照请求 = %d,应为 3", got)
	}
}
