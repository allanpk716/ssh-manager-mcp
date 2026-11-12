package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/sftp"

	"ssh-manager-mcp/internal/models"
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

func TestUploadContentRecordsWindowsSFTPRealPath(t *testing.T) {
	const secretValue = "windows-delivery-secret-40a23c"
	vaultStore := newStore(t)
	if err := vaultStore.AddSecret("WINDOWS_TOKEN", "", []byte(secretValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	handlers := &windowsSFTPRealPathHandlers{root: hostRoot}
	address, hostKey, cleanupServer := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		SFTPHandlers: sftp.Handlers{
			FileGet: handlers, FilePut: handlers, FileCmd: handlers, FileList: handlers,
		},
	})
	t.Cleanup(cleanupServer)
	serverID := seedRealServerShell(t, vaultStore, "windows-realpath", address, hostKey, models.ShellPowerShell)
	profileID, err := vaultStore.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := vaultStore.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := vaultStore.AllowSecrets([]string{"WINDOWS_TOKEN"}, []string{"windows-realpath"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(vaultStore, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, server)

	result := callUploadContent(
		t, clientSession, serverID,
		"token: {{sshmgr-secret:WINDOWS_TOKEN}}\n", `C:\Data\App.conf`,
	)
	if result.DeliveryRecordStatus != "recorded" || result.DeliveryRecordReason != "" {
		t.Fatalf("Windows 登记结果 = status %q reason %q，期待已登记且无原因",
			result.DeliveryRecordStatus, result.DeliveryRecordReason)
	}
	written := "token: " + secretValue + "\n"
	if got, rerr := os.ReadFile(filepath.Join(hostRoot, "Data", "App.conf")); rerr != nil || string(got) != written {
		t.Fatalf("Windows 远端文件内容 = %q, %v", got, rerr)
	}
	records, err := vaultStore.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{ServerID: serverID})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Path != "C:/Data/App.conf" {
		t.Fatalf("Windows SFTP 返回 /C:/ 时应登记为 C:/，实际：%+v", records)
	}
}

func TestFileUploadsDoNotClearSecretDeliveryRecord(t *testing.T) {
	const secretValue = "delivery-preserve-secret-82af30"
	vaultStore := newStore(t)
	if err := vaultStore.AddSecret("PRESERVE_TOKEN", "", []byte(secretValue), store.CLISource()); err != nil {
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
	if err := vaultStore.AllowSecrets([]string{"PRESERVE_TOKEN"}, []string{"real"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, tunnels, tasks, err := NewServer(vaultStore, profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	clientSession := connectInMemoryMCPClient(t, server)

	remotePath := toSlash(filepath.Join(t.TempDir(), "preserved.yaml"))
	template := "token: {{sshmgr-secret:PRESERVE_TOKEN}}\n"
	firstResult := callUploadContent(t, clientSession, serverID, template, remotePath)
	if firstResult.DeliveryRecordStatus != "recorded" {
		t.Fatalf("首次登记结果 = %+v，期待已登记", firstResult)
	}
	records, err := vaultStore.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{ServerID: serverID})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("首次上传后应有一条下发记录，实际 %d 条", len(records))
	}
	want := records[0]
	assertDeliveryRecordStillPresent := func(operation string) {
		t.Helper()
		after, err := vaultStore.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{ServerID: serverID})
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != 1 || after[0].Path != want.Path ||
			string(after[0].Template) != string(want.Template) ||
			after[0].FileSHA256 != want.FileSHA256 ||
			after[0].WrittenAt.Unix() != want.WrittenAt.Unix() ||
			len(after[0].Secrets) != 1 || after[0].Secrets[0].Name != "PRESERVE_TOKEN" ||
			after[0].Secrets[0].ValueVersion != 1 {
			t.Fatalf("%s 后下发记录应保持不变：%+v", operation, after)
		}
	}

	localSource := filepath.Join(t.TempDir(), "uploaded.txt")
	if err := os.WriteFile(localSource, []byte("uploaded without placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploadResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_file",
		Arguments: map[string]any{
			"server_id": serverID, "local_path": localSource, "remote_path": remotePath,
		},
	})
	if err != nil || uploadResult.IsError {
		t.Fatalf("upload_file 失败：err=%v result=%q", err, toolResultText(uploadResult))
	}
	assertDeliveryRecordStillPresent("upload_file 覆盖同路径")

	plainResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_content",
		Arguments: map[string]any{
			"server_id": serverID, "content": "plain text\n", "remote_path": remotePath,
		},
	})
	if err != nil || plainResult.IsError {
		t.Fatalf("不含占位符的 upload_content 失败：err=%v result=%q", err, toolResultText(plainResult))
	}
	assertDeliveryRecordStillPresent("不含占位符的 upload_content 覆盖同路径")

	relaySource := filepath.Join(t.TempDir(), "relayed.txt")
	if err := os.WriteFile(relaySource, []byte("relayed without placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relayResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_path": relaySource, "to_server_id": serverID,
			"to_path": remotePath, "fresh": true,
		},
	})
	if err != nil || relayResult.IsError {
		t.Fatalf("relay_file 失败：err=%v result=%q", err, toolResultText(relayResult))
	}
	var relayStart RelayOutput
	unmarshalToolJSON(t, relayResult, &relayStart)
	relayFinal := waitBackgroundTerminal(t, clientSession, relayStart.TaskID)
	if relayFinal.Status != bgStatusDone {
		t.Fatalf("relay_file 任务状态 = %q，错误 = %q", relayFinal.Status, relayFinal.Error)
	}
	if got, rerr := os.ReadFile(remotePath); rerr != nil || string(got) != "relayed without placeholder\n" {
		t.Fatalf("relay_file 覆盖后的远端文件内容 = %q, %v", got, rerr)
	}
	assertDeliveryRecordStillPresent("relay_file 覆盖同路径")
}

