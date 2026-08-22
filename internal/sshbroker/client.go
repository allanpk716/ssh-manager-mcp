package sshbroker

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client wraps an ssh.Client.
type Client struct {
	c *ssh.Client
	// closeOnce 使 Close 幂等于 kaStop 的关闭 (close(kaStop) 不可重入); plain
	// Connect 出品的 Client kaStop 为 nil, 行为与既有完全一致。
	closeOnce sync.Once
	// kaStop 关闭时通知 keepalive 循环退出 (ConnectKeepAlive 出品才有)。
	kaStop chan struct{}
}

// keepAliveSpec 是 keepalive 连接变体的参数 (zero 值 = 不启循环)。
// 注: spec §1 原文的 x/crypto ssh.KeepAliveConfig 已被证伪——go.mod 版
// v0.41.0 及最新发布版 (v0.55.0, 2026-08 核实) 均无该类型 (pkg.go.dev 同)。
// ConnectKeepAlive 按 OpenSSH 同型机制实现: 周期发 keepalive@openssh.com
// 全局请求 (wantReply=true), 连续 maxFail 次无响应判死 → 关底层连接 →
// 在途会话以错误返回 (后台引擎落 failed 态, spec §1 语义不变)。
type keepAliveSpec struct {
	interval time.Duration
	maxFail  int
}

// Connect dials the SSH server and authenticates. hostKeyCb enforces host-key
// policy. The SSHMGR_SSH_HOST_KEY_ALGORITHMS knob (if set) is validated FIRST
// and fail-closed: a typo'd value returns an error before any dial. ctx is
// honored: ssh.Dial itself cannot be interrupted, so on cancellation Connect
// returns ctx.Err() immediately and abandons the in-flight dial; a background
// goroutine closes the connection the dial eventually yields (so no *ssh.Client
// leaks). This bounds a cancelled dial to an unreachable host to milliseconds
// rather than the OS TCP timeout (~minutes).
func Connect(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error) {
	return connectWith(ctx, host, port, user, auth, hostKeyCb, keepAliveSpec{})
}

// ConnectKeepAlive 与 Connect 全同, 但为连接挂 30s 周期、3 次无响应判死的
// keepalive 循环 (Plan 32 T4: 后台 24h 长连接防 NAT/防火墙空闲拆连的诚实
// 失败形态; 前台 ≤5min 路径不走此变体)。循环生命周期随 Client.Close 终止。
func ConnectKeepAlive(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error) {
	return connectWith(ctx, host, port, user, auth, hostKeyCb, keepAliveSpec{interval: 30 * time.Second, maxFail: 3})
}

// connectWith 是 Connect/ConnectKeepAlive 的共用体: ka 为 zero 值时不启
// keepalive 循环 (Connect 的零行为变化); 否则拨号成功后起循环并接线 kaStop。
func connectWith(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec) (*Client, error) {
	algos, err := hostKeyAlgosChecked() // fail-closed BEFORE dial (typo → no connection attempt)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: hostKeyCb,
	}
	if algos != nil {
		cfg.HostKeyAlgorithms = algos
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	type result struct {
		c   *ssh.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ssh.Dial("tcp", addr, cfg)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			// Plan 31: scrub the dialed address (and any resolved-IP / DNS
			// residue) from the error text AT THE SOURCE, so every consumer —
			// MCP tool errors included — is safe by construction. redactAddr's
			// rendered text is itself prefixed with "ssh dial: " (and its frozen
			// degraded phrases carry the same prefix), so Connect returns it
			// AS-IS — no outer wrap, or the prefix would double end-to-end
			// (owner ruling). The chain survives via Unwrap (errors.Is
			// classification) and net.Error is delegated; see redact.go.
			return nil, redactAddr(r.err, host, port)
		}
		cli := &Client{c: r.c}
		if ka.interval > 0 && ka.maxFail > 0 {
			cli.kaStop = make(chan struct{})
			go cli.keepAliveLoop(ka.interval, ka.maxFail)
		}
		return cli, nil
	case <-ctx.Done():
		go func() {
			r := <-ch // let the in-flight Dial finish, then reclaim its connection
			if r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// keepAliveLoop 周期发 keepalive@openssh.com 全局请求 (wantReply): 对端回
// 应则计数清零; 连续 maxFail 次无响应判死, 关底层连接使在途会话以错误解阻。
// kaStop 关闭 (Client.Close) 即退出。已知取舍 (与所有 x/crypto 使用者同): 对
// 静默黑洞网络 (无 RST 的 NAT 超时), SendRequest 会阻塞在等回应上——判死只
// 覆盖「有错误信号」的死亡形态; 真连接冒烟/判死行为归 conformance 层。
func (c *Client) keepAliveLoop(interval time.Duration, maxFail int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-c.kaStop:
			return
		case <-ticker.C:
			if _, _, err := c.c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				fails++
				if fails >= maxFail {
					_ = c.c.Close() // 判死: 关连接, 在途会话以错误返回
					return
				}
				continue
			}
			fails = 0
		}
	}
}

// Close 幂等: 关 keepalive 循环 (若有) 并关底层连接 (ssh.Client.Close 自身
// 可重入)。终态即关的调用方 (后台引擎、CloseAll) 与 defer 清理可安全并发。
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.kaStop != nil {
			close(c.kaStop)
		}
	})
	return c.c.Close()
}
