package machineinit_test

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// withMachineEnv 把 vault 与客户端数据的全部路径固定在临时目录里：主存储、
// 主密钥文件、用户配置根（客户端 role.json 与缓存）、serve 证书探测。
func withMachineEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	vaultDir := filepath.Join(root, "vault", "ssh-manager")
	configRoot := filepath.Join(root, "config")
	t.Setenv("SSHMGR_VAULT_ROOT", filepath.Join(root, "vault"))
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(vaultDir, "master.key.plain"))
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(vaultDir, "serve-cert.pem"))
	return root
}

func TestInitializeStandaloneOnEmptyMachine(t *testing.T) {
	withMachineEnv(t)

	result, err := machineinit.Initialize(roles.RoleStandalone)
	if err != nil {
		t.Fatalf("空机器初始化为单机不应失败：%v", err)
	}
	if result.Kind != machineinit.ResultCreated {
		t.Fatalf("结果类型 = %v，应为新建 vault", result.Kind)
	}
	if !roles.VaultExists() {
		t.Fatal("初始化后应存在 vault")
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
	if result.MasterKeyPath == "" {
		t.Fatal("结果应带主密钥文件位置")
	}
	if _, err := os.Stat(result.MasterKeyPath); err != nil {
		t.Fatalf("主密钥文件应已写入 %s：%v", result.MasterKeyPath, err)
	}
}

func TestReadSnapshotOnEmptyMachine(t *testing.T) {
	withMachineEnv(t)

	snapshot, err := machineinit.ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Role != nil {
		t.Fatalf("空机器不应有 role.json：%+v", snapshot.Role)
	}
	if snapshot.VaultExists || snapshot.VaultOpenable {
		t.Fatalf("空机器不应有 vault：%+v", snapshot)
	}
	if snapshot.ClientCachePresent {
		t.Fatal("空机器不应有客户端缓存")
	}
	if snapshot.AdminPasswordSet {
		t.Fatal("空机器不应显示管理员口令已设置")
	}
	if snapshot.ServeWebUI.Known || snapshot.ServeWebUI.Running {
		t.Fatalf("空机器不应探测出 serve 的 Web UI：%+v", snapshot.ServeWebUI)
	}
}

func TestReadSnapshotOnInitializedServer(t *testing.T) {
	withMachineEnv(t)
	if _, err := machineinit.Initialize(roles.RoleServer); err != nil {
		t.Fatal(err)
	}
	probePort := freeLocalPort(t)
	masterKey, err := os.ReadFile(os.Getenv("SSHMGR_FILEKEY_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting("serve.ui_port", strconv.Itoa(probePort)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetWebAdminPassword([]byte("admin-password")); err != nil {
		t.Fatal(err)
	}

	snapshot, err := machineinit.ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Role == nil || snapshot.Role.Role != roles.RoleServer || !snapshot.Role.SetupComplete {
		t.Fatalf("快照应包含权威端 role.json：%+v", snapshot.Role)
	}
	if !snapshot.VaultExists || !snapshot.VaultOpenable || snapshot.VaultOpenErr != nil {
		t.Fatalf("快照应显示 vault 可打开：%+v", snapshot)
	}
	if snapshot.ClientCachePresent {
		t.Fatal("权威端不应有客户端缓存")
	}
	if !snapshot.AdminPasswordSet {
		t.Fatal("快照应显示管理员口令已设置")
	}
	if !snapshot.ServeWebUI.Known {
		t.Fatalf("快照应能推导 serve 的 Web UI 端口：%+v", snapshot.ServeWebUI)
	}
	if snapshot.ServeWebUI.Port != probePort {
		t.Fatalf("Web UI 端口 = %d，应为 %d", snapshot.ServeWebUI.Port, probePort)
	}
	if !containsString(snapshot.ServeWebUI.Addresses, "127.0.0.1") {
		t.Fatalf("Web UI 地址列表应包含 127.0.0.1：%v", snapshot.ServeWebUI.Addresses)
	}
	if snapshot.ServeWebUI.Running {
		t.Fatalf("没有服务监听时不应显示 Web UI 在运行：%+v", snapshot.ServeWebUI)
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestInitializeServerOnEmptyMachine(t *testing.T) {
	withMachineEnv(t)

	result, err := machineinit.Initialize(roles.RoleServer)
	if err != nil {
		t.Fatalf("空机器初始化为权威端不应失败：%v", err)
	}
	if result.Kind != machineinit.ResultCreated {
		t.Fatalf("结果类型 = %v，应为新建 vault", result.Kind)
	}
	if err := roles.VaultOpenErr(); err != nil {
		t.Fatalf("初始化后的 vault 应能打开：%v", err)
	}
	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleServer || !state.SetupComplete {
		t.Fatalf("role.json 应为权威端且 setup_complete 为真：%+v", state)
	}
}

func TestInitializeServerOnStandaloneKeepsVaultContents(t *testing.T) {
	withMachineEnv(t)
	masterKey := seedStandaloneVault(t, "gpu-box", "team-profile", "nightly-project")

	result, err := machineinit.Initialize(roles.RoleServer)
	if err != nil {
		t.Fatalf("单机升级为权威端不应失败：%v", err)
	}
	if result.Kind != machineinit.ResultRoleRewritten {
		t.Fatalf("结果类型 = %v，应为只改写角色", result.Kind)
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

// seedStandaloneVault 在临时目录里造一个已有数据的单机 vault，并写好
// role.json（setup_complete 为真），返回主密钥供测试再次打开 vault。
func seedStandaloneVault(t *testing.T, serverName, profileName, projectName string) []byte {
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

func TestInitializeReturnsAlreadyWhenTargetAchieved(t *testing.T) {
	withMachineEnv(t)
	seedStandaloneVault(t, "gpu-box", "team-profile", "nightly-project")

	result, err := machineinit.Initialize(roles.RoleStandalone)
	if err != nil {
		t.Fatalf("目标状态已达成时不应报错：%v", err)
	}
	if result.Kind != machineinit.ResultAlreadyInitialized {
		t.Fatalf("结果类型 = %v，应为目标已达成", result.Kind)
	}
	if result.MasterKeyPath == "" {
		t.Fatal("目标已达成时也应带主密钥文件位置")
	}
	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleStandalone || !state.SetupComplete {
		t.Fatalf("role.json 不应被改动：%+v", state)
	}
}

func TestInitializeRefusesInvalidMachines(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T)
		target  roles.Role
		wantErr string
	}{
		{
			name: "权威端降级为单机",
			prepare: func(t *testing.T) {
				withMachineEnv(t)
				seedStandaloneVault(t, "gpu-box", "team-profile", "nightly-project")
				if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
					t.Fatal(err)
				}
			},
			target:  roles.RoleStandalone,
			wantErr: "权威端不能降级为单机",
		},
		{
			name: "客户端角色",
			prepare: func(t *testing.T) {
				withMachineEnv(t)
				if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
					t.Fatal(err)
				}
			},
			target:  roles.RoleServer,
			wantErr: "客户端",
		},
		{
			name: "存在客户端缓存",
			prepare: func(t *testing.T) {
				withMachineEnv(t)
				cacheDir := filepath.Join(os.Getenv("SSHMGR_CONFIG_ROOT"), "ssh-manager")
				if err := os.MkdirAll(cacheDir, 0o700); err != nil {
					t.Fatal(err)
				}
				blob := []byte(`{"url":"https://192.0.2.9:7878","token":"device-code"}`)
				if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), blob, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			target:  roles.RoleStandalone,
			wantErr: "客户端",
		},
		{
			name: "单机角色但存在客户端缓存",
			prepare: func(t *testing.T) {
				withMachineEnv(t)
				if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
					t.Fatal(err)
				}
				cacheDir := filepath.Join(os.Getenv("SSHMGR_CONFIG_ROOT"), "ssh-manager")
				if err := os.MkdirAll(cacheDir, 0o700); err != nil {
					t.Fatal(err)
				}
				blob := []byte(`{"url":"https://192.0.2.9:7878","token":"device-code"}`)
				if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), blob, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			target:  roles.RoleServer,
			wantErr: "客户端",
		},
		{
			name:    "初始化为客户端",
			prepare: func(t *testing.T) { withMachineEnv(t) },
			target:  roles.RoleClient,
			wantErr: "只支持单机",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.prepare(t)
			result, err := machineinit.Initialize(testCase.target)
			if err == nil {
				t.Fatalf("应拒绝初始化，却成功了：%+v", result)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("错误应说明 %q：%v", testCase.wantErr, err)
			}
		})
	}
}

func TestSelectRoleWithoutVaultWritesIncompleteRole(t *testing.T) {
	withMachineEnv(t)

	if err := machineinit.SelectRoleWithoutVault(roles.RoleStandalone); err != nil {
		t.Fatalf("选择单机不应失败：%v", err)
	}

	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleStandalone || state.SetupComplete {
		t.Fatalf("选单机后应写入 setup_complete 为假的 role.json：%+v", state)
	}
	if roles.VaultExists() {
		t.Fatal("只选角色不应创建 vault")
	}
}

func TestInitializeAfterSelectedRoleCompletesSetup(t *testing.T) {
	withMachineEnv(t)
	if err := machineinit.SelectRoleWithoutVault(roles.RoleStandalone); err != nil {
		t.Fatal(err)
	}

	result, err := machineinit.Initialize(roles.RoleStandalone)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != machineinit.ResultCreated {
		t.Fatalf("结果类型 = %v，应为新建 vault", result.Kind)
	}

	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleStandalone || !state.SetupComplete {
		t.Fatalf("建好 vault 后 setup_complete 应为真：%+v", state)
	}
}
