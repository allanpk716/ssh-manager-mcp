package servermanage

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"

	_ "modernc.org/sqlite"
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
	if !result.Succeeded || result.CompletedAt.IsZero() || result.SuccessAddress != virtualHost ||
		result.SuccessNetworkName != "虚拟网" || result.Duration <= 0 || len(result.Attempts) != 1 ||
		!result.Attempts[0].OK || result.Attempts[0].Address != virtualHost ||
		result.Attempts[0].NetworkName != "虚拟网" || result.Attempts[0].Duration <= 0 {
		t.Fatalf("连接测试应返回结构化成功结果：%+v", result)
	}
	persisted, err := st.ServerConnectionTestResult(added.ServerID)
	if err != nil || persisted == nil || !persisted.Succeeded ||
		persisted.SuccessAddress != virtualHost || persisted.SuccessNetworkName != "虚拟网" ||
		persisted.CompletedAt.IsZero() || persisted.Duration <= 0 || len(persisted.Attempts) != 1 {
		t.Fatalf("连接测试成功后应持久保存结构化结果：%v %+v", err, persisted)
	}
	detail, err := New(st).Detail(added.ServerID)
	if err != nil || detail.ConnectionTest == nil || !detail.ConnectionTest.Succeeded ||
		detail.ConnectionTest.SuccessAddress != virtualHost ||
		detail.ConnectionTest.SuccessSource != models.DialAddressSourceVirtual {
		t.Fatalf("服务器详情应读回最近一次连接测试结果：%v %+v", err, detail.ConnectionTest)
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
	if !strings.Contains(rows[0].Command, `"fingerprint"`) ||
		strings.Contains(rows[0].Command, `"主机密钥指纹"`) {
		t.Fatalf("连接测试审计的指纹摘要键应与其他审计一致：%s", rows[0].Command)
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
	if result.Succeeded || result.FailureReason != models.ConnectionTestFailureHostKeyMismatch ||
		len(result.Attempts) != 2 || result.Attempts[0].FailureReason != models.ConnectionTestFailureHostKeyMismatch ||
		result.Attempts[1].FailureReason != models.ConnectionTestFailureHostKeyMismatch {
		t.Fatalf("主机密钥不一致应归类为白话失败并逐地址保存：%+v", result)
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted == nil || persisted.Succeeded ||
		persisted.FailureReason != models.ConnectionTestFailureHostKeyMismatch ||
		len(persisted.Attempts) != 2 || persisted.CompletedAt.IsZero() {
		t.Fatalf("主机密钥不一致的最近结果应持久保存：%v %+v", persistErr, persisted)
	}
	if result.AnchorResult != "" {
		t.Fatalf("连接失败时不得显示锚定校验成功：%+v", result)
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
	if err == nil || result.AnchorFingerprint == "" || result.AnchorResult != "" {
		t.Fatalf("认证失败应返回错误；锚事实可保留但不显示校验成功：%v %+v", err, result)
	}
	if result.Succeeded || result.FailureReason != models.ConnectionTestFailureAuthentication ||
		len(result.Attempts) != 1 || result.Attempts[0].FailureReason != models.ConnectionTestFailureAuthentication {
		t.Fatalf("认证失败应归类为白话失败并记录候选地址结果：%+v", result)
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted == nil || persisted.Succeeded ||
		persisted.FailureReason != models.ConnectionTestFailureAuthentication || len(persisted.Attempts) != 1 {
		t.Fatalf("认证失败的最近结果应持久保存：%v %+v", persistErr, persisted)
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
	if result.Succeeded || result.FailureReason != models.ConnectionTestFailureTimeout ||
		len(result.Attempts) != 2 || result.Attempts[0].FailureReason != models.ConnectionTestFailureTimeout {
		t.Fatalf("连接超时应归类为白话失败并逐地址保存：%+v", result)
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted == nil || persisted.Succeeded ||
		persisted.FailureReason != models.ConnectionTestFailureTimeout || len(persisted.Attempts) != 2 {
		t.Fatalf("连接超时的最近结果应持久保存：%v %+v", persistErr, persisted)
	}
	if strings.Contains(err.Error(), "login-secret") {
		t.Fatalf("超时错误不得包含登录密码：%v", err)
	}
}

// TestManagerConnectionTestPortUnreachable 覆盖端口不通：真实 SSH 服务不存
// 在时，整体与逐地址结果都使用 owner 可读的失败分类。
func TestManagerConnectionTestPortUnreachable(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := addConnectionMembership(t, st, targetDevice.Name, "127.0.0.1", true); err != nil {
		t.Fatal(err)
	}
	added := addConnectionServer(t, st, targetDevice.Name, "127.0.0.1", port)

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{Identifier: added.ServerID})
	if err == nil || result.Succeeded || result.FailureReason != models.ConnectionTestFailurePortUnreachable ||
		len(result.Attempts) != 1 || result.Attempts[0].FailureReason != models.ConnectionTestFailurePortUnreachable {
		t.Fatalf("端口不通应归类为白话失败并逐地址保存：%v %+v", err, result)
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted == nil || persisted.Succeeded ||
		persisted.FailureReason != models.ConnectionTestFailurePortUnreachable || len(persisted.Attempts) != 1 {
		t.Fatalf("端口不通的最近结果应持久保存：%v %+v", persistErr, persisted)
	}
}

// TestManagerConnectionTestCanceledDoesNotPersistResult 钉住进行中任务的
// 边界：调用方取消时还没有完成结果，不得写入最近一次完成结果。
// blockConnectionTestAuditWrites 用第二个 SQLite 连接安装触发器，让审计表
// 插入失败；服务器条目与连接测试结果表的写入不受影响，供保存顺序与连接后
// 保存步骤失败的测试使用。
func blockConnectionTestAuditWrites(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", os.Getenv("SSHMGR_STORE")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS block_audit_insert
BEFORE INSERT ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit blocked'); END`); err != nil {
		t.Fatal(err)
	}
}

// TestManagerConnectionTestConnectedSaveFailureKeepsConnectedFacts 覆盖已连
// 上但连接后保存步骤失败：整体记为失败并使用自含白话原因，实际连上的地址
// 与逐地址成功行如实保留，最近结果按失败保存。
func TestManagerConnectionTestConnectedSaveFailureKeepsConnectedFacts(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, true)
	hostKey := fixedConnectionHostKey(t)
	script := &connectionShellScript{family: "powershell"}
	address, _, stopServer := testsshd.Start(t, testsshd.Options{
		Password: "login-secret", HostKey: hostKey, Exec: script.exec,
	})
	defer stopServer()
	host, port := splitConnectionAddress(t, address)
	if err := addConnectionMembership(t, st, targetDevice.Name, host, false); err != nil {
		t.Fatal(err)
	}
	added := addConnectionServer(t, st, targetDevice.Name, host, port)
	srv, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(srv, hostKey.PublicKey().Marshal()); err != nil {
		t.Fatal(err)
	}
	blockConnectionTestAuditWrites(t)

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{
		Identifier: added.ServerID, ProbeShell: true,
	})
	if err == nil {
		t.Fatal("连接后的保存步骤失败应返回错误")
	}
	if result.Succeeded || result.FailureReason != models.ConnectionTestFailureConnectedSaveFailed {
		t.Fatalf("已连上但保存失败应使用自含失败原因：%+v", result)
	}
	if result.SuccessAddress != host || len(result.Attempts) != 1 || !result.Attempts[0].OK {
		t.Fatalf("已连上的地址与逐地址成功行应如实保留：%+v", result)
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted == nil || persisted.Succeeded ||
		persisted.FailureReason != models.ConnectionTestFailureConnectedSaveFailed ||
		persisted.SuccessAddress != host || len(persisted.Attempts) != 1 || !persisted.Attempts[0].OK {
		t.Fatalf("已连上但保存失败的最近结果应按失败保存并保留连上事实：%v %+v", persistErr, persisted)
	}
	if strings.Contains(err.Error(), "login-secret") {
		t.Fatalf("保存失败错误不得包含登录密码：%v", err)
	}
}

// TestManagerConnectionTestSuccessSavesResultBeforeAudit 覆盖成功路径的保
// 存顺序：先写最近结果，再写审计；审计失败照旧返回错误。
func TestManagerConnectionTestSuccessSavesResultBeforeAudit(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, true)
	hostKey := fixedConnectionHostKey(t)
	address, _, stopServer := testsshd.Start(t, testsshd.Options{
		Password: "login-secret", HostKey: hostKey,
		Exec: (&connectionShellScript{family: "posix"}).exec,
	})
	defer stopServer()
	host, port := splitConnectionAddress(t, address)
	if err := addConnectionMembership(t, st, targetDevice.Name, host, false); err != nil {
		t.Fatal(err)
	}
	added := addConnectionServer(t, st, targetDevice.Name, host, port)
	srv, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(srv, hostKey.PublicKey().Marshal()); err != nil {
		t.Fatal(err)
	}
	blockConnectionTestAuditWrites(t)

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{
		Identifier: added.ServerID, ProbeShell: false,
	})
	if err == nil {
		t.Fatal("审计写入失败应照旧返回错误")
	}
	if result == nil || !result.Succeeded || !result.Persisted {
		t.Fatalf("审计失败前连接结果应已按成功保存：%+v", result)
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted == nil || !persisted.Succeeded ||
		persisted.SuccessAddress != host || persisted.CompletedAt.IsZero() {
		t.Fatalf("成功路径应先保存最近结果再写审计：%v %+v", persistErr, persisted)
	}
}

func TestManagerConnectionTestCanceledDoesNotPersistResult(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, false)
	added := addConnectionServer(t, st, targetDevice.Name, "127.0.0.1", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := manager.TestConnection(ctx, ConnectionTestRequest{Identifier: added.ServerID})
	if err == nil {
		t.Fatal("已取消的连接测试应返回错误")
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted != nil {
		t.Fatalf("取消不应写入最近一次完成结果：%v %+v", persistErr, persisted)
	}
}

// TestManagerConnectionTestEarlyExitKeepsPreviousResult 覆盖拨号前提前退出
// 的边界：读取存储或服务器条目失败时不写入新的最近结果，也不得把零值完成
// 时间带回页面。
func TestManagerConnectionTestEarlyExitKeepsPreviousResult(t *testing.T) {
	vaultDir := t.TempDir()
	database := filepath.Join(vaultDir, "store.db")
	t.Setenv("SSHMGR_STORE", database)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(database, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", ShellType: models.ShellPOSIX,
	})
	if err != nil {
		t.Fatal(err)
	}
	previous := models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: true,
		SuccessAddress: "192.0.2.10", Duration: 80 * time.Millisecond,
	}
	if err := st.SaveServerConnectionTestResult(added, previous); err != nil {
		t.Fatal(err)
	}
	manager := New(st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	closed, err := manager.TestConnection(context.Background(), ConnectionTestRequest{Identifier: "web"})
	if err == nil || closed == nil || !closed.CompletedAt.IsZero() || closed.Succeeded {
		t.Fatalf("存储读取失败应提前返回零值完成结果：%v %+v", err, closed)
	}

	reopened, err := store.Open(database, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.ServerConnectionTestResult(added)
	if err != nil || persisted == nil || !persisted.Succeeded ||
		!persisted.CompletedAt.Equal(previous.CompletedAt) {
		t.Fatalf("提前退出不得覆盖既有最近结果：%v %+v", err, persisted)
	}

	missing, err := New(reopened).TestConnection(
		context.Background(), ConnectionTestRequest{Identifier: "server-does-not-exist"},
	)
	if err == nil || missing == nil || !missing.CompletedAt.IsZero() || missing.Succeeded {
		t.Fatalf("服务器条目不存在应提前返回零值完成结果：%v %+v", err, missing)
	}
	persistedAfterMissing, err := reopened.ServerConnectionTestResult(added)
	if err != nil || persistedAfterMissing == nil || !persistedAfterMissing.Succeeded {
		t.Fatalf("查不到服务器条目不得影响既有最近结果：%v %+v", err, persistedAfterMissing)
	}
}

// TestManagerConnectionTestPersistsActionableHint 覆盖拨号层白话提示的公共
// 边界：提示作为结构化字段进入返回结果与最近结果，不靠解析底层错误。
func TestManagerConnectionTestPersistsActionableHint(t *testing.T) {
	manager, st, targetDevice := newConnectionTestStore(t, false)
	if err := addConnectionTestNetwork(t, st, "备用局域网", models.NetworkPhysical); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership(targetDevice.Name, "备用局域网", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "fallback-only", Host: "127.0.0.1", Port: port, User: "deploy",
		ShellType: models.ShellPOSIX,
	}, loginCredential, nil, targetDevice.Name)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.TestConnection(context.Background(), ConnectionTestRequest{Identifier: added.ServerID})
	if err == nil {
		t.Fatal("兜底地址端口不通应失败")
	}
	wantHint := "请补登记本机设备与目标设备的共同网络；单机部署与权威端也可先设定本机设备。"
	if result.Hint != wantHint {
		t.Fatalf("返回结果应带结构化提示：%+v", result)
	}
	persisted, persistErr := st.ServerConnectionTestResult(added.ServerID)
	if persistErr != nil || persisted == nil || persisted.Hint != wantHint {
		t.Fatalf("最近结果应持久保存结构化提示：%v %+v", persistErr, persisted)
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
