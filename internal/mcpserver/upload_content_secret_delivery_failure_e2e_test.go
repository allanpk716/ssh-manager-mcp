package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
)

func TestUploadContentClientDeliveryFailureKeepsFileAndReportsReason(t *testing.T) {
	cases := []struct {
		name       string
		closeFirst bool
		refuse     bool
		wantReason string
	}{
		{
			name:       "权威端不可达",
			closeFirst: true,
			wantReason: "无法访问权威端",
		},
		{
			name:       "授权校验拒绝",
			refuse:     true,
			wantReason: "not granted to the initiating profile",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			const secretValue = "delivery-failure-secret-52a7bd"
			environment := newDeliveryClientE2E(t, "FAILURE_TOKEN", secretValue)
			if testCase.closeFirst {
				environment.authorityWeb.Close()
			} else if testCase.refuse {
				if _, err := environment.authority.RevokeServers(
					environment.profileID, []string{environment.serverID},
				); err != nil {
					t.Fatal(err)
				}
			}
			result, err := environment.session.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "upload_content",
				Arguments: map[string]any{
					"server_id":   environment.serverID,
					"content":     "token={{sshmgr-secret:FAILURE_TOKEN}}\n",
					"remote_path": environment.remoteTarget,
				},
			})
			if err != nil || result.IsError {
				t.Fatalf("登记失败不应让文件写入失败：err=%v result=%q", err, deliveryE2EToolText(result))
			}
			var output struct {
				FilledSecretNames    []string `json:"filled_secret_names"`
				DeliveryRecordStatus string   `json:"delivery_record_status"`
				DeliveryRecordReason string   `json:"delivery_record_reason"`
			}
			if err := json.Unmarshal([]byte(deliveryE2EToolText(result)), &output); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(output.FilledSecretNames) != fmt.Sprint([]string{"FAILURE_TOKEN"}) ||
				output.DeliveryRecordStatus != "not_recorded" ||
				!strings.Contains(output.DeliveryRecordReason, testCase.wantReason) {
				t.Fatalf("登记失败结果 = %#v，期待未登记并包含 %q", output, testCase.wantReason)
			}
			if got, rerr := os.ReadFile(environment.target); rerr != nil || string(got) != "token="+secretValue+"\n" {
				t.Fatalf("登记失败后的远端文件内容 = %q, %v", got, rerr)
			}
			if !testCase.closeFirst {
				records, rerr := environment.authority.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
				if rerr != nil {
					t.Fatal(rerr)
				}
				if len(records) != 0 {
					t.Fatalf("登记失败不应在权威端留下记录：%+v", records)
				}
			}
			if strings.Contains(deliveryE2EToolText(result), secretValue) {
				t.Fatalf("登记失败结果泄露机密值：%s", deliveryE2EToolText(result))
			}
		})
	}
}
