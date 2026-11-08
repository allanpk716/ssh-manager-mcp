package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// 备份文件的固定前缀与计划任务名：与 `backup create` 的默认前缀、部署安
// 装的计划任务名一致。
const (
	backupPrefix          = "vault"
	backupTaskName        = "sshmgr-backup"
	backupFreshnessBudget = 25 * time.Hour
)

// 校验失败的类别固定值：口令不符、文件损坏、文件不完整。
const (
	BackupFailurePassphrase = "passphrase"
	BackupFailureCorrupt    = "corrupt"
	BackupFailureIncomplete = "incomplete"
)

// BackupVerifyFailure 带上校验失败的类别；类别供备份页与状态命令展示，
// 原始错误保留给调用方输出。
type BackupVerifyFailure struct {
	Kind string
	Err  error
}

func (e *BackupVerifyFailure) Error() string { return e.Err.Error() }
func (e *BackupVerifyFailure) Unwrap() error { return e.Err }

// BackupVerifyFailureKind 从错误里取失败类别；不是备份校验失败时返回空串。
func BackupVerifyFailureKind(err error) string {
	var failure *BackupVerifyFailure
	if errors.As(err, &failure) {
		return failure.Kind
	}
	return ""
}

// VerifyEncryptedBackupBlob 对已读入的加密备份执行与 `backup verify` 相同
// 的两道校验：口令必须能解密，明文必须能重新解析为 vault 快照。
func VerifyEncryptedBackupBlob(file string, blob []byte, passphraseFile string) error {
	if passphraseFile == "" {
		return fmt.Errorf("%s is a passphrase-encrypted backup; --passphrase-file is required", filepath.Base(file))
	}
	passphrase, err := ReadPassphraseFile(passphraseFile)
	if err != nil {
		return err
	}
	plain, err := vaultio.Decrypt(passphrase, blob)
	if err != nil {
		return &BackupVerifyFailure{
			Kind: classifyDecryptFailure(err),
			Err:  fmt.Errorf("decrypt failed (wrong passphrase, or the file is corrupted): %w", err),
		}
	}
	if err := json.Unmarshal(plain, &store.Snapshot{}); err != nil {
		return &BackupVerifyFailure{
			Kind: BackupFailureCorrupt,
			Err:  fmt.Errorf("re-unmarshal failed: %w", err),
		}
	}
	return nil
}

// classifyDecryptFailure 把解密错误映射到失败类别：内容过短是文件不完整，
// 文件头不对是文件损坏；解密认证失败无法区分口令不符与篡改，按口令不符
// 归类并在展示文案里同时提示两种可能。
func classifyDecryptFailure(err error) string {
	if errors.Is(err, vaultio.ErrTruncated) {
		return BackupFailureIncomplete
	}
	if errors.Is(err, vaultio.ErrBadMagic) {
		return BackupFailureCorrupt
	}
	return BackupFailurePassphrase
}

// BackupVerifyResult 是一键校验最新备份的返回值。
type BackupVerifyResult struct {
	File        string
	OK          bool
	FailureKind string
}

// VerifyLatestBackup 按 backup.json 自动找到最新备份与口令文件并校验，
// 结果写入 backup.verify 审计行。
func (m *Manager) VerifyLatestBackup(source Source) (BackupVerifyResult, error) {
	cfg, err := m.loadBackupConfig()
	if err != nil {
		return BackupVerifyResult{}, err
	}
	latest, ok := LatestEncryptedBackup(cfg.Dir, backupPrefix)
	if !ok {
		return BackupVerifyResult{}, fmt.Errorf("no encrypted backup found in %s", cfg.Dir)
	}
	blob, err := os.ReadFile(latest.Path)
	if err != nil {
		return BackupVerifyResult{}, err
	}
	verifyErr := VerifyEncryptedBackupBlob(latest.Path, blob, cfg.PassphraseFile)
	summary := map[string]any{"file": latest.Name, "result": "ok"}
	if verifyErr != nil {
		summary["result"] = "error"
		if kind := BackupVerifyFailureKind(verifyErr); kind != "" {
			summary["failure_kind"] = kind
		}
	}
	auditErr := m.writeAudit("backup.verify", summary, verifyErr, source)
	if verifyErr != nil {
		return BackupVerifyResult{
			File: latest.Name, FailureKind: BackupVerifyFailureKind(verifyErr),
		}, verifyErr
	}
	return BackupVerifyResult{File: latest.Name, OK: true}, auditErr
}

