package mcpserver_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientops "ssh-manager-mcp/internal/clientops"
	mcpserver "ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type secretDeliveryForwardFixture struct {
	store             *store.Store
	databasePath      string
	server            *httptest.Server
	runner            *mcpserver.ServeRunner
	forwarder         *clientops.SecretDeliveryForwarder
	outsiderForwarder *clientops.SecretDeliveryForwarder
	deviceCodeID      string
	deviceToken       string
	grantedID         string
	outsiderID        string
}

func newSecretDeliveryForwardFixture(t *testing.T) *secretDeliveryForwardFixture {
	t.Helper()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "authority.db")
	vault, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vault.Close() })
	if err := vault.AddSecret(
		"API_TOKEN", "接口令牌", []byte("secret-value-must-not-leak"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	grantedID, err := vault.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	outsiderID, err := vault.AddServer(&models.Server{
		Name: "storage-1", Host: "192.0.2.11", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := vault.AddProfile("agents")
	if err != nil {
		t.Fatal(err)
	}
	otherProfileID, err := vault.AddProfile("other-agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.GrantServers(profileID, []string{grantedID}); err != nil {
		t.Fatal(err)
	}
	if err := vault.GrantServers(otherProfileID, []string{outsiderID}); err != nil {
		t.Fatal(err)
	}
	if err := vault.AllowSecrets(
		[]string{"API_TOKEN"}, []string{"web-1"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := vault.AddSecret(
		"DB_PASSWORD", "数据库密码", []byte("database-secret-must-not-leak"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	deviceCodeID, deviceToken, err := vault.AddCacheToken("office-laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.AddDevice("office-device", "", "office-laptop"); err != nil {
		t.Fatal(err)
	}
	_, outsiderToken, err := vault.AddCacheToken("other-laptop", otherProfileID)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := mcpserver.NewServeRunner(vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	server := httptest.NewTLSServer(runner.HTTPHandler())
	t.Cleanup(server.Close)
	certificatePin := mcpserver.SPKIFingerprint(server.Certificate())
	forwarder, err := clientops.NewSecretDeliveryForwarder(clientops.CacheCred{
		URL: server.URL, Token: deviceToken, Pin: certificatePin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(forwarder.Close)
	outsiderForwarder, err := clientops.NewSecretDeliveryForwarder(clientops.CacheCred{
		URL: server.URL, Token: outsiderToken, Pin: certificatePin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outsiderForwarder.Close)
	return &secretDeliveryForwardFixture{
		store: vault, databasePath: databasePath, server: server, runner: runner,
		forwarder: forwarder, outsiderForwarder: outsiderForwarder,
		deviceCodeID: deviceCodeID, deviceToken: deviceToken,
		grantedID: grantedID, outsiderID: outsiderID,
	}
}

func secretDeliveryRecord(serverID string) store.SecretDeliveryRecordInput {
	return store.SecretDeliveryRecordInput{
		ServerID:               serverID,
		Path:                   "/etc/app/config.yml",
		Template:               []byte("token: {{sshmgr-secret:API_TOKEN}}"),
		FileSHA256:             strings.Repeat("5", 64),
		WrittenAt:              time.Unix(1760000200, 0),
		Secrets:                []store.SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
		InitiatingDeviceName:   "client-asserted-device",
		InitiatingInstanceName: "client-asserted-instance",
		AuthorizationProfileID: "client-asserted-profile",
	}
}

func TestSecretDeliveryForwarderRecordsThroughDeviceCode(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)
	cacheDirectory := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDirectory)
	certificatePin := mcpserver.SPKIFingerprint(fixture.server.Certificate())
	if err := clientops.WriteCacheCredFor("", &clientops.CacheCred{
		URL: fixture.server.URL, Token: fixture.deviceToken, Pin: certificatePin,
	}); err != nil {
		t.Fatal(err)
	}
	_, cacheBinaryPath, _, _, err := clientops.CachePathsFor("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheBinaryPath, []byte("ciphertext-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheEntriesBefore, err := os.ReadDir(cacheDirectory)
	if err != nil {
		t.Fatal(err)
	}
	credentialBefore, err := os.ReadFile(filepath.Join(cacheDirectory, "cache.auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	cacheBytesBefore, err := os.ReadFile(cacheBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	result, err := fixture.forwarder.RecordSecretDelivery(
		context.Background(), secretDeliveryRecord(fixture.grantedID),
	)
	if err != nil {
		t.Fatalf("登记成功不应返回错误：%v", err)
	}
	if result.Status != clientops.SecretDeliveryRecorded ||
		result.RefusalReason != "" || result.Replaced {
		t.Fatalf("登记结果 = %+v，应为未替换的成功登记", result)
	}

	records, err := fixture.store.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("权威端应保留一条下发记录，实际：%+v", records)
	}
	record := records[0]
	if record.ServerID != fixture.grantedID || record.Path != "/etc/app/config.yml" ||
		string(record.Template) != "token: {{sshmgr-secret:API_TOKEN}}" ||
		len(record.Secrets) != 1 || record.Secrets[0].Name != "API_TOKEN" {
		t.Fatalf("下发记录内容不匹配：%+v", record)
	}
	if record.InitiatingDeviceName != "office-device" ||
		record.InitiatingInstanceName != "office-laptop" {
		t.Fatalf("权威端必须使用设备码关联的设备与实例，实际：%+v", record)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		Actions: []string{"secret-delivery-record"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceForward ||
		!strings.Contains(rows[0].Command, `"device_code":"`+fixture.deviceCodeID+`"`) {
		t.Fatalf("登记审计应记录发起设备码，实际：%+v", rows)
	}
	revisionAfter, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("登记不应推动快照修订号：before=%d after=%d", revisionBefore, revisionAfter)
	}
	cacheEntriesAfter, err := os.ReadDir(cacheDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(cacheEntriesBefore) != len(cacheEntriesAfter) {
		t.Fatalf("登记成功不得在本地缓存创建新文件：before=%+v after=%+v", cacheEntriesBefore, cacheEntriesAfter)
	}
	for _, entry := range cacheEntriesAfter {
		if entry.Name() == "quarantine" {
			t.Fatal("登记成功不得触发缓存隔离")
		}
	}
	credentialAfter, err := os.ReadFile(filepath.Join(cacheDirectory, "cache.auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	cacheBytesAfter, err := os.ReadFile(cacheBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(credentialBefore, credentialAfter) || !bytes.Equal(cacheBytesBefore, cacheBytesAfter) {
		t.Fatal("登记成功不得修改本地缓存凭据或快照")
	}
}

func TestSecretDeliveryForwarderRefusesServerOutsideProfile(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)

	result, err := fixture.outsiderForwarder.RecordSecretDelivery(
		context.Background(), secretDeliveryRecord(fixture.grantedID),
	)
	if err == nil {
		t.Fatal("范围外服务器应返回错误")
	}
	if result.Status != clientops.SecretDeliveryRefused ||
		result.RefusalReason != store.SecretDeliveryRefusalServerNotInProfile {
		t.Fatalf("拒绝结果 = %+v，应说明服务器不在设备码绑定 profile 内", result)
	}
	if result.Message == "" {
		t.Fatal("授权拒绝必须带回可呈现的原因")
	}
	records, err := fixture.store.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("授权拒绝不应留下下发记录，实际：%+v", records)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		Actions: []string{"secret-delivery-refused"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" ||
		!strings.Contains(rows[0].Command, string(store.SecretDeliveryRefusalServerNotInProfile)) {
		t.Fatalf("授权拒绝应写权威端审计，实际：%+v", rows)
	}
}

func TestSecretDeliveryForwarderRefusesSecretOutsideWritableList(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)
	input := secretDeliveryRecord(fixture.grantedID)
	input.Secrets = []store.SecretDeliverySecret{{
		Name: "DB_PASSWORD", ValueVersion: 1,
	}}

	result, err := fixture.forwarder.RecordSecretDelivery(context.Background(), input)
	if err == nil {
		t.Fatal("可写入列表外的机密项应返回错误")
	}
	if result.Status != clientops.SecretDeliveryRefused ||
		result.RefusalReason != store.SecretDeliveryRefusalSecretNotWritable {
		t.Fatalf("拒绝结果 = %+v，应说明机密项不可写入目标服务器", result)
	}
	if !strings.Contains(result.Message, "DB_PASSWORD") {
		t.Fatalf("拒绝原因应包含被拒绝的机密项名字：%q", result.Message)
	}
	records, err := fixture.store.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("可写入列表外拒绝不应留下下发记录，实际：%+v", records)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		Actions: []string{"secret-delivery-refused"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Command, "DB_PASSWORD") ||
		strings.Contains(rows[0].Command, "database-secret-must-not-leak") {
		t.Fatalf("拒绝审计应记录名字且不包含值，实际：%+v", rows)
	}
}

func TestSecretDeliveryForwarderAuditFailureLeavesNoRecord(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)
	revisionBefore, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	rawDatabase, err := sql.Open("sqlite", fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDatabase.Close()
	if _, err := rawDatabase.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.forwarder.RecordSecretDelivery(
		context.Background(), secretDeliveryRecord(fixture.grantedID),
	)
	if err == nil {
		t.Fatal("审计失败时登记必须失败")
	}
	if result.Status != clientops.SecretDeliveryUnreachable ||
		!clientops.IsSecretDeliveryUnreachable(err) {
		t.Fatalf("权威端存储故障应按不可达分支返回，实际：%+v %v", result, err)
	}
	records, err := fixture.store.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("审计失败不应留下下发记录，实际：%+v", records)
	}
	revisionAfter, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("失败的登记不应推动快照修订号：before=%d after=%d", revisionBefore, revisionAfter)
	}
}

func TestSecretDeliveryForwarderReportsUnreachableAuthority(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)
	fixture.server.Close()

	result, err := fixture.forwarder.RecordSecretDelivery(
		context.Background(), secretDeliveryRecord(fixture.grantedID),
	)
	if err == nil {
		t.Fatal("权威端不可达应返回错误")
	}
	if result.Status != clientops.SecretDeliveryUnreachable ||
		!clientops.IsSecretDeliveryUnreachable(err) ||
		result.Message != clientops.SecretDeliveryUnreachableText {
		t.Fatalf("不可达结果 = %+v，错误 = %v", result, err)
	}
	records, err := fixture.store.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("不可达不应在权威端留下下发记录，实际：%+v", records)
	}
}

func TestSecretDeliveryForwarderRejectsTemplateOverLimit(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)
	input := secretDeliveryRecord(fixture.grantedID)
	input.Template = bytes.Repeat([]byte("x"), int(store.UploadContentDefaultLimitBytes)+1)

	result, err := fixture.forwarder.RecordSecretDelivery(context.Background(), input)
	if err == nil {
		t.Fatal("超过上传内容默认上限的模板应返回错误")
	}
	if result.Status != clientops.SecretDeliveryRefused ||
		!strings.Contains(result.Message, "exceeds the upload content limit") {
		t.Fatalf("超限结果 = %+v，错误 = %v", result, err)
	}
	records, err := fixture.store.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("超限模板不应留下下发记录，实际：%+v", records)
	}
}

func TestSecretDeliveryForwarderAcceptsTemplateAtLimit(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)
	input := secretDeliveryRecord(fixture.grantedID)
	input.Template = bytes.Repeat([]byte("x"), int(store.UploadContentDefaultLimitBytes))

	result, err := fixture.forwarder.RecordSecretDelivery(context.Background(), input)
	if err != nil {
		t.Fatalf("默认上限内的模板应能登记：%v", err)
	}
	if result.Status != clientops.SecretDeliveryRecorded {
		t.Fatalf("默认上限内的登记结果 = %+v", result)
	}
	records, err := fixture.store.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || len(records[0].Template) != int(store.UploadContentDefaultLimitBytes) {
		t.Fatalf("权威端应保留默认上限内的模板，实际：%d 条", len(records))
	}
}

func TestSecretDeliveryForwarderDeviceCodeFailureKeepsCache(t *testing.T) {
	fixture := newSecretDeliveryForwardFixture(t)
	cacheDirectory := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDirectory)
	certificatePin := mcpserver.SPKIFingerprint(fixture.server.Certificate())
	if err := clientops.WriteCacheCredFor("", &clientops.CacheCred{
		URL: fixture.server.URL, Token: "not-a-real-device-code", Pin: certificatePin,
	}); err != nil {
		t.Fatal(err)
	}
	_, cacheBinaryPath, _, _, err := clientops.CachePathsFor("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheBinaryPath, []byte("ciphertext-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(cacheDirectory, "cache.auth.json")
	credentialBefore, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	cacheBefore, err := os.ReadFile(cacheBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	forwarder, err := clientops.NewSecretDeliveryForwarder(clientops.CacheCred{
		URL: fixture.server.URL, Token: "not-a-real-device-code", Pin: certificatePin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()

	result, err := forwarder.RecordSecretDelivery(
		context.Background(), secretDeliveryRecord(fixture.grantedID),
	)
	if err == nil {
		t.Fatal("设备码认证失败应返回错误")
	}
	if result.Status != clientops.SecretDeliveryRefused ||
		result.RefusalReason != store.SecretDeliveryRefusalDeviceCodeNotBound ||
		clientops.IsSecretDeliveryUnreachable(err) {
		t.Fatalf("设备码认证失败结果 = %+v，错误 = %v", result, err)
	}
	credentialAfter, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	cacheAfter, err := os.ReadFile(cacheBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(credentialBefore, credentialAfter) || !bytes.Equal(cacheBefore, cacheAfter) {
		t.Fatal("设备码认证失败不得修改本地缓存凭据或快照")
	}
	entries, err := os.ReadDir(cacheDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "quarantine" {
			t.Fatal("设备码认证失败不得触发缓存隔离")
		}
	}
}
