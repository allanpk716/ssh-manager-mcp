package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/termdetect"
)

var errSharesNeedVault = errors.New("本机还没有 vault：共享存储随 vault 一起存放，请先运行初始化向导（直接执行 sshmgr）")

var sharesStdinIsInteractive = func() bool {
	return termdetect.IsTerminal(os.Stdin.Fd())
}

const sharesSMBPrerequisites = "前提：首版只支持 SMB；提供方设备需关闭『禁止同一 IP 多个连接』类限制。"

func requireSharesVault() (*store.Store, error) {
	return requireOwnerVaultForCommand(
		errSharesNeedVault,
		errors.New("本机是缓存客户端：共享存储在权威端维护，请在权威端执行这条命令"),
	)
}

func newSharesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "shares", Short: "Manage registered shared storage"}
	cmd.AddCommand(sharesAddCmd(), sharesEditCmd(), sharesRmCmd(), sharesListCmd(),
		sharesShowCmd(), sharesUnmountCmd(), sharesRevokeCmd(), sharesReselectCmd(),
		sharesResidueCmd())
	return cmd
}

func sharesAddCmd() *cobra.Command {
	var device, user, passwordSecret, drive, note string
	var directories []string
	c := &cobra.Command{
		Use:   "add NAME --device 设备 --user 用户名 --password-secret 机密项 --drive 盘符 [--dir 用途=子路径 ...] [--note 文本]",
		Args:  cobra.ExactArgs(1),
		Short: "Register an SMB shared storage",
		Long: `在 vault 里登记一处供多台服务器共用的网络共享。

` + sharesSMBPrerequisites + `

共享名在同一 vault 内唯一（不区分大小写）；盘符只接受 D 到 Z，并在同一 vault 内唯一。
--dir 可重复给出，每条格式为 用途=子路径。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			parsedDirectories, err := parseSharedStorageDirectories(directories)
			if err != nil {
				return err
			}
			share, err := s.AddSharedStorage(store.SharedStorageInput{
				Name: args[0], Device: device, User: user,
				PasswordSecret: passwordSecret, Drive: drive,
				Directories: parsedDirectories, Note: note,
			}, store.CLISource())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "已登记共享存储 %s；版本号：%d\n", share.Name, share.Version)
			fmt.Fprintln(out, sharesSMBPrerequisites)
			if len(share.CandidateAddresses) == 0 {
				fmt.Fprintln(out, "警告：该设备没有网络成员，暂时无法挂载。")
			}
			return nil
		},
	}
	c.Flags().StringVar(&device, "device", "", "提供方设备名字")
	c.Flags().StringVar(&user, "user", "", "专用挂载账号的用户名")
	c.Flags().StringVar(&passwordSecret, "password-secret", "", "挂载密码所在机密项的名字")
	c.Flags().StringVar(&drive, "drive", "", "Windows 固定盘符，取值 D 到 Z")
	c.Flags().StringArrayVar(&directories, "dir", nil, "目录约定，格式为 用途=子路径，可重复")
	c.Flags().StringVar(&note, "note", "", "备注")
	_ = c.MarkFlagRequired("device")
	_ = c.MarkFlagRequired("user")
	_ = c.MarkFlagRequired("password-secret")
	_ = c.MarkFlagRequired("drive")
	return c
}

func sharesListCmd() *cobra.Command {
	var device, server, instance string
	c := &cobra.Command{
		Use:   "list [--device 设备] [--server 条目] [--instance <name>]",
		Args:  cobra.NoArgs,
		Short: "List registered shared storage",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeStore, err := openTopologyViewStore(instance)
			if err != nil {
				return err
			}
			defer closeStore()
			shares, err := s.ListSharedStorages(store.SharedStorageFilter{Device: device, Server: server})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(shares) == 0 {
				fmt.Fprintln(out, "没有匹配的共享存储")
				return nil
			}
			for _, share := range shares {
				printSharedStorageSummary(out, share)
			}
			return nil
		},
	}
	c.Flags().StringVar(&device, "device", "", "只显示这台提供方设备提供的共享存储")
	c.Flags().StringVar(&server, "server", "", "只显示这台服务器条目可挂载的共享存储")
	c.Flags().StringVar(&instance, "instance", "", topologyInstanceFlagHelp)
	return c
}

func sharesEditCmd() *cobra.Command {
	var name, device, user, passwordSecret, drive, note, changeNote string
	var directories []string
	var clearDirectories bool
	c := &cobra.Command{
		Use:   "edit NAME [--name 新共享名] [--device 设备] [--user 用户名] [--password-secret 机密项] [--drive 盘符] [--dir 用途=子路径 ... | --clear-dirs] [--note 文本] [--change-note 文本]",
		Args:  cobra.ExactArgs(1),
		Short: "Edit registered shared storage with partial-update semantics",
		RunE: func(cmd *cobra.Command, args []string) error {
			changedDirectories := cmd.Flags().Changed("dir")
			if changedDirectories && clearDirectories {
				return errors.New("--dir 与 --clear-dirs 互斥：前者整体替换目录约定，后者清空目录约定")
			}
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			changes := store.SharedStorageChanges{}
			if cmd.Flags().Changed("name") {
				changes.Name = &name
			}
			if cmd.Flags().Changed("device") {
				changes.Device = &device
			}
			if cmd.Flags().Changed("user") {
				changes.User = &user
			}
			if cmd.Flags().Changed("password-secret") {
				changes.PasswordSecret = &passwordSecret
			}
			if cmd.Flags().Changed("drive") {
				changes.Drive = &drive
			}
			if changedDirectories || clearDirectories {
				parsed := []models.SharedStorageDirectory{}
				if changedDirectories {
					parsed, err = parseSharedStorageDirectories(directories)
					if err != nil {
						return err
					}
				}
				changes.Directories = &parsed
			}
			if cmd.Flags().Changed("note") {
				changes.Note = &note
			}
			if cmd.Flags().Changed("change-note") {
				changes.ChangeNote = &changeNote
			}
			share, err := s.EditSharedStorage(args[0], changes, store.CLISource())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已修改共享存储 %s；版本号：%d\n", share.Name, share.Version)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "新的共享名")
	c.Flags().StringVar(&device, "device", "", "新的提供方设备名字")
	c.Flags().StringVar(&user, "user", "", "新的专用挂载账号用户名")
	c.Flags().StringVar(&passwordSecret, "password-secret", "", "新的挂载密码机密项名字")
	c.Flags().StringVar(&drive, "drive", "", "新的 Windows 固定盘符")
	c.Flags().StringArrayVar(&directories, "dir", nil, "新的目录约定，格式为 用途=子路径，可重复；给出时整体替换")
	c.Flags().BoolVar(&clearDirectories, "clear-dirs", false, "清空目录约定（与 --dir 互斥）")
	c.Flags().StringVar(&note, "note", "", "新的备注；传空字符串清空")
	c.Flags().StringVar(&changeNote, "change-note", "", "本次修改的变更说明；不给则清空旧说明")
	return c
}

func sharesShowCmd() *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "show NAME [--instance <name>]",
		Args:  cobra.ExactArgs(1),
		Short: "Show one registered shared storage",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeStore, err := openTopologyViewStore(instance)
			if err != nil {
				return err
			}
			defer closeStore()
			share, err := s.GetSharedStorage(args[0])
			if err != nil {
				return err
			}
			printSharedStorageDetail(cmd.OutOrStdout(), share)
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", topologyInstanceFlagHelp)
	return c
}

func sharesRmCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "rm NAME [--yes]",
		Args:  cobra.ExactArgs(1),
		Short: "Remove a registered shared storage after confirmation",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			share, err := s.GetSharedStorage(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "将删除共享存储 %s。\n", share.Name)
			fmt.Fprintln(out, "可挂载服务器：")
			printSharedStorageServers(out, share.MountableServers, "  ")
			fmt.Fprintln(out, "本命令只删除登记，不会去服务器上卸载。")
			if !yes {
				if !sharesStdinIsInteractive() {
					return errors.New("非交互终端不能确认删除共享存储；请加 --yes 后重跑")
				}
				fmt.Fprint(out, "输入 y 确认删除：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取删除确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未删除共享存储。")
					return nil
				}
			}
			deleted, err := s.DeleteSharedStorage(share.Name, store.CLISource())
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "已删除共享存储 %s\n", deleted.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "跳过删除确认；非交互终端必须显式给出")
	return c
}

func sharesUnmountCmd() *cobra.Command {
	var force, yes bool
	c := &cobra.Command{
		Use:   "unmount SHARE SERVER [--force] [--yes]",
		Args:  cobra.ExactArgs(2),
		Short: "Remove one shared storage mount from a Linux or macOS server",
		Long: `在一台 Linux 或 macOS 服务器上删除共享存储的持久挂载配置，再卸载挂载点。

服务器仍在可挂载范围内时，agent 之后仍可能重新挂载；彻底撤下授权请使用 shares revoke。
服务器已移出范围或共享登记已删除时，命令会留下不含机密的撤销标记。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "将卸载共享存储 %s 在服务器 %s 上的配置与挂载。\n", args[0], args[1])
			if force {
				fmt.Fprintln(out, "被占用时将强制卸载。")
			}
			if !yes {
				if !sharesStdinIsInteractive() {
					return errors.New("非交互终端不能确认共享存储卸载；请加 --yes 后重跑")
				}
				fmt.Fprint(out, "输入 y 确认共享存储卸载：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取共享存储卸载确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未卸载共享存储。")
					return nil
				}
			}
			result, err := sharemanage.New(s).Unmount(cmd.Context(), sharemanage.UnmountRequest{
				Share: args[0], Server: args[1], Force: force,
			})
			if err != nil {
				return err
			}
			if result.Result == "success" {
				fmt.Fprintf(out, "共享存储 %s 已从服务器 %s 卸载；版本号：%d\n",
					result.ShareName, result.ServerName, result.Version)
			} else {
				fmt.Fprintf(out, "共享存储 %s 的配置已删除，但服务器 %s 上挂载被占用并保留；结果：%s；版本号：%d\n",
					result.ShareName, result.ServerName, result.Result, result.Version)
			}
			for _, notice := range result.Notices {
				fmt.Fprintf(out, "提示：%s\n", notice)
			}
			for _, process := range result.BusyProcesses {
				fmt.Fprintf(out, "占用进程：%s %s %s\n", process.ProcessID, process.User, process.Command)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "被占用时强制卸载")
	c.Flags().BoolVar(&yes, "yes", false, "跳过共享存储卸载确认；非交互终端必须显式给出")
	return c
}

