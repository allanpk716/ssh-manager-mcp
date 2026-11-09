package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/tui"
)

var (
	errInventoryNeedVault = errors.New("本机还没有 vault：软件清单随 vault 一起存放，请先运行初始化向导（直接执行 sshmgr）")
	errInventoryClient    = errors.New("本机是缓存客户端：软件清单在权威端维护，请在权威端执行这条命令")
)

// inventoryStdinIsInteractive 判断进程标准输入是不是交互终端。测试可以替
// 换它来驱动交互与非交互两种删除确认路径。
var inventoryStdinIsInteractive = func() bool {
	return tui.IsTerminal(os.Stdin.Fd())
}

// requireInventoryVault 断言软件清单命令能写权威端或单机 vault。客户端没有
// 本地软件清单副本，后续在线查询票会为只读命令接上权威端接口。
func requireInventoryVault() (*store.Store, error) {
	return requireOwnerVaultForCommand(errInventoryNeedVault, errInventoryClient)
}

func newInventoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inventory",
		Short: "Manage and query the software inventory",
		Long: `管理与查询软件清单。写命令只在权威端与单机可用；查看（view）与搜索
（search）在客户端上经设备码在线查询权威端，不会读取本地缓存数据。

退出码：0 成功；1 一般错误；4 权威端不可达（仅客户端的查看、搜索与导
出）；5 部分设备采集失败（仅 collect-assigned）。`,
	}
	cmd.AddCommand(
		inventoryAddCmd(), inventoryEditCmd(), inventoryRmCmd(),
		inventoryClaimCmd(), inventoryUnclaimCmd(),
		inventoryViewCmd(), inventorySearchCmd(),
		inventoryTodoCmd(),
		inventoryExportCmd(),
		inventoryCollectCmd(), inventorySudoCmd(), inventoryCollectorCmd(),
		inventoryHistoryCmd(), inventoryStatusCmd(),
		inventoryCollectAssignedCmd(),
	)
	return cmd
}

