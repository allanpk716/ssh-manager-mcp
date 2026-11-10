package clientops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
)

// TestMountShareSnapshotSyncer_BehindAuthorityPulls：挂载前保鲜发现本地
// 缓存落后于权威端时，先完成全量同步，再返回权威端修订号。
func TestMountShareSnapshotSyncer_BehindAuthorityPulls(t *testing.T) {
	f := newRevisionCacheFixture(t)
	if _, err := f.a.st.AddServer(&models.Server{
		Name: "extra", Host: "192.0.2.11", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	syncer, err := NewMountShareSnapshotSyncerFor("")
	if err != nil {
		t.Fatal(err)
	}

	revision, err := syncer.SyncBeforeMount(context.Background())
	if err != nil {
		t.Fatalf("挂载前同步失败：%v", err)
	}
	want := f.a.revision(t)
	if revision != want {
		t.Fatalf("返回的权威端修订号 = %d，应为 %d", revision, want)
	}
	if got := f.a.snapshotHits.Load(); got != 2 {
		t.Fatalf("全量快照请求次数 = %d，应为 2（初次拉取与挂载前同步）", got)
	}
	if got := f.loaded(t).SnapshotRevision; got != want {
		t.Fatalf("同步后的缓存修订号 = %d，应为 %d", got, want)
	}
}

// TestMountShareSnapshotSyncer_CurrentSkipsPull：本地缓存与权威端一致时，
// 挂载前保鲜只查修订号，不下载全量快照。
func TestMountShareSnapshotSyncer_CurrentSkipsPull(t *testing.T) {
	f := newRevisionCacheFixture(t)
	syncer, err := NewMountShareSnapshotSyncerFor("")
	if err != nil {
		t.Fatal(err)
	}

	revision, err := syncer.SyncBeforeMount(context.Background())
	if err != nil {
		t.Fatalf("挂载前同步失败：%v", err)
	}
	if revision != f.a.revision(t) {
		t.Fatalf("返回的权威端修订号 = %d，应为 %d", revision, f.a.revision(t))
	}
	if got := f.a.versionHits.Load(); got != 1 {
		t.Fatalf("修订号查询次数 = %d，应为 1", got)
	}
	if got := f.a.snapshotHits.Load(); got != 1 {
		t.Fatalf("全量快照请求次数 = %d，应保持 1（初次拉取）", got)
	}
}

// TestMountShareSnapshotSyncer_FailuresReturnError：旧权威端没有修订号接口、
// 权威端不可达、缺少拉取凭据时，挂载前保鲜返回错误，交给工具层继续使用
// 本地快照并标记可能过期。
func TestMountShareSnapshotSyncer_FailuresReturnError(t *testing.T) {
	t.Run("旧权威端", func(t *testing.T) {
		f := newRevisionCacheFixture(t)
		f.a.versionCode.Store(404)
		syncer, err := NewMountShareSnapshotSyncerFor("")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := syncer.SyncBeforeMount(context.Background()); err == nil {
			t.Fatal("旧权威端没有修订号接口时应返回错误")
		}
		if got := f.a.snapshotHits.Load(); got != 1 {
			t.Fatalf("查询失败不应触发全量拉取，请求次数 = %d", got)
		}
	})

	t.Run("权威端不可达", func(t *testing.T) {
		newRevisionCacheFixture(t)
		closed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url, pin := closed.URL, mcpserver.SPKIFingerprint(closed.Certificate())
		closed.Close()
		if err := WriteCacheCred(&CacheCred{URL: url, Token: "device-code", Pin: pin}); err != nil {
			t.Fatal(err)
		}
		syncer, err := NewMountShareSnapshotSyncerFor("")
		if err != nil {
			t.Fatal(err)
		}
		_, err = syncer.SyncBeforeMount(context.Background())
		if err == nil || !strings.Contains(err.Error(), "query snapshot revision") {
			t.Fatalf("权威端不可达应返回查询错误，实际：%v", err)
		}
	})

	t.Run("缺少拉取凭据", func(t *testing.T) {
		newRevisionCacheFixture(t)
		credPath, err := CacheCredPathFor("")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(credPath); err != nil {
			t.Fatal(err)
		}
		syncer, err := NewMountShareSnapshotSyncerFor("")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := syncer.SyncBeforeMount(context.Background()); err == nil {
			t.Fatal("缺少拉取凭据时应返回错误")
		}
	})
}
