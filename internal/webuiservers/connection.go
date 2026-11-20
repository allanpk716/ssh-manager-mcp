package webuiservers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/webui"
)

// connectionTaskRetention 是已完成连接测试状态的保留数量；结果页只需要轮询
// 最近任务，过旧状态直接丢弃。
const connectionTaskRetention = 32

// connectionTestIncompleteMessage 是打不开加密库、找不到服务器条目、读取
// 主机密钥锚失败这类提前退出错误的固定白话说明；不携带底层错误，避免把排
// 查文本变成秘密暴露面。
const connectionTestIncompleteMessage = "连接测试没有完成：读取加密库或服务器条目失败，请稍后重试。"

// connectionTester 在进程内维护新增服务器的连接测试任务。它沿用拓扑探测
// 页面的「启动任务、后台执行、页面轮询」模式，实际连接与锚定全部委托服务
// 器管理模块的统一拨号路径。
type connectionTester struct {
	ctx   context.Context
	open  managerOpener
	mu    sync.Mutex
	tasks map[string]*webui.ServerConnectionTestStatus
	order []string
}

// ConnectionTesterFromManager 返回绑定固定服务器管理模块的连接测试入口。
func ConnectionTesterFromManager(manager *servermanage.Manager) webui.ServerConnectionTester {
	return ConnectionTesterFromManagerWithContext(manager, context.Background())
}

// ConnectionTesterFromManagerWithContext 让后台连接测试跟随 Web UI 服务的
// 关闭上下文取消。
func ConnectionTesterFromManagerWithContext(
	manager *servermanage.Manager, ctx context.Context,
) webui.ServerConnectionTester {
	return newConnectionTester(fixedManagerOpener(manager), ctx)
}

// ConnectionTesterFromOpener 每个任务重新打开存储，供单机回环 Web UI 使
// 用，避免后台任务长期占用存储句柄。
func ConnectionTesterFromOpener(open managerOpener) webui.ServerConnectionTester {
	return ConnectionTesterFromOpenerWithContext(open, context.Background())
}

// ConnectionTesterFromOpenerWithContext 与 ConnectionTesterFromOpener 相同，
// 并让任务继承调用方的关闭上下文。
func ConnectionTesterFromOpenerWithContext(open managerOpener, ctx context.Context) webui.ServerConnectionTester {
	return newConnectionTester(open, ctx)
}

func newConnectionTester(open managerOpener, ctx context.Context) *connectionTester {
	if ctx == nil {
		ctx = context.Background()
	}
	return &connectionTester{ctx: ctx, open: open, tasks: map[string]*webui.ServerConnectionTestStatus{}}
}

// StartServerTest 立即登记并启动后台任务；它不等待连接完成。
func (r *connectionTester) StartServerTest(
	serverID string, probeShell bool, source webui.ServerWriteSource, anchor webui.ServerConnectionAnchor,
) (string, error) {
	if serverID == "" {
		return "", fmt.Errorf("服务器标识不能为空")
	}
	id, err := randomConnectionTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ServerConnectionTestStatus{
		ID: id, State: webui.ServerConnectionTestRunning, ServerID: serverID,
		ProbeShell: probeShell, AnchorCopied: anchor.Copied,
		AnchorCopyFromServerName: anchor.FromServerName, AnchorCopyConflict: anchor.Conflict,
	}
	r.mu.Lock()
	r.tasks[id] = task
	r.order = append(r.order, id)
	r.mu.Unlock()
	go r.runConnectionTest(task, source)
	return id, nil
}

// ServerTestStatus 返回任务状态的副本。
func (r *connectionTester) ServerTestStatus(taskID string) (*webui.ServerConnectionTestStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[taskID]
	if !ok {
		return nil, false
	}
	status := *task
	status.ConnectionNotes = append([]string(nil), task.ConnectionNotes...)
	if task.Result != nil {
		result := *task.Result
		result.Attempts = append([]webui.ServerConnectionTestAttemptView(nil), task.Result.Attempts...)
		status.Result = &result
	}
	return &status, true
}

func (r *connectionTester) runConnectionTest(task *webui.ServerConnectionTestStatus, source webui.ServerWriteSource) {
	var result *servermanage.ConnectionTestResult
	var err error
	operation := func(manager *servermanage.Manager) error {
		result, err = manager.TestConnection(r.ctx, servermanage.ConnectionTestRequest{
			Identifier: task.ServerID, ProbeShell: task.ProbeShell,
			Source: servermanage.WebUISource(source.RemoteAddress),
		})
		return err
	}
	runErr := withManager(r.open, operation)
	r.finishConnectionTest(task, result, runErr)
}

func (r *connectionTester) finishConnectionTest(
	task *webui.ServerConnectionTestStatus, result *servermanage.ConnectionTestResult, err error,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if result != nil {
		task.ConnectionNotes = append([]string(nil), result.ConnectionNotes...)
		task.AnchorFingerprint = result.AnchorFingerprint
		task.AnchorResult = result.AnchorResult
		task.HandshakeAddress = result.HandshakeAddress
		task.ShellDetected = string(result.ShellDetected)
		task.ShellEvidence = result.ShellEvidence
		task.Result = webui.NewServerConnectionTestResultView(result.ServerConnectionTestResult)
		task.Result.Persisted = result.Persisted
	}
	if err != nil {
		task.State = webui.ServerConnectionTestFailed
		switch {
		case errors.Is(err, context.Canceled):
			task.Message = "连接被取消"
			task.Result = &webui.ServerConnectionTestResultView{
				State: webui.ServerConnectionTestFailed, FailureLabel: "连接被取消",
			}
		case task.Result != nil && !task.Result.CompletedAt.IsZero():
			// 已有完成结果时按结构化白话分类展示；成功结果的分类为空，说明
			// 只有审计等后续记录失败，页面继续显示已连上的事实。
			task.Message = task.Result.FailureLabel
		default:
			task.Message = connectionTestIncompleteMessage
			task.Result = &webui.ServerConnectionTestResultView{
				State: webui.ServerConnectionTestFailed, FailureLabel: "其他连接失败",
			}
		}
		if task.ProbeShell {
			task.ShellNotice = "连接失败，shell 未验证；条目保持默认 POSIX 标注。"
		}
		r.pruneConnectionTasksLocked()
		return
	}
	task.State = webui.ServerConnectionTestSucceeded
	if task.ProbeShell && task.ShellDetected == "" {
		task.ShellNotice = "无法判定 shell 类型，未猜测；请人工确认后修改标注。"
	}
	r.pruneConnectionTasksLocked()
}

func (r *connectionTester) pruneConnectionTasksLocked() {
	if len(r.tasks) <= connectionTaskRetention {
		return
	}
	for len(r.order) > 0 {
		id := r.order[0]
		r.order = r.order[1:]
		if task := r.tasks[id]; task != nil && task.State == webui.ServerConnectionTestRunning {
			r.order = append(r.order, id)
			break
		}
		delete(r.tasks, id)
		if len(r.tasks) <= connectionTaskRetention {
			return
		}
	}
}

func randomConnectionTaskID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成连接测试任务标识失败：%w", err)
	}
	return "connect-" + hex.EncodeToString(buffer), nil
}
