package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

const mountShareTestPassword = `mount-P@$$ 'quotes' "double" \ end`
const mountShareTestPasswordURLEncoded = `mount-P%40%24%24%20%27quotes%27%20%22double%22%20%5C%20end`

type mountShareFakeServer interface {
	handler(command string, stdin io.Reader) (string, string, int)
	recorded() (commands []string, inputs [][]byte, credential string, configuration []string)
	writeCounts() (credentialWrites int, configurationWrites int)
	remountCount() int
	setShareMetadata(shareID string)
	setShareVersion(version int64)
}

type fakeLinuxMountServer struct {
	mu sync.Mutex

	commands            []string
	standardInputs      [][]byte
	credentialFile      string
	fstabLines          []string
	credentialWrites    int
	configurationWrites int
	remounts            int
	mounted             bool
	// mountTableMissing 模拟挂载点目录可以列出、但挂载表里没有对应
	// cifs 文件系统：挂载工具的最终验证必须失败。
	mountTableMissing  bool
	reachableAddresses map[string]bool
	existingAddress    string
	existingProtocol   string
	remoteVersion      int64
	ownerFstabLines    []string
	legacyFstabMarker  bool
	lockHeld           bool
	staleLock          bool
	// revokedMarkerVersion 为 0 表示没有撤销标记；非 0 表示服务器上存在
	// 这个版本的撤销标记。
	revokedMarkerVersion int64

	missingMountCIFS bool
	workingVersion   string
	shareVersion     int64
	shareID          string
	probeOutput      string
}

func (f *fakeLinuxMountServer) handler(command string, stdin io.Reader) (string, string, int) {
	stdinBytes, err := io.ReadAll(stdin)
	if err != nil {
		return "", "read standard input: " + err.Error(), 1
	}
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.standardInputs = append(f.standardInputs, append([]byte(nil), stdinBytes...))
	f.mu.Unlock()

	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Linux\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one "):
		if f.probeOutput != "" {
			return f.probeOutput, "", 0
		}
		probeOutput := strings.Builder{}
		for index, address := range []string{"192.0.2.10", "100.64.10.10"} {
			if strings.Contains(command, "sshmgr_probe_one "+fmt.Sprint(index)+" "+address+" ") {
				if f.reachableAddresses[address] {
					probeOutput.WriteString(fmt.Sprintf("SSHMGR-PROBE %d ok 2 \n", index))
				} else {
					probeOutput.WriteString(fmt.Sprintf("SSHMGR-PROBE %d fail 2 no-route\n", index))
				}
			}
		}
		return probeOutput.String(), "", 0
	case strings.Contains(command, "SSHMGR-LINUX-MOUNT"):
		if f.missingMountCIFS {
			return "", "sshmgr: mount.cifs not found", 83
		}
		selectedVersion := f.workingVersion
		if selectedVersion == "" {
			selectedVersion = "3.1.1"
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.staleLock {
			for _, want := range []string{
				"sshmgr_lock_dir='/var/run/sshmgr-mount.lock'",
				`date +%s > "$sshmgr_lock_owner"`,
				`rm -f "$sshmgr_lock_owner"`,
				`rmdir "$sshmgr_lock_dir"`,
			} {
				if !strings.Contains(command, want) {
					return "", "过期锁回收片段缺少 " + want, 91
				}
			}
		}
		// 变更路径必须先在临时挂载点上试挂；全部协议失败时要在触碰
		// 正式挂载点之前退出。
		trialExit := strings.Index(command, "protocol-failed")
		realUnmount := strings.Index(command, "\nsshmgr_prepare_remount\n")
		if !strings.Contains(command, "/var/run/sshmgr-trial-datasets") ||
			!strings.Contains(command, `umount "$sshmgr_trial_point"`) ||
			trialExit < 0 || realUnmount < 0 || trialExit > realUnmount {
			return "", "Linux 固定命令必须先在临时挂载点试挂，全部失败时不得触碰正式挂载点", 91
		}
		if selectedVersion == "none" {
			return "", "SSHMGR-LINUX-MOUNT protocol-failed", 84
		}
		if f.mountTableMissing && !strings.Contains(command, "/proc/mounts") {
			return "", "Linux 固定命令的最终验证必须核验 /proc/mounts 挂载表", 91
		}
		if f.mountTableMissing && !strings.Contains(command, "/proc/mounts") {
			return "", "Linux 固定命令的最终验证必须核验 /proc/mounts 挂载表", 91
		}
		if f.lockHeld {
			return "", "SSHMGR-LINUX-MOUNT lock-timeout", 87
		}
		if f.remoteVersion > f.shareVersion {
			return "", "SSHMGR-LINUX-MOUNT remote-version-newer", 88
		}
		if f.revokedMarkerVersion != 0 {
			if !strings.Contains(command, "sshmgr_revoked_marker='/etc/sshmgr/shares/datasets.revoked'") ||
				!strings.Contains(command, `-ge "$sshmgr_expected_version"`) {
				return "", "Linux 固定挂载命令必须在持锁后读取撤销标记", 91
			}
			if f.revokedMarkerVersion >= f.shareVersion {
				return "", "SSHMGR-LINUX-MOUNT share-revoked", 90
			}
			if !strings.Contains(command, `rm -f "$sshmgr_revoked_marker"`) {
				return "", "Linux 固定挂载命令成功后必须删除旧撤销标记", 91
			}
			f.revokedMarkerVersion = 0
		}
		wasExisting := f.existingAddress != ""
		selectedAddress := "192.0.2.10"
		selectedIndex := 1
		if f.existingAddress == "100.64.10.10" && f.reachableAddresses[f.existingAddress] {
			selectedAddress = "100.64.10.10"
			if f.reachableAddresses["192.0.2.10"] {
				selectedIndex = 2
			}
		} else if !f.reachableAddresses["192.0.2.10"] && f.reachableAddresses["100.64.10.10"] {
			selectedAddress = "100.64.10.10"
			selectedIndex = 1
		}
		needsChange := f.existingAddress != selectedAddress || f.remoteVersion != f.shareVersion ||
			!f.mounted || !f.containsCurrentManagedFstabMarker()
		if !needsChange && len(f.fstabLines) > 0 {
			return fmt.Sprintf("SSHMGR-LINUX-MOUNT result=no-change selected=%d\n", selectedIndex), "", 0
		}
		// 变更路径总是先写临时凭据试挂，试挂成功后原子替换正式凭据文件。
		f.credentialFile = "username=mount-user\npassword=" + string(stdinBytes)
		f.credentialWrites++
		if selectedAddress != f.existingAddress {
			selectedVersion = f.workingVersion
			if selectedVersion == "" {
				selectedVersion = "3.1.1"
			}
		} else {
			selectedVersion = f.existingProtocol
			if f.workingVersion != "" && f.workingVersion != f.existingProtocol {
				// 旧协议在临时挂载点试挂失败后，按协议版本序列降级。
				selectedVersion = f.workingVersion
			}
		}
		f.configurationWrites++
		f.remounts++
		if f.mountTableMissing {
			return "", "SSHMGR-LINUX-MOUNT verify-failed", 86
		}
		f.existingAddress = selectedAddress
		f.existingProtocol = selectedVersion
		f.remoteVersion = f.shareVersion
		f.mounted = true
		updatedLines := make([]string, 0, len(f.fstabLines))
		replacedManagedLine := false
		managedLine := f.managedFstabLine(selectedAddress, selectedVersion, f.shareVersion)
		for _, line := range f.fstabLines {
			if !strings.Contains(line, "sshmgr-share=datasets,") {
				updatedLines = append(updatedLines, line)
			} else if !replacedManagedLine {
				updatedLines = append(updatedLines, managedLine)
				replacedManagedLine = true
			}
		}
		if !replacedManagedLine {
			updatedLines = append(updatedLines, managedLine)
		}
		f.fstabLines = updatedLines
		action := "update-success"
		if !wasExisting {
			action = "first-mount-success"
		}
		return fmt.Sprintf("SSHMGR-LINUX-MOUNT result=%s selected=%d\n", action, selectedIndex), "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

func (f *fakeLinuxMountServer) containsCurrentManagedFstabMarker() bool {
	for _, line := range f.fstabLines {
		if strings.Contains(line, ",x-sshmgr-share=datasets,x-sshmgr-version=") {
			return true
		}
	}
	return false
}

func (f *fakeLinuxMountServer) recorded() (commands []string, inputs [][]byte, credential string, fstab []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), append([][]byte(nil), f.standardInputs...), f.credentialFile, append([]string(nil), f.fstabLines...)
}

func (f *fakeLinuxMountServer) writeCounts() (credentialWrites int, configurationWrites int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credentialWrites, f.configurationWrites
}

func (f *fakeLinuxMountServer) remountCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.remounts
}

func (f *fakeLinuxMountServer) setShareVersion(version int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shareVersion = version
}

func (f *fakeLinuxMountServer) setShareMetadata(shareID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shareID = shareID
}

func (f *fakeLinuxMountServer) managedFstabLine(address, protocolVersion string, version int64) string {
	return "//" + address + "/datasets /mnt/sshmgr/datasets cifs " +
		"credentials=/etc/sshmgr/shares/" + f.shareID + ".credentials,vers=" + protocolVersion +
		",x-sshmgr-share=datasets,x-sshmgr-version=" + fmt.Sprint(version) +
		",_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0"
}

type fakeDarwinMountServer struct {
	mu sync.Mutex

	commands           []string
	standardInputs     [][]byte
	autoMasterLines    []string
	mapLines           []string
	mapOwner           string
	mapMode            string
	autoMasterWrites   int
	mapWrites          int
	automountRefreshes int
	mountPointAccesses int
	remounts           int
	mounted            bool
	// mountTableMissing 模拟挂载点目录可以列出、但挂载表里没有对应
	// cifs 文件系统：挂载工具的最终验证必须失败。
	mountTableMissing   bool
	mountTableFirmlink  bool
	unmounts            int
	unmountFails        bool
	reachableAddresses  map[string]bool
	existingAddress     string
	remoteVersion       int64
	shareVersion        int64
	shareID             string
	missingAutomount    bool
	lockHeld            bool
	staleLock           bool
	autoMasterReference bool
	// revokedMarkerVersion 为 0 表示没有撤销标记；非 0 表示服务器上存在
	// 这个版本的撤销标记。
	revokedMarkerVersion int64
}

