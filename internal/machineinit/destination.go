package machineinit

import (
	"fmt"

	"ssh-manager-mcp/internal/roles"
)

// UIDestinationKind 是 `sshmgr ui` 根据本机状态应走的路径。
type UIDestinationKind int

const (
	// UIDestinationInitializationWizard 表示启动回环初始化向导。
	UIDestinationInitializationWizard UIDestinationKind = iota
	// UIDestinationStandaloneWebUI 表示启动单机正式 Web UI。
	UIDestinationStandaloneWebUI
	// UIDestinationServerWizard 表示权威端初始化向导。
	UIDestinationServerWizard
	// UIDestinationAuthorityStatus 表示不启动监听，只打印权威端 Web UI 状态。
	UIDestinationAuthorityStatus
	// UIDestinationClientWebUI 表示启动客户端只读 Web UI。
	UIDestinationClientWebUI
)

// WizardStep 是初始化向导的步骤入口。
type WizardStep int

const (
	// WizardStepChooseRole 是「选择本机角色」步骤。
	WizardStepChooseRole WizardStep = iota
	// WizardStepCreateVault 是「创建 vault」步骤。
	WizardStepCreateVault
	// WizardStepSetAdminPassword 是权威端「设置管理员口令」步骤。
	WizardStepSetAdminPassword
	// WizardStepInstallServe 是权威端「安装 serve」步骤。
	WizardStepInstallServe
)

// UIDestination 是 `sshmgr ui` 的去向。Role 与 Step 只有向导去向会填写。
type UIDestination struct {
	Kind UIDestinationKind
	Role roles.Role
	Step WizardStep
}

// DecideUIDestination 是纯函数：只根据本机状态快照决定 `sshmgr ui` 走哪条
// 路，不读文件、不探测端口、不改动状态。
func DecideUIDestination(snapshot StateSnapshot) (UIDestination, error) {
	if snapshot.VaultExists && !snapshot.VaultOpenable {
		return UIDestination{}, fmt.Errorf(
			"本机 vault 存在但锁定或不可读：%w — 钥匙问题先运行 `sshmgr unlock`；权限或数据库打开失败则运行 `sshmgr doctor` 查 store 边车检查（不会降级为 client 模式）",
			snapshot.VaultOpenErr,
		)
	}

	if snapshot.Role == nil {
		switch {
		case snapshot.VaultExists && snapshot.VaultOpenable:
			if snapshot.DetectedRole == roles.RoleServer {
				return serverDestination(snapshot)
			}
			return UIDestination{Kind: UIDestinationStandaloneWebUI}, nil
		case snapshot.ClientCachePresent:
			return UIDestination{Kind: UIDestinationClientWebUI}, nil
		default:
			return UIDestination{
				Kind: UIDestinationInitializationWizard,
				Step: WizardStepChooseRole,
			}, nil
		}
	}

	switch snapshot.Role.Role {
	case roles.RoleClient:
		return UIDestination{Kind: UIDestinationClientWebUI}, nil
	case roles.RoleStandalone:
		if !snapshot.VaultExists {
			if !snapshot.Role.SetupComplete {
				return UIDestination{
					Kind: UIDestinationInitializationWizard,
					Role: roles.RoleStandalone,
					Step: WizardStepCreateVault,
				}, nil
			}
			return UIDestination{}, fmt.Errorf(
				"role.json 声明本机为 %s，但 vault 不存在：运行 `sshmgr clear` 清除残留状态后重新运行向导",
				snapshot.Role.Role,
			)
		}
		return UIDestination{Kind: UIDestinationStandaloneWebUI}, nil
	case roles.RoleServer:
		return serverDestination(snapshot)
	default:
		return UIDestination{}, fmt.Errorf("role.json role 值非法 %q — 运行 `sshmgr clear` 后重新初始化", snapshot.Role.Role)
	}
}

// serverDestination 按权威端的三项实时初始化状态选择步骤；`setup_complete`
// 不参与权威端是否完成初始化的判断。
func serverDestination(snapshot StateSnapshot) (UIDestination, error) {
	if snapshot.ServeWebUI.Running {
		return UIDestination{Kind: UIDestinationAuthorityStatus}, nil
	}
	destination := UIDestination{Kind: UIDestinationServerWizard, Role: roles.RoleServer}
	switch {
	case !snapshot.VaultExists || !snapshot.VaultOpenable:
		destination.Step = WizardStepCreateVault
	case !snapshot.AdminPasswordSet:
		destination.Step = WizardStepSetAdminPassword
	default:
		destination.Step = WizardStepInstallServe
	}
	return destination, nil
}
