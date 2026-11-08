package servermanage

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"ssh-manager-mcp/internal/importer"
	"ssh-manager-mcp/internal/models"
)

// 候选凭据注释代号：Web UI 与命令行换成人能读懂的说明。
const (
	ImportNoteKey             = "key"
	ImportNoteNoCredential    = "no-credential"
	ImportNoteNeedsPassphrase = "needs-passphrase"
)

// ImportCandidate 是导入计划里的一台可导入候选。
type ImportCandidate struct {
	Name string
	Host string
	Port int
	User string
	// Note 是凭据注释代号：key、no-credential、needs-passphrase。
	Note string
}

// ImportSkip 是计划阶段跳过的一条记录：名字（或别名）与原因代号。
type ImportSkip struct {
	Name   string
	Reason string
}

// ImportPlan 是一次 SSH 配置导入计划：可导入候选、两类跳过与可选 profile。
type ImportPlan struct {
	Path         string
	MatchWarning bool
	Candidates   []ImportCandidate
	VaultSkipped []ImportSkip
	ParseSkipped []ImportSkip
	ProfileID    string
	ProfileName  string
}

// ImportEntryStatus 是导入执行结果里的一台机器状态。
type ImportEntryStatus struct {
	Name   string
	Status string
	Error  string
}

// ImportResult 是一次导入执行的结构化结果：四类计数、逐台状态与导入成功的
// 服务器标识符（补全队列直接使用）。
type ImportResult struct {
	Imported          int
	Skipped           int
	Failed            int
	Pending           int
	Entries           []ImportEntryStatus
	ImportedServerIDs []string
}

// PlanImport 解析 SSH 配置并与 vault 现有服务器做冲突规划。profile 非空时
// 先解析并校验存在，失败则整个计划失败且不读取任何私钥文件。
func (m *Manager) PlanImport(configPath, profile string) (*ImportPlan, error) {
	plan := &ImportPlan{Path: configPath}
	if profile != "" {
		profID, err := m.profileIDByName(profile)
		if err != nil {
			return nil, err
		}
		plan.ProfileID, plan.ProfileName = profID, profile
	}
	candidates, err := m.planCandidates(configPath, plan)
	if err != nil {
		return nil, err
	}
	configDir := filepath.Dir(configPath)
	seen := map[[32]byte]string{}
	for _, cand := range candidates {
		pick := importer.PickKey(cand, configDir, seen)
		note := ImportNoteNoCredential
		if pick.Cred != nil {
			note = ImportNoteKey
			if pick.NeedsPass {
				note = ImportNoteNeedsPassphrase
			}
		}
		plan.Candidates = append(plan.Candidates, ImportCandidate{
			Name: cand.Name, Host: cand.Host, Port: cand.Port, User: cand.User, Note: note,
		})
	}
	return plan, nil
}

// planCandidates 解析配置并把 vault 冲突写进 plan（两类跳过），返回仍可导入
// 的候选，顺序保持配置顺序。
func (m *Manager) planCandidates(configPath string, plan *ImportPlan) ([]importer.Candidate, error) {
	parsed, err := importer.Parse(configPath, currentUserName())
	if err != nil {
		return nil, err
	}
	if plan != nil {
		plan.MatchWarning = parsed.MatchWarning
		for _, skipped := range parsed.Skipped {
			plan.ParseSkipped = append(plan.ParseSkipped, ImportSkip{Name: skipped.Alias, Reason: skipped.Reason})
		}
	}
	existing, err := m.st.ListServers()
	if err != nil {
		return nil, err
	}
	ex := make([]importer.ExistingServer, len(existing))
	for i, srv := range existing {
		ex[i] = importer.ExistingServer{Name: srv.Name, Host: srv.Host, Port: srv.Port, User: srv.User}
	}
	candidates, vaultSkips := importer.PlanImport(parsed.Candidates, ex)
	if plan != nil {
		for _, skipped := range vaultSkips {
			plan.VaultSkipped = append(plan.VaultSkipped, ImportSkip{Name: skipped.Alias, Reason: skipped.Reason})
		}
	}
	return candidates, nil
}

