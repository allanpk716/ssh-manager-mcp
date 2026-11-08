package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"ssh-manager-mcp/internal/updater"
)

// InstanceAmbiguousError 表示客户端有多个命名实例且未指定用哪一个。命令行
// 需要把原始错误原样交给使用者，因此保留内部错误。
type InstanceAmbiguousError struct {
	Err error
}

func (e *InstanceAmbiguousError) Error() string { return e.Err.Error() }

func (e *InstanceAmbiguousError) Unwrap() error { return e.Err }

// errNoRecommendedVersion 是客户端读不到推荐版本时的固定拒绝理由。
var errNoRecommendedVersion = errors.New("无法确定推荐版本，请先升级权威端，或用 `--version` 指定")

// UpdateRunRequest 是命令行更新链的一次执行请求；输出与确认都由调用方注入。
type UpdateRunRequest struct {
	// CheckOnly 对应 `--check`：只报告版本，不下载不替换。
	CheckOnly bool
	// WantTag 对应 `--version <tag>`。
	WantTag string
	// File 对应 `--file <包>`；SHA256Hex 与 NoVerify 只在该模式下生效。
	File      string
	SHA256Hex string
	NoVerify  bool
	// Instance 是客户端实例名。
	Instance string
	// Source 记录这次升级从哪里来，写审计时使用。
	Source Source
	// Output 输出证据行；调用方决定写到终端还是其他位置。
	Output func(format string, args ...any)
	// Confirm 在替换前询问确认；为空表示已确认（例如命令行 --yes）。
	Confirm func(prompt string) (bool, error)
	// Progress 在每个阶段开始时回调；可为空。
	Progress func(UpdateStep)
}

// UpdateRunResult 是命令行链执行后的摘要；重启服务分支由调用方继续处理。
type UpdateRunResult struct {
	ClientRole     bool
	CurrentVersion string
	TargetVersion  string
	CheckOnly      bool
}

