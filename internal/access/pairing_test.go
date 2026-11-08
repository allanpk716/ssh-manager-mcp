package access

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// seedPairingRequest 直接写入一条待批准配对请求，供访问管理模块的公共
// 配对接口测试使用；目标地址使用环回地址，机械地址检查稳定判定为异常。
func seedPairingRequest(t *testing.T, st *store.Store, now time.Time, name string) string {
	t.Helper()
	return seedPairingRequestState(t, st, now, name, "pending", time.Time{})
}

func seedPairingRequestState(t *testing.T, st *store.Store, now time.Time, name, state string, approvedDeadline time.Time) string {
	t.Helper()
	id := sha256.Sum256([]byte(name))
	row := &store.PendingPairing{
		ID: id[:], Name: name, TargetURL: "https://127.0.0.1:7878", SAS: "314159",
		ProfileHint: "ops", ReplaceInactive: true, SourceIP: "192.0.2.9",
		ClientPub: make([]byte, 32), Cnonce: make([]byte, 16),
		ServerPub: make([]byte, 32), Snonce: make([]byte, 16), Sig: make([]byte, 64),
		State:            state,
		EnrollDeadline:   now.Add(10 * time.Minute).Unix(),
		ApprovedDeadline: approvedDeadline.Unix(),
	}
	if err := st.AddPendingPairing(row, 0, 0); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(id[:])
}

// ownerAuditRowsWithStatus 读回指定动作与状态的 owner 审计行（最新在前）。
func ownerAuditRowsWithStatus(t *testing.T, st *store.Store, status string, actions ...string) []store.AuditRow {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: actions, Statuses: []string{status}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPairingApprovalChecksComparisonAndAddressThenChangesState(t *testing.T) {
	now := time.Unix(1800000000, 0)
	m, st := newTestManager(t)
	st.NowFn = func() time.Time { return now }
	profileID, err := st.AddProfile("ops")
	if err != nil {
		t.Fatal(err)
	}
	approveID := seedPairingRequest(t, st, now, "laptop")

	rows, err := m.ListPairingRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("配对请求数量 = %d，应为 1：%+v", len(rows), rows)
	}
	row := rows[0]
	if row.ID != approveID || row.Name != "laptop" || row.TargetURL != "https://127.0.0.1:7878" ||
		row.ShortCode != "314159" || row.SourceAddress != "192.0.2.9" || row.ProfileHint != "ops" ||
		!row.ReplaceInactive || !row.ForeignTarget || row.State != "pending" || row.RemainingSeconds != 600 {
		t.Fatalf("配对请求列表形状不正确：%+v", row)
	}

	var compareErr *PairingComparisonRequiredError
	err = m.ApprovePairing(approveID, "ops", false, "", WebUISource("198.51.100.7"))
	if !errors.As(err, &compareErr) {
		t.Fatalf("未勾选短认证码比对时应返回专用错误，实际 %v", err)
	}
	var foreignErr *PairingForeignTargetError
	err = m.ApprovePairing(approveID, "ops", true, "", WebUISource("198.51.100.7"))
	if !errors.As(err, &foreignErr) || foreignErr.TargetURL != "https://127.0.0.1:7878" {
		t.Fatalf("异常地址未输入精确 OVERRIDE 时应返回专用错误，实际 %#v", err)
	}
	if err := m.ApprovePairing(approveID, "ops", true, "override", WebUISource("198.51.100.7")); !errors.As(err, &foreignErr) {
		t.Fatalf("覆盖文字不精确时应返回地址异常错误，实际 %v", err)
	}
	if rows, _ = m.ListPairingRequests(); rows[0].State != "pending" {
		t.Fatalf("被拒绝的批准提交不应改变请求状态：%+v", rows[0])
	}

	if err := m.ApprovePairing(approveID, "ops", true, "OVERRIDE", WebUISource("198.51.100.7")); err != nil {
		t.Fatalf("勾选比对并精确覆盖后应批准：%v", err)
	}
	rows, err = m.ListPairingRequests()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != "approved" || rows[0].ProfileName != "ops" || rows[0].RemainingSeconds != 120 {
		t.Fatalf("批准后的列表形状不正确：%+v", rows[0])
	}
	var notPending *PairingNotPendingError
	if err := m.ApprovePairing(approveID, "ops", true, "OVERRIDE", WebUISource("198.51.100.7")); !errors.As(err, &notPending) {
		t.Fatalf("重复批准应返回非待批准错误，实际 %v", err)
	}

	approveAudit := ownerAuditRowsWithStatus(t, st, "ok", "pair.approve")
	if len(approveAudit) != 1 {
		t.Fatalf("pair.approve 成功审计行数量 = %d，应为 1：%+v", len(approveAudit), approveAudit)
	}
	assertAuditShape(t, approveAudit[0], "pair.approve", "ok", `{"ip":"192.0.2.9","name":"laptop","profile":"`+profileID+`"}`, store.AuditSourceWebUI, "198.51.100.7")
	for _, audit := range approveAudit {
		if strings.Contains(audit.Command, "314159") {
			t.Fatalf("pair.approve 摘要不应包含短认证码：%s", audit.Command)
		}
	}
	approveFailures := ownerAuditRowsWithStatus(t, st, "error", "pair.approve")
	if len(approveFailures) != 4 {
		t.Fatalf("pair.approve 失败审计行数量 = %d，应为 4：%+v", len(approveFailures), approveFailures)
	}
	for _, audit := range approveFailures {
		if audit.Source != store.AuditSourceWebUI || audit.SourceIP != "198.51.100.7" || strings.Contains(audit.Command, "314159") {
			t.Fatalf("pair.approve 失败审计行不正确：%+v", audit)
		}
	}

	rejectID := seedPairingRequest(t, st, now, "tablet")
	if err := m.RejectPairing(rejectID, WebUISource("198.51.100.7")); err != nil {
		t.Fatalf("拒绝待批准请求应成功：%v", err)
	}
	rows, err = m.ListPairingRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != approveID {
		t.Fatalf("拒绝后的列表不应再显示被拒绝请求：%+v", rows)
	}
	rejectAudit := ownerAuditRowsWithStatus(t, st, "ok", "pair.reject")
	if len(rejectAudit) != 1 {
		t.Fatalf("pair.reject 审计行数量 = %d，应为 1：%+v", len(rejectAudit), rejectAudit)
	}
	assertAuditShape(t, rejectAudit[0], "pair.reject", "ok", `{"ip":"192.0.2.9","name":"tablet"}`, store.AuditSourceWebUI, "198.51.100.7")
}

