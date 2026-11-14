package webui

// ShareRemoteOperation 是共享存储远端操作的类型代号。后续卸载、撤销、重
// 选地址与残留检查继续沿用同一个任务视图。
const (
	ShareRemoteOperationCheck = "check"
	ShareRemoteOperationMount = "mount"
)

// ShareTaskState 是共享存储远端操作任务的状态代号：running 表示后台执
// 行中，done 表示已结束并带结论，error 表示启动后的执行失败。
type ShareTaskState string

// 共享存储远端操作任务的三个状态代号。
const (
	ShareTaskRunning ShareTaskState = "running"
	ShareTaskDone    ShareTaskState = "done"
	ShareTaskError   ShareTaskState = "error"
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
	// HasConfigEntry 与 HasCredentialFile 分别表示现场检查确认远端仍有
	// sshmgr 持久挂载配置与凭据文件，供页面给出准确的撤销依据。
	HasConfigEntry    bool
	HasCredentialFile bool
	Version           int64
	Restricted        bool
	ObservedAt        string
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
	State       ShareTaskState
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

// ShareRemoteTasksWithLatestChecks 是既能启动现场探测与单台挂载任务、又
// 能读写最近一次现场检查结果的远端任务视图。装配处把同一个实例同时交
// 给 SharesRemoteTasks、ShareLatestChecks 与残留检查任务。
type ShareRemoteTasksWithLatestChecks interface {
	ShareRemoteTasks
	ShareLatestCheckSource
	ShareCheckRecorder
}

// ShareCheckRecorder 把一处共享存储的一台服务器现场结论登记到最近现场
// 状态。残留检查完成后用它补充现场探测留下的登记。
type ShareCheckRecorder interface {
	RecordShareCheckResult(shareID string, result ShareCheckResult)
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
