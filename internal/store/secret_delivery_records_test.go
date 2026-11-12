package store

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func seedSecretDeliveryVault(t *testing.T) (*Store, string, string) {
	t.Helper()
	s := newTestStore(t)
	if err := s.AddSecret("API_TOKEN", "接口令牌", []byte("secret-value-must-not-leak"), CLISource()); err != nil {
		t.Fatal(err)
	}
	serverID, err := s.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := s.AddProfile("agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"API_TOKEN"}, []string{"web-1"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	return s, serverID, profileID
}

func TestRecordSecretDeliveryAuthorizationRefusalsAreDistinctAndAudited(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	otherProfileID, err := s.AddProfile("other-agents")
	if err != nil {
		t.Fatal(err)
	}
	notGrantedCodeID, _, err := s.AddCacheToken("other-laptop", otherProfileID)
	if err != nil {
		t.Fatal(err)
	}
	grantedCodeID, _, err := s.AddCacheToken("office-laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("DB_PASSWORD", "数据库密码", []byte("database-secret-must-not-leak"), CLISource()); err != nil {
		t.Fatal(err)
	}
	input := SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml",
		Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
		FileSHA256: strings.Repeat("5", 64), WrittenAt: time.Unix(1760000200, 0),
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
	}

	profileDenied := input
	profileDenied.AuthorizationProfileID = otherProfileID
	profileDenied.Secrets = []SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}}
	_, err = s.RecordSecretDelivery(profileDenied, ForwardedWriteSource(notGrantedCodeID))
	var refusal *SecretDeliveryRefusalError
	if !errors.As(err, &refusal) ||
		refusal.Reason != SecretDeliveryRefusalServerNotInProfile ||
		refusal.ServerName != "web-1" {
		t.Fatalf("profile 授权拒绝应带回可区分原因，实际：%v", err)
	}

	disallowed := input
	disallowed.AuthorizationProfileID = profileID
	disallowed.Secrets = []SecretDeliverySecret{{Name: "DB_PASSWORD", ValueVersion: 1}}
	_, err = s.RecordSecretDelivery(disallowed, ForwardedWriteSource(grantedCodeID))
	if !errors.As(err, &refusal) ||
		refusal.Reason != SecretDeliveryRefusalSecretNotWritable ||
		refusal.SecretName != "DB_PASSWORD" || refusal.ServerName != "web-1" {
		t.Fatalf("机密项可写入列表拒绝应带回可区分原因，实际：%v", err)
	}

	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("授权拒绝不应留下下发记录，实际：%+v", records)
	}
	rows, err := s.QueryAudit(AuditFilter{Actions: []string{"secret-delivery-refused"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Status != "error" || rows[1].Status != "error" ||
		rows[0].Source != AuditSourceForward || rows[1].Source != AuditSourceForward {
		t.Fatalf("两次授权拒绝应各写一行错误审计，实际：%+v", rows)
	}
	for _, row := range rows {
		if strings.Contains(row.Command, "secret-value-must-not-leak") ||
			strings.Contains(row.Command, "database-secret-must-not-leak") {
			t.Fatalf("拒绝审计泄露机密项值：%s", row.Command)
		}
	}
}

func TestRecordSecretDeliveryRejectsTemplateOverUploadContentLimit(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	template := make([]byte, UploadContentDefaultLimitBytes+1)
	copy(template, "token: {{sshmgr-secret:API_TOKEN}}")

	_, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml", Template: template,
		FileSHA256: strings.Repeat("6", 64), WrittenAt: time.Unix(1760000300, 0),
		Secrets: []SecretDeliverySecret{{
			Name: "API_TOKEN", ValueVersion: 1,
		}},
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
	}, CLISource())
	var tooLarge *SecretDeliveryTemplateTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != UploadContentDefaultLimitBytes ||
		tooLarge.Size != int64(len(template)) {
		t.Fatalf("超限模板应返回带大小的拒绝错误，实际：%v", err)
	}
	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("超限模板不应落库，实际：%+v", records)
	}
}

