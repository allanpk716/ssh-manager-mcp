package webui

import (
	"strings"
	"testing"
)

type stubShareRevokeRunner struct{}

func (r *stubShareRevokeRunner) StartShareRevoke(ShareRevokeRequest) (string, error) {
	return "revoke-task", nil
}

func (r *stubShareRevokeRunner) ShareRevokeStatus(string) (*ShareRevokeTaskStatus, bool) {
	return nil, false
}

type stubShareLatestCheckSource struct{}

func (s stubShareLatestCheckSource) LatestShareCheck(string) ([]ShareCheckResult, bool) {
	return nil, false
}

// TestShareRevokeRunnerRequiresLatestCheckSource 验证装配处漏传最近现场
// 探测来源时构造直接报错，而不是撤销页静默失去联动卸载能力。
func TestShareRevokeRunnerRequiresLatestCheckSource(t *testing.T) {
	_, _, err := New(Config{
		Role: RoleStandalone, Port: 18080,
		Servers: &stubServerSource{}, Access: &stubAccessStore{},
		ShareRevokeRunner: &stubShareRevokeRunner{},
	})
	if err == nil || !strings.Contains(err.Error(), "最近现场探测") {
		t.Fatalf("提供撤销授权任务却缺少最近现场探测来源应报错，实际：%v", err)
	}
}

// TestShareRevokeRunnerAcceptsExplicitLatestCheckSource 验证显式传入最近
// 现场探测来源后可以构造。
func TestShareRevokeRunnerAcceptsExplicitLatestCheckSource(t *testing.T) {
	_, _, err := New(Config{
		Role: RoleStandalone, Port: 18080,
		Servers: &stubServerSource{}, Access: &stubAccessStore{},
		ShareRevokeRunner: &stubShareRevokeRunner{},
		ShareLatestChecks: stubShareLatestCheckSource{},
	})
	if err != nil {
		t.Fatalf("显式提供最近现场探测来源后构造应成功：%v", err)
	}
}
