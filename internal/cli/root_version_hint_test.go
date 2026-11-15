package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
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
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
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

// seedAgedNamedClientCache writes one named, server-anchored cache whose pull
// time is beyond the 24-hour offline limit. The returned count observes the
// fake authority server, so a test can prove that a command made no request.
func seedAgedNamedClientCache(t *testing.T, recommendedVersion string) (requests *int) {
	t.Helper()
	userDir := t.TempDir()
	dekDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("APPDATA", userDir)
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "store.db"))
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")

	requestCount := 0
	body, err := json.Marshal(&store.Snapshot{
		Version:            1,
		RecommendedVersion: recommendedVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(authority.Close)

	instance := "server-local"
	_, bin, metaPath, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := vaultio.EncryptWithKey(dek, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, ciphertext, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"url":%q,"pulled_at":%d,"server_anchored":true,"device_name":%q}`,
		authority.URL, time.Now().Add(-25*time.Hour).Unix(), instance)
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clientops.WriteCacheCredFor(instance, &clientops.CacheCred{
		URL:   authority.URL,
		Token: "device-code",
		Pin:   "sha256:" + strings.Repeat("00", 32),
	}); err != nil {
		t.Fatal(err)
	}
	dekPath, err := paths.CacheDekPathFor(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dekPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dekPath, dek, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	return &requestCount
}

func assertAgedNamedClientCacheIntact(t *testing.T, requests *int, stderr string) {
	t.Helper()
	instance := "server-local"
	_, bin, metaPath, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		t.Fatal(err)
	}
	credPath, err := clientops.CacheCredPathFor(instance)
	if err != nil {
		t.Fatal(err)
	}
	dekPath, err := paths.CacheDekPathFor(instance)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bin, metaPath, credPath, dekPath} {
		if _, serr := os.Stat(path); serr != nil {
			t.Fatalf("%s must remain after the command: %v", filepath.Base(path), serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(filepath.Dir(bin), "quarantine")); !os.IsNotExist(serr) {
		t.Fatalf("the command must not create quarantine material: %v", serr)
	}
	if *requests != 0 {
		t.Fatalf("the command made %d authority request(s)", *requests)
	}
	if strings.Contains(stderr, "QUARANTINED") {
		t.Fatalf("the command must not quarantine the cache, standard error: %q", stderr)
	}
}

func TestReadOnlyCommandsDoNotQuarantineAgedNamedClientCache(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantFail bool
	}{
		{name: "serve status", args: []string{"serve", "status", "--addr", "127.0.0.1:1"}},
		{name: "cache help flag", args: []string{"cache", "--help"}},
		{name: "root help flag", args: []string{"--help"}},
		{name: "built-in help command", args: []string{"help", "cache"}},
		{name: "argument error", args: []string{"cache", "status", "--instance", "../bad"}, wantFail: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := seedAgedNamedClientCache(t, "999.0.0")
			root := NewRootCmd()
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			root.SetOut(stdout)
			root.SetErr(stderr)
			root.SetArgs(test.args)
			err := root.Execute()
			if (err != nil) != test.wantFail {
				t.Fatalf("Execute error = %v, want failure = %t", err, test.wantFail)
			}
			assertAgedNamedClientCacheIntact(t, requests, stderr.String())
		})
	}
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

func TestRootCommandDoesNotReadCacheAfterCommandFailure(t *testing.T) {
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
	if strings.Contains(stderr.String(), "客户端 1.0.0 落后于权威端") {
		t.Fatalf("命令失败后不应读取缓存并输出版本提示，实际 %q", stderr.String())
	}
}

func TestRootCommandWritesNewerClientHintForOldAuthority(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })
	_ = seedRootVersionCache(t, "")

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "客户端比权威端新，建议先升级权威端\n"
	if stderr.String() != want {
		t.Fatalf("标准错误输出 = %q，应为 %q", stderr.String(), want)
	}
}

func TestRootCommandDoesNotHintForDevClientAndOldAuthority(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "dev"
	t.Cleanup(func() { buildinfo.Version = saved })
	_ = seedRootVersionCache(t, "")

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "" {
		t.Fatalf("开发构建连旧权威端不应提示，实际 %q", stderr.String())
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

func TestDoctorShowsOldAuthorityRecommendedVersion(t *testing.T) {
	saved := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = saved })
	withDoctorDirs(t)
	dek := seedRootVersionCache(t, "")
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
	want := "客户端版本 1.0.0；推荐版本 权威端过旧，未提供推荐版本；客户端比权威端新，建议先升级权威端"
	if !bytes.Contains(stdout.Bytes(), []byte(want)) {
		t.Fatalf("doctor 输出缺少 %q：\n%s", want, stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("doctor 不应在标准错误输出重复根命令提示：%q", stderr.String())
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
