package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 种一个实例槽（bin+meta 可解）——直接用 clientops.DoPull 太重，写最小材料:
// meta + withDEK seam + LoadCacheSnapshotFor 可载的 bin。此处只验证 VIEW,
// bin 用真加密太啰嗦:改为断言"列出了实例名 + 加载错误也成行不炸"。
func TestCacheStatus_ListsAllInstances(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	for _, n := range []string{"agentA", "agentB"} {
		dir := filepath.Join(userDir, "ssh-manager", "instances", n)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "cache.meta.json"),
			[]byte(fmt.Sprintf(`{"url":"https://s","pulled_at":1,"server_anchored":true,"scoped":false,"device_name":%q}`, n)), 0o600)
		os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("not-decryptable"), 0o600) // 触发行级错误
	}
	var out bytes.Buffer
	cmd := newCacheCmd()
	cmd.SetArgs([]string{"status"})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status list mode must not fail overall: %v", err)
	}
	got := out.String()
	for _, want := range []string{"default", "agentA", "agentB", "device:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
