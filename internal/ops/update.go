package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/updater"
)

// UpdateClientState 是客户端角色读取本地快照后的版本判定结果。
type UpdateClientState struct {
	// IsClient 表示本机是客户端角色。
	IsClient bool
	// RecommendedVersion 是本地快照里的推荐版本；读不到时为空串。
	RecommendedVersion string
}

// UpdateDependencies 是升级服务的可注入依赖；零值字段在构造时填入生产实现。
type UpdateDependencies struct {
	// SelfPath 定位要被替换的二进制。
	SelfPath func() (string, error)
	// CurrentVersion 返回当前版本。
	CurrentVersion func() string
	// ClientState 按实例读取客户端角色与推荐版本；instance 为空表示默认实例。
	ClientState func(instance string) (UpdateClientState, error)
	// Audit 写审计行；为空时跳过审计。
	Audit func(source Source, entry UpdateAuditEntry) error

	// 以下函数是更新器各阶段的可替换依赖；测试与不同部署形态可注入替身。
	LatestRelease      func(context.Context) (*updater.Release, error)
	ReleaseByTag       func(context.Context, string) (*updater.Release, error)
	DownloadAsset      func(context.Context, string, string, string) (string, error)
	ParseChecksums     func([]byte, string) (string, error)
	ExtractBinary      func(string, string) (string, error)
	StagedVersionCheck func(string, string) (string, error)
	ReplaceBinary      func(string, string) error
	WriteSignal        func(string, string) (updater.GenerationSignal, error)
}

// UpdateStatus 是升级页与检查命令共用的版本状态。
type UpdateStatus struct {
	CurrentVersion     string
	release            *updater.Release
	RecommendedVersion string
	LatestVersion      string
	LatestError        string
	TargetVersion      string
	AssetName          string
	CanUpgrade         bool
	Reason             string
	Downgrade          bool
	NewerThanAuthority bool
	IsClient           bool
}

// UpdateService 是一次进程内共享的升级服务；同一时间只允许一个升级在执行。
type UpdateService struct {
	deps     UpdateDependencies
	running  sync.Mutex
	inFlight atomic.Int32
	// downloadPause 只给并发测试使用：下载开始前阻塞。
	downloadPause func()
}

// NewUpdateService 用给定依赖构造升级服务，空依赖填入生产实现。
func NewUpdateService(deps UpdateDependencies) *UpdateService {
	if deps.SelfPath == nil {
		deps.SelfPath = defaultSelfPath
	}
	if deps.CurrentVersion == nil {
		deps.CurrentVersion = defaultCurrentVersion
	}
	if deps.ClientState == nil {
		deps.ClientState = func(string) (UpdateClientState, error) {
			return UpdateClientState{}, nil
		}
	}
	if deps.LatestRelease == nil {
		deps.LatestRelease = updater.LatestRelease
	}
	if deps.ReleaseByTag == nil {
		deps.ReleaseByTag = updater.ReleaseByTag
	}
	if deps.DownloadAsset == nil {
		deps.DownloadAsset = updater.DownloadAsset
	}
	if deps.ParseChecksums == nil {
		deps.ParseChecksums = updater.ParseChecksums
	}
	if deps.ExtractBinary == nil {
		deps.ExtractBinary = updater.ExtractBinary
	}
	if deps.StagedVersionCheck == nil {
		deps.StagedVersionCheck = updater.StagedVersionCheck
	}
	if deps.ReplaceBinary == nil {
		deps.ReplaceBinary = updater.ReplaceBinary
	}
	if deps.WriteSignal == nil {
		deps.WriteSignal = updater.WriteGenerationSignal
	}
	return &UpdateService{deps: deps}
}

