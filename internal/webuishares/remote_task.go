package webuishares

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

const shareRemoteTaskRetention = 64

type shareRemoteTaskStore struct {
	ctx   context.Context
	open  func() (*store.Store, func(), error)
	mu    sync.Mutex
	tasks map[string]*webui.ShareRemoteTaskStatus
	order []string
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
	if ctx == nil {
		ctx = context.Background()
	}
	return &shareRemoteTaskStore{
		ctx: ctx, open: open, tasks: map[string]*webui.ShareRemoteTaskStatus{},
	}
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
	id, err := newShareRemoteTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ShareRemoteTaskStatus{
		ID: id, Operation: request.Operation, ShareID: request.ShareID,
		ServerID: request.ServerID, State: webui.ShareRemoteTaskRunning,
	}
	s.mu.Lock()
	s.tasks[id] = task
	s.order = append(s.order, id)
	s.mu.Unlock()
	go s.run(task, request)
	return id, nil
}

func (s *shareRemoteTaskStore) ShareRemoteTaskStatus(
	taskID string,
) (*webui.ShareRemoteTaskStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return nil, false
	}
	status := *task
	status.CheckResult = append([]webui.ShareCheckResult(nil), task.CheckResult...)
	if task.MountResult != nil {
		result := *task.MountResult
		result.Notices = append([]string(nil), task.MountResult.Notices...)
		status.MountResult = &result
	}
	return &status, true
}

func (s *shareRemoteTaskStore) run(
	task *webui.ShareRemoteTaskStatus, request webui.ShareRemoteTaskRequest,
) {
	switch request.Operation {
	case webui.ShareRemoteOperationCheck:
		s.runCheck(task, request)
	case webui.ShareRemoteOperationMount:
		s.runMount(task, request)
	}
}

func (s *shareRemoteTaskStore) runCheck(
	task *webui.ShareRemoteTaskStatus, request webui.ShareRemoteTaskRequest,
) {
	var report *sharemanage.CheckReport
	err := withStore(s.open, func(st *store.Store) error {
		var operationErr error
		report, operationErr = sharemanage.New(st).Check(s.ctx, sharemanage.CheckRequest{
			Share: request.ShareID,
		})
		return operationErr
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		task.State = webui.ShareRemoteTaskError
		task.Message = "现场探测失败，请稍后重试。"
		s.pruneLocked()
		return
	}
	task.State = webui.ShareRemoteTaskDone
	task.ShareName = report.ShareName
	task.CheckResult = make([]webui.ShareCheckResult, 0, len(report.Servers))
	for _, result := range report.Servers {
		task.CheckResult = append(task.CheckResult, webui.ShareCheckResult{
			ServerID: result.ServerID, ServerName: result.ServerName,
			System: result.System, Status: result.Status, Reason: result.Reason,
			Notice: result.Notice, Managed: result.Managed, Version: result.Version,
			Restricted: result.Restricted,
			ObservedAt: result.ObservedAt.Format(time.RFC3339),
		})
	}
	s.pruneLocked()
}

func (s *shareRemoteTaskStore) runMount(
	task *webui.ShareRemoteTaskStatus, request webui.ShareRemoteTaskRequest,
) {
	var result *sharemanage.MountResult
	err := withStore(s.open, func(st *store.Store) error {
		var operationErr error
		result, operationErr = sharemanage.New(st).Mount(s.ctx, sharemanage.MountRequest{
			Share: request.ShareID, Server: request.ServerID,
			Source: sharemanage.WebUISource(request.Source.RemoteAddress),
		})
		return operationErr
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		task.State = webui.ShareRemoteTaskError
		task.Message = err.Error()
		s.pruneLocked()
		return
	}
	task.State = webui.ShareRemoteTaskDone
	task.ShareName = result.ShareName
	task.MountResult = &webui.ShareMountResult{
		ShareName: result.ShareName, ServerID: result.ServerID,
		ServerName: result.ServerName, Version: result.Version,
		MountPoint: result.MountPoint, Network: result.Network,
		NetworkSequence: result.NetworkSequence, Action: result.Action,
		Notices: append([]string(nil), result.Notices...),
	}
	s.pruneLocked()
}

func (s *shareRemoteTaskStore) pruneLocked() {
	if len(s.tasks) <= shareRemoteTaskRetention {
		return
	}
	remove := len(s.tasks) - shareRemoteTaskRetention
	for _, id := range s.order {
		if remove == 0 {
			break
		}
		if _, ok := s.tasks[id]; !ok {
			continue
		}
		delete(s.tasks, id)
		remove--
	}
	kept := make([]string, 0, len(s.tasks))
	for _, id := range s.order {
		if _, ok := s.tasks[id]; ok {
			kept = append(kept, id)
		}
	}
	s.order = kept
}

func newShareRemoteTaskID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("生成共享存储远端任务标识失败：%w", err)
	}
	return hex.EncodeToString(data[:]), nil
}
