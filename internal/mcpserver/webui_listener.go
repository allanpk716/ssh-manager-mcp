package mcpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"ssh-manager-mcp/internal/webui"
)

// WebUIListenerStatus 是一次同步后的监听状态。PendingAddresses 是绑定失败、
// 正在等待 30 秒重试的地址。
type WebUIListenerStatus struct {
	ListeningAddresses []string
	PendingAddresses   []string
	NextRetry          time.Time
	NeedsManualListen  bool
}

// WebUIListenerOptions 是 Web UI 监听管理者的注入点。Bind 与 Serve 分开，
// 测试可以观察绑定与关闭，不需要真的处理 HTTPS 流量。
type WebUIListenerOptions struct {
	Port            int
	Plan            func(time.Time) (webui.ListenPlan, error)
	Bind            func(address string) (net.Listener, error)
	Serve           func(net.Listener) error
	SetAllowedHosts func([]string)
	Log             func(format string, arguments ...any)
}

type webUIListenerManager struct {
	options WebUIListenerOptions
	mu      sync.Mutex
	active  map[string]net.Listener
	pending map[string]time.Time
	hintLog bool
	closed  bool
}

// NewWebUIListenerManager 构造监听管理者。它不立即绑定；调用方在启动时调用
// Sync，然后让 Run 周期性感知拓扑变化。
func NewWebUIListenerManager(options WebUIListenerOptions) *webUIListenerManager {
	if options.Bind == nil {
		options.Bind = func(address string) (net.Listener, error) {
			return net.Listen("tcp", address)
		}
	}
	return &webUIListenerManager{
		options: options,
		active:  map[string]net.Listener{},
		pending: map[string]time.Time{},
	}
}

// Sync 按当前推导结果整理监听：关闭不再需要的地址，打开新增地址，失败地址
// 记录 30 秒后的重试时间，并把实际监听地址同步给 Web UI Handler。
func (m *webUIListenerManager) Sync(now time.Time) (WebUIListenerStatus, error) {
	plan, err := m.options.Plan(now)
	if err != nil {
		return WebUIListenerStatus{}, err
	}
	desired := addressSet(plan.Addresses)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return WebUIListenerStatus{}, errors.New("Web UI 监听管理者已关闭")
	}
	var opened []net.Listener
	for address, listener := range m.active {
		if _, ok := desired[address]; !ok {
			delete(m.active, address)
			_ = listener.Close()
		}
	}
	for _, address := range plan.Addresses {
		if _, ok := m.active[address]; ok {
			continue
		}
		if retryAt, ok := m.pending[address]; ok && now.Before(retryAt) {
			continue
		}
		listener, bindErr := m.options.Bind(net.JoinHostPort(address, strconv.Itoa(m.options.Port)))
		if bindErr != nil {
			m.pending[address] = now.Add(30 * time.Second)
			m.logLocked("sshmgr serve: Web UI 绑定 %s 失败，30 秒后重试：%v", address, bindErr)
			continue
		}
		delete(m.pending, address)
		m.active[address] = listener
		opened = append(opened, listener)
	}
	activeAddresses := make([]string, 0, len(m.active))
	pendingAddresses := make([]string, 0, len(m.pending))
	nextRetry := time.Time{}
	for address := range m.active {
		activeAddresses = append(activeAddresses, address)
	}
	for address, retryAt := range m.pending {
		if _, ok := desired[address]; !ok {
			delete(m.pending, address)
			continue
		}
		pendingAddresses = append(pendingAddresses, address)
		if nextRetry.IsZero() || retryAt.Before(nextRetry) {
			nextRetry = retryAt
		}
	}
	sort.Strings(activeAddresses)
	sort.Strings(pendingAddresses)
	if plan.NeedsManualListen && !m.hintLog {
		m.logLocked("sshmgr serve: 未找到本机存在的虚拟局域网地址，Web UI 当前只监听 127.0.0.1；可用设置项 serve.ui_listen 指定地址")
	}
	m.hintLog = plan.NeedsManualListen
	m.mu.Unlock()

	for _, listener := range opened {
		go func(listener net.Listener) {
			if err := m.options.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.options.Log("sshmgr serve: Web UI 监听 %s 退出：%v", listener.Addr(), err)
			}
		}(listener)
	}
	if m.options.SetAllowedHosts != nil {
		m.options.SetAllowedHosts(activeAddresses)
	}
	return WebUIListenerStatus{
		ListeningAddresses: activeAddresses,
		PendingAddresses:   pendingAddresses,
		NextRetry:          nextRetry,
		NeedsManualListen:  plan.NeedsManualListen,
	}, nil
}

// Run 周期性重新计算监听地址。固定轮询避免要求 vault 提供专门的变更通知。
func (m *webUIListenerManager) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return m.Close()
	default:
	}
	if _, err := m.Sync(time.Now()); err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return m.Close()
		case <-ticker.C:
			if _, err := m.Sync(time.Now()); err != nil {
				return err
			}
		}
	}
}

// Close 关闭所有实际打开的监听。
func (m *webUIListenerManager) Close() error {
	m.mu.Lock()
	m.closed = true
	var listeners []net.Listener
	for address, listener := range m.active {
		listeners = append(listeners, listener)
		delete(m.active, address)
	}
	m.pending = map[string]time.Time{}
	m.mu.Unlock()
	var combined error
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	if m.options.SetAllowedHosts != nil {
		m.options.SetAllowedHosts(nil)
	}
	return combined
}

func (m *webUIListenerManager) logLocked(format string, arguments ...any) {
	if m.options.Log != nil {
		m.options.Log(format, arguments...)
	}
}

func addressSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
