package servermanage

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// connectionShellScript 按指定 shell 家族回答现有两条固定探测命令，并记录
// 服务器实际收到的命令。
type connectionShellScript struct {
	family string
	mu     sync.Mutex
	seen   []string
}

func (s *connectionShellScript) exec(command string, _ io.Reader) (string, string, int) {
	s.mu.Lock()
	s.seen = append(s.seen, command)
	s.mu.Unlock()
	switch command {
	case "echo %OS%":
		if s.family == "cmd" {
			return "Windows_NT\r\n", "", 0
		}
		return "%OS%\r\n", "", 0
	case "echo $PSVersionTable":
		switch s.family {
		case "powershell":
			return "PSVersion 7.4.1\r\nPSEdition Core\r\n", "", 0
		case "cmd", "unknown":
			return "$PSVersionTable\r\n", "", 0
		default:
			return "\n", "", 0
		}
	}
	return "", "", 1
}

func (s *connectionShellScript) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// TestManagerConnectionTestAnchorsVirtualAddressAndWritesShell 覆盖公共连
// 接测试边界：条目同时有物理地址与虚拟局域网地址且尚无锚时，首次锚定只经
// 虚拟局域网；自动探测 shell 成功后回写标注，页面数据带真实指纹与握手地址。
func TestManagerConnectionTestAnchorsVirtualAddressAndWritesShell(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("目标设备", "", ""); err != nil {
		t.Fatal(err)
	}
	local, err := st.AddDevice("本机设备", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(local.ID); err != nil {
		t.Fatal(err)
	}

	virtualSigner, physicalSigner := fixedConnectionHostKey(t), fixedConnectionHostKey(t)
	script := &connectionShellScript{family: "powershell"}
	virtualAuthRequests := &atomic.Int64{}
	virtualAddress, virtualKey, stopVirtual := testsshd.Start(t, testsshd.Options{
		ListenHost: "::1", Password: "login-secret", HostKey: virtualSigner,
		Exec:         script.exec,
		AuthRequests: virtualAuthRequests,
	})
	defer stopVirtual()
	virtualHost, virtualPort := splitConnectionAddress(t, virtualAddress)
	physicalAuthRequests := &atomic.Int64{}
	physicalAddress, _, stopPhysical := testsshd.Start(t, testsshd.Options{
		ListenHost: "127.0.0.1", ListenPort: virtualPort, Password: "login-secret",
		HostKey: physicalSigner, AuthRequests: physicalAuthRequests,
		Exec: script.exec,
	})
	defer stopPhysical()
	physicalHost, _ := splitConnectionAddress(t, physicalAddress)

	for _, membership := range []struct {
		device, network, address string
	}{
		{"目标设备", "家A局域网", physicalHost},
		{"目标设备", "虚拟网", virtualHost},
		{"本机设备", "家A局域网", "localhost"},
		{"本机设备", "虚拟网", "ip6-localhost"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	loginCredential := &models.Credential{
		Type: models.CredPassword, Secret: []byte("login-secret"),
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "web", User: "deploy", Port: virtualPort,
		ShellType: models.ShellPOSIX,
	}, loginCredential, nil, "目标设备")
	if err != nil {
		t.Fatal(err)
	}

	result, err := New(st).TestConnection(context.Background(), ConnectionTestRequest{
		Identifier: added.ServerID, ProbeShell: true, Source: WebUISource("192.0.2.9"),
	})
	if err != nil {
		t.Fatalf("连接测试失败：%v", err)
	}
	wantFingerprint := ssh.FingerprintSHA256(virtualKey)
	if result.AnchorFingerprint != wantFingerprint || result.AnchorResult != "首次连接已记录主机密钥锚" ||
		result.HandshakeAddress != virtualAddress || result.ShellDetected != models.ShellPowerShell {
		t.Fatalf("连接测试结果不完整：%+v", result)
	}
	if commands := script.commands(); strings.Join(commands, ";") != "echo %OS%;echo $PSVersionTable" {
		t.Fatalf("自动探测必须执行现有两条固定命令，实际 %q", commands)
	}
	if physicalAuthRequests.Load() != 0 {
		t.Fatalf("首次锚定有虚拟局域网地址时不得尝试物理地址，认证请求数=%d", physicalAuthRequests.Load())
	}
	if virtualAuthRequests.Load() == 0 {
		t.Fatal("虚拟局域网地址应完成认证")
	}
	saved, err := st.GetServer(added.ServerID)
	if err != nil || saved.ShellType != models.ShellPowerShell {
		t.Fatalf("自动探测结果应回写 shell 标注：%v %+v", err, saved)
	}
	pin, err := st.GetEntryPinMeta(added.ServerID)
	if err != nil || pin == nil || pin.Source != store.PinSourceTofu || pin.HandshakeAddr != virtualAddress {
		t.Fatalf("锚应记录真实虚拟局域网握手地址：%v %+v", err, pin)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.connect-test"}})
	if err != nil || len(rows) != 1 || rows[0].Status != "ok" || rows[0].Source != store.AuditSourceWebUI {
		t.Fatalf("连接测试应有成功审计行：%v %+v", err, rows)
	}
	for _, row := range rows {
		if strings.Contains(row.Command, "login-secret") {
			t.Fatalf("审计不得包含登录密码：%+v", row)
		}
	}
}

// TestManagerConnectionTestPhysicalOnlyKeepsFirstTrustSemantics 覆盖没有虚
// 拟局域网地址的条目：首次锚定沿用既有候选地址语义，物理局域网地址可以直接
// 记录锚。
func TestManagerConnectionTestPhysicalOnlyKeepsFirstTrustSemantics(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, false)
	address, hostKey, stopServer := testsshd.Start(t, testsshd.Options{
		Password: "login-secret", HostKey: fixedConnectionHostKey(t),
		Exec: (&connectionShellScript{family: "posix"}).exec,
	})
	defer stopServer()
	host, port := splitConnectionAddress(t, address)
	if err := addConnectionMembership(t, st, targetDevice.Name, host, true); err != nil {
		t.Fatal(err)
	}
	added := addConnectionServer(t, st, targetDevice.Name, host, port)

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{Identifier: added.ServerID})
	if err != nil {
		t.Fatal(err)
	}
	pin, pinErr := st.GetEntryPinMeta(added.ServerID)
	if pinErr != nil || pin == nil || pin.Source != store.PinSourceTofu || pin.HandshakeAddr != address ||
		result.AnchorFingerprint != ssh.FingerprintSHA256(hostKey) {
		t.Fatalf("只有物理地址时应按物理地址首次锚定：%v %+v", pinErr, pin)
	}
}

// TestManagerConnectionTestHostKeyMismatchSkipsWithoutCredentials 覆盖密钥
// 不一致路径：两个虚拟地址都被跳过，页面说明逐地址给出原因，且在任何认证
// 请求发出前停止。
func TestManagerConnectionTestHostKeyMismatchSkipsWithoutCredentials(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, true)
	firstSigner, secondSigner, pinnedSigner := fixedConnectionHostKey(t), fixedConnectionHostKey(t), fixedConnectionHostKey(t)
	firstAuthRequests, secondAuthRequests := &atomic.Int64{}, &atomic.Int64{}
	firstAddress, _, stopFirst := testsshd.Start(t, testsshd.Options{
		ListenHost: "127.0.0.1", Password: "login-secret", HostKey: firstSigner,
		AuthRequests: firstAuthRequests, Exec: (&connectionShellScript{family: "posix"}).exec,
	})
	defer stopFirst()
	_, firstPort := splitConnectionAddress(t, firstAddress)
	secondAddress, _, stopSecond := testsshd.Start(t, testsshd.Options{
		ListenHost: "::1", ListenPort: firstPort, Password: "login-secret", HostKey: secondSigner,
		AuthRequests: secondAuthRequests, Exec: (&connectionShellScript{family: "posix"}).exec,
	})
	defer stopSecond()
	firstHost, _ := splitConnectionAddress(t, firstAddress)
	secondHost, _ := splitConnectionAddress(t, secondAddress)
	if _, err := st.AddNetworkMembership(targetDevice.Name, "虚拟网", firstHost); err != nil {
		t.Fatal(err)
	}
	if err := addConnectionTestNetwork(t, st, "虚拟网二", models.NetworkVirtual); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("本机设备", "虚拟网二", "ip6-localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership(targetDevice.Name, "虚拟网二", secondHost); err != nil {
		t.Fatal(err)
	}
	added := addConnectionServer(t, st, targetDevice.Name, firstHost, firstPort)
	srv, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(srv, pinnedSigner.PublicKey().Marshal()); err != nil {
		t.Fatal(err)
	}

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{Identifier: added.ServerID})
	if err == nil || len(result.ConnectionNotes) != 2 {
		t.Fatalf("两个不一致地址都应被跳过：%v %+v", err, result)
	}
	for _, note := range result.ConnectionNotes {
		if !strings.Contains(note, "呈现了不同的主机密钥，已跳过") {
			t.Fatalf("跳过说明应给出原因：%q", note)
		}
	}
	if firstAuthRequests.Load() != 0 || secondAuthRequests.Load() != 0 {
		t.Fatalf("主机密钥不一致时不得发送登录凭据，认证请求数=%d/%d",
			firstAuthRequests.Load(), secondAuthRequests.Load())
	}
	if strings.Contains(err.Error(), "login-secret") || strings.Contains(strings.Join(result.ConnectionNotes, "\n"), "login-secret") {
		t.Fatalf("失败结果不得包含登录密码：%v %+v", err, result)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.connect-test"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row.Command, "login-secret") {
			t.Fatalf("失败审计不得包含登录密码：%+v", row)
		}
	}
}

