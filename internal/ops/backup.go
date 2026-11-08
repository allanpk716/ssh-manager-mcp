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
	"strconv"
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

// CheckBackupTargetDir 执行备份目录的两道写前检查：标记文件必须存在（判
// 断挂载是否就绪），目录里不能有 .git 工作树。
func CheckBackupTargetDir(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, backupMarkerName)); err != nil {
		return fmt.Errorf("marker file %s not found in --dir %s — refusing to write (is the NAS mounted? create the marker ON the mounted share after mounting)",
			backupMarkerName, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return fmt.Errorf("--dir %s contains a .git directory — refusing to write credentials into a git working tree", dir)
	}
	return nil
}

// NextEncryptedBackupName 按 vault-<UTC 时间>.sme 选名，同秒冲突时追加
// -2/-3。
func NextEncryptedBackupName(dir, prefix string, now time.Time) (string, error) {
	base := prefix + "-" + now.UTC().Format("20060102-150405")
	name := base + ".sme"
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name, nil
		} else if err != nil {
			return "", err
		}
		name = fmt.Sprintf("%s-%d.sme", base, n)
		if n > 99 {
			return "", fmt.Errorf("too many same-second collisions for %s", base)
		}
	}
}

// checkEncryptedGeneration 写入前试解目录里最新的加密备份：当前口令必须
// 能解开它，否则拒绝写入，避免备份谱系在口令变化或文件损坏后悄悄分叉。
func checkEncryptedGeneration(dir, prefix string, passphrase []byte) error {
	files := ListEncryptedBackups(dir, prefix)
	if len(files) == 0 {
		return nil
	}
	latest := files[0]
	blob, err := os.ReadFile(latest.Path)
	if err != nil {
		return fmt.Errorf("generation check: reading the latest encrypted backup %s: %w", latest.Name, err)
	}
	if _, err := vaultio.Decrypt(passphrase, blob); err != nil {
		return fmt.Errorf("generation check failed: cannot decrypt the latest encrypted backup %s with this passphrase (the passphrase changed, or the file is corrupted) — refusing to write a new encrypted backup", latest.Name)
	}
	return nil
}

// rotateEncryptedBackups 按文件名保留最新的 keep 份加密备份，其余删除；
// 同目录的明文备份不受影响。
func rotateEncryptedBackups(dir, prefix string, keep int) error {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.sme"))
	if err != nil {
		return err
	}
	if len(matches) <= keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for _, path := range matches[keep:] {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			continue
		}
	}
	return nil
}

// EncryptedBackupRequest 描述一次加密备份的写入参数。SnapshotJSON 由调用
// 方提供，命令行与运维模块各自从已解锁的 vault 取快照。
type EncryptedBackupRequest struct {
	Dir            string
	Prefix         string
	Keep           int
	PassphraseFile string
	SnapshotJSON   func() ([]byte, error)
	Now            func() time.Time
}

// EncryptedBackupResult 是加密备份引擎的返回值。RotationErr 非空表示备份
// 文件已写入并通过回读校验，只是清理旧备份失败，调用方按警告处理。
type EncryptedBackupResult struct {
	Name        string
	RotationErr error
}

// CreateEncryptedBackup 执行与 `backup create` 相同的加密备份链：读口令、
// 加锁、口令试解、取快照、加密、原子写入、回读校验、按份数轮转。
func CreateEncryptedBackup(request EncryptedBackupRequest) (EncryptedBackupResult, error) {
	passphrase, err := ReadPassphraseFile(request.PassphraseFile)
	if err != nil {
		return EncryptedBackupResult{}, err
	}
	if len(passphrase) == 0 {
		return EncryptedBackupResult{}, fmt.Errorf("passphrase must not be empty")
	}
	lock, err := AcquireBackupLock(request.Dir)
	if err != nil {
		return EncryptedBackupResult{}, err
	}
	defer lock.Release()

	if err := checkEncryptedGeneration(request.Dir, request.Prefix, passphrase); err != nil {
		return EncryptedBackupResult{}, err
	}
	data, err := request.SnapshotJSON()
	if err != nil {
		return EncryptedBackupResult{}, err
	}
	blob, err := vaultio.Encrypt(passphrase, data)
	if err != nil {
		return EncryptedBackupResult{}, err
	}
	now := request.Now
	if now == nil {
		now = time.Now
	}
	name, err := NextEncryptedBackupName(request.Dir, request.Prefix, now())
	if err != nil {
		return EncryptedBackupResult{}, err
	}
	finalPath, err := AtomicWriteFile(request.Dir, name, blob)
	if err != nil {
		return EncryptedBackupResult{}, err
	}
	if err := verifyEncryptedWritten(finalPath, passphrase); err != nil {
		return EncryptedBackupResult{}, fmt.Errorf("post-write verification failed for %s: %w", finalPath, err)
	}
	result := EncryptedBackupResult{Name: filepath.Base(finalPath)}
	if request.Keep > 0 {
		if err := rotateEncryptedBackups(request.Dir, request.Prefix, request.Keep); err != nil {
			result.RotationErr = fmt.Errorf("rotation error (backups left in place): %w", err)
		}
	}
	return result, nil
}

