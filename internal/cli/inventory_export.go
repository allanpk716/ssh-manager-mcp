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

// ExportSoftwareInventory 按命令行可见范围查询全量软件清单。系统包必须
// 包含在导出里；客户端走同一条在线查询链路，不读本地缓存。
func (s *inventoryQuerySource) ExportSoftwareInventory(device string) (softwareinventory.QueryResult, error) {
	if s.local != nil {
		result, err := softwareinventory.New(s.local).Query(softwareinventory.QueryOptions{
			Device:                device,
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
		Device: device, IncludeSystemPackages: true,
	})
	if err != nil {
		return softwareinventory.QueryResult{}, s.mapError(err)
	}
	return result, nil
}

func inventoryExportCmd() *cobra.Command {
	var format, device, outputPath, instance string
	c := &cobra.Command{
		Use:   "export --format cyclonedx [--device 设备] [--output 文件]",
		Args:  cobra.NoArgs,
		Short: "Export the software inventory",
		Long: `导出软件清单。--format 目前只支持 cyclonedx，并且必须显式给出；
导出始终包含系统包。客户端经设备码在线查询权威端；权威端不可达时使用
退出码 4。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "cyclonedx" {
				return fmt.Errorf("导出格式不合法；目前只支持 --format cyclonedx")
			}
			source, err := openInventoryQuerySource(instance)
			if err != nil {
				return err
			}
			defer source.Close()
			result, err := source.ExportSoftwareInventory(device)
			if err != nil {
				return err
			}
			document, err := softwareinventory.MarshalCycloneDX(result, softwareinventory.CycloneDXExportOptions{
				GeneratedAt: time.Now().UTC(), SSHManagerVersion: buildinfo.Version,
				ProfileName: result.ProfileName,
			})
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
	c.Flags().StringVar(&format, "format", "", "导出格式；当前只支持 cyclonedx")
	c.Flags().StringVar(&device, "device", "", "只导出一台设备；默认导出全部可见设备")
	c.Flags().StringVar(&outputPath, "output", "", "写入文件；默认写到标准输出")
	c.Flags().StringVar(&instance, "instance", "", "客户端的命名缓存实例（默认槽省略；与 SSHMGR_CACHE_DIR 互斥）")
	_ = c.MarkFlagRequired("format")
	return c
}