// Check 汇总当前版本、推荐版本、GitHub 最新版本与目标版本，并给出能否升级
// 与原因；不下载任何内容。
func (s *UpdateService) Check(ctx context.Context, instance string) (UpdateStatus, error) {
	status := UpdateStatus{CurrentVersion: s.deps.CurrentVersion()}
	state, err := s.deps.ClientState(instance)
	if err != nil {
		status.Reason = err.Error()
		return status, nil
	}
	status.IsClient = state.IsClient
	status.RecommendedVersion = state.RecommendedVersion
	if !state.IsClient {
		status.RecommendedVersion = status.CurrentVersion
	}

	release, releaseErr := s.latestRelease(ctx)
	if releaseErr != nil {
		status.LatestError = releaseErr.Error()
		if !state.IsClient {
			status.Reason = "读取 GitHub 最新版本失败：" + releaseErr.Error()
			return status, nil
		}
	} else {
		status.LatestVersion = release.Tag
		status.release = release
	}

	target := release
	if state.IsClient {
		if state.RecommendedVersion == "" {
			status.Reason = "读不到推荐版本，请先同步或先升级权威端"
			return status, nil
		}
		byTag, byTagErr := s.deps.ReleaseByTag(ctx, state.RecommendedVersion)
		if byTagErr != nil {
			status.Reason = "读取推荐版本发布信息失败：" + byTagErr.Error()
			return status, nil
		}
		target = byTag
		status.release = byTag
	}
	status.TargetVersion = target.Tag
	status.AssetName = target.AssetName

	current, currentErr := updater.ParseVersion(status.CurrentVersion)
	targetVersion, targetErr := updater.ParseVersion(target.Tag)
	switch {
	case currentErr != nil || targetErr != nil:
		status.CanUpgrade = true
		status.Reason = "当前版本无法解析，将按目标版本继续"
	case updater.CompareVersions(targetVersion, current) > 0:
		status.CanUpgrade = true
	case updater.CompareVersions(targetVersion, current) == 0:
		status.Reason = "已是最新版本"
	case status.IsClient:
		status.Downgrade = true
		status.NewerThanAuthority = true
		status.CanUpgrade = true
		status.Reason = "当前版本比权威端新"
	default:
		status.Downgrade = true
		status.CanUpgrade = true
		status.Reason = "目标版本比当前版本旧（回滚通道）"
	}
	return status, nil
}

func (s *UpdateService) latestRelease(ctx context.Context) (*updater.Release, error) {
	return s.deps.LatestRelease(ctx)
}

// defaultSelfPath 定位当前进程的二进制（解析符号链接后的规范路径）。
func defaultSelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// defaultCurrentVersion 返回编译进二进制的版本号。
func defaultCurrentVersion() string { return buildinfo.Version }

// UpdateStep 标记升级链中的一个阶段；失败时页面与命令行据此说明失败在哪一步。
type UpdateStep string

const (
	UpdateStepCheck    UpdateStep = "check"
	UpdateStepDownload UpdateStep = "download"
	UpdateStepVerify   UpdateStep = "verify"
	UpdateStepStaged   UpdateStep = "staged"
	UpdateStepReplace  UpdateStep = "replace"
)

// Text 返回阶段的中文显示名。
func (s UpdateStep) Text() string {
	switch s {
	case UpdateStepCheck:
		return "检查"
	case UpdateStepDownload:
		return "下载"
	case UpdateStepVerify:
		return "校验"
	case UpdateStepStaged:
		return "试运行"
	case UpdateStepReplace:
		return "替换"
	default:
		return string(s)
	}
}

// UpdateError 带失败阶段的升级错误。
type UpdateError struct {
	Step UpdateStep
	Err  error
}

func (e *UpdateError) Error() string {
	return fmt.Sprintf("%s失败：%v", e.Step.Text(), e.Err)
}

func (e *UpdateError) Unwrap() error { return e.Err }

// ErrUpdateInProgress 表示同一时间已有一次升级在执行。
var ErrUpdateInProgress = errors.New("升级正在进行")

// UpdateApplyRequest 是一次替换请求。Web 表单提交本身就是确认，因此这里
// 不再询问；命令行链路把交互确认注入在专用入口里。
type UpdateApplyRequest struct {
	// Instance 是客户端实例名；空串表示默认实例。
	Instance string
	// Source 记录这次升级从哪里来，写审计时使用。
	Source Source
	// Progress 在每个阶段开始时回调；可为空。
	Progress func(UpdateStep)
}

// UpdateApplyResult 是一次已提交的替换结果。
type UpdateApplyResult struct {
	OldVersion string
	NewVersion string
	BinaryPath string
	// Warning 汇总不推翻替换结果的警告，例如代际信号写入失败。
	Warning string
}

// UpdateAuditEntry 是升级动作写审计时使用的摘要字段；不含任何令牌。
type UpdateAuditEntry struct {
	Action     string
	OldVersion string
	NewVersion string
	Result     string
	Detail     string
}

// AuditFields 返回审计行的来源与来源地址列。
func (s Source) AuditFields() (source, sourceIP string) {
	return s.kind, s.addr
}

