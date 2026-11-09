package cli

import (
	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/webui"
)

// machineInitializer 把初始化模块适配成 Web UI 注入的窄接口。Web UI 包不
// 导入 machineinit，避免页面层与 serve Web UI 适配层形成导入环。
type machineInitializer struct{}

func (machineInitializer) ReadSnapshot() (webui.InitializationSnapshot, error) {
	snapshot, err := machineinit.ReadSnapshot()
	if err != nil {
		return webui.InitializationSnapshot{}, err
	}
	destination, err := machineinit.DecideUIDestination(snapshot)
	if err != nil {
		return webui.InitializationSnapshot{}, err
	}
	out := webui.InitializationSnapshot{}
	switch destination.Step {
	case machineinit.WizardStepCreateVault:
		out.Step = webui.InitializationStepCreateVault
	default:
		out.Step = webui.InitializationStepChooseRole
	}
	switch destination.Role {
	case roles.RoleStandalone:
		out.Role = webui.InitializationRoleStandalone
	case roles.RoleServer:
		out.Role = webui.InitializationRoleServer
	case roles.RoleClient:
		out.Role = webui.InitializationRoleClient
	}
	return out, nil
}

func (machineInitializer) SelectRoleWithoutVault(role webui.InitializationRole) error {
	return machineinit.SelectRoleWithoutVault(machineRole(role))
}

func (machineInitializer) Initialize(role webui.InitializationRole) (webui.InitializationResult, error) {
	result, err := machineinit.Initialize(machineRole(role))
	return webui.InitializationResult{MasterKeyPath: result.MasterKeyPath}, err
}

func machineRole(role webui.InitializationRole) roles.Role {
	return roles.Role(role)
}
