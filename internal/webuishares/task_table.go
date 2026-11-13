package webuishares

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"ssh-manager-mcp/internal/webui"
)

// shareTaskRetention 是每类共享存储远端任务统一保留的数量。页面轮询只需
// 要最近的任务；超过数量时只清理已结束任务，运行中的任务永远保留，进程
// 重启后任务表自然清空。
const shareTaskRetention = 64

// shareTaskFinished 按状态代号判断一个共享存储任务是否已结束。
func shareTaskFinished(state webui.ShareTaskState) bool {
	return state != webui.ShareTaskRunning
}

// shareTaskTable 是共享存储页各类远端操作共用的进程内任务表：登记任
// 务、在后台协程执行、读取状态副本，并在超过保留数量时按启动顺序清理
// 已结束任务。clone 必须把任务状态深拷贝成可安全交给调用方的副本；任务
// 后台协程使用 launch 派生的上下文，装配处取消 Web UI 生命周期上下文时
// 一并取消。
type shareTaskTable[T any] struct {
	ctx      context.Context
	mu       sync.Mutex
	tasks    map[string]*T
	order    []string
	clone    func(*T) *T
	finished func(*T) bool
}

// newShareTaskTable 构造一张任务表。ctx 是 Web UI 的生命周期上下文，传
// nil 时退回不可取消的空上下文（仅测试使用）。
func newShareTaskTable[T any](
	ctx context.Context, clone func(*T) *T, finished func(*T) bool,
) *shareTaskTable[T] {
	if ctx == nil {
		ctx = context.Background()
	}
	return &shareTaskTable[T]{
		ctx: ctx, tasks: map[string]*T{},
		clone: clone, finished: finished,
	}
}

// register 登记一个已带任务标识的任务；标识同时作为任务表键。
func (t *shareTaskTable[T]) register(id string, task *T) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tasks[id] = task
	t.order = append(t.order, id)
}

// launch 在后台协程执行任务，任务上下文由 Web UI 生命周期上下文派生，
// 任务返回时自动释放。
func (t *shareTaskTable[T]) launch(task *T, run func(task *T, ctx context.Context)) {
	ctx, cancel := context.WithCancel(t.ctx)
	go func() {
		defer cancel()
		run(task, ctx)
	}()
}

// status 返回任务状态副本；任务不存在或已被清理时返回 false。
func (t *shareTaskTable[T]) status(id string) (*T, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	task, ok := t.tasks[id]
	if !ok {
		return nil, false
	}
	return t.clone(task), true
}

// update 在持锁状态下修改任务并执行清理，保证读取方只能看到完整的任务
// 状态。任务已被清理时修改不生效。
func (t *shareTaskTable[T]) update(id string, mutate func(*T)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task, ok := t.tasks[id]; ok {
		mutate(task)
	}
	t.pruneLocked()
}

// pruneLocked 在任务超过保留数量时按启动顺序清理已结束任务；运行中的任
// 务不清理，避免页面正在轮询的任务突然消失。
func (t *shareTaskTable[T]) pruneLocked() {
	if len(t.tasks) <= shareTaskRetention {
		return
	}
	kept := make([]string, 0, len(t.order))
	for _, id := range t.order {
		task, ok := t.tasks[id]
		if !ok {
			continue
		}
		if len(t.tasks) > shareTaskRetention && t.finished(task) {
			delete(t.tasks, id)
			continue
		}
		kept = append(kept, id)
	}
	t.order = kept
}

// newShareTaskID 生成一个随机任务标识。
func newShareTaskID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("生成共享存储任务标识失败：%w", err)
	}
	return hex.EncodeToString(data[:]), nil
}
