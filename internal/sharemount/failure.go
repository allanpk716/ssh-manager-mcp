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
