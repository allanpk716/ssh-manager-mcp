package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func newDevicesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "devices", Short: "Manage topology devices"}
	cmd.AddCommand(devicesAddCmd(), devicesEditCmd(), devicesShowCmd(), devicesListCmd(), devicesRmCmd(), newDevicesMemberCmd())
	return cmd
}

func devicesListCmd() *cobra.Command {
	var site, network string
	var unassigned bool
	c := &cobra.Command{
		Use:   "list [--site 站点] [--network 网络] [--unassigned]",
		Args:  cobra.NoArgs,
		Short: "List topology devices, optionally filtered",
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := topologyRoleFor()
			if err != nil {
				return err
			}
			if role == policyRoleClient {
				return errors.New("网络拓扑这一版尚未随快照下发；请在权威端查看设备")
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			devices, err := s.ListDevices(store.DeviceFilter{
				Site: site, Network: network, Unassigned: unassigned,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(devices) == 0 {
				fmt.Fprintln(out, "没有匹配的设备")
				return nil
			}
			for _, device := range devices {
				printDevice(out, device, device.Memberships)
			}
			return nil
		},
	}
	c.Flags().StringVar(&site, "site", "", "只显示在这个站点的物理局域网里的设备")
	c.Flags().StringVar(&network, "network", "", "只显示在这个网络里的设备")
	c.Flags().BoolVar(&unassigned, "unassigned", false, "只显示没有任何网络成员的设备")
	return c
}

func devicesRmCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "rm NAME [--yes]",
		Args:  cobra.ExactArgs(1),
		Short: "Remove a device without server entries after confirmation",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if !yes {
				if !topologyStdinIsInteractive() {
					return errors.New("非交互终端不能确认删除设备；请加 --yes 后重跑")
				}
				device, err := s.GetDevice(args[0])
				if err != nil {
					return err
				}
				memberships, err := s.ListNetworkMemberships(args[0])
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "将删除设备 %s。\n", device.Name)
				fmt.Fprintln(out, "网络成员：")
				if len(memberships) == 0 {
					fmt.Fprintln(out, "  无")
				}
				for _, membership := range memberships {
					fmt.Fprintf(out, "  %s：%s\n", membership.NetworkName, membership.Address)
				}
				client := device.ClientName
				if client == "" {
					client = "无"
				}
				fmt.Fprintf(out, "会被解除关联的客户端实例：%s\n", client)
				fmt.Fprint(out, "输入 y 确认删除：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取删除确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未删除设备。")
					return nil
				}
			}
			device, _, err := s.DeleteDevice(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已删除设备 %s\n", device.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "跳过删除确认；非交互终端必须显式给出")
	return c
}

func newDevicesMemberCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "member", Short: "Manage a device's network memberships"}
	cmd.AddCommand(devicesMemberAddCmd(), devicesMemberEditCmd(), devicesMemberRmCmd())
	return cmd
}

func devicesMemberAddCmd() *cobra.Command {
	var network, address string
	c := &cobra.Command{
		Use:   "add DEVICE --network NAME --address ADDR",
		Args:  cobra.ExactArgs(1),
		Short: "Add a device to a network",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			membership, err := s.AddNetworkMembership(args[0], network, address)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已把设备 %s 加入网络 %s：%s\n",
				membership.DeviceName, membership.NetworkName, membership.Address)
			return nil
		},
	}
	c.Flags().StringVar(&network, "network", "", "网络名字")
	c.Flags().StringVar(&address, "address", "", "设备在这个网络里的 IP 或域名")
	_ = c.MarkFlagRequired("network")
	_ = c.MarkFlagRequired("address")
	return c
}

func devicesMemberEditCmd() *cobra.Command {
	var network, address string
	c := &cobra.Command{
		Use:   "edit DEVICE --network NAME --address ADDR",
		Args:  cobra.ExactArgs(1),
		Short: "Edit a device's address in one network",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			membership, err := s.EditNetworkMembership(args[0], network, address)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已修改设备 %s 在网络 %s 的地址：%s\n",
				membership.DeviceName, membership.NetworkName, membership.Address)
			return nil
		},
	}
	c.Flags().StringVar(&network, "network", "", "网络名字")
	c.Flags().StringVar(&address, "address", "", "新的 IP 或域名")
	_ = c.MarkFlagRequired("network")
	_ = c.MarkFlagRequired("address")
	return c
}

