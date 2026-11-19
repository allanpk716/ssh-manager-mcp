package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
)

// TestDropInheritedCacheDirOverride:领养启动(热升级的继任)时,父桥进程
// 遗传下来的 SSHMGR_CACHE_DIR(父桥运行态为自己固定的实例目录)必须被剥
// 除——继任继承的命令行带 --instance,两者互斥会让继任启动即被拒、换代
// 永远失败。非领养启动、命令行无 --instance、或环境本就没有该变量时,
// 不得动它。
func TestDropInheritedCacheDirOverride(t *testing.T) {
	cases := []struct {
		name     string
		instance string
		adopted  bool
		cacheDir string
		wantDrop bool
	}{
		{"领养启动剥除遗传值", "mini6", true, "/some/instance/dir", true},
		{"非领养启动不动", "mini6", false, "/some/instance/dir", false},
		{"命令行无 --instance 不动", "", true, "/some/instance/dir", false},
		{"领养启动但本就没有该变量", "mini6", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": tc.cacheDir})
			if tc.adopted {
				// 任意 SSHMGR_HOTSWAP_* 键存在即领养启动。
				t.Setenv("SSHMGR_HOTSWAP_READY", "/tmp/adopted-probe-ready.json")
			}
			dropped, err := dropInheritedCacheDirOverride(tc.instance)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dropped != tc.wantDrop {
				t.Fatalf("dropped = %v, want %v", dropped, tc.wantDrop)
			}
			wantVal := tc.cacheDir
			if tc.wantDrop {
				wantVal = "" // 剥除后必须为空
			}
			if got := os.Getenv("SSHMGR_CACHE_DIR"); got != wantVal {
				t.Fatalf("SSHMGR_CACHE_DIR = %q, want %q", got, wantVal)
			}
		})
	}
}

// TestMcpAdoptedStartSurvivesInheritedCacheDir:端到端回归——领养启动 +
// 遗传 SSHMGR_CACHE_DIR + 命令行 --instance 时,mcp 命令不得再死在
// 「--instance 与 SSHMGR_CACHE_DIR 互斥」的启动校验上(会因别的原因失败,
// 比如隔离目录里没有该实例的缓存,那正是本测试预期的终点)。
func TestMcpAdoptedStartSurvivesInheritedCacheDir(t *testing.T) {
	// SSHMGR_CACHE_DEK_DIR 是目录级缝:整棵实例树(含实例缓存)从它派生,
	// 指到临时目录即与本机真实实例隔离。
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR":     t.TempDir(),
		"SSHMGR_CACHE_DEK_DIR": t.TempDir(),
		"SSHMGR_HOTSWAP_READY": filepath.Join(t.TempDir(), "ready.json"),
		"SSHMGR_TOKEN":         "probe-token",
	})
	root := NewRootCmd()
	root.SetArgs([]string{"mcp", "--cache", "--instance", "probe-adopted"})
	err := root.Execute()
	if err == nil {
		t.Fatal("隔离目录里没有实例缓存,命令必须失败(但不是死于互斥校验)")
	}
	if strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("领养启动不得再因互斥校验被拒: %v", err)
	}
}

// TestBuildEnvKeepsHotswapContract:锁住 BuildEnv 的既有契约——它只剥
// SSHMGR_HOTSWAP_* 遗留,别的一律透传;本修复选择在 mcp 命令层剥遗传的
// SSHMGR_CACHE_DIR 而不是在 BuildEnv 里,依赖的就是这个契约。
func TestBuildEnvKeepsHotswapContract(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"SSHMGR_CACHE_DIR=/parent/resolved/instance",
		"SSHMGR_HOTSWAP_STALE=old",
	}
	out, err := hotswap.BuildEnv(base, hotswap.Session{"k": "v"}, "/tmp/r.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "SSHMGR_HOTSWAP_STALE") {
		t.Fatal("上一轮领养遗留必须剥除")
	}
	if !strings.Contains(joined, "SSHMGR_CACHE_DIR=/parent/resolved/instance") {
		t.Fatal("BuildEnv 必须透传非 SSHMGR_HOTSWAP_* 变量(剥遗传值是 mcp 命令层的职责)")
	}
	if !strings.Contains(joined, "SSHMGR_HOTSWAP_K=v") {
		t.Fatal("会话键必须注入")
	}
}
