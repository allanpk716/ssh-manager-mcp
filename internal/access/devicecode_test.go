package access

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// fakeFingerprintSource 注入固定的权威端证书公钥指纹来源，签发路径因此可
// 以在测试里不触碰真实证书文件。
func fakeFingerprintSource(t *testing.T, m *Manager, fingerprint string, loadErr error) {
	t.Helper()
	m.serveCertFingerprint = func() (string, error) { return fingerprint, loadErr }
}

// TestAddDeviceCodeIssuesWorkingCodeAndWritesAudit 覆盖设备码签发与撤销的
// 核心行为：签出的设备码能通过存储的认证校验（等价于下一次拉取被接受），
// 撤销后同一设备码认证失败；两次写操作各留一条摘要正确的审计行，摘要里
// 绝不出现设备码本体。
func TestAddDeviceCodeIssuesWorkingCodeAndWritesAudit(t *testing.T) {
	m, st := newTestManager(t)
	fakeFingerprintSource(t, m, "sha256:device-test-fingerprint", nil)
	profileID, err := st.AddProfile("ops")
	if err != nil {
		t.Fatal(err)
	}

	result, err := m.AddDeviceCode("laptop", "ops", "https://serve.example:7878", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "laptop" || result.Code == "" || result.Fingerprint != "sha256:device-test-fingerprint" {
		t.Fatalf("签发结果 = %+v，应带设备名、一次性设备码与证书指纹", result)
	}
	wantCommand := "sshmgr cache pull --url https://serve.example:7878 --token '" +
		result.Code + ":sha256:device-test-fingerprint'"
	if result.PullCommand != wantCommand {
		t.Fatalf("拉取命令 = %q，应为 %q", result.PullCommand, wantCommand)
	}

	verified, err := st.VerifyCacheToken(result.Code)
	if err != nil {
		t.Fatal(err)
	}
	if verified == nil || verified.Name != "laptop" || verified.ProfileID != profileID {
		t.Fatalf("新签发的设备码应能通过认证，实际 %+v", verified)
	}

	addRows := ownerAuditRows(t, st, "cache-token.add")
	if len(addRows) != 1 {
		t.Fatalf("cache-token.add 审计行数量 = %d，应为 1：%+v", len(addRows), addRows)
	}
	assertAuditShape(t, addRows[0], "cache-token.add", "ok", `{"name":"laptop","profile":"ops"}`, store.AuditSourceCLI, "")
	if strings.Contains(addRows[0].Command, result.Code) {
		t.Fatalf("审计摘要不应包含设备码：%s", addRows[0].Command)
	}

	if err := m.RevokeDeviceCode("laptop", WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	rejected, err := st.VerifyCacheToken(result.Code)
	if err != nil {
		t.Fatal(err)
	}
	if rejected != nil {
		t.Fatalf("撤销后的设备码应认证失败，实际 %+v", rejected)
	}

	revokeRows := ownerAuditRows(t, st, "cache-token.revoke")
	if len(revokeRows) != 1 {
		t.Fatalf("cache-token.revoke 审计行数量 = %d，应为 1：%+v", len(revokeRows), revokeRows)
	}
	assertAuditShape(t, revokeRows[0], "cache-token.revoke", "ok", `{"name":"laptop"}`, store.AuditSourceWebUI, "192.0.2.7")
}

// TestAddDeviceCodeFailuresLeaveNoCodeAndWriteErrorAudit 钉住失败路径：profile
// 不存在、证书指纹读取失败、模块没注入指纹来源时不产生设备码，且各留一条
// 失败审计行；撤销不存在的设备码同样留失败行。
func TestAddDeviceCodeFailuresLeaveNoCodeAndWriteErrorAudit(t *testing.T) {
	m, st := newTestManager(t)
	if _, err := st.AddProfile("ops"); err != nil {
		t.Fatal(err)
	}

	if _, err := m.AddDeviceCode("laptop", "ghost", "https://serve.example:7878", CLISource()); err == nil {
		t.Fatal("profile 不存在时应报错")
	}
	fakeFingerprintSource(t, m, "", errors.New("cert unreadable"))
	if _, err := m.AddDeviceCode("laptop", "ops", "https://serve.example:7878", CLISource()); err == nil {
		t.Fatal("证书指纹读取失败时应报错")
	}
	if _, err := m.AddDeviceCode("desk", "ops", "https://serve.example:7878", CLISource()); err == nil {
		t.Fatal("未注入指纹来源时应报错")
	}

	// 注入失败来源的模块已覆盖前一种情形；这里显式换成健康来源重试同名
	// 设备，证明前面的失败没有留下半成品设备码。
	fakeFingerprintSource(t, m, "sha256:device-test-fingerprint", nil)
	if _, err := m.AddDeviceCode("laptop", "ops", "https://serve.example:7878", CLISource()); err != nil {
		t.Fatalf("失败不应留下设备码，重签应成功：%v", err)
	}

	if err := m.RevokeDeviceCode("nope", CLISource()); err == nil {
		t.Fatal("撤销不存在的设备码应报错")
	}

	addRows := ownerAuditRows(t, st, "cache-token.add")
	if len(addRows) != 4 {
		t.Fatalf("cache-token.add 审计行数量 = %d，应为 4：%+v", len(addRows), addRows)
	}
	assertAuditShape(t, addRows[0], "cache-token.add", "ok", `{"name":"laptop","profile":"ops"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, addRows[1], "cache-token.add", "error", `{"name":"desk","profile":"ops"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, addRows[2], "cache-token.add", "error", `{"name":"laptop","profile":"ops"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, addRows[3], "cache-token.add", "error", `{"name":"laptop","profile":"ghost"}`, store.AuditSourceCLI, "")
	revokeRows := ownerAuditRows(t, st, "cache-token.revoke")
	if len(revokeRows) != 1 {
		t.Fatalf("cache-token.revoke 审计行数量 = %d，应为 1：%+v", len(revokeRows), revokeRows)
	}
	assertAuditShape(t, revokeRows[0], "cache-token.revoke", "error", `{"name":"nope"}`, store.AuditSourceCLI, "")

	tokens, err := st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].Name != "laptop" || tokens[0].Status != models.CacheTokenActive {
		t.Fatalf("失败路径不应留下设备码，最终列表 = %+v", tokens)
	}
}

// TestListDeviceCodesIncludesRevokedAndClientVersion 钉住设备码列表形状：
// 已撤销的仍列出，最近拉取时间与最近客户端版本随行返回，profile 名字已
// 解析（未绑定的旧设备码保留空标识）。
func TestListDeviceCodesIncludesRevokedAndClientVersion(t *testing.T) {
	m, st := newTestManager(t)
	fakeFingerprintSource(t, m, "sha256:device-test-fingerprint", nil)
	opsID, err := st.AddProfile("ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddProfile("dev"); err != nil {
		t.Fatal(err)
	}
	laptopID, laptopCode, err := st.AddCacheToken("laptop", opsID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("desk", opsID); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchCacheToken(laptopID, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeCacheToken("desk"); err != nil {
		t.Fatal(err)
	}

	rows, err := m.ListDeviceCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("设备码列表数量 = %d，应为 2：%+v", len(rows), rows)
	}
	desk, laptop := rows[0], rows[1]
	if laptop.Name != "laptop" || desk.Name != "desk" {
		t.Fatalf("设备码列表应按名字排序：%+v", rows)
	}
	if laptop.ID != laptopID || laptop.TokenPrefix == "" || laptop.TokenPrefix == laptopCode {
		t.Fatalf("laptop 行标识与令牌前缀不正确：%+v", laptop)
	}
	if laptop.Status != models.CacheTokenActive || laptop.ProfileName != "ops" || laptop.ProfileID != opsID {
		t.Fatalf("laptop 行状态或 profile 不正确：%+v", laptop)
	}
	if laptop.ClientVersion != "v1.2.3" || laptop.LastPullAt.IsZero() {
		t.Fatalf("laptop 行应带最近拉取时间与客户端版本：%+v", laptop)
	}
	if desk.Status != models.CacheTokenRevoked || desk.ProfileName != "ops" {
		t.Fatalf("desk 行应显示已撤销与 profile：%+v", desk)
	}
	if desk.ClientVersion != "" || !desk.LastPullAt.IsZero() {
		t.Fatalf("desk 行从未拉取，不应有版本与时间：%+v", desk)
	}
}
