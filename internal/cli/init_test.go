package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// withInitMachineEnv 把一台「空机器」的全部路径固定在临时用户目录里。
func withInitMachineEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	vaultRoot := filepath.Join(root, "vault")
	configRoot := filepath.Join(root, "config")
	vaultDir := filepath.Join(vaultRoot, "ssh-manager")
	withEnv(t, map[string]string{
		"SSHMGR_VAULT_ROOT":    vaultRoot,
		"SSHMGR_CONFIG_ROOT":   configRoot,
		"SSHMGR_STORE":         filepath.Join(vaultDir, "store.db"),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(vaultDir, "master.key.plain"),
		"SSHMGR_MASTERKEY_HEX": "",
		"SSHMGR_SERVE_CERT":    filepath.Join(vaultDir, "serve-cert.pem"),
	})
	return root
}

func TestInitStandaloneOnEmptyMachine(t *testing.T) {
	withInitMachineEnv(t)

	output := runCli(t, "init", "--role", "standalone")

	for _, want := range []string{"单机", "master.key.plain", "备份"} {
		if !strings.Contains(output, want) {
			t.Fatalf("输出应包含 %q：%s", want, output)
		}
	}
	if !strings.Contains(output, os.Getenv("SSHMGR_FILEKEY_PATH")) {
		t.Fatalf("输出应包含主密钥文件位置：%s", output)
	}
	if err := roles.VaultOpenErr(); err != nil {
		t.Fatalf("初始化后的 vault 应能打开：%v", err)
	}
	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleStandalone || !state.SetupComplete {
		t.Fatalf("role.json 应为单机且 setup_complete 为真：%+v", state)
	}
}

func TestInitServerOnEmptyMachine(t *testing.T) {
	withInitMachineEnv(t)

	output := runCli(t, "init", "--role", "server")

	for _, want := range []string{"权威端", "sshmgr serve admin set", "sshmgr serve install", "备份"} {
		if !strings.Contains(output, want) {
			t.Fatalf("输出应包含 %q：%s", want, output)
		}
	}
	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleServer || !state.SetupComplete {
		t.Fatalf("role.json 应为权威端且 setup_complete 为真：%+v", state)
	}
	if err := roles.VaultOpenErr(); err != nil {
		t.Fatalf("初始化后的 vault 应能打开：%v", err)
	}
}

func TestInitServerOnStandaloneUpgradesWithoutTouchingVault(t *testing.T) {
	withInitMachineEnv(t)
	masterKey := seedInitVault(t, "gpu-box", "team-profile", "nightly-project")

	output := runCli(t, "init", "--role", "server")

	for _, want := range []string{"权威端", "vault 内容保持不变", "sshmgr serve admin set", "sshmgr serve install"} {
		if !strings.Contains(output, want) {
			t.Fatalf("输出应包含 %q：%s", want, output)
		}
	}
	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleServer || !state.SetupComplete {
		t.Fatalf("role.json 应改为权威端且 setup_complete 为真：%+v", state)
	}
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "gpu-box" {
		t.Fatalf("升级不应改动服务器数据：%+v", servers)
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Name != "team-profile" {
		t.Fatalf("升级不应改动 profile 数据：%+v", profiles)
	}
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Name != "nightly-project" {
		t.Fatalf("升级不应改动 project 数据：%+v", projects)
	}
}

// seedInitVault 造一个已有服务器、profile、project 的单机 vault，并写好
// 单机 role.json，返回主密钥供测试再次打开 vault。
func seedInitVault(t *testing.T, serverName, profileName, projectName string) []byte {
	t.Helper()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(os.Getenv("SSHMGR_FILEKEY_PATH")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SSHMGR_FILEKEY_PATH"), masterKey, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddServer(&models.Server{Name: serverName, Host: "192.0.2.10", User: "ubuntu", Port: 22}); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile(profileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddProject(projectName, profileID); err != nil {
		t.Fatal(err)
	}
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	return masterKey
}

func TestInitRefusesInvalidMachines(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T)
		args    []string
		wantErr string
	}{
		{
			name: "权威端降级为单机",
			prepare: func(t *testing.T) {
				withInitMachineEnv(t)
				seedInitVault(t, "gpu-box", "team-profile", "nightly-project")
				if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
					t.Fatal(err)
				}
			},
			args:    []string{"init", "--role", "standalone"},
			wantErr: "权威端不能降级为单机",
		},
		{
			name: "客户端角色",
			prepare: func(t *testing.T) {
				withInitMachineEnv(t)
				if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
					t.Fatal(err)
				}
			},
			args:    []string{"init", "--role", "server"},
			wantErr: "客户端",
		},
		{
			name: "存在客户端缓存",
			prepare: func(t *testing.T) {
				withInitMachineEnv(t)
				cacheDir := filepath.Join(os.Getenv("SSHMGR_CONFIG_ROOT"), "ssh-manager")
				if err := os.MkdirAll(cacheDir, 0o700); err != nil {
					t.Fatal(err)
				}
				blob := []byte(`{"url":"https://192.0.2.9:7878","token":"device-code"}`)
				if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), blob, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			args:    []string{"init", "--role", "standalone"},
			wantErr: "客户端",
		},
		{
			name:    "role 缺失",
			prepare: func(t *testing.T) { withInitMachineEnv(t) },
			args:    []string{"init"},
			wantErr: "--role 是必填参数",
		},
		{
			name:    "role 取值非法",
			prepare: func(t *testing.T) { withInitMachineEnv(t) },
			args:    []string{"init", "--role", "client"},
			wantErr: "无效",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.prepare(t)
			root := NewRootCmd()
			root.SetArgs(testCase.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("命令应失败，却成功了")
			}
			if ExitCodeFor(err) == 0 {
				t.Fatalf("退出码应为非零：%v", err)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("错误应包含 %q：%v", testCase.wantErr, err)
			}
		})
	}
}

func TestInitAlreadyAtTargetRoleSucceeds(t *testing.T) {
	withInitMachineEnv(t)
	seedInitVault(t, "gpu-box", "team-profile", "nightly-project")

	output := runCli(t, "init", "--role", "standalone")

	if !strings.Contains(output, "已是该角色") {
		t.Fatalf("重复执行应输出「已是该角色」：%s", output)
	}
	if !strings.Contains(output, os.Getenv("SSHMGR_FILEKEY_PATH")) {
		t.Fatalf("重复执行也应输出主密钥文件位置：%s", output)
	}
}