func (f *fakeDarwinMountServer) handler(command string, stdin io.Reader) (string, string, int) {
	stdinBytes, err := io.ReadAll(stdin)
	if err != nil {
		return "", "read standard input: " + err.Error(), 1
	}
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.standardInputs = append(f.standardInputs, append([]byte(nil), stdinBytes...))
	f.mu.Unlock()

	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Darwin\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one "):
		probeOutput := strings.Builder{}
		for index, address := range []string{"192.0.2.10", "100.64.10.10"} {
			if strings.Contains(command, "sshmgr_probe_one "+fmt.Sprint(index)+" "+address+" ") {
				if f.reachableAddresses[address] {
					probeOutput.WriteString(fmt.Sprintf("SSHMGR-PROBE %d ok 2 \n", index))
				} else {
					probeOutput.WriteString(fmt.Sprintf("SSHMGR-PROBE %d fail 2 no-route\n", index))
				}
			}
		}
		return probeOutput.String(), "", 0
	case strings.Contains(command, "SSHMGR-DARWIN-MOUNT"):
		if f.missingAutomount {
			return "", "SSHMGR-DARWIN-MOUNT missing-automount", 83
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.staleLock {
			for _, want := range []string{
				"sshmgr_lock_dir='/var/run/sshmgr-mount.lock'",
				`date +%s > "$sshmgr_lock_owner"`,
				`rm -f "$sshmgr_lock_owner"`,
				`rmdir "$sshmgr_lock_dir"`,
			} {
				if !strings.Contains(command, want) {
					return "", "过期锁回收片段缺少 " + want, 91
				}
			}
		}
		if f.lockHeld {
			return "", "SSHMGR-DARWIN-MOUNT lock-timeout", 87
		}
		if f.remoteVersion > f.shareVersion {
			return "", "SSHMGR-DARWIN-MOUNT remote-version-newer", 88
		}
		if f.revokedMarkerVersion != 0 {
			if !strings.Contains(command, "sshmgr_revoked_marker='/etc/sshmgr/datasets.revoked'") ||
				!strings.Contains(command, `-ge "$sshmgr_expected_version"`) {
				return "", "macOS 固定挂载命令必须在持锁后读取撤销标记", 91
			}
			if f.revokedMarkerVersion >= f.shareVersion {
				return "", "SSHMGR-DARWIN-MOUNT share-revoked", 90
			}
			if !strings.Contains(command, `rm -f "$sshmgr_revoked_marker"`) {
				return "", "macOS 固定挂载命令成功后必须删除旧撤销标记", 91
			}
			f.revokedMarkerVersion = 0
		}
		if f.mountTableMissing && !strings.Contains(command, `(smbfs`) {
			return "", "macOS 固定命令的最终验证必须核验 mount 输出挂载表", 91
		}
		if f.mountTableFirmlink {
			for _, want := range []string{
				`sshmgr_data_mount_point="/System/Volumes/Data$mount_point"`,
				`grep -qF " on $sshmgr_data_mount_point (smbfs"`,
			} {
				if !strings.Contains(command, want) {
					return "", "macOS 固定命令的挂载表核验必须接受数据卷 firmlink 路径", 91
				}
			}
		}
		selectedAddress := "192.0.2.10"
		selectedIndex := 1
		if f.existingAddress == "100.64.10.10" && f.reachableAddresses[f.existingAddress] {
			selectedAddress = "100.64.10.10"
			if f.reachableAddresses["192.0.2.10"] {
				selectedIndex = 2
			}
		} else if !f.reachableAddresses["192.0.2.10"] && f.reachableAddresses["100.64.10.10"] {
			selectedAddress = "100.64.10.10"
		}
		wasExisting := f.existingAddress != ""
		resetAutoMaster := !f.autoMasterReference
		configurationMatches := wasExisting && f.remoteVersion == f.shareVersion &&
			f.existingAddress == selectedAddress && f.mounted
		if configurationMatches && !resetAutoMaster {
			return fmt.Sprintf("SSHMGR-DARWIN-MOUNT result=no-change selected=%d\n", selectedIndex), "", 0
		}
		mapRewritten := wasExisting && !configurationMatches
		if mapRewritten && f.mounted {
			for _, want := range []string{
				`umount "$mount_point"`,
				`diskutil unmount force "$mount_point"`,
			} {
				if !strings.Contains(command, want) {
					return "", "macOS 固定命令缺少卸载旧挂载步骤 " + want, 91
				}
			}
			if f.unmountFails {
				return "", "SSHMGR-DARWIN-MOUNT unmount-failed", 85
			}
			f.unmounts++
			f.mounted = false
		}
		if !configurationMatches {
			updatedMap := make([]string, 0, len(f.mapLines)+2)
			currentMarker := fmt.Sprintf("# sshmgr-share=datasets;sshmgr-version=%d", f.shareVersion)
			currentEntry := fmt.Sprintf("datasets -fstype=smbfs ://mount-user:%s@%s/datasets",
				mountShareTestPasswordURLEncoded, selectedAddress)
			replacedMarker := false
			skipEntry := false
			for _, line := range f.mapLines {
				if skipEntry {
					skipEntry = false
					continue
				}
				if strings.HasPrefix(line, "# sshmgr-share=datasets;sshmgr-version=") {
					updatedMap = append(updatedMap, currentMarker, currentEntry)
					replacedMarker = true
					skipEntry = true
					continue
				}
				updatedMap = append(updatedMap, line)
			}
			if !replacedMarker {
				updatedMap = append(updatedMap, currentMarker, currentEntry)
			}
			f.mapLines = updatedMap
			f.mapOwner = "root"
			f.mapMode = "600"
			f.mapWrites++
			f.existingAddress = selectedAddress
			f.remoteVersion = f.shareVersion
		}
		if resetAutoMaster {
			f.autoMasterReference = true
			f.autoMasterLines = append(f.autoMasterLines, "/mnt/sshmgr /etc/sshmgr/auto_share")
			f.autoMasterWrites++
		}
		f.automountRefreshes++
		f.mountPointAccesses++
		if f.mountTableMissing {
			return "", "SSHMGR-DARWIN-MOUNT verify-failed", 86
		}
		f.mounted = true
		f.remounts++
		action := "update-success"
		if !wasExisting {
			action = "first-mount-success"
		} else if configurationMatches {
			action = "remount-success"
		}
		return fmt.Sprintf("SSHMGR-DARWIN-MOUNT result=%s selected=%d reset=%s\n",
			action, selectedIndex, boolString(resetAutoMaster)), "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

func boolString(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// assertMountShareAuditResult 断言最近一条 mount-share 审计的固定结果
// 分类；分类不由错误文本推导，便于审查失败原因。
func assertMountShareAuditResult(t *testing.T, st *store.Store, wantResult string) map[string]any {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatalf("没有 mount-share 审计行，期待分类 %q", wantResult)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[len(rows)-1].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != wantResult {
		t.Fatalf("mount-share 审计分类 = %v，期待 %q（摘要 %#v）", summary["result"], wantResult, summary)
	}
	return summary
}

func (f *fakeDarwinMountServer) recorded() ([]string, [][]byte, string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	configuration := append(append([]string(nil), f.autoMasterLines...), f.mapLines...)
	return append([]string(nil), f.commands...), append([][]byte(nil), f.standardInputs...), "", configuration
}

func (f *fakeDarwinMountServer) writeCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mapWrites, f.autoMasterWrites
}

func (f *fakeDarwinMountServer) remountCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.remounts
}

func (f *fakeDarwinMountServer) setShareVersion(version int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shareVersion = version
}

func (f *fakeDarwinMountServer) setShareMetadata(shareID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shareID = shareID
}

func (f *fakeDarwinMountServer) recordedDarwin() (commands []string, inputs [][]byte, autoMaster []string, mapLines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...), append([][]byte(nil), f.standardInputs...),
		append([]string(nil), f.autoMasterLines...), append([]string(nil), f.mapLines...)
}

func (f *fakeDarwinMountServer) resetAutoMasterReference() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoMasterReference = false
	updated := make([]string, 0, len(f.autoMasterLines))
	for _, line := range f.autoMasterLines {
		if line != "/mnt/sshmgr /etc/sshmgr/auto_share" {
			updated = append(updated, line)
		}
	}
	f.autoMasterLines = updated
}

func (f *fakeDarwinMountServer) darwinState() (owner string, mode string, refreshes int, accesses int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mapOwner, f.mapMode, f.automountRefreshes, f.mountPointAccesses
}

func (f *fakeDarwinMountServer) unmountCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unmounts
}

type mountShareFixture struct {
	store     *store.Store
	profileID string
	targetID  string
	shareID   string
	session   *mcp.ClientSession
	fake      mountShareFakeServer
}

type mountShareSeed struct {
	store     *store.Store
	profileID string
	targetID  string
	shareID   string
	fake      mountShareFakeServer
}