// Apply 执行检查、下载、校验、试运行、替换与写代际信号的完整链路。除检查
// 与替换二进制外不改任何状态；失败时返回带阶段信息的 UpdateError。
func (s *UpdateService) Apply(ctx context.Context, req UpdateApplyRequest) (UpdateApplyResult, error) {
	if !s.running.TryLock() {
		return UpdateApplyResult{}, ErrUpdateInProgress
	}
	defer s.running.Unlock()
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	if s.downloadPause != nil {
		s.downloadPause()
	}

	report := func(step UpdateStep) {
		if req.Progress != nil {
			req.Progress(step)
		}
	}
	fail := func(step UpdateStep, err error, status UpdateStatus) (UpdateApplyResult, error) {
		s.writeAudit(req.Source, UpdateAuditEntry{
			Action: "update.apply", OldVersion: status.CurrentVersion, NewVersion: status.TargetVersion,
			Result: "error", Detail: step.Text(),
		})
		return UpdateApplyResult{OldVersion: status.CurrentVersion, NewVersion: status.TargetVersion},
			&UpdateError{Step: step, Err: err}
	}

	report(UpdateStepCheck)
	status, err := s.Check(ctx, req.Instance)
	if err != nil {
		return fail(UpdateStepCheck, err, status)
	}
	if !status.CanUpgrade {
		return fail(UpdateStepCheck, errors.New(status.Reason), status)
	}
	self, err := s.deps.SelfPath()
	if err != nil {
		return fail(UpdateStepCheck, err, status)
	}

	tmpdir, err := os.MkdirTemp(filepath.Dir(self), ".sshmgr-update-tmp-*")
	if err != nil {
		return fail(UpdateStepDownload, fmt.Errorf("在二进制同目录创建临时目录：%w", err), status)
	}
	defer os.RemoveAll(tmpdir)

	report(UpdateStepDownload)
	sumPath, err := s.deps.DownloadAsset(ctx, status.release.ChecksumsURL, "", tmpdir)
	if err != nil {
		return fail(UpdateStepDownload, err, status)
	}
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return fail(UpdateStepVerify, fmt.Errorf("读取校验和清单：%w", err), status)
	}

	report(UpdateStepVerify)
	want, err := s.deps.ParseChecksums(data, status.AssetName)
	if err != nil {
		return fail(UpdateStepVerify, err, status)
	}
	archivePath, err := s.deps.DownloadAsset(ctx, status.release.AssetURL, want, tmpdir)
	if err != nil {
		return fail(UpdateStepVerify, err, status)
	}
	staged, err := s.deps.ExtractBinary(archivePath, runtime.GOOS)
	if err != nil {
		return fail(UpdateStepVerify, err, status)
	}

	report(UpdateStepStaged)
	if _, err := s.deps.StagedVersionCheck(staged, status.TargetVersion); err != nil {
		return fail(UpdateStepStaged, fmt.Errorf("保留原文件，零变更：%w", err), status)
	}

	report(UpdateStepReplace)
	replaceErr := s.deps.ReplaceBinary(staged, self)
	result := UpdateApplyResult{
		OldVersion: status.CurrentVersion, NewVersion: status.TargetVersion, BinaryPath: self,
	}
	var committedWarning *updater.CommittedWithError
	if errors.As(replaceErr, &committedWarning) {
		result.Warning = committedWarning.Error()
	} else if replaceErr != nil {
		return fail(UpdateStepReplace, replaceErr, status)
	}
	if _, werr := s.deps.WriteSignal(self, status.TargetVersion); werr != nil {
		warning := fmt.Sprintf("写入代际信号失败（运行中的桥感知不到本次换版，下次升级会重写）：%v", werr)
		result.Warning = strings.Join(nonEmpty(result.Warning, warning), "；")
	}
	entry := UpdateAuditEntry{
		Action: "update.apply", OldVersion: result.OldVersion, NewVersion: result.NewVersion,
		Result: "ok", Detail: result.Warning,
	}
	s.writeAudit(req.Source, entry)
	return result, nil
}

// UpgradeInProgress 报告当前是否已有一次升级在执行。
func (s *UpdateService) UpgradeInProgress() bool {
	return s.inFlight.Load() == 1
}

// writeAudit 尽力写审计：审计失败只并入警告，不让已提交的替换被误报为失败。
func (s *UpdateService) writeAudit(source Source, entry UpdateAuditEntry) error {
	if s.deps.Audit == nil {
		return nil
	}
	return s.deps.Audit(source, entry)
}

// WriteUpdateAudit 把替换二进制的结果写入权威 vault 审计表。
func (m *Manager) WriteUpdateAudit(source Source, entry UpdateAuditEntry) error {
	summary := map[string]any{
		"old_version": entry.OldVersion, "new_version": entry.NewVersion, "result": entry.Result,
	}
	if entry.Detail != "" {
		summary["detail"] = entry.Detail
	}
	row := store.OwnerAuditRow(entry.Action, summary)
	row.Source, row.SourceIP = source.AuditFields()
	if entry.Result != "ok" {
		row.Status = "error"
	}
	return m.st.WriteAudit(row)
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// SetDownloadPause 注入下载开始前的阻塞点；并发测试用它持有第一次升级，
// 生产路径不要调用。传 nil 取消阻塞。
func (s *UpdateService) SetDownloadPause(pause func()) {
	s.downloadPause = pause
}
