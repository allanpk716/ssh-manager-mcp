// Package sharemounttest 提供共享存储测试用的状态化 Linux 假服务器。
// 它经进程内 SSH 服务接收固定命令，维护持久配置、挂载状态与状态变化
// 历史，供 owner 侧管理模块的外部行为测试使用。
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
	LinuxUnmountMarker   = "SSHMGR-LINUX-UNMOUNT"
	LinuxReselectMarker  = "SSHMGR-LINUX-RESELECT"
	LinuxEnumerateMarker = "SSHMGR-LINUX-ENUM"
)

type BusyProcess struct {
	ProcessID string
	User      string
	Command   string
}

type LinuxServer struct {
	mu sync.Mutex

	ShareID      string
	ShareName    string
	ShareVersion int64
	// ReachableAddresses 模拟从目标服务器探测 TCP 445 成功的共享地址。
	ReachableAddresses []string

	Credential string
	FstabLines []string
	Mounted    bool

	RevokedMarker     string
	RevokedMarkerPath string
	History           []string
	Commands          []string
	Inputs            [][]byte

	BusyProcesses []BusyProcess
	LockHeld      bool
	ForceSucceeds bool
}

type LinuxServerState struct {
	Credential        string
	FstabLines        []string
	Mounted           bool
	RevokedMarker     string
	RevokedMarkerPath string
	History           []string
	Commands          []string
	StandardInputs    [][]byte
}

func (s *LinuxServer) Handler(command string, stdin io.Reader) (string, string, int) {
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
		return "Linux\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one"):
		return probeOutput(command, s.ReachableAddresses), "", 0
	case strings.Contains(command, LinuxReselectMarker):
		return s.reselect(command, input)
	case strings.Contains(command, LinuxEnumerateMarker):
		return s.enumerateOutput(), "", 0
	case strings.Contains(command, LinuxUnmountMarker):
		if s.LockHeld {
			return "", LinuxUnmountMarker + " lock-timeout", 87
		}
		if !s.hasManagedFstabEntry() {
			return "", LinuxUnmountMarker + " not-managed", 89
		}
		if strings.Contains(command, "sshmgr_reject_remote_version_newer=yes") &&
			s.ShareVersion > requestedVersion(command) {
			return "", LinuxUnmountMarker + " remote-version-newer", 91
		}
		s.deleteConfiguration()
		force := strings.Contains(command, "sshmgr_force=yes")
		if strings.Contains(command, "sshmgr_write_revoked_marker=yes") {
			s.writeRevokedMarker(command)
		}
		if strings.Contains(command, "sshmgr_skip_unmount=yes") {
			return s.unmountOutput("success"), "", 0
		}
		if !s.Mounted {
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
			return "", LinuxUnmountMarker + " force-unmount-failed", 85
		}
		s.unmount()
		return s.unmountOutput("success"), "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

