package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/store"
)

func devicesProbeCmd() *cobra.Command {
	var from, instance string
	c := &cobra.Command{
		Use:   "probe --from <self|服务器编号> <目标>...",
		Args:  cobra.MinimumNArgs(1),
		Short: "Probe TCP connectivity to registered devices",
		Long: "从本机或某台受管服务器出发，探测登记设备的传输控制协议端口连通与建连耗时。\n" +
			"目标写服务器编号或设备名，可附「:端口」；端口缺省用服务器条目端口或 22。\n" +
			"只探测拓扑登记的共同网络地址；结果只存在本机，不回传权威端。",
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := topologyRoleFor(instance)
			if err != nil {
				return err
			}
			var st *store.Store
			if role == policyRoleClient {
				probeStore, closeProbeStore, err := openClientProbeStore(instance)
				if err != nil {
					return err
				}
				// 与拓扑查看命令不同：探测是诊断动作，客户端实例的审计写
				// 进旁车审计文件，而不是不写审计的只读库设置。
				defer closeProbeStore()
				st = probeStore
			} else {
				opened, err := openUnlockedStore()
				if err != nil {
					return err
				}
				defer opened.Close()
				st = opened
			}
			if err := refuseWhenClientCacheAuthorityTooOld(st); err != nil {
				return err
			}

			service := &probe.Service{Store: st, Instance: instance}
			if results, ok, pathErr := probeobserve.ResultsFor(instance); pathErr == nil && ok {
				service.Results = results
			}
			start := time.Now()
			report, probeErr := service.Probe(cmd.Context(), probe.Request{From: from, Targets: args})
			summary, originServerID := probe.AuditSummary(from, report, probeErr)
			status := "error"
			if probeErr == nil {
				status = "ok"
			}
			if err := st.WriteAudit(store.AuditRow{
				TS: start, ServerID: originServerID, Action: "probe-connectivity", Status: status,
				Source: store.AuditSourceCLI, Command: store.MarshalAuditSummary(summary),
			}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "sshmgr: 写审计失败: %v\n", err)
			}
			if probeErr != nil {
				return probeErr
			}
			fmt.Fprintln(cmd.OutOrStdout(), report.Text())
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "探测起点：self 或服务器编号")
	_ = c.MarkFlagRequired("from")
	c.Flags().StringVar(&instance, "instance", "", topologyInstanceFlagHelp)
	return c
}

// openClientProbeStore 为客户端上的一次连通探测打开数据源：把本地快照导
// 入一次性临时库，并把审计写进实例的旁车审计文件。命令行探测与 Web UI 的
// 按需触发共用这一份打开逻辑。
func openClientProbeStore(instance string) (*store.Store, func(), error) {
	snapshot, err := loadTopologyClientSnapshot(instance)
	if err != nil {
		return nil, nil, err
	}
	_, _, _, auditPath, err := clientops.CachePathsFor(instance)
	if err != nil {
		return nil, nil, err
	}
	auditFile, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	temporary, err := store.OpenTemporaryClientCacheStore(snapshot, "sshmgr-probe-*.db")
	if err != nil {
		_ = auditFile.Close()
		return nil, nil, err
	}
	temporary.Store.SetCacheRecommendedVersion(snapshot.RecommendedVersion)
	temporary.Store.SetReadOnly(auditFile)
	return temporary.Store, func() {
		_ = temporary.Close()
		_ = auditFile.Close()
	}, nil
}
