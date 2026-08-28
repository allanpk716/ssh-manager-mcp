package cli

import (
	"bytes"
	"strings"
	"testing"
)

// Plan 42 批1 T7 —— pair 命令的 CLI 层冒烟:注册、flag 全集、TOFU 默认拒经
// CLI 透传(不触网;clientops 层的冻结文案)。

func TestPairCmd_RegisteredWithFlags(t *testing.T) {
	root := newRootForTest(t)
	pair, _, err := root.Find([]string{"pair"})
	if err != nil {
		t.Fatal("pair subcommand not registered:", err)
	}
	for _, flag := range []string{"url", "pin", "allow-tofu", "profile-hint", "write-mcp", "instance", "force"} {
		if pair.Flags().Lookup(flag) == nil {
			t.Errorf("pair missing --%s flag", flag)
		}
	}
}

func TestPairCmd_TOFURefusedThroughCLI(t *testing.T) {
	// 不设 SSHMGR_CACHE_DIR(--instance 与之互斥);TOFU 拒绝发生在任何 IO 之前。
	root := NewRootCmd()
	var out, errb bytes.Buffer
	root.SetArgs([]string{"pair", "--url", "https://127.0.0.1:7878", "--instance", "tofu-cli"})
	root.SetOut(&out)
	root.SetErr(&errb)
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing TOFU") {
		t.Fatalf("CLI must surface the frozen TOFU refusal, got %v", err)
	}
}

func TestPairCmd_InstanceRequired(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"pair", "--url", "https://127.0.0.1:7878"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--instance is required") {
		t.Fatalf("pair without --instance must be refused, got %v", err)
	}
}

func TestPairCmd_BadPinHardError(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"pair", "--url", "https://127.0.0.1:7878", "--instance", "x", "--pin", "not-a-pin"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not a valid sha256") {
		t.Fatalf("a pin-shaped-but-invalid value must hard-fail before any network, got %v", err)
	}
}