// verifyEncryptedWritten 回读刚写入的备份，用同一口令解密并重新解析为
// vault 快照。
func verifyEncryptedWritten(path string, passphrase []byte) error {
	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	plain, err := vaultio.Decrypt(passphrase, blob)
	if err != nil {
		return fmt.Errorf("decrypt failed: %w", err)
	}
	if err := json.Unmarshal(plain, &store.Snapshot{}); err != nil {
		return fmt.Errorf("re-unmarshal failed: %w", err)
	}
	return nil
}

// BackupCreateResult 是立即备份的返回值；Skipped 表示已有备份正在进行，
// 本次没有写入。
type BackupCreateResult struct {
	File    string
	Skipped bool
}

// CreateBackup 按 backup.json 的参数立即执行一次加密备份，结果写入
// backup.create 审计行。备份写入时已回读校验，成功行同时算作一次成功
// 校验。
func (m *Manager) CreateBackup(source Source) (BackupCreateResult, error) {
	cfg, err := m.loadBackupConfig()
	if err != nil {
		return BackupCreateResult{}, err
	}
	if err := CheckBackupTargetDir(cfg.Dir); err != nil {
		return BackupCreateResult{}, err
	}
	outcome, err := CreateEncryptedBackup(EncryptedBackupRequest{
		Dir: cfg.Dir, Prefix: backupPrefix, Keep: cfg.Keep,
		PassphraseFile: cfg.PassphraseFile, SnapshotJSON: m.snapshotJSON, Now: m.now,
	})
	if errors.Is(err, ErrConcurrentBackup) {
		_ = m.RecordBackupCreate(source, "", "skipped", nil)
		return BackupCreateResult{Skipped: true}, nil
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	auditErr := m.RecordBackupCreate(source, outcome.Name, result, err)
	if err != nil {
		return BackupCreateResult{}, err
	}
	return BackupCreateResult{File: outcome.Name}, auditErr
}

// snapshotJSON 从当前已解锁的 vault 导出快照，缩进与 export 命令一致。
func (m *Manager) snapshotJSON() ([]byte, error) {
	snapshot, err := m.st.ExportSnapshot()
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(snapshot, "", "  ")
}

// RecordBackupCreate 记录一次 backup.create 审计行；立即备份与命令行
// backup create 共用，摘要只含文件名与结果。
func (m *Manager) RecordBackupCreate(source Source, file, result string, opErr error) error {
	summary := map[string]any{"result": result}
	if file != "" {
		summary["file"] = file
	}
	return m.writeAudit("backup.create", summary, opErr, source)
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

// staleLockDuration 是备份锁被视作遗留的最小时长。备份通常数秒完成；超
// 过 5 分钟仍持有锁说明存储已挂起，后来者可以回收锁。
const staleLockDuration = 300 * time.Second

const backupLockName = ".ssh-manager-backup.lock"
const backupMarkerName = ".ssh-manager-backup-marker"

// ErrConcurrentBackup 表示已有一次备份正在进行。调用方按「跳过」处理，
// 不视为错误。
var ErrConcurrentBackup = errors.New("another backup in progress")

// BackupLock 是防止并发备份的建议锁文件。独占创建不会在进程退出时自动
// 释放，崩溃后由后来者按时间回收。
type BackupLock struct {
	path string
}

// AcquireBackupLock 创建独占锁文件并写入开始时间。锁已存在时读取其中
// 的时间：超过回收时长则接管，否则返回 ErrConcurrentBackup。
func AcquireBackupLock(dir string) (*BackupLock, error) {
	path := filepath.Join(dir, backupLockName)
	if lock, err := tryCreateBackupLock(path); err == nil {
		return lock, nil
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	stale, err := backupLockIsStale(path)
	if err != nil {
		// 锁内容不可读或已被人抢先删掉：再试一次创建，仍失败按并发处理。
		if lock, retryErr := tryCreateBackupLock(path); retryErr == nil {
			return lock, nil
		}
		return nil, ErrConcurrentBackup
	}
	if !stale {
		return nil, ErrConcurrentBackup
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reclaim stale lock %s: %w", path, err)
	}
	if lock, err := tryCreateBackupLock(path); err == nil {
		return lock, nil
	}
	return nil, ErrConcurrentBackup
}

func tryCreateBackupLock(path string) (*BackupLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.WriteString(strconv.FormatInt(time.Now().Unix(), 10) + "\n"); err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return &BackupLock{path: path}, nil
}

func backupLockIsStale(path string) (bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	start, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return false, err
	}
	return time.Since(time.Unix(start, 0)) > staleLockDuration, nil
}

// Release 删除锁文件；已不存在时忽略。
func (l *BackupLock) Release() {
	if l == nil || l.path == "" {
		return
	}
	os.Remove(l.path)
}

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
