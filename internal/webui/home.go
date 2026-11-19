package webui

import (
	"net/http"
	"strconv"
	"strings"
)

// HomeTask 是主页「待你处理」里的一张卡片。
type HomeTask struct {
	Title    string
	Detail   string
	Href     string
	Priority bool
}

// HomeRecentChange 是主页最近变化时间线中的一行。
type HomeRecentChange struct {
	TimeText string
	Summary  string
}

// homeView 是主页状态总览的展示数据。
type homeView struct {
	RoleLabel     string
	Tasks         []HomeTask
	ServerCount   int
	AttentionText string
	// ClientsKnown 表示本机角色能否统计客户端实例；客户端角色看不到
	// 权威端的设备码，不能显示误导性的零。
	ClientsKnown  bool
	ClientCount   int
	ProfileCount  int
	RecentChanges []HomeRecentChange
}

// handleHomePage 渲染权威端与回环模式共用的主页状态总览。
func (h *handler) handleHomePage(w http.ResponseWriter, r *http.Request) {
	view, err := h.buildHomeView(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取主页数据失败", "读取服务器、授权或审计数据失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "home.tmpl", pageData{Title: "状态总览", Home: view})
}

func (h *handler) buildHomeView(r *http.Request) (*homeView, error) {
	view := &homeView{RoleLabel: h.caps.RoleLabel}
	view.ClientsKnown = h.caps.HasPairingAndDeviceCodes
	servers, err := h.cfg.Servers.ServerList(h.requestClientInstance(r), false)
	if err != nil {
		return nil, err
	}
	view.ServerCount = len(servers)
	attentionCount := 0
	attentionNames := make([]string, 0, 2)
	for _, server := range servers {
		if len(server.AttentionReasons) == 0 {
			continue
		}
		attentionCount++
		if len(attentionNames) < 2 {
			attentionNames = append(attentionNames, server.Name)
		}
	}
	if attentionCount > 0 {
		view.AttentionText = homeCountText(attentionCount, "台服务器需要注意")
		view.Tasks = append(view.Tasks, HomeTask{
			Title:  view.AttentionText,
			Detail: strings.Join(attentionNames, "、") + " 需要补齐信息",
			Href:   "/ui/servers/?attention=1",
		})
	}

	if h.cfg.Role == RoleServer && h.cfg.Access != nil {
		pairings, err := h.cfg.Access.ListPairingRequests()
		if err != nil {
			return nil, err
		}
		pendingNames := make([]string, 0, len(pairings))
		for _, pairing := range pairings {
			if pairing.State == "pending" {
				pendingNames = append(pendingNames, pairing.Name)
			}
		}
		if len(pendingNames) > 0 {
			view.Tasks = append([]HomeTask{{
				Title:    homeCountText(len(pendingNames), "个配对请求待审批"),
				Detail:   pendingNames[0] + " 请求接入，等待你确认",
				Href:     "/ui/clients/pairing",
				Priority: true,
			}}, view.Tasks...)
		}
		clients, err := h.cfg.Access.ListClients()
		if err != nil {
			return nil, err
		}
		for _, client := range clients {
			if !client.Revoked {
				view.ClientCount++
			}
		}
	}
	if h.cfg.Access != nil && h.caps.CanManageProfiles {
		profiles, err := h.cfg.Access.ListProfiles()
		if err != nil {
			return nil, err
		}
		view.ProfileCount = len(profiles)
	}
	if missingSecrets, missingShares := h.missingSecretNoteCount(), h.missingShareNoteCount(); missingSecrets > 0 || missingShares > 0 {
		details := make([]string, 0, 2)
		if missingSecrets > 0 {
			details = append(details, homeCountText(missingSecrets, "个机密项"))
		}
		if missingShares > 0 {
			details = append(details, homeCountText(missingShares, "个共享存储"))
		}
		view.Tasks = append(view.Tasks, HomeTask{
			Title:  homeCountText(missingSecrets+missingShares, "个资源缺备注"),
			Detail: strings.Join(details, "、") + "缺备注，agent 无法判断用途",
			Href:   "/ui/authorization/",
		})
	}
	if h.cfg.SoftwareInventory != nil && h.caps.ShowSoftwareInventoryArea {
		todos, err := h.cfg.SoftwareInventory.SoftwareInventoryTodos()
		if err != nil {
			return nil, err
		}
		unidentified := len(todos.UnidentifiedProcesses)
		stale := len(todos.StaleDevices)
		if unidentified > 0 || stale > 0 {
			details := make([]string, 0, 2)
			if unidentified > 0 {
				details = append(details, homeCountText(unidentified, "个未识别进程待认领"))
			}
			if stale > 0 {
				details = append(details, homeCountText(stale, "台设备清单已过期"))
			}
			view.Tasks = append(view.Tasks, HomeTask{
				Title:  "软件清单有待办",
				Detail: strings.Join(details, "、"),
				Href:   "/ui/software-inventory/todos",
			})
		}
	}
	if h.cfg.Ops != nil && h.caps.CanQueryVaultAudit {
		rows, err := h.cfg.Ops.QueryAudit(OpsAuditQuery{Limit: 5})
		if err != nil {
			return nil, err
		}
		view.RecentChanges = homeRecentChanges(rows)
		return view, nil
	}
	if h.cfg.Ops != nil && h.caps.CanQueryClientAudit {
		rows, err := h.cfg.Ops.QueryClientAudit(h.requestClientInstance(r), 5)
		if err != nil {
			return nil, err
		}
		view.RecentChanges = homeRecentChanges(rows)
	}
	return view, nil
}

