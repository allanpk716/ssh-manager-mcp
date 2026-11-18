package access

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/localaddr"
	"ssh-manager-mcp/internal/store"
)

// PairingRequest 是配对页的一行展示数据。短认证码只有在存在时返回；设备
// 名、目标地址、来源地址与 profile 提示都来自客户端请求，调用方展示前需
// 保持超文本转义。临时配对私钥绝不进入这个结构。
type PairingRequest struct {
	ID               string
	Name             string
	TargetURL        string
	ShortCode        string
	SourceAddress    string
	ProfileHint      string
	ProfileName      string
	ReplaceInactive  bool
	State            string
	ForeignTarget    bool
	RemainingSeconds int64
	HasShortCode     bool
}

// PairingComparisonRequiredError 表示 owner 没有确认已逐位比对短认证码。
type PairingComparisonRequiredError struct{}

func (e *PairingComparisonRequiredError) Error() string {
	return "必须先与客户端屏幕上的短认证码逐位比对一致，才能批准配对"
}

// PairingForeignTargetError 表示客户端声明的目标地址不属于本机，也不在
// 已登记的本机对外地址里。
type PairingForeignTargetError struct {
	TargetURL string
}

func (e *PairingForeignTargetError) Error() string {
	return fmt.Sprintf("⚠ 配对声明目标不属于本机地址（%s），也不在已登记的本机对外地址里，建议拒绝；Web UI 不能放行，命令行可用 --allow-foreign-url 显式放行", e.TargetURL)
}

// PairingNotPendingError 表示请求已超时、已被批准或已被拒绝，不再是待批准
// 状态。
type PairingNotPendingError struct {
	State string
}

func (e *PairingNotPendingError) Error() string {
	return "该请求已不是待批准状态"
}

// ListPairingRequests 列出仍在展示窗口内的配对请求：待批准、已批准等待客
// 户端完成、已完成等待按现有规则清理。列表带服务端计算的剩余时间与目标
// 地址是否属于本机的判断。
func (m *Manager) ListPairingRequests() ([]PairingRequest, error) {
	publicAddresses, err := m.registeredPublicAddresses()
	if err != nil {
		return nil, err
	}
	rows, err := m.st.ListVisiblePairing()
	if err != nil {
		return nil, err
	}
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(profiles))
	for _, profile := range profiles {
		names[profile.ID] = profile.Name
	}
	now := m.clockNow().Unix()
	out := make([]PairingRequest, 0, len(rows))
	for _, row := range rows {
		deadline := row.EnrollDeadline
		if row.State == "approved" || row.State == "delivered" {
			deadline = row.ApprovedDeadline
		}
		remaining := deadline - now
		if remaining < 0 {
			remaining = 0
		}
		shortCode := strings.TrimSpace(row.SAS)
		out = append(out, PairingRequest{
			ID: hex.EncodeToString(row.ID), Name: row.Name, TargetURL: row.TargetURL,
			ShortCode: shortCode, SourceAddress: row.SourceIP, ProfileHint: row.ProfileHint,
			ProfileName: names[row.Profile], ReplaceInactive: row.ReplaceInactive, State: row.State,
			ForeignTarget: foreignPairingTarget(row.TargetURL, publicAddresses), RemainingSeconds: remaining,
			HasShortCode: shortCode != "",
		})
	}
	return out, nil
}

