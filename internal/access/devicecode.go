package access

import (
	"errors"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
)

// DeviceCodeSummary 是设备码列表的一行：令牌本体从不出现，只有前缀；已
// 撤销的设备码仍列出，供回查历史。
type DeviceCodeSummary struct {
	ID            string
	Name          string
	TokenPrefix   string
	Status        models.CacheTokenStatus
	ProfileID     string
	ProfileName   string
	LastPullAt    time.Time
	ClientVersion string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DeviceCodeResult 是签发设备码的结果：一次性设备码、权威端证书公钥指纹
// 和可直接粘贴的拉取命令只在这一次调用里返回。调用方必须在同一次响应里
// 展示，离开页面后不能再查看。
type DeviceCodeResult struct {
	Name        string
	Code        string
	Fingerprint string
	PullCommand string
}

// DeviceCodePullCommand 返回已代入设备码与证书公钥指纹的 cache pull 命令。
// 命令行与 Web UI 共用这个函数，保证两个入口给 owner 抄的是同一条命令。
func DeviceCodePullCommand(serveURL, code, fingerprint string) string {
	return fmt.Sprintf("sshmgr cache pull --url %s --token '%s:%s'", serveURL, code, fingerprint)
}

// ListDeviceCodes 按名字顺序列出全部设备码（含已撤销的），并解析各自绑定
// 的 profile 名字；未绑定的旧设备码保留空标识符与空名字。
func (m *Manager) ListDeviceCodes() ([]DeviceCodeSummary, error) {
	devices, err := m.st.ListCacheTokens()
	if err != nil {
		return nil, err
	}
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	profileNames := make(map[string]string, len(profiles))
	for _, p := range profiles {
		profileNames[p.ID] = p.Name
	}
	out := make([]DeviceCodeSummary, 0, len(devices))
	for _, ct := range devices {
		out = append(out, DeviceCodeSummary{
			ID:            ct.ID,
			Name:          ct.Name,
			TokenPrefix:   ct.TokenPrefix,
			Status:        ct.Status,
			ProfileID:     ct.ProfileID,
			ProfileName:   profileNames[ct.ProfileID],
			LastPullAt:    ct.LastPullAt,
			ClientVersion: ct.ClientVersion,
			CreatedAt:     ct.CreatedAt,
			UpdatedAt:     ct.UpdatedAt,
		})
	}
	return out, nil
}

// resolveDeviceCode 按设备名或标识符解析（命令行传名字，Web UI 的表单与
// 链接用标识符）。找不到时返回与命令行一致口径的错误。
func (m *Manager) resolveDeviceCode(identifier string) (*models.CacheToken, error) {
	devices, err := m.st.ListCacheTokens()
	if err != nil {
		return nil, err
	}
	for _, ct := range devices {
		if ct.Name == identifier || ct.ID == identifier {
			return ct, nil
		}
	}
	return nil, fmt.Errorf("device code %q not found", identifier)
}

// AddDeviceCode 签发一个绑定 profile 的设备码并写 `cache-token.add` 审计
// 行。证书指纹在产生设备码之前读取，读取失败时不留下设备码；失败也写
// 失败审计行。审计摘要只有设备名与 profile 名，绝不含设备码。
func (m *Manager) AddDeviceCode(name, profile, serveURL string, src Source) (DeviceCodeResult, error) {
	var result DeviceCodeResult
	summary := map[string]any{"name": name, "profile": profile}
	err := func() error {
		p, err := m.resolveProfile(profile)
		if err != nil {
			return err
		}
		summary["profile"] = p.Name
		if m.serveCertFingerprint == nil {
			return errors.New("访问管理模块没有配置权威端证书指纹来源，不能签发设备码")
		}
		fingerprint, err := m.serveCertFingerprint()
		if err != nil {
			return fmt.Errorf("load serve cert for fingerprint: %w (run `serve cert-info` to diagnose)", err)
		}
		_, code, err := m.st.AddCacheToken(name, p.ID)
		if err != nil {
			return err
		}
		result = DeviceCodeResult{
			Name:        name,
			Code:        code,
			Fingerprint: fingerprint,
			PullCommand: DeviceCodePullCommand(serveURL, code, fingerprint),
		}
		return nil
	}()
	if aerr := m.writeAudit("cache-token.add", summary, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return result, err
}

// RevokeDeviceCode 撤销一个设备码并写 `cache-token.revoke` 审计行：撤销
// 是懒惰生效，该设备的下一次拉取即被拒绝。设备码不存在时报错并写失败
// 审计行。
func (m *Manager) RevokeDeviceCode(identifier string, src Source) error {
	summary := map[string]any{"name": identifier}
	err := func() error {
		ct, err := m.resolveDeviceCode(identifier)
		if err != nil {
			return err
		}
		summary["name"] = ct.Name
		return m.st.RevokeCacheToken(ct.Name)
	}()
	if aerr := m.writeAudit("cache-token.revoke", summary, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return err
}
