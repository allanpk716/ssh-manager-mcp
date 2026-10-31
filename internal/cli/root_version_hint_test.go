package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// seedRootVersionCache 写出真实的本地加密缓存，之后不再联系权威端。命令测试
// 用它证明根命令提示只读本地数据，且不污染标准输出。
func seedRootVersionCacheForInstance(t *testing.T, recommendedVersion, instance string) []byte {
	t.Helper()
	userDir := t.TempDir()
	vaultDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("APPDATA", userDir)
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	if instance == "" {
		t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	} else {
		t.Setenv("SSHMGR_CACHE_DIR", "")
		t.Setenv("SSHMGR_CACHE_DEK", "")
	}

	snap := &store.Snapshot{Version: 1, RecommendedVersion: recommendedVersion}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	serve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if instance != "" {
			w.Header().Set("X-Sshmgr-Device-Name", instance)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(serve.Close)

	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	mem := &store.MemKeyProvider{}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	prev := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prev })

	pin := mcpserver.SPKIFingerprint(serve.Certificate())
	if _, err := clientops.DoPull(serve.URL, "device-code", pin, clientops.PullOpts{StatusOut: nil, Instance: instance}); err != nil {
		t.Fatalf("准备缓存拉取失败：%v", err)
	}
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	serve.Close() // 根命令执行阶段权威端不可达，提示必须仍能来自本地缓存。
	return dek
}

func seedRootVersionCache(t *testing.T, recommendedVersion string) []byte {
	return seedRootVersionCacheForInstance(t, recommendedVersion, "")
}

func TestRootCommandWritesVersionHintToStderr(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })
	_ = seedRootVersionCache(t, "1.1.0")

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "1.0.0\n" {
		t.Fatalf("标准输出 = %q，只应包含版本", stdout.String())
	}
	want := "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级\n"
	if stderr.String() != want {
		t.Fatalf("标准错误输出 = %q，应为 %q", stderr.String(), want)
	}
}

func TestRootCommandWritesVersionHintAfterCommandFailure(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })
	_ = seedRootVersionCache(t, "1.1.0")

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"version", "--unknown-flag"})
	if err := root.Execute(); err == nil {
		t.Fatal("未知参数应让命令失败")
	}
	want := "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级\n"
	if !strings.HasSuffix(stderr.String(), want) {
		t.Fatalf("命令失败后的标准错误输出应以版本提示结尾，实际为 %q", stderr.String())
	}
}

func TestRootCommandReadsOnlyNamedCacheInstance(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })
	_ = seedRootVersionCacheForInstance(t, "1.1.0", "laptop")

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级\n"
	if stderr.String() != want {
		t.Fatalf("只有一个命名实例时标准错误输出 = %q，应为 %q", stderr.String(), want)
	}
}

func TestDoctorListsVersionsWithoutRootHint(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "dev"
	t.Cleanup(func() { buildinfo.Version = saved })
	withDoctorDirs(t)
	dek := seedRootVersionCache(t, "1.1.0")
	if err := os.WriteFile(os.Getenv("SSHMGR_CACHE_DEK"), dek, 0o600); err != nil {
		t.Fatal(err)
	}
	stubServeServiceState(t, "NOT INSTALLED")

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"doctor"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor: %v\n%s", err, stdout.String())
	}
	for _, want := range []string{
		"sshmgr doctor (dev)",
		"客户端版本 dev；推荐版本 1.1.0",
	} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("doctor 输出缺少 %q：\n%s", want, stdout.String())
		}
	}
	if stderr.String() != "" {
		t.Fatalf("doctor 不应在标准错误输出重复根命令提示：%q", stderr.String())
	}
}

func TestDoctorListsOnlyNamedCacheInstanceVersion(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "dev"
	t.Cleanup(func() { buildinfo.Version = saved })
	withDoctorDirs(t)
	dek := seedRootVersionCacheForInstance(t, "1.1.0", "laptop")
	dekPath, err := paths.CacheDekPathFor("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dekPath, dek, 0o600); err != nil {
		t.Fatal(err)
	}
	stubServeServiceState(t, "NOT INSTALLED")

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"doctor"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor: %v\n%s", err, stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("客户端版本 dev；推荐版本 1.1.0")) {
		t.Fatalf("doctor 应读取唯一命名实例的推荐版本：\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("doctor 不应输出根命令提示：%q", stderr.String())
	}
}

func TestUpdateDoesNotRepeatVersionHint(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })
	seedRootVersionCache(t, "1.1.0")

	home, self, _ := writeSelfFixture(t)
	startFakeUpdateSource(t, "v1.1.0", testBinBytes(t))
	seamUpdateDefaults(t, self, "1.0.0")
	t.Cleanup(func() {
		entries, err := os.ReadDir(home)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("update --check 不应替换二进制，发现 %d 个文件", len(entries))
		}
	})

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"update", "--check"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update --check: %v\n%s", err, stdout.String())
	}
	for _, want := range []string{"1.0.0", "v1.1.0"} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("update 输出缺少 %q：\n%s", want, stdout.String())
		}
	}
	if stderr.String() != "" {
		t.Fatalf("update 不应在标准错误输出重复根命令提示：%q", stderr.String())
	}
}