// windowsSFTPRealPathHandlers 模拟 Windows 自带 OpenSSH 的 SFTP 行为：写入
// C:/...，RealPath 却返回盘符前带斜杠的 /C:/...。
type windowsSFTPRealPathHandlers struct {
	root string
}

func (h *windowsSFTPRealPathHandlers) hostPath(remotePath string) string {
	withoutDrive := strings.TrimPrefix(remotePath, "/")
	if len(withoutDrive) >= 2 && withoutDrive[1] == ':' {
		withoutDrive = withoutDrive[2:]
	}
	return filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(withoutDrive, "/")))
}

func (h *windowsSFTPRealPathHandlers) Fileread(request *sftp.Request) (io.ReaderAt, error) {
	return os.Open(h.hostPath(request.Filepath))
}

func (h *windowsSFTPRealPathHandlers) Filewrite(request *sftp.Request) (io.WriterAt, error) {
	flags := os.O_WRONLY
	openFlags := request.Pflags()
	if openFlags.Read {
		flags = os.O_RDWR
	}
	if openFlags.Creat {
		flags |= os.O_CREATE
	}
	if openFlags.Trunc {
		flags |= os.O_TRUNC
	}
	if openFlags.Excl {
		flags |= os.O_EXCL
	}
	return os.OpenFile(h.hostPath(request.Filepath), flags, 0o666)
}

func (h *windowsSFTPRealPathHandlers) Filecmd(request *sftp.Request) error {
	switch request.Method {
	case "Mkdir":
		return os.Mkdir(h.hostPath(request.Filepath), 0o755)
	case "Setstat":
		return nil
	default:
		return os.ErrInvalid
	}
}

func (h *windowsSFTPRealPathHandlers) Filelist(request *sftp.Request) (sftp.ListerAt, error) {
	info, err := os.Stat(h.hostPath(request.Filepath))
	if err != nil {
		return nil, err
	}
	return &fileInfoLister{info: info}, nil
}

func (h *windowsSFTPRealPathHandlers) RealPath(remotePath string) (string, error) {
	return "/C:/Data/App.conf", nil
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
