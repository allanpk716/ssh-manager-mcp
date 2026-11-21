package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestExportSnapshot_LegacyHostKeysSkipEmptyHost：导出给旧客户端的旧格式主
// 机密钥指纹锚行跳过主机为空的条目，不产生「:22」这样的假行；非空条目的
// 分组行为不变（同地址同密钥合并为一行、按地址排序）。
func TestExportSnapshot_LegacyHostKeysSkipEmptyHost(t *testing.T) {
	s := newTestStore(t)
	credID, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("s3cr3t")})
	if err != nil {
		t.Fatal(err)
	}
	hosted := &models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: credID}
	hostedID, err := s.AddServer(hosted)
	if err != nil {
		t.Fatal(err)
	}
	hosted.ID = hostedID
	sibling := &models.Server{Name: "gpu2", Host: "192.0.2.10", Port: 22, User: "root",
		AuthMethod: models.AuthPassword, CredentialID: credID}
	siblingID, err := s.AddServer(sibling)
	if err != nil {
		t.Fatal(err)
	}
	sibling.ID = siblingID
	bare := &models.Server{Name: "bare", Host: "", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: credID}
	bareID, err := s.AddServer(bare)
	if err != nil {
		t.Fatal(err)
	}
	bare.ID = bareID
	// 三台条目各落同一把密钥的锚：非空两台同地址应合并成一行，空主机
	// 条目不产生任何旧行。
	key := []byte("legacy-skip-fake-host-key")
	for _, srv := range []*models.Server{hosted, sibling, bare} {
		if err := s.SaveEntryPin(srv, key); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.EntryHostKeys) != 3 {
		t.Fatalf("条目锚应全部保留在 EntryHostKeys：%+v", snap.EntryHostKeys)
	}
	if len(snap.HostKeys) != 1 || snap.HostKeys[0].HostPort != "192.0.2.10:22" {
		t.Fatalf("旧格式锚行应只保留非空主机的分组：%+v", snap.HostKeys)
	}
	for _, row := range snap.HostKeys {
		if row.HostPort == ":22" {
			t.Fatalf("旧格式锚行不得包含空主机假地址：%+v", row)
		}
	}
}
