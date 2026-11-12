package mcpserver_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	clientops "ssh-manager-mcp/internal/clientops"
	mcpserver "ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestUploadContentClientRecordsDeliveryThroughAuthority(t *testing.T) {
	const secretValue = "client-delivery-secret-91d4e6"
	authority := newDeliveryE2EVault(t)
	address, hostKey, cleanupTarget := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanupTarget)
	serverID := seedDeliveryE2EServer(t, authority, address, hostKey)
	if err := authority.AddSecret("CLIENT_TOKEN", "客户端接口令牌", []byte(secretValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := authority.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := authority.AllowSecrets([]string{"CLIENT_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
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
	if _, err := authority.AddDevice("office-device", "", "office-laptop"); err != nil {
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
	t.Cleanup(authorityHTTP.Close)
	certificatePin := mcpserver.SPKIFingerprint(authorityHTTP.Certificate())
	forwarder, err := clientops.NewSecretDeliveryForwarder(clientops.CacheCred{
		URL: authorityHTTP.URL, Token: deviceToken, Pin: certificatePin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(forwarder.Close)

	auditPath := filepath.Join(t.TempDir(), "client-audit.jsonl")
	broker, tunnels, tasks, _, cleanupBroker, err := mcpserver.NewCacheBrokerWithRefresherAndDeliveryRecorder(
		projectToken, snapshot, auditPath, nil, nil, "laptop", nil, nil, forwarder,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupBroker)
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectDeliveryE2EClient(t, broker)

	target := filepath.Join(t.TempDir(), "client-config.yaml")
	template := "token: {{sshmgr-secret:CLIENT_TOKEN}}\n"
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id": serverID, "content": template, "remote_path": target,
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("客户端 upload_content 失败：err=%v result=%q", err, deliveryE2EToolText(result))
	}
	var output struct {
		FilledSecretNames    []string `json:"filled_secret_names"`
		DeliveryRecordStatus string   `json:"delivery_record_status"`
		DeliveryRecordReason string   `json:"delivery_record_reason"`
	}
	if err := json.Unmarshal([]byte(deliveryE2EToolText(result)), &output); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(output.FilledSecretNames) != fmt.Sprint([]string{"CLIENT_TOKEN"}) ||
		output.DeliveryRecordStatus != "recorded" || output.DeliveryRecordReason != "" {
		t.Fatalf("客户端上传结果 = %#v，期待填值并已登记", output)
	}
	written := "token: " + secretValue + "\n"
	if got, rerr := os.ReadFile(target); rerr != nil || string(got) != written {
		t.Fatalf("远端文件内容 = %q, %v", got, rerr)
	}

	records, err := authority.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("权威端应登记一条下发记录，实际 %d 条：%+v", len(records), records)
	}
	digest := sha256.Sum256([]byte(written))
	record := records[0]
	if record.ServerID != serverID || record.Path != target ||
		string(record.Template) != template ||
		record.FileSHA256 != hex.EncodeToString(digest[:]) ||
		len(record.Secrets) != 1 || record.Secrets[0].Name != "CLIENT_TOKEN" ||
		record.Secrets[0].ValueVersion != 1 ||
		record.InitiatingDeviceName != "office-device" ||
		record.InitiatingInstanceName != "office-laptop" {
		t.Fatalf("权威端下发记录不匹配：%+v", record)
	}
	if strings.Contains(deliveryE2EToolText(result), secretValue) ||
		strings.Contains(deliveryE2EToolText(result), "file_sha256") ||
		strings.Contains(deliveryE2EToolText(result), "value_version") {
		t.Fatalf("客户端上传结果暴露了不该返回的信息：%s", deliveryE2EToolText(result))
	}

	localAudit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(localAudit), secretValue) ||
		strings.Contains(string(localAudit), "secret-delivery-record") {
		t.Fatalf("客户端本地审计只应镜像上传操作，不应包含值或本地下发记录：%s", localAudit)
	}

	plainTarget := filepath.Join(t.TempDir(), "plain.txt")
	plainResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id": serverID, "content": "plain text\n", "remote_path": plainTarget,
		},
	})
	if err != nil || plainResult.IsError {
		t.Fatalf("不含占位符的 upload_content 失败：err=%v result=%q", err, deliveryE2EToolText(plainResult))
	}
	var plainOutput struct {
		DeliveryRecordStatus string `json:"delivery_record_status"`
		DeliveryRecordReason string `json:"delivery_record_reason"`
	}
	if err := json.Unmarshal([]byte(deliveryE2EToolText(plainResult)), &plainOutput); err != nil {
		t.Fatal(err)
	}
	if plainOutput.DeliveryRecordStatus != "not_recorded" ||
		!strings.Contains(plainOutput.DeliveryRecordReason, "内容不含机密项占位符") {
		t.Fatalf("不含占位符的登记结果 = %#v，应说明无需登记", plainOutput)
	}
	recordsAfterPlain, err := authority.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recordsAfterPlain) != 1 {
		t.Fatalf("不含占位符的上传不应新增或清理下发记录，实际 %d 条", len(recordsAfterPlain))
	}
}

func newDeliveryE2EVault(t *testing.T) *store.Store {
	t.Helper()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	vaultStore, err := store.Open(filepath.Join(t.TempDir(), "authority.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vaultStore.Close() })
	return vaultStore
}

func seedDeliveryE2EServer(t *testing.T, vaultStore *store.Store, address string, hostKey ssh.PublicKey) string {
	t.Helper()
	host, portText, _ := strings.Cut(address, ":")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, err := vaultStore.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &models.Server{
		Name: "real", Host: host, Port: port, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}
	serverID, err := vaultStore.AddServer(server)
	if err != nil {
		t.Fatal(err)
	}
	server.ID = serverID
	if err := vaultStore.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	return serverID
}

func connectDeliveryE2EClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func deliveryE2EToolText(result *mcp.CallToolResult) string {
	var builder strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	return builder.String()
}
