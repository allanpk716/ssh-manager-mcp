package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/softwareinventory"
)

// inventoryClaimCmd 用一条已登记的声明认领实测条目。稳定键来自统一查询
// 或待办列表的 JSON 输出；它不是数据库行号，实测结果整体替换后仍然有效。
func inventoryClaimCmd() *cobra.Command {
	var observedKey string
	c := &cobra.Command{
		Use:   "claim ID --observed-key 实测稳定键",
		Args:  cobra.ExactArgs(1),
		Short: "Claim an observed item with a declaration",
		Long: `用一条声明认领同一设备上的实测条目或未识别进程。认领后两者合成一条
软件条目显示：版本与监听端口以实测为准，暴露级别、标准标识与备注取声明值。

实测稳定键可用 inventory view --json 或 inventory todo --json 查看。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := softwareinventory.New(s).ClaimDeclaration(
				args[0], observedKey, softwareinventory.CLISource(),
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已认领软件声明 %s\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&observedKey, "observed-key", "", "实测条目的稳定键")
	_ = c.MarkFlagRequired("observed-key")
	return c
}

// inventoryUnclaimCmd 取消一条声明的认领关系。
func inventoryUnclaimCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unclaim ID",
		Args:  cobra.ExactArgs(1),
		Short: "Remove the claim relation from a declaration",
		Long: `取消一条声明对实测条目或未识别进程的认领。实测条目不会被删除；
下一次查询会重新按声明与实测两条显示。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := softwareinventory.New(s).UnclaimDeclaration(
				args[0], softwareinventory.CLISource(),
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已取消认领软件声明 %s\n", args[0])
			return nil
		},
	}
}
