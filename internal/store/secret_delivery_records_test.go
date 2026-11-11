package store

import (
	"bytes"
	"encoding/base64"
	"errors"
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
	var tooLarge *ErrSecretDeliveryTemplateTooLarge
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

func TestRecordSecretDeliveryRejectsTemplateContainingSecretValue(t *testing.T) {
	s, serverID, profileID := seedSecretDeliveryVault(t)
	if err := s.SetSecretValue("API_TOKEN", []byte("new-secret-value-must-not-leak"), CLISource()); err != nil {
		t.Fatal(err)
	}

	_, err := s.RecordSecretDelivery(SecretDeliveryRecordInput{
		ServerID: serverID, Path: "/etc/app/config.yml",
		Template:   []byte("token: secret-value-must-not-leak"),
		FileSHA256: strings.Repeat("c", 64), WrittenAt: time.Unix(1760000800, 0),
		Secrets: []SecretDeliverySecret{{
			Name: "API_TOKEN", ValueVersion: 1,
		}},
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
	}, CLISource())
	if !errors.Is(err, ErrSecretDeliveryTemplateContainsValue) {
		t.Fatalf("模板含机密项值应拒绝登记，实际：%v", err)
	}
	records, err := s.SecretDeliveryRecords(SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("含值模板不应落库，实际：%+v", records)
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
		oldTemplate := base64.StdEncoding.EncodeToString([]byte("first: {{sshmgr-secret:API_TOKEN}}"))
		if row.Action == "secret-delivery-replace" && strings.Contains(row.Command, oldTemplate) {
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
