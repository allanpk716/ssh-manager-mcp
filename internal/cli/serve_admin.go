package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

var serveAdminPasswordPrompt = func() ([]byte, error) {
	fmt.Fprint(os.Stderr, "输入管理员口令：")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return password, err
}

func newServeAdminCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "admin",
		Short: "Manage the authoritative Web UI administrator password and sessions",
	}
	c.AddCommand(
		newServeAdminSetCmd(),
		newServeAdminUnlockCmd(),
		newServeAdminLogoutAllCmd(),
		newServeAdminStatusCmd(),
	)
	return c
}

func requireServerRoleVault() (*store.Store, error) {
	launch, err := resolveMachineRole("")
	if err != nil {
		return nil, err
	}
	if launch.Kind != roles.LaunchBroker || launch.ResumeSetup || launch.Role != roles.RoleServer {
		return nil, NewExitCodeError(1, errors.New("`serve admin` 只能在权威端主机执行；单机与客户端没有权威端 Web 管理员口令"))
	}
	return openUnlockedStore()
}

// requireServerRoleVaultForPasswordSet 是 `serve admin set` 的角色判定：
// 只要本机角色是权威端且 vault 可打开就放行，不再因 setup_complete 为假
// 而拒绝（该标记只保留「选了角色、还没建 vault」的续做含义；升级中的
// 权威端也要能先设管理员口令）。
func requireServerRoleVaultForPasswordSet() (*store.Store, error) {
	launch, err := resolveMachineRole("")
	if err != nil {
		return nil, err
	}
	if launch.Kind != roles.LaunchBroker || launch.Role != roles.RoleServer {
		return nil, NewExitCodeError(1, errors.New("`serve admin` 只能在权威端主机执行；单机与客户端没有权威端 Web 管理员口令"))
	}
	return openUnlockedStore()
}

func writeAuthorityWebUIStatus(out io.Writer, st *store.Store) error {
	passwordSet, err := st.WebAdminPasswordIsSet()
	if err != nil {
		return err
	}
	port, plan, err := mcpserver.ReadWebUIListenPlan(st)
	if err != nil {
		return err
	}
	certPath, err := paths.ServeCertPath()
	if err != nil {
		return err
	}
	var fingerprintLines []string
	_, statErr := os.Stat(certPath)
	if errors.Is(statErr, os.ErrNotExist) {
		markerPath, markerErr := paths.ServeCertMarkerPath()
		if markerErr != nil {
			return markerErr
		}
		if _, markerStatErr := os.Stat(markerPath); markerStatErr == nil {
			return errors.New("serve 证书缺失但初始化标记仍存在；请按证书恢复流程处理，不能自动生成")
		} else if !errors.Is(markerStatErr, os.ErrNotExist) {
			return markerStatErr
		}
		fingerprintLines = []string{
			"证书：`serve` 尚未生成证书，先启动一次 `sshmgr serve`。",
			fmt.Sprintf("证书路径：%s（默认证书路径；若 serve 使用 --tls-cert 指定其他证书，以登录页显示为准。）", certPath),
		}
	} else if statErr != nil {
		return statErr
	} else {
		_, _, fingerprint, readErr := mcpserver.ReadServeCertFingerprint()
		if readErr != nil {
			return readErr
		}
		fingerprintLines = []string{
			fmt.Sprintf("证书公钥 SHA-256 指纹：%s", fingerprint),
			fmt.Sprintf("证书路径：%s（默认证书路径；若 serve 使用 --tls-cert 指定其他证书，以登录页显示为准。）", certPath),
		}
	}
	passwordState := "未设置"
	if passwordSet {
		passwordState = "已设置"
	}
	fmt.Fprintf(out, "管理员口令：%s\n", passwordState)
	fmt.Fprintf(out, "Web UI 端口：%d\n", port)
	for _, address := range plan.Addresses {
		fmt.Fprintf(out, "Web UI 地址：https://%s/ui/\n", net.JoinHostPort(address, strconv.Itoa(port)))
	}
	if plan.NeedsManualListen {
		fmt.Fprintln(out, "提示：未找到本机存在的虚拟局域网地址，当前只监听 127.0.0.1；可用设置项 serve.ui_listen 指定地址。")
	}
	for _, line := range fingerprintLines {
		fmt.Fprintln(out, line)
	}
	if !passwordSet {
		fmt.Fprintln(out, "设置指引：在权威端主机上运行 `sshmgr serve admin set` 设置管理员口令。")
	}
	return nil
}

func newServeAdminSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set",
		Short: "Set or reset the authoritative Web UI administrator password",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requireServerRoleVaultForPasswordSet()
			if err != nil {
				return err
			}
			if err := st.Close(); err != nil {
				return err
			}
			first, err := serveAdminPasswordPrompt()
			if err != nil {
				return fmt.Errorf("读取管理员口令失败：%w", err)
			}
			second, err := serveAdminPasswordPrompt()
			if err != nil {
				return fmt.Errorf("读取第二次管理员口令失败：%w", err)
			}
			if err := machineinit.ValidateAdminPassword(first, second); err != nil {
				return NewExitCodeError(1, err)
			}
			generation, err := machineinit.SetAdminPassword(first, serveAdminAuditRow("admin.password_set", nil))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "管理员口令已设置，全部 Web 会话已作废（会话代 %d）。\n", generation)
			return nil
		},
	}
}

func newServeAdminUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock [IP]",
		Short: "Unlock a Web UI login lock; without an IP, list current locks",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requireServerRoleVault()
			if err != nil {
				return err
			}
			defer st.Close()
			if len(args) == 0 {
				locks, err := st.ListWebAdminLoginLocks(time.Now())
				if err != nil {
					return err
				}
				if len(locks) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "当前没有锁定的来源 IP。")
					return nil
				}
				for _, lock := range locks {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  剩余 %s\n", lock.IP, lock.Remaining)
				}
				return nil
			}
			ip := net.ParseIP(args[0])
			if ip == nil {
				return NewExitCodeError(1, fmt.Errorf("%q 不是有效的 IP 地址", args[0]))
			}
			detail := map[string]string{"ip": ip.String()}
			if err := st.ClearWebAdminLoginFailuresWithAudit(ip.String(), serveAdminAuditRow("admin.unlock", detail)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "来源 IP %s 的登录锁定已解除。\n", ip.String())
			return nil
		},
	}
}

func newServeAdminLogoutAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout-all",
		Short: "Log out all authoritative Web UI sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requireServerRoleVault()
			if err != nil {
				return err
			}
			defer st.Close()
			generation, err := st.LogoutAllWebAdminSessionsWithAudit(serveAdminAuditRow("admin.logout_all", nil))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "全部 Web 会话已注销（会话代 %d）。\n", generation)
			return nil
		},
	}
}

func newServeAdminStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show authoritative Web UI addresses and certificate fingerprint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requireServerRoleVault()
			if err != nil {
				return err
			}
			defer st.Close()
			return writeAuthorityWebUIStatus(cmd.OutOrStdout(), st)
		},
	}
}

func serveAdminAuditRow(action string, detail map[string]string) store.AuditRow {
	summary := make(map[string]any, len(detail))
	for key, value := range detail {
		summary[key] = value
	}
	return store.AuditRow{
		Action: action, Status: "ok",
		Source: store.AuditSourceCLI, Command: store.MarshalAuditSummary(summary),
	}
}
