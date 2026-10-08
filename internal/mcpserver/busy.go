package mcpserver

import (
	"context"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 忙判据库 (桥热升级 spec 实施决策第 2 条, D5/F3): 桥进程内统一的忙判定 ——
// 活跃隧道>0, 或运行中后台任务>0 (终态保留期不算; relay 传输已挂任务槽,
// 同一口径), 或在飞未答请求>0 (豁免发起换手的本次调用)。忙时: 工具触发返回
// 活跃清单, 自动触发静默跳过下轮再试。消费方是 reload_self 工具与自动换手
// (后续票接线): 忙检查即 busy.Report()。

// BusyReport 是统一忙报告 (即「活跃清单」的数据形态): 是否忙 + 各分项计数。
// 字段带 snake_case 线格式标签, 可直接内嵌进工具返回结构渲染成活跃清单。
type BusyReport struct {
	Busy             bool `json:"busy"`
	ActiveTunnels    int  `json:"active_tunnels"`
	RunningTasks     int  `json:"running_tasks"`
	InflightRequests int  `json:"inflight_requests"`
}

// BusyTracker 聚合三路计数。隧道与任务两路不复制状态 —— 持计数源闭包
// (TunnelManager.ActiveTunnels / TaskManager.RunningTasks 的方法值), 每次
// 快照现读, 各管理器自己的锁是各自的并发安全边界; 在飞一路是本结构内的
// 原子计数, 由工具处理器包装层 (wrapInflight) 进出各增减一次。
type BusyTracker struct {
	activeTunnels func() int
	runningTasks  func() int
	inflight      atomic.Int64
}

// NewBusyTracker 组装忙判据库。两个计数源闭包必须并发安全 (管理器方法值满足)。
func NewBusyTracker(activeTunnels, runningTasks func() int) *BusyTracker {
	return &BusyTracker{activeTunnels: activeTunnels, runningTasks: runningTasks}
}

// Report 返回当前忙快照。三分项非同一锁下的原子读 (各归各的并发边界), 快照
// 语义即为消费方契约: 忙判据是或聚合, 单分项的瞬时毛刺不影响「忙」的判定
// 方向 (漏报窗口: 恰在快照瞬间全部归零 —— 与换手动作的秒级耗时相比可忽略)。
func (b *BusyTracker) Report() BusyReport {
	r := BusyReport{
		ActiveTunnels:    b.activeTunnels(),
		RunningTasks:     b.runningTasks(),
		InflightRequests: int(b.inflight.Load()),
	}
	r.Busy = r.ActiveTunnels > 0 || r.RunningTasks > 0 || r.InflightRequests > 0
	return r
}

// InflightRequests 返回在飞未答请求计数 (观察口: 测试与后续换手路径诊断)。
func (b *BusyTracker) InflightRequests() int { return int(b.inflight.Load()) }

// wrapInflight 把工具处理器包进在飞计数: 处理器执行期间计数 +1, 返回后 -1
// (defer 配平, 错误路径同算)。exempt=true 的注册不计入 —— 发起换手的
// reload_self 必须豁免自身, 否则它做忙检查时永远看见自己忙碌 (工具触发永远
// 自拒, spec D5)。豁免放注册点而非 context: 处理器拿到 ctx 时包装层已计数
// 完毕, 调用自身无法事后给自己减计数; Enter/Leave 手工配对则把配平责任摊给
// 每个豁免方, 注册点一个布尔是最小侵入形态。
func wrapInflight[In, Out any](b *BusyTracker, h mcp.ToolHandlerFor[In, Out], exempt bool) mcp.ToolHandlerFor[In, Out] {
	if exempt {
		return h
	}
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		b.inflight.Add(1)
		defer b.inflight.Add(-1)
		return h(ctx, req, in)
	}
}

// addTrackedTool 是 mcp.AddTool 的忙计数挂接版: server.go 的全部工具注册走
// 它, 处理器一律包上在飞计数 (豁免注册形态: wrapInflight(busy, h, true) +
// mcp.AddTool —— reload_self 票使用)。
func addTrackedTool[In, Out any](srv *mcp.Server, busy *BusyTracker, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(srv, t, wrapInflight(busy, h, false))
}
