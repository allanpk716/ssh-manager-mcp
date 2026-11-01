package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/paths"
)

// IsolateDirs 把用户配置目录与 ssh-manager 的系统级文件目录全部指向测试
// 临时目录，并返回 vault 目录、用户配置目录。HOME、XDG_CONFIG_HOME 与
// APPDATA 指向同一个实际的用户配置目录，使三个平台的 os.UserConfigDir
// 解析结果一致；SSHMGR_CACHE_DEK_DIR 等 paths 包环境变量覆盖 /var/lib、
// C:\ProgramData 一类的系统默认位置。
func IsolateDirs(t *testing.T) (vaultDir, userDir string) {
	t.Helper()
	root := t.TempDir()
	vaultDir = filepath.Join(root, "vault")
	// macOS 忽略 XDG_CONFIG_HOME，只根据 HOME 拼出 Library/Application
	// Support；把另外两个平台的环境变量也指到这个目录，三个系统就共用同
	// 一份隔离后的用户配置树。
	userDir = filepath.Join(root, "Library", "Application Support")
	if err := os.MkdirAll(vaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("APPDATA", userDir)

	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, paths.StoreFilename))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(vaultDir, paths.MasterKeyFilename))
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv(paths.CacheDekDirEnv, vaultDir)
	t.Setenv("SSHMGR_SERVE_LOG", filepath.Join(vaultDir, paths.ServeLogFilename))
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(vaultDir, paths.ServeCertFilename))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(vaultDir, paths.ServeKeyFilename))
	t.Setenv("SSHMGR_SERVE_MARKER", filepath.Join(vaultDir, paths.ServeCertMarkerFilename))
	return vaultDir, userDir
}
