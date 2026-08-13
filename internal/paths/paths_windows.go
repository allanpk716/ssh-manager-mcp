package paths

// vaultRoot returns the platform data root (Windows: ProgramData).
// See spec §3.1.
func vaultRoot() (string, error) {
	return "C:\\ProgramData", nil
}