// loadBackupConfig 从标准位置读取备份配置；配置缺失或有误时返回错误。
func (m *Manager) loadBackupConfig() (*BackupConfig, error) {
	path, err := m.backupConfigPath()
	if err != nil {
		return nil, fmt.Errorf("定位备份配置失败：%w", err)
	}
	cfg, err := LoadBackupConfig(path)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("backup config path is empty")
	}
	return cfg, nil
}

// BackupConfig 是解析并通过校验的 backup.json：备份目录、保留份数与口令
// 文件路径。
type BackupConfig struct {
	Dir            string
	Keep           int
	PassphraseFile string
}

// backupConfigFile 镜像 backup.json 的原始字段。用指针字段区分「字段缺
// 失」与「零值」：keep 为 0 表示不轮转，是合法值。
type backupConfigFile struct {
	Dir            *string `json:"dir"`
	Keep           *int    `json:"keep"`
	PassphraseFile *string `json:"passphrase_file"`
}

// LoadBackupConfig 读取并校验一份 backup.json。空路径表示「没有传入配
// 置」，返回 (nil, nil)。缺失、非法 JSON、缺字段、相对路径、口令文件在
// 备份目录里都是硬错误。
func LoadBackupConfig(path string) (*BackupConfig, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("backup config: file not found: %s: %w", path, err)
		}
		return nil, fmt.Errorf("backup config: reading %s: %w", path, err)
	}
	var cf backupConfigFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, fmt.Errorf("backup config: %s is not valid JSON: %v", path, err)
	}
	var missing []string
	if cf.Dir == nil {
		missing = append(missing, `"dir"`)
	}
	if cf.Keep == nil {
		missing = append(missing, `"keep"`)
	}
	if cf.PassphraseFile == nil {
		missing = append(missing, `"passphrase_file"`)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("backup config: missing required field(s) %s in %s",
			strings.Join(missing, ", "), path)
	}
	if !filepath.IsAbs(*cf.Dir) {
		return nil, fmt.Errorf("backup config: %q must be an absolute path, got %q in %s",
			"dir", *cf.Dir, path)
	}
	if !filepath.IsAbs(*cf.PassphraseFile) {
		return nil, fmt.Errorf("backup config: %q must be an absolute path, got %q in %s",
			"passphrase_file", *cf.PassphraseFile, path)
	}
	if err := ValidatePassphraseNotInDir(*cf.Dir, *cf.PassphraseFile); err != nil {
		return nil, fmt.Errorf("backup config: %w", err)
	}
	return &BackupConfig{Dir: *cf.Dir, Keep: *cf.Keep, PassphraseFile: *cf.PassphraseFile}, nil
}

// ValidateBackupConfigValues 对表单里的备份配置值执行与 LoadBackupConfig
// 相同的值校验：两个路径必须是绝对路径，且口令文件不能位于备份目录里。
func ValidateBackupConfigValues(dir, passphraseFile string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("backup config: %q must be an absolute path, got %q", "dir", dir)
	}
	if !filepath.IsAbs(passphraseFile) {
		return fmt.Errorf("backup config: %q must be an absolute path, got %q", "passphrase_file", passphraseFile)
	}
	return ValidatePassphraseNotInDir(dir, passphraseFile)
}

// SaveBackupConfig 按 LoadBackupConfig 读取的同一格式写 backup.json，使用
// 与备份文件相同的原子写法。
func SaveBackupConfig(configPath, dir string, keep int, passphraseFile string) error {
	data, err := json.Marshal(backupConfigFile{Dir: &dir, Keep: &keep, PassphraseFile: &passphraseFile})
	if err != nil {
		return err
	}
	_, err = AtomicWriteFile(filepath.Dir(configPath), filepath.Base(configPath), data)
	return err
}

