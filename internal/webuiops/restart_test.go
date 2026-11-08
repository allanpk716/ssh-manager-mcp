package webuiops

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

type fakeAuthorityServiceManager struct {
	calls int
}

func (f *fakeAuthorityServiceManager) Restart() error {
	f.calls++
	return nil
}

func TestAuthorityAdapterRestartsServiceAndWritesWebAudit(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	vaultStore, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer vaultStore.Close()
	manager := ops.New(vaultStore)
	serviceManager := &fakeAuthorityServiceManager{}
	restarts := ops.NewRestartService(ops.RestartDependencies{
		NewService: func() (ops.ServiceRestarter, error) { return serviceManager, nil },
		Audit:      manager.WriteRestartAudit,
	})
	adapter := FromManagerWithAuthorityUpdates(manager, newTestUpdateService(t), restarts)

	restartsBeforeSchedule := -1
	restartAdapter, ok := adapter.(webui.OpsRestartStore)
	if !ok {
		t.Fatal("权威端适配器应提供重启接口")
	}
	err = restartAdapter.RequestServiceRestart("198.51.100.9", func(restart func()) {
		restartsBeforeSchedule = serviceManager.calls
		if restartsBeforeSchedule != 0 {
			t.Fatalf("响应结束前不应重启，实际调用次数=%d", restartsBeforeSchedule)
		}
		restart()
	})
	if err != nil {
		t.Fatal(err)
	}
	if serviceManager.calls != 1 {
		t.Fatalf("调度后应重启一次，实际 %d 次", serviceManager.calls)
	}

	rows, err := adapter.QueryAudit(webui.OpsAuditQuery{Actions: []string{"service.restart"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应写入一条重启审计，实际 %+v", rows)
	}
	row := rows[0]
	if row.Source != store.AuditSourceWebUI || row.SourceIP != "198.51.100.9" ||
		row.Status != "ok" || !strings.Contains(row.Command, `"result":"ok"`) {
		t.Fatalf("重启审计来源或结果不符：%+v", row)
	}
}

func TestAuthorityAdapterReportsRestartDependencyError(t *testing.T) {
	adapter := FromManagerWithAuthorityUpdates(nil, nil, nil)
	restartAdapter, ok := adapter.(webui.OpsRestartStore)
	if !ok {
		t.Fatal("权威端适配器应提供重启接口")
	}
	err := restartAdapter.RequestServiceRestart("127.0.0.1", func(func()) {})
	if err == nil || !errors.Is(err, errRestartNotWired) {
		t.Fatalf("未接线服务管理器应返回明确错误，实际 %v", err)
	}
}
