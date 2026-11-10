package clientops

// 票 #188：声明草稿提交的客户端转发器。进程内端到端沿用查询转发器的测
// 试写法，钉住两端契约、设备码认证与分支错误文本。

import (
	"net/http/httptest"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestSoftwareDeclarationDraftsForwarderAgainstServeHandler 转发器把一批草
// 稿提交到权威端，拿回草稿编号；权威端只写草稿并记录设备码编号。
func TestSoftwareDeclarationDraftsForwarderAgainstServeHandler(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/serve.db", mustClientopsMasterKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddDevice("gpu-设备", "", ""); err != nil {
		t.Fatal(err)
	}
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "gpu-设备")
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	runner, err := mcpserver.NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	serve := httptest.NewTLSServer(runner.HTTPHandler())
	defer serve.Close()
	_, token, err := st.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	pin := mcpserver.SPKIFingerprint(serve.Certificate())
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: serve.URL, Token: token + ":" + pin, Pin: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	if err := st.ReplaceSoftwareObservedItems("gpu-设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			StableKey: "observed-key-1", Category: models.SoftwareCategoryUnidentified,
			Name: "unknown-service", ExecutablePath: "/opt/gateway/bin/gateway",
		}},
		ServerID: added.ServerID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	output, err := forwarder.DraftSoftwareDeclarations(mcpserver.DraftSoftwareDeclarationsInput{
		Declarations: []models.SoftwareDeclarationDraft{
			{
				SoftwareDeclaration: models.SoftwareDeclaration{
					Device: "gpu-设备", Category: models.SoftwareCategoryServiceProgram,
					Name: "业务网关", Version: "2.1",
					ListeningPorts: []models.SoftwareListeningPort{{
						Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
						Exposure: models.SoftwareExposureVirtualLAN,
					}},
				},
				ClaimedObservedKey: "observed-key-1",
			},
			{
				SoftwareDeclaration: models.SoftwareDeclaration{
					Device: "gpu-设备", Category: models.SoftwareCategorySystemPackage,
					Name: "libc6", Version: "2.39-0ubuntu8",
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.DraftIDs) != 2 || output.DraftIDs[0] == "" {
		t.Fatalf("应拿回两个草稿编号：%+v", output)
	}
	drafts, err := st.SoftwareDeclarationDrafts(store.AllSoftwareDevices())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 2 {
		t.Fatalf("权威端应只写草稿：%+v", drafts)
	}
	found := false
	for _, draft := range drafts {
		if draft.ClaimedObservedKey == "observed-key-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("权威端应保留认领键：%+v", drafts)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"software-declaration-draft"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Source != store.AuditSourceForward {
		t.Fatalf("草稿提交审计应记录受审计转发写来源：%+v", rows)
	}

	// 越界设备：403 分支文本。
	if _, err := st.AddDevice("授权外设备", "", ""); err != nil {
		t.Fatal(err)
	}
	_, err = forwarder.DraftSoftwareDeclarations(mcpserver.DraftSoftwareDeclarationsInput{
		Declarations: []models.SoftwareDeclarationDraft{{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "授权外设备", Category: models.SoftwareCategoryServiceProgram,
				Name: "redis", Version: "7.2",
			},
		}},
	})
	if err == nil || err.Error() != softwareDeclarationDraftsForbiddenText {
		t.Fatalf("越界设备应返回 403 分支文本：%v", err)
	}
}

// TestSoftwareDeclarationDraftsForwarderUnreachable 没有拉取凭据时与不可
// 达同样固定报错，不排队、不留本地状态。
func TestSoftwareDeclarationDraftsForwarderUnreachable(t *testing.T) {
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	_, err = forwarder.DraftSoftwareDeclarations(mcpserver.DraftSoftwareDeclarationsInput{
		Declarations: []models.SoftwareDeclarationDraft{{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "gpu-设备", Category: models.SoftwareCategoryServiceProgram,
				Name: "nginx", Version: "1.25.3",
			},
		}},
	})
	if err == nil || !IsSoftwareInventoryUnreachable(err) {
		t.Fatalf("无凭据时应按权威端不可达分支报错：%v", err)
	}
}
