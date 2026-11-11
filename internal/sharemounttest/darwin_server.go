// Package sharemounttest 提供共享存储测试用的状态化 macOS 假服务器。
package sharemounttest

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	DarwinUnmountMarker  = "SSHMGR-DARWIN-UNMOUNT"
	DarwinReselectMarker = "SSHMGR-DARWIN-RESELECT"
)

type DarwinServer struct {
	mu sync.Mutex

	ShareName    string
	ShareVersion int64
	// ReachableAddresses 模拟从目标服务器探测 TCP 445 成功的共享地址。
	ReachableAddresses []string
	MapLines           []string
	AutoMasterLines    []string
	Mounted            bool

	RevokedMarker     string
	RevokedMarkerPath string
	History           []string
	Commands          []string
	Inputs            [][]byte

	BusyProcesses []BusyProcess
	LockHeld      bool
	ForceSucceeds bool
}

type DarwinServerState struct {
	MapLines          []string
	AutoMasterLines   []string
	Mounted           bool
	RevokedMarker     string
	RevokedMarkerPath string
	History           []string
	Commands          []string
	StandardInputs    [][]byte
}

func (s *DarwinServer) Handler(command string, stdin io.Reader) (string, string, int) {
	input, err := io.ReadAll(stdin)
	if err != nil {
		return "", "read standard input: " + err.Error(), 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Commands = append(s.Commands, command)
	s.Inputs = append(s.Inputs, append([]byte(nil), input...))

	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Darwin\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one"):
		return probeOutput(command, s.ReachableAddresses), "", 0
	case strings.Contains(command, DarwinReselectMarker):
		return s.reselect(command, input)
	case strings.Contains(command, DarwinUnmountMarker):
		if s.LockHeld {
			return "", DarwinUnmountMarker + " lock-timeout", 87
		}
		if !s.hasManagedMapEntry() {
			return "", DarwinUnmountMarker + " not-managed", 89
		}
		if strings.Contains(command, "sshmgr_reject_remote_version_newer=yes") &&
			s.ShareVersion > requestedVersion(command) {
			return "", DarwinUnmountMarker + " remote-version-newer", 91
		}
		s.deleteConfiguration()
		force := strings.Contains(command, "sshmgr_force=yes")
		if strings.Contains(command, "sshmgr_write_revoked_marker=yes") {
			s.writeRevokedMarker(command)
		}
		if strings.Contains(command, "sshmgr_skip_unmount=yes") {
			s.reloadAutomount()
			return s.unmountOutput("success"), "", 0
		}
		if !s.Mounted {
			s.reloadAutomount()
			return s.unmountOutput("success"), "", 0
		}
		if len(s.BusyProcesses) > 0 && !force {
			output := s.unmountOutput("busy")
			for _, process := range s.BusyProcesses {
				output += fmt.Sprintf("SSHMGR-PROCESS pid=%s user=%s command=%s\n",
					process.ProcessID, process.User, process.Command)
			}
			return output, "", 0
		}
		if len(s.BusyProcesses) > 0 && !s.ForceSucceeds {
			return "", DarwinUnmountMarker + " force-unmount-failed", 85
		}
		s.unmount()
		s.reloadAutomount()
		return s.unmountOutput("success"), "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

func (s *DarwinServer) reselect(command string, input []byte) (string, string, int) {
	if s.LockHeld {
		return "", DarwinReselectMarker + " lock-timeout", 87
	}
	if !s.hasManagedMapEntry() {
		return "", DarwinReselectMarker + " not-managed", 89
	}
	targetAddress := firstCandidateAddress(command)
	currentAddress := ""
	for _, line := range s.MapLines {
		if strings.HasPrefix(line, s.managedMarkerPrefix()) {
			continue
		}
		if strings.Contains(line, "@") {
			currentAddress = strings.SplitN(strings.SplitN(line, "@", 2)[1], "/", 2)[0]
			break
		}
	}
	if currentAddress == targetAddress {
		return s.reselectOutput("no-change"), "", 0
	}
	force := strings.Contains(command, "sshmgr_force=yes")
	if len(s.BusyProcesses) > 0 && !force {
		output := s.reselectOutput("busy")
		for _, process := range s.BusyProcesses {
			output += fmt.Sprintf("SSHMGR-PROCESS pid=%s user=%s command=%s\n",
				process.ProcessID, process.User, process.Command)
		}
		return output, "", 0
	}
	if len(s.BusyProcesses) > 0 && !s.ForceSucceeds {
		return "", DarwinReselectMarker + " force-unmount-failed", 85
	}
	s.unmount()
	password := strings.TrimSpace(string(input))
	s.MapLines = []string{
		fmt.Sprintf("# sshmgr-share=%s;sshmgr-version=%d", url.PathEscape(s.ShareName), s.ShareVersion),
		fmt.Sprintf("%s -fstype=smbfs ://mount-user:%s@%s/%s",
			s.ShareName, url.PathEscape(password), targetAddress, s.ShareName),
	}
	s.AutoMasterLines = append(s.AutoMasterLines, "/mnt/sshmgr /etc/sshmgr/auto_share")
	s.Mounted = true
	s.History = append(s.History, "write-configuration", "mount", "reload-automount")
	return s.reselectOutput("update-success"), "", 0
}

func (s *DarwinServer) reselectOutput(result string) string {
	return fmt.Sprintf("%s result=%s selected=1 version=%d\n",
		DarwinReselectMarker, result, s.ShareVersion)
}

func (s *DarwinServer) State() DarwinServerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return DarwinServerState{
		MapLines:          append([]string(nil), s.MapLines...),
		AutoMasterLines:   append([]string(nil), s.AutoMasterLines...),
		Mounted:           s.Mounted,
		RevokedMarker:     s.RevokedMarker,
		RevokedMarkerPath: s.RevokedMarkerPath,
		History:           append([]string(nil), s.History...),
		Commands:          append([]string(nil), s.Commands...),
		StandardInputs:    append([][]byte(nil), s.Inputs...),
	}
}

func (s *DarwinServer) SeedManagedConfiguration(address string, version int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ShareVersion = version
	s.MapLines = append(s.MapLines,
		fmt.Sprintf("# sshmgr-share=%s;sshmgr-version=%d", url.PathEscape(s.ShareName), version),
		fmt.Sprintf("%s -fstype=smbfs ://mount-user:mount-password@%s/%s", s.ShareName, address, s.ShareName),
	)
	s.AutoMasterLines = append(s.AutoMasterLines, "/mnt/sshmgr /etc/sshmgr/auto_share")
	s.Mounted = true
}

func (s *DarwinServer) managedMarkerPrefix() string {
	return "# sshmgr-share=" + url.PathEscape(s.ShareName) + ";"
}

func (s *DarwinServer) hasManagedMapEntry() bool {
	for _, line := range s.MapLines {
		if strings.HasPrefix(line, s.managedMarkerPrefix()) {
			return true
		}
	}
	return false
}

func (s *DarwinServer) deleteConfiguration() {
	kept := make([]string, 0, len(s.MapLines))
	skipEntry := false
	for _, line := range s.MapLines {
		if skipEntry {
			skipEntry = false
			continue
		}
		if strings.HasPrefix(line, s.managedMarkerPrefix()) {
			skipEntry = true
			continue
		}
		kept = append(kept, line)
	}
	s.MapLines = kept
	s.History = append(s.History, "delete-configuration")
}

func (s *DarwinServer) writeRevokedMarker(command string) {
	version := s.ShareVersion
	for _, field := range strings.Fields(command) {
		if value, found := strings.CutPrefix(field, "sshmgr_version="); found {
			if parsed, err := strconv.ParseInt(strings.Trim(value, "'"), 10, 64); err == nil {
				version = parsed
			}
		}
	}
	if version == 0 {
		version = s.ShareVersion + 1
	}
	s.RevokedMarker = fmt.Sprintf("sshmgr-share=%s;sshmgr-version=%d;sshmgr-state=revoked",
		url.PathEscape(s.ShareName), version)
	s.RevokedMarkerPath = "/etc/sshmgr/" + url.PathEscape(s.ShareName) + ".revoked"
	s.ShareVersion = version
	s.History = append(s.History, "write-revoked-marker")
}

func (s *DarwinServer) unmount() {
	s.Mounted = false
	s.History = append(s.History, "unmount")
}

func (s *DarwinServer) reloadAutomount() {
	if len(s.MapLines) == 0 {
		kept := make([]string, 0, len(s.AutoMasterLines))
		for _, line := range s.AutoMasterLines {
			if line == "/mnt/sshmgr /etc/sshmgr/auto_share" {
				continue
			}
			kept = append(kept, line)
		}
		s.AutoMasterLines = kept
	}
	s.History = append(s.History, "reload-automount")
}

func (s *DarwinServer) unmountOutput(result string) string {
	return fmt.Sprintf("%s result=%s version=%d\n", DarwinUnmountMarker, result, s.ShareVersion)
}