func parseSharedStorageDirectories(values []string) ([]models.SharedStorageDirectory, error) {
	out := make([]models.SharedStorageDirectory, 0, len(values))
	for _, value := range values {
		purpose, path, found := strings.Cut(value, "=")
		if !found {
			return nil, fmt.Errorf("目录约定 %q 缺少等号，格式为 用途=子路径", value)
		}
		out = append(out, models.SharedStorageDirectory{Purpose: purpose, Path: path})
	}
	return out, nil
}

func printSharedStorageSummary(out io.Writer, share *models.SharedStorage) {
	fmt.Fprintf(out, "共享存储 %s — 提供方设备 %s，盘符 %s，版本号 %d，可挂载服务器数量 %d\n",
		share.Name, share.DeviceName, share.Drive, share.Version, len(share.MountableServers))
}

func printSharedStorageDetail(out io.Writer, share *models.SharedStorage) {
	fmt.Fprintf(out, "共享存储 %s — 版本号：%d\n", share.Name, share.Version)
	fmt.Fprintf(out, "  提供方设备：%s\n", share.DeviceName)
	fmt.Fprintf(out, "  挂载账号用户名：%s\n", share.MountUser)
	fmt.Fprintf(out, "  挂载密码机密项：%s\n", share.PasswordSecretName)
	fmt.Fprintf(out, "  盘符：%s\n", share.Drive)
	if len(share.CandidateAddresses) == 0 {
		fmt.Fprintln(out, "  候选访问地址：无（该设备没有网络成员，暂时无法挂载）")
	} else {
		fmt.Fprintf(out, "  候选访问地址：%s\n", strings.Join(share.CandidateAddresses, "、"))
	}
	fmt.Fprintln(out, "  目录约定：")
	if len(share.Directories) == 0 {
		fmt.Fprintln(out, "    无")
	} else {
		for _, directory := range share.Directories {
			fmt.Fprintf(out, "    %s=%s\n", directory.Purpose, directory.Path)
		}
	}
	if share.Note == "" {
		fmt.Fprintln(out, "  备注：无")
	} else {
		fmt.Fprintf(out, "  备注：%s\n", share.Note)
	}
	if share.ChangeNote == "" {
		fmt.Fprintln(out, "  变更说明：无")
	} else {
		fmt.Fprintf(out, "  变更说明：%s\n", share.ChangeNote)
	}
	fmt.Fprintln(out, "  可挂载服务器：")
	printSharedStorageServers(out, share.MountableServers, "    ")
}

func printSharedStorageServers(out io.Writer, servers []models.SharedStorageServer, indent string) {
	if len(servers) == 0 {
		fmt.Fprintln(out, indent+"无")
		return
	}
	for _, server := range servers {
		fmt.Fprintf(out, "%s%s（挂载点 %s）\n", indent, server.Name, server.MountPoint)
	}
}
