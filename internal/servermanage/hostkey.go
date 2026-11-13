package servermanage

import (
	"fmt"
	"time"

	"ssh-manager-mcp/internal/store"
)

// ReplaceHostKeyPin 用 owner 输入的 OpenSSH 主机密钥指纹覆盖服务器条目的
// 主机密钥锚。语义与 servers pin-hostkey <name> --fingerprint … --force 一
// 致：force 为真才替换已有锚，为假时已有锚会被拒绝；指纹格式校验与命令行
// 共用同一套判定。返回值表示条目是否存在，false 表示找不到条目且未写任何
// 数据。审计动作沿用 pin-manual 的现有习惯，来源列按调用方填写。
func (m *Manager) ReplaceHostKeyPin(identifier, fingerprint string, force bool, src Source) (bool, error) {
	srv, err := m.st.ResolveServer(identifier)
	if err != nil {
		return false, err
	}
	if srv == nil {
		return false, nil
	}
	blob, display, err := store.ParseFingerprintPin(fingerprint)
	if err != nil {
		return true, &FieldError{
			Field: "fingerprint",
			CLI:   err.Error(),
			Page:  "主机密钥指纹必须是 SHA256 冒号加 32 字节摘要的无填充 base64 字符串（ssh-keygen -lf 的输出形式）。",
		}
	}
	hostPort := store.EntryFallbackAddr(srv)
	auditFor := func(old *store.PinMeta) store.AuditRow {
		command := fmt.Sprintf("entry=%s host=%s fp=%s via=fingerprint forced=%t", srv.Name, hostPort, display, force)
		if old != nil {
			command += " was=" + store.PinFingerprint(old.Blob, old.Format)
		}
		row := store.AuditRow{
			TS: time.Now(), ServerID: srv.ID, Action: "pin-manual",
			Command: command, Status: "ok",
		}
		row.Source = src.kind
		row.SourceIP = src.addr
		return row
	}
	if _, err := m.st.UpsertManualEntryPin(srv, "", blob, store.PinFormatFingerprint, force, auditFor); err != nil {
		return true, err
	}
	return true, nil
}

// ClearHostKeyPin 删除服务器条目当前的主机密钥锚并写审计，幂等：条目本来
// 就没有锚时成功返回且不写审计。返回值表示条目是否存在。审计动作沿用
// pin-clear 的现有习惯，来源列按调用方填写。
func (m *Manager) ClearHostKeyPin(identifier string, src Source) (bool, error) {
	srv, err := m.st.ResolveServer(identifier)
	if err != nil {
		return false, err
	}
	if srv == nil {
		return false, nil
	}
	auditFor := func(old store.PinMeta) store.AuditRow {
		handshakeAddr := old.HandshakeAddr
		if handshakeAddr == "" {
			handshakeAddr = "-"
		}
		row := store.AuditRow{
			TS: time.Now(), ServerID: srv.ID, Action: "pin-clear",
			Command: fmt.Sprintf("entry=%s host=%s fp=%s source=%s handshake=%s",
				srv.Name, store.EntryFallbackAddr(srv),
				store.PinFingerprint(old.Blob, old.Format), old.Source, handshakeAddr),
			Status: "ok",
		}
		row.Source = src.kind
		row.SourceIP = src.addr
		return row
	}
	if _, err := m.st.ClearEntryPin(srv.ID, auditFor); err != nil {
		return true, err
	}
	return true, nil
}
