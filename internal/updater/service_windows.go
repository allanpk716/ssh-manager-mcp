//go:build windows

// SCM bridge for the update path pre-check (spec §4.4): read the registered
// binary path of a Windows service straight from the Service Control Manager
// API. Deliberately NOT `sc qc` text parsing — localized output breaks on
// zh-CN hosts (FINDING E lesson, carried forward from Plan 16).
package updater

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() {
	// Wire the windows-only SCM implementation into the platform seam that
	// service.go's RegisteredBinaryPath dispatches to.
	scmQueryBinaryPath = windowsRegisteredBinaryPath
}

// windowsRegisteredBinaryPath opens the service with SERVICE_QUERY_CONFIG
// (read-only, no elevation required) and returns the executable path parsed
// out of lpBinaryPathName. Arguments the service was installed with (e.g.
// `serve --addr 0.0.0.0:7878`) are stripped by parseWindowsBinaryPath.
func windowsRegisteredBinaryPath(name string) (string, error) {
	mgr, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", fmt.Errorf("open service control manager: %w", err)
	}
	// SCM handles close via CloseServiceHandle (NOT windows.CloseHandle —
	// different handle table).
	defer windows.CloseServiceHandle(mgr)

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", fmt.Errorf("service name %q: %w", name, err)
	}
	svc, err := windows.OpenService(mgr, namePtr, windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return "", fmt.Errorf("open service %q: %w", name, err)
	}
	defer windows.CloseServiceHandle(svc)

	// Sizing call: nil buffer + 0 size fails with ERROR_INSUFFICIENT_BUFFER
	// and stores the required byte count in need.
	var need uint32
	sizingErr := windows.QueryServiceConfig(svc, nil, 0, &need)
	if need == 0 {
		return "", fmt.Errorf("size QueryServiceConfig for service %q: err=%v, needed=0", name, sizingErr)
	}

	buf := make([]byte, need)
	cfg := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
	if err := windows.QueryServiceConfig(svc, cfg, need, &need); err != nil {
		return "", fmt.Errorf("query service config for service %q: %w", name, err)
	}

	exe := parseWindowsBinaryPath(windows.UTF16PtrToString(cfg.BinaryPathName))
	if exe == "" {
		return "", fmt.Errorf("service %q reports an empty BinaryPathName", name)
	}
	return exe, nil
}
