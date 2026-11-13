package webuishares

import (
	"context"
	"testing"
	"time"

	"ssh-manager-mcp/internal/webui"
)

type taskTableProbe struct {
	ID    string
	State webui.ShareTaskState
	Notes []string
}

func cloneTaskTableProbe(task *taskTableProbe) *taskTableProbe {
	status := *task
	status.Notes = append([]string(nil), task.Notes...)
	return &status
}

func taskTableProbeFinished(task *taskTableProbe) bool {
	return task.State != webui.ShareTaskRunning
}

func newProbeTable(ctx context.Context) *shareTaskTable[taskTableProbe] {
	return newShareTaskTable(
		ctx, cloneTaskTableProbe, taskTableProbeFinished,
	)
}

func registerProbeTask(t *testing.T, table *shareTaskTable[taskTableProbe], state webui.ShareTaskState) string {
	t.Helper()
	id, err := newShareTaskID()
	if err != nil {
		t.Fatal(err)
	}
	table.register(id, &taskTableProbe{ID: id, State: state, Notes: []string{"原始记录"}})
	return id
}

// TestShareTaskTablePruneOnlyRemovesFinishedTasks 验证清理策略：超过保留
// 数量时只按启动顺序删除已结束任务，运行中的任务即使最旧也保留。
func TestShareTaskTablePruneOnlyRemovesFinishedTasks(t *testing.T) {
	table := newProbeTable(nil)
	runningOld := registerProbeTask(t, table, webui.ShareTaskRunning)
	runningNew := registerProbeTask(t, table, webui.ShareTaskRunning)
	finished := make([]string, 0, shareTaskRetention+3)
	for index := 0; index < shareTaskRetention+3; index++ {
		finished = append(finished, registerProbeTask(t, table, webui.ShareTaskDone))
	}

	table.update(runningNew, func(task *taskTableProbe) {})

	if got := len(table.tasks); got != shareTaskRetention {
		t.Fatalf("清理后应保留 %d 个任务，实际 %d 个", shareTaskRetention, got)
	}
	for _, id := range []string{runningOld, runningNew} {
		if _, ok := table.status(id); !ok {
			t.Fatalf("运行中的任务 %s 不应被清理", id)
		}
	}
	removed := 0
	for _, id := range finished {
		if _, ok := table.status(id); !ok {
			removed++
		}
	}
	if removed != 5 {
		t.Fatalf("应清理 5 个最旧的已结束任务，实际清理 %d 个", removed)
	}
	for _, id := range finished[:5] {
		if _, ok := table.status(id); ok {
			t.Fatalf("最旧的已结束任务 %s 应被清理", id)
		}
	}
	for _, id := range finished[5:] {
		if _, ok := table.status(id); !ok {
			t.Fatalf("未超量的已结束任务 %s 不应被清理", id)
		}
	}
}

// TestShareTaskTableLaunchDerivesCancelableContext 验证任务上下文由装配
// 处传入的生命周期上下文派生：上层取消时任务能收到取消信号。
func TestShareTaskTableLaunchDerivesCancelableContext(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	table := newProbeTable(base)
	started := make(chan struct{})
	finished := make(chan error, 1)
	table.launch(&taskTableProbe{State: webui.ShareTaskRunning}, func(_ *taskTableProbe, ctx context.Context) {
		close(started)
		<-ctx.Done()
		finished <- ctx.Err()
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("后台任务没有启动")
	}
	cancel()
	select {
	case err := <-finished:
		if err != context.Canceled {
			t.Fatalf("任务上下文应随生命周期上下文取消，实际错误：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消生命周期上下文后任务没有收到取消信号")
	}
}

// TestShareTaskTableStatusReturnsIsolatedCopy 验证读取状态得到深拷贝副
// 本，调用方修改副本不影响任务表里的原状态。
func TestShareTaskTableStatusReturnsIsolatedCopy(t *testing.T) {
	table := newProbeTable(nil)
	id := registerProbeTask(t, table, webui.ShareTaskDone)
	status, ok := table.status(id)
	if !ok {
		t.Fatal("刚登记的任务应能读取状态")
	}
	status.Notes[0] = "被调用方修改"
	if stored, _ := table.status(id); stored.Notes[0] != "原始记录" {
		t.Fatalf("读取状态应返回独立副本，实际共享了切片：%v", stored.Notes)
	}
}
