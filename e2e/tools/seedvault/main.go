// seedvault 是 Playwright 浏览器测试的造库工具：在指定目录里生成一个可
// 直接打开的单机 vault（两台服务器、一个 profile、一条初始授权），并写好
// 单机角色状态。测试进程用它准备临时加密库，再启动 `sshmgr ui`。
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：seedvault <临时库目录>")
		os.Exit(2)
	}
	if err := seed(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	vaultDir := filepath.Join(dir, "ssh-manager")
	if err := os.MkdirAll(vaultDir, 0o700); err != nil {
		return err
	}
	backupDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(backupDir, ".ssh-manager-backup-marker"), nil, 0o600); err != nil {
		return err
	}
	passPath := filepath.Join(dir, "backup.pass")
	if err := os.WriteFile(passPath, []byte("browser-test-backup-pass"), 0o600); err != nil {
		return err
	}
	key, err := store.GenerateMasterKey()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "master.key.plain")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), key)
	if err != nil {
		return err
	}
	defer st.Close()
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("browser-test-password"),
	})
	if err != nil {
		return err
	}
	device, err := st.AddDevice("工作站", "浏览器测试设备", "")
	if err != nil {
		return err
	}
	webID, err := st.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 22, User: "owner", Role: "运维网站",
		DeviceID:   device.ID,
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		return err
	}
	if _, err := st.AddServer(&models.Server{
		Name: "db-1", Host: "192.0.2.11", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}); err != nil {
		return err
	}
	oldID, err := st.AddServerWithCredentials(&models.Server{
		Name: "old-1", Host: "192.0.2.12", Port: 22, User: "owner", Role: "待删除",
	}, &models.Credential{
		Type: models.CredPassword, Secret: []byte("browser-test-old-password"),
	}, nil)
	if err != nil {
		return err
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		return err
	}
	if err := st.GrantServers(profileID, []string{webID, oldID}); err != nil {
		return err
	}
	if _, _, err := st.AddCacheToken("laptop-client", profileID); err != nil {
		return err
	}
	if err := st.AddSecretWithEnvironmentName("MATRIX_TOKEN", "浏览器测试机密项", []byte("browser-test-secret-value"), "MATRIX_TOKEN_FILE", store.CLISource()); err != nil {
		return err
	}
	if err := st.AllowSecrets([]string{"MATRIX_TOKEN"}, []string{"db-1"}, store.CLISource()); err != nil {
		return err
	}
	if strings.TrimSpace(os.Getenv("SSHMGR_E2E_SHARES")) == "1" {
		if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: "share-db", Host: "192.0.2.13", Port: 22, User: "owner", ShellType: models.ShellPOSIX,
		}, &models.Credential{Type: models.CredPassword, Secret: []byte("browser-test-share-db-password")}, nil, "工作站"); err != nil {
			return err
		}
		if err := st.AddSecret("SHARE_MOUNT_PASSWORD", "共享存储挂载密码", []byte("share-mount-password-value"), store.CLISource()); err != nil {
			return err
		}
		if err := st.AllowSecrets([]string{"SHARE_MOUNT_PASSWORD"}, []string{"web-1"}, store.CLISource()); err != nil {
			return err
		}
		if _, err := st.AddSharedStorage(store.SharedStorageInput{
			Name: "datasets", Device: "工作站", User: "browser-mount-user",
			PasswordSecret: "SHARE_MOUNT_PASSWORD", Drive: "X",
			Directories: []models.SharedStorageDirectory{{Purpose: "临时数据", Path: "tmp"}},
		}, store.CLISource()); err != nil {
			return err
		}
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: models.SoftwareCategoryOperatingSystem, Name: "Ubuntu", Version: "24.04.1 LTS"},
			{Category: models.SoftwareCategorySystemPackage, Name: "base-files", Version: "13.5ubuntu3"},
			{Category: models.SoftwareCategorySystemPackage, Name: "zlib1g", Version: "1:1.3.dfsg-3.1ubuntu1"},
			{
				Category: models.SoftwareCategoryServiceProgram, Name: "nginx", Version: "1.24.0-2ubuntu7",
				ListeningPorts: []models.SoftwareListeningPort{{
					Address: "0.0.0.0", Port: 80, Protocol: "tcp",
					Exposure: models.SoftwareExposureAllAddresses,
				}},
			},
			{
				StableKey: "unidentified-8443", Category: models.SoftwareCategoryUnidentified,
				Name: "unknown-gateway", ProcessName: "gateway", ExecutablePath: "/opt/gateway/gateway",
				ListeningPorts: []models.SoftwareListeningPort{{
					Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
					Exposure: models.SoftwareExposureAllAddresses,
				}},
			},
		},
		ServerID: webID, CommandSetVersion: softwareinventory.POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		return err
	}
	if _, err := softwareinventory.New(st).SubmitDeclarationDrafts([]models.SoftwareDeclarationDraft{{
		SoftwareDeclaration: models.SoftwareDeclaration{
			Device: "工作站", Category: models.SoftwareCategoryServiceProgram,
			Name: "业务网关", Version: "2.1.0",
		},
	}}, softwareinventory.AllDevicesVisible(), softwareinventory.AgentSource()); err != nil {
		return err
	}
	for i := 0; i < 120; i++ {
		if err := st.WriteAudit(store.AuditRow{
			TS: time.Now().Add(-time.Duration(i) * time.Minute), Action: "e2e.audit",
			Command: fmt.Sprintf("浏览器测试审计记录 %d", i+1), Status: "ok",
			Source: store.AuditSourceCLI,
		}); err != nil {
			return err
		}
	}
	if err := seedTopology(st); err != nil {
		return err
	}
	role := roles.RoleStandalone
	if os.Getenv("SSHMGR_E2E_ROLE") == "server" {
		role = roles.RoleServer
		if err := writeBrowserTestCertificate(dir); err != nil {
			return err
		}
		if _, err := st.SetWebAdminPasswordWithAudit(
			[]byte(os.Getenv("SSHMGR_E2E_ADMIN_PASSWORD")),
			store.AuditRow{Action: "admin.password_set", Status: "ok", Source: store.AuditSourceCLI},
		); err != nil {
			return err
		}
		if port := strings.TrimSpace(os.Getenv("SSHMGR_E2E_WEB_PORT")); port != "" {
			if err := st.SetSetting("serve.ui_port", port); err != nil {
				return err
			}
			if err := st.SetSetting("serve.ui_listen", "127.0.0.1"); err != nil {
				return err
			}
		}
	}
	return roles.Save(roles.State{Role: role, SetupComplete: true})
}

