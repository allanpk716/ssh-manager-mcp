package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientfeatures"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/termdetect"
)

// errTopologyNeedVault 是「本机既没有 vault 也没有客户端缓存」的统一引导。
var errTopologyNeedVault = errors.New("本机还没有 vault：网络拓扑随 vault 一起存放，请先运行初始化向导（直接执行 sshmgr）")

// topologyStdinIsInteractive 判断标准输入是不是交互终端。删除网络需要确认；脚本
// 场景没有交互终端，必须显式给 --yes。
var topologyStdinIsInteractive = func() bool {
	return termdetect.IsTerminal(os.Stdin.Fd())
}

// topologyRoleFor 沿用机密项命令的角色判定链：vault 优先，其次看客户端
// 缓存登记；两者都没有时给出初始化引导。
func topologyRoleFor(instance string) (policyRole, error) {
	return roleForVaultBackedCommand(instance, errTopologyNeedVault)
}

// refuseWhenClientCacheAuthorityTooOld 是命令行拓扑与共享存储查询的统一
// 旧权威端降级入口。
func refuseWhenClientCacheAuthorityTooOld(s *store.Store) error {
	if s.CacheAuthorityTooOld() {
		return clientfeatures.ErrTopologyAndSharedStorageAuthorityTooOld
	}
	return nil
}

// requireTopologyVault 断言本机持有 vault 并返回已解锁的存储。客户端上的
// 拓扑写命令必须到权威端执行。
func requireTopologyVault() (*store.Store, error) {
	return requireOwnerVaultForCommand(
		errTopologyNeedVault,
		errors.New("本机是缓存客户端：网络拓扑在权威端维护——请在权威端执行这条命令"),
	)
}

// openTopologyViewStore 打开拓扑查看命令的数据源：权威端与单机部署读
// vault；客户端把本地缓存快照导入一个一次性临时库后只读查询，复用存储
// 层既有的筛选与排序语义。返回的清理函数负责关闭并删除临时库及其附属
// 文件。
func openTopologyViewStore(instance string) (*store.Store, func(), error) {
	role, err := topologyRoleFor(instance)
	if err != nil {
		return nil, nil, err
	}
	if role != policyRoleClient {
		s, err := openUnlockedStore()
		if err != nil {
			return nil, nil, err
		}
		return s, func() { s.Close() }, nil
	}

	snapshot, err := loadTopologyClientSnapshot(instance)
	if err != nil {
		return nil, nil, err
	}
	temporary, err := store.OpenTemporaryClientCacheStore(snapshot, "sshmgr-topology-*.db")
	if err != nil {
		return nil, nil, err
	}
	temporary.Store.SetCacheRecommendedVersion(snapshot.RecommendedVersion)
	temporary.Store.SetReadOnly(nil)
	return temporary.Store, func() { _ = temporary.Close() }, nil
}

// topologyInstanceFlagHelp 是四个拓扑查看命令的 --instance 说明。
const topologyInstanceFlagHelp = "在客户端上读取命名实例 instances/<名称> 的缓存；省略时读默认缓存槽；只有命名实例时必须指定；与 SSHMGR_CACHE_DIR、SSHMGR_CACHE_DEK 互斥"

// loadTopologyClientSnapshot 按命令策略查看命令的同一规则选择客户端缓存
// 实例：点名实例就只读该实例；默认缓存槽为空而本机有命名实例时报错并列
// 出实例名，绝不替用户猜。拓扑查看必须有快照，读失败时不像命令策略那样
// 退回内置默认规则，而是保留原有的缓存拉取指引。
func loadTopologyClientSnapshot(instance string) (*store.Snapshot, error) {
	if instance != "" {
		snapshot, err := clientops.LoadCacheSnapshotFor(instance)
		if err != nil {
			return nil, fmt.Errorf("cache instance %q: %w", instance, err)
		}
		return snapshot, nil
	}
	if os.Getenv("SSHMGR_CACHE_DIR") == "" && !cachePresentFor("") {
		if names, listErr := clientops.ListInstances(); listErr == nil && len(names) > 0 {
			return nil, fmt.Errorf(
				"no cache in the default slot, but %d named client instance(s) exist: %s — pass --instance <name> to read topology (the instance is never guessed)",
				len(names), strings.Join(names, ", "))
		}
	}
	snapshot, err := clientops.LoadCacheSnapshotFor("")
	if err != nil {
		return nil, fmt.Errorf("本机客户端缓存不可读：请先执行 cache pull，再查看网络拓扑（%w）", err)
	}
	return snapshot, nil
}

func newSitesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sites", Short: "Manage network topology sites"}
	cmd.AddCommand(sitesAddCmd(), sitesEditCmd(), sitesRmCmd(), sitesListCmd())
	return cmd
}

func sitesAddCmd() *cobra.Command {
	var note string
	c := &cobra.Command{
		Use:   "add NAME [--note 文本]",
		Args:  cobra.ExactArgs(1),
		Short: "Register a topology site",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			site, err := s.AddSite(args[0], note)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已登记站点 %s\n", site.Name)
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "备注")
	return c
}

