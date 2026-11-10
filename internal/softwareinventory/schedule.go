package softwareinventory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// 采集调度的固定限制来自规格：每天保持新鲜，因此距上次成功已满一天就到
// 期；周期检查建议每小时一轮；并发上限建议 4。
const (
	// CollectDueAfter 是设备到期阈值：距上次成功采集达到这个时长后，下
	// 一轮重新采集；从未成功采集过的设备始终到期。
	CollectDueAfter = 24 * time.Hour
	// DefaultScheduleInterval 是 Run 周期检查的默认间隔。
	DefaultScheduleInterval = time.Hour
	// DefaultScheduleConcurrency 是一轮里同时采集的设备数上限。
	DefaultScheduleConcurrency = 4
)

// SchedulerCollectFunc 对一台设备执行一次采集。生产路径默认复用单台立即
// 采集服务；测试注入假函数验证调度行为。
type SchedulerCollectFunc func(ctx context.Context, request CollectRequest) (*CollectionReport, error)

// Scheduler 是软件清单采集调度服务：周期检查「采集执行者是本实例、且距
// 上次成功已满一天或有待执行的立即采集请求」的设备，按并发上限采集。
// 时钟、间隔与采集函数都可注入；serve 只负责启动与收尾。
type Scheduler struct {
	Store *store.Store
	// Collector 是本实例的采集执行者身份；零值表示权威端或单机。
	Collector models.Collector
	// Collect 为空时复用绑定同一存储的单台采集服务。
	Collect SchedulerCollectFunc
	// Now 提供到期判断用的时钟；空值使用真实时钟。
	Now func() time.Time
	// Interval 是 Run 的周期；空值用 DefaultScheduleInterval。
	Interval time.Duration
	// Concurrency 是一轮的并发上限；小于 1 时用默认值。
	Concurrency int
	// Logf 为空时不输出日志；serve 注入 stderr 记录每轮结论。
	Logf func(format string, arguments ...any)

	service *Service
}

// SchedulerDeviceReport 是一轮里一台设备的结论。
type SchedulerDeviceReport struct {
	DeviceName       string            `json:"device_name"`
	PendingImmediate bool              `json:"pending_immediate"`
	Report           *CollectionReport `json:"report,omitempty"`
	Err              string            `json:"error,omitempty"`
}

// SchedulerRoundReport 是一轮的结论：到期与跳过数量、每台设备的采集结
// 果。设备按「立即采集请求优先，其余按设备名」的执行顺序排列。
type SchedulerRoundReport struct {
	StartedAt     time.Time               `json:"started_at"`
	EndedAt       time.Time               `json:"ended_at"`
	Due           int                     `json:"due"`
	SkippedNotDue int                     `json:"skipped_not_due"`
	Succeeded     int                     `json:"succeeded"`
	Failed        int                     `json:"failed"`
	Devices       []SchedulerDeviceReport `json:"devices"`
}

// NewScheduler 构造复用单台采集服务的调度服务。
func NewScheduler(st *store.Store) *Scheduler {
	return &Scheduler{Store: st, service: &Service{Store: st}}
}

// ExecuteRound 执行一轮：读本实例名下的受管设备状态，筛出到期设备（距
// 上次成功已满一天，或有待执行的立即采集请求），立即采集请求排最前，
// 其余按设备名排序，按并发上限采集。单台设备失败不中断本轮。
func (s *Scheduler) ExecuteRound(ctx context.Context) (*SchedulerRoundReport, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("软件清单采集调度服务没有绑定存储")
	}
	report := &SchedulerRoundReport{StartedAt: s.now()}
	statuses, err := s.Store.SoftwareCollectionStatusesForCollector(s.collector())
	if err != nil {
		return nil, err
	}
	now := s.now()
	due := make([]models.SoftwareCollectionStatus, 0, len(statuses))
	for _, status := range statuses {
		if status.PendingImmediate || now.Sub(status.LastSuccessAt) >= CollectDueAfter {
			due = append(due, status)
		}
	}
	report.Due = len(due)
	report.SkippedNotDue = len(statuses) - len(due)
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].PendingImmediate != due[j].PendingImmediate {
			return due[i].PendingImmediate
		}
		return due[i].DeviceName < due[j].DeviceName
	})

	reports := make([]SchedulerDeviceReport, len(due))
	for i := range due {
		reports[i] = SchedulerDeviceReport{
			DeviceName: due[i].DeviceName, PendingImmediate: due[i].PendingImmediate,
		}
	}
	workerCount := s.concurrency()
	if workerCount > len(due) {
		workerCount = len(due)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				device := reports[index]
				if ctx.Err() != nil {
					device.Err = "本轮在执行前已停止"
					reports[index] = device
					continue
				}
				result, err := s.collect()(ctx, CollectRequest{
					DeviceName:  due[index].DeviceName,
					ServerEntry: due[index].PreferredServerID,
					Source:      CLISource(),
				})
				if err != nil {
					device.Err = err.Error()
				} else {
					device.Report = result
				}
				reports[index] = device
			}
		}()
	}
	for index := range due {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- index:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	// 没有被分派到的设备（上下文取消时停止发任务）在这里补上结论，避免
	// 既无报告又无错误的空条目被误计为成功。
	for i := range reports {
		if reports[i].Report == nil && reports[i].Err == "" {
			reports[i].Err = "本轮在执行前已停止"
		}
	}
	for i := range reports {
		if reports[i].Err == "" {
			report.Succeeded++
		} else {
			report.Failed++
		}
	}
	report.Devices = reports
	report.EndedAt = s.now()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

// Run 启动即执行一轮（补上停机期间错过的），之后按周期反复执行；上下
// 文取消时干净返回。单轮失败只记日志，不结束调度。
func (s *Scheduler) Run(ctx context.Context) error {
	s.roundLogged(ctx)
	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.roundLogged(ctx)
		}
	}
}

func (s *Scheduler) roundLogged(ctx context.Context) {
	report, err := s.ExecuteRound(ctx)
	if s.Logf == nil {
		return
	}
	if err != nil {
		s.Logf("执行一轮失败：%v", err)
		return
	}
	s.Logf("执行一轮完成：到期 %d 台，成功 %d 台，失败 %d 台，未到期跳过 %d 台",
		report.Due, report.Succeeded, report.Failed, report.SkippedNotDue)
}

func (s *Scheduler) collector() models.Collector { return s.Collector }

func (s *Scheduler) concurrency() int {
	if s.Concurrency < 1 {
		return DefaultScheduleConcurrency
	}
	return s.Concurrency
}

func (s *Scheduler) interval() time.Duration {
	if s.Interval <= 0 {
		return DefaultScheduleInterval
	}
	return s.Interval
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) collect() SchedulerCollectFunc {
	if s.Collect != nil {
		return s.Collect
	}
	if s.service == nil {
		s.service = &Service{Store: s.Store}
	}
	return s.service.Collect
}
