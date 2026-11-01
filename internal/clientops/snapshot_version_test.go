package clientops

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// revisionAuthority 是测试用的权威端桩:配置放在真实 store 里,快照
// 修订号与导出内容都来自真实导出路径;/snapshot/version 与 /snapshot 的
// 请求次数和请求头被记录下来供断言。
type revisionAuthority struct {
	st           *store.Store
	profileID    string
	url          string
	pin          string
	versionHits  atomic.Int32
	snapshotHits atomic.Int32
	versionCode  atomic.Int32
	mu           sync.Mutex
	authHeader   string
	clientHeader string
}

func newRevisionAuthority(t *testing.T) *revisionAuthority {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "authority.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	serverID, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	a := &revisionAuthority{st: st, profileID: profileID}
	handler := func(w http.ResponseWriter, req *http.Request) {
		a.mu.Lock()
		a.authHeader = req.Header.Get("Authorization")
		a.clientHeader = req.Header.Get(buildinfo.ClientVersionHeader)
		a.mu.Unlock()
		switch req.URL.Path {
		case "/snapshot/version":
			a.versionHits.Add(1)
			if code := int(a.versionCode.Load()); code != 0 {
				w.WriteHeader(code)
				return
			}
			revision, rerr := st.SnapshotRevision()
			if rerr != nil {
				t.Errorf("读取快照修订号:%v", rerr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				SnapshotRevision int64 `json:"snapshot_revision"`
			}{revision})
		case "/snapshot":
			a.snapshotHits.Add(1)
			snap, serr := st.ExportSnapshotForProfile(profileID, true)
			if serr != nil {
				t.Errorf("导出快照:%v", serr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snap)
		default:
			http.NotFound(w, req)
		}
	}
	url, pin := newPinnedTLSServer(t, handler)
	a.url, a.pin = url, pin
	return a
}

func (a *revisionAuthority) revision(t *testing.T) int64 {
	t.Helper()
	revision, err := a.st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func (a *revisionAuthority) headers() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authHeader, a.clientHeader
}

// revisionCacheFixture 建立默认实例的真实加密缓存并写入拉取凭据。
type revisionCacheFixture struct {
	dir string
	a   *revisionAuthority
}

func newRevisionCacheFixture(t *testing.T) *revisionCacheFixture {
	t.Helper()
	mem := withDEK(t)
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	f := &revisionCacheFixture{dir: t.TempDir(), a: newRevisionAuthority(t)}
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": f.dir})
	if err := WriteCacheCred(&CacheCred{URL: f.a.url, Token: "device-code", Pin: f.a.pin}); err != nil {
		t.Fatal(err)
	}
	if _, err := DoPull(f.a.url, "device-code", f.a.pin, PullOpts{}); err != nil {
		t.Fatalf("初次拉取:%v", err)
	}
	return f
}

func (f *revisionCacheFixture) ageCache(t *testing.T, age time.Duration) {
	t.Helper()
	bin := filepath.Join(f.dir, "cache.bin")
	old := time.Now().Add(-2 * age)
	if err := os.Chtimes(bin, old, old); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(f.dir, "cache.meta.json")
	meta, err := readCacheMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	meta.PulledAt = old.Unix()
	blob, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	ResetLazyPullBackoffForTest()
}

func (f *revisionCacheFixture) meta(t *testing.T) cacheMeta {
	t.Helper()
	meta, err := readCacheMeta(filepath.Join(f.dir, "cache.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func (f *revisionCacheFixture) loaded(t *testing.T) *store.Snapshot {
	t.Helper()
	snap, err := LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestMaybeLazyPull_RevisionUnchangedSkipsDownload: 到期保鲜先查修订号;修订
// 号没变就不下载全量快照,只刷新缓存元数据里的最后联系时间。
func TestMaybeLazyPull_RevisionUnchangedSkipsDownload(t *testing.T) {
	f := newRevisionCacheFixture(t)
	f.ageCache(t, time.Minute)
	before := f.meta(t).PulledAt

	if err := MaybeLazyPullFor("", time.Minute); err != nil {
		t.Fatalf("到期保鲜:%v", err)
	}
	if got := f.a.versionHits.Load(); got != 1 {
		t.Fatalf("修订号查询次数 = %d,应为 1", got)
	}
	if got := f.a.snapshotHits.Load(); got != 1 {
		t.Fatalf("全量快照请求次数 = %d,应保持 1(初次拉取)", got)
	}
	after := f.meta(t)
	if after.PulledAt <= before {
		t.Fatalf("最后联系时间未刷新:%d -> %d", before, after.PulledAt)
	}
	if !after.ServerAnchored {
		t.Fatal("刷新后的最后联系时间应保持来自权威端时间")
	}
	auth, client := f.a.headers()
	if auth != "Bearer device-code" {
		t.Fatalf("设备码请求头 = %q,应为 Bearer device-code", auth)
	}
	if client != buildinfo.Version {
		t.Fatalf("客户端版本请求头 = %q,应为 %q", client, buildinfo.Version)
	}
}

// TestMaybeLazyPull_RevisionChangedPullsSnapshot: 修订号变化后才拉取全量
// 快照,并把新修订号写进加密缓存。
func TestMaybeLazyPull_RevisionChangedPullsSnapshot(t *testing.T) {
	f := newRevisionCacheFixture(t)
	f.ageCache(t, time.Minute)
	if _, err := f.a.st.AddServer(&models.Server{Name: "extra", Host: "192.0.2.11", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	if err := MaybeLazyPullFor("", time.Minute); err != nil {
		t.Fatalf("到期保鲜:%v", err)
	}
	if got := f.a.snapshotHits.Load(); got != 2 {
		t.Fatalf("全量快照请求次数 = %d,应为 2", got)
	}
	want := f.a.revision(t)
	if got := f.loaded(t).SnapshotRevision; got != want {
		t.Fatalf("缓存中的快照修订号 = %d,应为 %d", got, want)
	}
}

// TestMaybeLazyPull_LegacyCacheWithoutRevisionPulls: 旧缓存没有修订号时按 0
// 处理,第一次到期保鲜会拉取全量快照。
func TestMaybeLazyPull_LegacyCacheWithoutRevisionPulls(t *testing.T) {
	f := newRevisionCacheFixture(t)
	legacy := `{"version":1,"credentials":[],"servers":[],"profiles":[],"grants":[],"projects":[],"host_keys":[],"audit":[]}`
	dek, err := loadDEK("")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.EncryptWithKey(dek, []byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	f.ageCache(t, time.Minute)
	if err := MaybeLazyPullFor("", time.Minute); err != nil {
		t.Fatalf("到期保鲜:%v", err)
	}
	if got := f.a.versionHits.Load(); got != 1 {
		t.Fatalf("修订号查询次数 = %d,应为 1", got)
	}
	want := f.a.revision(t)
	if got := f.a.snapshotHits.Load(); got != 2 {
		t.Fatalf("全量快照请求次数 = %d,应为 2", got)
	}
	if got := f.loaded(t).SnapshotRevision; got != want {
		t.Fatalf("缓存中的快照修订号 = %d,应为 %d", got, want)
	}
}

// TestMaybeLazyPull_Version404FallsBackToFullPull: 旧权威端没有修订号接口,
// 到期保鲜回到原来的直接全量拉取。
func TestMaybeLazyPull_Version404FallsBackToFullPull(t *testing.T) {
	f := newRevisionCacheFixture(t)
	f.ageCache(t, time.Minute)
	f.a.versionCode.Store(http.StatusNotFound)
	if err := MaybeLazyPullFor("", time.Minute); err != nil {
		t.Fatalf("到期保鲜:%v", err)
	}
	if got := f.a.snapshotHits.Load(); got != 2 {
		t.Fatalf("全量快照请求次数 = %d,应为 2(404 回退)", got)
	}
}

// TestMaybeLazyPull_VersionFailureKeepsCache: 修订号查询失败时继续使用当前
// 快照,不下载,也不改动缓存。
func TestMaybeLazyPull_VersionFailureKeepsCache(t *testing.T) {
	f := newRevisionCacheFixture(t)
	f.ageCache(t, time.Minute)
	f.a.versionCode.Store(http.StatusInternalServerError)
	before, err := os.ReadFile(filepath.Join(f.dir, "cache.bin"))
	if err != nil {
		t.Fatal(err)
	}
	pulledBefore := f.meta(t).PulledAt

	err = MaybeLazyPullFor("", time.Minute)
	if err == nil {
		t.Fatal("查询失败应返回错误交给调用方记录日志")
	}
	if got := f.a.snapshotHits.Load(); got != 1 {
		t.Fatalf("全量快照请求次数 = %d,应保持 1", got)
	}
	after, err := os.ReadFile(filepath.Join(f.dir, "cache.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("查询失败不得改写加密缓存")
	}
	if got := f.meta(t).PulledAt; got != pulledBefore {
		t.Fatalf("最后联系时间 = %d,应保持 %d", got, pulledBefore)
	}
	if _, lerr := LoadCacheSnapshot(); lerr != nil {
		t.Fatalf("当前快照必须可继续加载:%v", lerr)
	}
}
