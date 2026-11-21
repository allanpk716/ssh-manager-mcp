package softwareinventory

import (
	"context"
	"testing"
	"time"
)

// TestDeviceCollectionLocksAcquireAndRelease 覆盖基本拿放：空闲时直接拿
// 到，释放后可再次拿到；未持有时释放是空操作；不同设备互不影响。
func TestDeviceCollectionLocksAcquireAndRelease(t *testing.T) {
	locks := NewDeviceCollectionLocks()
	ctx := context.Background()
	if !locks.Acquire(ctx, "工作站") {
		t.Fatal("空闲设备应能直接拿到采集权")
	}
	locks.Release("工作站")
	if !locks.Acquire(ctx, "工作站") {
		t.Fatal("释放后应能再次拿到采集权")
	}
	locks.Release("工作站")
	locks.Release("工作站")
	if !locks.Acquire(ctx, "另一台") {
		t.Fatal("别的设备的持有状态不应影响这台设备")
	}
	locks.Release("另一台")
}

// TestDeviceCollectionLocksWaiterBlocksUntilRelease 验证被他人持有时等待
// 者阻塞，Release 后按先来后到依次拿到。
func TestDeviceCollectionLocksWaiterBlocksUntilRelease(t *testing.T) {
	locks := NewDeviceCollectionLocks()
	ctx := context.Background()
	if !locks.Acquire(ctx, "工作站") {
		t.Fatal("第一个调用方应直接拿到采集权")
	}
	acquired := make(chan int, 2)
	go func() {
		if locks.Acquire(ctx, "工作站") {
			acquired <- 1
			locks.Release("工作站")
		}
	}()
	// 等第一个等待者确定排进队列后再启动第二个，保证先来后到的顺序可断言。
	time.Sleep(50 * time.Millisecond)
	go func() {
		if locks.Acquire(ctx, "工作站") {
			acquired <- 2
			locks.Release("工作站")
		}
	}()
	select {
	case <-acquired:
		t.Fatal("持有期间等待者不应拿到采集权")
	case <-time.After(50 * time.Millisecond):
	}
	locks.Release("工作站")
	first := <-acquired
	second := <-acquired
	if first != 1 || second != 2 {
		t.Fatalf("等待者应按先来后到拿到，实际顺序 %d、%d", first, second)
	}
	// 全部等待者拿放完毕后锁应回到空闲。
	if !locks.Acquire(ctx, "工作站") {
		t.Fatal("等待者全部放回后应能再次拿到采集权")
	}
	locks.Release("工作站")
}

// TestDeviceCollectionLocksContextCancel 验证上下文结束时等待者返回
// false，且不破坏后续获取；上下文结束与持有权移交同时发生时，等待者也会
// 把拿到的持有权归还，锁最终回到空闲。
func TestDeviceCollectionLocksContextCancel(t *testing.T) {
	locks := NewDeviceCollectionLocks()
	if !locks.Acquire(context.Background(), "工作站") {
		t.Fatal("主调用方应先拿到采集权")
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 无论走到「拿到后取消」还是「取消后拿到」分支，都要放回去。
		if locks.Acquire(waitCtx, "工作站") {
			locks.Release("工作站")
		}
	}()
	// 等等待者排进队列，再取消它的上下文并释放持有权。
	time.Sleep(50 * time.Millisecond)
	cancel()
	locks.Release("工作站")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("上下文结束后等待者没有及时返回")
	}
	// 取消的等待者不残留：新的获取应当直接成功。
	if !locks.Acquire(context.Background(), "工作站") {
		t.Fatal("取消的等待者不应破坏后续获取")
	}
	locks.Release("工作站")

	// 等待时上下文已结束：应立即返回 false，不入队。
	if locks.Acquire(waitCtx, "工作站") {
		t.Fatal("上下文已结束时获取应返回 false")
	}
	if !locks.Acquire(context.Background(), "工作站") {
		t.Fatal("被拒绝的获取不应占住采集权")
	}
	locks.Release("工作站")
}