func (s *LinuxServer) reselect(command string, input []byte) (string, string, int) {
	if s.LockHeld {
		return "", LinuxReselectMarker + " lock-timeout", 87
	}
	if !s.hasManagedFstabEntry() {
		return "", LinuxReselectMarker + " not-managed", 89
	}
	targetAddress := firstCandidateAddress(command)
	currentAddress := ""
	for _, line := range s.FstabLines {
		if strings.Contains(line, "sshmgr-share="+s.ShareName+",") ||
			strings.Contains(line, "sshmgr-share="+s.ShareName+";") {
			currentAddress = strings.SplitN(strings.SplitN(line, "//", 2)[1], "/", 2)[0]
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
		return "", LinuxReselectMarker + " force-unmount-failed", 85
	}
	s.unmount()
	password := strings.TrimSpace(string(input))
	s.FstabLines = []string{fmt.Sprintf(
		"//%s/%s /mnt/sshmgr/%s cifs credentials=/etc/sshmgr/shares/%s.credentials,vers=3.1.1,sshmgr-share=%s,sshmgr-version=%d,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0",
		targetAddress, s.ShareName, s.ShareName, s.ShareID, s.ShareName, s.ShareVersion,
	)}
	s.Credential = "username=mount-user\npassword=" + password + "\n"
	s.Mounted = true
	s.History = append(s.History, "write-configuration", "mount")
	return s.reselectOutput("update-success"), "", 0
}

func (s *LinuxServer) reselectOutput(result string) string {
	return fmt.Sprintf("%s result=%s selected=1 version=%d\n",
		LinuxReselectMarker, result, s.ShareVersion)
}

// enumerateOutput 按固定枚举协议汇报当前状态。枚举是只读操作：不改任何
// 字段，也不写状态变化历史。
func (s *LinuxServer) enumerateOutput() string {
	var out strings.Builder
	if s.hasManagedFstabEntry() {
		credentials := "no"
		if s.Credential != "" {
			credentials = "yes"
		}
		fmt.Fprintf(&out, "%s entry=%s version=%d credentials=%s\n",
			LinuxEnumerateMarker, url.PathEscape(s.ShareName), s.ShareVersion, credentials)
	}
	if s.RevokedMarker != "" {
		fmt.Fprintf(&out, "%s revoked=%s version=%d\n",
			LinuxEnumerateMarker, url.PathEscape(s.ShareName), s.ShareVersion)
	}
	if s.Mounted {
		fmt.Fprintf(&out, "%s mount=%s\n", LinuxEnumerateMarker, s.ShareName)
	}
	return out.String()
}

func requestedVersion(command string) int64 {
	for _, field := range strings.Fields(command) {
		if value, found := strings.CutPrefix(field, "sshmgr_version="); found {
			if parsed, err := strconv.ParseInt(strings.Trim(value, "'"), 10, 64); err == nil {
				return parsed
			}
		}
	}
	return 0
}

func (s *LinuxServer) State() LinuxServerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LinuxServerState{
		Credential:        s.Credential,
		FstabLines:        append([]string(nil), s.FstabLines...),
		Mounted:           s.Mounted,
		RevokedMarker:     s.RevokedMarker,
		RevokedMarkerPath: s.RevokedMarkerPath,
		History:           append([]string(nil), s.History...),
		Commands:          append([]string(nil), s.Commands...),
		StandardInputs:    append([][]byte(nil), s.Inputs...),
	}
}

func (s *LinuxServer) SeedManagedConfiguration(address string, version int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ShareVersion = version
	s.FstabLines = append(s.FstabLines, fmt.Sprintf(
		"//%s/%s /mnt/sshmgr/%s cifs credentials=/etc/sshmgr/shares/%s.credentials,vers=3.1.1,sshmgr-share=%s,sshmgr-version=%d,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0",
		address, s.ShareName, s.ShareName, s.ShareID, s.ShareName, version,
	))
	s.Credential = "username=mount-user\npassword=mount-password\n"
	s.Mounted = true
}

func (s *LinuxServer) hasManagedFstabEntry() bool {
	for _, line := range s.FstabLines {
		if strings.Contains(line, "sshmgr-share="+s.ShareName+";") ||
			strings.Contains(line, "sshmgr-share="+s.ShareName+",") {
			return true
		}
	}
	return false
}

func (s *LinuxServer) deleteConfiguration() {
	kept := make([]string, 0, len(s.FstabLines))
	for _, line := range s.FstabLines {
		if strings.Contains(line, "sshmgr-share="+s.ShareName+";") ||
			strings.Contains(line, "sshmgr-share="+s.ShareName+",") {
			continue
		}
		kept = append(kept, line)
	}
	s.FstabLines = kept
	s.Credential = ""
	s.History = append(s.History, "delete-configuration")
}

func (s *LinuxServer) writeRevokedMarker(command string) {
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
	s.RevokedMarkerPath = "/etc/sshmgr/shares/" + url.PathEscape(s.ShareName) + ".revoked"
	s.ShareVersion = version
	s.History = append(s.History, "write-revoked-marker")
}

func (s *LinuxServer) unmount() {
	s.Mounted = false
	s.History = append(s.History, "unmount")
}

func (s *LinuxServer) unmountOutput(result string) string {
	return fmt.Sprintf("%s result=%s version=%d\n", LinuxUnmountMarker, result, s.ShareVersion)
}