// TestManagerConnectionTestShellDetectionDoesNotGuess 覆盖 cmd 与无法判定
// 两种探测结论：可判定才回写；无法判定时保留默认 POSIX 并给出依据。
func TestManagerConnectionTestShellDetectionDoesNotGuess(t *testing.T) {
	cases := []struct {
		name       string
		family     string
		detected   models.ShellType
		wantStored models.ShellType
	}{
		{name: "cmd", family: "cmd", detected: models.ShellWindowsCmd, wantStored: models.ShellWindowsCmd},
		{name: "unknown", family: "unknown", detected: "", wantStored: models.ShellPOSIX},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manager, st, targetDevice := newConnectionTestStore(t, true)
			script := &connectionShellScript{family: testCase.family}
			address, _, stopServer := testsshd.Start(t, testsshd.Options{
				Password: "login-secret", HostKey: fixedConnectionHostKey(t), Exec: script.exec,
			})
			defer stopServer()
			host, port := splitConnectionAddress(t, address)
			if err := addConnectionMembership(t, st, targetDevice.Name, host, false); err != nil {
				t.Fatal(err)
			}
			added := addConnectionServer(t, st, targetDevice.Name, host, port)

			result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{
				Identifier: added.ServerID, ProbeShell: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.ShellDetected != testCase.detected || result.ShellEvidence == "" {
				t.Fatalf("shell 结论应带依据且不猜测：%+v", result)
			}
			saved, err := st.GetServer(added.ServerID)
			if err != nil || saved.ShellType != testCase.wantStored {
				t.Fatalf("shell 标注回写不正确：%v %+v", err, saved)
			}
		})
	}
}

