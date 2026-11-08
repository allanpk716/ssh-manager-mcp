package cli

import (
	"testing"

	"github.com/kardianos/service"

	"ssh-manager-mcp/internal/buildinfo"
)

func TestServeWebUIUsesExistingServiceAbstractionForRestart(t *testing.T) {
	previous := serviceNew
	defer func() { serviceNew = previous }()

	var gotConfig *service.Config
	serviceNew = func(_ service.Interface, config *service.Config) (serviceRestarter, error) {
		gotConfig = config
		return &fakeRestarter{}, nil
	}
	manager, err := newServeServiceRestarter()
	if err != nil {
		t.Fatal(err)
	}
	if manager == nil {
		t.Fatal("应返回服务管理器句柄")
	}
	if gotConfig == nil || gotConfig.Name != buildinfo.ServeServiceName || gotConfig.Option == nil {
		t.Fatalf("重启应复用 serve 服务的系统服务抽象配置：%+v", gotConfig)
	}
}