// ApprovePairing 批准一条待批准配对请求。请求必须已确认逐位比对短认证
// 码；目标地址不属于本机时只有命令行显式放行可以继续。成功审计行与状态
// 变化在同一个存储事务里提交，失败请求由模块写失败审计行。
func (m *Manager) ApprovePairing(identifier, profile string, compared bool, allowForeignTarget bool, src Source) error {
	row, err := m.resolvePairing(identifier)
	if err != nil {
		return err
	}
	summary := map[string]any{"ip": row.SourceIP, "name": row.Name, "profile": profile}
	err = func() error {
		if row.State != "pending" {
			return &PairingNotPendingError{State: row.State}
		}
		if !compared {
			return &PairingComparisonRequiredError{}
		}
		publicAddresses, addressErr := m.registeredPublicAddresses()
		if addressErr != nil {
			return addressErr
		}
		if foreignPairingTarget(row.TargetURL, publicAddresses) && !allowForeignTarget {
			return &PairingForeignTargetError{TargetURL: row.TargetURL}
		}
		resolved, resolveErr := m.resolveProfile(profile)
		if resolveErr != nil {
			return resolveErr
		}
		summary["profile"] = resolved.Name
		ok, approveErr := m.st.ApprovePairingWithSource(row.ID, resolved.ID, src.kind, src.addr)
		if approveErr != nil {
			return approveErr
		}
		if !ok {
			return &PairingNotPendingError{State: "changed"}
		}
		return nil
	}()
	if err != nil {
		if auditErr := m.writeAudit("pair.approve", summary, err, src); auditErr != nil {
			return errors.Join(err, auditErr)
		}
	}
	return err
}

// RejectPairing 拒绝一条待批准配对请求；成功审计行与状态变化在同一个存
// 储事务里提交，失败请求由模块写失败审计行。
func (m *Manager) RejectPairing(identifier string, src Source) error {
	row, err := m.resolvePairing(identifier)
	if err != nil {
		return err
	}
	summary := map[string]any{"ip": row.SourceIP, "name": row.Name}
	err = func() error {
		if row.State != "pending" {
			return &PairingNotPendingError{State: row.State}
		}
		ok, rejectErr := m.st.RejectPairingWithSource(row.ID, src.kind, src.addr)
		if rejectErr != nil {
			return rejectErr
		}
		if !ok {
			return &PairingNotPendingError{State: "changed"}
		}
		return nil
	}()
	if err != nil {
		if auditErr := m.writeAudit("pair.reject", summary, err, src); auditErr != nil {
			return errors.Join(err, auditErr)
		}
	}
	return err
}

// resolvePairing 按配对请求标识符或设备名解析。设备名必须唯一；标识符是
// 32 字节请求标识的十六进制文本。
func (m *Manager) resolvePairing(identifier string) (*store.PendingPairing, error) {
	rows, err := m.st.ListPendingPairing()
	if err != nil {
		return nil, err
	}
	var byName []*store.PendingPairing
	var byID *store.PendingPairing
	if id, decodeErr := hex.DecodeString(strings.TrimSpace(identifier)); decodeErr == nil && len(id) == 32 {
		stored, findErr := m.st.PairingByID(id)
		if findErr != nil {
			return nil, findErr
		}
		if stored != nil {
			byID = stored
		}
	}
	for i := range rows {
		if rows[i].Name == identifier {
			byName = append(byName, &rows[i])
		}
	}
	switch {
	case len(byName) == 1:
		return byName[0], nil
	case len(byName) > 1:
		return nil, fmt.Errorf("设备名 %q 对应多条配对请求，请用请求标识符指定", identifier)
	case byID != nil:
		return byID, nil
	}
	return nil, fmt.Errorf("没有找到配对请求 %q", identifier)
}

// clockNow 读取存储上的可控时钟。配对窗口由存储在同一时钟下裁决，列表
// 的剩余秒数必须与批准、拒绝、完成窗口保持同一个时间语义。
func (m *Manager) clockNow() time.Time {
	if m.st.NowFn != nil {
		return m.st.NowFn()
	}
	return time.Now()
}

// foreignPairingTarget 与配对接口使用的机械地址检查保持同一规则：目标主
// 机不是本机非环回地址、本机主机名或已登记对外地址时判定为异常，解析失
// 败也按异常处理。
func foreignPairingTarget(targetURL string, publicAddresses []localaddr.RegisteredAddress) bool {
	return localaddr.ForeignTarget(targetURL, publicAddresses...)
}

// registeredPublicAddresses 读取并解析已登记的本机对外地址。设置值非法时
// 返回错误并按异常地址处理，避免被绕过。
func (m *Manager) registeredPublicAddresses() ([]localaddr.RegisteredAddress, error) {
	value, stored, err := m.st.GetSetting(localaddr.PublicAddressesSettingKey)
	if err != nil {
		return nil, err
	}
	if !stored {
		return nil, nil
	}
	return localaddr.ParseRegisteredAddresses(value)
}
