//go:build !windows

package paths

// vaultRoot returns the platform data root (Linux/macOS: /var/lib).
// See spec §3.1. macOS uses /var/lib too (not Homebrew-tied) — xcheck consensus F.
func vaultRoot() (string, error) {
	return "/var/lib", nil
}
