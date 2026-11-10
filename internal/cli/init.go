package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/roles"
)

func newInitCmd() *cobra.Command {
	var roleText string
	c := &cobra.Command{
		Use:   "init",
		Short: "Initialize this machine as standalone or the authority",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := parseInitRole(roleText)
			if err != nil {
				return NewExitCodeError(1, err)
			}
			result, err := machineinit.Initialize(role)
			if err != nil {
				return NewExitCodeError(1, err)
			}
			return writeInitResult(cmd.OutOrStdout(), role, result)
		},
	}
	c.Flags().StringVar(&roleText, "role", "", "machine role: standalone or server (required)")
	return c
}

func parseInitRole(roleText string) (roles.Role, error) {
	switch strings.TrimSpace(roleText) {
	case "":
		return "", errors.New("--role 是必填参数，取值为 standalone（单机）或 server（权威端）")
	case string(roles.RoleStandalone):
		return roles.RoleStandalone, nil
	case string(roles.RoleServer):
		return roles.RoleServer, nil
	default:
		return "", fmt.Errorf("--role 取值 %q 无效，只能是 standalone（单机）或 server（权威端）", roleText)
	}
}

func writeInitResult(out io.Writer, role roles.Role, result machineinit.Result) error {
	roleName := "单机"
	if role == roles.RoleServer {
		roleName = "权威端"
	}
	switch result.Kind {
	case machineinit.ResultAlreadyInitialized:
		fmt.Fprintf(out, "已是该角色（%s），无需改动。\n", roleName)
	case machineinit.ResultCreated:
		fmt.Fprintf(out, "已初始化为%s。\n", roleName)
	case machineinit.ResultRoleRewritten:
		if role == roles.RoleServer {
			fmt.Fprintln(out, "已升级为权威端，vault 内容保持不变。")
		} else {
			fmt.Fprintf(out, "已初始化为%s。\n", roleName)
		}
	}
	fmt.Fprintf(out, "主密钥文件：%s\n", result.MasterKeyPath)
	fmt.Fprintln(out, "请备份此文件；文件丢失后 vault 将无法打开。")
	if role == roles.RoleServer && result.Kind != machineinit.ResultAlreadyInitialized {
		fmt.Fprintln(out, "接下来两步：")
		fmt.Fprintln(out, "1. sshmgr serve admin set")
		fmt.Fprintln(out, "2. 提权运行 sshmgr serve install")
	}
	return nil
}
