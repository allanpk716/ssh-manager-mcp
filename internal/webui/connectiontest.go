package webui

import (
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// ServerConnectionTestState 是新增服务器后自动连接测试任务的状态。
type ServerConnectionTestState string

const (
	// ServerConnectionTestRunning 表示任务仍在后台执行。
	ServerConnectionTestRunning ServerConnectionTestState = "running"
	// ServerConnectionTestSucceeded 表示连接测试完成且条目已验证。
	ServerConnectionTestSucceeded ServerConnectionTestState = "succeeded"
	// ServerConnectionTestFailed 表示测试未完成或失败，条目保持未验证。
	ServerConnectionTestFailed ServerConnectionTestState = "failed"
	// ServerConnectionTestMissing 只用于批量轮询：任务已被保留策略回收，
	// 页面需要回到列表读取持久结果；它不是新的连接测试结论。
	ServerConnectionTestMissing ServerConnectionTestState = "missing"
)

// ServerConnectionTestStatus 是一次自动连接测试的页面数据。错误与说明必须
// 只包含可展示内容，调用方不得把登录凭据或提权口令放进任何字段。
type ServerConnectionTestStatus struct {
	ID                string                    `json:"task_id"`
	State             ServerConnectionTestState `json:"state"`
	ServerID          string                    `json:"server_id"`
	Message           string                    `json:"message,omitempty"`
	ConnectionNotes   []string                  `json:"connection_notes,omitempty"`
	AnchorFingerprint string                    `json:"anchor_fingerprint,omitempty"`
	AnchorResult      string                    `json:"anchor_result,omitempty"`
	HandshakeAddress  string                    `json:"handshake_address,omitempty"`
	ProbeShell        bool                      `json:"probe_shell"`
	ShellDetected     string                    `json:"shell_detected,omitempty"`
	ShellEvidence     string                    `json:"shell_evidence,omitempty"`
	ShellNotice       string                    `json:"shell_notice,omitempty"`
	// Result 是可复用给详情页、服务器列表卡片与新增向导的结构化结果。
	Result *ServerConnectionTestResultView `json:"result,omitempty"`
	// AnchorCopied 与 AnchorCopyConflict 描述保存事务里复制的既有锚；它们
	// 只随本任务在进程内传递，不来自结果页网址查询参数。
	AnchorCopied             bool   `json:"anchor_copied,omitempty"`
	AnchorCopyFromServerName string `json:"anchor_copy_from_server,omitempty"`
	AnchorCopyConflict       bool   `json:"anchor_copy_conflict,omitempty"`
}

// ServerConnectionTestResultView 是连接测试结果的共享页面数据。它只携带
// 可展示事实与白话分类，不携带底层错误或任何凭据。
type ServerConnectionTestResultView struct {
	State              ServerConnectionTestState
	PollPath           string
	CompletedAt        time.Time
	Succeeded          bool
	FailureLabel       string
	Hint               string
	SuccessAddress     string
	SuccessNetworkName string
	SuccessSource      models.DialAddressSource
	Duration           time.Duration
	Attempts           []ServerConnectionTestAttemptView
	// Persisted 表示结果已经写入加密库；为假时页面不得把它当成「上次测
	// 试」事实带外更新页头。
	Persisted bool
}

// ServerConnectionTestAttemptView 是一个候选地址在页面上的结构化结果。
type ServerConnectionTestAttemptView struct {
	NetworkName  string
	Address      string
	Source       models.DialAddressSource
	OK           bool
	FailureLabel string
	Duration     time.Duration
}

// NewServerConnectionTestResultView 把存储或任务层的结果换成共享页面数
// 据；零值完成时间保持为零，模板仍可安全渲染。
func NewServerConnectionTestResultView(
	result models.ServerConnectionTestResult,
) *ServerConnectionTestResultView {
	view := &ServerConnectionTestResultView{
		State: ServerConnectionTestFailed, CompletedAt: result.CompletedAt,
		Succeeded: result.Succeeded, SuccessAddress: result.SuccessAddress,
		SuccessNetworkName: result.SuccessNetworkName, SuccessSource: result.SuccessSource,
		Hint:     result.Hint,
		Duration: result.Duration, Persisted: !result.CompletedAt.IsZero(),
	}
	if result.Succeeded {
		view.State = ServerConnectionTestSucceeded
	} else {
		view.FailureLabel = result.FailureReason.Label()
	}
	for _, attempt := range result.Attempts {
		attemptView := ServerConnectionTestAttemptView{
			NetworkName: attempt.NetworkName, Address: attempt.Address, Source: attempt.Source,
			OK: attempt.OK, Duration: attempt.Duration,
		}
		if !attempt.OK {
			attemptView.FailureLabel = attempt.FailureReason.Label()
		}
		view.Attempts = append(view.Attempts, attemptView)
	}
	return view
}

// fallbackNetworkLabelText 是服务器条目登记的兜底地址不属于任何登记网络
// 时使用的固定网络指称。
const fallbackNetworkLabelText = "其他（公网或未登记网络）"

// connectionTestNetworkLabel 返回一个候选地址的网络指称；服务器条目登记
// 的兜底地址不属于任何登记网络，显示固定文案。
func connectionTestNetworkLabel(name string, source models.DialAddressSource) string {
	if source == models.DialAddressSourceFallback {
		return fallbackNetworkLabelText
	}
	return name
}

// SuccessNetworkLabel 返回成功地址的网络指称；服务器条目登记的兜底地址不
// 属于任何登记网络，显示固定文案。
func (v *ServerConnectionTestResultView) SuccessNetworkLabel() string {
	return connectionTestNetworkLabel(v.SuccessNetworkName, v.SuccessSource)
}

// SuccessSummaryText 返回成功结果的短说明；网络名或地址为空时省略该段和
// 它的分隔点，避免出现连续分隔点。
func (v *ServerConnectionTestResultView) SuccessSummaryText() string {
	parts := make([]string, 0, 3)
	if v.SuccessAddress != "" {
		parts = append(parts, v.SuccessAddress)
	}
	if label := v.SuccessNetworkLabel(); label != "" {
		parts = append(parts, label)
	}
	parts = append(parts, fmt.Sprintf("耗时 %d 毫秒", v.Duration.Milliseconds()))
	return strings.Join(parts, " · ")
}

// ShortStatusText 返回列表或页头可复用的短状态文案；服务器列表卡片只使用
// 这个字段，不复用完整结果区。
func (v *ServerConnectionTestResultView) ShortStatusText() string {
	return v.State.StatusText()
}

// StatusText 返回连接测试状态的自含短文案；详情页、服务器列表卡片与新增
// 结果页共用同一份映射。
func (s ServerConnectionTestState) StatusText() string {
	switch s {
	case ServerConnectionTestRunning:
		return "正在连接"
	case ServerConnectionTestSucceeded:
		return "已连上"
	default:
		return "连不上"
	}
}

// formatConnectionTestCardTime 返回服务器列表卡片使用的本地时区短时间：
// 当天「今天 HH:MM」、昨天「昨天 HH:MM」、同年「M月D日 HH:MM」、跨年
// 「YYYY年M月D日」。now 由调用方传入，供单元测试固定日期判断。
func formatConnectionTestCardTime(completedAt, now time.Time) string {
	completed := completedAt.In(time.Local)
	current := now.In(time.Local)
	completedDate := time.Date(
		completed.Year(), completed.Month(), completed.Day(), 0, 0, 0, 0, time.Local,
	)
	currentDate := time.Date(current.Year(), current.Month(), current.Day(), 0, 0, 0, 0, time.Local)
	switch {
	case completedDate.Equal(currentDate):
		return "今天 " + completed.Format("15:04")
	case completedDate.AddDate(0, 0, 1).Equal(currentDate):
		return "昨天 " + completed.Format("15:04")
	case completed.Year() == current.Year():
		return completed.Format("1月2日 15:04")
	default:
		return completed.Format("2006年1月2日")
	}
}

// NetworkLabel 返回候选地址行的网络指称；服务器条目登记的兜底地址不属于
// 任何登记网络，显示固定文案。
func (v ServerConnectionTestAttemptView) NetworkLabel() string {
	return connectionTestNetworkLabel(v.NetworkName, v.Source)
}

// ServerConnectionAnchor 是启动连接测试时随任务保存的锚复制事实。
type ServerConnectionAnchor struct {
	Copied         bool
	FromServerName string
	Conflict       bool
}

// ServerConnectionTester 是新增服务器后启动与读取连接测试任务的唯一入口。
// 实现必须立即返回任务标识；测试在后台执行，不能阻塞表单提交返回。
type ServerConnectionTester interface {
	StartServerTest(serverID string, probeShell bool, source ServerWriteSource, anchor ServerConnectionAnchor) (string, error)
	ServerTestStatus(taskID string) (*ServerConnectionTestStatus, bool)
}

// ServerConnectionTestBatchTask 是批量启动结果里一台服务器条目对应的任
// 务；页面用它在内存里保存服务器条目到任务的映射。
type ServerConnectionTestBatchTask struct {
	ServerID string `json:"server_id"`
	TaskID   string `json:"task_id"`
}

// ServerConnectionTestBatchStart 是批量启动接口的返回值；只携带任务标识，
// 不等待任何 SSH 连接完成。
type ServerConnectionTestBatchStart struct {
	Tasks []ServerConnectionTestBatchTask `json:"tasks"`
}

// ServerConnectionTestBatchCard 是批量轮询里一台服务器条目的卡片短状态。
// 文案由 #326 的列表短状态规则生成，页面只负责替换显示。
type ServerConnectionTestBatchCard struct {
	ServerID string                    `json:"server_id"`
	TaskID   string                    `json:"task_id"`
	State    ServerConnectionTestState `json:"state"`
	Title    string                    `json:"title"`
	Detail   string                    `json:"detail,omitempty"`
}

// ServerConnectionTestBatchStatus 是批量轮询接口的返回值：逐卡状态加汇总
// 计数；Done 表示这一批没有仍在运行的任务。
type ServerConnectionTestBatchStatus struct {
	Cards     []ServerConnectionTestBatchCard `json:"cards"`
	Done      bool                            `json:"done"`
	Total     int                             `json:"total"`
	Running   int                             `json:"running"`
	Succeeded int                             `json:"succeeded"`
	Failed    int                             `json:"failed"`
	Missing   int                             `json:"missing,omitempty"`
	// Reload 表示有任务状态已被保留策略回收，页面应整页读取持久结果。
	Reload bool `json:"reload,omitempty"`
}

// NewServerConnectionTestBatchCard 把一台任务状态换成服务器列表卡片的短
// 状态数据；进行中、成功与失败共用 #326 的文案映射，不另建第二套状态。
func NewServerConnectionTestBatchCard(
	status *ServerConnectionTestStatus, now time.Time,
) ServerConnectionTestBatchCard {
	if status == nil {
		return ServerConnectionTestBatchCard{State: ServerConnectionTestMissing}
	}
	card := ServerConnectionTestBatchCard{
		ServerID: status.ServerID, TaskID: status.ID, State: status.State,
	}
	if status.State == ServerConnectionTestRunning {
		card.Title = status.State.StatusText()
		return card
	}
	if status.Result == nil {
		card.Title = ServerConnectionTestFailed.StatusText()
		return card
	}
	card.State = status.Result.State
	card.Title = status.Result.ShortStatusText()
	if !status.Result.CompletedAt.IsZero() {
		card.Title = formatConnectionTestCardTime(status.Result.CompletedAt, now) + " " + card.Title
	}
	if status.Result.Succeeded {
		card.Detail = status.Result.SuccessSummaryText()
		return card
	}
	if status.Result.FailureLabel != "" {
		card.Title += "：" + status.Result.FailureLabel
	}
	return card
}
