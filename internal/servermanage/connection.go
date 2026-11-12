package servermanage

import (
	"context"
	"fmt"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/shellprobe"
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
	var notes []string
	writeSource := storeWriteSource(req.Source)
	if req.ProbeShell {
		detected, evidence, notes, err = shellprobe.ProbeServerWithSource(ctx, m.st, srv, writeSource)
	} else {
		notes, err = shellprobe.ConnectServerWithSource(ctx, m.st, srv, writeSource)
	}
	result := &ConnectionTestResult{
		ConnectionNotes: notes,
		ShellDetected:   detected,
		ShellEvidence:   evidence,
	}
	if err != nil {
		m.applyAnchorFacts(result, srv.ID)
		_ = m.writeConnectionTestAudit(req.Source, srv.Name, "error", result.AnchorFingerprint, string(detected), err)
		return result, err
	}

	if req.ProbeShell && detected != "" && detected != srv.ShellType {
		detectedText := string(detected)
		if _, err = m.Edit(EditRequest{
			Identifier: srv.ID, ExpectedRevision: srv.Revision, CheckRevision: true,
			Shell: &detectedText,
		}, req.Source); err != nil {
			m.applyAnchorFacts(result, srv.ID)
			_ = m.writeConnectionTestAudit(req.Source, srv.Name, "error", result.AnchorFingerprint, string(detected), err)
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
	return result, nil
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