// Run 执行命令行更新链的角色判定、版本发现、下载、校验、试运行、确认、替换
// 与代际信号。HTTPS、SHA-256 校验与回滚语义来自更新器原函数，未做任何改写。
func (s *UpdateService) Run(ctx context.Context, req UpdateRunRequest) (UpdateRunResult, error) {
	if !s.running.TryLock() {
		return UpdateRunResult{}, ErrUpdateInProgress
	}
	defer s.running.Unlock()

	out := req.Output
	say := func(format string, args ...any) {
		if out != nil {
			out(format, args...)
		}
	}
	report := func(step UpdateStep) {
		if req.Progress != nil {
			req.Progress(step)
		}
	}
	fail := func(step UpdateStep, err error, status UpdateStatus) (UpdateRunResult, error) {
		s.auditFailure(req, status, step)
		return UpdateRunResult{CurrentVersion: status.CurrentVersion, TargetVersion: status.TargetVersion},
			&UpdateError{Step: step, Err: err}
	}

	report(UpdateStepCheck)
	status := UpdateStatus{CurrentVersion: s.deps.CurrentVersion()}

	// 三个目标来源共用同一种角色判定；客户端不能越过权威端已声明的版本
	// 去装 GitHub 最新版。推荐版本只读本地加密缓存；这里不访问权威端，
	// 断网时也能执行。
	recommendedVersion := ""
	clientRole := false
	if req.File == "" && req.WantTag == "" {
		state, readErr := s.deps.ClientState(req.Instance)
		status.IsClient = state.IsClient
		if readErr != nil && state.IsClient {
			var ambiguous *InstanceAmbiguousError
			if errors.As(readErr, &ambiguous) {
				return UpdateRunResult{CurrentVersion: status.CurrentVersion}, readErr
			}
			return fail(UpdateStepCheck, errNoRecommendedVersion, status)
		}
		clientRole = state.IsClient
		if state.IsClient {
			recommendedVersion = state.RecommendedVersion
		}
		if clientRole && recommendedVersion == "" {
			return fail(UpdateStepCheck, errNoRecommendedVersion, status)
		}
	} else {
		state, _ := s.deps.ClientState(req.Instance)
		clientRole = state.IsClient
	}

	curStr := status.CurrentVersion
	curVer, curErr := updater.ParseVersion(curStr)

	var (
		rel        *updater.Release // nil on the --file path
		latestRel  *updater.Release // 客户端 --check 同时展示的 GitHub 最新版
		targetVer  string           // "v…" tag, or the staged output (--file)
		assetLabel string
		downgrade  bool
	)
	targetSource := targetFromGitHubLatest
	if req.WantTag != "" {
		targetSource = targetFromSpecifiedVersion
	} else if recommendedVersion != "" {
		targetSource = targetFromRecommendedVersion
	}

	if req.File == "" {
		var derr error
		switch targetSource {
		case targetFromSpecifiedVersion:
			rel, derr = s.deps.ReleaseByTag(ctx, req.WantTag)
		case targetFromRecommendedVersion:
			rel, derr = s.deps.ReleaseByTag(ctx, recommendedVersion)
			if derr == nil && req.CheckOnly {
				latestRel, derr = s.deps.LatestRelease(ctx)
			}
		default:
			rel, derr = s.deps.LatestRelease(ctx)
		}
		if derr != nil {
			return fail(UpdateStepCheck, derr, status)
		}
		targetVer = rel.Tag
		assetLabel = rel.AssetName
		status.TargetVersion = targetVer

		if curErr != nil {
			if targetSource == targetFromGitHubLatest {
				// dev / 本地构建:拒绝自动比较,要求显式 --version(spec §4.1)。
				return fail(UpdateStepCheck, fmt.Errorf(
					"无法解析当前版本 %q(本地构建?):自动比较已拒绝;请用 --version 显式指定目标版本", curStr), status)
			}
			say("警告: 无法判定升降级(当前版本 %q 无法解析);将按%s %s 继续\n", curStr, targetSource, rel.Tag)
		} else {
			tgt, terr := updater.ParseVersion(rel.Tag)
			if terr != nil {
				return fail(UpdateStepCheck, fmt.Errorf("目标 tag %s 不是可比较的版本: %w", rel.Tag, terr), status)
			}
			switch updater.CompareVersions(tgt, curVer) {
			case 0:
				if targetSource == targetFromSpecifiedVersion {
					return fail(UpdateStepCheck, fmt.Errorf(
						"已安装该版本(%s);如需重装请先卸载或选择其他版本", rel.Tag), status)
				}
				// 客户端 --check 仍要继续输出推荐版本与 GitHub 最新版本。
				if req.CheckOnly && targetSource == targetFromRecommendedVersion {
					break
				}
				say("已是最新(%s);无事可做\n", rel.Tag)
				return UpdateRunResult{
					ClientRole: clientRole, CurrentVersion: curStr, TargetVersion: targetVer, CheckOnly: req.CheckOnly,
				}, nil
			case -1:
				downgrade = true
			}
		}

		if req.CheckOnly {
			// 干跑:只报 当前/最新/资产名,零下载零替换。
			which := "最新版本"
			if targetSource == targetFromSpecifiedVersion {
				which = "目标版本"
			}
			if targetSource == targetFromRecommendedVersion {
				say("当前版本: %s\n推荐版本: %s\nGitHub 最新版本: %s\n",
					curStr, recommendedVersion, latestRel.Tag)
				say("资产: %s\n干跑:未下载、未替换、未重启任何东西\n", rel.AssetName)
				return UpdateRunResult{
					ClientRole: clientRole, CurrentVersion: curStr, TargetVersion: targetVer, CheckOnly: true,
				}, nil
			}
			say("当前版本: %s\n%s: %s\n资产: %s\n干跑:未下载、未替换、未重启任何东西\n",
				curStr, which, rel.Tag, rel.AssetName)
			return UpdateRunResult{
				ClientRole: clientRole, CurrentVersion: curStr, TargetVersion: targetVer, CheckOnly: true,
			}, nil
		}
		say("版本: %s → %s\n资产: %s\n", curStr, rel.Tag, rel.AssetName)
	} else {
		assetLabel = "--file " + req.File
		if curErr != nil {
			say("警告: 无法判定升降级(当前版本 %q 无法解析);目标版本以 staged 输出为准\n", curStr)
		}
	}

	self, err := s.deps.SelfPath()
	if err != nil {
		return fail(UpdateStepCheck, fmt.Errorf("定位自身可执行文件: %w", err), status)
	}

	report(UpdateStepDownload)
	tmpdir, err := os.MkdirTemp(filepath.Dir(self), ".sshmgr-update-tmp-*")
	if err != nil {
		// exe 目录不可写的确定性出错点(spec §4.3:明确报错提示 sudo/管理员,
		// 不自动提权——给一条有出路的指引)。
		return fail(UpdateStepDownload, fmt.Errorf(
			"在 exe 同目录创建临时目录(同卷保证 rename 原子): %w\n"+
				"该目录不可写——用管理员/sudo 运行,或将二进制移至用户可写目录;update 不会自动提权", err), status)
	}
	defer os.RemoveAll(tmpdir) // 零残留:staged 成功时已被 rename 走,失败时整目录清理

	var (
		archivePath string
		shaLabel    string
	)
	if req.File == "" {
		// checksums.txt 自举(空 hash 仅限它),再以解析出的 hash 下资产(T4 硬约束)。
		report(UpdateStepDownload)
		sumPath, derr := s.deps.DownloadAsset(ctx, rel.ChecksumsURL, "", tmpdir)
		if derr != nil {
			return fail(UpdateStepDownload, derr, status)
		}
		data, rerr := os.ReadFile(sumPath)
		if rerr != nil {
			return fail(UpdateStepVerify, fmt.Errorf("读取 %s: %w", filepath.Base(sumPath), rerr), status)
		}
		report(UpdateStepVerify)
		want, perr := s.deps.ParseChecksums(data, rel.AssetName)
		if perr != nil {
			return fail(UpdateStepVerify, perr, status)
		}
		archivePath, derr = s.deps.DownloadAsset(ctx, rel.AssetURL, want, tmpdir)
		if derr != nil {
			return fail(UpdateStepVerify, derr, status)
		}
		shaLabel = "SHA256: 命中 " + want
	} else {
		// --sha256 在复制前就核对(尽早失败);--no-verify 打印未校验警告留档。
		if req.SHA256Hex != "" {
			got, herr := fileSHA256(req.File)
			if herr != nil {
				return fail(UpdateStepVerify, herr, status)
			}
			if got != strings.ToLower(req.SHA256Hex) {
				return fail(UpdateStepVerify, fmt.Errorf(
					"--file sha256 不匹配: got %s want %s(目标文件零触碰)", got, req.SHA256Hex), status)
			}
			shaLabel = "SHA256: 命中(--sha256) " + strings.ToLower(req.SHA256Hex)
		} else {
			say("警告: --file 未做任何完整性校验(--no-verify;owner 手供文件,信任=你下载的它)\n")
			shaLabel = "SHA256: 未校验(--no-verify)"
		}
		// 复制进 exe 同目录的 tmpdir:ExtractBinary 把 staged 落在归档旁边,
		// 不能在用户的包目录里写文件。
		archivePath = filepath.Join(tmpdir, filepath.Base(req.File))
		if cerr := copyFile(req.File, archivePath, 0o755); cerr != nil {
			return fail(UpdateStepDownload, fmt.Errorf("复制本地包到临时目录: %w", cerr), status)
		}
	}

	report(UpdateStepVerify)
	staged, xerr := s.deps.ExtractBinary(archivePath, runtime.GOOS)
	if xerr != nil {
		return fail(UpdateStepVerify, xerr, status)
	}

	stagedReport := ""
	if req.File == "" {
		report(UpdateStepStaged)
		got, serr := s.deps.StagedVersionCheck(staged, targetVer)
		if serr != nil {
			return fail(UpdateStepStaged, fmt.Errorf("staged 自检未通过(保留原文件,零变更): %w", serr), status)
		}
		stagedReport = got
	} else {
		report(UpdateStepStaged)
		// --file 路径:staged 输出本身即目标版本,没有先行可比的 want。
		// 以 want="" 探测——StagedVersionCheck 契约保证:got=="" ⟺ staged
		// 没能回答(无法执行/超时/超限/空输出),此时中止;got!="" 时的
		// mismatch 错误是本路径的设计产物(目标在它回答之前不存在),取 got。
		got, serr := s.deps.StagedVersionCheck(staged, "")
		if got == "" {
			return fail(UpdateStepStaged, fmt.Errorf("staged 自检未通过(保留原文件,零变更): %w", serr), status)
		}
		targetVer = got
		status.TargetVersion = got
		stagedReport = got

		// "已装该版本"拒绝与降级判定以 staged 输出为基准(spec §4.2(5))。
		if targetVer == curStr {
			return fail(UpdateStepStaged, fmt.Errorf(
				"已装该版本(%s);--file 提供的包与当前版本相同", targetVer), status)
		}
		if curErr == nil {
			if fv, ferr := updater.ParseVersion(targetVer); ferr == nil {
				switch updater.CompareVersions(fv, curVer) {
				case 0:
					return fail(UpdateStepStaged, fmt.Errorf(
						"已装该版本(%s);--file 提供的包与当前版本相同", targetVer), status)
				case -1:
					downgrade = true
				}
			} else {
				say("警告: 无法判定升降级(staged 输出 %q 不是版本号);按 owner 指定继续\n", targetVer)
			}
		}
		say("版本: %s → %s(staged)\n资产: %s\n", curStr, targetVer, assetLabel)
	}
	say("%s\nstaged 自检: OK (%s)\n", shaLabel, stagedReport)

	// --- 确认 + 降级提示 ---------------------------------------------------------
	if downgrade {
		// 降级 = 回滚通道:即使 --yes 也打印醒目行(spec §4.4)。
		say("⚠ 降级至 %s(回滚通道)\n", targetVer)
	}
	if req.Confirm != nil {
		action := "更新"
		if downgrade {
			action = "降级"
		}
		prompt := fmt.Sprintf("将%s sshmgr:%s → %s(资产 %s)。继续?[Y/n]", action, curStr, targetVer, assetLabel)
		ok, cerr := req.Confirm(prompt)
		if cerr != nil {
			return UpdateRunResult{CurrentVersion: curStr, TargetVersion: targetVer}, cerr
		}
		if !ok {
			say("已取消(零变更)\n")
			return UpdateRunResult{
				ClientRole: clientRole, CurrentVersion: curStr, TargetVersion: targetVer,
			}, nil
		}
	}

	// --- 事务性替换 --------------------------------------------------------------
	report(UpdateStepReplace)
	replaceErr := s.deps.ReplaceBinary(staged, self)
	if replaceErr != nil {
		var committed *updater.CommittedWithError
		if errors.As(replaceErr, &committed) {
			// committed-with-error:替换已生效,不回滚不掩饰,如实报告后继续。
			say("警告(已提交): %v\n", replaceErr)
		} else {
			return fail(UpdateStepReplace, fmt.Errorf("替换失败: %w", replaceErr), status)
		}
	}
	say("替换: %s\n", self)

	// 替换成功点(GitHub 与 --file 两条路在此汇合;CommittedWithError 同样落到
	// 这里——盘上二进制确已换过)。在二进制同目录写代际信号(桥热升级实施
	// 决策第 1 条)。信号写失败不推翻已提交的替换:警告后继续。
	if _, werr := s.deps.WriteSignal(self, targetVer); werr != nil {
		say("警告: 写入代际信号失败(运行中的桥感知不到本次换版,下次 update 重写): %v\n", werr)
	}

	entry := UpdateAuditEntry{
		Action: "update.apply", OldVersion: curStr, NewVersion: targetVer, Result: "ok",
	}
	if err := s.writeAudit(req.Source, entry); err != nil {
		say("警告: 写入升级审计失败: %v\n", err)
	}
	return UpdateRunResult{
		ClientRole: clientRole, CurrentVersion: curStr, TargetVersion: targetVer,
	}, nil
}

