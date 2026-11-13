package store

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestNetworkWritesKeepCommandLineAndWebUISources(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	cliInput := models.Network{Name: "命令行网络", Type: models.NetworkPhysical, SiteName: "家A"}
	webInput := models.Network{Name: "Web UI 网络", Type: models.NetworkPhysical, SiteName: "家A"}
	if _, err := st.AddNetwork(cliInput); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkWithSource(webInput, WebUISource("192.0.2.9")); err != nil {
		t.Fatal(err)
	}

	cliNote, webNote := "命令行备注", "Web UI 备注"
	if _, err := st.EditNetwork(cliInput.Name, NetworkChanges{Note: &cliNote}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EditNetworkWithSource(
		webInput.Name, NetworkChanges{Note: &webNote}, WebUISource("192.0.2.9"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteNetwork(cliInput.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteNetworkWithSource(webInput.Name, WebUISource("192.0.2.9")); err != nil {
		t.Fatal(err)
	}

	assertNetworkAuditSource := func(action, networkName, source, sourceIP string) {
		t.Helper()
		rows, err := st.QueryAudit(AuditFilter{Actions: []string{action}})
		if err != nil {
			t.Fatalf("查询网络审计行失败：%v", err)
		}
		for _, row := range rows {
			if strings.Contains(row.Command, networkName) {
				if row.Source != source || row.SourceIP != sourceIP {
					t.Fatalf("网络 %s 的 %s 审计来源错误：want %q/%q，got %+v",
						networkName, action, source, sourceIP, row)
				}
				return
			}
		}
		t.Fatalf("找不到网络 %s 的 %s 审计行：%+v", networkName, action, rows)
	}
	assertNetworkAuditSource("network.add", cliInput.Name, "", "")
	assertNetworkAuditSource("network.add", webInput.Name, AuditSourceWebUI, "192.0.2.9")
	assertNetworkAuditSource("network.edit", cliInput.Name, "", "")
	assertNetworkAuditSource("network.edit", webInput.Name, AuditSourceWebUI, "192.0.2.9")
	assertNetworkAuditSource("network.rm", cliInput.Name, "", "")
	assertNetworkAuditSource("network.rm", webInput.Name, AuditSourceWebUI, "192.0.2.9")
}