// seedMountShareVault 建立共享存储挂载测试共用的权威端数据：提供方与目标
// 设备、两个共同网络、目标服务器条目与凭据、挂载密码机密项、共享登记和
// 授权。单机工具面与客户端缓存面都在这份种子上继续装配。
func seedMountShareVault(t *testing.T, setup mountShareFixtureSetup) mountShareSeed {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "mount-share-test.db")
	storeForTest := newStoreAt(t, databasePath)
	var fake mountShareFakeServer
	if setup.darwinTarget {
		darwinFake := &fakeDarwinMountServer{
			shareVersion: 1, remoteVersion: setup.darwinRemoteVersion,
			reachableAddresses: map[string]bool{"192.0.2.10": true},
			existingAddress:    setup.darwinExistingAddress,
			missingAutomount:   setup.darwinMissingAutomount, lockHeld: setup.lockHeld,
			staleLock: setup.staleLock, mountTableMissing: setup.mountTableMissing,
			mountTableFirmlink: setup.darwinMountTableFirmlink,
			autoMasterLines: []string{
				"# Automounter master map",
				"/System/Volumes/Data /System/Volumes/Data auto_databackup",
			},
		}
		if len(setup.reachable) != 0 {
			darwinFake.reachableAddresses = make(map[string]bool, len(setup.reachable))
			for _, address := range setup.reachable {
				darwinFake.reachableAddresses[address] = true
			}
		}
		if darwinFake.existingAddress != "" {
			darwinFake.mounted = true
		}
		fake = darwinFake
	} else {
		linuxFake := &fakeLinuxMountServer{
			missingMountCIFS: setup.missingMountCIFS, workingVersion: setup.workingVersion,
			shareVersion: 1, remoteVersion: setup.remoteVersion, probeOutput: setup.probeOutput,
			reachableAddresses: map[string]bool{"192.0.2.10": true},
			existingAddress:    setup.existingAddress, existingProtocol: setup.existingProtocol,
			ownerFstabLines: append([]string(nil), setup.ownerFstabLines...), lockHeld: setup.lockHeld,
			staleLock: setup.staleLock, mountTableMissing: setup.mountTableMissing,
			legacyFstabMarker: setup.legacyFstabMarker,
		}
		if len(setup.reachable) != 0 {
			linuxFake.reachableAddresses = make(map[string]bool, len(setup.reachable))
			for _, address := range setup.reachable {
				linuxFake.reachableAddresses[address] = true
			}
		}
		if strings.Contains(setup.probeOutput, "SSHMGR-PROBE 0 fail") {
			linuxFake.reachableAddresses["192.0.2.10"] = false
		}
		if strings.Contains(setup.probeOutput, "SSHMGR-PROBE 1 ok") {
			linuxFake.reachableAddresses["100.64.10.10"] = true
		}
		if linuxFake.existingAddress != "" {
			if linuxFake.existingProtocol == "" {
				linuxFake.existingProtocol = "2.0"
			}
			linuxFake.mounted = true
		}
		fake = linuxFake
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)

	_, err := storeForTest.AddDevice("NAS", "共享存储提供方", "")
	if err != nil {
		t.Fatal(err)
	}
	targetDeviceName := "linux-device"
	if setup.darwinTarget {
		targetDeviceName = "darwin-device"
	}
	targetServerName := "linux-web"
	if setup.darwinTarget {
		targetServerName = "darwin-web"
	}
	_, err = storeForTest.AddDevice(targetDeviceName, "挂载目标", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddNetwork(models.Network{Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddNetwork(models.Network{Name: "办公虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if setup.targetNoCommonNetwork {
		if _, err := storeForTest.AddNetwork(models.Network{Name: "目标独立虚拟局域网", Type: models.NetworkVirtual}); err != nil {
			t.Fatal(err)
		}
	}
	for _, membership := range []struct{ network, address string }{
		{"办公物理局域网", "192.0.2.10"},
		{"办公虚拟局域网", "100.64.10.10"},
	} {
		if _, err := storeForTest.AddNetworkMembership("NAS", membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	targetNetworks := []struct{ network, address string }{
		{"办公物理局域网", "192.0.2.20"}, {"办公虚拟局域网", "100.64.10.20"},
	}
	if setup.targetNoCommonNetwork {
		targetNetworks = []struct{ network, address string }{{"目标独立虚拟局域网", "198.51.100.20"}}
	}
	for _, membership := range targetNetworks {
		if _, err := storeForTest.AddNetworkMembership(targetDeviceName, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}

	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	var sudoCredential *models.Credential
	if setup.sudo {
		sudoCredential = &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	}
	server := &models.Server{
		Name: targetServerName, Host: addr[:strings.LastIndex(addr, ":")],
		Port: portOfAddr(addr), User: "deploy", ShellType: models.ShellPOSIX,
	}
	added, err := storeForTest.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, targetDeviceName)
	if err != nil {
		t.Fatal(err)
	}
	targetID := added.ServerID
	if setup.targetWithoutDevice {
		rawDatabase, err := sql.Open("sqlite", databasePath)
		if err != nil {
			t.Fatal(err)
		}
		_, unlinkErr := rawDatabase.Exec(`UPDATE servers SET device_id=NULL WHERE id=?`, targetID)
		closeErr := rawDatabase.Close()
		if unlinkErr != nil || closeErr != nil {
			t.Fatalf("解除服务器设备关联失败：unlink=%v close=%v", unlinkErr, closeErr)
		}
	}
	if err := storeForTest.SaveEntryPin(serverByAddr(storeForTest, server.Host, server.Port), hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}

	profileID, err := storeForTest.AddProfile("共享挂载 profile")
	if err != nil {
		t.Fatal(err)
	}
	granted := []string{targetID}
	if setup.targetOutOfProfile || setup.notWritable {
		visibleID, err := storeForTest.AddServer(&models.Server{
			Name: "visible-peer", Host: "192.0.2.31", Port: 22, User: "deploy",
			AuthMethod: models.AuthPassword,
		})
		if err != nil {
			t.Fatal(err)
		}
		granted = []string{visibleID}
		if setup.notWritable {
			granted = append(granted, targetID)
		}
	}
	if err := storeForTest.GrantServers(profileID, granted); err != nil {
		t.Fatal(err)
	}
	if err := storeForTest.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码", []byte(mountShareTestPassword), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	allowedTargets := []string{targetServerName}
	if setup.notWritable {
		allowedTargets = []string{"visible-peer"}
	}
	if err := storeForTest.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, allowedTargets, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := storeForTest.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "数据集", Path: "datasets"},
		},
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	addedShare, err := storeForTest.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	fake.setShareMetadata(addedShare.ID)
	if linuxFake, ok := fake.(*fakeLinuxMountServer); ok && linuxFake.existingAddress != "" {
		linuxFake.fstabLines = append([]string(nil), linuxFake.ownerFstabLines...)
		managedLine := linuxFake.managedFstabLine(
			linuxFake.existingAddress, linuxFake.existingProtocol, linuxFake.remoteVersion,
		)
		if linuxFake.legacyFstabMarker {
			managedLine = strings.ReplaceAll(managedLine, ",x-sshmgr-", ",sshmgr-")
		}
		if len(linuxFake.fstabLines) > 1 {
			linuxFake.fstabLines = append(linuxFake.fstabLines[:1],
				append([]string{managedLine}, linuxFake.fstabLines[1:]...)...)
		} else {
			linuxFake.fstabLines = append(linuxFake.fstabLines, managedLine)
		}
		linuxFake.credentialFile = "username=mount-user\npassword=old-password\n"
	}
	if darwinFake, ok := fake.(*fakeDarwinMountServer); ok && darwinFake.existingAddress != "" {
		darwinFake.autoMasterReference = true
		darwinFake.autoMasterLines = append(darwinFake.autoMasterLines, "/mnt/sshmgr /etc/sshmgr/auto_share")
		darwinFake.mapLines = []string{
			fmt.Sprintf("# sshmgr-share=datasets;sshmgr-version=%d", darwinFake.remoteVersion),
			fmt.Sprintf("datasets -fstype=smbfs ://mount-user:old-password@%s/datasets",
				darwinFake.existingAddress),
		}
	}
	if setup.shareInvisible {
		if err := storeForTest.DisallowSecrets(
			[]string{"DATASETS_MOUNT_PASSWORD"}, []string{targetServerName}, store.CLISource(),
		); err != nil {
			t.Fatal(err)
		}
	}
	if setup.windowsTarget {
		windowsCredential, err := storeForTest.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")})
		if err != nil {
			t.Fatal(err)
		}
		windowsID, err := storeForTest.AddServer(&models.Server{
			Name: "windows-web", Host: "192.0.2.30", Port: 22, User: "deploy",
			AuthMethod: models.AuthPassword, CredentialID: windowsCredential,
			ShellType: models.ShellPowerShell,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := storeForTest.GrantServers(profileID, []string{windowsID}); err != nil {
			t.Fatal(err)
		}
		if err := storeForTest.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, []string{"windows-web"}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
		targetID = windowsID
	}
	return mountShareSeed{
		store: storeForTest, profileID: profileID, targetID: targetID,
		shareID: addedShare.ID, fake: fake,
	}
}

func newMountShareFixture(t *testing.T, options ...func(*mountShareFixtureSetup)) mountShareFixture {
	t.Helper()
	setup := mountShareFixtureSetup{sudo: true}
	for _, option := range options {
		option(&setup)
	}
	seed := seedMountShareVault(t, setup)
	serverFace, tunnels, tasks, err := NewServer(seed.store, seed.profileID, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tunnels.CloseAll)
	t.Cleanup(tasks.CloseAll)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := serverFace.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return mountShareFixture{
		store: seed.store, profileID: seed.profileID, targetID: seed.targetID,
		shareID: seed.shareID, session: clientSession, fake: seed.fake,
	}
}

type mountShareFixtureSetup struct {
	sudo                     bool
	missingMountCIFS         bool
	workingVersion           string
	probeOutput              string
	reachable                []string
	existingAddress          string
	existingProtocol         string
	remoteVersion            int64
	ownerFstabLines          []string
	legacyFstabMarker        bool
	lockHeld                 bool
	staleLock                bool
	mountTableMissing        bool
	notWritable              bool
	targetOutOfProfile       bool
	windowsTarget            bool
	shareInvisible           bool
	targetWithoutDevice      bool
	targetNoCommonNetwork    bool
	darwinTarget             bool
	darwinExistingAddress    string
	darwinRemoteVersion      int64
	darwinMountTableFirmlink bool
	darwinMissingAutomount   bool
	oldAuthoritySnapshot     bool
}

func TestMountShareTargetWithoutDeviceFailsWithoutProbe(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.targetWithoutDevice = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "目标服务器未关联拓扑设备，请 owner 先把服务器归到设备上"
	if !isError || text != want {
		t.Fatalf("报错 = %q (%v)，期待 %q", text, output, want)
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 1 || strings.TrimSpace(commands[0]) != "uname -s" ||
		credential != "" || len(fstab) != 0 {
		t.Fatalf("缺少设备关联时只应识别系统，不应探测地址：%v %q %v", commands, credential, fstab)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].ServerID != fixture.targetID ||
		strings.Contains(rows[0].Command, mountShareTestPassword) ||
		strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "100.64.10.10") {
		t.Fatalf("失败审计 = %+v", rows)
	}
	assertMountShareAuditResult(t, fixture.store, "no-device")
}

func TestMountShareAuditResultClassification(t *testing.T) {
	type classificationCase struct {
		name   string
		err    error
		result *sharemount.Result
		want   string
	}
	tests := []classificationCase{
		{name: "首次挂载成功", result: &sharemount.Result{Action: "first-mount-success"}, want: "success"},
		{name: "改写成功", result: &sharemount.Result{Action: "update-success"}, want: "success"},
		{name: "重新挂载成功", result: &sharemount.Result{Action: "remount-success"}, want: "success"},
		{name: "无需改动", result: &sharemount.Result{Action: "no-change"}, want: "no-change"},
		{name: "共享不存在", err: errMountShareNotFound, want: "not-found"},
		{name: "不在可写入列表", err: errMountShareNotWritable, want: "not-in-writable-servers"},
		{name: "Windows 服务器", err: errMountShareWindowsUnsupported, want: "windows-unsupported"},
		{name: "没有结果与错误", want: "error"},
		{name: "兜底错误", err: errors.New("其他失败"), want: "error"},
		{name: "没有共同网络", err: &sharemount.SelectionFailure{
			Reason: sharemount.SelectionErrorNoCommonNetwork,
		}, want: "no-common-network"},
		{name: "全部候选不可达", err: &sharemount.SelectionFailure{
			Reason: sharemount.SelectionErrorAllCandidatesFailed,
		}, want: "all-candidates-unreachable"},
	}
	for _, class := range []string{
		sharemount.MountClassUnsupportedSystem, sharemount.MountClassMissingSudo,
		sharemount.MountClassMissingSoftware, sharemount.MountClassLockTimeout,
		sharemount.MountClassRemoteVersionNewer, sharemount.MountClassProtocolFailed,
		sharemount.MountClassVerifyFailed, sharemount.MountClassUnmountFailed,
		sharemount.MountClassNoDevice, sharemount.MountClassSSHError, sharemount.MountClassError,
	} {
		tests = append(tests, classificationCase{
			name: "领域失败 " + class, err: &sharemount.MountFailure{Class: class}, want: class,
		})
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mountShareAuditResult(testCase.err, testCase.result); got != testCase.want {
				t.Fatalf("审计分类 = %q，期待 %q", got, testCase.want)
			}
		})
	}
}

func TestMountShareNoCommonNetworkListsBothSides(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.targetNoCommonNetwork = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "目标服务器所属设备与共享存储提供方设备没有共同网络，未探测\n" +
		"共享存储提供方设备所在网络：办公物理局域网、办公虚拟局域网\n" +
		"目标服务器所属设备所在网络：目标独立虚拟局域网\n" +
		"请 owner 先补齐网络拓扑登记"
	if !isError || text != want {
		t.Fatalf("报错 = %q (%v)，期待 %q", text, output, want)
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 1 || strings.TrimSpace(commands[0]) != "uname -s" {
		t.Fatalf("没有共同网络时只应识别系统，不应探测地址：%v", commands)
	}
	if credential != "" || len(fstab) != 0 {
		t.Fatalf("没有共同网络时不应写配置：credential=%q fstab=%v", credential, fstab)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].ServerID != fixture.targetID {
		t.Fatalf("失败审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "no-common-network" ||
		summary["selection_error"] != "no-common-network" ||
		fmt.Sprintf("%v", summary["provider_networks"]) != "[办公物理局域网 办公虚拟局域网]" ||
		fmt.Sprintf("%v", summary["target_networks"]) != "[目标独立虚拟局域网]" {
		t.Fatalf("选地址失败审计摘要 = %#v", summary)
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10", "198.51.100.20"} {
		if strings.Contains(text, address) || strings.Contains(rows[0].Command, address) {
			t.Fatalf("选地址失败泄漏地址 %s：%s\n%s", address, text, rows[0].Command)
		}
	}
}

func TestMountShareAllCandidatesFailListsEveryAttempt(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.probeOutput = "SSHMGR-PROBE 0 fail 12 connection-refused\n" +
			"SSHMGR-PROBE 1 fail 15 timeout\n"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "全部候选地址在目标服务器上探测失败，登记与实际不一致：\n" +
		"办公物理局域网 第1个地址  ✗连接被拒绝 12ms\n" +
		"办公虚拟局域网 第1个地址  ✗连接超时 15ms\n" +
		"未写入任何挂载配置"
	if !isError || text != want {
		t.Fatalf("报错 = %q (%v)，期待 %q", text, output, want)
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 2 || !strings.Contains(commands[1], "192.0.2.10") ||
		!strings.Contains(commands[1], "100.64.10.10") {
		t.Fatalf("全部失败前仍应一次探测所有候选：%v", commands)
	}
	if credential != "" || len(fstab) != 0 {
		t.Fatalf("全部候选失败后不应写配置：credential=%q fstab=%v", credential, fstab)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].ServerID != fixture.targetID {
		t.Fatalf("失败审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "all-candidates-unreachable" ||
		summary["selection_error"] != "all-candidates-failed" {
		t.Fatalf("选地址失败审计摘要 = %#v", summary)
	}
	attempts, ok := summary["attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("逐地址审计 = %#v", summary["attempts"])
	}
	first := attempts[0].(map[string]any)
	second := attempts[1].(map[string]any)
	if first["network_name"] != "办公物理局域网" || first["network_sequence"] != float64(1) ||
		first["error_category"] != "connection-refused" || first["duration_ms"] != float64(12) ||
		second["network_name"] != "办公虚拟局域网" || second["network_sequence"] != float64(1) ||
		second["error_category"] != "timeout" || second["duration_ms"] != float64(15) {
		t.Fatalf("逐地址审计 = %#v", attempts)
	}
	for _, leakText := range []string{text, rows[0].Command} {
		for _, address := range []string{"192.0.2.10", "100.64.10.10"} {
			if strings.Contains(leakText, address) {
				t.Fatalf("选地址失败泄漏地址 %s：%s", address, leakText)
			}
		}
		if strings.Contains(leakText, mountShareTestPassword) {
			t.Fatalf("选地址失败泄漏挂载密码：%s", leakText)
		}
	}
}

func TestMountSharePhysicalFailuresFallBackToVirtualNetwork(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.probeOutput = "SSHMGR-PROBE 0 fail 12 connection-refused\n" +
			"SSHMGR-PROBE 1 ok 18 \n"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("物理地址失败但虚拟地址可用时应成功：%s", text)
	}
	if output["network_name"] != "办公虚拟局域网" || output["network_sequence"] != float64(1) {
		t.Fatalf("降级输出 = %#v", output)
	}
	notes, ok := output["mount_notes"].([]any)
	if !ok || len(notes) != 2 ||
		notes[0] != "已使用虚拟局域网：办公虚拟局域网 第1个地址" ||
		notes[1] != "物理局域网地址不通：办公物理局域网 第1个地址  ✗连接被拒绝 12ms" {
		t.Fatalf("降级说明 = %#v", output["mount_notes"])
	}
	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 3 || strings.Contains(commands[2], "192.0.2.10") ||
		!strings.Contains(commands[2], "100.64.10.10") {
		t.Fatalf("降级后固定挂载命令应只使用虚拟地址：%v", commands)
	}
	if credential == "" || len(fstab) != 1 || !strings.Contains(fstab[0], "//100.64.10.10/datasets") {
		t.Fatalf("降级后的远端配置 = %q %#v", credential, fstab)
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10"} {
		if strings.Contains(text, address) {
			t.Fatalf("降级成功泄漏地址 %s：%s", address, text)
		}
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" {
		t.Fatalf("降级成功审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "success" || summary["action"] != "first-mount-success" ||
		summary["network_fallback"] != "virtual-network" ||
		summary["network_name"] != "办公虚拟局域网" ||
		fmt.Sprintf("%v", summary["mount_notes"]) != fmt.Sprintf("%v", output["mount_notes"]) {
		t.Fatalf("降级成功审计摘要 = %#v", summary)
	}
	if strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "100.64.10.10") ||
		strings.Contains(rows[0].Command, mountShareTestPassword) {
		t.Fatalf("降级成功审计泄漏敏感信息：%s", rows[0].Command)
	}
}

func callMountShare(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (map[string]any, string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "mount_share", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := toolResultText(result)
	output := map[string]any{}
	if !result.IsError {
		if err := json.Unmarshal([]byte(text), &output); err != nil {
			t.Fatalf("解析 mount_share 输出失败：%v\n%s", err, text)
		}
	}
	return output, text, result.IsError
}

func TestMountShareLinuxFirstMountSelectsPhysicalNetwork(t *testing.T) {
	fixture := newMountShareFixture(t)
	if err := fixture.store.AddPolicyRule(policy.CustomRule{
		ID:          "MOUNT-SHARE-POLICY-LEAK",
		Shells:      []models.ShellType{models.ShellPOSIX},
		Regex:       `SSHMGR-LINUX-MOUNT|mount -t cifs`,
		Disposition: policy.DispositionBlock,
		Explanation: "测试规则：若固定挂载命令进入命令策略，这次调用必须被拦截",
	}); err != nil {
		t.Fatal(err)
	}

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("mount_share 失败：%s", text)
	}
	if output["mount_point"] != "/mnt/sshmgr/datasets" || output["version"] != float64(1) ||
		output["network_name"] != "办公物理局域网" || output["network_sequence"] != float64(1) {
		t.Fatalf("首次挂载输出 = %#v", output)
	}
	paths, ok := output["paths"].([]any)
	if !ok || len(paths) != 2 ||
		paths[0].(map[string]any)["full_path"] != "/mnt/sshmgr/datasets/tmp" ||
		paths[1].(map[string]any)["full_path"] != "/mnt/sshmgr/datasets/datasets" {
		t.Fatalf("目录约定路径 = %#v", output["paths"])
	}

	commands, inputs, credential, fstab := fixture.fake.recorded()
	remoteAll := strings.Join(commands, "\n")
	if len(commands) != 3 || !strings.Contains(commands[1], "192.0.2.10") ||
		!strings.Contains(commands[1], "100.64.10.10") ||
		strings.Index(commands[1], "192.0.2.10") > strings.Index(commands[1], "100.64.10.10") {
		t.Fatalf("候选探测命令应一次跑完并保持物理局域网在前：%s", commands[1])
	}
	if !strings.Contains(commands[2], "192.0.2.10") || strings.Contains(commands[2], "100.64.10.10") {
		t.Fatalf("应只把物理局域网地址写入挂载固定命令：%s", commands[2])
	}
	for _, want := range []string{
		"_netdev", "nofail", "x-systemd.automount", "x-systemd.mount-timeout=10",
		"/etc/sshmgr/shares/" + fixture.shareID + ".credentials", "systemctl daemon-reload",
		`install -d -m 755 "$mount_point"`,
		"install -d -m 700 '/etc/sshmgr/shares'\ninstall -d -m 755 '/mnt/sshmgr'",
	} {
		if !strings.Contains(remoteAll, want) {
			t.Fatalf("远端固定命令缺少 %q：%s", want, remoteAll)
		}
	}
	if len(inputs) != 3 || string(inputs[2]) != mountShareTestPassword+"\n" {
		t.Fatalf("提权命令标准输入 = %q，全部输入数 = %d", inputs, len(inputs))
	}
	if credential != "username=mount-user\npassword="+mountShareTestPassword+"\n" {
		t.Fatalf("最终凭据文件 = %q", credential)
	}
	if len(fstab) != 1 || !strings.Contains(fstab[0], "vers=3.1.1") {
		t.Fatalf("fstab 行 = %#v", fstab)
	}
	for _, leakText := range []string{text, remoteAll} {
		if strings.Contains(leakText, mountShareTestPassword) {
			t.Fatalf("挂载密码泄漏：%s", leakText)
		}
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.10"} {
		if strings.Contains(text, address) {
			t.Fatalf("工具结果泄漏地址 %s：%s", address, text)
		}
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" || rows[0].ServerID != fixture.targetID {
		t.Fatalf("mount-share 审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["share"] != "datasets" || summary["version"] != float64(1) ||
		summary["network_name"] != "办公物理局域网" || summary["network_sequence"] != float64(1) ||
		summary["result"] != "success" || summary["action"] != "first-mount-success" {
		t.Fatalf("mount-share 审计摘要 = %#v", summary)
	}
	if strings.Contains(rows[0].Command, mountShareTestPassword) ||
		strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "100.64.10.10") {
		t.Fatalf("mount-share 审计泄漏密码或地址：%s", rows[0].Command)
	}

	toolList, err := fixture.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var description string
	for _, tool := range toolList.Tools {
		if tool.Name == "mount_share" {
			description = tool.Description
		}
	}
	for _, want := range []string{
		"idempotent", "cannot unmount", "revocation marker",
		"until a newer shared-storage authorization is synced", "Windows servers do not need it",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("mount_share 描述缺少 %q：%s", want, description)
		}
	}
}

func TestMountShareDarwinFirstMountUsesIndirectAutomountMap(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
	})
	if err := fixture.store.AddPolicyRule(policy.CustomRule{
		ID:          "MOUNT-SHARE-DARWIN-POLICY-LEAK",
		Shells:      []models.ShellType{models.ShellPOSIX},
		Regex:       `SSHMGR-DARWIN-MOUNT|automount -cv`,
		Disposition: policy.DispositionBlock,
		Explanation: "测试规则：若 macOS 固定挂载命令进入命令策略，这次调用必须被拦截",
	}); err != nil {
		t.Fatal(err)
	}

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("macOS mount_share 失败：%s", text)
	}
	if output["result"] != "first-mount-success" || output["mount_point"] != "/mnt/sshmgr/datasets" ||
		output["network_name"] != "办公物理局域网" || output["network_sequence"] != float64(1) {
		t.Fatalf("macOS 首次挂载输出 = %#v", output)
	}

	darwinFake, ok := fixture.fake.(*fakeDarwinMountServer)
	if !ok {
		t.Fatalf("测试夹具没有使用 macOS 假服务器：%T", fixture.fake)
	}
	commands, inputs, autoMaster, mapLines := darwinFake.recordedDarwin()
	if len(commands) != 3 {
		t.Fatalf("macOS 远端命令数 = %d：%#v", len(commands), commands)
	}
	mountCommand := commands[2]
	for _, want := range []string{
		"printf '%s %s\\n' '/mnt/sshmgr' '/etc/sshmgr/auto_share'",
		"/etc/sshmgr/auto_share",
		"sshmgr-share=datasets;sshmgr-version=1",
		"automount -cv",
		`ls -A "$mount_point"`,
		"SSHMGR-DARWIN-MOUNT",
	} {
		if !strings.Contains(mountCommand, want) {
			t.Fatalf("macOS 固定命令缺少 %q：%s", want, mountCommand)
		}
	}
	if strings.Contains(mountCommand, "LaunchDaemon") || strings.Contains(mountCommand, "launchd") {
		t.Fatalf("macOS 固定命令不能使用 LaunchDaemon：%s", mountCommand)
	}
	if len(inputs) != 3 || string(inputs[2]) != mountShareTestPassword+"\n" {
		t.Fatalf("macOS 提权命令标准输入 = %q，全部输入数 = %d", inputs, len(inputs))
	}
	wantMap := []string{
		"# sshmgr-share=datasets;sshmgr-version=1",
		"datasets -fstype=smbfs ://mount-user:" + mountShareTestPasswordURLEncoded + "@192.0.2.10/datasets",
	}
	if strings.Join(mapLines, "\n") != strings.Join(wantMap, "\n") {
		t.Fatalf("macOS 间接映射文件 = %#v，期待 %#v", mapLines, wantMap)
	}
	foundReference := false
	for _, line := range autoMaster {
		if line == "/mnt/sshmgr /etc/sshmgr/auto_share" {
			foundReference = true
		}
	}
	if !foundReference {
		t.Fatalf("autofs 主配置没有间接映射引用：%#v", autoMaster)
	}
	owner, mode, refreshes, accesses := darwinFake.darwinState()
	if owner != "root" || mode != "600" || refreshes != 1 || accesses != 1 {
		t.Fatalf("macOS 映射文件或验证状态错误：owner=%q mode=%q refreshes=%d accesses=%d",
			owner, mode, refreshes, accesses)
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" {
		t.Fatalf("macOS mount-share 审计 = %+v", rows)
	}
	for _, leakText := range []string{text, mountCommand, rows[0].Command} {
		if strings.Contains(leakText, mountShareTestPassword) ||
			strings.Contains(leakText, mountShareTestPasswordURLEncoded) {
			t.Fatalf("macOS 挂载密码泄漏：%s", leakText)
		}
	}
}

func TestMountShareDarwinRebuildsResetAutoMasterReference(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "192.0.2.10"
		setup.darwinRemoteVersion = 1
	})
	fixture.fake.(*fakeDarwinMountServer).resetAutoMasterReference()

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("主配置被重置后 macOS mount_share 失败：%s", text)
	}
	if output["result"] != "remount-success" {
		t.Fatalf("主配置重建后的输出 = %#v", output)
	}
	notes, ok := output["mount_notes"].([]any)
	if !ok || len(notes) != 1 || notes[0] != "系统配置被重置，已重建" {
		t.Fatalf("主配置重建说明 = %#v", output["mount_notes"])
	}

	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	_, _, autoMaster, mapLines := darwinFake.recordedDarwin()
	foundReference := false
	for _, line := range autoMaster {
		if line == "/mnt/sshmgr /etc/sshmgr/auto_share" {
			foundReference = true
		}
	}
	if !foundReference {
		t.Fatalf("主配置重建后仍缺少间接映射引用：%#v", autoMaster)
	}
	if len(mapLines) != 2 || strings.Contains(strings.Join(mapLines, "\n"), "old-password") == false {
		t.Fatalf("重建主配置不应改写间接映射：%#v", mapLines)
	}
	mapWrites, autoMasterWrites := darwinFake.writeCounts()
	if mapWrites != 0 || autoMasterWrites != 1 {
		t.Fatalf("重建主配置的写入错误：映射 %d 次，主配置 %d 次", mapWrites, autoMasterWrites)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].Command, "系统配置被重置，已重建") {
		t.Fatalf("主配置重建审计 = %+v", rows)
	}
}

func TestMountShareDarwinFirmlinkMountTableReturnsNoChange(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "192.0.2.10"
		setup.darwinRemoteVersion = 1
		setup.darwinMountTableFirmlink = true
	})

	for callIndex := 1; callIndex <= 2; callIndex++ {
		output, text, isError := callMountShare(t, fixture.session, map[string]any{
			"share": "datasets", "server_id": fixture.targetID,
		})
		if isError {
			t.Fatalf("第 %d 次 macOS mount_share 失败：%s", callIndex, text)
		}
		if output["result"] != "no-change" {
			t.Fatalf("第 %d 次 macOS 输出 = %#v，应识别数据卷 firmlink 挂载点",
				callIndex, output)
		}
	}

	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	mapWrites, autoMasterWrites := darwinFake.writeCounts()
	if mapWrites != 0 || autoMasterWrites != 0 || darwinFake.remountCount() != 0 {
		t.Fatalf("firmlink 挂载不应触发改写：映射 %d 次，主配置 %d 次，挂载刷新 %d 次",
			mapWrites, autoMasterWrites, darwinFake.remountCount())
	}
}

