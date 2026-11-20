package webui

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleServerBatchConnectionTest 是服务器列表多选后的批量连接测试接口。
// POST 为每台选中的服务器条目启动一个既有后台任务并立即返回任务标识；
// GET 按任务逐台返回进行中或完成短状态，供页面更新卡片与汇总提示。
func (h *handler) handleServerBatchConnectionTest(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handleServerBatchConnectionTestStart(w, r)
	case http.MethodGet:
		h.handleServerBatchConnectionTestStatus(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		h.writeServerBatchConnectionError(w, http.StatusMethodNotAllowed, "批量测试连接只接受 GET 或 POST 请求。")
	}
}

// handleServerBatchConnectionTestStart 逐台启动既有连接测试任务；重复的
// 服务器条目只启动一次，任何入口校验失败时一台也不启动。
func (h *handler) handleServerBatchConnectionTestStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ServerConnectionTester == nil {
		h.writeServerBatchConnectionError(w, http.StatusBadRequest, "当前部署没有提供连接测试来源。")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.writeServerBatchConnectionError(w, http.StatusBadRequest, "批量测试请求无法读取，请刷新页面后重试。")
		return
	}
	ids := uniqueServerBatchConnectionValues(r.PostForm["server"])
	if len(ids) == 0 {
		h.writeServerBatchConnectionError(w, http.StatusBadRequest, "请先选择要测试的服务器。")
		return
	}
	rows, err := h.cfg.Servers.ServerList(h.requestClientInstance(r), false)
	if err != nil {
		h.logError(r, "批量连接测试读取列表", err)
		h.writeServerBatchConnectionError(w, http.StatusInternalServerError, "读取服务器列表失败，请稍后重试。")
		return
	}
	known := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		known[row.ID] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			h.writeServerBatchConnectionError(w, http.StatusBadRequest, "选中的服务器不存在，请刷新列表后重试。")
			return
		}
	}

	source := ServerWriteSource{RemoteAddress: remoteIP(r)}
	start := ServerConnectionTestBatchStart{Tasks: make([]ServerConnectionTestBatchTask, 0, len(ids))}
	for _, id := range ids {
		detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), id)
		if err != nil {
			h.logError(r, "批量连接测试读取详情", err)
			h.writeServerBatchConnectionError(w, http.StatusInternalServerError, "读取服务器详情失败，请稍后重试。")
			return
		}
		if detail == nil {
			h.writeServerBatchConnectionError(w, http.StatusBadRequest, "选中的服务器不存在，请刷新列表后重试。")
			return
		}
		taskID, err := h.cfg.ServerConnectionTester.StartServerTest(
			detail.ID, strings.TrimSpace(detail.Shell) == "", source, ServerConnectionAnchor{},
		)
		if err != nil || taskID == "" {
			h.logError(r, "批量连接测试启动", err)
			h.writeServerBatchConnectionError(w, http.StatusInternalServerError, "连接测试未启动，请稍后重试。")
			return
		}
		start.Tasks = append(start.Tasks, ServerConnectionTestBatchTask{ServerID: detail.ID, TaskID: taskID})
	}
	h.writeServerBatchConnectionJSON(w, http.StatusOK, start)
}

// handleServerBatchConnectionTestStatus 聚合每台任务的短状态；任务被保留
// 策略回收时要求页面整页读取持久结果，而不是猜测结论。
func (h *handler) handleServerBatchConnectionTestStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ServerConnectionTester == nil {
		h.writeServerBatchConnectionError(w, http.StatusBadRequest, "当前部署没有提供连接测试来源。")
		return
	}
	taskIDs := uniqueServerBatchConnectionValues(r.URL.Query()["task"])
	if len(taskIDs) == 0 {
		h.writeServerBatchConnectionError(w, http.StatusBadRequest, "缺少连接测试任务。")
		return
	}
	status := ServerConnectionTestBatchStatus{
		Total: len(taskIDs), Cards: make([]ServerConnectionTestBatchCard, 0, len(taskIDs)),
	}
	now := h.cfg.now()
	for _, taskID := range taskIDs {
		task, ok := h.cfg.ServerConnectionTester.ServerTestStatus(taskID)
		if !ok || task == nil {
			status.Cards = append(status.Cards, ServerConnectionTestBatchCard{
				TaskID: taskID, State: ServerConnectionTestMissing,
			})
			status.Missing++
			status.Reload = true
			continue
		}
		card := NewServerConnectionTestBatchCard(task, now)
		status.Cards = append(status.Cards, card)
		switch card.State {
		case ServerConnectionTestRunning:
			status.Running++
		case ServerConnectionTestSucceeded:
			status.Succeeded++
		case ServerConnectionTestFailed:
			status.Failed++
		default:
			status.Missing++
			status.Reload = true
		}
	}
	status.Done = status.Running == 0
	h.writeServerBatchConnectionJSON(w, http.StatusOK, status)
}

// uniqueServerBatchConnectionValues 去掉空白项并按首次出现顺序去重，批量
// 启动与轮询共用同一规则。
func uniqueServerBatchConnectionValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var out []string
	for _, value := range values {
		item := strings.TrimSpace(value)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func (h *handler) writeServerBatchConnectionJSON(
	w http.ResponseWriter, status int, value any,
) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *handler) writeServerBatchConnectionError(
	w http.ResponseWriter, status int, message string,
) {
	h.writeServerBatchConnectionJSON(w, status, map[string]string{"error": message})
}
