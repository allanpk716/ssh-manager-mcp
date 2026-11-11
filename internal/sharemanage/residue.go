package sharemanage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
)

// residueConcurrencyLimit 是残留检查逐台并发探测的上限，避免一次检查占
// 满 vault 所在机器与全部目标服务器的连接资源。
const residueConcurrencyLimit = 4

// 残留检查逐台结果的固定分类。
const (
	ResidueStatusResidue    = "residue"
	ResidueStatusMarkerOnly = "marker-only"
	ResidueStatusNoResidue  = "no-residue"
	ResidueStatusUnchecked  = "unchecked"
	ResidueStatusSkipped    = "skipped"
)

// 残留原因的固定分类。
const (
	ResidueReasonShareUnregistered = "share-unregistered"
	ResidueReasonServerOutOfRange  = "server-out-of-range"
)

// ResidueRequest 是一次残留检查请求。Share 为空时检查全部共享；指定共享
// 名时只报告该共享的发现。
type ResidueRequest struct {
	Share string
}

// ResidueFinding 是一台服务器上一个共享的残留结论，不包含共享地址、账号
// 或密码。仅有撤销标记时 RevokedMarkerOnly 为真，这是撤销后的预期状态，
// 不需要清理，因此不带清理命令。
type ResidueFinding struct {
	ShareName         string
	Version           int64
	Reason            string
	HasConfigEntry    bool
	HasCredentialFile bool
	HasMount          bool
	HasRevokedMarker  bool
	RevokedMarkerOnly bool
	CleanupCommand    string
}

// ResidueServerResult 是一台服务器的残留检查结论。
type ResidueServerResult struct {
	ServerID   string
	ServerName string
	System     string
	Status     string
	Reason     string
	Notice     string
	Findings   []ResidueFinding
}

// ResidueReport 是一次残留检查的完整逐台结果。
type ResidueReport struct {
	Servers []ResidueServerResult
}

// Residue 逐台只读检测 vault 里的 Linux 与 macOS 服务器，找出不在可挂载
// 范围内却仍留有凭据文件、挂载配置或挂载的共享。连不上的服务器列为未检
// 查；Windows 服务器跳过，因为 sshmgr 不在其上留下任何东西。
func (m *Manager) Residue(ctx context.Context, request ResidueRequest) (*ResidueReport, error) {
	if m == nil || m.store == nil {
		return nil, errManagerWithoutStore
	}
	servers, err := m.store.ListServers()
	if err != nil {
		return nil, err
	}
	shares, err := m.store.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return nil, err
	}
	sharesByName := make(map[string]*models.SharedStorage, len(shares))
	for _, share := range shares {
		sharesByName[strings.ToLower(share.Name)] = share
	}
	filterKey := strings.ToLower(strings.TrimSpace(request.Share))

	results := make([]ResidueServerResult, len(servers))
	limits := make(chan struct{}, residueConcurrencyLimit)
	var wait sync.WaitGroup
	for index, server := range servers {
		if models.NormalizeShellType(string(server.ShellType)).IsWindows() {
			results[index] = ResidueServerResult{
				ServerID: server.ID, ServerName: server.Name,
				System: sharemount.CheckSystemWindows, Status: ResidueStatusSkipped,
				Notice: "跳过 Windows 服务器：sshmgr 不在其上留下任何东西，无需残留检查",
			}
			continue
		}
		wait.Add(1)
		limits <- struct{}{}
		go func(index int, server *models.Server) {
			defer func() { <-limits }()
			defer wait.Done()
			results[index] = m.checkServerResidue(ctx, server, sharesByName, filterKey)
		}(index, server)
	}
	wait.Wait()
	sort.Slice(results, func(i, j int) bool {
		return results[i].ServerName < results[j].ServerName
	})
	report := &ResidueReport{Servers: results}
	if err := m.writeResidueAudit(strings.TrimSpace(request.Share), results); err != nil {
		return nil, err
	}
	return report, nil
}

