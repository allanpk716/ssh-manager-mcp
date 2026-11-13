package sharemanage

import (
	"io"
	"strings"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestMountIsIdempotentAndReportsSafeResult(t *testing.T) {
	st := newCheckTestStore(t)
	fake := &mountFakeServer{}
	target := startCheckSSH(t, fake.handler)
	seedMountTopology(t, st)
	addRemoteMountServer(t, st, "linux-web", target)
	if err := st.AllowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	first, err := New(st).Mount(t.Context(), MountRequest{
		Share: "datasets", Server: "linux-web", Source: CLISource(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ServerName != "linux-web" || first.MountPoint != "/mnt/sshmgr/datasets" ||
		first.Network != "办公虚拟局域网" || first.NetworkSequence != 1 ||
		first.Action != "first-mount-success" || first.Version != 1 {
		t.Fatalf("首次挂载结果不正确：%+v", first)
	}
	if len(first.Notices) == 0 || !strings.Contains(strings.Join(first.Notices, "\n"), "物理局域网地址不通") {
		t.Fatalf("选地址降级应给出不含地址的警告：%+v", first)
	}
	if fake.credentialValue() != "username=mount-user\npassword=mount-password\n" {
		t.Fatal("远端固定命令应收到挂载账号与机密项值")
	}

	fake.setAction("no-change")
	second, err := New(st).Mount(t.Context(), MountRequest{
		Share: "datasets", Server: "linux-web", Source: CLISource(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != "no-change" || second.MountPoint != "/mnt/sshmgr/datasets" {
		t.Fatalf("重复挂载应呈现无需改动：%+v", second)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"share-mount"}})
	if err != nil || len(rows) != 2 || rows[0].Status != "ok" || rows[0].Source != store.AuditSourceCLI {
		t.Fatalf("挂载应逐次写入 owner 审计：%v %+v", err, rows)
	}
	auditText := rows[0].Command + rows[1].Command
	for _, forbidden := range []string{"mount-password", "192.0.2.10", "100.64.10.10"} {
		if strings.Contains(auditText, forbidden) {
			t.Fatalf("挂载审计不得包含密码或地址：%s", auditText)
		}
	}
}

func TestMountRejectsWindowsWithoutConnecting(t *testing.T) {
	st := newCheckTestStore(t)
	if err := st.AddSecret(
		"NAS_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	addLocalCheckServer(t, st, "windows-web", models.ShellPowerShell)
	if err := st.AllowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"}, []string{"windows-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	_, err := New(st).Mount(t.Context(), MountRequest{
		Share: "datasets", Server: "windows-web", Source: CLISource(),
	})
	if err == nil || !strings.Contains(err.Error(), "Windows 服务器不能持久挂载共享存储") ||
		!strings.Contains(err.Error(), "盘符由每条命令执行前的会话内注入提供") {
		t.Fatalf("Windows 应给出固定说明：%v", err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"share-mount"}})
	if err != nil || len(rows) != 1 || rows[0].Status != "error" ||
		rows[0].Command == "" || strings.Contains(rows[0].Command, "mount-password") {
		t.Fatalf("Windows 拒绝也应写入安全审计：%v %+v", err, rows)
	}
}

type mountFakeServer struct {
	mu         sync.Mutex
	nextAction string
	credential string
}

func (f *mountFakeServer) setAction(action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAction = action
}

func (f *mountFakeServer) credentialValue() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credential
}

func (f *mountFakeServer) handler(command string, stdin io.Reader) (string, string, int) {
	input, _ := io.ReadAll(stdin)
	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Linux\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one"):
		if strings.Contains(command, "192.0.2.10") {
			return "SSHMGR-PROBE 0 fail 2 no-route\nSSHMGR-PROBE 1 ok 2 \n", "", 0
		}
		return "SSHMGR-PROBE 0 ok 2 \n", "", 0
	case strings.Contains(command, "SSHMGR-LINUX-MOUNT"):
		action := "first-mount-success"
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.nextAction != "" {
			action = f.nextAction
		}
		f.credential = "username=mount-user\npassword=" + string(input)
		return "SSHMGR-LINUX-MOUNT result=" + action + " selected=1\n", "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

func seedMountTopology(t *testing.T, st *store.Store) {
	t.Helper()
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
	if _, err := st.AddDevice("linux-device", "挂载目标", ""); err != nil {
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
		"NAS_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
}

func addRemoteMountServer(
	t *testing.T, st *store.Store, name string, target checkSSHTarget,
) {
	t.Helper()
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: name, Host: target.address[:strings.LastIndex(target.address, ":")],
		Port: portOfCheckAddress(target.address), User: "deploy",
		ShellType: models.ShellPOSIX,
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
	if err := st.SaveEntryPin(server, target.hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
}
