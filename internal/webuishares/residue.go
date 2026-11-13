package webuishares

import (
	"context"
	"errors"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// residueRunner 把共享存储详情页的残留检查请求接到共享存储管理模块，并
// 在进程内维护「启动任务、轮询任务状态」的任务表。逐台枚举与审计全部
// 复用共享存储管理模块，不另写远端脚本。
type residueRunner struct {
	open  func() (*store.Store, func(), error)
	tasks *shareTaskTable[webui.ShareResidueTaskStatus]
}

// ResidueTasksFromStore 把按请求打开的存储接成共享存储残留检查任务视
// 图。
func ResidueTasksFromStore(
	open func() (*store.Store, func(), error),
) webui.ShareResidueRunner {
	return ResidueTasksFromStoreWithContext(open, context.Background())
}

// ResidueTasksFromStoreWithContext 与 ResidueTasksFromStore 相同，并让任
// 务继承 Web UI 服务的关闭上下文。
func ResidueTasksFromStoreWithContext(
	open func() (*store.Store, func(), error), ctx context.Context,
) webui.ShareResidueRunner {
	return &residueRunner{
		open: open,
		tasks: newShareTaskTable(
			ctx, cloneShareResidueTaskStatus,
			func(task *webui.ShareResidueTaskStatus) bool { return shareTaskFinished(task.State) },
		),
	}
}

func cloneShareResidueTaskStatus(task *webui.ShareResidueTaskStatus) *webui.ShareResidueTaskStatus {
	status := *task
	status.Servers = append([]webui.ShareResidueServerResult(nil), task.Servers...)
	for index := range status.Servers {
		status.Servers[index].Findings = append(
			[]webui.ShareResidueFinding(nil), task.Servers[index].Findings...,
		)
	}
	return &status
}

// StartShareResidue 校验输入后启动后台残留检查任务并立即返回任务标识。
func (r *residueRunner) StartShareResidue(
	request webui.ShareResidueRequest,
) (string, error) {
	if !request.AllShares && request.ShareID == "" {
		return "", errors.New("按共享检查时共享存储标识不能为空")
	}
	if !request.AllShares && request.ShareName == "" {
		return "", errors.New("按共享检查时共享名不能为空")
	}
	id, err := newShareTaskID()
	if err != nil {
		return "", err
	}
	task := &webui.ShareResidueTaskStatus{
		ID: id, State: webui.ShareTaskRunning,
		ShareID: request.ShareID, ShareName: request.ShareName,
		AllShares: request.AllShares,
	}
	r.tasks.register(id, task)
	r.tasks.launch(task, func(task *webui.ShareResidueTaskStatus, ctx context.Context) {
		r.runResidue(task, request, ctx)
	})
	return id, nil
}

// ShareResidueStatus 返回任务状态；任务不存在或已被清理时返回 false。
func (r *residueRunner) ShareResidueStatus(taskID string) (*webui.ShareResidueTaskStatus, bool) {
	return r.tasks.status(taskID)
}

// runResidue 在后台执行一次残留检查。共享存储管理模块负责逐台枚举与审
// 计；这里只把结论换算成页面任务状态。
func (r *residueRunner) runResidue(
	task *webui.ShareResidueTaskStatus,
	request webui.ShareResidueRequest,
	ctx context.Context,
) {
	finish := func(state webui.ShareTaskState, failureMessage string, servers []webui.ShareResidueServerResult) {
		r.tasks.update(task.ID, func(task *webui.ShareResidueTaskStatus) {
			task.State = state
			task.FailureMessage = failureMessage
			task.Servers = servers
			if servers != nil {
				task.Counts = shareResidueCounts(servers)
			}
		})
	}
	filterName := request.ShareName
	if request.AllShares {
		filterName = ""
	}
	var report *sharemanage.ResidueReport
	err := withStore(r.open, func(st *store.Store) error {
		var operationErr error
		report, operationErr = sharemanage.New(st).Residue(ctx, sharemanage.ResidueRequest{
			Share: filterName, Source: sharemanage.WebUISource(request.Source.RemoteAddress),
		})
		return operationErr
	})
	if err != nil {
		finish(webui.ShareTaskError, "残留检查失败，请稍后重试。", nil)
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
	finish(webui.ShareTaskDone, "", servers)
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
