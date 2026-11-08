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

	"ssh-manager-mcp/internal/mcpserver"
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
		Short: "管理权威端 Web UI 的管理员口令与会话",
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

func writeAuthorityWebUIStatus(out io.Writer, st *store.Store) error {
	passwordSet, err := st.WebAdminPasswordIsSet()
	if err != nil {
		return err
	}
	port, plan, err := mcpserver.ReadWebUIListenPlan(st)
	if err != nil {
		return err
	}
	_, _, fingerprint, err := mcpserver.LoadOrCreateServeCert()
	if err != nil {
		return err
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
	fmt.Fprintf(out, "证书公钥 SHA-256 指纹：%s\n", fingerprint)
	if !passwordSet {
		fmt.Fprintln(out, "设置指引：在权威端运行 `sshmgr ui` 走向导，或运行 `sshmgr serve admin set` 设置管理员口令。")
	}
	return nil
}

func newServeAdminSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set",
		Short: "设置或重置权威端 Web UI 的管理员口令",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requireServerRoleVault()
			if err != nil {
				return err
			}
			defer st.Close()
			first, err := serveAdminPasswordPrompt()
			if err != nil {
				return fmt.Errorf("读取管理员口令失败：%w", err)
			}
			second, err := serveAdminPasswordPrompt()
			if err != nil {
				return fmt.Errorf("读取第二次管理员口令失败：%w", err)
			}
			if string(first) != string(second) {
				return NewExitCodeError(1, errors.New("两次输入的管理员口令不一致"))
			}
			if len(first) == 0 {
				return NewExitCodeError(1, errors.New("管理员口令不能为空"))
			}
			generation, err := st.SetWebAdminPassword(first)
			if err != nil {
				return err
			}
			if err := writeServeAdminAudit(st, "cli.admin.password.set", nil); err != nil {
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
		Short: "解除一个 Web UI 登录锁定；不带参数时列出当前锁定",
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
			if err := st.ClearWebAdminLoginFailures(ip.String()); err != nil {
				return err
			}
			if err := writeServeAdminAudit(st, "cli.admin.unlock", map[string]string{"ip": ip.String()}); err != nil {
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
		Short: "注销全部权威端 Web UI 会话",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requireServerRoleVault()
			if err != nil {
				return err
			}
			defer st.Close()
			generation, err := st.LogoutAllWebAdminSessions()
			if err != nil {
				return err
			}
			if err := writeServeAdminAudit(st, "cli.admin.logout_all", nil); err != nil {
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
		Short: "显示权威端 Web UI 地址与证书指纹",
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

func writeServeAdminAudit(st *store.Store, action string, detail map[string]string) error {
	summary := make(map[string]any, len(detail))
	for key, value := range detail {
		summary[key] = value
	}
	return st.WriteAudit(store.AuditRow{
		TS: time.Now(), Action: action, Status: "ok",
		Source: store.AuditSourceCLI, Command: store.MarshalAuditSummary(summary),
	})
}
