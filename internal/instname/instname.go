// Package instname validates device-code / cache-instance names (Plan 40 §2.1).
// One rule set shared by BOTH ends: the server rejects illegal names at
// cache-tokens add/bind (source gate), the client re-validates before any
// instance-directory write (defense) — the name becomes a directory/file name
// (instances/<name>/, cache-dek-<name>.key), so this closes path traversal and
// "dead on arrival" Windows filesystem forms.
package instname

import (
	"fmt"
	"regexp"
	"strings"
)

var pattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$`)

// dosReserved are Windows reserved device names. The check applies to the
// FIRST DOT-SEGMENT of the name (experiment-verified, spec §0.10): con.foo /
// COM1.x / nul.tar.gz pass a whole-name equality check but MkdirAll fails.
var dosReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Valid reports whether name is a legal device/instance name. The returned
// error text is standalone (no caller context needed) and always leads with
// "invalid device name" so wrapping call sites keep a stable grep anchor.
func Valid(name string) error {
	if !pattern.MatchString(name) {
		return fmt.Errorf("invalid device name %q: must be 1-64 chars matching ^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$ (letters/digits/dots/underscores/hyphens; alphanumeric first and last)", name)
	}
	seg := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		seg = name[:i]
	}
	if dosReserved[strings.ToUpper(seg)] {
		return fmt.Errorf("invalid device name %q: first dot-segment %q is a reserved device name on Windows (CON/PRN/AUX/NUL/COM1-9/LPT1-9)", name, seg)
	}
	return nil
}

// Fold lowercases ASCII letters only — the same casefold SQLite's lower()
// applies, which is what the server-side uniqueness queries rely on. Non-ASCII
// bytes pass through unchanged (a Unicode fold could merge legacy free-text
// names that Windows/SQLite treat as distinct).
func Fold(name string) string {
	b := []byte(name)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