// homeRecentChanges 把审计行换算成主页时间线；优先展示写入时留下的摘要，
// 没有摘要时退回「动作 · 服务器 · 状态」。
var homeAuditActionNames = map[string]string{
	"server.add":          "新增服务器条目",
	"server.edit":         "编辑服务器条目",
	"server.delete":       "删除服务器条目",
	"server.rm":           "删除服务器条目",
	"server.import":       "导入服务器条目",
	"server.connect-test": "测试服务器连接",
	"profile.add":         "新增 Profile",
	"profile.grant":       "调整 Profile 授权",
	"profile.revoke":      "调整 Profile 授权",
	"profile.rm":          "删除 Profile",
	"secret.grant":        "调整机密项授权",
	"secret.allow":        "调整机密项授权",
	"secret-disallow":     "调整机密项授权",
	"share-revoke":        "撤销共享存储",
	"pair.approve":        "批准配对",
	"pair.reject":         "拒绝配对",
	"client.remove":       "移除客户端实例",
	"client.sync":         "客户端同步",
	"cache-token.add":     "添加客户端实例",
	"cache-token.revoke":  "吊销客户端拉取凭据",
	"cache-token.bind":    "绑定客户端实例",
	"admin.password_set":  "设置管理员口令",
	"backup.create":       "创建备份",
	"backup.verify":       "校验备份",
	"backup.config":       "修改备份设置",
}

func homeAuditActionName(action string) string {
	if name, ok := homeAuditActionNames[action]; ok {
		return name
	}
	return "系统操作"
}

func homeRecentChanges(rows []OpsAuditRow) []HomeRecentChange {
	changes := make([]HomeRecentChange, 0, len(rows))
	for _, row := range rows {
		summary := row.Command
		if summary == "" {
			summary = homeAuditActionName(row.Action)
			if server := row.ServerDisplay(); server != "无" {
				summary += " · " + server
			}
			if row.Status != "" && row.Status != "ok" {
				summary += " · 状态 " + row.Status
			}
		}
		changes = append(changes, HomeRecentChange{
			TimeText: row.TS.Format("2006-01-02 15:04:05"),
			Summary:  summary,
		})
	}
	return changes
}

func homeCountText(count int, noun string) string {
	return strconv.Itoa(count) + " " + noun
}
