package webuishares

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// residueTaskRetention 是已完成残留检查任务的状态保留数量；页面轮询只需
// 要最近的任务，进程重启后任务表自然清空。
const residueTaskRetention = 32

// residueRunner 把共享存储详情页的残留检查请求接到共享存储管理模块，并
// 在进程内维护「启动任务、轮询任务状态」的任务表。逐台枚举与审计全部
// 复用共享存储管理模块，不另写远端脚本。
type residueRunner struct {
	open  func() (*store.Store, func(), error)
	mu    sync.Mutex
	tasks map[string]*webui.ShareResidueTaskStatus
}

// ResidueTasksFromStore 把按请求打开的存储接成共享存储残留检查任务视
// 图。
func ResidueTasksFromStore(
	open func() (*store.Store, func(), error),
) webui.ShareResidueRunner {
	return &residueRunner{open: open, tasks: map[string]*webui.ShareResidueTaskStatus{}}
}

// StartShareResidue 校验输入后启动后台残留检查任务并立即返回任务标识。
func (r *residueRunner) StartShareResidue(
	request webui.ShareResidueRequest, source webui.ShareWriteSource,
) (string, error) {
	if request.ShareID == "" {
		return "", errors.New("共享存储标识不能为空")
	}
	if !request.AllShares && request.ShareName == "" {
		return "", errors.New("按共享检查时共享名不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, err := randomResidueTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ShareResidueTaskStatus{
		ID: id, State: "running",
		ShareID: request.ShareID, ShareName: request.ShareName,
		AllShares: request.AllShares,
	}
	r.tasks[id] = task
	go r.runResidue(task, request, source)
	return id, nil
}

// ShareResidueStatus 返回任务状态；任务不存在或已被清理时返回 false。
func (r *residueRunner) ShareResidueStatus(taskID string) (*webui.ShareResidueTaskStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	task, ok := r.tasks[taskID]
	if !ok {
		return nil, false
	}
	status := *task
	status.Servers = append([]webui.ShareResidueServerResult(nil), task.Servers...)
	for index := range status.Servers {
		status.Servers[index].Findings = append(
			[]webui.ShareResidueFinding(nil), task.Servers[index].Findings...,
		)
	}
	return &status, true
}

// runResidue 在后台执行一次残留检查。共享存储管理模块负责逐台枚举与审
// 计；这里只把结论换算成页面任务状态。
func (r *residueRunner) runResidue(
	task *webui.ShareResidueTaskStatus,
	request webui.ShareResidueRequest,
	source webui.ShareWriteSource,
) {
	finish := func(state, failureMessage string, servers []webui.ShareResidueServerResult) {
		r.mu.Lock()
		task.State = state
		task.FailureMessage = failureMessage
		task.Servers = servers
		if servers != nil {
			task.Counts = shareResidueCounts(servers)
		}
		r.pruneTasksLocked()
		r.mu.Unlock()
	}
	filterName := request.ShareName
	if request.AllShares {
		filterName = ""
	}
	var report *sharemanage.ResidueReport
	err := withStore(r.open, func(st *store.Store) error {
		var operationErr error
		report, operationErr = sharemanage.New(st).Residue(context.Background(), sharemanage.ResidueRequest{
			Share: filterName, Source: sharemanage.WebUISource(source.RemoteAddress),
		})
		return operationErr
	})
	if err != nil {
		finish("error", "残留检查失败，请稍后重试。", nil)
		return
	}
	servers := make([]webui.ShareResidueServerResult, 0, len(report.Servers))
	for _, result := range report.Servers {
		row := webui.ShareResidueServerResult{
			ServerID: result.ServerID, ServerName: result.ServerName,
			System: result.System, Status: result.Status,
			Reason: result.Reason, Notice: result.Notice,
		}
		for _, finding := range result.Findings {
			row.Findings = append(row.Findings, webui.ShareResidueFinding{
				ShareName: finding.ShareName, Version: finding.Version,
				Reason:         finding.Reason,
				HasConfigEntry: finding.HasConfigEntry, HasCredentialFile: finding.HasCredentialFile,
				HasMount: finding.HasMount, HasRevokedMarker: finding.HasRevokedMarker,
				RevokedMarkerOnly: finding.RevokedMarkerOnly,
				HasCleanup:        finding.CleanupCommand != "",
			})
		}
		servers = append(servers, row)
	}
	finish("done", "", servers)
}

// shareResidueCounts 统计五类逐台结论的台数，与页面汇总文案一一对应。
func shareResidueCounts(servers []webui.ShareResidueServerResult) webui.ShareResidueCounts {
	var counts webui.ShareResidueCounts
	for _, server := range servers {
		switch server.Status {
		case sharemanage.ResidueStatusResidue:
			counts.Residue++
		case sharemanage.ResidueStatusMarkerOnly:
			counts.MarkerOnly++
		case sharemanage.ResidueStatusUnchecked:
			counts.Unchecked++
		case sharemanage.ResidueStatusNoResidue:
			counts.NoResidue++
		case sharemanage.ResidueStatusSkipped:
			counts.Skipped++
		}
	}
	return counts
}

func (r *residueRunner) pruneTasksLocked() {
	if len(r.tasks) <= residueTaskRetention {
		return
	}
	for id, task := range r.tasks {
		if task.State != "running" {
			delete(r.tasks, id)
			if len(r.tasks) <= residueTaskRetention {
				return
			}
		}
	}
}

func randomResidueTaskID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成残留检查任务标识失败：%w", err)
	}
	return hex.EncodeToString(buffer), nil
}
