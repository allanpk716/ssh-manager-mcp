package ops

import (
	"ssh-manager-mcp/internal/roles"
)

// RoleInfo 是本机角色文件的只读展示。
type RoleInfo struct {
	Role          roles.Role
	SetupComplete bool
	Path          string
}

// ReadRole 读取本机角色文件与它所在的路径。文件不存在时角色为空、路径指向
// 单机 vault 的预期位置，页面仍能告诉 owner 下一步应先完成初始化。
func ReadRole() (RoleInfo, error) {
	state, err := roles.Load()
	if err != nil {
		return RoleInfo{}, err
	}
	if state == nil {
		path, pathErr := roles.RolePath(roles.RoleStandalone)
		if pathErr != nil {
			return RoleInfo{}, pathErr
		}
		return RoleInfo{Path: path}, nil
	}
	path, err := roles.RolePath(state.Role)
	if err != nil {
		return RoleInfo{}, err
	}
	return RoleInfo{Role: state.Role, SetupComplete: state.SetupComplete, Path: path}, nil
}