func sitesEditCmd() *cobra.Command {
	var name, note string
	c := &cobra.Command{
		Use:   "edit NAME [--name 新名字] [--note 新备注]",
		Args:  cobra.ExactArgs(1),
		Short: "Edit a topology site with partial-update semantics",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			changes := store.SiteChanges{}
			if cmd.Flags().Changed("name") {
				changes.Name = &name
			}
			if cmd.Flags().Changed("note") {
				changes.Note = &note
			}
			site, err := s.EditSite(args[0], changes)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已修改站点 %s；新名字 %s\n", args[0], site.Name)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "新名字")
	c.Flags().StringVar(&note, "note", "", "新备注；传空字符串清空")
	return c
}

func sitesRmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rm NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Remove an empty topology site",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.DeleteSite(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已删除站点 %s\n", args[0])
			return nil
		},
	}
	return c
}

func sitesListCmd() *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "list [--instance <name>]",
		Args:  cobra.NoArgs,
		Short: "List topology sites and their physical networks",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeStore, err := openTopologyViewStore(instance)
			if err != nil {
				return err
			}
			defer closeStore()
			if err := refuseWhenClientCacheAuthorityTooOld(s); err != nil {
				return err
			}
			sites, err := s.ListSites()
			if err != nil {
				return err
			}
			physicalNetworks, err := s.ListNetworks(store.NetworkFilter{Type: models.NetworkPhysical})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(sites) == 0 {
				fmt.Fprintln(out, "还没有登记站点")
				return nil
			}
			networksBySite := make(map[string][]*models.Network)
			for _, network := range physicalNetworks {
				networksBySite[network.SiteID] = append(networksBySite[network.SiteID], network)
			}
			for _, site := range sites {
				note := site.Note
				if note == "" {
					note = "（无备注）"
				}
				fmt.Fprintf(out, "站点 %s — %s\n", site.Name, note)
				networks := networksBySite[site.ID]
				if len(networks) == 0 {
					fmt.Fprintln(out, "  物理局域网：无")
					continue
				}
				for _, network := range networks {
					fmt.Fprintf(out, "  物理局域网：%s", network.Name)
					if network.NetworkRange != "" {
						fmt.Fprintf(out, "（网段 %s）", network.NetworkRange)
					}
					fmt.Fprintln(out)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", topologyInstanceFlagHelp)
	return c
}

func newNetworksCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "networks", Short: "Manage topology networks"}
	cmd.AddCommand(networksAddCmd(), networksEditCmd(), networksRmCmd(), networksListCmd())
	return cmd
}

func networksAddCmd() *cobra.Command {
	var (
		networkType  string
		site         string
		note         string
		networkRange string
		bandwidth    int64
		metered      bool
	)
	c := &cobra.Command{
		Use:   "add NAME --type physical|virtual [--site 站点] [--note 文本] [--cidr 网段] [--bandwidth-mbps 带宽] [--metered]",
		Args:  cobra.ExactArgs(1),
		Short: "Register a physical or virtual network",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			parsedType, err := models.ParseNetworkType(networkType)
			if err != nil {
				return err
			}
			network, err := s.AddNetwork(models.Network{
				Name: args[0], Type: parsedType, SiteName: site, Note: note,
				NetworkRange: networkRange, BandwidthMbps: bandwidth, Metered: metered,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已登记网络 %s（%s）\n", network.Name, networkTypeLabel(network.Type))
			return nil
		},
	}
	c.Flags().StringVar(&networkType, "type", "", "网络类型：physical（物理局域网）或 virtual（虚拟局域网）")
	c.Flags().StringVar(&site, "site", "", "所属站点；物理局域网必填，虚拟局域网不能使用")
	c.Flags().StringVar(&note, "note", "", "备注")
	c.Flags().StringVar(&networkRange, "cidr", "", "网段；只给人看，不参与判断")
	c.Flags().Int64Var(&bandwidth, "bandwidth-mbps", 0, "标称带宽，单位 Mbit/s；0 表示未填写")
	c.Flags().BoolVar(&metered, "metered", false, "是否按流量计费")
	_ = c.MarkFlagRequired("type")
	return c
}

func networksEditCmd() *cobra.Command {
	var (
		networkType  string
		name         string
		site         string
		note         string
		networkRange string
		bandwidth    int64
		metered      bool
	)
	c := &cobra.Command{
		Use:   "edit NAME [--name 新名字] [--site 站点] [--note 新备注] [--cidr 新网段] [--bandwidth-mbps 新带宽] [--metered=true|false]",
		Args:  cobra.ExactArgs(1),
		Short: "Edit a network with partial-update semantics; the type is immutable",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if cmd.Flags().Changed("type") {
				return errors.New("网络类型登记后不能修改；如需改成另一类网络，请删除后重建")
			}
			changes := store.NetworkChanges{}
			if cmd.Flags().Changed("name") {
				changes.Name = &name
			}
			if cmd.Flags().Changed("site") {
				changes.Site = &site
			}
			if cmd.Flags().Changed("note") {
				changes.Note = &note
			}
			if cmd.Flags().Changed("cidr") {
				changes.NetworkRange = &networkRange
			}
			if cmd.Flags().Changed("bandwidth-mbps") {
				changes.BandwidthMbps = &bandwidth
			}
			if cmd.Flags().Changed("metered") {
				changes.Metered = &metered
			}
			network, err := s.EditNetwork(args[0], changes)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已修改网络 %s；新名字 %s\n", args[0], network.Name)
			return nil
		},
	}
	c.Flags().StringVar(&networkType, "type", "", "网络类型登记后不可修改；此参数只会触发明确报错")
	c.Flags().StringVar(&name, "name", "", "新名字")
	c.Flags().StringVar(&site, "site", "", "新的所属站点")
	c.Flags().StringVar(&note, "note", "", "新备注；传空字符串清空")
	c.Flags().StringVar(&networkRange, "cidr", "", "新网段；传空字符串清空")
	c.Flags().Int64Var(&bandwidth, "bandwidth-mbps", 0, "新的标称带宽，单位 Mbit/s；0 表示未填写")
	c.Flags().BoolVar(&metered, "metered", false, "是否按流量计费")
	return c
}

func networksRmCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "rm NAME [--yes]",
		Args:  cobra.ExactArgs(1),
		Short: "Remove a network after confirmation",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireTopologyVault()
			if err != nil {
				return err
			}
			defer s.Close()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "将删除网络 %s。\n", args[0])
			memberships, err := s.ListNetworkMembershipsForNetwork(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "网络成员：")
			if len(memberships) == 0 {
				fmt.Fprintln(out, "  无")
			}
			for _, membership := range memberships {
				fmt.Fprintf(out, "  %s — %s\n", membership.DeviceName, membership.Address)
			}
			if !yes {
				if !topologyStdinIsInteractive() {
					return errors.New("非交互终端不能确认删除网络；请加 --yes 后重跑")
				}
				fmt.Fprint(out, "输入 y 确认删除：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取删除确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未删除网络。")
					return nil
				}
			}
			onlyFallbackServers, err := s.DeleteNetwork(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "已删除网络 %s\n", args[0])
			for _, server := range onlyFallbackServers {
				fmt.Fprintf(out, "服务器条目 %s：候选地址只剩兜底 Host\n", server)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "跳过删除确认；非交互终端必须显式给出")
	return c
}

func networksListCmd() *cobra.Command {
	var site, networkType, instance string
	c := &cobra.Command{
		Use:   "list [--site 站点] [--type physical|virtual] [--instance <name>]",
		Args:  cobra.NoArgs,
		Short: "List topology networks, optionally filtered",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeStore, err := openTopologyViewStore(instance)
			if err != nil {
				return err
			}
			defer closeStore()
			if err := refuseWhenClientCacheAuthorityTooOld(s); err != nil {
				return err
			}
			parsedType := models.NetworkType(networkType)
			if parsedType != "" {
				parsedType, err = models.ParseNetworkType(networkType)
				if err != nil {
					return err
				}
			}
			networks, err := s.ListNetworks(store.NetworkFilter{Site: site, Type: parsedType})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(networks) == 0 {
				fmt.Fprintln(out, "没有匹配的网络")
				return nil
			}
			for _, network := range networks {
				printNetwork(out, network)
			}
			return nil
		},
	}
	c.Flags().StringVar(&site, "site", "", "只显示属于该站点的物理局域网")
	c.Flags().StringVar(&networkType, "type", "", "只显示该类型：physical 或 virtual")
	c.Flags().StringVar(&instance, "instance", "", topologyInstanceFlagHelp)
	return c
}

// printNetwork 输出一个网络的登记字段；空选填字段显示为未填写。
func printNetwork(out io.Writer, network *models.Network) {
	site := network.SiteName
	if site == "" {
		site = "不属于站点"
	}
	fmt.Fprintf(out, "网络 %s — %s\n", network.Name, networkTypeLabel(network.Type))
	fmt.Fprintf(out, "  站点：%s\n", site)
	if network.Note != "" {
		fmt.Fprintf(out, "  备注：%s\n", network.Note)
	}
	if network.NetworkRange != "" {
		fmt.Fprintf(out, "  网段：%s\n", network.NetworkRange)
	}
	if network.BandwidthMbps > 0 {
		fmt.Fprintf(out, "  标称带宽：%d Mbit/s\n", network.BandwidthMbps)
	}
	if network.Metered {
		fmt.Fprintln(out, "  按流量计费")
	}
}

// networkTypeLabel 把网络类型的存储代号换成中文说明。
func networkTypeLabel(networkType models.NetworkType) string {
	if networkType == models.NetworkVirtual {
		return "虚拟局域网"
	}
	return "物理局域网"
}
