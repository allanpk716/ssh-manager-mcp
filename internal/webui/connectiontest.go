package webui

// ServerConnectionTestState 是新增服务器后自动连接测试任务的状态。
type ServerConnectionTestState string

const (
	// ServerConnectionTestRunning 表示任务仍在后台执行。
	ServerConnectionTestRunning ServerConnectionTestState = "running"
	// ServerConnectionTestSucceeded 表示连接测试完成且条目已验证。
	ServerConnectionTestSucceeded ServerConnectionTestState = "succeeded"
	// ServerConnectionTestFailed 表示测试未完成或失败，条目保持未验证。
	ServerConnectionTestFailed ServerConnectionTestState = "failed"
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
}

// ServerConnectionTester 是新增服务器后启动与读取连接测试任务的唯一入口。
// 实现必须立即返回任务标识；测试在后台执行，不能阻塞表单提交返回。
type ServerConnectionTester interface {
	StartServerTest(instance, serverID string, probeShell bool, source ServerWriteSource) (string, error)
	ServerTestStatus(taskID string) (*ServerConnectionTestStatus, bool)
}