func TestMountShareDarwinRepeatReturnsNoChange(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
	})

	first, firstText, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("第一次 macOS mount_share 失败：%s", firstText)
	}
	second, secondText, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("重复 macOS mount_share 失败：%s", secondText)
	}
	if first["result"] != "first-mount-success" || second["result"] != "no-change" {
		t.Fatalf("两次 macOS 输出 = %#v %#v", first, second)
	}
	if notes, ok := second["mount_notes"].([]any); ok && len(notes) != 0 {
		t.Fatalf("macOS 无需改动时不应给出提示：%#v", notes)
	}

	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	mapWrites, autoMasterWrites := darwinFake.writeCounts()
	if mapWrites != 1 || autoMasterWrites != 1 || darwinFake.remountCount() != 1 {
		t.Fatalf("重复调用不应改写 macOS 配置：映射 %d 次，主配置 %d 次，挂载刷新 %d 次",
			mapWrites, autoMasterWrites, darwinFake.remountCount())
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("macOS mount-share 审计行数 = %d", len(rows))
	}
	for _, row := range rows {
		if strings.Contains(row.Command, mountShareTestPassword) ||
			strings.Contains(row.Command, mountShareTestPasswordURLEncoded) {
			t.Fatalf("macOS 审计泄漏挂载密码：%s", row.Command)
		}
	}
}

