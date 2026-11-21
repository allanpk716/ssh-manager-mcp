package store

import (
	"encoding/json"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestDeviceDeleteServerRmSummaryEmptyHostKeepsEmptyHostString：删除设备级
// 联里的服务器条目删除审计（server.rm）对空主机条目保持 JSON 摘要 host 为
// 空串，机器可读接口不改语义。
func TestDeviceDeleteServerRmSummaryEmptyHostKeepsEmptyHostString(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	credential := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "裸条目", Host: "", Port: 22, User: "ops", AuthMethod: models.AuthPassword,
	}, credential, nil, "工作站"); err != nil {
		t.Fatal(err)
	}
	confirmed, err := st.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteDeviceCascading("工作站", confirmed); err != nil {
		t.Fatal(err)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"server.rm"}})
	if err != nil {
		t.Fatal(err)
	}
	// 设备级联里的条目删除审计不落 ServerID，按摘要里的条目名辨认。
	found := false
	for _, row := range rows {
		if !strings.Contains(row.Command, `"name":"裸条目"`) {
			continue
		}
		found = true
		summary := map[string]any{}
		if err := json.Unmarshal([]byte(row.Command), &summary); err != nil {
			t.Fatalf("server.rm 审计摘要应为 JSON：%q (%v)", row.Command, err)
		}
		if summary["host"] != "" {
			t.Fatalf("空主机条目的删除审计摘要 host 应为空串，实际 %v", summary["host"])
		}
	}
	if !found {
		t.Fatalf("应找到空主机条目的 server.rm 审计：%+v", rows)
	}
}
