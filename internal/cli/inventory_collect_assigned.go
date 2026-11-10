package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// exitCodePartialCollectFailure 是「部分设备采集失败」的专门退出码：成功
// 0、一般错误 1、权威端不可达 4 之外，计划任务与外部程序用它区分整轮完
// 成但个别设备失败的情况。
const exitCodePartialCollectFailure = 5

// inventoryCollectAssignedRunner 执行本地一轮采集。测试替换这个变量注入
// 假报告；生产实现复用调度服务的「执行一轮」。
var inventoryCollectAssignedRunner = func(
	ctx context.Context, st *store.Store,
) (*softwareinventory.SchedulerRoundReport, error) {
	return softwareinventory.NewScheduler(st).ExecuteRound(ctx)
}

// inventoryCollectAssignedClient 执行客户端实例的一轮在线采集。测试替换
// 这个变量注入报告；生产路径经设备码取分派、用本地 SSH 能力采集并逐台
// 上交，客户端本地不保存软件清单。
var inventoryCollectAssignedClient = func(
	ctx context.Context, instance string,
) (*softwareinventory.SchedulerRoundReport, error) {
	// 外层 runInventoryCollectAssignedClient 已持有本地锁；这里不能再调用
	// 带锁的公共入口，避免同一进程第二次抢同一把锁。
	credential, err := clientops.ReadCacheCredFor(instance)
	if err != nil {
		return nil, err
	}
	if credential == nil {
		credential = &clientops.CacheCred{}
	}
	forwarder, err := clientops.NewSoftwareInventoryForwarder(*credential)
	if err != nil {
		return nil, err
	}
	defer forwarder.Close()
	snapshot, err := loadInventoryClientSnapshot(instance)
	if err != nil {
		return nil, err
	}
	temporary, err := store.OpenTemporaryClientCacheStore(snapshot, "sshmgr-inventory-collect-*.db")
	if err != nil {
		return nil, err
	}
	defer func() { _ = temporary.Close() }()
	collector := &clientops.SoftwareInventoryClientCollector{
		Forwarder: forwarder,
		OpenStore: func() (*store.Store, func(), error) {
			return temporary.Store, func() {}, nil
		},
	}
	return collector.CollectAssigned(ctx)
}

// runInventoryCollectAssignedClient 输出客户端采集报告并映射两个专门退
// 出码：权威端不可达为 4，部分设备采集失败为 5。
func runInventoryCollectAssignedClient(ctx context.Context, instance string, out io.Writer, jsonOutput bool) error {
	lockPath, err := clientops.SoftwareInventoryCollectionLockPath(instance)
	if err != nil {
		return err
	}
	lock, err := clientops.AcquireSoftwareInventoryCollectionLock(lockPath)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	report, err := inventoryCollectAssignedClient(ctx, instance)
	if err != nil {
		if clientops.IsSoftwareInventoryUnreachable(err) {
			return NewExitCodeError(exitCodeAuthorityUnreachable, err)
		}
		return err
	}
	if err := writeCollectAssignedReport(out, report, jsonOutput); err != nil {
		return err
	}
	if report.Failed > 0 {
		return NewExitCodeError(exitCodePartialCollectFailure, errors.New(
			"部分设备采集失败；失败设备与原因见上面的输出"))
	}
	return nil
}

// loadInventoryClientSnapshot 按客户端命令的实例选择规则读缓存快照；默认
// 槽为空且存在命名实例时要求显式点名，绝不猜测。
func loadInventoryClientSnapshot(instance string) (*store.Snapshot, error) {
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
				"默认缓存槽没有快照，但本机有 %d 个命名客户端实例：%s；请用 --instance 明确选择",
				len(names), strings.Join(names, ", "))
		}
	}
	snapshot, err := clientops.LoadCacheSnapshotFor("")
	if err != nil {
		return nil, fmt.Errorf("本机客户端缓存不可读：请先执行 cache pull，再采集软件清单（%w）", err)
	}
	return snapshot, nil
}

