package webuiaccess

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestManagerAdapterUsesAccessManagerAndWebUIAuditSource(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credentialID, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("password")})
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := st.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}

	var source webui.AccessStore = FromManager(access.New(st))
	if err := source.AddProfile("team-a", webui.AccessWriteSource{RemoteAddress: "192.0.2.7"}); err != nil {
		t.Fatal(err)
	}
	change, conflict, err := source.SetServerGrants(
		profileID, nil, []string{serverID},
		webui.AccessWriteSource{RemoteAddress: "192.0.2.7"},
	)
	if err != nil || conflict != nil {
		t.Fatalf("授权调整失败：%v conflict=%+v", err, conflict)
	}
	if len(change.Added) != 1 || change.Added[0].Name != "web-1" {
		t.Fatalf("授权调整结果不正确：%+v", change)
	}

	profiles, err := source.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].Name != "team-a" || profiles[1].Servers[0].Name != "web-1" {
		t.Fatalf("适配后的 profile 列表不正确：%+v", profiles)
	}
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"profile.add", "profile.grant"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("审计行数量 = %d，应为 2：%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.7" {
			t.Fatalf("审计来源应带 Web UI 与来源地址，实际 %+v", row)
		}
	}
}

func TestManagerAdapterDeviceCodesIssueListAndRevoke(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}

	var source webui.AccessStore = FromManager(access.New(st, access.WithServeCertFingerprint(func() (string, error) {
		return "sha256:adapter-fingerprint", nil
	})))
	result, err := source.AddDeviceCode(
		"laptop", profileID, "https://serve.example:7878",
		webui.AccessWriteSource{RemoteAddress: "192.0.2.7"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "laptop" || result.Code == "" || result.Fingerprint != "sha256:adapter-fingerprint" {
		t.Fatalf("签发结果映射不正确：%+v", result)
	}
	if result.PullCommand != "sshmgr cache pull --url https://serve.example:7878 --token '"+result.Code+":sha256:adapter-fingerprint'" {
		t.Fatalf("拉取命令映射不正确：%q", result.PullCommand)
	}

	rows, err := source.ListDeviceCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "laptop" || rows[0].Status != "active" || rows[0].ProfileName != "运维" {
		t.Fatalf("适配后的设备码列表不正确：%+v", rows)
	}
	if rows[0].TokenPrefix == "" || rows[0].ID == "" {
		t.Fatalf("适配后的设备码行缺少标识符或前缀：%+v", rows[0])
	}

	if err := source.RevokeDeviceCode(rows[0].ID, webui.AccessWriteSource{RemoteAddress: "192.0.2.7"}); err != nil {
		t.Fatal(err)
	}
	rows, err = source.ListDeviceCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "revoked" {
		t.Fatalf("撤销后列表应仍含该行并标记已撤销：%+v", rows)
	}

	auditRows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"cache-token.add", "cache-token.revoke"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 2 {
		t.Fatalf("设备码审计行数量 = %d，应为 2：%+v", len(auditRows), auditRows)
	}
	for _, row := range auditRows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.7" {
			t.Fatalf("设备码审计来源应带 Web UI 与来源地址，实际 %+v", row)
		}
	}
}
