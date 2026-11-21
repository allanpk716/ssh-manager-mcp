package softwareinventory

import (
	"context"
	"sync"
)

// DeviceCollectionLocks 是权威端进程内「同一台设备同一时间只被一方采集」
// 的互斥表，供每日定时采集与「立即采集轮次」共享，避免同一台设备被并发
// 重复采集。锁只在进程内存里，不落库；进程重启自然全部回到空闲。
type DeviceCollectionLocks struct {
	mu sync.Mutex
	// held 记录每台设备当前是否被持有。
	held map[string]bool
	// queue 是每台设备的先来后到等待队列，元素在获得持有权时被关闭。
	queue map[string][]chan struct{}
}

// NewDeviceCollectionLocks 构造一个空的每设备采集互斥表。
func NewDeviceCollectionLocks() *DeviceCollectionLocks {
	return &DeviceCollectionLocks{
		held:  map[string]bool{},
		queue: map[string][]chan struct{}{},
	}
}

// Acquire 拿一台设备的采集权。设备空闲时立即拿到并返回 true；被他人持有
// 时按先来后到等待，直到拿到（true）或 ctx 结束（false）。
func (l *DeviceCollectionLocks) Acquire(ctx context.Context, device string) bool {
	if ctx.Err() != nil {
		return false
	}
	l.mu.Lock()
	if !l.held[device] {
		l.held[device] = true
		l.mu.Unlock()
		return true
	}
	waiter := make(chan struct{})
	l.queue[device] = append(l.queue[device], waiter)
	l.mu.Unlock()
	select {
	case <-waiter:
		return true
	case <-ctx.Done():
		// 把自己从等待队列摘除。若此刻已经被移交持有权（队列里找不到自
		// 己），要补一次 Release 归还，避免持有权泄漏。
		l.mu.Lock()
		for i, queued := range l.queue[device] {
			if queued == waiter {
				l.queue[device] = append(l.queue[device][:i], l.queue[device][i+1:]...)
				l.mu.Unlock()
				return false
			}
		}
		l.mu.Unlock()
		l.Release(device)
		return false
	}
}

// Release 释放一台设备的采集权；未持有时是空操作。等待队列非空时把持有
// 权直接移交给队头，队头从 Acquire 的等待中拿到并负责之后的释放。
func (l *DeviceCollectionLocks) Release(device string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held[device] {
		return
	}
	if queue := l.queue[device]; len(queue) > 0 {
		l.queue[device] = queue[1:]
		close(queue[0])
		return
	}
	delete(l.held, device)
}