func TestMountShareDarwinUpdatesOnlyCurrentMarkerEntry(t *testing.T) {
	ownerBefore := "owner-share -fstype=smbfs ://owner:secret@192.0.2.99/owner-share"
	ownerAfter := "# owner comment"
	otherMarker := "# sshmgr-share=archive;sshmgr-version=9"
	otherEntry := "archive -fstype=smbfs ://mount-user:old-password@100.64.10.10/archive"
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "100.64.10.10"
		setup.darwinRemoteVersion = 0
		setup.reachable = []string{"192.0.2.10", "100.64.10.10"}
	})
	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	darwinFake.mapLines = []string{
		ownerBefore, otherMarker, otherEntry,
		"# sshmgr-share=datasets;sshmgr-version=0",
		"datasets -fstype=smbfs ://mount-user:old-password@100.64.10.10/datasets",
		ownerAfter,
	}

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("macOS 更新映射失败：%s", text)
	}
	if output["result"] != "update-success" || output["network_name"] != "办公虚拟局域网" ||
		output["network_sequence"] != float64(1) {
		t.Fatalf("macOS 更新输出 = %#v", output)
	}
	wantMap := []string{
		ownerBefore,
		otherMarker,
		otherEntry,
		"# sshmgr-share=datasets;sshmgr-version=1",
		"datasets -fstype=smbfs ://mount-user:" + mountShareTestPasswordURLEncoded + "@100.64.10.10/datasets",
		ownerAfter,
	}
	_, _, _, mapLines := darwinFake.recordedDarwin()
	if strings.Join(mapLines, "\n") != strings.Join(wantMap, "\n") {
		t.Fatalf("macOS 映射替换结果 =\n%#v\n期待\n%#v", mapLines, wantMap)
	}
}

func TestMountShareDarwinReselectsWhenExistingAddressUnreachable(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "100.64.10.10"
		setup.darwinRemoteVersion = 1
		setup.reachable = []string{"192.0.2.10"}
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("macOS 重新选址失败：%s", text)
	}
	if output["result"] != "update-success" || output["network_name"] != "办公物理局域网" {
		t.Fatalf("macOS 重新选址输出 = %#v", output)
	}
	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	_, _, _, mapLines := darwinFake.recordedDarwin()
	if len(mapLines) != 2 || !strings.Contains(mapLines[1], "@192.0.2.10/datasets") {
		t.Fatalf("macOS 重新选址后的映射 = %#v", mapLines)
	}
}

