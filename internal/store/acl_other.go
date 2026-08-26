//go:build !windows

package store

// HardenACL is a no-op on non-Windows platforms: Unix uses file mode bits
// (0600 set by the caller's os.OpenFile / WriteFile) plus directory 0700 for
// protection (spec §5.2). Returns nil so FileKeyProvider.Set can call it
// unconditionally without a runtime.GOOS branch at every call site (which
// would break cross-compilation — HardenACL is only defined under the
// windows build tag).
func HardenACL(path string) error { return nil }

// InspectFileACL reports Supported=false on non-Windows: mode bits are that
// platform's protection layer (see HardenACL's note). Callers branch on
// Supported instead of runtime.GOOS (spec §1.5).
func InspectFileACL(path string) (FileACLReport, error) {
	return FileACLReport{Supported: false}, nil
}
