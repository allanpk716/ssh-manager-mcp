package servermanage

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedAddTopology 登记一个站点、一个物理局域网与一个虚拟局域网，并返回两
// 个 profile 的标识符。
func seedAddTopology(t *testing.T, m *Manager) (physical, virtual, profileID, secondProfileID string) {
	t.Helper()
	if _, err := m.st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	physicalNetwork, err := m.st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"})
	if err != nil {
		t.Fatal(err)
	}
	virtualNetwork, err := m.st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err = m.st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	secondProfileID, err = m.st.AddProfile("值班")
	if err != nil {
		t.Fatal(err)
	}
	return physicalNetwork.Name, virtualNetwork.Name, profileID, secondProfileID
}

func TestManagerAddServerCreatesDeviceMembershipsAndGrantsProfiles(t *testing.T) {
	m := newTestManager(t)
	physical, virtual, profileID, secondProfileID := seedAddTopology(t, m)

	result, err := m.Add(AddRequest{
		Server: models.Server{
			Name: "web", User: "deploy", Port: 2202, ShellType: models.ShellPOSIX,
			Role: "网站", Hardware: "2 核 4GB", Tags: []string{"prod"}, ExposeHost: true,
		},
		LoginCredential: &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")},
		SudoCredential:  &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-secret")},
		DeviceName:      "工作站",
		Memberships: []NetworkMembershipInput{
			{Network: physical, Address: "192.168.1.10"},
			{Network: virtual, Address: "100.64.0.10"},
		},
		ProfileIDs: []string{profileID, secondProfileID},
	}, WebUISource("192.0.2.7"))
	if err != nil {
		t.Fatal(err)
	}

	srv, err := m.st.GetServer(result.ServerID)
	if err != nil || srv == nil {
		t.Fatalf("新增后的服务器条目应可读取：%v %+v", err, srv)
	}
	if srv.Host != "192.168.1.10" || srv.DeviceID == "" || srv.AuthMethod != models.AuthPassword || !srv.ExposeHost {
		t.Fatalf("服务器条目连接信息或设备归属不正确：%+v", srv)
	}
	if result.DeviceName != "工作站" || !result.CreatedDevice {
		t.Fatalf("新增结果应说明新建设备：%+v", result)
	}
	memberships, err := m.st.ListNetworkMemberships("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 2 || memberships[0].Address != "192.168.1.10" || memberships[1].Address != "100.64.0.10" {
		t.Fatalf("新设备应有物理与虚拟网络成员：%+v", memberships)
	}
	for _, id := range []string{profileID, secondProfileID} {
		granted, err := m.st.ServersForProfile(id)
		if err != nil || len(granted) != 1 || granted[0] != srv.ID {
			t.Fatalf("profile %s 应看到新增服务器：%v %v", id, granted, err)
		}
	}
	rows, err := m.st.QueryAudit(store.AuditFilter{
		Actions: []string{"server.add", "profile.grant", "device.add", "device.member-add"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("新增、新设备、两个网络成员与两个授权应各有审计行，实际 %d", len(rows))
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.7" {
			t.Fatalf("审计来源应为 Web UI：%+v", row)
		}
		if strings.Contains(row.Command, "login-secret") || strings.Contains(row.Command, "sudo-secret") {
			t.Fatalf("审计不得包含凭据：%s", row.Command)
		}
	}
}

func TestManagerAddServerUsesExistingDeviceAndDerivesFallbackHost(t *testing.T) {
	m := newTestManager(t)
	physical, virtual, _, _ := seedAddTopology(t, m)
	device, err := m.st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.AddNetworkMembership(device.Name, physical, "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.AddNetworkMembership(device.Name, virtual, "100.64.0.20"); err != nil {
		t.Fatal(err)
	}

	result, err := m.Add(AddRequest{
		Server:     models.Server{Name: "db", User: "root", Port: 22},
		DeviceName: device.Name, ExistingDevice: true,
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := m.st.GetServer(result.ServerID)
	if srv.Host != "192.168.1.20" || srv.DeviceID != device.ID {
		t.Fatalf("已有设备的兜底主机应取第一个物理局域网地址：%+v", srv)
	}
	memberships, _ := m.st.ListNetworkMemberships(device.Name)
	if len(memberships) != 2 {
		t.Fatalf("复用设备不得重复登记网络成员：%+v", memberships)
	}

	// 只有虚拟局域网成员的已有设备没有物理地址可取；空兜底主机允许保存。
	if _, err := m.st.RemoveNetworkMembership(device.Name, physical); err != nil {
		t.Fatal(err)
	}
	result, err = m.Add(AddRequest{
		Server:     models.Server{Name: "db2", User: "root", Port: 22},
		DeviceName: device.Name, ExistingDevice: true,
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	srv, _ = m.st.GetServer(result.ServerID)
	if srv.Host != "" {
		t.Fatalf("没有物理局域网地址时兜底主机应为空：%+v", srv)
	}
}

// TestManagerAddServerWithoutProfilesStaysInvisible 证明不选 profile 是合法
// 保存：条目保留，但没有任何 profile 能看到它。
func TestManagerAddServerWithoutProfilesStaysInvisible(t *testing.T) {
	m := newTestManager(t)
	_, _, profileID, _ := seedAddTopology(t, m)
	result, err := m.Add(AddRequest{
		Server: models.Server{Name: "invisible", User: "root", Port: 22, Host: "192.0.2.10"},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	granted, err := m.st.ServersForProfile(profileID)
	if err != nil || len(granted) != 0 {
		t.Fatalf("未选择 profile 时不应授权：%v %v", granted, err)
	}
	if srv, _ := m.st.GetServer(result.ServerID); srv == nil {
		t.Fatal("未选择 profile 时服务器条目仍应保存")
	}
}

func TestManagerAddServerValidatesBeforeWriting(t *testing.T) {
	m := newTestManager(t)
	physical, _, profileID, _ := seedAddTopology(t, m)
	other, err := m.st.AddDevice("已有设备", "", "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Add(AddRequest{
		Server:     models.Server{Name: "bad", User: "root", Port: 22},
		DeviceName: other.Name, ExistingDevice: true,
	}, CLISource())
	var missingHostErr *FieldError
	if !errors.As(err, &missingHostErr) || !strings.Contains(missingHostErr.Page, "兜底主机") || !strings.Contains(missingHostErr.Page, "网络成员地址") {
		t.Fatalf("已有设备没有成员且未给兜底主机应拒绝：%v", err)
	}
	if servers, _ := m.st.ListServers(); len(servers) != 0 {
		t.Fatalf("校验失败不得写入服务器条目：%v", servers)
	}
	if _, err := m.st.AddNetworkMembership(other.Name, physical, "192.168.1.30"); err != nil {
		t.Fatal(err)
	}

	_, err = m.Add(AddRequest{
		Server: models.Server{Name: "conflict", User: "root", Port: 22, Host: "198.51.100.1"},
		Memberships: []NetworkMembershipInput{
			{Network: physical, Address: "192.168.1.30"},
		},
	}, CLISource())
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || !strings.Contains(fieldErr.Page, "已有设备") || !strings.Contains(fieldErr.Page, "家A局域网") {
		t.Fatalf("网络地址冲突应说明占用设备与网络：%v", err)
	}
	if servers, _ := m.st.ListServers(); len(servers) != 0 {
		t.Fatalf("地址冲突不得先写入服务器条目：%v", servers)
	}

	_, err = m.Add(AddRequest{
		Server:     models.Server{Name: "profile", User: "root", Port: 22, Host: "198.51.100.2"},
		ProfileIDs: []string{"no-such-profile"},
	}, CLISource())
	if err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("profile 不存在应拒绝：%v", err)
	}
	if servers, _ := m.st.ListServers(); len(servers) != 0 {
		t.Fatalf("profile 校验失败不得写入服务器条目：%v", servers)
	}
	if granted, _ := m.st.ServersForProfile(profileID); len(granted) != 0 {
		t.Fatalf("校验失败不得授权：%v", granted)
	}
}