// ValidatePassphraseNotInDir 强制口令文件位置约束：口令文件不能位于备份
// 目录内（或就是备份目录本身），否则密钥会与它解开的密文放在同一处。
func ValidatePassphraseNotInDir(dir, passphraseFile string) error {
	resolvedDir, err := resolvePathForComparison(dir)
	if err != nil {
		return fmt.Errorf("resolving backup dir %s: %w", dir, err)
	}
	resolvedPassphraseFile, err := resolvePathForComparison(passphraseFile)
	if err != nil {
		return fmt.Errorf("resolving passphrase_file %s: %w", passphraseFile, err)
	}
	if pathInsideDirTree(resolvedPassphraseFile, resolvedDir) {
		return fmt.Errorf("passphrase_file %s must not be inside the backup dir %s — the passphrase file cannot live in the directory being backed up; move it outside the backup directory",
			passphraseFile, dir)
	}
	return nil
}

// resolvePathForComparison 解析符号链接，让同一物理位置的两种写法可以相
// 等比较。尚不存在的路径按最近存在的父目录解析后拼回缺失元素；其他失败
// 原样返回，保持校验闭合。
func resolvePathForComparison(path string) (string, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolutePath)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if len(missing) == 0 {
				return resolved, nil
			}
			return filepath.Join(append([]string{resolved}, missing...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// EvalSymlinks 对断开的符号链接也会报「不存在」；链接本身存在时直接
		// 失败，避免把它当缺失元素拆掉后丢失目标。
		if _, statErr := os.Lstat(current); statErr == nil {
			return "", err
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

// pathInsideDirTree 判断 child 是否就是 dir 或位于 dir 之下。两侧都应是
// 清理过的路径；卷名必须一致，Windows 上再按大小写不敏感比较。
func pathInsideDirTree(child, dir string) bool {
	volChild, volDir := filepath.VolumeName(child), filepath.VolumeName(dir)
	if !strings.EqualFold(volChild, volDir) {
		return false
	}
	restChild := strings.TrimPrefix(child, volChild)
	restDir := strings.TrimPrefix(dir, volDir)
	if runtime.GOOS == "windows" {
		restChild = strings.ToLower(restChild)
		restDir = strings.ToLower(restDir)
	}
	if restChild == restDir {
		return true
	}
	if !strings.HasSuffix(restDir, string(filepath.Separator)) {
		restDir += string(filepath.Separator)
	}
	return strings.HasPrefix(restChild, restDir)
}

// ReadPassphraseFile 从文件读取口令，只去掉一个结尾换行（echo/printf 写
// 法都能用）。文件权限由部署者负责。
func ReadPassphraseFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --passphrase-file: %w", err)
	}
	return []byte(strings.TrimSuffix(strings.TrimSuffix(string(raw), "\r\n"), "\n")), nil
}

// AtomicWriteFile 把数据写入临时文件（0600）、同步落盘、改名为最终文件，
// 任何失败都会清理临时文件。非 Windows 上还会同步父目录。
func AtomicWriteFile(dir, name string, data []byte) (string, error) {
	final := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return final, nil
}

// BackupFile 是备份目录里的一个备份文件；ModTime 只用于显示与新鲜度判
// 断，选取顺序始终按文件名。
type BackupFile struct {
	Name    string
	Path    string
	ModTime time.Time
}

// ListEncryptedBackups 按文件名从新到旧列出备份目录里的加密备份；与轮
// 转和口令试解使用同一条排序规则。目录不存在或不可读时返回空列表。
func ListEncryptedBackups(dir, prefix string) []BackupFile {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.sme"))
	if err != nil {
		return nil
	}
	out := make([]BackupFile, 0, len(matches))
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || info.IsDir() {
			continue
		}
		out = append(out, BackupFile{Name: filepath.Base(match), Path: match, ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// LatestEncryptedBackup 按文件名选出最新的加密备份。
func LatestEncryptedBackup(dir, prefix string) (BackupFile, bool) {
	files := ListEncryptedBackups(dir, prefix)
	if len(files) == 0 {
		return BackupFile{}, false
	}
	return files[0], true
}

// BackupStatus 是备份页与 `backup status` 共用的状态快照。页面加载时不
// 解密备份；校验状态来自最近一次校验的审计记录。
type BackupStatus struct {
	ConfigPath string
	// Config 为 nil 表示配置缺失或有误，具体原因看 ConfigError。
	Config        *BackupConfig
	ConfigError   string
	ConfigMissing bool
	// TaskInstalled 表示计划任务已安装；TaskProbed 为 false 表示当前平
	// 台不做探测（仅支持 Windows 计划任务）。
	TaskInstalled bool
	TaskProbed    bool
	Total         int
	LatestName    string
	LatestTime    time.Time
	HasLatest     bool
	Stale         bool
	Verify        *BackupVerifyState
}

// BackupVerifyState 是针对某个备份文件的最近一次校验结果。
type BackupVerifyState struct {
	File        string
	OK          bool
	FailureKind string
	At          time.Time
}

// BackupStatus 汇总备份配置、计划任务探测、备份清单、新鲜度与最近一次
// 校验结果。
func (m *Manager) BackupStatus() (BackupStatus, error) {
	var status BackupStatus
	path, err := m.backupConfigPath()
	if err != nil {
		return status, fmt.Errorf("定位备份配置失败：%w", err)
	}
	status.ConfigPath = path
	cfg, cfgErr := LoadBackupConfig(path)
	if cfgErr != nil {
		status.ConfigError = cfgErr.Error()
		status.ConfigMissing = errors.Is(cfgErr, fs.ErrNotExist)
		status.TaskInstalled, status.TaskProbed = m.backupScheduleProbe()
		return status, nil
	}
	status.Config = cfg
	files := ListEncryptedBackups(cfg.Dir, backupPrefix)
	status.Total = len(files)
	if len(files) > 0 {
		latest := files[0]
		status.HasLatest = true
		status.LatestName = latest.Name
		status.LatestTime = latest.ModTime
		status.Stale = m.now().Sub(latest.ModTime) > backupFreshnessBudget
		status.Verify = m.latestVerifyState(latest.Name)
	}
	status.TaskInstalled, status.TaskProbed = m.backupScheduleProbe()
	return status, nil
}

// backupAuditLookback 是查找校验结果时最多回看的审计行数。摘要按文件名
// 匹配，没有专用的数据库索引，回看不足时状态退化为「尚未校验」。
const backupAuditLookback = 500

// latestVerifyState 在审计里查找针对某个文件的最近一次校验记录；立即备
// 份的 backup.create 成功行也算一次成功校验。
func (m *Manager) latestVerifyState(file string) *BackupVerifyState {
	rows, err := m.st.QueryAudit(store.AuditFilter{
		Actions: []string{"backup.verify", "backup.create"},
		Limit:   backupAuditLookback,
	})
	if err != nil {
		return nil
	}
	for _, row := range rows {
		var summary struct {
			File        string `json:"file"`
			Result      string `json:"result"`
			FailureKind string `json:"failure_kind"`
		}
		if json.Unmarshal([]byte(row.Command), &summary) != nil || summary.File != file {
			continue
		}
		state := &BackupVerifyState{
			File: file, OK: summary.Result == "ok",
			FailureKind: summary.FailureKind, At: row.TS,
		}
		if state.OK {
			state.FailureKind = ""
		}
		return state
	}
	return nil
}

// writeAudit 记录一次运维写操作的 owner 审计行：操作成功写 Status=ok，
// 失败写 error；摘要由调用方按白名单构造，不得包含口令与令牌。
func (m *Manager) writeAudit(action string, summary map[string]any, opErr error, src Source) error {
	row := store.OwnerAuditRow(action, summary)
	row.Source = src.kind
	row.SourceIP = src.addr
	if opErr != nil {
		row.Status = "error"
	}
	return m.st.WriteAudit(row)
}

// defaultBackupConfigPath 返回 backup.json 的标准位置：vault 目录下的
// backup.json，计划任务、命令行与 Web UI 共用。
func defaultBackupConfigPath() (string, error) {
	dir, err := paths.VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "backup.json"), nil
}

// defaultBackupScheduleProbe 只读探测 Windows 计划任务是否存在（schtasks
// /Query）；不安装、不修改计划任务。其他平台不探测。
func defaultBackupScheduleProbe() (installed, probed bool) {
	if runtime.GOOS != "windows" {
		return false, false
	}
	return exec.Command("schtasks", "/Query", "/TN", backupTaskName).Run() == nil, true
}