func (m *Manager) checkServerResidue(
	ctx context.Context, server *models.Server,
	sharesByName map[string]*models.SharedStorage, filterKey string,
) ResidueServerResult {
	result := ResidueServerResult{ServerID: server.ID, ServerName: server.Name}
	enumerated, err := m.mounter.Enumerate(ctx, server)
	if err != nil {
		result.Status = ResidueStatusUnchecked
		result.Reason = residueErrorReason(err)
		result.Notice = residueErrorNotice(err)
		return result
	}
	result.System = enumerated.System
	for _, state := range enumerated.States {
		finding := residueFinding(state, server, sharesByName)
		if finding == nil {
			continue
		}
		if filterKey != "" && strings.ToLower(finding.ShareName) != filterKey {
			continue
		}
		result.Findings = append(result.Findings, *finding)
	}
	result.Status = ResidueStatusNoResidue
	for _, finding := range result.Findings {
		if !finding.RevokedMarkerOnly {
			result.Status = ResidueStatusResidue
			break
		}
	}
	if result.Status == ResidueStatusNoResidue && len(result.Findings) > 0 {
		result.Status = ResidueStatusMarkerOnly
	}
	return result
}

func residueFinding(
	state sharemount.EnumerateShareState, server *models.Server,
	sharesByName map[string]*models.SharedStorage,
) *ResidueFinding {
	if !state.HasConfigEntry && !state.HasCredential && !state.HasMount {
		return &ResidueFinding{
			ShareName: state.Share, Version: state.Version,
			HasRevokedMarker: true, RevokedMarkerOnly: true,
		}
	}
	share := sharesByName[strings.ToLower(state.Share)]
	if share != nil && sharedStorageHasMountableServer(share, server.ID) {
		return nil
	}
	finding := &ResidueFinding{
		ShareName: state.Share, Version: state.Version,
		HasConfigEntry: state.HasConfigEntry, HasCredentialFile: state.HasCredential,
		HasMount: state.HasMount, HasRevokedMarker: state.HasRevokedMarker,
		CleanupCommand: fmt.Sprintf("shares unmount %s %s --yes", state.Share, server.Name),
	}
	if share == nil {
		finding.Reason = ResidueReasonShareUnregistered
	} else {
		finding.Reason = ResidueReasonServerOutOfRange
	}
	return finding
}

func residueErrorReason(err error) string {
	switch {
	case errors.Is(err, sharemount.ErrNoSudo):
		return "missing-sudo"
	case errors.Is(err, sharemount.ErrUnsupportedSystem):
		return "unsupported-system"
	case errors.Is(err, sharemount.ErrEnumerateOutputInvalid):
		return "output-invalid"
	default:
		return "ssh-error"
	}
}

func residueErrorNotice(err error) string {
	switch {
	case errors.Is(err, sharemount.ErrNoSudo):
		return "服务器条目没有配置 sudo 凭据，无法读取持久配置"
	case errors.Is(err, sharemount.ErrUnsupportedSystem):
		return "目标服务器操作系统不支持共享存储枚举"
	case errors.Is(err, sharemount.ErrEnumerateOutputInvalid):
		return "枚举命令输出不合法，无法给出可靠结论"
	default:
		return "连接服务器或执行枚举命令失败"
	}
}

func (m *Manager) writeResidueAudit(filterName string, results []ResidueServerResult) error {
	summary := map[string]any{}
	if filterName != "" {
		summary["share"] = filterName
	}
	servers := make([]map[string]any, 0, len(results))
	for _, result := range results {
		item := map[string]any{"server": result.ServerName, "result": result.Status}
		if len(result.Findings) > 0 {
			shares := make([]map[string]any, 0, len(result.Findings))
			for _, finding := range result.Findings {
				shares = append(shares, map[string]any{
					"share": finding.ShareName, "version": finding.Version,
				})
			}
			item["shares"] = shares
		}
		if result.Reason != "" {
			item["reason"] = result.Reason
		}
		servers = append(servers, item)
	}
	summary["servers"] = servers
	return m.store.WriteOwnerAudit("share-residue-check", summary)
}
