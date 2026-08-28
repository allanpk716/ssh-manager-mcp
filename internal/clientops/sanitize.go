// sanitize.go — C0/C1 control-character stripping for UNAUTHENTICATED input at
// render time (spec rev4 §3.2/§3.3-1 codex#4: "所有展示面渲染前剥离 C0/C1 控制
// 字符"). Discovery offers, pairing rows (name/target_url/profile_hint) and
// similar peer-supplied strings reach terminals (CLI stdout, the broker TUI)
// verbatim; a stray ESC/C1 byte in such a field is a terminal-injection vector
// (cursor moves, screen clears, title rewrites). Rendering surfaces call
// StripC0C1 right before printing. Lives in clientops because that is the
// package both internal/cli and internal/tui may import (clientops must not be
// imported by mcpserver; the serve-side discovery responder sanitizes at
// message-construction time instead).
package clientops

import "strings"

// StripC0C1 removes every C0 control character (U+0000–U+001F — including \n,
// \r and \t: the output is always a single inline render line) and every C1
// control character plus DEL (U+007F–U+009F) from s. All other runes —
// including full-width CJK and the ⚠/· glyphs the pairing surfaces use — pass
// through untouched. Byte-range note: UTF-8 encodes U+0080–U+009F as two bytes,
// and ranging over runes (not bytes) is what makes the strip exact rather than
// a mojibake-prone byte filter.
func StripC0C1(s string) string {
	if s == "" {
		return s
	}
	needsStrip := false
	for _, r := range s {
		if r <= 0x1F || (r >= 0x7F && r <= 0x9F) {
			needsStrip = true
			break
		}
	}
	if !needsStrip {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r <= 0x1F || (r >= 0x7F && r <= 0x9F) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