func TestMountShareDarwinMissingAutomountAndRemoteNewerVersionFail(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*mountShareFixtureSetup)
		wantErr    string
		wantResult string
	}{
		{
			name: "缺少 automount", wantResult: "missing-software",
			setup: func(setup *mountShareFixtureSetup) {
				setup.darwinTarget = true
				setup.darwinMissingAutomount = true
			},
			wantErr: "目标服务器缺少 automount，请先由 owner 修复 macOS 系统组件；挂载工具不会自动安装",
		},
		{
			name: "远端版本更新", wantResult: "remote-version-newer",
			setup: func(setup *mountShareFixtureSetup) {
				setup.darwinTarget = true
				setup.darwinExistingAddress = "192.0.2.10"
				setup.darwinRemoteVersion = 2
			},
			wantErr: "目标服务器上的共享存储版本更新，请先同步快照后再调用挂载工具",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMountShareFixture(t, testCase.setup)
			output, text, isError := callMountShare(t, fixture.session, map[string]any{
				"share": "datasets", "server_id": fixture.targetID,
			})
			if !isError || text != testCase.wantErr {
				t.Fatalf("报错 = %q (%v)，期待 %q", text, output, testCase.wantErr)
			}
			darwinFake := fixture.fake.(*fakeDarwinMountServer)
			mapWrites, autoMasterWrites := darwinFake.writeCounts()
			if mapWrites != 0 || autoMasterWrites != 0 {
				t.Fatalf("失败路径不应改写配置：映射 %d 次，主配置 %d 次", mapWrites, autoMasterWrites)
			}
			commands, _, _, _ := darwinFake.recordedDarwin()
			for _, leakText := range []string{text, strings.Join(commands, "\n")} {
				if strings.Contains(leakText, mountShareTestPassword) ||
					strings.Contains(leakText, mountShareTestPasswordURLEncoded) {
					t.Fatalf("失败路径泄漏挂载密码：%s", leakText)
				}
			}
			assertMountShareAuditResult(t, fixture.store, testCase.wantResult)
		})
	}
}

func TestMountShareDarwinRejectsRevokedShare(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "192.0.2.10"
		setup.darwinRemoteVersion = 1
	})
	darwinFake, ok := fixture.fake.(*fakeDarwinMountServer)
	if !ok {
		t.Fatalf("测试夹具没有使用 macOS 假服务器：%T", fixture.fake)
	}
	darwinFake.revokedMarkerVersion = darwinFake.shareVersion

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "共享存储已从这台服务器撤销，不能重新挂载；如 owner 已重新授权，请先同步快照后再试"
	if !isError || text != want {
		t.Fatalf("已撤销报错 = %q (%v)，期待 %q", text, output, want)
	}
	if mapWrites, autoMasterWrites := darwinFake.writeCounts(); mapWrites != 0 || autoMasterWrites != 0 {
		t.Fatalf("已撤销时不应改写：映射 %d 次，主配置 %d 次", mapWrites, autoMasterWrites)
	}
	_, _, _, mapLines := darwinFake.recordedDarwin()
	if len(mapLines) != 2 || !strings.Contains(mapLines[0], "sshmgr-version=1") ||
		!strings.Contains(mapLines[1], "@192.0.2.10/datasets") {
		t.Fatalf("已撤销时 macOS 映射被误改：%#v", mapLines)
	}
	assertMountShareAuditResult(t, fixture.store, "share-revoked")
}

func TestMountShareDarwinNewerAuthorizationRemovesRevokedMarker(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "192.0.2.10"
		setup.darwinRemoteVersion = 1
	})
	changeNote := "重新授权共享存储"
	if _, err := fixture.store.EditSharedStorage("datasets", store.SharedStorageChanges{
		ChangeNote: &changeNote,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	updatedShare, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	darwinFake, ok := fixture.fake.(*fakeDarwinMountServer)
	if !ok {
		t.Fatalf("测试夹具没有使用 macOS 假服务器：%T", fixture.fake)
	}
	darwinFake.setShareVersion(updatedShare.Version)
	darwinFake.revokedMarkerVersion = 1

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("快照版本更新后 mount_share 失败：%s", text)
	}
	if output["result"] != "update-success" || output["version"] != float64(updatedShare.Version) {
		t.Fatalf("重新授权后的输出 = %#v", output)
	}
	if darwinFake.revokedMarkerVersion != 0 {
		t.Fatalf("挂载成功后撤销标记仍存在，版本号 = %d", darwinFake.revokedMarkerVersion)
	}
	if mapWrites, autoMasterWrites := darwinFake.writeCounts(); mapWrites != 1 || autoMasterWrites != 0 {
		t.Fatalf("重新授权后的写入次数错误：映射 %d，主配置 %d", mapWrites, autoMasterWrites)
	}
	_, _, _, mapLines := darwinFake.recordedDarwin()
	if len(mapLines) != 2 ||
		!strings.Contains(mapLines[0], fmt.Sprintf("sshmgr-version=%d", updatedShare.Version)) ||
		!strings.Contains(mapLines[1], mountShareTestPasswordURLEncoded+"@192.0.2.10/datasets") {
		t.Fatalf("重新授权后的 macOS 映射错误：%#v", mapLines)
	}
}

func TestMountShareDarwinReportsLockTimeout(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.lockHeld = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "另一个挂载正在进行，请稍后再试" {
		t.Fatalf("macOS 文件锁超时报错 = %q (%v)", text, output)
	}
	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	mapWrites, autoMasterWrites := darwinFake.writeCounts()
	if mapWrites != 0 || autoMasterWrites != 0 {
		t.Fatalf("macOS 拿锁失败后不应改写配置：映射 %d 次，主配置 %d 次", mapWrites, autoMasterWrites)
	}
	assertMountShareAuditResult(t, fixture.store, "lock-timeout")
}

func TestMountShareDarwinRecoversExpiredStaleLock(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.staleLock = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("macOS 过期残留锁应被回收并完成挂载：%s", text)
	}
	if output["result"] != "first-mount-success" {
		t.Fatalf("macOS 过期锁回收后的输出 = %#v", output)
	}
	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	if mapWrites, autoMasterWrites := darwinFake.writeCounts(); mapWrites != 1 || autoMasterWrites != 1 {
		t.Fatalf("macOS 过期锁回收后的写入错误：映射 %d 次，主配置 %d 次", mapWrites, autoMasterWrites)
	}
}

func TestMountShareDarwinRemountsAfterMapRewrite(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "192.0.2.10"
		setup.darwinRemoteVersion = 0
	})
	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	if !darwinFake.mounted {
		t.Fatal("改写场景应从已挂载状态开始")
	}

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("macOS 映射改写后应重新挂载：%s", text)
	}
	if output["result"] != "update-success" {
		t.Fatalf("macOS 映射改写后的输出 = %#v", output)
	}
	if unmounts := darwinFake.unmountCount(); unmounts != 1 {
		t.Fatalf("映射改写后应卸载一次旧 smbfs 挂载，实际 %d 次", unmounts)
	}
	owner, mode, refreshes, accesses := darwinFake.darwinState()
	if owner != "root" || mode != "600" || refreshes != 1 || accesses != 1 {
		t.Fatalf("macOS 改写后的刷新状态错误：owner=%q mode=%q refreshes=%d accesses=%d",
			owner, mode, refreshes, accesses)
	}
	if !darwinFake.mounted {
		t.Fatal("macOS 改写后应完成重新挂载")
	}
}

func TestMountShareDarwinRewriteUnmountFailureStops(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.darwinExistingAddress = "192.0.2.10"
		setup.darwinRemoteVersion = 0
	})
	darwinFake := fixture.fake.(*fakeDarwinMountServer)
	darwinFake.unmountFails = true

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "macOS 卸载旧挂载失败，请 owner 手动检查后重试" {
		t.Fatalf("卸载失败的报错 = %q (%v)", text, output)
	}
	if unmounts := darwinFake.unmountCount(); unmounts != 0 {
		t.Fatalf("卸载失败时不应记录成功卸载：%d 次", unmounts)
	}
}

func TestMountShareDarwinEmptyDirectoryFailsMountTableVerification(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.darwinTarget = true
		setup.mountTableMissing = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "macOS 挂载点验证失败，不能确认共享已可用" {
		t.Fatalf("macOS 空目录且挂载表没有条目时的报错 = %q (%v)", text, output)
	}
}

func TestMountShareLinuxSMBVersionFallsBackTo20(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.workingVersion = "2.0"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("协议降级到 2.0 后应成功：%s", text)
	}
	if output["network_name"] != "办公物理局域网" {
		t.Fatalf("协议降级输出 = %#v", output)
	}
	_, _, _, fstab := fixture.fake.recorded()
	if len(fstab) != 1 || !strings.Contains(fstab[0], "vers=2.0") {
		t.Fatalf("协议降级后的 fstab = %#v", fstab)
	}
	commands, _, _, _ := fixture.fake.recorded()
	if !strings.Contains(commands[2], "for sshmgr_version in 3.1.1 3.0 2.1 2.0; do") {
		t.Fatalf("协议版本必须按 3.1.1、3.0、2.1、2.0 依次尝试：%s", commands[2])
	}
	if strings.Contains(text, "2.0") {
		t.Fatalf("试出的 SMB 协议版本不应回传：%s", text)
	}
}

func TestMountShareLinuxRepeatReturnsNoChange(t *testing.T) {
	fixture := newMountShareFixture(t)

	first, firstText, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("第一次 mount_share 失败：%s", firstText)
	}
	second, secondText, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("重复 mount_share 失败：%s", secondText)
	}
	if first["result"] != "first-mount-success" || second["result"] != "no-change" {
		t.Fatalf("两次输出 = %#v %#v", first, second)
	}
	if notes, ok := second["mount_notes"].([]any); ok && len(notes) != 0 {
		t.Fatalf("无需改动时不应给出切换提示：%#v", second["mount_notes"])
	}

	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 1 || configurationWrites != 1 {
		t.Fatalf("重复调用不应改写配置：凭据写入 %d 次，配置写入 %d 次", credentialWrites, configurationWrites)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("mount-share 审计行数 = %d", len(rows))
	}
	foundNoChangeAudit := false
	for _, row := range rows {
		var summary map[string]any
		if err := json.Unmarshal([]byte(row.Command), &summary); err != nil {
			t.Fatal(err)
		}
		if summary["result"] == "no-change" {
			foundNoChangeAudit = true
			break
		}
	}
	if !foundNoChangeAudit {
		t.Fatalf("没有找到无需改动审计：%+v", rows)
	}
}