func devicesMemberRmCmd() *cobra.Command {
	var network string
	c := &cobra.Command{
		Use:   "rm DEVICE --network NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Remove a device from one network",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			membership, err := s.RemoveNetworkMembership(args[0], network)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已把设备 %s 移出网络 %s\n",
				membership.DeviceName, membership.NetworkName)
			return nil
		},
	}
	c.Flags().StringVar(&network, "network", "", "网络名字")
	_ = c.MarkFlagRequired("network")
	return c
}

func devicesAddCmd() *cobra.Command {
	var note, client string
	c := &cobra.Command{
		Use:   "add NAME [--note 文本] [--client 设备码]",
		Args:  cobra.ExactArgs(1),
		Short: "Register a topology device",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			device, err := s.AddDevice(args[0], note, client)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已登记设备 %s\n", device.Name)
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "备注")
	c.Flags().StringVar(&client, "client", "", "关联的客户端实例设备码")
	return c
}

func devicesEditCmd() *cobra.Command {
	var name, note, client string
	var noClient bool
	c := &cobra.Command{
		Use:   "edit NAME [--name 新名字] [--note 新备注] [--client 设备码 | --no-client]",
		Args:  cobra.ExactArgs(1),
		Short: "Edit a topology device with partial-update semantics",
		RunE: func(cmd *cobra.Command, args []string) error {
			changedClient := cmd.Flags().Changed("client")
			if changedClient && noClient {
				return errors.New("--client 与 --no-client 不能同时使用")
			}
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			changes := store.DeviceChanges{}
			if cmd.Flags().Changed("name") {
				changes.Name = &name
			}
			if cmd.Flags().Changed("note") {
				changes.Note = &note
			}
			if changedClient {
				changes.Client = &client
			} else if noClient {
				empty := ""
				changes.Client = &empty
			}
			device, err := s.EditDevice(args[0], changes)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已修改设备 %s；新名字 %s\n", args[0], device.Name)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "新名字")
	c.Flags().StringVar(&note, "note", "", "新备注；传空字符串清空")
	c.Flags().StringVar(&client, "client", "", "新的客户端实例设备码")
	c.Flags().BoolVar(&noClient, "no-client", false, "解除客户端实例关联")
	return c
}

func devicesShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Show a topology device",
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := topologyRoleFor()
			if err != nil {
				return err
			}
			if role == policyRoleClient {
				return errors.New("网络拓扑这一版尚未随快照下发；请在权威端查看设备")
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			device, err := s.GetDevice(args[0])
			if err != nil {
				return err
			}
			memberships, err := s.ListNetworkMemberships(args[0])
			if err != nil {
				return err
			}
			printDevice(cmd.OutOrStdout(), device, memberships)
			return nil
		},
	}
}

// printDevice 输出设备详情；网络成员由网络成员命令补上。
func printDevice(w io.Writer, device *models.Device, memberships []*models.NetworkMembership) {
	fmt.Fprintf(w, "设备 %s\n", device.Name)
	if device.Note != "" {
		fmt.Fprintf(w, "  备注：%s\n", device.Note)
	}
	client := device.ClientName
	if client == "" {
		client = "无"
	}
	fmt.Fprintf(w, "  客户端实例：%s\n", client)
	if len(memberships) == 0 {
		fmt.Fprintln(w, "  网络成员：无")
		return
	}
	for _, membership := range memberships {
		network := membership.NetworkName + "（" + networkTypeLabel(membership.NetworkType)
		if membership.SiteName != "" {
			network += "，站点 " + membership.SiteName
		}
		network += "）"
		fmt.Fprintf(w, "  网络成员：%s：%s\n", network, membership.Address)
	}
}
