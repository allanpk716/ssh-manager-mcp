package webuishares

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

type shareRemoteTaskStore struct {
	open         func() (*store.Store, func(), error)
	tasks        *shareTaskTable[webui.ShareRemoteTaskStatus]
	checkMu      sync.Mutex
	latestChecks map[string][]webui.ShareCheckResult
}

// RemoteTasksFromStore 返回绑定存储打开函数的共享存储远端任务视图。任
// 务只保存在当前进程内存里，后台执行时重新打开存储，避免长期占用句柄。
func RemoteTasksFromStore(
	open func() (*store.Store, func(), error),
) webui.ShareRemoteTasks {
	return RemoteTasksFromStoreWithContext(open, context.Background())
}

// RemoteTasksFromStoreWithContext 与 RemoteTasksFromStore 相同，并让任务
// 继承 Web UI 服务的关闭上下文。
func RemoteTasksFromStoreWithContext(
	open func() (*store.Store, func(), error), ctx context.Context,
) webui.ShareRemoteTasks {
	return &shareRemoteTaskStore{
		open: open,
		tasks: newShareTaskTable(
			ctx, cloneShareRemoteTaskStatus,
			func(task *webui.ShareRemoteTaskStatus) bool { return shareTaskFinished(task.State) },
		),
		latestChecks: map[string][]webui.ShareCheckResult{},
	}
}

func cloneShareRemoteTaskStatus(task *webui.ShareRemoteTaskStatus) *webui.ShareRemoteTaskStatus {
	status := *task
	status.CheckResult = append([]webui.ShareCheckResult(nil), task.CheckResult...)
	if task.MountResult != nil {
		result := *task.MountResult
		result.Notices = append([]string(nil), task.MountResult.Notices...)
		status.MountResult = &result
	}
	return &status
}

func (s *shareRemoteTaskStore) StartShareRemoteTask(
	request webui.ShareRemoteTaskRequest,
) (string, error) {
	switch request.Operation {
	case webui.ShareRemoteOperationCheck, webui.ShareRemoteOperationMount:
	default:
		return "", fmt.Errorf("未知的共享存储远端操作：%s", request.Operation)
	}
	if request.ShareID == "" {
		return "", fmt.Errorf("共享存储标识不能为空")
	}
	if request.Operation == webui.ShareRemoteOperationMount && request.ServerID == "" {
		return "", fmt.Errorf("服务器标识不能为空")
	}
	id, err := newShareTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ShareRemoteTaskStatus{
		ID: id, Operation: request.Operation, ShareID: request.ShareID,
		ServerID: request.ServerID, State: webui.ShareTaskRunning,
	}
	s.tasks.register(id, task)
	s.tasks.launch(task, func(task *webui.ShareRemoteTaskStatus, ctx context.Context) {
		s.run(task, request, ctx)
	})
	return id, nil
}

func (s *shareRemoteTaskStore) ShareRemoteTaskStatus(
	taskID string,
) (*webui.ShareRemoteTaskStatus, bool) {
	return s.tasks.status(taskID)
}

// LatestShareCheck 返回一处共享存储最近一次完成的现场探测结果。结果只
// 保存在当前进程内存里，进程重启后需要重新探测。
func (s *shareRemoteTaskStore) LatestShareCheck(shareID string) ([]webui.ShareCheckResult, bool) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	results, ok := s.latestChecks[shareID]
	if !ok {
		return nil, false
	}
	return append([]webui.ShareCheckResult(nil), results...), true
}

func (s *shareRemoteTaskStore) run(
	task *webui.ShareRemoteTaskStatus, request webui.ShareRemoteTaskRequest, ctx context.Context,
) {
	switch request.Operation {
	case webui.ShareRemoteOperationCheck:
		s.runCheck(task, request, ctx)
	case webui.ShareRemoteOperationMount:
		s.runMount(task, request, ctx)
	}
}

func (s *shareRemoteTaskStore) runCheck(
	task *webui.ShareRemoteTaskStatus, request webui.ShareRemoteTaskRequest, ctx context.Context,
) {
	var report *sharemanage.CheckReport
	err := withStore(s.open, func(st *store.Store) error {
		var operationErr error
		report, operationErr = sharemanage.New(st).Check(ctx, sharemanage.CheckRequest{
			Share: request.ShareID,
		})
		return operationErr
	})
	if err != nil {
		s.tasks.update(task.ID, func(task *webui.ShareRemoteTaskStatus) {
			task.State = webui.ShareTaskError
			task.Message = "现场探测失败，请稍后重试。"
		})
		return
	}
	results := make([]webui.ShareCheckResult, 0, len(report.Servers))
	for _, result := range report.Servers {
		results = append(results, webui.ShareCheckResult{
			ServerID: result.ServerID, ServerName: result.ServerName,
			System: result.System, Status: result.Status, Reason: result.Reason,
			Notice: result.Notice, Managed: result.Managed, Version: result.Version,
			Restricted: result.Restricted,
			ObservedAt: result.ObservedAt.Format(time.RFC3339),
		})
	}
	s.checkMu.Lock()
	s.latestChecks[request.ShareID] = append([]webui.ShareCheckResult(nil), results...)
	s.checkMu.Unlock()
	s.tasks.update(task.ID, func(task *webui.ShareRemoteTaskStatus) {
		task.State = webui.ShareTaskDone
		task.ShareName = report.ShareName
		task.CheckResult = results
	})
}

func (s *shareRemoteTaskStore) runMount(
	task *webui.ShareRemoteTaskStatus, request webui.ShareRemoteTaskRequest, ctx context.Context,
) {
	var result *sharemanage.MountResult
	err := withStore(s.open, func(st *store.Store) error {
		var operationErr error
		result, operationErr = sharemanage.New(st).Mount(ctx, sharemanage.MountRequest{
			Share: request.ShareID, Server: request.ServerID,
			Source: sharemanage.WebUISource(request.Source.RemoteAddress),
		})
		return operationErr
	})
	if err != nil {
		s.tasks.update(task.ID, func(task *webui.ShareRemoteTaskStatus) {
			task.State = webui.ShareTaskError
			task.Message = err.Error()
		})
		return
	}
	s.tasks.update(task.ID, func(task *webui.ShareRemoteTaskStatus) {
		task.State = webui.ShareTaskDone
		task.ShareName = result.ShareName
		task.MountResult = &webui.ShareMountResult{
			ShareName: result.ShareName, ServerID: result.ServerID,
			ServerName: result.ServerName, Version: result.Version,
			MountPoint: result.MountPoint, Network: result.Network,
			NetworkSequence: result.NetworkSequence, Action: result.Action,
			Notices: append([]string(nil), result.Notices...),
		}
	})
}