func TestRecordSecretDeliveryUsesCallerProvidedTemplateLimit(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	input := SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml",
		FileSHA256: strings.Repeat("d", 64), WrittenAt: time.Unix(1760000600, 0),
		Secrets: []SecretDeliverySecret{{
			Name: "API_TOKEN", ValueVersion: 1,
		}},
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
		TemplateLimitBytes:     UploadContentDefaultLimitBytes + 1024,
	}
	input.Template = bytes.Repeat([]byte("x"), int(UploadContentDefaultLimitBytes)+1)
	if _, err := s.RecordSecretDelivery(input, CLISource()); err != nil {
		t.Fatalf("调用方给定的更大上限内应允许登记：%v", err)
	}

	input.Template = bytes.Repeat([]byte("y"), int(input.TemplateLimitBytes)+1)
	_, err := s.RecordSecretDelivery(input, CLISource())
	var tooLarge *SecretDeliveryTemplateTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != input.TemplateLimitBytes ||
		tooLarge.Size != int64(len(input.Template)) {
		t.Fatalf("超过调用方给定上限应返回带大小与上限的错误，实际：%v", err)
	}
}

func TestRecordSecretDeliveryAllowsTemplateContainingSecretValueText(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	if err := s.SetSecretValue("API_TOKEN", []byte("new-secret-value-must-not-leak"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("PORT_TOKEN", "端口机密项", []byte("80808080"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"PORT_TOKEN"}, []string{"web-1"}, CLISource()); err != nil {
		t.Fatal(err)
	}

	_, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml",
		Template:   []byte("port: 80808080\ntoken: {{sshmgr-secret:API_TOKEN}}"),
		FileSHA256: strings.Repeat("c", 64), WrittenAt: time.Unix(1760000800, 0),
		Secrets: []SecretDeliverySecret{
			{Name: "API_TOKEN", ValueVersion: 2},
			{Name: "PORT_TOKEN", ValueVersion: 1},
		},
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
	}, CLISource())
	if err != nil {
		t.Fatalf("模板是否包含机密项值由调用方保证，登记口不应解密 vault：%v", err)
	}
	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || string(records[0].Template) != "port: 80808080\ntoken: {{sshmgr-secret:API_TOKEN}}" {
		t.Fatalf("模板文本包含机密项值也不应阻止登记，实际：%+v", records)
	}
}

func TestRecordSecretDeliveryUsesProfileBoundToForwardingDeviceCode(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	deviceCodeID, _, err := s.AddCacheToken("office-laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDevice("office-device", "", "office-laptop"); err != nil {
		t.Fatal(err)
	}

	result, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml",
		Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
		FileSHA256: strings.Repeat("7", 64), WrittenAt: time.Unix(1760000400, 0),
		Secrets: []SecretDeliverySecret{{
			Name: "API_TOKEN", ValueVersion: 1,
		}},
		InitiatingDeviceName: "client-asserted-device", InitiatingInstanceName: "client-asserted-instance",
	}, ForwardedWriteSource(deviceCodeID))
	if err != nil {
		t.Fatal(err)
	}
	if result.Replaced {
		t.Fatal("首次转发登记不应替换旧记录")
	}
	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 ||
		records[0].InitiatingDeviceName != "office-device" ||
		records[0].InitiatingInstanceName != "office-laptop" {
		t.Fatalf("转发登记应使用设备码关联的设备与实例，实际：%+v", records)
	}
	rows, err := s.QueryAudit(AuditFilter{Actions: []string{"secret-delivery-record"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != AuditSourceForward ||
		!strings.Contains(rows[0].Command, `"device_code":"`+deviceCodeID+`"`) {
		t.Fatalf("转发登记审计应记录设备码编号，实际：%+v", rows)
	}
}

func TestRecordSecretDeliveryRequiresAbsolutePath(t *testing.T) {
	s, linuxID, profileID := seedSecretDeliveryVault(t)
	windowsID, err := s.AddServer(&models.Server{
		Name: "windows-1", Host: "192.0.2.11", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profileID, []string{windowsID}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"API_TOKEN"}, []string{"windows-1"}, CLISource()); err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name     string
		serverID string
		path     string
	}{
		{name: "Linux 相对路径", serverID: linuxID, path: "app/config.yml"},
		{name: "Windows 无根路径", serverID: windowsID, path: `C:app\config.yml`},
		{name: "Windows 相对路径", serverID: windowsID, path: `app\config.yml`},
	} {
		_, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
			ServerID: testCase.serverID, Path: testCase.path,
			Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
			FileSHA256: strings.Repeat("8", 64), WrittenAt: time.Unix(1760000500, 0),
			Secrets: []SecretDeliverySecret{{
				Name: "API_TOKEN", ValueVersion: 1,
			}},
			InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
			AuthorizationProfileID: profileID,
		}, CLISource())
		if !errors.Is(err, ErrSecretDeliveryInvalidPath) {
			t.Fatalf("%s：应拒绝非绝对路径，实际：%v", testCase.name, err)
		}
	}
	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("非绝对路径不应落库，实际：%+v", records)
	}
}