func inventoryAddCmd() *cobra.Command {
	var category, name, version, packageURL, cpe, belongsTo, note string
	var listens []string
	c := &cobra.Command{
		Use:   "add DEVICE --category 类别 --name 名称 --version 版本",
		Args:  cobra.ExactArgs(1),
		Short: "Register a declared software item",
		Long: `登记一条声明。版本按输入原样保存；类别可用值：
operating_system、service_program、container_image、driver_firmware、system_package。

监听端口参数可重复，格式为“监听地址,端口,协议,暴露级别”。协议可用 tcp 或 udp；
暴露级别可用 local_machine、specific_address、all_addresses、local_area_network、
virtual_local_area_network、public_network。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ports, err := parseInventoryListens(listens)
			if err != nil {
				return err
			}
			parsedCategory, err := parseInventoryCategory(category)
			if err != nil {
				return err
			}
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			id, err := softwareinventory.New(s).AddDeclaration(softwareinventory.Declaration{
				Device: args[0], Category: parsedCategory, Name: name, Version: version,
				PackageURL: packageURL, CPE: cpe, ListeningPorts: ports,
				BelongsTo: belongsTo, Note: note,
			}, softwareinventory.CLISource())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已登记软件声明 %s\n", id)
			return nil
		},
	}
	c.Flags().StringVar(&category, "category", "", "软件类别")
	c.Flags().StringVar(&name, "name", "", "软件名字")
	c.Flags().StringVar(&version, "version", "", "精确版本；原样保存")
	c.Flags().StringVar(&packageURL, "purl", "", "软件包统一地址（可选）")
	c.Flags().StringVar(&cpe, "cpe", "", "美国国家漏洞库产品命名（可选）")
	c.Flags().StringArrayVar(&listens, "listen", nil, "监听端口：监听地址,端口,协议,暴露级别；可重复")
	c.Flags().StringVar(&belongsTo, "belongs-to", "", "所属（例如容器引擎或业务系统）")
	c.Flags().StringVar(&note, "note", "", "备注")
	_ = c.MarkFlagRequired("category")
	_ = c.MarkFlagRequired("name")
	_ = c.MarkFlagRequired("version")
	return c
}

func inventoryEditCmd() *cobra.Command {
	var device, category, name, version, packageURL, cpe, belongsTo, note string
	var listens []string
	var clearListens bool
	c := &cobra.Command{
		Use:   "edit ID [--device 设备] [--category 类别] [--name 名称] [--version 版本] ...",
		Args:  cobra.ExactArgs(1),
		Short: "Edit a declared software item",
		RunE: func(cmd *cobra.Command, args []string) error {
			var ports []softwareinventory.ListeningPort
			if cmd.Flags().Changed("listen") {
				parsed, err := parseInventoryListens(listens)
				if err != nil {
					return err
				}
				ports = parsed
			}
			edit := softwareinventory.DeclarationEdit{}
			if cmd.Flags().Changed("device") {
				edit.Device = &device
			}
			if cmd.Flags().Changed("category") {
				parsed, err := parseInventoryCategory(category)
				if err != nil {
					return err
				}
				edit.Category = &parsed
			}
			if cmd.Flags().Changed("name") {
				edit.Name = &name
			}
			if cmd.Flags().Changed("version") {
				edit.Version = &version
			}
			if cmd.Flags().Changed("purl") {
				edit.PackageURL = &packageURL
			}
			if cmd.Flags().Changed("cpe") {
				edit.CPE = &cpe
			}
			if cmd.Flags().Changed("listen") || clearListens {
				edit.ListeningPorts = &ports
			}
			if cmd.Flags().Changed("belongs-to") {
				edit.BelongsTo = &belongsTo
			}
			if cmd.Flags().Changed("note") {
				edit.Note = &note
			}
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := softwareinventory.New(s).EditDeclaration(args[0], edit, softwareinventory.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已修改软件声明 %s\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&device, "device", "", "新的设备名字")
	c.Flags().StringVar(&category, "category", "", "新的软件类别")
	c.Flags().StringVar(&name, "name", "", "新的软件名字")
	c.Flags().StringVar(&version, "version", "", "新的精确版本；原样保存")
	c.Flags().StringVar(&packageURL, "purl", "", "新的软件包统一地址；传空清除")
	c.Flags().StringVar(&cpe, "cpe", "", "新的美国国家漏洞库产品命名；传空清除")
	c.Flags().StringArrayVar(&listens, "listen", nil, "新的监听端口集合；整体替换既有端口")
	c.Flags().BoolVar(&clearListens, "clear-listen", false, "清除全部监听端口")
	c.Flags().StringVar(&belongsTo, "belongs-to", "", "新的所属；传空清除")
	c.Flags().StringVar(&note, "note", "", "新的备注；传空清除")
	return c
}

func inventoryRmCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "rm ID [--yes]",
		Args:  cobra.ExactArgs(1),
		Short: "Remove a declared software item after confirmation",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			out := cmd.OutOrStdout()
			if !yes {
				if !inventoryStdinIsInteractive() {
					return errors.New("非交互终端不能确认删除软件声明；请加 --yes 后重跑")
				}
				fmt.Fprint(out, "输入 y 确认删除软件声明：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取删除确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未删除软件声明。")
					return nil
				}
			}
			if err := softwareinventory.New(s).DeleteDeclaration(args[0], softwareinventory.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(out, "已删除软件声明 %s\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "跳过删除确认；非交互终端必须显式给出")
	return c
}

func inventoryViewCmd() *cobra.Command {
	var jsonOutput bool
	var allSystemPackages bool
	var instance string
	c := &cobra.Command{
		Use:   "view DEVICE [--all-system-packages] [--json]",
		Args:  cobra.ExactArgs(1),
		Short: "Show one device's software inventory by category",
		RunE: func(cmd *cobra.Command, args []string) error {
			source, err := openInventoryQuerySource(instance)
			if err != nil {
				return err
			}
			defer source.Close()
			inventory, err := source.ViewDevice(args[0], allSystemPackages)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeInventoryGroupsJSON(cmd.OutOrStdout(), inventory)
			}
			printDeviceInventory(cmd.OutOrStdout(), inventory)
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "客户端的命名缓存实例（默认槽省略；与 SSHMGR_CACHE_DIR 互斥）")
	c.Flags().BoolVar(&allSystemPackages, "all-system-packages", false, "列出全部系统包；默认只显示数量")
	c.Flags().BoolVar(&jsonOutput, "json", false, "以 JSON Lines 输出；每个类别分组一行")
	return c
}

func inventorySearchCmd() *cobra.Command {
	var jsonOutput bool
	var instance string
	c := &cobra.Command{
		Use:   "search NAME [--json]",
		Args:  cobra.ExactArgs(1),
		Short: "Search software by name across devices",
		RunE: func(cmd *cobra.Command, args []string) error {
			source, err := openInventoryQuerySource(instance)
			if err != nil {
				return err
			}
			defer source.Close()
			results, err := source.SearchSoftware(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOutput {
				for _, result := range results {
					encoder := json.NewEncoder(out)
					if err := encoder.Encode(result); err != nil {
						return err
					}
				}
				return nil
			}
			if len(results) == 0 {
				fmt.Fprintln(out, "没有匹配的软件条目。")
				return nil
			}
			for _, result := range results {
				fmt.Fprintf(out, "%s：", result.DeviceName)
				printInventoryItem(out, result.Item)
			}
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "客户端的命名缓存实例（默认槽省略；与 SSHMGR_CACHE_DIR 互斥）")
	c.Flags().BoolVar(&jsonOutput, "json", false, "以 JSON Lines 输出；每条结果一行")
	return c
}

func parseInventoryCategory(value string) (softwareinventory.Category, error) {
	category := softwareinventory.Category(strings.ReplaceAll(value, "-", "_"))
	switch category {
	case softwareinventory.CategoryOperatingSystem,
		softwareinventory.CategoryServiceProgram,
		softwareinventory.CategoryContainerImage,
		softwareinventory.CategoryDriverFirmware,
		softwareinventory.CategorySystemPackage:
		return category, nil
	default:
		return "", errors.New("软件类别不合法；可用值见 inventory add --help")
	}
}

func parseInventoryListens(values []string) ([]softwareinventory.ListeningPort, error) {
	ports := make([]softwareinventory.ListeningPort, 0, len(values))
	for _, value := range values {
		fields := strings.Split(value, ",")
		if len(fields) != 4 {
			return nil, errors.New("监听端口格式不合法；应为 监听地址,端口,协议,暴露级别")
		}
		port, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, errors.New("监听端口格式不合法；端口必须是数字")
		}
		ports = append(ports, softwareinventory.ListeningPort{
			Address: fields[0], Port: port, Protocol: fields[2],
			Exposure: softwareinventory.Exposure(strings.ReplaceAll(fields[3], "-", "_")),
		})
	}
	return ports, nil
}

type inventoryGroupJSON struct {
	DeviceName string `json:"device_name"`
	softwareinventory.ItemGroup
}

func writeInventoryGroupsJSON(w io.Writer, inventory *softwareinventory.DeviceInventory) error {
	for _, group := range inventory.Groups {
		encoder := json.NewEncoder(w)
		if err := encoder.Encode(inventoryGroupJSON{DeviceName: inventory.DeviceName, ItemGroup: group}); err != nil {
			return err
		}
	}
	return nil
}

func printDeviceInventory(w io.Writer, inventory *softwareinventory.DeviceInventory) {
	fmt.Fprintf(w, "设备 %s\n", inventory.DeviceName)
	for _, group := range inventory.Groups {
		if group.Category == softwareinventory.CategorySystemPackage && group.SystemPackagesOmitted {
			fmt.Fprintf(w, "%s：%d 条（默认省略；加 --all-system-packages 查看）\n",
				group.CategoryLabel, group.SystemPackageCount)
			continue
		}
		fmt.Fprintf(w, "%s：%d 条\n", group.CategoryLabel, len(group.Items))
		for _, item := range group.Items {
			printInventoryItem(w, item)
		}
	}
}

func printInventoryItem(w io.Writer, item softwareinventory.Item) {
	fmt.Fprintf(w, "%s %s（来源：%s）\n", item.Name, item.Version, item.Source)
	if item.VersionMismatch {
		fmt.Fprintf(w, "  声明版本不一致：%s；已按实测版本显示\n", item.DeclaredVersion)
	}
	if item.Mismatch {
		fmt.Fprintf(w, "  对不上：声明没有认领实测条目\n")
	}
	if item.PackageURL != "" {
		fmt.Fprintf(w, "  软件包统一地址：%s\n", item.PackageURL)
	}
	if item.CPE != "" {
		fmt.Fprintf(w, "  美国国家漏洞库产品命名：%s\n", item.CPE)
	}
	for _, port := range item.ListeningPorts {
		fmt.Fprintf(w, "  监听端口：%s:%d/%s（%s）\n",
			port.Address, port.Port, port.Protocol, port.Exposure.Label())
	}
	if item.BelongsTo != "" {
		fmt.Fprintf(w, "  所属：%s\n", item.BelongsTo)
	}
	if item.Note != "" {
		fmt.Fprintf(w, "  备注：%s\n", item.Note)
	}
}