// TestManagerConnectionTestAuthenticationFailureKeepsSavedRows 覆盖认证失败：
// 服务器条目、设备、网络成员与 profile 授权全部保留，shell 维持默认 POSIX。
func TestManagerConnectionTestAuthenticationFailureKeepsSavedRows(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, true)
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	address, _, stopServer := testsshd.Start(t, testsshd.Options{
		Password: "correct-login-secret", HostKey: fixedConnectionHostKey(t),
		Exec: (&connectionShellScript{family: "posix"}).exec,
	})
	defer stopServer()
	host, port := splitConnectionAddress(t, address)
	if err := addConnectionMembership(t, st, targetDevice.Name, host, false); err != nil {
		t.Fatal(err)
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("wrong-login-secret")}
	added, err := manager.Add(AddRequest{
		Server: models.Server{
			Name: "web", User: "deploy", Host: "", Port: port, ShellType: models.ShellPOSIX,
		},
		LoginCredential: loginCredential,
		SudoCredential:  &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-secret")},
		DeviceName:      targetDevice.Name, ExistingDevice: true, ProfileIDs: []string{profileID},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{
		Identifier: added.ServerID, ProbeShell: true,
	})
	testErr := err
	if err == nil || result.AnchorResult == "" {
		t.Fatalf("认证失败应返回错误；主机密钥锚可按首次信任保留：%v %+v", err, result)
	}
	saved, err := st.GetServer(added.ServerID)
	if err != nil || saved.ShellType != models.ShellPOSIX || saved.AuthMethod != models.AuthPassword ||
		saved.SudoCredentialID == "" || saved.DeviceID != targetDevice.ID {
		t.Fatalf("认证失败不得删除条目或凭据：%v %+v", err, saved)
	}
	if pin, pinErr := st.GetEntryPinMeta(saved.ID); pinErr != nil || pin == nil {
		t.Fatalf("认证失败后首次锚定应保留：%v %+v", pinErr, pin)
	}
	memberships, err := st.ListNetworkMemberships(targetDevice.Name)
	if err != nil || len(memberships) != 1 {
		t.Fatalf("网络成员应保留：%v %+v", err, memberships)
	}
	granted, err := st.ServersForProfile(profileID)
	if err != nil || len(granted) != 1 || granted[0] != saved.ID {
		t.Fatalf("profile 授权应保留：%v %v", granted, err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.connect-test"}})
	if err != nil || len(rows) != 1 || rows[0].Status != "error" {
		t.Fatalf("认证失败应有失败审计：%v %+v", err, rows)
	}
	for _, secret := range []string{"wrong-login-secret", "correct-login-secret", "sudo-secret"} {
		if strings.Contains(testErr.Error(), secret) || strings.Contains(rows[0].Command, secret) {
			t.Fatalf("失败结果或审计不得包含凭据：%v %+v", testErr, rows[0])
		}
	}
}

// TestManagerConnectionTestAddressTimeout 覆盖多个候选地址时的连接超时汇
// 总；超时时间用环境变量缩短，避免测试等待系统默认超时。
func TestManagerConnectionTestAddressTimeout(t *testing.T) {
	t.Setenv("SSHMGR_SSH_ADDRESS_TIMEOUT", "100ms")
	manager, st, targetDevice := newConnectionTestStore(t, false)
	blackholeAddress := startConnectionBlackhole(t)
	blackholeHost, port := splitConnectionAddress(t, blackholeAddress)
	if _, err := st.AddNetworkMembership(targetDevice.Name, "家A局域网", blackholeHost); err != nil {
		t.Fatal(err)
	}
	if err := addConnectionTestNetwork(t, st, "备用局域网", models.NetworkPhysical); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("本机设备", "备用局域网", "ip6-localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership(targetDevice.Name, "备用局域网", "::1"); err != nil {
		t.Fatal(err)
	}
	added := addConnectionServer(t, st, targetDevice.Name, blackholeHost, port)

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{Identifier: added.ServerID})
	if err == nil || !strings.Contains(err.Error(), "连接超时") {
		t.Fatalf("多地址测试应汇总连接超时：%v %+v", err, result)
	}
	if strings.Contains(err.Error(), "login-secret") {
		t.Fatalf("超时错误不得包含登录密码：%v", err)
	}
}

func newConnectionTestStore(t *testing.T, virtual bool) (*Manager, *store.Store, *models.Device) {
	t.Helper()
	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if err := addConnectionTestNetwork(t, st, "家A局域网", models.NetworkPhysical); err != nil {
		t.Fatal(err)
	}
	if virtual {
		if err := addConnectionTestNetwork(t, st, "虚拟网", models.NetworkVirtual); err != nil {
			t.Fatal(err)
		}
	}
	target, err := st.AddDevice("目标设备", "", "")
	if err != nil {
		t.Fatal(err)
	}
	local, err := st.AddDevice("本机设备", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(local.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("本机设备", "家A局域网", "localhost"); err != nil {
		t.Fatal(err)
	}
	if virtual {
		if _, err := st.AddNetworkMembership("本机设备", "虚拟网", "ip6-localhost"); err != nil {
			t.Fatal(err)
		}
	}
	return New(st), st, target
}

func addConnectionTestNetwork(t *testing.T, st *store.Store, name string, networkType models.NetworkType) error {
	t.Helper()
	network := models.Network{Name: name, Type: networkType}
	if networkType == models.NetworkPhysical {
		network.SiteName = "家A"
	}
	_, err := st.AddNetwork(network)
	return err
}

func addConnectionMembership(
	t *testing.T, st *store.Store, targetDevice, host string, physical bool,
) error {
	t.Helper()
	networkName := "虚拟网"
	dialerAddress := "ip6-localhost"
	if physical {
		networkName = "家A局域网"
		dialerAddress = "localhost"
	}
	localMembers, err := st.ListNetworkMemberships("本机设备")
	if err != nil {
		return err
	}
	localHasNetwork := false
	for _, membership := range localMembers {
		if membership.NetworkName == networkName {
			localHasNetwork = true
			break
		}
	}
	if !localHasNetwork {
		if _, err := st.AddNetworkMembership("本机设备", networkName, dialerAddress); err != nil {
			return err
		}
	}
	_, err = st.AddNetworkMembership(targetDevice, networkName, host)
	return err
}

func addConnectionServer(t *testing.T, st *store.Store, deviceName, host string, port int) *store.AddServerDeviceResult {
	t.Helper()
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "web", Host: host, Port: port, User: "deploy", ShellType: models.ShellPOSIX,
	}, loginCredential, nil, deviceName)
	if err != nil {
		t.Fatal(err)
	}
	return added
}

func startConnectionBlackhole(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			defer connection.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

func fixedConnectionHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func splitConnectionAddress(t *testing.T, address string) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}
