package hotswap

// autoswap.go — 自动换手循环(桥热升级 spec 实施决策第 1/2 条,D12/D5):
//
// 桥进程内定期(默认约 30 秒,DefaultAutoSwapInterval)读盘上代际信号,较
// 出生代际新且不忙时,经与 reload_self 工具同一个 Arm 入口(单飞、同一退位
// 编排)自动完成换手——静默,不发错误;忙时静默跳过本轮(无输出、不换手),
// 下一轮再试;Arm 失败(继任拉不起/就绪超时,已就地回退)记一行日志后下轮
// 再试。多把桥(同机不同缓存实例)各自持有一个 ReloadService、各自记出生
// 代际,无共享内存状态,天然互不干扰(实施决策第 5 条)。
//
// 生命周期:循环随 StartAutoSwap 的 ctx 取消而停(serveBridge 在会话收场、
// 泵终态后返回时取消)。武装成功后循环照常空转——Arm 的单飞占位使后续各轮
// 立即返回 already_armed,不会再发起第二次换手。

import (
	"context"
	"time"

	"ssh-manager-mcp/internal/updater"
)

// DefaultAutoSwapInterval 是自动换手的生产轮询间隔(spec 实施决策第 1 条
// 「约 30 秒」)。测试经 SSHMGR_TEST_AUTOSWAP_INTERVAL_MS 缝注入短周期
// (mcpserver/run.go applyReloadTimingSeams)。
const DefaultAutoSwapInterval = 30 * time.Second

// StartAutoSwap 启动自动换手循环(goroutine,立即返回):每
// AutoSwapInterval 一轮,盘上代际较出生代际新且 Busy() 不忙时自动 Arm。
// 无力拉继任的桥(cfg.Adopted,如取不到自身可执行路径)不启动轮询——Arm
// 一律拒绝,轮询无意义。ctx 取消即停(serveBridge 的收尾点)。
func (s *ReloadService) StartAutoSwap(ctx context.Context) {
	if s.cfg.Adopted {
		return
	}
	go s.autoSwapLoop(ctx)
}

func (s *ReloadService) autoSwapLoop(ctx context.Context) {
	interval := s.cfg.AutoSwapInterval
	if interval <= 0 {
		interval = DefaultAutoSwapInterval // 防零值(常态下 NewReloadService 已填默认)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.autoSwapTick()
		}
	}
}

// autoSwapTick 执行一轮检查。忙 → 静默跳过(无输出、不换手,规格实施决策
// 第 2 条);无新代际(信号缺失/损坏=无信号,或盘上不较出生代际新)→ 不动
// (不比版本大小,降级同样只看代际,决策第 1 条);already_armed(如工具
// 触发的换手正在途)→ 静默等待其收场;Arm 失败已就地回退 → 如实记日志,
// 下一轮再试。
func (s *ReloadService) autoSwapTick() {
	sig, ok := updater.ReadGenerationSignal(s.cfg.Exe)
	if !ok || sig.Gen <= s.cfg.BirthGeneration {
		return
	}
	if s.cfg.Busy != nil && s.cfg.Busy() {
		return // 忙:静默跳过本轮,下轮再试
	}
	switch arm := s.Arm(); arm.State {
	case ArmArmed, ArmAlreadyArmed:
		// 已武装(本轮或此前):退位编排接管时间线,循环此后空转。
	case ArmFailed:
		s.logf("auto handover attempt failed (bridge keeps serving): %v", arm.Err)
	}
}
