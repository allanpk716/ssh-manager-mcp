package cli

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"
)

// TestRootRejectsInvalidAddressTimeoutAtStartup: 每个候选 SSH 地址的短超时
// 环境变量沿用「非法值即拒绝启动」的先例，version 这类不拨号的命令也不能
// 绕过校验。
func TestRootRejectsInvalidAddressTimeoutAtStartup(t *testing.T) {
	t.Setenv("SSHMGR_SSH_ADDRESS_TIMEOUT", "not-a-duration")
	root := NewRootCmd()
	root.SetArgs([]string{"version"})

	err := root.Execute()
	if err == nil {
		t.Fatal("invalid SSHMGR_SSH_ADDRESS_TIMEOUT must reject startup")
	}
	if !strings.Contains(err.Error(), sshbroker.AddressTimeoutEnv) {
		t.Fatalf("error must name %s, got %v", sshbroker.AddressTimeoutEnv, err)
	}
}
