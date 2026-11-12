package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestUploadContentRecordsSecretDeliveryOnSingleMachine(t *testing.T) {
	const firstValue = "single-machine-first-secret-3f4a91"
	const secondValue = "single-machine-second-secret-7b8c02"
	vaultStore := newStore(t)
	if err := vaultStore.AddSecret("API_TOKEN", "接口令牌", []byte(firstValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	address, hostKey, cleanupServer := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanupServer)
	serverID := seedRealServer(t, vaultStore, "real", address, hostKey, "")
	profileID, err := vaultStore.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := vaultStore.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := vaultStore.AllowSecrets([]string{"API_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(vaultStore, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, server)

	remotePath := filepath.Join(t.TempDir(), "actual.yaml")
	firstTemplate := "token: {{sshmgr-secret:API_TOKEN}}\n"
	firstResult := callUploadContent(t, clientSession, serverID, firstTemplate, toSlash(remotePath))
	if firstResult.DeliveryRecordStatus != "recorded" || firstResult.DeliveryRecordReason != "" {
		t.Fatalf("第一次登记结果 = status %q reason %q，期待已登记且无原因",
			firstResult.DeliveryRecordStatus, firstResult.DeliveryRecordReason)
	}
	firstWritten := "token: " + firstValue + "\n"
	if got, err := os.ReadFile(remotePath); err != nil || string(got) != firstWritten {
		t.Fatalf("第一次远端文件内容 = %q, %v", got, err)
	}

	if err := vaultStore.SetSecretValue("API_TOKEN", []byte(secondValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	secondTemplate := "token={{sshmgr-secret:API_TOKEN}}\n"
	secondResult := callUploadContent(t, clientSession, serverID, secondTemplate, toSlash(remotePath))
	if secondResult.DeliveryRecordStatus != "recorded" || secondResult.DeliveryRecordReason != "" {
		t.Fatalf("重复路径登记结果 = status %q reason %q，期待已登记且无原因",
			secondResult.DeliveryRecordStatus, secondResult.DeliveryRecordReason)
	}
	secondWritten := "token=" + secondValue + "\n"
	if got, err := os.ReadFile(remotePath); err != nil || string(got) != secondWritten {
		t.Fatalf("重复写入后的远端文件内容 = %q, %v", got, err)
	}

	records, err := vaultStore.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("重复路径应只保留最新一条下发记录，实际 %d 条：%+v", len(records), records)
	}
	finalPath := toSlash(remotePath)
	digest := sha256.Sum256([]byte(secondWritten))
	record := records[0]
	if record.ServerID != serverID || record.Path != finalPath ||
		string(record.Template) != secondTemplate ||
		record.FileSHA256 != hex.EncodeToString(digest[:]) ||
		len(record.Secrets) != 1 || record.Secrets[0].Name != "API_TOKEN" ||
		record.Secrets[0].ValueVersion != 2 {
		t.Fatalf("下发记录字段不匹配：path=%q want=%q template=%q hash=%q secrets=%+v",
			record.Path, finalPath, record.Template, record.FileSHA256, record.Secrets)
	}
}

type uploadContentDeliveryResult struct {
	Bytes                int64    `json:"bytes"`
	FilledSecretNames    []string `json:"filled_secret_names"`
	DeliveryRecordStatus string   `json:"delivery_record_status"`
	DeliveryRecordReason string   `json:"delivery_record_reason,omitempty"`
}

func callUploadContent(
	t *testing.T, session *mcp.ClientSession, serverID, content, remotePath string,
) uploadContentDeliveryResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id":   serverID,
			"content":     content,
			"remote_path": remotePath,
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("upload_content 失败：err=%v result=%q", err, toolResultText(result))
	}
	var output uploadContentDeliveryResult
	unmarshalToolJSON(t, result, &output)
	if output.DeliveryRecordStatus == "" {
		t.Fatalf("upload_content 缺少登记状态：%s", toolResultText(result))
	}
	for _, forbidden := range []string{"file_sha256", "value_version", "first-secret", "second-secret"} {
		if strings.Contains(toolResultText(result), forbidden) {
			t.Fatalf("upload_content 结果暴露了不该返回的信息：%s", toolResultText(result))
		}
	}
	return output
}
