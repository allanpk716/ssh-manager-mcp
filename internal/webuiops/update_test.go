package webuiops

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/updater"
	"ssh-manager-mcp/internal/webui"
)

func newTestUpdateService(t *testing.T) *ops.UpdateService {
	// 指到不可用的回环端口，检查里的 GitHub 最新版本查询立即失败，测试不
	// 访问真实 GitHub。
	t.Setenv("SSHMGR_UPDATE_BASE", "http://127.0.0.1:1")
	return ops.NewUpdateService(ops.UpdateDependencies{
		CurrentVersion: func() string { return "0.1.0" },
		ClientState: func(instance string) (ops.UpdateClientState, error) {
			return ops.UpdateClientState{IsClient: true}, nil
		},
	})
}

func TestUpdatesOnlyAdapterPassesInstanceAndMapsCheckFailure(t *testing.T) {
	adapter := ClientAuditStoreWithUpdates(newTestUpdateService(t))

	status, err := adapter.CheckUpdate("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsClient || status.CanUpgrade || !strings.Contains(status.Reason, "先同步或先升级权威端") {
		t.Fatalf("客户端读不到推荐版本的检查结果不符：%+v", status)
	}

	_, applyErr := adapter.ApplyUpdate("laptop", "127.0.0.1")
	var updateErr *webui.OpsUpdateError
	if !errors.As(applyErr, &updateErr) {
		t.Fatalf("应返回带阶段的升级错误，实际 %v", applyErr)
	}
	if updateErr.Step != webui.OpsUpdateStepCheck || updateErr.Message == "" {
		t.Fatalf("错误阶段与消息不符：%+v", updateErr)
	}
}

func TestUpdatesAdapterMapsInProgress(t *testing.T) {
	release := make(chan struct{})
	svc := ops.NewUpdateService(ops.UpdateDependencies{
		CurrentVersion: func() string { return "0.1.0" },
		ClientState: func(string) (ops.UpdateClientState, error) {
			return ops.UpdateClientState{IsClient: true, RecommendedVersion: "v0.2.0"}, nil
		},
		LatestRelease: func(context.Context) (*updater.Release, error) {
			return &updater.Release{
				Tag: "v0.2.0", AssetName: "asset.tar.gz",
				AssetURL: "https://127.0.0.1/asset.tar.gz", ChecksumsURL: "https://127.0.0.1/checksums.txt",
			}, nil
		},
		ReleaseByTag: func(_ context.Context, tag string) (*updater.Release, error) {
			return &updater.Release{
				Tag: tag, AssetName: "asset.tar.gz",
				AssetURL: "https://127.0.0.1/asset.tar.gz", ChecksumsURL: "https://127.0.0.1/checksums.txt",
			}, nil
		},
		DownloadAsset: func(context.Context, string, string, string) (string, error) {
			return "", errors.New("blocked")
		},
	})
	svc.SetDownloadPause(func() { <-release })
	adapter := ClientAuditStoreWithUpdates(svc)

	done := make(chan error, 1)
	go func() {
		_, err := adapter.ApplyUpdate("", "127.0.0.1")
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if svc.UpgradeInProgress() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待第一个升级开始超时")
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, err := adapter.ApplyUpdate("", "127.0.0.1")
	var updateErr *webui.OpsUpdateError
	if !errors.As(err, &updateErr) || updateErr.Step != webui.OpsUpdateStepInProgress {
		t.Fatalf("并发提交应映射为进行中错误，实际 %v", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("第一个升级本应失败于下载，实际成功")
	}
}

func TestFromOpenerWithUpdatesKeepsAuditQueries(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.WriteAudit(store.OwnerAuditRow("update.apply", map[string]any{"result": "ok"})); err != nil {
		t.Fatal(err)
	}
	adapter := FromOpenerWithUpdates(func() (*ops.Manager, func(), error) {
		return ops.New(st), func() {}, nil
	}, newTestUpdateService(t))
	rows, err := adapter.QueryAudit(webui.OpsAuditQuery{Actions: []string{"update.apply"}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "update.apply" {
		t.Fatalf("升级适配不应影响审计查询：%+v", rows)
	}
}
