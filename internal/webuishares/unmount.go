package webuishares

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// unmountTaskRetention 是已完成卸载任务的状态保留数量；页面轮询只需要最
// 近的任务，进程重启后任务表自然清空。
const unmountTaskRetention = 32

// unmountRunner 把共享存储详情页的单台卸载请求接到共享存储管理模块，并
// 在进程内维护「启动任务、轮询任务状态」的任务表。远端卸载、占用进程检
// 测与强制卸载全部复用共享存储管理模块，不另写远端脚本。
type unmountRunner struct {
	open  func() (*store.Store, func(), error)
	mu    sync.Mutex
	tasks map[string]*webui.ShareUnmountTaskStatus
}

// UnmountTasksFromStore 把按请求打开的存储接成共享存储单台卸载任务视图。
func UnmountTasksFromStore(open func() (*store.Store, func(), error)) webui.ShareUnmountRunner {
	return &unmountRunner{open: open, tasks: map[string]*webui.ShareUnmountTaskStatus{}}
}

// StartShareUnmount 校验输入后启动后台卸载任务并立即返回任务标识。
func (r *unmountRunner) StartShareUnmount(
	request webui.ShareUnmountRequest, source webui.ShareWriteSource,
) (string, error) {
	if request.ShareID == "" || request.ShareName == "" ||
		request.ServerID == "" || request.ServerName == "" {
		return "", errors.New("共享存储与服务器条目不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, err := randomUnmountTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ShareUnmountTaskStatus{
		ID: id, State: "running",
		ShareID: request.ShareID, ShareName: request.ShareName,
		ServerID: request.ServerID, ServerName: request.ServerName, Force: request.Force,
	}
	r.tasks[id] = task
	go r.runUnmount(task, request, source)
	return id, nil
}

// ShareUnmountStatus 返回任务状态；任务不存在或已被清理时返回 false。
func (r *unmountRunner) ShareUnmountStatus(taskID string) (*webui.ShareUnmountTaskStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[taskID]
	if !ok {
		return nil, false
	}
	status := *task
	status.Notices = append([]string(nil), task.Notices...)
	status.BusyProcesses = append([]webui.ShareUnmountBusyProcess(nil), task.BusyProcesses...)
	return &status, true
}

// runUnmount 在后台执行一次单台卸载。共享存储管理模块负责远端行为与审
// 计；这里只把结论换算成页面任务状态。
func (r *unmountRunner) runUnmount(
	task *webui.ShareUnmountTaskStatus,
	request webui.ShareUnmountRequest,
	source webui.ShareWriteSource,
) {
	finish := func(result, failureMessage string, notices []string, busy []webui.ShareUnmountBusyProcess, version int64) {
		r.mu.Lock()
		task.State = "done"
		task.Result = result
		task.FailureMessage = failureMessage
		task.Notices = notices
		task.BusyProcesses = busy
		task.Version = version
		r.pruneTasksLocked()
		r.mu.Unlock()
	}
	st, closeStore, err := r.open()
	if err != nil {
		finish("error", "打开共享存储数据失败："+err.Error(), nil, nil, 0)
		return
	}
	result, unmountErr := sharemanage.New(st).Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: request.ShareName, Server: request.ServerName, Force: request.Force,
		Source: sharemanage.WebUISource(source.RemoteAddress),
	})
	// 先关闭后台连接，再把任务标记为完成，避免页面读取结果时与后台连接
	// 的数据库锁竞争。
	closeStore()
	if unmountErr != nil {
		resultCategory := unmountErrorResult(unmountErr, result)
		finish(resultCategory, unmountErr.Error(), nil, nil, 0)
		return
	}
	notices := append([]string(nil), result.Notices...)
	busy := make([]webui.ShareUnmountBusyProcess, 0, len(result.BusyProcesses))
	for _, process := range result.BusyProcesses {
		busy = append(busy, webui.ShareUnmountBusyProcess{
			ProcessID: process.ProcessID, User: process.User, Command: process.Command,
		})
	}
	finish(result.Result, "", notices, busy, result.Version)
}

// unmountErrorResult 把共享存储管理模块的错误换算成固定结论分类，与审计
// 行的结论分类保持同一套取值。
func unmountErrorResult(err error, result *sharemanage.UnmountResult) string {
	switch {
	case errors.Is(err, sharemanage.ErrWindowsUnmountUnsupported):
		return "windows-unsupported"
	case errors.Is(err, sharemount.ErrUnmountSSH):
		return "ssh-error"
	case errors.Is(err, sharemount.ErrNoSudo):
		return "missing-sudo"
	case errors.Is(err, sharemount.ErrUnmountUnsupportedSystem):
		return "unsupported-system"
	case errors.Is(err, sharemount.ErrUnmountNotManaged):
		return "not-managed"
	case errors.Is(err, sharemount.ErrUnmountLockTimeout):
		return "lock-timeout"
	case errors.Is(err, sharemount.ErrUnmountFailed):
		return "unmount-failed"
	case errors.Is(err, sharemount.ErrUnmountOutputInvalid):
		return "output-invalid"
	case errors.Is(err, sharemount.ErrUnmountRemoteVersionNewer):
		return "remote-version-newer"
	case result != nil && result.Result != "":
		return result.Result
	default:
		return "error"
	}
}

func (r *unmountRunner) pruneTasksLocked() {
	if len(r.tasks) <= unmountTaskRetention {
		return
	}
	for id, task := range r.tasks {
		if task.State != "running" {
			delete(r.tasks, id)
			if len(r.tasks) <= unmountTaskRetention {
				return
			}
		}
	}
}

func randomUnmountTaskID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成卸载任务标识失败：%w", err)
	}
	return hex.EncodeToString(buffer), nil
}