func TestRecordSecretDeliveryAuditFailureRollsBackAndDoesNotBumpRevision(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	before, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	input := SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml",
		Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
		FileSHA256: strings.Repeat("9", 64), WrittenAt: time.Unix(1760000600, 0),
		Secrets: []SecretDeliverySecret{{
			Name: "API_TOKEN", ValueVersion: 1,
		}},
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
	}
	if _, err := s.RecordSecretDelivery(input, CLISource()); err != nil {
		t.Fatal(err)
	}
	afterSuccess, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterSuccess != before {
		t.Fatalf("登记不应推动快照修订号：before=%d after=%d", before, afterSuccess)
	}

	if _, err := s.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	replacement := input
	replacement.Template = []byte("replacement: {{sshmgr-secret:API_TOKEN}}")
	replacement.FileSHA256 = strings.Repeat("a", 64)
	if _, err := s.RecordSecretDelivery(replacement, CLISource()); err == nil {
		t.Fatal("审计写失败时登记必须失败")
	}
	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || string(records[0].Template) != "token: {{sshmgr-secret:API_TOKEN}}" {
		t.Fatalf("审计失败时替换必须整体回滚，实际：%+v", records)
	}
	afterFailure, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure != before {
		t.Fatalf("失败的登记不应推动快照修订号：before=%d after=%d", before, afterFailure)
	}
}

func TestSecretDeliveryRecordsFilterBySecretAndServer(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	if err := s.AddSecret("DB_PASSWORD", "数据库密码", []byte("database-secret-must-not-leak"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"DB_PASSWORD"}, []string{"web-1"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	record := func(path string, secrets ...SecretDeliverySecret) {
		t.Helper()
		_, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
			ServerID: serverID, Path: path,
			Template:   []byte("configuration template"),
			FileSHA256: strings.Repeat("b", 64), WrittenAt: time.Unix(1760000700, 0),
			Secrets: secrets, InitiatingDeviceName: "office-laptop",
			InitiatingInstanceName: "agent-a", AuthorizationProfileID: profileID,
		}, CLISource())
		if err != nil {
			t.Fatal(err)
		}
	}
	record("/etc/api.yml", SecretDeliverySecret{Name: "API_TOKEN", ValueVersion: 1})
	record("/etc/db.yml", SecretDeliverySecret{Name: "DB_PASSWORD", ValueVersion: 1})
	record("/etc/both.yml",
		SecretDeliverySecret{Name: "API_TOKEN", ValueVersion: 1},
		SecretDeliverySecret{Name: "DB_PASSWORD", ValueVersion: 1},
	)

	bySecret, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{SecretName: "DB_PASSWORD"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySecret) != 2 || bySecret[0].Path != "/etc/both.yml" || bySecret[1].Path != "/etc/db.yml" {
		t.Fatalf("按机密项查询应返回引用它的记录，实际：%+v", bySecret)
	}
	bothFilters, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{
		SecretName: "API_TOKEN", ServerID: serverID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bothFilters) != 2 || bothFilters[0].Path != "/etc/api.yml" || bothFilters[1].Path != "/etc/both.yml" {
		t.Fatalf("机密项与服务器筛选应同时生效，实际：%+v", bothFilters)
	}
	for _, record := range append(bySecret, bothFilters...) {
		if bytes.Contains(record.Template, []byte("database-secret-must-not-leak")) ||
			bytes.Contains(record.Template, []byte("secret-value-must-not-leak")) {
			t.Fatalf("查询结果泄露机密项值：%s", record.Template)
		}
	}
}

