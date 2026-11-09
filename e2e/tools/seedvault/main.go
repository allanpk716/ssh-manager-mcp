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
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
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
