package cli

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestSharesResidueRejectsCacheClient(t *testing.T) {
	seedSharesClientCache(t, true)

	errText := runCliErr(t, "shares", "residue")
	if !strings.Contains(errText, "本机是缓存客户端") || !strings.Contains(errText, "权威端执行") {
		t.Fatalf("缓存客户端应被拒绝：%s", errText)
	}
}

func TestSharesResidueShowsFindingsAndCleanupCommands(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("NAS_MOUNT_PASSWORD", "共享存储挂载密码",
		[]byte("mount-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	linux := &sharemounttest.LinuxServer{ShareName: "datasets"}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: linux.Handler,
	})
	t.Cleanup(cleanup)
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	server := &models.Server{
		Name: "linux-web", Host: addr[:strings.LastIndex(addr, ":")], Port: portOfAddr(addr),
		User: "deploy", ShellType: models.ShellPOSIX,
	}
	added, err := st.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, "NAS")
	if err != nil {
		t.Fatal(err)
	}
	server.ID = added.ServerID
	if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "windows-web", Host: "127.0.0.1", Port: 1, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	}); err != nil {
		t.Fatal(err)
	}
	share, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	// linux-web 从未进入可挂载范围，模拟撤销登记失败后留下的现场残留。
	linux.ShareID = share.ID
	linux.SeedManagedConfiguration("192.0.2.10", share.Version)
	st.Close()

	output := runCli(t, "shares", "residue")
	for _, want := range []string{
		"服务器 linux-web", "datasets", "配置条目", "凭据文件", "挂载",
		"不在可挂载范围内", "清理命令：shares unmount datasets linux-web --yes",
		"服务器 windows-web", "跳过", "sshmgr 不在其上留下任何东西",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("输出应包含 %q：\n%s", want, output)
		}
	}
	filtered := runCli(t, "shares", "residue", "media")
	if strings.Contains(filtered, "清理命令") {
		t.Fatalf("指定 media 时不应输出 datasets 的清理命令：\n%s", filtered)
	}
}