// writeBrowserTestCertificate 给权威端浏览器测试生成一张 P-256 椭圆曲线
// 自签名证书。正式 serve 的自动证书使用 Ed25519 签名算法供客户端钉死指
// 纹；这份测试证书只让 Chromium 能完成临时权威端页面的传输层安全握手。
func writeBrowserTestCertificate(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sshmgr browser test"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
	}
	certificate, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})
	if err := os.WriteFile(filepath.Join(dir, "browser-test-cert.pem"), certificatePEM, 0o600); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return os.WriteFile(filepath.Join(dir, "browser-test-key.pem"), keyPEM, 0o600)
}

// seedTopology 为网络拓扑浏览器测试补充拓扑数据。只在设置
// SSHMGR_E2E_TOPOLOGY 时启用，避免改变其它用例看到的列表与详情。
func seedTopology(st *store.Store) error {
	if strings.TrimSpace(os.Getenv("SSHMGR_E2E_TOPOLOGY")) != "1" {
		return nil
	}
	if _, err := st.AddSite("家A", "物理场所"); err != nil {
		return err
	}
	if _, err := st.AddSite("机房", ""); err != nil {
		return err
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A", Note: "家里的网络",
		NetworkRange: "192.168.1.0/24", BandwidthMbps: 1000, Metered: true,
	}); err != nil {
		return err
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房",
	}); err != nil {
		return err
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual, Note: "跨站点"}); err != nil {
		return err
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网B", Type: models.NetworkVirtual}); err != nil {
		return err
	}
	if _, err := st.MoveServerToDevice("db-1", "工作站"); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20"); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("工作站", "虚拟网", "100.64.0.20"); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("工作站", "虚拟网B", "100.65.0.20"); err != nil {
		return err
	}
	if _, err := st.AddDevice("NAS", "照片与备份", ""); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("NAS", "家A局域网", "192.168.1.30"); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("NAS", "虚拟网", "100.64.0.30"); err != nil {
		return err
	}
	if _, err := st.AddDevice("机房机器", "", ""); err != nil {
		return err
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "gpu-1", Host: "203.0.113.5", Port: 22, User: "deploy",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("browser-test-password")}, nil, "机房机器"); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("机房机器", "机房局域网", "10.0.0.5"); err != nil {
		return err
	}
	if _, err := st.AddDevice("笔记本", "", "laptop-client"); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("笔记本", "虚拟网", "100.64.0.9"); err != nil {
		return err
	}
	if _, err := st.AddDevice("探测机", "浏览器测试探测目标", ""); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("探测机", "家A局域网", "127.0.0.1"); err != nil {
		return err
	}
	if _, err := st.AddNetworkMembership("探测机", "虚拟网", "127.0.0.2"); err != nil {
		return err
	}
	if port := strings.TrimSpace(os.Getenv("SSHMGR_E2E_PROBE_PORT")); port != "" {
		probePort, err := strconv.Atoi(port)
		if err != nil {
			return fmt.Errorf("SSHMGR_E2E_PROBE_PORT 不是端口号：%w", err)
		}
		if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: "probe-target", Host: "127.0.0.1", Port: probePort, User: "owner",
		}, &models.Credential{Type: models.CredPassword, Secret: []byte("browser-test-password")}, nil, "探测机"); err != nil {
			return err
		}
	}
	if err := st.AddSecretWithEnvironmentName("NAS_PASSWORD", "NAS 登录密码", []byte("nas-login-secret"), "", store.CLISource()); err != nil {
		return err
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "家NAS共享", Device: "NAS", User: "backup", PasswordSecret: "NAS_PASSWORD", Drive: "N",
	}, store.CLISource()); err != nil {
		return err
	}
	workstation, err := st.GetDevice("工作站")
	if err != nil {
		return err
	}
	if err := roles.SetLocalDeviceID(workstation.ID); err != nil {
		return err
	}
	return seedProbeResults(st, workstation.ID)
}

