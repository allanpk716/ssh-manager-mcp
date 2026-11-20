package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/softwareinventory"
)

// inventoryExportConditions 是导出命令与查询命令共享的读取条件；空值表
// 示不限制。
type inventoryExportConditions struct {
	Device       string
	Name         string
	ExactVersion string
	Site         string
	Network      string
}

// ExportSoftwareInventory 按命令行可见范围查询软件清单。系统包必须包含
// 在导出里；客户端走同一条在线查询链路，不读本地缓存。
func (s *inventoryQuerySource) ExportSoftwareInventory(
	conditions inventoryExportConditions,
) (softwareinventory.QueryResult, error) {
	if s.local != nil {
		result, err := softwareinventory.New(s.local).Query(softwareinventory.QueryOptions{
			Device:                conditions.Device,
			Name:                  conditions.Name,
			ExactVersion:          conditions.ExactVersion,
			Site:                  conditions.Site,
			Network:               conditions.Network,
			IncludeSystemPackages: true,
			Visibility:            softwareinventory.AllDevicesVisible(),
		})
		if err != nil {
			return softwareinventory.QueryResult{}, err
		}
		result.ProfileName = "owner"
		return result, nil
	}
	result, err := s.forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Device: conditions.Device, Name: conditions.Name,
		ExactVersion: conditions.ExactVersion, Site: conditions.Site,
		Network: conditions.Network, IncludeSystemPackages: true,
	})
	if err != nil {
		return softwareinventory.QueryResult{}, s.mapError(err)
	}
	return result, nil
}

func inventoryExportCmd() *cobra.Command {
	var format, device, name, exactVersion, site, network, outputPath, instance string
	c := &cobra.Command{
		Use:   "export --format csv|cyclonedx [--device 设备] [--name 软件名] [--version 精确版本] [--site 地点路径] [--network 网络] [--output 文件]",
		Args:  cobra.NoArgs,
		Short: "Export the software inventory",
		Long: `导出软件清单。--format 必须显式写为 csv 或 cyclonedx；csv 是带
字节顺序标记的平铺总表，每个软件条目一行。软件名、精确版本、地点、网络
与设备条件沿用查询语义；导出始终包含系统包。客户端经设备码在线查询权
威端；权威端不可达时使用退出码 4。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "csv" && format != "cyclonedx" {
				return fmt.Errorf("导出格式不合法；只支持 --format csv 或 --format cyclonedx")
			}
			source, err := openInventoryQuerySource(instance)
			if err != nil {
				return err
			}
			defer source.Close()
			result, err := source.ExportSoftwareInventory(inventoryExportConditions{
				Device: device, Name: name, ExactVersion: exactVersion,
				Site: site, Network: network,
			})
			if err != nil {
				return err
			}
			var document []byte
			if format == "csv" {
				document, err = softwareinventory.MarshalCSV(result)
			} else {
				document, err = softwareinventory.MarshalCycloneDX(result, softwareinventory.CycloneDXExportOptions{
					GeneratedAt: time.Now().UTC(), SSHManagerVersion: buildinfo.Version,
					ProfileName: result.ProfileName,
				})
			}
			if err != nil {
				return err
			}
			if outputPath == "" {
				_, err = cmd.OutOrStdout().Write(document)
				return err
			}
			return os.WriteFile(outputPath, document, 0o600)
		},
	}
	c.Flags().StringVar(&format, "format", "", "导出格式；必须显式写为 csv 或 cyclonedx")
	c.Flags().StringVar(&device, "device", "", "只导出一台设备；默认导出全部可见设备")
	c.Flags().StringVar(&name, "name", "", "软件名子串；与查询命令一致")
	c.Flags().StringVar(&exactVersion, "version", "", "精确版本；去除首尾空白后逐字相等")
	c.Flags().StringVar(&site, "site", "", "地点路径；包含该地点及全部下级地点")
	c.Flags().StringVar(&network, "network", "", "网络名字；可选物理局域网或虚拟局域网")
	c.Flags().StringVar(&outputPath, "output", "", "写入文件；默认写到标准输出")
	c.Flags().StringVar(&instance, "instance", "", "客户端的命名缓存实例（默认槽省略；与 SSHMGR_CACHE_DIR 互斥）")
	_ = c.MarkFlagRequired("format")
	return c
}
