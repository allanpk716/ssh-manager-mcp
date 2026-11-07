package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func newDevicesLocalCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "local", Short: "Manage this machine's local topology device setting"}
	cmd.AddCommand(devicesLocalSetCmd(), devicesLocalShowCmd(), devicesLocalClearCmd())
	return cmd
}

func devicesLocalSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Set which topology device is this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			device, err := s.GetDevice(args[0])
			if err != nil {
				return err
			}
			if err := roles.SetLocalDeviceID(device.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已设定本机设备：%s\n", device.Name)
			return nil
		},
	}
}

func devicesLocalShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Args:  cobra.NoArgs,
		Short: "Show which topology device is this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			deviceID, err := roles.ReadLocalDeviceID()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if deviceID == "" {
				fmt.Fprintln(out, "未设定本机设备")
				return nil
			}
			devices, err := s.ListDevices(store.DeviceFilter{})
			if err != nil {
				return err
			}
			for _, device := range devices {
				if device.ID == deviceID {
					fmt.Fprintf(out, "本机设备：%s\n", device.Name)
					return nil
				}
			}
			fmt.Fprintln(out, "本机设备指向的设备已不存在，按未设定处理")
			return nil
		},
	}
}

func devicesLocalClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Args:  cobra.NoArgs,
		Short: "Clear this machine's local topology device setting",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			removed, err := roles.ClearLocalDevice()
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintln(cmd.OutOrStdout(), "已清除本机设备设定")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "本来就没有本机设备设定")
			return nil
		},
	}
}
