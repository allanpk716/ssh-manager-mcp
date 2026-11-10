package sharemount

// MountFailureClass 是挂载失败的固定分类代号。审计按代号记录结论，不依
// 赖错误文本；代号本身不含地址、挂载账号或机密。
const (
	MountClassUnsupportedSystem  = "unsupported-system"
	MountClassMissingSudo        = "missing-sudo"
	MountClassMissingSoftware    = "missing-software"
	MountClassLockTimeout        = "lock-timeout"
	MountClassRemoteVersionNewer = "remote-version-newer"
	MountClassProtocolFailed     = "protocol-failed"
	MountClassVerifyFailed       = "verify-failed"
	MountClassUnmountFailed      = "unmount-failed"
	MountClassNoDevice           = "no-device"
	MountClassSSHError           = "ssh-error"
	MountClassError              = "error"
)

// MountFailure 是带固定分类的挂载失败；错误文本仍面向使用者，分类供审计
// 与呈现层映射，不需要匹配文本。
type MountFailure struct {
	Class string
	Err   error
}

func (e *MountFailure) Error() string { return e.Err.Error() }

func (e *MountFailure) Unwrap() error { return e.Err }

func mountFailure(class string, err error) *MountFailure {
	return &MountFailure{Class: class, Err: err}
}

// CheckFailureReason 是现场检测无法得出结论的固定原因代号。
const (
	CheckFailureSSHConnection     = "ssh-connection"
	CheckFailureSudo              = "sudo"
	CheckFailureUnsupportedSystem = "unsupported-system"
)

// CheckFailure 是一台服务器现场检测失败的安全结论；呈现层用它给出
// 「不可达」的原因说明，不匹配错误文本。
type CheckFailure struct {
	Reason string
	Err    error
}

func (e *CheckFailure) Error() string { return e.Err.Error() }

func (e *CheckFailure) Unwrap() error { return e.Err }

// RemoteSudoFailure 表示固定命令的 sudo 提权没有完成（密码错误、
// sudoers 拒绝等）。它是进程内错误，不携带地址或机密。
type RemoteSudoFailure struct {
	Diagnostic string
}

func (e *RemoteSudoFailure) Error() string { return e.Diagnostic }