func TestRecordSecretDeliveryReplacesSamePathAndNormalizesPathKey(t *testing.T) {
	s, linuxID, profileID := seedSecretDeliveryVault(t)
	windowsID, err := s.AddServer(&models.Server{
		Name: "windows-1", Host: "192.0.2.11", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profileID, []string{windowsID}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"API_TOKEN"}, []string{"windows-1"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	input := func(serverID, path, template, digest string) SecretDeliveryRecordInput {
		return SecretDeliveryRecordInput{
			ServerID: serverID, Path: path, Template: []byte(template),
			FileSHA256: digest, WrittenAt: time.Unix(1760000100, 0),
			Secrets: []SecretDeliverySecret{{
				Name: "API_TOKEN", ValueVersion: 1,
			}},
			InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
			AuthorizationProfileID: profileID,
		}
	}

	first, err := s.RecordSecretDelivery(input(
		windowsID, `C:\App\Config\settings.yml`, "first: {{sshmgr-secret:API_TOKEN}}", strings.Repeat("1", 64),
	), CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if first.Replaced {
		t.Fatal("Windows 路径首次登记不应替换")
	}
	if err := s.SetSecretValue("API_TOKEN", []byte("new-secret-value-must-not-leak"), CLISource()); err != nil {
		t.Fatal(err)
	}
	replacement := input(
		windowsID, `c:/APP\config\SETTINGS.yml`, "second: {{sshmgr-secret:API_TOKEN}}", strings.Repeat("2", 64),
	)
	replacement.Secrets[0].ValueVersion = 2
	replacement.InitiatingDeviceName = "field-laptop"
	second, err := s.RecordSecretDelivery(replacement, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replaced {
		t.Fatal("Windows 同一路径大小写与分隔符不同时应替换旧记录")
	}
	if second.Record.Path != `c:/APP/config/SETTINGS.yml` {
		t.Fatalf("Windows 路径应统一为正斜杠，实际：%q", second.Record.Path)
	}

	if _, err := s.RecordSecretDelivery(input(linuxID, "/Opt/app.yml", "old", strings.Repeat("3", 64)), CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSecretDelivery(input(linuxID, "/opt/app.yml", "new", strings.Repeat("4", 64)), CLISource()); err != nil {
		t.Fatal(err)
	}
	byServer, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{ServerID: linuxID})
	if err != nil {
		t.Fatal(err)
	}
	if len(byServer) != 2 {
		t.Fatalf("Linux 路径应按大小写保留两条记录，实际：%+v", byServer)
	}

	windowsRecords, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{ServerID: windowsID})
	if err != nil {
		t.Fatal(err)
	}
	if len(windowsRecords) != 1 || string(windowsRecords[0].Template) != "second: {{sshmgr-secret:API_TOKEN}}" {
		t.Fatalf("Windows 同一路径应只保留最新记录，实际：%+v", windowsRecords)
	}
	auditRows, err := s.QueryAudit(AuditFilter{Actions: []string{"secret-delivery-record", "secret-delivery-replace"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 4 {
		t.Fatalf("四次登记应各有一行审计，其中 Windows 第二次为替换，实际：%d 行", len(auditRows))
	}
	var sawReplacementHistory bool
	for _, row := range auditRows {
		if strings.Contains(row.Command, "secret-value-must-not-leak") ||
			strings.Contains(row.Command, "new-secret-value-must-not-leak") {
			t.Fatalf("审计泄露机密项值：%s", row.Command)
		}
		if row.Action == "secret-delivery-replace" &&
			strings.Contains(row.Command, `"previous_template":"first: {{sshmgr-secret:API_TOKEN}}"`) {
			sawReplacementHistory = true
		}
		if row.Action == "secret-delivery-replace" &&
			!strings.Contains(row.Command, `"previous_secrets":"API_TOKEN@1"`) {
			t.Fatalf("替换审计应保留旧机密项名字与值版本号，实际：%s", row.Command)
		}
	}
	if !sawReplacementHistory {
		t.Fatalf("替换审计应保留旧模板原文，实际：%+v", auditRows)
	}
}

func TestRecordSecretDeliveryWindowsSFTPRealPathUsesOneRecord(t *testing.T) {
	s, _, profileID := seedSecretDeliveryVault(t)
	windowsID, err := s.AddServer(&models.Server{
		Name: "windows-realpath", Host: "192.0.2.12", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profileID, []string{windowsID}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"API_TOKEN"}, []string{"windows-realpath"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	paths := []string{"/C:/Data/App.conf", `C:\Data\App.conf`, "c:/data/app.conf"}
	for index, path := range paths {
		result, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
			ServerID: windowsID, Path: path,
			Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
			FileSHA256: strings.Repeat("1", 64),
			WrittenAt:  time.Unix(int64(1760000100+index), 0),
			Secrets: []SecretDeliverySecret{{
				Name: "API_TOKEN", ValueVersion: 1,
			}},
			InitiatingDeviceName:   "office-laptop",
			InitiatingInstanceName: "agent-a",
			AuthorizationProfileID: profileID,
		}, CLISource())
		if err != nil {
			t.Fatalf("登记 Windows 路径 %q 失败：%v", path, err)
		}
		if index > 0 && !result.Replaced {
			t.Fatalf("Windows 路径 %q 应替换同一路径的旧记录", path)
		}
	}
	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{ServerID: windowsID})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Path != "c:/data/app.conf" ||
		string(records[0].Template) != "token: {{sshmgr-secret:API_TOKEN}}" {
		t.Fatalf("Windows SFTP realpath 三种写法应保留同一条最新记录，实际：%+v", records)
	}
}

func TestRecordSecretDeliveryStoresQueryableRecord(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	writtenAt := time.Unix(1760000000, 0)

	result, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
		ServerID:               serverID,
		Path:                   "/etc/app/config.yml",
		Template:               []byte("token: {{sshmgr-secret:API_TOKEN}}\n"),
		FileSHA256:             "333f368d8870a15a55d5562583d020ebd8b6b0e1b10398c6ca4c60a6c78c3724",
		Secrets:                []SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
		WrittenAt:              writtenAt,
		InitiatingDeviceName:   "office-laptop",
		InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if result.Replaced {
		t.Fatal("首次登记不应报告替换")
	}

	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("应能查询到一条下发记录，实际：%+v", records)
	}
	got := records[0]
	if got.ServerID != serverID || got.ServerName != "web-1" ||
		got.Path != "/etc/app/config.yml" ||
		string(got.Template) != "token: {{sshmgr-secret:API_TOKEN}}\n" ||
		got.FileSHA256 != "333f368d8870a15a55d5562583d020ebd8b6b0e1b10398c6ca4c60a6c78c3724" ||
		!got.WrittenAt.Equal(writtenAt) ||
		got.InitiatingDeviceName != "office-laptop" || got.InitiatingInstanceName != "agent-a" {
		t.Fatalf("下发记录字段不正确：%+v", got)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "API_TOKEN" ||
		got.Secrets[0].ValueVersion != 1 || got.Secrets[0].Deleted {
		t.Fatalf("机密项引用不正确：%+v", got.Secrets)
	}
}

// TestSecretDeliveryRecordsSurviveSnapshotExportImport 覆盖 owner 全量导出与
// 加密备份共用的快照路径：下发记录逐字段恢复到新 vault，机密项已删除的标
// 注也保持不变；profile 裁剪的客户端快照不携带记录。
func TestSecretDeliveryRecordsSurviveSnapshotExportImport(t *testing.T) {
	a, serverID, profileID := seedSecretDeliveryVault(t)
	if err := a.AddSecret("DB_PASSWORD", "数据库密码", []byte("database-secret-must-not-leak"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := a.AllowSecrets([]string{"API_TOKEN", "DB_PASSWORD"}, []string{"web-1"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	input := func(path string, names []SecretDeliverySecret, digest string) SecretDeliveryRecordInput {
		return SecretDeliveryRecordInput{
			ServerID: serverID, Path: path,
			Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
			FileSHA256: digest, WrittenAt: time.Unix(1760000400, 0),
			Secrets: names, InitiatingDeviceName: "office-laptop",
			InitiatingInstanceName: "agent-a", AuthorizationProfileID: profileID,
		}
	}
	if _, err := a.RecordSecretDelivery(input(
		"/etc/app/token.yml",
		[]SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
		strings.Repeat("7", 64),
	), CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecordSecretDelivery(input(
		"/etc/app/database.yml",
		[]SecretDeliverySecret{{Name: "DB_PASSWORD", ValueVersion: 1}},
		strings.Repeat("8", 64),
	), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteSecret("API_TOKEN", CLISource()); err != nil {
		t.Fatal(err)
	}
	want, err := a.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 || want[0].Path != "/etc/app/database.yml" || want[0].Secrets[0].Deleted ||
		want[1].Path != "/etc/app/token.yml" || !want[1].Secrets[0].Deleted {
		t.Fatalf("删除 API_TOKEN 后两条记录应分别带删除标注与不带标注：%+v", want)
	}

	snap, err := a.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.SecretDeliveryRecords) != 2 {
		t.Fatalf("全量导出应携带全部下发记录，实际：%+v", snap.SecretDeliveryRecords)
	}
	profileSnap, err := a.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(profileSnap.SecretDeliveryRecords) != 0 {
		t.Fatalf("profile 裁剪快照不应携带下发记录，实际：%+v", profileSnap.SecretDeliveryRecords)
	}

	b := newTestStore(t)
	if err := b.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	got, err := b.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("导入新 vault 后下发记录内容变化：\nwant %+v\ngot  %+v", want, got)
	}
}

// TestServerDeletePreviewListsAndCascadesDeliveryRecords 覆盖服务器条目删除
// 的三件事：预览列出将删除的下发记录；预览摘要覆盖记录，确认后新增记录要
// 求重新确认；确认删除后记录随条目删除并写不含机密项值的审计行。
func TestServerDeletePreviewListsAndCascadesDeliveryRecords(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	writtenAt := time.Unix(1760000500, 0)
	recordInput := func(path string) SecretDeliveryRecordInput {
		return SecretDeliveryRecordInput{
			ServerID: serverID, Path: path,
			Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
			FileSHA256: strings.Repeat("9", 64), WrittenAt: writtenAt,
			Secrets:              []SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
			InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
			AuthorizationProfileID: profileID,
		}
	}
	revisionBeforeRecord, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSecretDelivery(recordInput("/etc/app/config.yml"), CLISource()); err != nil {
		t.Fatal(err)
	}
	revisionAfterRecord, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfterRecord != revisionBeforeRecord {
		t.Fatalf("登记下发记录不应推动快照修订号：%d -> %d", revisionBeforeRecord, revisionAfterRecord)
	}

	preview, err := s.ServerDeletePreview(serverID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.DeliveryRecords) != 1 {
		t.Fatalf("删除预览应列出下发记录：%+v", preview.DeliveryRecords)
	}
	got := preview.DeliveryRecords[0]
	if got.ServerName != "web-1" || got.Path != "/etc/app/config.yml" ||
		len(got.SecretNames) != 1 || got.SecretNames[0] != "API_TOKEN" ||
		!got.WrittenAt.Equal(writtenAt) {
		t.Fatalf("预览里的下发记录字段不正确：%+v", got)
	}

	// 确认与执行之间新增另一条下发记录：旧摘要必须被拒绝。
	if _, err := s.RecordSecretDelivery(recordInput("/etc/app/other.yml"), CLISource()); err != nil {
		t.Fatal(err)
	}
	_, err = s.DeleteServerCascadingConfirmed(serverID, preview.Digest())
	if !errors.Is(err, ErrServerDeletePreviewChanged) {
		t.Fatalf("预览变化后应整体中止，实际：%v", err)
	}
	if records, _ := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{ServerID: serverID}); len(records) != 2 {
		t.Fatalf("中止删除时不得删除下发记录：%+v", records)
	}

	fresh, err := s.ServerDeletePreview(serverID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteServerCascadingConfirmed(serverID, fresh.Digest()); err != nil {
		t.Fatal(err)
	}
	if records, _ := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{}); len(records) != 0 {
		t.Fatalf("确认删除后下发记录应随服务器条目删除：%+v", records)
	}
	auditRows, err := s.QueryAudit(AuditFilter{Actions: []string{"secret-delivery-rm"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 2 {
		t.Fatalf("两条下发记录的删除应各写一行审计，实际 %d 行：%+v", len(auditRows), auditRows)
	}
	for _, row := range auditRows {
		if !strings.Contains(row.Command, "/etc/app/") || !strings.Contains(row.Command, "API_TOKEN") {
			t.Fatalf("下发记录删除审计应含路径与机密项名字：%s", row.Command)
		}
		if strings.Contains(row.Command, "secret-value-must-not-leak") {
			t.Fatalf("下发记录删除审计不得出现机密项值：%s", row.Command)
		}
	}
}

// TestDeliveryRecordImpactsDoNotAdvanceRevision 把「下发记录本身的删
// 除不推动快照修订号」钉在存储层：结构完全相同、唯一差别是是否登记过下发
// 记录的两个库，删除服务器条目后的修订号增量必须相同。
func TestDeliveryRecordImpactsDoNotAdvanceRevision(t *testing.T) {
	withRecord, serverID, profileID := seedSecretDeliveryVault(t)
	record := SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml",
		Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
		FileSHA256: strings.Repeat("2", 64), WrittenAt: time.Unix(1760000600, 0),
		Secrets:              []SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
	}
	if _, err := withRecord.RecordSecretDelivery(record, CLISource()); err != nil {
		t.Fatal(err)
	}
	withoutRecord, bareID, _ := seedSecretDeliveryVault(t)

	withBefore, err := withRecord.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	withoutBefore, err := withoutRecord.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	for store, id := range map[*Store]string{withRecord: serverID, withoutRecord: bareID} {
		preview, err := store.ServerDeletePreview(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.DeleteServerCascadingConfirmed(id, preview.Digest()); err != nil {
			t.Fatal(err)
		}
	}
	withAfter, err := withRecord.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	withoutAfter, err := withoutRecord.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if withAfter-withBefore != withoutAfter-withoutBefore {
		t.Fatalf("下发记录的删除不应额外推动快照修订号：有记录库 %d -> %d，无记录库 %d -> %d",
			withBefore, withAfter, withoutBefore, withoutAfter)
	}
}

// TestDeviceDeletePreviewCoversAllServerDeliveryRecords 设备删除预览必须列
// 出该设备全部服务器条目的下发记录；确认后记录随条目删除并写审计，其他
// 设备的记录不受影响。
func TestDeviceDeletePreviewCoversAllServerDeliveryRecords(t *testing.T) {
	s, _, profileID := seedSecretDeliveryVault(t)
	if _, err := s.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDevice("别的设备", "", ""); err != nil {
		t.Fatal(err)
	}
	addServerToDevice := func(name, device string) string {
		t.Helper()
		added, err := s.AddServerWithCredentialsToDevice(&models.Server{
			Name: name, Host: "192.0.2.20", Port: 22, User: "deploy",
		}, nil, nil, device)
		if err != nil {
			t.Fatal(err)
		}
		return added.ServerID
	}
	workA := addServerToDevice("work-a", "工作站")
	workB := addServerToDevice("work-b", "工作站")
	other := addServerToDevice("other-host", "别的设备")
	if err := s.GrantServers(profileID, []string{workA, workB, other}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"API_TOKEN"}, []string{"work-a", "work-b", "other-host"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	record := func(serverID, path string) SecretDeliveryRecordInput {
		return SecretDeliveryRecordInput{
			ServerID: serverID, Path: path,
			Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
			FileSHA256: strings.Repeat("3", 64), WrittenAt: time.Unix(1760000700, 0),
			Secrets:              []SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
			InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
			AuthorizationProfileID: profileID,
		}
	}
	for _, input := range []SecretDeliveryRecordInput{
		record(workA, "/etc/app/a.yml"), record(workB, "/etc/app/b.yml"), record(other, "/etc/app/other.yml"),
	} {
		if _, err := s.RecordSecretDelivery(input, CLISource()); err != nil {
			t.Fatal(err)
		}
	}

	preview, err := s.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.DeliveryRecords) != 2 {
		t.Fatalf("设备删除预览应列出该设备全部服务器条目的下发记录：%+v", preview.DeliveryRecords)
	}
	if preview.DeliveryRecords[0].ServerName != "work-a" || preview.DeliveryRecords[0].Path != "/etc/app/a.yml" ||
		preview.DeliveryRecords[1].ServerName != "work-b" || preview.DeliveryRecords[1].Path != "/etc/app/b.yml" {
		t.Fatalf("设备删除预览的下发记录不正确：%+v", preview.DeliveryRecords)
	}

	// 确认后又给该设备的服务器登记记录：旧预览必须整体中止。
	if _, err := s.RecordSecretDelivery(record(workA, "/etc/app/late.yml"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDeviceCascading("工作站", preview); !errors.Is(err, ErrDeviceDeletePreviewChanged) {
		t.Fatalf("设备删除预览变化后应整体中止，实际：%v", err)
	}

	fresh, err := s.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDeviceCascading("工作站", fresh); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ServerName != "other-host" {
		t.Fatalf("设备删除后只应保留其他设备的下发记录：%+v", remaining)
	}
	for _, record := range remaining {
		if record.ServerName == "work-a" || record.ServerName == "work-b" {
			t.Fatalf("设备的服务器条目记录应随设备删除：%+v", record)
		}
	}
	auditRows, err := s.QueryAudit(AuditFilter{Actions: []string{"secret-delivery-rm"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 3 {
		t.Fatalf("随设备删除的三条下发记录应各写一行审计，实际 %d 行", len(auditRows))
	}
}
