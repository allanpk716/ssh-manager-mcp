package webuishares

import (
	"context"
	"errors"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

type revokeRunner struct {
	open  func() (*store.Store, func(), error)
	tasks *shareTaskTable[webui.ShareRevokeTaskStatus]
}

// RevokeTasksFromStore 把按请求打开的存储接成共享存储单台撤销授权任务
// 视图。任务只保存在当前进程内存里，进程重启后不排队补跑。
func RevokeTasksFromStore(
	open func() (*store.Store, func(), error),
) webui.ShareRevokeRunner {
	return RevokeTasksFromStoreWithContext(open, context.Background())
}

// RevokeTasksFromStoreWithContext 与 RevokeTasksFromStore 相同，并让任务
// 继承 Web UI 服务的关闭上下文。
func RevokeTasksFromStoreWithContext(
	open func() (*store.Store, func(), error), ctx context.Context,
) webui.ShareRevokeRunner {
	return &revokeRunner{
		open: open,
		tasks: newShareTaskTable(
			ctx, cloneShareRevokeTaskStatus,
			func(task *webui.ShareRevokeTaskStatus) bool { return shareTaskFinished(task.State) },
		),
	}
}

func cloneShareRevokeTaskStatus(task *webui.ShareRevokeTaskStatus) *webui.ShareRevokeTaskStatus {
	status := *task
	if task.Result != nil {
		result := *task.Result
		result.RemainingMountServers = append([]string(nil), task.Result.RemainingMountServers...)
		result.BusyProcesses = append([]webui.ShareRevokeBusyProcess(nil), task.Result.BusyProcesses...)
		result.Notices = append([]string(nil), task.Result.Notices...)
		status.Result = &result
	}
	return &status
}

func (r *revokeRunner) StartShareRevoke(
	request webui.ShareRevokeRequest,
) (string, error) {
	if request.ShareID == "" || request.ShareName == "" ||
		request.ServerID == "" || request.ServerName == "" {
		return "", errors.New("共享存储与服务器条目不能为空")
	}
	id, err := newShareTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ShareRevokeTaskStatus{
		ID: id, State: webui.ShareTaskRunning,
		ShareID: request.ShareID, ShareName: request.ShareName,
		ServerID: request.ServerID, ServerName: request.ServerName,
		ServerShell: request.ServerShell, MountPoint: request.MountPoint,
	}
	r.tasks.register(id, task)
	r.tasks.launch(task, func(task *webui.ShareRevokeTaskStatus, ctx context.Context) {
		r.run(task, request, ctx)
	})
	return id, nil
}

func (r *revokeRunner) ShareRevokeStatus(taskID string) (*webui.ShareRevokeTaskStatus, bool) {
	return r.tasks.status(taskID)
}

func (r *revokeRunner) run(
	task *webui.ShareRevokeTaskStatus, request webui.ShareRevokeRequest, ctx context.Context,
) {
	var result *sharemanage.RevokeResult
	err := withStore(r.open, func(st *store.Store) error {
		var operationErr error
		if request.CleanupOnly {
			unmount, cleanupErr := sharemanage.New(st).Unmount(ctx, sharemanage.UnmountRequest{
				Share: request.ShareName, Server: request.ServerID, Force: request.Force,
				Source: sharemanage.WebUISource(request.Source.RemoteAddress),
			})
			if unmount != nil {
				result = &sharemanage.RevokeResult{
					ShareName: request.ShareName, ServerID: request.ServerID,
					ServerName: request.ServerName, Result: unmount.Result,
					Version: unmount.Version, Unmount: true, Force: request.Force,
					BusyProcesses: unmount.BusyProcesses,
					Notices:       unmount.Notices,
				}
			}
			return cleanupErr
		}
		result, operationErr = sharemanage.New(st).Revoke(ctx, sharemanage.RevokeRequest{
			Share: request.ShareName, Server: request.ServerID,
			Unmount: request.Unmount, Force: request.Force,
			NewPassword: request.NewPassword,
			Source:      sharemanage.WebUISource(request.Source.RemoteAddress),
		})
		return operationErr
	})
	for index := range request.NewPassword {
		request.NewPassword[index] = 0
	}
	if result != nil {
		r.finish(task, result, err)
		return
	}
	r.tasks.update(task.ID, func(task *webui.ShareRevokeTaskStatus) {
		task.State = webui.ShareTaskError
		task.FailureMessage = "撤销授权失败，请稍后重试。"
	})
}

func (r *revokeRunner) finish(
	task *webui.ShareRevokeTaskStatus, result *sharemanage.RevokeResult, err error,
) {
	view := &webui.ShareRevokeResult{
		Result: result.Result, Version: result.Version, Unmount: result.Unmount,
		Force: result.Force, MountPreserved: result.MountPreserved,
		PasswordChanged:       result.PasswordChanged,
		RemainingMountServers: append([]string(nil), result.RemainingMountServers...),
		Notices:               append([]string(nil), result.Notices...),
	}
	for _, process := range result.BusyProcesses {
		view.BusyProcesses = append(view.BusyProcesses, webui.ShareRevokeBusyProcess{
			ProcessID: process.ProcessID, User: process.User, Command: process.Command,
		})
	}
	r.tasks.update(task.ID, func(task *webui.ShareRevokeTaskStatus) {
		task.State = webui.ShareTaskDone
		task.Result = view
		if err != nil {
			task.FailureMessage = shareRevokeFailureMessage(result.Result)
		}
	})
}

func shareRevokeFailureMessage(result string) string {
	switch result {
	case "ssh-error":
		return "连接目标服务器或执行固定撤销命令失败。"
	case "missing-sudo":
		return "服务器条目没有配置 sudo 凭据。"
	case "unsupported-system":
		return "目标服务器操作系统不支持共享存储撤销。"
	case "not-managed":
		return "目标服务器上没有这个共享的 sshmgr 挂载配置。"
	case "lock-timeout":
		return "另一个共享存储操作正在进行。"
	case "unmount-failed":
		return "卸载共享存储挂载点失败。"
	case "output-invalid":
		return "固定撤销命令返回了不合法结论。"
	case "remote-version-newer":
		return "目标服务器上的共享存储版本号更新，已停止清理。"
	default:
		return "共享存储撤销授权的远端清理未完成。"
	}
}
