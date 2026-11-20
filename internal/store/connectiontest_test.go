package store

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// TestServerConnectionTestResultPersistOverwriteAndReopen 覆盖加密库公共
// 读写边界：最近一次结果按服务器条目保存，重复测试整体覆盖，关闭后重开
// 仍能读回同一份结构化结果。
func TestServerConnectionTestResultPersistOverwriteAndReopen(t *testing.T) {
	key := make([]byte, 32)
	randRead(t, key)
	database := filepath.Join(t.TempDir(), "vault.db")
	st, err := Open(database, key)
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServer(&models.Server{Name: "web", Host: "192.0.2.10", Port: 22})
	if err != nil {
		t.Fatal(err)
	}
	first := models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: true,
		SuccessAddress: "192.0.2.10", SuccessNetworkName: "办公网", Duration: 125 * time.Millisecond,
		Attempts: []models.ServerConnectionTestAttempt{{
			NetworkName: "办公网", Address: "192.0.2.10", Source: models.DialAddressSourcePhysical,
			OK: true, Duration: 120 * time.Millisecond,
		}},
	}
	if err := st.SaveServerConnectionTestResult(added, first); err != nil {
		t.Fatalf("保存最近一次连接测试结果失败：%v", err)
	}
	second := models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_100, 0), Succeeded: false,
		FailureReason: models.ConnectionTestFailurePortUnreachable,
		Duration:      240 * time.Millisecond,
		Attempts: []models.ServerConnectionTestAttempt{{
			NetworkName: "办公网", Address: "192.0.2.10", Source: models.DialAddressSourcePhysical,
			FailureReason: models.ConnectionTestFailurePortUnreachable, Duration: 40 * time.Millisecond,
		}},
	}
	if err := st.SaveServerConnectionTestResult(added, second); err != nil {
		t.Fatalf("覆盖最近一次连接测试结果失败：%v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(database, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ServerConnectionTestResult(added)
	if err != nil {
		t.Fatalf("读回最近一次连接测试结果失败：%v", err)
	}
	if got == nil || !reflect.DeepEqual(*got, second) {
		t.Fatalf("重开后应读回覆盖后的结果，得到 %+v，想要 %+v", got, second)
	}
}

// TestServerConnectionTestResultDeletedWithServer 覆盖条目删除的公共边界：
// 删除服务器条目时，它的最近一次连接测试结果同步清理。
func TestServerConnectionTestResultDeletedWithServer(t *testing.T) {
	st := newTestStore(t)
	added, err := st.AddServer(&models.Server{Name: "web", Host: "192.0.2.10", Port: 22})
	if err != nil {
		t.Fatal(err)
	}
	result := models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: true,
		SuccessAddress: "192.0.2.10", SuccessNetworkName: "办公网",
		Duration: 125 * time.Millisecond,
	}
	if err := st.SaveServerConnectionTestResult(added, result); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServerCascading(added); err != nil {
		t.Fatal(err)
	}
	got, err := st.ServerConnectionTestResult(added)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("删除服务器条目后不应残留连接测试结果：%+v", got)
	}
}

// TestServerConnectionTestResultsBatchRead 覆盖列表用的批量读取边界：
// 一次调用带回多个服务器条目的最近结果，从未测试的条目不出现在返回值里。
func TestServerConnectionTestResultsBatchRead(t *testing.T) {
	st := newTestStore(t)
	tested := make([]string, 0, 2)
	for name, succeeded := range map[string]bool{"web": true, "db": false} {
		id, err := st.AddServer(&models.Server{Name: name, Host: "192.0.2.10", Port: 22})
		if err != nil {
			t.Fatal(err)
		}
		result := models.ServerConnectionTestResult{
			CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: succeeded,
			Duration: 90 * time.Millisecond,
		}
		if succeeded {
			result.SuccessAddress = "192.0.2.10"
			result.SuccessNetworkName = "办公网"
		} else {
			result.FailureReason = models.ConnectionTestFailureTimeout
		}
		if err := st.SaveServerConnectionTestResult(id, result); err != nil {
			t.Fatal(err)
		}
		tested = append(tested, id)
	}
	neverTested, err := st.AddServer(&models.Server{Name: "cache", Host: "192.0.2.12", Port: 22})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.ServerConnectionTestResults(append([]string{neverTested}, tested...))
	if err != nil {
		t.Fatalf("批量读取连接测试结果失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("批量结果应只包含两台已测试服务器，实际 %d 台：%+v", len(got), got)
	}
	for _, id := range tested {
		result := got[id]
		if result == nil || result.CompletedAt.IsZero() || result.Duration != 90*time.Millisecond {
			t.Fatalf("服务器 %s 的批量结果不完整：%+v", id, result)
		}
	}
	if got[neverTested] != nil {
		t.Fatalf("从未测试的服务器不应出现在批量结果里：%+v", got[neverTested])
	}

	empty, err := st.ServerConnectionTestResults(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空列表应返回空结果且不报错：%v %+v", err, empty)
	}
}

// TestOpenAddsServerConnectionTestResultTableToOldStore 覆盖旧库升级：打
// 开缺少连接测试结果表的既有加密库时自动补表。
func TestOpenAddsServerConnectionTestResultTableToOldStore(t *testing.T) {
	key := make([]byte, 32)
	randRead(t, key)
	database := filepath.Join(t.TempDir(), "old-vault.db")
	st, err := Open(database, key)
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServer(&models.Server{Name: "web", Host: "192.0.2.10", Port: 22})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DROP TABLE server_connection_test_results`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(database, key)
	if err != nil {
		t.Fatalf("旧库升级失败：%v", err)
	}
	defer reopened.Close()
	result := models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: true,
		SuccessAddress: "192.0.2.10", SuccessSource: models.DialAddressSourceFallback,
	}
	if err := reopened.SaveServerConnectionTestResult(added, result); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.ServerConnectionTestResult(added)
	if err != nil || got == nil || got.SuccessSource != models.DialAddressSourceFallback {
		t.Fatalf("升级后的库应能保存并读回结果：%v %+v", err, got)
	}
}