func TestPairingRequestTimeoutAndCompletedRows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	m, st := newTestManager(t)
	st.NowFn = func() time.Time { return now }
	if _, err := st.AddProfile("ops"); err != nil {
		t.Fatal(err)
	}
	expiredID := seedPairingRequest(t, st, now, "expired")
	now = now.Add(11 * time.Minute)
	st.NowFn = func() time.Time { return now }

	rows, err := m.ListPairingRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("超时请求不应继续显示：%+v", rows)
	}
	var notPending *PairingNotPendingError
	if err := m.ApprovePairing(expiredID, "ops", true, "OVERRIDE", CLISource()); !errors.As(err, &notPending) {
		t.Fatalf("超时后批准应返回非待批准错误，实际 %v", err)
	}
	if err := m.RejectPairing(expiredID, CLISource()); !errors.As(err, &notPending) {
		t.Fatalf("超时后拒绝应返回非待批准错误，实际 %v", err)
	}

	now = time.Unix(1800000600, 0)
	st.NowFn = func() time.Time { return now }
	delivered := seedPairingRequestState(t, st, now, "completed", "delivered", now.Add(5*time.Minute))
	rows, err = m.ListPairingRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != delivered || rows[0].State != "delivered" || rows[0].RemainingSeconds != 300 {
		t.Fatalf("已完成请求应在现有过期规则内继续显示：%+v", rows)
	}
}

// TestPairingFailureAuditWriteIsVisible 钉住失败审计本身写失败时的可见性：
// 返回值必须同时携带原操作失败与审计失败，不能把审计错误静默丢掉。
func TestPairingFailureAuditWriteIsVisible(t *testing.T) {
	now := time.Unix(1800000000, 0)
	m, st := newTestManager(t)
	st.NowFn = func() time.Time { return now }
	if _, err := st.AddProfile("ops"); err != nil {
		t.Fatal(err)
	}
	approveID := seedPairingRequest(t, st, now, "laptop")
	rejectID := seedPairingRequest(t, st, now, "tablet")

	err := st.ExecForTest(`CREATE TRIGGER pairing_audit_unavailable
BEFORE INSERT ON audit_log
WHEN NEW.action IN ('pair.approve', 'pair.reject') AND NEW.status = 'error'
BEGIN
  SELECT RAISE(ABORT, 'pairing audit unavailable');
END`)
	if err != nil {
		t.Fatal(err)
	}

	approveErr := m.ApprovePairing(approveID, "ops", false, "", WebUISource("198.51.100.7"))
	if !strings.Contains(approveErr.Error(), "必须先与客户端屏幕上的短认证码逐位比对一致") ||
		!strings.Contains(approveErr.Error(), "pairing audit unavailable") {
		t.Fatalf("批准失败与审计失败都应可见，实际 %v", approveErr)
	}

	if err := m.RejectPairing(rejectID, WebUISource("198.51.100.7")); err != nil {
		t.Fatalf("第一次拒绝应成功：%v", err)
	}
	rejectErr := m.RejectPairing(rejectID, WebUISource("198.51.100.7"))
	if !strings.Contains(rejectErr.Error(), "该请求已不是待批准状态") ||
		!strings.Contains(rejectErr.Error(), "pairing audit unavailable") {
		t.Fatalf("重复拒绝失败与审计失败都应可见，实际 %v", rejectErr)
	}
}