// auditFailure 给没有产生替换的失败留一条审计记录；这里记录的是目标已确定
// 但未提交的失败，替换成功路径由 Run/Apply 各自写入。
func (s *UpdateService) auditFailure(req UpdateRunRequest, status UpdateStatus, step UpdateStep) {
	if status.TargetVersion == "" {
		return
	}
	entry := UpdateAuditEntry{
		Action: "update.apply", OldVersion: status.CurrentVersion, NewVersion: status.TargetVersion,
		Result: "error", Detail: step.Text(),
	}
	_ = s.writeAudit(req.Source, entry)
}

// updateTargetSource 表示不带 --file 时目标版本的来源；这个判定只做一次，
// 后续版本发现、比较与输出都复用同一结果。
type updateTargetSource int

const (
	targetFromSpecifiedVersion updateTargetSource = iota
	targetFromRecommendedVersion
	targetFromGitHubLatest
)

func (s updateTargetSource) String() string {
	switch s {
	case targetFromSpecifiedVersion:
		return "指定版本"
	case targetFromRecommendedVersion:
		return "推荐版本"
	default:
		return "GitHub 最新版本"
	}
}

// fileSHA256 returns the lowercase hex sha256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开 --file %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("计算 --file %s 的 sha256: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile 把本地包复制到临时目录，保持可执行权限。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
