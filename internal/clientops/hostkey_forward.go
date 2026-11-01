package clientops

// 客户端缓存模式的锚存取包装（规格 #88）：首次连接即信任的回调经这里把
// 学到的主机密钥转发给权威端，转发成功（201，或 409 且密钥相同）后把锚写
// 进当前热更新代的本地缓存，键为服务器条目。没有转发能力时退化为只读库
// 的拒绝，绝不写本地可写覆盖层。

import (
	"errors"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// forwardingHostKeys adapts cache mode to the TOFU callback. cur is the
// holder's Current (resolved per call); fwd is the cli-built PinForwarder for
// this instance.
type forwardingHostKeys struct {
	cur func() *store.Store
	fwd *PinForwarder
}

// ForwardingHostKeys returns the RunStdioCache binder: invoking it with the
// holder's Current yields the process-lifetime forwarding HostKeyStore.
func ForwardingHostKeys(fwd *PinForwarder) func(cur func() *store.Store) sshbroker.HostKeyStore {
	return func(cur func() *store.Store) sshbroker.HostKeyStore {
		return &forwardingHostKeys{cur: cur, fwd: fwd}
	}
}

// GetEntryPin 在回调发生时的当前热更新代里读取条目的锚。
func (h *forwardingHostKeys) GetEntryPin(srv *models.Server) (*store.Pin, error) {
	return h.cur().GetEntryPin(srv)
}

// SaveEntryPin is the forwarding path: an unknown anchor is POSTed to the
// broker (audited server-side), and on 201 / 409-equal applied to the CURRENT
// store locally so the handshake continues. Every other outcome surfaces the
// branch's verbatim text — no prefix — via sshbroker.PresentedHostKeyError.
func (h *forwardingHostKeys) SaveEntryPin(srv *models.Server, marshaledKey []byte) error {
	if h.fwd == nil {
		// No forwarding capability: the plain read-only store refusal.
		return h.cur().SaveEntryPin(srv, marshaledKey)
	}
	ferr := h.fwd.Forward(srv, marshaledKey)
	if ferr == nil || errors.Is(ferr, ErrPinForwardEqual) {
		// cur() is re-resolved HERE — after the HTTP call — so a hot rebuild
		// that fired during the forwarding window lands the anchor in the
		// current generation.
		if aerr := h.cur().ApplyForwardedEntryPin(srv, store.EntryFallbackAddr(srv), marshaledKey); aerr != nil {
			return &sshbroker.PresentedHostKeyError{Err: aerr}
		}
		return nil
	}
	return &sshbroker.PresentedHostKeyError{Err: ferr}
}
