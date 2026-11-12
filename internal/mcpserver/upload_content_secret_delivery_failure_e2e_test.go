package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	clientops "ssh-manager-mcp/internal/clientops"
	mcpserver "ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
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
			authority := newDeliveryE2EVault(t)
			address, hostKey, cleanupTarget := testsshd.Start(t, testsshd.Options{Password: "pw"})
			t.Cleanup(cleanupTarget)
			serverID := seedDeliveryE2EServer(t, authority, address, hostKey)
			if err := authority.AddSecret("FAILURE_TOKEN", "", []byte(secretValue), store.CLISource()); err != nil {
				t.Fatal(err)
			}
			profileID, err := authority.AddProfile("agent-profile")
			if err != nil {
				t.Fatal(err)
			}
			if err := authority.GrantServers(profileID, []string{serverID}); err != nil {
				t.Fatal(err)
			}
			if err := authority.AllowSecrets([]string{"FAILURE_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
				t.Fatal(err)
			}
			_, projectToken, err := authority.AddProject("客户端项目", profileID)
			if err != nil {
				t.Fatal(err)
			}
			deviceCodeID, deviceToken, err := authority.AddCacheToken("office-laptop", profileID)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := authority.ExportSnapshotForProfile(profileID, true, deviceCodeID)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := mcpserver.NewServeRunner(authority)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(runner.Close)
			authorityHTTP := httptest.NewTLSServer(runner.HTTPHandler())
			certificatePin := mcpserver.SPKIFingerprint(authorityHTTP.Certificate())
			forwarder, err := clientops.NewSecretDeliveryForwarder(clientops.CacheCred{
				URL: authorityHTTP.URL, Token: deviceToken, Pin: certificatePin,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(forwarder.Close)
			if testCase.closeFirst {
				authorityHTTP.Close()
			} else if testCase.refuse {
				if _, err := authority.RevokeServers(profileID, []string{serverID}); err != nil {
					t.Fatal(err)
				}
			}

			broker, tunnels, tasks, _, cleanupBroker, err := mcpserver.NewCacheBrokerWithRefresherAndDeliveryRecorder(
				projectToken, snapshot, filepath.Join(t.TempDir(), "audit.jsonl"), nil, nil,
				"laptop", nil, nil, forwarder,
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanupBroker)
			t.Cleanup(tunnels.CloseAll)
			t.Cleanup(tasks.CloseAll)
			clientSession := connectDeliveryE2EClient(t, broker)
			target := filepath.Join(t.TempDir(), "config.yaml")
			result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "upload_content",
				Arguments: map[string]any{
					"server_id":   serverID,
					"content":     "token={{sshmgr-secret:FAILURE_TOKEN}}\n",
					"remote_path": target,
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
			if got, rerr := os.ReadFile(target); rerr != nil || string(got) != "token="+secretValue+"\n" {
				t.Fatalf("登记失败后的远端文件内容 = %q, %v", got, rerr)
			}
			if !testCase.closeFirst {
				records, rerr := authority.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
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
