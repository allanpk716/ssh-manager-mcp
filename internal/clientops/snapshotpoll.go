package clientops

// 快照修订号后台轮询循环。多机客户端的每个桥
// (`mcp --cache`)启动一个可停止的循环,默认每 5 分钟查询一次权威端的
// GET /snapshot/version:
//
//	查询修订号 → 与内存快照的修订号相同就结束本轮 → 不同时先读共享缓存
//	的修订号,已达到远端修订号就直接热加载 → 否则沿用现有全量拉取函数
//	(唯一临时文件加原子重命名)再热加载。
//
// 「热加载」一律经调用方注入的 CacheRefresher 完成——它就是桥现有的统一
// 换快照入口(工具调用前的热加载与这里共用同一条路径),本包不另写任何替换
// 内存快照的逻辑。不加跨进程文件锁:各个桥的定时器起点不同,即使同时拉取,
// 原子替换也保证缓存不会写坏。
//
// 失败处理:查询或拉取失败(不可达、超时、证书错误、服务器内部错误)只写
// 日志,继续用当前快照,下一轮再试,不阻断工具调用;旧权威端对修订号接口
// 返回 404 时,这个桥停止轮询,到期检查回到原来的按有效期全量拉取。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

// SnapshotPollerConfig 配置一个桥的后台轮询循环。Interval 由调用方注入
// (默认值来自实例配置,测试可注入很短的间隔,不必真的等 5 分钟)。
type SnapshotPollerConfig struct {
	// Instance 是缓存实例名("" = 默认实例)。
	Instance string
	// Interval 是轮询间隔;小于等于 0 时不启动循环。
	Interval time.Duration
	// Refresher 是桥的统一换快照入口;为空时不启动循环。
	Refresher mcpserver.CacheRefresher
	// LogOutput 是诊断日志出口;为空时写到标准错误流。
	LogOutput io.Writer
	// SoftwareInventoryCollect 是快照新鲜度轮询每轮结束后触发的软件清单
	// 顺手补采。它自己负责取本地锁文件;这里只保证一个桥同时只有一份。
	SoftwareInventoryCollect func()
}

type snapshotPoller struct {
	cfg   SnapshotPollerConfig
	logw  io.Writer
	stopC chan struct{}
	wg    sync.WaitGroup
	once  sync.Once
	// collectWG 与 collectRunning 保证顺手补采不与本轮、下一轮重叠。
	collectWG      sync.WaitGroup
	collectRunning atomic.Bool
	// missingCredLogged 避免缺少拉取凭据时每轮刷一条相同的日志(凭据可能
	// 之后由别的进程写入,循环继续等待)。只在循环 goroutine 里读写。
	missingCredLogged bool
}

// StartSnapshotPoller 启动可停止的后台轮询循环,返回停止函数;停止函数会等
// 正在执行的一轮结束后返回,可以安全地重复调用。间隔或触发器缺省时返回
// 无操作的停止函数(单机部署的桥不启动定时器,由调用方直接不启动)。
func StartSnapshotPoller(cfg SnapshotPollerConfig) func() {
	if cfg.Interval <= 0 || cfg.Refresher == nil {
		return func() {}
	}
	p := &snapshotPoller{cfg: cfg, logw: cfg.LogOutput, stopC: make(chan struct{})}
	if p.logw == nil {
		p.logw = os.Stderr
	}
	p.wg.Add(1)
	go p.loop()
	return p.stop
}

func (p *snapshotPoller) logf(format string, args ...any) {
	fmt.Fprintf(p.logw, "sshmgr: snapshot poll: "+format+"\n", args...)
}

func (p *snapshotPoller) stop() {
	p.once.Do(func() { close(p.stopC) })
	p.wg.Wait()
}

// loop 启动后先立即执行一轮(桥刚拉过缓存,这一轮通常只是确认修订号相同),
// 之后按注入的间隔循环;任意一轮返回 false(旧权威端 404、设备码被权威端
// 销毁)即永久停止。
func (p *snapshotPoller) loop() {
	defer p.wg.Done()
	if !p.round() {
		return
	}
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopC:
			return
		case <-ticker.C:
			if !p.round() {
				return
			}
		}
	}
}

func (p *snapshotPoller) startSoftwareInventoryCollect() {
	if p.cfg.SoftwareInventoryCollect == nil || !p.collectRunning.CompareAndSwap(false, true) {
		return
	}
	p.collectWG.Add(1)
	go func() {
		defer p.collectWG.Done()
		defer p.collectRunning.Store(false)
		p.cfg.SoftwareInventoryCollect()
	}()
}

// round 执行一轮轮询,返回是否继续。所有失败都只写日志:权威端不可达时继续
// 用当前快照,下一轮再试。
func (p *snapshotPoller) round() bool {
	defer p.startSoftwareInventoryCollect()
	cred, err := ReadCacheCredFor(p.cfg.Instance)
	if err != nil {
		p.logf("read pull credential: %v", err)
		return true
	}
	if cred == nil {
		if !p.missingCredLogged {
			p.missingCredLogged = true
			p.logf("no pull credential; waiting for one before polling snapshot revisions")
		}
		return true
	}
	code, pin := deviceCodeAndPin(*cred)
	if pin == "" {
		if !p.missingCredLogged {
			p.missingCredLogged = true
			p.logf("pull credential has no pin; refusing plaintext revision queries")
		}
		return true
	}

	remoteRevision, _, err := querySnapshotVersion(context.Background(), cred.URL, code, pin, LazyPullTimeout)
	switch {
	case errors.Is(err, errSnapshotVersionNotFound):
		// 旧权威端没有修订号接口:这个桥停止轮询,到期检查回到原来的按
		// 有效期全量拉取(整体降级提示属于后续票)。
		p.logf("authority has no snapshot version endpoint; stopping background polling")
		return false
	case err != nil:
		p.logf("query snapshot revision: %v (keeping current snapshot)", err)
		return true
	}
	if remoteRevision == p.cfg.Refresher.CurrentRevision() {
		return true
	}

	// 修订号不同:先看共享缓存。同一台机器上别的桥可能已经把它更新到同样
	// 或更新的修订号——已达到就直接热加载,不再下载。
	if cacheRevision, present, rerr := cachedSnapshotRevision(p.cfg.Instance); rerr == nil && present && cacheRevision >= remoteRevision {
		p.cfg.Refresher.ReloadNow()
		return true
	}

	// 共享缓存不够新:沿用现有的全量拉取函数(原子替换缓存)再热加载。
	if _, err := DoPull(cred.URL, code, pin, PullOpts{
		Timeout:   LazyPullTimeout,
		StatusOut: p.logw,
		Instance:  p.cfg.Instance,
	}); err != nil {
		if errors.Is(err, ErrCacheQuarantined) {
			// 权威端拒绝了设备码并销毁了缓存材料:本进程后续的自动拉取
			// 一并停住,与工具调用路径的隔离标志保持一致。
			cacheQuarantinedFlag = true
			p.logf("authority rejected the device code; cache quarantined, stopping background polling")
			return false
		}
		p.logf("pull snapshot: %v (keeping current snapshot)", err)
		return true
	}
	p.cfg.Refresher.ReloadNow()
	return true
}