func TestMountShareLinuxCommandUsesLockAndMarker(t *testing.T) {
	fixture := newMountShareFixture(t)

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("mount_share 失败：%s", text)
	}
	if output["result"] != "first-mount-success" {
		t.Fatalf("首次挂载输出 = %#v", output)
	}

	commands, _, _, fstab := fixture.fake.recorded()
	if len(commands) != 3 {
		t.Fatalf("远端命令 = %#v", commands)
	}
	mountCommand := commands[2]
	for _, want := range []string{
		"sshmgr_share_marker='sshmgr-share=datasets;sshmgr-version=1'",
		"sshmgr_lock_dir='/var/run/sshmgr-mount.lock'",
		`date +%s > "$sshmgr_lock_owner"`,
		`rm -f "$sshmgr_lock_owner"`,
		"mkdir \"$sshmgr_lock_dir\"",
		"rmdir \"$sshmgr_lock_dir\"",
		"trap sshmgr_release_shared_storage_lock EXIT HUP INT TERM",
	} {
		if !strings.Contains(mountCommand, want) {
			t.Fatalf("远端固定命令缺少 %q：%s", want, mountCommand)
		}
	}
	if strings.Contains(mountCommand, "rm -rf") {
		t.Fatalf("远端固定命令不能递归删除目录：%s", mountCommand)
	}
	if len(fstab) != 1 || !strings.Contains(fstab[0], "x-sshmgr-share=datasets,x-sshmgr-version=1") {
		t.Fatalf("带标记的 fstab = %#v", fstab)
	}
}

func TestMountShareLinuxKeepsWorkingExistingAddress(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.reachable = []string{"192.0.2.10", "100.64.10.10"}
		setup.existingAddress = "100.64.10.10"
		setup.existingProtocol = "2.0"
		setup.remoteVersion = 1
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("现有地址可用时 mount_share 失败：%s", text)
	}
	if output["result"] != "no-change" || output["network_name"] != "办公虚拟局域网" ||
		output["network_sequence"] != float64(1) {
		t.Fatalf("保留现有地址的输出 = %#v", output)
	}
	notices, ok := output["mount_notes"].([]any)
	if !ok || len(notices) != 1 || !strings.Contains(notices[0].(string), "存在更靠前的物理局域网地址") ||
		!strings.Contains(notices[0].(string), "由 owner 手动切换") {
		t.Fatalf("切换提示 = %#v", output["mount_notes"])
	}

	commands, _, credential, fstab := fixture.fake.recorded()
	if len(commands) != 3 || !strings.Contains(commands[2], "192.0.2.10") ||
		!strings.Contains(commands[2], "100.64.10.10") {
		t.Fatalf("固定命令应携带全部可连通候选：%s", commands[2])
	}
	if credential != "username=mount-user\npassword=old-password\n" ||
		len(fstab) != 1 || !strings.Contains(fstab[0], "//100.64.10.10/datasets") ||
		!strings.Contains(fstab[0], "vers=2.0") {
		t.Fatalf("现有配置被改写：credential=%q fstab=%#v", credential, fstab)
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 0 || configurationWrites != 0 {
		t.Fatalf("现有配置无需改写：凭据写入 %d 次，配置写入 %d 次", credentialWrites, configurationWrites)
	}
}

func TestMountShareLinuxRewritesLegacyFstabMarker(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "3.1.1"
		setup.remoteVersion = 1
		setup.legacyFstabMarker = true
	})

	first, firstText, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("改写旧格式标记失败：%s", firstText)
	}
	if first["result"] != "update-success" {
		t.Fatalf("旧格式标记应触发更新，输出 = %#v", first)
	}
	_, _, _, fstab := fixture.fake.recorded()
	if len(fstab) != 1 || strings.Contains(fstab[0], ",sshmgr-share=") ||
		!strings.Contains(fstab[0], ",x-sshmgr-share=datasets,x-sshmgr-version=1,") {
		t.Fatalf("旧格式标记没有改写为用户空间选项：%#v", fstab)
	}

	second, secondText, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("改写后重复挂载失败：%s", secondText)
	}
	if second["result"] != "no-change" {
		t.Fatalf("改写后重复挂载输出 = %#v", second)
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 1 || configurationWrites != 1 {
		t.Fatalf("旧格式只应改写一次：凭据写入 %d 次，配置写入 %d 次",
			credentialWrites, configurationWrites)
	}
}

func TestMountShareLinuxReselectsWhenExistingAddressIsUnreachable(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.reachable = []string{"192.0.2.10"}
		setup.existingAddress = "100.64.10.10"
		setup.existingProtocol = "2.0"
		setup.remoteVersion = 1
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("现有地址不通时 mount_share 失败：%s", text)
	}
	if output["result"] != "update-success" || output["network_name"] != "办公物理局域网" {
		t.Fatalf("重选地址后的输出 = %#v", output)
	}

	_, inputs, credential, fstab := fixture.fake.recorded()
	if len(fstab) != 1 || !strings.Contains(fstab[0], "//192.0.2.10/datasets") ||
		!strings.Contains(fstab[0], "vers=3.1.1") {
		t.Fatalf("重选后的 fstab = %#v", fstab)
	}
	if credential != "username=mount-user\npassword="+mountShareTestPassword+"\n" {
		t.Fatalf("换地址应按试挂流程原子替换凭据文件：%q", credential)
	}
	for _, input := range inputs {
		if strings.Contains(string(input), "old-password") {
			t.Fatalf("旧密码不应进入本次标准输入：%q", input)
		}
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 1 || configurationWrites != 1 {
		t.Fatalf("重选地址后的写入次数错误：凭据 %d，配置 %d", credentialWrites, configurationWrites)
	}
}

func TestMountShareLinuxRejectsRemoteNewerVersion(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "3.1.1"
		setup.remoteVersion = 2
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "目标服务器上的共享存储版本更新，请先同步快照后再调用挂载工具" {
		t.Fatalf("远端版本更新的报错 = %q (%v)", text, output)
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 0 || configurationWrites != 0 {
		t.Fatalf("远端版本更新时不应改写：凭据 %d 次，配置 %d 次", credentialWrites, configurationWrites)
	}
	if credential, fstab := func() (string, []string) {
		_, _, credential, fstab := fixture.fake.recorded()
		return credential, fstab
	}(); len(fstab) != 1 || !strings.Contains(fstab[0], "sshmgr-version=2") ||
		credential != "username=mount-user\npassword=old-password\n" {
		t.Fatalf("远端配置被误改：credential=%q fstab=%#v", credential, fstab)
	}
	assertMountShareAuditResult(t, fixture.store, "remote-version-newer")
}

func TestMountShareLinuxRejectsRevokedShare(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "3.1.1"
		setup.remoteVersion = 1
	})
	linuxFake, ok := fixture.fake.(*fakeLinuxMountServer)
	if !ok {
		t.Fatalf("测试夹具没有使用 Linux 假服务器：%T", fixture.fake)
	}
	linuxFake.revokedMarkerVersion = linuxFake.shareVersion

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	want := "共享存储已从这台服务器撤销，不能重新挂载；如 owner 已重新授权，请先同步快照后再试"
	if !isError || text != want {
		t.Fatalf("已撤销报错 = %q (%v)，期待 %q", text, output, want)
	}
	if text == "目标服务器上的共享存储版本更新，请先同步快照后再调用挂载工具" {
		t.Fatal("已撤销不能伪装成远端版本更新")
	}
	if credentialWrites, configurationWrites := linuxFake.writeCounts(); credentialWrites != 0 || configurationWrites != 0 {
		t.Fatalf("已撤销时不应改写：凭据 %d 次，配置 %d 次", credentialWrites, configurationWrites)
	}
	_, _, credential, fstab := linuxFake.recorded()
	if credential != "username=mount-user\npassword=old-password\n" ||
		len(fstab) != 1 || !strings.Contains(fstab[0], "sshmgr-version=1") {
		t.Fatalf("已撤销时远端配置被误改：credential=%q fstab=%#v", credential, fstab)
	}
	assertMountShareAuditResult(t, fixture.store, "share-revoked")
}

func TestMountShareLinuxNewerAuthorizationRemovesRevokedMarker(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "3.1.1"
		setup.remoteVersion = 1
	})
	changeNote := "重新授权共享存储"
	if _, err := fixture.store.EditSharedStorage("datasets", store.SharedStorageChanges{
		ChangeNote: &changeNote,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	updatedShare, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	linuxFake, ok := fixture.fake.(*fakeLinuxMountServer)
	if !ok {
		t.Fatalf("测试夹具没有使用 Linux 假服务器：%T", fixture.fake)
	}
	linuxFake.setShareVersion(updatedShare.Version)
	linuxFake.revokedMarkerVersion = 1

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("快照版本更新后 mount_share 失败：%s", text)
	}
	if output["result"] != "update-success" || output["version"] != float64(updatedShare.Version) {
		t.Fatalf("重新授权后的输出 = %#v", output)
	}
	if linuxFake.revokedMarkerVersion != 0 {
		t.Fatalf("挂载成功后撤销标记仍存在，版本号 = %d", linuxFake.revokedMarkerVersion)
	}
	if credentialWrites, configurationWrites := linuxFake.writeCounts(); credentialWrites != 1 || configurationWrites != 1 {
		t.Fatalf("重新授权后的写入次数错误：凭据 %d，配置 %d", credentialWrites, configurationWrites)
	}
	_, _, credential, fstab := linuxFake.recorded()
	if credential != "username=mount-user\npassword="+mountShareTestPassword+"\n" ||
		len(fstab) != 1 || !strings.Contains(fstab[0], fmt.Sprintf("sshmgr-version=%d", updatedShare.Version)) {
		t.Fatalf("重新授权后的配置错误：credential=%q fstab=%#v", credential, fstab)
	}
}