// RunImport 重新解析并执行导入：selected 是要导入的候选名字集合；不在计划
// 里或产生冲突的名字按跳过计数。同批相同私钥只建一份凭据，加密私钥原样导
// 入并写待补口令标签。profile 非空时在导入成功后授权这批服务器。
func (m *Manager) RunImport(configPath string, selected []string, profile string, _ Source) (*ImportResult, error) {
	plan := &ImportPlan{Path: configPath}
	if profile != "" {
		profID, err := m.profileIDByName(profile)
		if err != nil {
			return nil, err
		}
		plan.ProfileID, plan.ProfileName = profID, profile
	}
	candidates, err := m.planCandidates(configPath, plan)
	if err != nil {
		return nil, err
	}
	result := &ImportResult{}
	selectedSet := make(map[string]bool, len(selected))
	for _, name := range selected {
		selectedSet[name] = true
	}
	status := func(name, text string, failure error) {
		entry := ImportEntryStatus{Name: name, Status: text}
		if failure != nil {
			entry.Error = failure.Error()
		}
		result.Entries = append(result.Entries, entry)
	}
	for _, skipped := range plan.VaultSkipped {
		status(skipped.Name, "skipped:"+skipped.Reason, nil)
		result.Skipped++
	}
	for _, skipped := range plan.ParseSkipped {
		status(skipped.Name, "skipped:"+skipped.Reason, nil)
		result.Skipped++
	}
	configDir := filepath.Dir(configPath)
	keyIDs := map[[32]byte]string{}
	for _, cand := range candidates {
		if !selectedSet[cand.Name] {
			status(cand.Name, "skipped:unselected", nil)
			result.Skipped++
			continue
		}
		pick := importer.PickKey(cand, configDir, keyIDs)
		srv := &models.Server{Name: cand.Name, Host: cand.Host, Port: cand.Port, User: cand.User}
		if pick.NeedsPass {
			srv.Tags = []string{models.TagNeedsPassphrase}
		}
		id, err := m.insertServerRow(srv, pick.Cred, nil)
		if err != nil {
			status(cand.Name, "failed", err)
			result.Failed++
			continue
		}
		if pick.Minted && pick.Cred != nil && pick.Cred.ID != "" {
			keyIDs[pick.Sum] = pick.Cred.ID
		}
		result.Imported++
		result.ImportedServerIDs = append(result.ImportedServerIDs, id)
		status(cand.Name, "imported", nil)
	}
	if plan.ProfileID != "" && len(result.ImportedServerIDs) > 0 {
		if err := m.st.GrantServers(plan.ProfileID, result.ImportedServerIDs); err != nil {
			return result, fmt.Errorf("导入成功但授权 profile 失败：%w", err)
		}
	}
	result.Pending = m.pendingCount(result.ImportedServerIDs)
	return result, nil
}

// pendingCount 统计给定服务器里当前仍需要注意的数量；导入结果与补全结果页
// 共用，数字始终按最新数据计算。
func (m *Manager) pendingCount(ids []string) int {
	count := 0
	for _, id := range ids {
		srv, err := m.st.GetServer(id)
		if err != nil || srv == nil {
			continue
		}
		if len(models.ServerAttentionReasons(srv.CredentialID, srv.Role, srv.Tags)) > 0 {
			count++
		}
	}
	return count
}

// AttentionPendingIDs 返回给定服务器里仍需要注意的标识符，供补全队列推进
// 与结果页计算。
func (m *Manager) AttentionPendingIDs(ids []string) []string {
	var pending []string
	for _, id := range ids {
		srv, err := m.st.GetServer(id)
		if err != nil || srv == nil {
			continue
		}
		if len(models.ServerAttentionReasons(srv.CredentialID, srv.Role, srv.Tags)) > 0 {
			pending = append(pending, id)
		}
	}
	return pending
}

func (m *Manager) profileIDByName(name string) (string, error) {
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return "", err
	}
	for _, profile := range profiles {
		if profile.Name == name {
			return profile.ID, nil
		}
	}
	return "", fmt.Errorf("profile %q not found（可先创建 profile 再导入）", name)
}

// currentUserName 是 SSH 配置里 User 省略时的兜底用户名：先取操作系统当前
// 用户，再退回环境变量。
func currentUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	return os.Getenv("USER")
}
