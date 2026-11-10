package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
)

// inventoryCollectorCmd 在权威端登记一台设备由哪个客户端实例采集。
func inventoryCollectorCmd() *cobra.Command {
	var client, entry string
	var authority bool
	c := &cobra.Command{
		Use:   "collector DEVICE (--client 设备码名|--authority) [--entry 服务器条目]",
		Args:  cobra.ExactArgs(1),
		Short: "登记一台设备的采集执行者",
		Long: `登记一台设备的实测采集执行者。DEVICE 是设备名；--client 把采集交给
某个客户端实例，值是权威端设备码的名字；--authority 改回权威端或单机执
行。--entry 可指定这台设备上的采集用服务器条目，不指定时每次采集再按
默认规则选择。客户端实例登记前会校验该设备码绑定 profile 包含显式指定的
服务器条目，失败时拒绝并说明原因。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if authority == (client != "") {
				return errors.New("必须且只能给出 --client 或 --authority 之一")
			}
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.SetSoftwareCollectionCollector(
				args[0], client, entry, store.CLISource(),
			); err != nil {
				return err
			}
			serverText := "默认规则选择的"
			if entry != "" {
				serverText = entry
			}
			fmt.Fprintf(cmd.OutOrStdout(), "设备 %s 的采集执行者已登记为 %s（采集用服务器条目：%s）。\n",
				args[0], collectorName(client, authority), serverText)
			return nil
		},
	}
	c.Flags().StringVar(&client, "client", "", "客户端实例的设备码名")
	c.Flags().BoolVar(&authority, "authority", false, "改回由权威端或单机采集")
	c.Flags().StringVar(&entry, "entry", "", "采集用的服务器条目（可选）")
	return c
}

func collectorName(client string, authority bool) string {
	if authority {
		return "权威端"
	}
	return client
}
