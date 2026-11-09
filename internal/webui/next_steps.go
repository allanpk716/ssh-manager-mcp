package webui

import "net/http"

// NextStep 是单机首页「下一步引导」卡片中的一步。
type NextStep struct {
	Title       string
	Description string
	Href        string
}

// standaloneNextSteps 读取单机 vault 里的服务器、Profile 与项目令牌，返回
// 还没完成的初始化后续步骤。客户端与权威端不使用这张卡片。
func (h *handler) standaloneNextSteps(r *http.Request) ([]NextStep, error) {
	if h.cfg.Role != RoleStandalone {
		return nil, nil
	}
	servers, err := h.cfg.Servers.Servers(h.requestClientInstance(r))
	if err != nil {
		return nil, err
	}
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		return nil, err
	}
	projects, err := h.cfg.Access.ListProjects(true)
	if err != nil {
		return nil, err
	}

	var steps []NextStep
	if len(servers) == 0 {
		steps = append(steps, NextStep{
			Title:       "录服务器",
			Description: "先录入要管理的服务器，或导入现有的 SSH 配置。",
			Href:        "/ui/servers/import",
		})
	}
	if len(profiles) == 0 {
		steps = append(steps, NextStep{
			Title:       "建 Profile 并授权",
			Description: "把服务器分配给 Profile，后续项目令牌按 Profile 取得访问范围。",
			Href:        "/ui/access/profiles",
		})
	}
	if len(projects) == 0 {
		steps = append(steps, NextStep{
			Title:       "建项目并取令牌",
			Description: "为使用 sshmgr 的项目签发令牌，并复制到它的模型上下文配置。",
			Href:        "/ui/access/projects",
		})
	}
	return steps, nil
}