// seedProbeResults 为浏览器测试写一份本机起点的本地探测结果：NAS 家A局域
// 网成功、虚拟局域网超时；其余设备留空，用于呈现未探测与无共同网络。
func seedProbeResults(st *store.Store, originDeviceID string) error {
	if strings.TrimSpace(os.Getenv("SSHMGR_E2E_TOPOLOGY")) != "1" {
		return nil
	}
	nas, err := st.GetDevice("NAS")
	if err != nil {
		return err
	}
	path, ok, err := probeobserve.PathFor("")
	if err != nil || !ok {
		return err
	}
	results := proberesults.NewFileProbeResults(path)
	observedAt := time.Now().Add(-10 * time.Second)
	for _, observation := range []proberesults.Observation{
		{
			OriginDeviceID: originDeviceID, TargetDeviceID: nas.ID, Network: "家A局域网",
			Address: "192.168.1.30", Port: 22, OK: true, Duration: 3 * time.Millisecond,
			ObservedAt: observedAt, Source: proberesults.SourceProbe,
		},
		{
			OriginDeviceID: originDeviceID, TargetDeviceID: nas.ID, Network: "虚拟网",
			Address: "100.64.0.30", Port: 22, Category: models.ProbeErrorTimeout,
			Duration: 1500 * time.Millisecond, ObservedAt: observedAt, Source: proberesults.SourceProbe,
		},
	} {
		if err := results.Record(observation); err != nil {
			return err
		}
	}
	return nil
}
