package servermanage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/shellprobe"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// ConnectionTestRequest 描述新增服务器保存后的一次自动连接测试。
type ConnectionTestRequest struct {
	Identifier string
	ProbeShell bool
	Source     Source
}

// ConnectionTestResult 是连接测试给页面与任务层的结果。主机密钥指纹不是登
// 录秘密，可以展示；登录凭据与提权口令绝不进入这里的任何字段。
type ConnectionTestResult struct {
	models.ServerConnectionTestResult
	ConnectionNotes   []string
	ShellDetected     models.ShellType
	ShellEvidence     string
	AnchorFingerprint string
	AnchorResult      string
	HandshakeAddress  string
}

// TestConnection 按服务器条目统一拨号入口测试连接，首次连接沿用首次信任规
// 则记录锚；需要自动探测 shell 时在同一连接上执行既有两条固定命令并回写。
func (m *Manager) TestConnection(
	ctx context.Context, req ConnectionTestRequest,
) (*ConnectionTestResult, error) {
	srv, err := m.st.ResolveServer(req.Identifier)
	if err != nil || srv == nil {
		if err == nil {
			err = fmt.Errorf("server %q not found", req.Identifier)
		}
		_ = m.writeConnectionTestAudit(req.Source, req.Identifier, "error", "", "", err)
		return &ConnectionTestResult{}, err
	}
	before, err := m.st.GetEntryPinMeta(srv.ID)
	if err != nil {
		_ = m.writeConnectionTestAudit(req.Source, srv.Name, "error", "", "", err)
		return &ConnectionTestResult{}, err
	}

	var detected models.ShellType
	var evidence string
	var dial sshbroker.ServerDialResult
	writeSource := storeWriteSource(req.Source)
	if req.ProbeShell {
		dial, detected, evidence, err = shellprobe.ProbeServerWithDialResult(ctx, m.st, srv, writeSource)
	} else {
		dial, err = shellprobe.ConnectServerWithDialResult(ctx, m.st, srv, writeSource)
	}
	result := &ConnectionTestResult{
		ServerConnectionTestResult: connectionTestStorageResult(dial, time.Now()),
		ConnectionNotes:            dial.ConnectionNotes,
		ShellDetected:              detected,
		ShellEvidence:              evidence,
	}
	if err != nil {
		result.Succeeded = false
		result.FailureReason = overallConnectionFailureReason(dial, err)
		var persistErr error
		if !errors.Is(err, context.Canceled) {
			persistErr = m.st.SaveServerConnectionTestResult(srv.ID, result.ServerConnectionTestResult)
		}
		m.applyAnchorFacts(result, srv.ID)
		_ = m.writeConnectionTestAudit(req.Source, srv.Name, "error", result.AnchorFingerprint, string(detected), err)
		if persistErr != nil {
			return result, fmt.Errorf("%w；保存最近一次连接测试结果失败：%v", err, persistErr)
		}
		return result, err
	}

	if req.ProbeShell && detected != "" && detected != srv.ShellType {
		detectedText := string(detected)
		if _, err = m.Edit(EditRequest{
			Identifier: srv.ID, ExpectedRevision: srv.Revision, CheckRevision: true,
			Shell: &detectedText,
		}, req.Source); err != nil {
			m.applyAnchorFacts(result, srv.ID)
			result.Succeeded = false
			result.FailureReason = models.ConnectionTestFailureOther
			persistErr := m.st.SaveServerConnectionTestResult(srv.ID, result.ServerConnectionTestResult)
			_ = m.writeConnectionTestAudit(req.Source, srv.Name, "error", result.AnchorFingerprint, string(detected), err)
			if persistErr != nil {
				return result, fmt.Errorf("%w；保存最近一次连接测试结果失败：%v", err, persistErr)
			}
			return result, err
		}
	}

	after, err := m.st.GetEntryPinMeta(srv.ID)
	if err != nil {
		_ = m.writeConnectionTestAudit(req.Source, srv.Name, "error", "", string(detected), err)
		return result, err
	}
	if after != nil {
		result.AnchorFingerprint = store.PinFingerprint(after.Blob, after.Format)
		result.HandshakeAddress = after.HandshakeAddr
		if before == nil {
			result.AnchorResult = "首次连接已记录主机密钥锚"
		} else {
			result.AnchorResult = "已按现有主机密钥锚完成校验"
		}
	}
	if err := m.writeConnectionTestAudit(req.Source, srv.Name, "ok", result.AnchorFingerprint, string(detected), nil); err != nil {
		return result, err
	}
	if err := m.st.SaveServerConnectionTestResult(srv.ID, result.ServerConnectionTestResult); err != nil {
		return result, fmt.Errorf("保存最近一次连接测试结果失败：%w", err)
	}
	return result, nil
}

func connectionTestStorageResult(
	dial sshbroker.ServerDialResult, completedAt time.Time,
) models.ServerConnectionTestResult {
	return models.ServerConnectionTestResult{
		CompletedAt: completedAt, Succeeded: true, Attempts: dial.Attempts,
		SuccessAddress: dial.SuccessAddress, SuccessNetworkName: dial.SuccessNetworkName,
		SuccessSource: dial.SuccessSource,
		Duration:      dial.Duration,
	}
}

func overallConnectionFailureReason(
	dial sshbroker.ServerDialResult, err error,
) models.ConnectionTestFailureReason {
	priority := []models.ConnectionTestFailureReason{
		models.ConnectionTestFailureHostKeyMismatch,
		models.ConnectionTestFailureAuthentication,
		models.ConnectionTestFailureTimeout,
		models.ConnectionTestFailurePortUnreachable,
	}
	found := map[models.ConnectionTestFailureReason]bool{}
	for _, attempt := range dial.Attempts {
		found[attempt.FailureReason] = true
	}
	for _, reason := range priority {
		if found[reason] {
			return reason
		}
	}
	return models.ConnectionTestFailureOther
}

// applyAnchorFacts 只补充失败结果仍可确认的锚事实；锚定结论只在连接测试成
// 功后由调用方设置。
func (m *Manager) applyAnchorFacts(result *ConnectionTestResult, serverID string) {
	after, err := m.st.GetEntryPinMeta(serverID)
	if err != nil || after == nil {
		return
	}
	result.AnchorFingerprint = store.PinFingerprint(after.Blob, after.Format)
	result.HandshakeAddress = after.HandshakeAddr
}

func (m *Manager) writeConnectionTestAudit(source Source, name, status, fingerprint, shell string, err error) error {
	summary := map[string]any{"name": name, "result": status}
	if fingerprint != "" {
		summary["fingerprint"] = fingerprint
	}
	if shell != "" {
		summary["shell"] = shell
	}
	return m.writeAudit("server.connect-test", summary, err, source)
}
