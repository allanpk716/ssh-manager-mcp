package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// serverBatchConnectionTaskQuery 保存一个连接测试任务与其服务器条目的对应
// 关系，供任务被回收后读取持久结果。
type serverBatchConnectionTaskQuery struct {
	TaskID   string
	ServerID string
}

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
	ids := dedupeServerBatchSelection(r.PostForm["server"])
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
	details := make([]*ServerDetailView, 0, len(ids))
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
		details = append(details, detail)
	}

	source := ServerWriteSource{RemoteAddress: remoteIP(r)}
	start := ServerConnectionTestBatchStart{Tasks: make([]ServerConnectionTestBatchTask, 0, len(ids))}
	for _, detail := range details {
		taskID, err := h.cfg.ServerConnectionTester.StartServerTest(
			detail.ID, strings.TrimSpace(detail.Shell) == "", source, ServerConnectionAnchor{},
		)
		if err != nil || taskID == "" {
			h.logError(r, "批量连接测试启动", err)
			start.Tasks = append(start.Tasks, ServerConnectionTestBatchTask{
				ServerID: detail.ID, Error: "未能启动连接测试，请稍后重试。",
			})
			continue
		}
		start.Tasks = append(start.Tasks, ServerConnectionTestBatchTask{ServerID: detail.ID, TaskID: taskID})
	}
	h.writeServerBatchConnectionJSON(w, http.StatusOK, start)
}

// handleServerBatchConnectionTestStatus 聚合每台任务的短状态；任务被保留
// 策略回收时，按随任务传入的服务器条目读取最近一次持久结果。
func (h *handler) handleServerBatchConnectionTestStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ServerConnectionTester == nil {
		h.writeServerBatchConnectionError(w, http.StatusBadRequest, "当前部署没有提供连接测试来源。")
		return
	}
	queries := serverBatchConnectionTaskQueries(r.URL.Query())
	if len(queries) == 0 {
		h.writeServerBatchConnectionError(w, http.StatusBadRequest, "缺少连接测试任务。")
		return
	}
	status := ServerConnectionTestBatchStatus{
		Total: len(queries), Cards: make([]ServerConnectionTestBatchCard, 0, len(queries)),
	}
	now := h.cfg.now()
	for _, item := range queries {
		var card ServerConnectionTestBatchCard
		task, ok := h.cfg.ServerConnectionTester.ServerTestStatus(item.TaskID)
		if !ok || task == nil {
			card = h.persistedServerBatchConnectionCard(item.TaskID, item.ServerID, now, r)
		} else {
			card = NewServerConnectionTestBatchCard(task, now)
		}
		status.Cards = append(status.Cards, card)
		switch card.State {
		case ServerConnectionTestRunning:
			status.Running++
		case ServerConnectionTestSucceeded:
			status.Succeeded++
		case ServerConnectionTestFailed:
			status.Failed++
		}
	}
	status.Done = status.Running == 0
	h.writeServerBatchConnectionJSON(w, http.StatusOK, status)
}

func serverBatchConnectionTaskQueries(values url.Values) []serverBatchConnectionTaskQuery {
	taskValues := values["task"]
	serverValues := values["server"]
	if len(taskValues) == 0 || len(taskValues) != len(serverValues) {
		return nil
	}
	queries := make([]serverBatchConnectionTaskQuery, 0, len(taskValues))
	for index, taskValue := range taskValues {
		taskID := strings.TrimSpace(taskValue)
		serverID := strings.TrimSpace(serverValues[index])
		if taskID == "" || serverID == "" {
			return nil
		}
		queries = append(queries, serverBatchConnectionTaskQuery{TaskID: taskID, ServerID: serverID})
	}
	return queries
}

// persistedServerBatchConnectionCard 把已被回收的任务换回服务器条目最近一
// 次持久结果；从未测试时沿用列表卡片的既有短状态。
func (h *handler) persistedServerBatchConnectionCard(
	taskID, serverID string, now time.Time, r *http.Request,
) ServerConnectionTestBatchCard {
	detail, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), serverID)
	if err != nil {
		h.logError(r, "批量连接测试读取最近结果", err)
		return ServerConnectionTestBatchCard{
			ServerID: serverID, TaskID: taskID, State: ServerConnectionTestFailed,
			Title: "读取最近连接测试结果失败",
		}
	}
	if detail == nil || detail.ConnectionTest == nil {
		return ServerConnectionTestBatchCard{
			ServerID: serverID, TaskID: taskID, State: ServerConnectionTestFailed,
			Title: "从未测试", Untested: true,
		}
	}
	return NewServerConnectionTestBatchCard(&ServerConnectionTestStatus{
		ID: taskID, ServerID: serverID, Result: detail.ConnectionTest,
	}, now)
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