func TestMountShareLinuxUpdatesOlderRemoteVersionAndPassword(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "2.0"
		setup.remoteVersion = 1
	})
	newPassword := `new-P@$$ 'quotes' "double" \ end`
	if err := fixture.store.SetSecretValue(
		"DATASETS_MOUNT_PASSWORD", []byte(newPassword), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	changeNote := "更换挂载密码"
	if _, err := fixture.store.EditSharedStorage("datasets", store.SharedStorageChanges{
		ChangeNote: &changeNote,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	updatedShare, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	fixture.fake.setShareVersion(updatedShare.Version)

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("远端版本较旧时 mount_share 失败：%s", text)
	}
	if output["result"] != "update-success" || output["version"] != float64(updatedShare.Version) {
		t.Fatalf("更新后的输出 = %#v", output)
	}

	commands, inputs, credential, fstab := fixture.fake.recorded()
	if len(inputs) != 3 || string(inputs[2]) != newPassword+"\n" {
		t.Fatalf("新密码标准输入 = %q", inputs)
	}
	if credential != "username=mount-user\npassword="+newPassword+"\n" {
		t.Fatalf("新凭据文件 = %q", credential)
	}
	if len(fstab) != 1 || !strings.Contains(fstab[0], fmt.Sprintf("sshmgr-version=%d", updatedShare.Version)) ||
		!strings.Contains(fstab[0], "//192.0.2.10/datasets") ||
		!strings.Contains(fstab[0], "vers=2.0") {
		t.Fatalf("更新后的 fstab = %#v", fstab)
	}
	if !strings.Contains(commands[2], fmt.Sprintf("sshmgr-share=datasets;sshmgr-version=%d", updatedShare.Version)) ||
		strings.Contains(commands[2], newPassword) {
		t.Fatalf("远端命令标记或密码传递错误：%s", commands[2])
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 1 || configurationWrites != 1 || fixture.fake.remountCount() != 1 {
		t.Fatalf("更新写入或重挂次数错误：凭据 %d，配置 %d，重挂 %d",
			credentialWrites, configurationWrites, fixture.fake.remountCount())
	}
}

func TestMountShareLinuxReportsLockTimeout(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.lockHeld = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "另一个挂载正在进行，请稍后再试" {
		t.Fatalf("文件锁超时报错 = %q (%v)", text, output)
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 0 || configurationWrites != 0 {
		t.Fatalf("拿锁失败后不应改写：凭据 %d 次，配置 %d 次", credentialWrites, configurationWrites)
	}
	assertMountShareAuditResult(t, fixture.store, "lock-timeout")
}

func TestMountShareLinuxRecoversExpiredStaleLock(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.staleLock = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("过期残留锁应被回收并完成挂载：%s", text)
	}
	if output["result"] != "first-mount-success" {
		t.Fatalf("过期锁回收后的输出 = %#v", output)
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 1 || configurationWrites != 1 {
		t.Fatalf("过期锁回收后的写入次数错误：凭据 %d 次，配置 %d 次", credentialWrites, configurationWrites)
	}
}

func TestMountShareLinuxEmptyDirectoryFailsMountTableVerification(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.mountTableMissing = true
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "Linux 挂载点验证失败，不能确认共享已可用" {
		t.Fatalf("空目录且挂载表没有条目时的报错 = %q (%v)", text, output)
	}
}

func TestMountShareLinuxUpdateKeepsOriginalsWhenAllTrialsFail(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "2.0"
		setup.remoteVersion = 0
		setup.workingVersion = "none"
	})
	_, _, credentialBefore, fstabBefore := fixture.fake.recorded()

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "SMB 协议版本 3.1.1、3.0、2.1、2.0 全部试挂失败，未写入 fstab 配置" {
		t.Fatalf("改写时试挂全败的报错 = %q (%v)", text, output)
	}
	_, _, credentialAfter, fstabAfter := fixture.fake.recorded()
	if credentialAfter != credentialBefore || strings.Join(fstabAfter, "\n") != strings.Join(fstabBefore, "\n") {
		t.Fatalf("试挂全败后原配置被破坏：\ncredential %q -> %q\nfstab %#v -> %#v",
			credentialBefore, credentialAfter, fstabBefore, fstabAfter)
	}
	credentialWrites, configurationWrites := fixture.fake.writeCounts()
	if credentialWrites != 0 || configurationWrites != 0 {
		t.Fatalf("试挂全败后不应改写正式配置：凭据 %d 次，配置 %d 次", credentialWrites, configurationWrites)
	}
}

func TestMountShareLinuxFallsBackWhenExistingProtocolFails(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "3.1.1"
		setup.remoteVersion = 0
		setup.workingVersion = "2.0"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("旧协议失效后应按序列降级试挂：%s", text)
	}
	if output["result"] != "update-success" {
		t.Fatalf("协议降级更新的输出 = %#v", output)
	}
	_, _, _, fstab := fixture.fake.recorded()
	if len(fstab) != 1 || !strings.Contains(fstab[0], "vers=2.0") {
		t.Fatalf("协议降级后的 fstab = %#v", fstab)
	}
}

func TestMountShareLinuxPreservesOwnerFstabEntries(t *testing.T) {
	ownerBefore := "UUID=11111111-2222-3333-4444-555555555555 /backup ext4 defaults 0 2"
	ownerAfter := "//owner.example/archive /owner-archive cifs credentials=/etc/owner.credentials 0 0"
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.existingAddress = "192.0.2.10"
		setup.existingProtocol = "2.0"
		setup.remoteVersion = 0
		setup.ownerFstabLines = []string{ownerBefore, ownerAfter}
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if isError {
		t.Fatalf("更新 sshmgr 条目失败：%s", text)
	}
	if output["result"] != "update-success" {
		t.Fatalf("更新输出 = %#v", output)
	}

	_, _, _, fstab := fixture.fake.recorded()
	if len(fstab) != 3 || fstab[0] != ownerBefore || fstab[2] != ownerAfter ||
		!strings.Contains(fstab[1], "x-sshmgr-share=datasets,x-sshmgr-version=1") {
		t.Fatalf("owner 手写条目没有保持原位：%#v", fstab)
	}
}

func TestMountShareLinuxAllSMBVersionsFailWritesNoFstab(t *testing.T) {
	fixture := newMountShareFixture(t, func(setup *mountShareFixtureSetup) {
		setup.workingVersion = "none"
	})

	output, text, isError := callMountShare(t, fixture.session, map[string]any{
		"share": "datasets", "server_id": fixture.targetID,
	})
	if !isError || text != "SMB 协议版本 3.1.1、3.0、2.1、2.0 全部试挂失败，未写入 fstab 配置" {
		t.Fatalf("全部协议失败的报错 = %q (%v)", text, output)
	}
	_, _, credential, fstab := fixture.fake.recorded()
	if credential != "" || len(fstab) != 0 {
		t.Fatalf("全部协议失败后不应留下配置：credential=%q fstab=%v", credential, fstab)
	}
}

func TestMountShareLinuxMissingSudoAndMountCIFS(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*mountShareFixtureSetup)
		wantErr    string
		wantResult string
	}{
		{
			name:       "缺少 sudo 凭据",
			setup:      func(setup *mountShareFixtureSetup) { setup.sudo = false },
			wantErr:    "服务器条目没有配置 sudo 凭据，请先由 owner 配置 sudo",
			wantResult: "missing-sudo",
		},
		{
			name:       "缺少 mount.cifs",
			setup:      func(setup *mountShareFixtureSetup) { setup.missingMountCIFS = true },
			wantErr:    "目标服务器缺少 mount.cifs，请先由 owner 安装 cifs-utils；挂载工具不会自动安装",
			wantResult: "missing-software",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMountShareFixture(t, testCase.setup)
			output, text, isError := callMountShare(t, fixture.session, map[string]any{
				"share": "datasets", "server_id": fixture.targetID,
			})
			if !isError || text != testCase.wantErr {
				t.Fatalf("报错 = %q (%v)，期待 %q", text, output, testCase.wantErr)
			}
			commands, inputs, credential, fstab := fixture.fake.recorded()
			for _, leakText := range []string{text, strings.Join(commands, "\n"), credential, strings.Join(fstab, "\n")} {
				if strings.Contains(leakText, mountShareTestPassword) {
					t.Fatalf("失败路径泄漏挂载密码：%s", leakText)
				}
			}
			if testCase.name == "缺少 sudo 凭据" &&
				(len(commands) != 1 || strings.TrimSpace(commands[0]) != "uname -s") {
				t.Fatalf("缺少 sudo 时应先识别系统并立即停止：%v", commands)
			}
			if testCase.name == "缺少 mount.cifs" && (len(inputs) != 3 || credential != "") {
				t.Fatalf("缺少 mount.cifs 时不应生成凭据文件：inputs=%q credential=%q", inputs, credential)
			}
			rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != "error" {
				t.Fatalf("失败审计 = %+v", rows)
			}
			var summary map[string]any
			if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
				t.Fatal(err)
			}
			if summary["result"] != testCase.wantResult {
				t.Fatalf("%s 的审计分类 = %v，期待 %q（摘要 %#v）",
					testCase.name, summary["result"], testCase.wantResult, summary)
			}
		})
	}
}

func TestMountShareAuthorizationFailures(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*mountShareFixtureSetup)
		share      string
		wantErr    string
		wantResult string
		// wantNoVersion 表示共享不可见时审计摘要不应携带版本字段。
		wantNoVersion bool
	}{
		{
			name: "服务器不在 profile 内", share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.targetOutOfProfile = true
			},
			wantErr:    "共享存储或服务器不存在",
			wantResult: "not-found",
			// 服务器不在 profile 内时共享本身可见，版本号仍记录真实值。
		},
		{
			name: "共享对 profile 不可见", share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.shareInvisible = true
			},
			wantErr:       "共享存储或服务器不存在",
			wantResult:    "not-found",
			wantNoVersion: true,
		},
		{
			name: "服务器不在可写入列表内", share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.notWritable = true
			},
			wantErr:    "服务器不在挂载密码机密项的可写入服务器列表内，请先由 owner 把服务器加入该机密项的可写入服务器列表",
			wantResult: "not-in-writable-servers",
		},
		{
			name: "Windows 服务器", share: "datasets",
			setup: func(setup *mountShareFixtureSetup) {
				setup.windowsTarget = true
			},
			wantErr:    "Windows 服务器不能持久挂载共享存储；网络盘符由每条命令执行前的会话内注入提供，不需要 mount_share",
			wantResult: "windows-unsupported",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMountShareFixture(t, testCase.setup)
			output, text, isError := callMountShare(t, fixture.session, map[string]any{
				"share": testCase.share, "server_id": fixture.targetID,
			})
			if !isError || text != testCase.wantErr {
				t.Fatalf("报错 = %q (%v)，期待 %q", text, output, testCase.wantErr)
			}
			commands, _, credential, fstab := fixture.fake.recorded()
			if len(commands) != 0 || credential != "" || len(fstab) != 0 {
				t.Fatalf("授权失败不应触达目标服务器：%v %q %v", commands, credential, fstab)
			}
			rows, err := fixture.store.QueryAudit(store.AuditFilter{Actions: []string{"mount-share"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != "error" ||
				strings.Contains(rows[0].Command, mountShareTestPassword) {
				t.Fatalf("失败审计 = %+v", rows)
			}
			var summary map[string]any
			if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
				t.Fatal(err)
			}
			if summary["result"] != testCase.wantResult {
				t.Fatalf("%s 的审计分类 = %v，期待 %q（摘要 %#v）",
					testCase.name, summary["result"], testCase.wantResult, summary)
			}
			if _, hasVersion := summary["version"]; testCase.wantNoVersion && hasVersion {
				t.Fatalf("%s 共享不可见时审计不应携带版本字段：%#v", testCase.name, summary)
			}
		})
	}
}