func inventoryCollectAssignedCmd() *cobra.Command {
	var jsonOutput bool
	var instance string
	c := &cobra.Command{
		Use:   "collect-assigned [--json]",
		Short: "采集分派给本实例的设备（每天到期一轮）",
		Long: `采集分派给本实例的设备：读取本地库，把「距上次成功采集已满一天，或
有待执行的立即采集请求」的受管设备全部采集一遍并落库。单机由 owner 用
系统计划任务每天调用；权威端 serve 已内置同样的周期调度，这条子命令主
要用于单机与手动补采。客户端实例经权威端在线取分派的路径在后续版本提
供。

退出码：0 成功（含没有到期设备）；1 一般错误；5 部分设备采集失败。

系统计划任务调用示例（sshmgr 不替你安装，只给出写法）：
  launchd（每天 06:00，ProgramArguments 按本机安装路径填写）：
    /usr/local/bin/sshmgr inventory collect-assigned
    StartCalendarInterval -> Hour 6, Minute 0
  cron（每天 06:00）：
    0 6 * * * /usr/local/bin/sshmgr inventory collect-assigned
  Windows 计划任务（每天 06:00）：
    schtasks /Create /TN sshmgr-inventory /SC DAILY /ST 06:00 /TR "sshmgr inventory collect-assigned"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := roleForVaultBackedCommand(instance, errInventoryNeedVault)
			if err != nil {
				return err
			}
			if role == policyRoleClient {
				return runInventoryCollectAssignedClient(cmd.Context(), instance, cmd.OutOrStdout(), jsonOutput)
			}
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			report, err := inventoryCollectAssignedRunner(cmd.Context(), st)
			if err != nil {
				return err
			}
			if err := writeCollectAssignedReport(cmd.OutOrStdout(), report, jsonOutput); err != nil {
				return err
			}
			if report.Failed > 0 {
				return NewExitCodeError(exitCodePartialCollectFailure, errors.New(
					"部分设备采集失败；失败设备与原因见上面的输出"))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOutput, "json", false, "以一行 JSON 输出一轮报告")
	c.Flags().StringVar(&instance, "instance", "", "在客户端上读取命名实例 instances/<名称> 的缓存；省略时读默认缓存槽")
	return c
}

type collectAssignedDeviceJSON struct {
	DeviceName       string             `json:"device_name"`
	PendingImmediate bool               `json:"pending_immediate"`
	Error            string             `json:"error,omitempty"`
	Report           *collectReportJSON `json:"report,omitempty"`
}

type collectAssignedJSON struct {
	Due           int                         `json:"due"`
	SkippedNotDue int                         `json:"skipped_not_due"`
	Succeeded     int                         `json:"succeeded"`
	Failed        int                         `json:"failed"`
	Devices       []collectAssignedDeviceJSON `json:"devices"`
}

// writeCollectAssignedReport 输出一轮采集的结论：统计一行加每台设备一行；
// JSON 模式输出一行汇总。
func writeCollectAssignedReport(
	w io.Writer, report *softwareinventory.SchedulerRoundReport, jsonOutput bool,
) error {
	if jsonOutput {
		out := collectAssignedJSON{
			Due: report.Due, SkippedNotDue: report.SkippedNotDue,
			Succeeded: report.Succeeded, Failed: report.Failed,
			Devices: make([]collectAssignedDeviceJSON, 0, len(report.Devices)),
		}
		for _, device := range report.Devices {
			row := collectAssignedDeviceJSON{
				DeviceName: device.DeviceName, PendingImmediate: device.PendingImmediate,
				Error: device.Err,
			}
			if device.Report != nil {
				row.Report = collectionReportToJSON(device.Report)
			}
			out.Devices = append(out.Devices, row)
		}
		return json.NewEncoder(w).Encode(out)
	}
	if report.Due == 0 {
		fmt.Fprintf(w, "本轮没有到期设备（检查 %d 台，未到期跳过 %d 台）。\n",
			report.Due+report.SkippedNotDue, report.SkippedNotDue)
		return nil
	}
	fmt.Fprintf(w, "软件清单采集一轮完成：到期 %d 台，成功 %d 台，失败 %d 台，未到期跳过 %d 台\n",
		report.Due, report.Succeeded, report.Failed, report.SkippedNotDue)
	for _, device := range report.Devices {
		if device.Err != "" {
			fmt.Fprintf(w, "设备 %s 采集失败：%s\n", device.DeviceName, device.Err)
			continue
		}
		if device.Report == nil {
			fmt.Fprintf(w, "设备 %s 已采集\n", device.DeviceName)
			continue
		}
		detail := device.Report
		suffix := ""
		if detail.Incomplete {
			suffix = "，结果不完整：" + strings.Join(detail.Notes, "；")
		}
		fmt.Fprintf(w, "设备 %s 已采集（条目 %s，命令集 %s，系统 %s%s）\n",
			device.DeviceName, detail.ServerEntryName, detail.CommandSetVersion,
			detail.SystemType, suffix)
	}
	return nil
}

// collectionReportToJSON 把一台设备的采集报告转换成 JSON 行结构，与立即
// 采集命令的 --json 输出保持同一形态。
func collectionReportToJSON(report *softwareinventory.CollectionReport) *collectReportJSON {
	counts := map[string]int{}
	for category, count := range report.ItemCounts {
		counts[category.Label()] = count
	}
	return &collectReportJSON{
		DeviceName: report.DeviceName, ServerEntryName: report.ServerEntryName,
		Sudo: report.Sudo, CommandSetVersion: report.CommandSetVersion,
		SystemType: report.SystemType, Incomplete: report.Incomplete,
		Notes: report.Notes, ItemCounts: counts,
	}
}
