package softwareinventory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// CollectionSubmission 是客户端实例采集后要交给权威端的结果。失败结果保
// 留 Items 为空与 Error 原因，成功与不完整结果带全部实测条目。
type CollectionSubmission struct {
	DeviceID          string                          `json:"device_id"`
	DeviceName        string                          `json:"device_name"`
	ServerID          string                          `json:"server_id"`
	Sudo              bool                            `json:"sudo"`
	CommandSetVersion string                          `json:"command_set_version"`
	SystemType        string                          `json:"system_type"`
	Incomplete        bool                            `json:"incomplete"`
	Notes             []string                        `json:"notes,omitempty"`
	Items             []models.SoftwareObservedItem   `json:"items"`
	Failed            bool                            `json:"failed"`
	Error             string                          `json:"error,omitempty"`
	StartedAt         time.Time                       `json:"started_at"`
	EndedAt           time.Time                       `json:"ended_at"`
	ItemCounts        map[models.SoftwareCategory]int `json:"-"`
}

// CollectForSubmission 由客户端实例按权威端分派采集一台设备，只返回待上
// 交结果，不写客户端本地软件清单。SSH 执行仍使用客户端快照中的服务器条
// 目与凭据，并复用与权威端采集相同的固定命令、解析和输出上限。
func (s *Service) CollectForSubmission(
	parent context.Context, assignment models.SoftwareCollectionAssignment,
) (*CollectionSubmission, *CollectionReport, error) {
	if s == nil || s.Store == nil {
		return nil, nil, fmt.Errorf("软件清单采集服务没有绑定存储")
	}
	deviceName := strings.TrimSpace(assignment.DeviceName)
	if deviceName == "" {
		return nil, nil, fmt.Errorf("分派里的设备名不能为空")
	}
	startedAt := s.now()
	servers, err := s.Store.FullServersForDevice(deviceName)
	if err != nil {
		return nil, nil, err
	}
	var chosen *models.Server
	for _, server := range servers {
		if server.ID == assignment.ServerID || (assignment.ServerID == "" && server.Name == assignment.ServerName) {
			chosen = server
			break
		}
	}
	if chosen == nil {
		return nil, nil, fmt.Errorf("客户端缓存里找不到分派使用的服务器条目 %s", assignment.ServerName)
	}
	if assignment.Sudo && chosen.SudoCredentialID == "" {
		return nil, nil, fmt.Errorf("服务器条目 %s 打开了 sudo 采集但没有配置 sudo 凭据", chosen.Name)
	}
	shell := models.NormalizeShellType(string(chosen.ShellType))
	command, err := BuildInventoryCommand(shell)
	if err != nil {
		return nil, nil, err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultCollectTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	execResult, execErr := s.remoteExec()(ctx, chosen, shell, command, assignment.Sudo)

	var collection *ParsedCollection
	var parseErr error
	switch shell {
	case models.ShellPowerShell, models.ShellWindowsCmd:
		collection, parseErr = parseWindowsInventoryOutput(execResult.Stdout)
	default:
		collection, parseErr = parseLinuxInventoryOutput(execResult.Stdout)
	}
	if parseErr != nil {
		reason := fmt.Sprintf("固定命令集输出无法解析：%v", parseErr)
		if execErr != nil {
			reason = fmt.Sprintf("固定命令执行失败：%v", execErr)
		}
		return s.failedSubmission(assignment, chosen, reason, startedAt), nil, nil
	}
	if execResult.TimedOut {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "固定命令执行超时")
	}
	if execErr != nil {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, fmt.Sprintf("固定命令执行中断：%v", execErr))
	}
	if execResult.Truncated {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "输出超过单次采集上限")
	}
	for i := range collection.Items {
		collection.Items[i].CollectedAt = s.now()
		sortObservedPortsByName(collection.Items[i].ListeningPorts)
	}
	submission := &CollectionSubmission{
		DeviceID: assignment.DeviceID, DeviceName: deviceName, ServerID: chosen.ID,
		Sudo: assignment.Sudo, CommandSetVersion: collection.CommandSetVersion,
		SystemType: collection.SystemType, Incomplete: collection.Incomplete,
		Notes: collection.Notes, Items: collection.Items,
		StartedAt: startedAt, EndedAt: s.now(),
		ItemCounts: itemCounts(collection.Items),
	}
	report := &CollectionReport{
		DeviceName: deviceName, ServerEntryName: chosen.Name, Sudo: assignment.Sudo,
		CommandSetVersion: collection.CommandSetVersion, SystemType: collection.SystemType,
		Incomplete: collection.Incomplete, Notes: collection.Notes,
		ItemCounts: itemCounts(collection.Items), StartedAt: startedAt, EndedAt: s.now(),
	}
	return submission, report, nil
}

func (s *Service) failedSubmission(
	assignment models.SoftwareCollectionAssignment,
	server *models.Server,
	reason string,
	startedAt time.Time,
) *CollectionSubmission {
	return &CollectionSubmission{
		DeviceID: assignment.DeviceID, DeviceName: assignment.DeviceName,
		ServerID: server.ID, Sudo: assignment.Sudo, Failed: true, Error: reason,
		Items: []models.SoftwareObservedItem{}, StartedAt: startedAt, EndedAt: s.now(),
	}
}
