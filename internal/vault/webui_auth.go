package vault

import (
	"time"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// WebUIAuthStore 把 vault 的 Web 管理员鉴权状态适配成 Web UI 包的窄接口。
// serve 进程与命令行共用这份适配，避免两处分别解释数据库存储格式。
type WebUIAuthStore struct {
	store *store.Store
}

// NewWebUIAuthStore 返回指向已解锁 vault 的 Web UI 鉴权存储适配器。
func NewWebUIAuthStore(st *store.Store) webui.AuthStore {
	return WebUIAuthStore{store: st}
}

func (s WebUIAuthStore) AdminPasswordIsSet() (bool, error) {
	return s.store.WebAdminPasswordIsSet()
}

func (s WebUIAuthStore) VerifyAdminPassword(password []byte) (bool, error) {
	return s.store.VerifyWebAdminPassword(password)
}

func (s WebUIAuthStore) SetAdminPassword(password []byte) (int64, error) {
	return s.store.SetWebAdminPassword(password)
}

func (s WebUIAuthStore) SessionGeneration() (int64, error) {
	return s.store.WebAdminSessionGeneration()
}

func (s WebUIAuthStore) LogoutAllSessions() (int64, error) {
	return s.store.LogoutAllWebAdminSessions()
}

func (s WebUIAuthStore) LoginLimit(ip string, now time.Time) (webui.LoginLimit, error) {
	state, err := s.store.WebAdminLoginState(ip, now)
	return webui.LoginLimit{
		Locked: state.Locked, LockRemaining: state.LockRemaining, Delay: state.Delay,
	}, err
}

func (s WebUIAuthStore) RecordLoginFailure(ip string, now time.Time) (webui.LoginLimit, error) {
	state, err := s.store.RecordWebAdminLoginFailure(ip, now)
	return webui.LoginLimit{
		Locked: state.Locked, LockRemaining: state.LockRemaining, Delay: state.Delay,
	}, err
}

func (s WebUIAuthStore) ClearLoginLock(ip string) error {
	return s.store.ClearWebAdminLoginFailures(ip)
}

func (s WebUIAuthStore) WriteAudit(event webui.AuditEvent) error {
	detail := make(map[string]any, len(event.Detail))
	for key, value := range event.Detail {
		detail[key] = value
	}
	status := event.Status
	if status == "" {
		status = "ok"
	}
	return s.store.WriteAudit(store.AuditRow{
		TS: time.Now(), Action: event.Action, Status: status,
		Source: store.AuditSourceWebUI, SourceIP: event.SourceIP,
		Command: store.MarshalAuditSummary(detail),
	})
}
