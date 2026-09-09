package clientops

// Plan 48 §2.2 — the cache-mode HostKeyStore: a forwarding wrapper between the
// TOFU callback and the hot-reloading read-only store. This lives in clientops
// (not mcpserver) because every piece it composes is ours — the PinForwarder,
// the §2.1 branch texts, ErrPinForwardEqual — and clientops already builds on
// mcpserver, so the dependency cannot point the other way. mcpserver binds it
// through ForwardingHostKeys at RunStdioCache time.
//
// Reads AND writes resolve cur() INSIDE every method (the real-time
// resolution iron rule: the ≤10 s forwarding HTTP call may straddle a hot
// rebuild, so any construction-time store capture would strand the anchor in
// a swapped-out generation — §8 T5b asserts this). SaveHostKey forwards the
// presented key to the broker's /pin-hostkey endpoint:
//
//	201            → ApplyForwardedHostKey on the current store → nil (handshake continues)
//	409 equal=true → ApplyForwardedHostKey (the broker already holds an equal
//	                 anchor) → nil — automatic closure of duplicate forwards
//	anything else  → the branch's verbatim §2.1/§4 text, raised through
//	                 sshbroker.PresentedHostKeyError so HostKeyTOFU adds no prefix
//
// A nil forwarder (no forwarding capability) degrades to the plain read-only
// store behavior — SaveHostKey returns ErrReadOnly exactly as pre-Plan-48
// cache mode did. Never a silent local writable overlay (§0).

import (
	"errors"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// forwardingHostKeys adapts cache mode to the TOFU callback. cur is the
// holder's Current (resolved per call — the iron rule); fwd is the cli-built
// PinForwarder for this instance.
type forwardingHostKeys struct {
	cur func() *store.Store
	fwd *PinForwarder
}

// ForwardingHostKeys returns the RunStdioCache binder (the "转发构造器" the
// brief hands from cli): invoking it with the holder's Current yields the
// process-lifetime forwarding HostKeyStore. The two-phase shape exists because
// the hot-reload holder — and therefore the cur resolver — only exists inside
// RunStdioCache, while the PinForwarder is built at the --instance resolver in
// cli (reading an instance is always explicit; Plan 40/46).
func ForwardingHostKeys(fwd *PinForwarder) func(cur func() *store.Store) sshbroker.HostKeyStore {
	return func(cur func() *store.Store) sshbroker.HostKeyStore {
		return &forwardingHostKeys{cur: cur, fwd: fwd}
	}
}

// GetHostKey reads the CURRENT generation — resolved at callback time, not
// construction time (§2.2).
func (h *forwardingHostKeys) GetHostKey(host string, port int) (*store.Pin, error) {
	return h.cur().GetHostKey(host, port)
}

// SaveHostKey is the forwarding path: an unknown anchor is POSTed to the
// broker (audited server-side), and on 201 / 409-equal applied to the CURRENT
// store locally so the handshake continues. Every other outcome surfaces the
// branch's verbatim text — no prefix — via sshbroker.PresentedHostKeyError.
func (h *forwardingHostKeys) SaveHostKey(host string, port int, marshaledKey []byte) error {
	if h.fwd == nil {
		// No forwarding capability: the plain read-only store refusal
		// (ErrReadOnly, wrapped by HostKeyTOFU as before — pre-Plan-48 form).
		return h.cur().SaveHostKey(host, port, marshaledKey)
	}
	ferr := h.fwd.Forward(host, port, marshaledKey)
	if ferr == nil || errors.Is(ferr, ErrPinForwardEqual) {
		// 201, or the broker already holds an EQUAL anchor: apply locally.
		// cur() is re-resolved HERE — after the HTTP call — so a hot rebuild
		// that fired during the forwarding window lands the anchor in the
		// current generation (the T5b contract).
		if aerr := h.cur().ApplyForwardedHostKey(host, port, marshaledKey); aerr != nil {
			// Ultra-rare race (§2.3): a rebuild between the 201 and this apply
			// swapped in a generation holding a DIFFERENT anchor. The text
			// already carries the pull-and-retry guidance.
			return &sshbroker.PresentedHostKeyError{Err: aerr}
		}
		return nil
	}
	return &sshbroker.PresentedHostKeyError{Err: ferr}
}
