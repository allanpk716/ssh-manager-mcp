package webuishares

import (
	"context"
	"errors"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// reselectRunner 把共享存储详情页的重选地址请求接到共享存储管理模块，并
// 在进程内维护「启动任务、轮询任务状态」的任务表。候选地址探测与远端切
// 换全部复用共享存储管理模块，不另写远端脚本。
type reselectRunner struct {
	open  func() (*store.Store, func(), error)
	tasks *shareTaskTable[webui.ShareReselectTaskStatus]
}

// ReselectTasksFromStore 把按请求打开的存储接成共享存储重选地址任务视
// 图。
func ReselectTasksFromStore(
	open func() (*store.Store, func(), error),
) webui.ShareReselectRunner {
	return ReselectTasksFromStoreWithContext(open, context.Background())
}

// ReselectTasksFromStoreWithContext 与 ReselectTasksFromStore 相同，并让
// 任务继承 Web UI 服务的关闭上下文。
func ReselectTasksFromStoreWithContext(
	open func() (*store.Store, func(), error), ctx context.Context,
) webui.ShareReselectRunner {
	return &reselectRunner{
		open: open,
		tasks: newShareTaskTable(
			ctx, cloneShareReselectTaskStatus,
			func(task *webui.ShareReselectTaskStatus) bool { return shareTaskFinished(task.State) },
		),
	}
}

func cloneShareReselectTaskStatus(task *webui.ShareReselectTaskStatus) *webui.ShareReselectTaskStatus {
	status := *task
	status.Notices = append([]string(nil), task.Notices...)
	status.BusyProcesses = append([]webui.ShareReselectBusyProcess(nil), task.BusyProcesses...)
	return &status
}

// StartShareReselect 校验输入后启动后台重选任务并立即返回任务标识。
func (r *reselectRunner) StartShareReselect(
	request webui.ShareReselectRequest,
) (string, error) {
	if request.ShareID == "" || request.ShareName == "" ||
		request.ServerID == "" || request.ServerName == "" {
		return "", errors.New("共享存储与服务器条目不能为空")
	}
	id, err := newShareTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ShareReselectTaskStatus{
		ID: id, State: webui.ShareTaskRunning,
		ShareID: request.ShareID, ShareName: request.ShareName,
		ServerID: request.ServerID, ServerName: request.ServerName, Force: request.Force,
	}
	r.tasks.register(id, task)
	r.tasks.launch(task, func(task *webui.ShareReselectTaskStatus, ctx context.Context) {
		r.runReselect(task, request, ctx)
	})
	return id, nil
}

// ShareReselectStatus 返回任务状态；任务不存在或已被清理时返回 false。
func (r *reselectRunner) ShareReselectStatus(taskID string) (*webui.ShareReselectTaskStatus, bool) {
	return r.tasks.status(taskID)
}

// runReselect 在后台执行一次重选地址。共享存储管理模块负责远端行为与审
// 计；这里只把结论换算成页面任务状态。
func (r *reselectRunner) runReselect(
	task *webui.ShareReselectTaskStatus,
	request webui.ShareReselectRequest,
	ctx context.Context,
) {
	finish := func(result, failureMessage string, notices []string,
		busy []webui.ShareReselectBusyProcess, version int64, network string, sequence int, physical bool) {
		r.tasks.update(task.ID, func(task *webui.ShareReselectTaskStatus) {
			task.State = webui.ShareTaskDone
			task.Result = result
			task.FailureMessage = failureMessage
			task.Notices = notices
			task.BusyProcesses = busy
			task.Version = version
			task.NetworkName = network
			task.NetworkSequence = sequence
			task.Physical = physical
		})
	}
	st, closeStore, err := r.open()
	if err != nil {
		finish("error", "打开共享存储数据失败："+err.Error(), nil, nil, 0, "", 0, false)
		return
	}
	result, reselectErr := sharemanage.New(st).Reselect(ctx, sharemanage.ReselectRequest{
		Share: request.ShareName, Server: request.ServerName, Force: request.Force,
		Source: sharemanage.WebUISource(request.Source.RemoteAddress),
	})
	// 先关闭后台连接，再把任务标记为完成，避免页面读取结果时与后台连接
	// 的数据库锁竞争。
	closeStore()
	if reselectErr != nil {
		finish(reselectErrorResult(reselectErr, result), reselectErr.Error(), nil, nil, 0, "", 0, false)
		return
	}
	notices := append([]string(nil), result.Notices...)
	busy := make([]webui.ShareReselectBusyProcess, 0, len(result.BusyProcesses))
	for _, process := range result.BusyProcesses {
		busy = append(busy, webui.ShareReselectBusyProcess{
			ProcessID: process.ProcessID, User: process.User, Command: process.Command,
		})
	}
	finish(result.Result, "", notices, busy, result.Version,
		result.NetworkName, result.NetworkSequence, result.Physical)
}

// reselectErrorResult 把共享存储管理模块的错误换算成固定结论分类，与审
// 计行的结论分类保持同一套取值。
func reselectErrorResult(err error, result *sharemanage.ReselectResult) string {
	switch {
	case errors.Is(err, sharemanage.ErrWindowsReselectUnsupported):
		return "windows-unsupported"
	case errors.Is(err, sharemanage.ErrReselectOutsideMountableRange):
		return "outside-mountable-range"
	case errors.Is(err, sharemount.ErrReselectSSH):
		return "ssh-error"
	case errors.Is(err, sharemount.ErrNoSudo):
		return "missing-sudo"
	case errors.Is(err, sharemount.ErrUnsupportedSystem):
		return "unsupported-system"
	case errors.Is(err, sharemount.ErrReselectNotManaged):
		return "not-managed"
	case errors.Is(err, sharemount.ErrReselectLockTimeout):
		return "lock-timeout"
	case errors.Is(err, sharemount.ErrReselectUnmountFailed):
		return "unmount-failed"
	case errors.Is(err, sharemount.ErrReselectOutputInvalid):
		return "output-invalid"
	case result != nil && result.Result != "":
		return result.Result
	default:
		var selection *sharemount.SelectionFailure
		if errors.As(err, &selection) {
			return "selection-failed"
		}
		var mountFailure *sharemount.MountFailure
		if errors.As(err, &mountFailure) {
			return mountFailure.Class
		}
		return "error"
	}
}
