//go:build windows

package cli

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// serveProcessRunningWindows reports whether the registered sshmgr-serve
// Windows service has a live process. Querying the service avoids tasklist's
// process-image-only output: an MCP bridge or another sshmgr subcommand must
// never make `serve status` report serve as running.
func serveProcessRunningWindows() bool {
	return windowsServeServiceProcessRunning(
		queryWindowsServeServiceProcessID, windowsProcessIsAlive,
	)
}

func queryWindowsServeServiceProcessID() (uint32, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, fmt.Errorf("open service control manager: %w", err)
	}
	defer windows.CloseServiceHandle(scm) //nolint:errcheck // cleanup only

	serviceName, err := syscall.UTF16PtrFromString(serveServiceName)
	if err != nil {
		return 0, err
	}
	service, err := windows.OpenService(scm, serviceName, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, fmt.Errorf("open service %s: %w", serveServiceName, err)
	}
	defer windows.CloseServiceHandle(service) //nolint:errcheck // cleanup only

	var status windows.SERVICE_STATUS_PROCESS
	needed := uint32(0)
	if err := windows.QueryServiceStatusEx(
		service,
		windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&status)),
		uint32(unsafe.Sizeof(status)),
		&needed,
	); err != nil {
		return 0, fmt.Errorf("query service %s status: %w", serveServiceName, err)
	}
	if status.ProcessId == 0 {
		return 0, fmt.Errorf("service %s has no process number", serveServiceName)
	}
	return status.ProcessId, nil
}

func windowsProcessIsAlive(processID uint32) bool {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return false
	}
	return windows.CloseHandle(process) == nil
}

// serveProcessRunningPOSIX is unreachable on Windows builds; the untagged
// serve_service.go dispatches via runtime.GOOS, so the linker needs a symbol
// with this name but it is never called on Windows. Keep a panic stub so a
// future code path that accidentally calls it surfaces clearly.
func serveProcessRunningPOSIX() bool { panic("unreachable on windows") }
