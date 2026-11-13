package webui

// ShareRemoteOperation 是共享存储远端操作的类型代号。后续卸载、撤销、重
// 选地址与残留检查继续沿用同一个任务视图。
const (
	ShareRemoteOperationCheck = "check"
	ShareRemoteOperationMount = "mount"
)

// ShareRemoteTaskState 是远端操作任务状态代号。
const (
	ShareRemoteTaskRunning = "running"
	ShareRemoteTaskDone    = "done"
	ShareRemoteTaskError   = "error"
)

// ShareCheckStatus 是页面展示的现场状态代号。
const (
	ShareCheckStatusMounted   = "mounted"
	ShareCheckStatusUnmounted = "unmounted"
	ShareCheckStatusSkipped   = "skipped"
)

// ShareCheckResult 是页面展示的一台服务器现场探测结论。它不包含地址、挂
// 载账号或密码。
type ShareCheckResult struct {
	ServerID   string
	ServerName string
	System     string
	Status     string
	Reason     string
	Notice     string
	Managed    bool
	Version    int64
	Restricted bool
	ObservedAt string
}

// ShareMountResult 是页面展示的一次单台服务器挂载结论。
type ShareMountResult struct {
	ShareName       string
	ServerID        string
	ServerName      string
	Version         int64
	MountPoint      string
	Network         string
	NetworkSequence int
	Action          string
	Notices         []string
}

// ShareRemoteTaskRequest 是启动一次共享存储远端任务的页面请求。ServerID
// 只对单台服务器操作有意义。
type ShareRemoteTaskRequest struct {
	Operation string
	ShareID   string
	ServerID  string
	Source    ShareWriteSource
}

// ShareRemoteTaskStatus 是进程内远端任务的状态与安全结果。
type ShareRemoteTaskStatus struct {
	ID          string
	Operation   string
	ShareID     string
	ShareName   string
	ServerID    string
	State       string
	Message     string
	CheckResult []ShareCheckResult
	MountResult *ShareMountResult
}

// ShareRemoteTasks 是共享存储远端操作的进程内任务视图。任务只保存在当前
// Web UI 进程内存里，进程重启后状态即丢失。
type ShareRemoteTasks interface {
	StartShareRemoteTask(request ShareRemoteTaskRequest) (string, error)
	ShareRemoteTaskStatus(taskID string) (*ShareRemoteTaskStatus, bool)
}

// ShareRemoteTaskView 是任务页与任务片段的展示数据。Lost 为真时表示任务
// 表里没有这个任务，页面提示可重新发起而不排队补跑。
type ShareRemoteTaskView struct {
	Share           *ShareRow
	Task            *ShareRemoteTaskStatus
	Lost            bool
	RestartCheck    bool
	RestartMount    bool
	RestartServerID string
	PollPath        string
}
