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
	DarwinUnmountMarker   = "SSHMGR-DARWIN-UNMOUNT"
	DarwinEnumerateMarker = "SSHMGR-DARWIN-ENUM"
)

type DarwinServer struct {
	mu sync.Mutex

	ShareName       string
	ShareVersion    int64
	MapLines        []string
	AutoMasterLines []string
	Mounted         bool

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
	case strings.Contains(command, DarwinEnumerateMarker):
		return s.enumerateOutput(), "", 0
	case strings.Contains(command, DarwinUnmountMarker):
		if s.LockHeld {
			return "", DarwinUnmountMarker + " lock-timeout", 87
		}
		if !s.hasManagedMapEntry() {
			return "", DarwinUnmountMarker + " not-managed", 89
		}
		s.deleteConfiguration()
		force := strings.Contains(command, "sshmgr_force=yes")
		if strings.Contains(command, "sshmgr_write_revoked_marker=yes") {
			s.writeRevokedMarker(command)
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

// enumerateOutput 按固定枚举协议汇报当前状态。枚举是只读操作：不改任何
// 字段，也不写状态变化历史。
func (s *DarwinServer) enumerateOutput() string {
	var out strings.Builder
	if s.hasManagedMapEntry() {
		fmt.Fprintf(&out, "%s entry=%s version=%d\n",
			DarwinEnumerateMarker, url.PathEscape(s.ShareName), s.ShareVersion)
	}
	if s.RevokedMarker != "" {
		fmt.Fprintf(&out, "%s revoked=%s version=%d\n",
			DarwinEnumerateMarker, url.PathEscape(s.ShareName), s.ShareVersion)
	}
	if s.Mounted {
		fmt.Fprintf(&out, "%s mount=%s\n", DarwinEnumerateMarker, s.ShareName)
	}
	return out.String()
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
