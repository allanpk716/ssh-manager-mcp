package cli

import (
	"io"
	"strings"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestSharesMountIsIdempotentAndNeedsNoConfirmation(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	fake := &sharesMountFakeServer{}
	address, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)
	seedSharesMountTopology(t, st)
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "linux-web", Host: address[:strings.LastIndex(address, ":")],
		Port: portOfAddr(address), User: "deploy", ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")},
		&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")},
		"linux-device")
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	first := runCli(t, "shares", "mount", "datasets", "linux-web")
	for _, want := range []string{
		"已挂载共享存储 datasets", "服务器 linux-web", "/mnt/sshmgr/datasets",
		"实际网络：办公虚拟局域网", "物理局域网地址不通",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("首次挂载输出缺少 %q：\n%s", want, first)
		}
	}
	if strings.Contains(first, "确认") || strings.Contains(first, "mount-password") {
		t.Fatalf("挂载不应要求确认或显示密码：\n%s", first)
	}
	fake.setAction("no-change")
	second := runCli(t, "shares", "mount", "datasets", "linux-web")
	if !strings.Contains(second, "无需改动") || strings.Contains(second, "确认") {
		t.Fatalf("重复挂载应输出无需改动：\n%s", second)
	}
}

func TestSharesMountRejectsCacheClientAndWindowsServer(t *testing.T) {
	seedSharesClientCache(t, true)
	cacheError := runCliErr(t, "shares", "mount", "datasets", "linux-web")
	if !strings.Contains(cacheError, "本机是缓存客户端") ||
		!strings.Contains(cacheError, "权威端执行") {
		t.Fatalf("缓存客户端应被拒绝：%s", cacheError)
	}

	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	windowsError := runCliErr(t, "shares", "mount", "datasets", "windows-web")
	for _, want := range []string{
		"Windows 服务器不能持久挂载共享存储", "盘符由每条命令执行前的会话内注入提供",
	} {
		if !strings.Contains(windowsError, want) {
			t.Fatalf("Windows 拒绝说明缺少 %q：%s", want, windowsError)
		}
	}
}

func TestSharesShowCheckPrintsEveryServerConclusion(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(
		"NAS_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	linux := sharesCheckFakeServer("Linux", "SSHMGR-LINUX-CHECK", strings.Join([]string{
		"SSHMGR-LINUX-CHECK mode=full",
		"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=1 mounted=yes",
	}, "\n"))
	darwin := sharesCheckFakeServer("Darwin", "SSHMGR-DARWIN-CHECK", strings.Join([]string{
		"SSHMGR-DARWIN-CHECK mode=limited",
		"SSHMGR-DARWIN-CHECK index=1 mounted=no",
		"SSHMGR-DARWIN-CHECK auto-master=no",
	}, "\n"))
	addSharesCheckServer(t, st, "linux-web", linux, models.ShellPOSIX, true)
	addSharesCheckServer(t, st, "darwin-web", darwin, models.ShellPOSIX, false)
	addSharesCheckServer(t, st, "windows-web", nil, models.ShellPowerShell, false)
	addSharesCheckServer(t, st, "offline-web", nil, models.ShellPOSIX, false)
	if err := st.AllowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"},
		[]string{"linux-web", "darwin-web", "windows-web", "offline-web"},
		store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	output := runCli(t, "shares", "show", "datasets", "--check")
	for _, want := range []string{
		"现场探测", "服务器 linux-web：已挂载", "服务器 darwin-web：未挂载",
		"受限检测", "macOS 系统主配置已被重置", "服务器 windows-web：不检测",
		"盘符由每条命令执行前注入", "服务器 offline-web：不可达", "观察时间",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("现场探测输出缺少 %q：\n%s", want, output)
		}
	}
	if strings.Contains(output, "192.0.2.") {
		t.Fatalf("现场探测输出不得包含具体地址：\n%s", output)
	}
}

func TestSharesShowCheckRejectsCacheClient(t *testing.T) {
	seedSharesClientCache(t, true)

	errText := runCliErr(t, "shares", "show", "datasets", "--check")
	if !strings.Contains(errText, "本机是缓存客户端") ||
		!strings.Contains(errText, "权威端执行") {
		t.Fatalf("缓存客户端的现场探测应被拒绝：%s", errText)
	}
}

func seedSharesMountTopology(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("linux-device", "挂载目标", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "办公虚拟局域网", Type: models.NetworkVirtual,
	}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device  string
		network string
		address string
	}{
		{"NAS", "办公物理局域网", "192.0.2.10"},
		{"NAS", "办公虚拟局域网", "100.64.10.10"},
		{"linux-device", "办公物理局域网", "192.0.2.20"},
		{"linux-device", "办公虚拟局域网", "100.64.10.20"},
	} {
		if _, err := st.AddNetworkMembership(
			membership.device, membership.network, membership.address,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddSecret(
		"DATASETS_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
}

func addSharesCheckServer(
	t *testing.T, st *store.Store, name string,
	handler func(string, io.Reader) (string, string, int), shell models.ShellType, withSudo bool,
) {
	t.Helper()
	address := "127.0.0.1:1"
	var hostKey []byte
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	var sudoCredential *models.Credential
	if handler != nil {
		listenAddress, parsedKey, cleanup := testsshd.Start(t, testsshd.Options{
			Password: "login-pw", SudoPassword: "sudo-pw", Exec: handler,
		})
		t.Cleanup(cleanup)
		address = listenAddress
		hostKey = parsedKey.Marshal()
		if withSudo {
			sudoCredential = &models.Credential{
				Type: models.CredPassword, Secret: []byte("sudo-pw"),
			}
		}
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: name, Host: address[:strings.LastIndex(address, ":")],
		Port: portOfAddr(address), User: "deploy", ShellType: shell,
	}, loginCredential, sudoCredential, "")
	if err != nil {
		t.Fatal(err)
	}
	if handler == nil {
		return
	}
	server, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(server, hostKey); err != nil {
		t.Fatal(err)
	}
}

type sharesMountFakeServer struct {
	mu         sync.Mutex
	nextAction string
}

func (f *sharesMountFakeServer) setAction(action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAction = action
}

func (f *sharesMountFakeServer) handler(command string, stdin io.Reader) (string, string, int) {
	input, _ := io.ReadAll(stdin)
	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Linux\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one"):
		return "SSHMGR-PROBE 0 fail 2 no-route\nSSHMGR-PROBE 1 ok 2 \n", "", 0
	case strings.Contains(command, "SSHMGR-LINUX-MOUNT"):
		f.mu.Lock()
		defer f.mu.Unlock()
		action := "first-mount-success"
		if f.nextAction != "" {
			action = f.nextAction
		}
		_ = input
		return "SSHMGR-LINUX-MOUNT result=" + action + " selected=1\n", "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

func sharesCheckFakeServer(system, marker, output string) func(string, io.Reader) (string, string, int) {
	return func(command string, _ io.Reader) (string, string, int) {
		if strings.TrimSpace(command) == "uname -s" {
			return system + "\n", "", 0
		}
		if strings.Contains(command, marker) {
			return output + "\n", "", 0
		}
		return "", "unexpected command: " + command, 90
	}
}
