package sshbroker

import (
	"context"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// hostKeyVerificationContext 把候选地址短超时限制到「建立连接并完成主机密
// 钥校验」这一段。主机密钥校验通过后，登录认证继续受调用方原始上下文约
// 束，而不再被这个短超时切断。
type hostKeyVerificationContext struct {
	parent    context.Context
	deadline  time.Time
	timer     *time.Timer
	release   chan struct{}
	stop      chan struct{}
	done      chan struct{}
	signalOne sync.Once
	stopOne   sync.Once

	mu       sync.Mutex
	err      error
	released bool
}

func newHostKeyVerificationContext(parent context.Context, timeout time.Duration) (*hostKeyVerificationContext, func(), func()) {
	ctx := &hostKeyVerificationContext{
		parent:   parent,
		deadline: time.Now().Add(timeout),
		timer:    time.NewTimer(timeout),
		release:  make(chan struct{}),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go func() {
		select {
		case <-parent.Done():
			ctx.finish(parent.Err())
		case <-ctx.timer.C:
			ctx.finish(context.DeadlineExceeded)
		case <-ctx.release:
			ctx.timer.Stop()
			ctx.setReleased()
			select {
			case <-parent.Done():
				ctx.finish(parent.Err())
			case <-ctx.stop:
			}
		case <-ctx.stop:
			ctx.timer.Stop()
		}
	}()
	return ctx, ctx.releaseHostKeyVerification, ctx.stopTiming
}

func (c *hostKeyVerificationContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	released := c.released
	deadline := c.deadline
	c.mu.Unlock()
	if released {
		return c.parent.Deadline()
	}
	parentDeadline, parentPresent := c.parent.Deadline()
	if parentPresent && parentDeadline.Before(deadline) {
		return parentDeadline, true
	}
	return deadline, true
}

func (c *hostKeyVerificationContext) Done() <-chan struct{} {
	return c.done
}

func (c *hostKeyVerificationContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *hostKeyVerificationContext) Value(key any) any {
	return c.parent.Value(key)
}

func (c *hostKeyVerificationContext) releaseHostKeyVerification() {
	c.signalOne.Do(func() {
		c.setReleased()
		close(c.release)
	})
}

func (c *hostKeyVerificationContext) stopTiming() {
	c.stopOne.Do(func() { close(c.stop) })
}

func (c *hostKeyVerificationContext) setReleased() {
	c.mu.Lock()
	c.released = true
	c.mu.Unlock()
}

func (c *hostKeyVerificationContext) finish(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	c.mu.Unlock()
	close(c.done)
}

// hostKeyCallbackReleasingVerification 在服务器呈现主机密钥、校验回调开始
// 时解除候选地址短超时。其后的锚读取、锚定转发与指纹比较只受调用方原始
// 上下文约束；校验错误仍由回调返回并让统一入口跳过该地址。
func hostKeyCallbackReleasingVerification(callback ssh.HostKeyCallback, release func()) ssh.HostKeyCallback {
	return func(hostname string, address net.Addr, remote ssh.PublicKey) error {
		release()
		if err := callback(hostname, address, remote); err != nil {
			return err
		}
		return nil
	}
}
