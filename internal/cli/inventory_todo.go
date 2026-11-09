package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/softwareinventory"
)

// inventoryTodoCmd 列出软件清单的三类待办。
func inventoryTodoCmd() *cobra.Command {
	var jsonOutput bool
	c := &cobra.Command{
		Use:   "todo [--json]",
		Args:  cobra.NoArgs,
		Short: "List software inventory follow-ups",
		Long: `列出软件清单的三类待办：

1. 待确认暴露：实测监听所有地址但没有声明暴露级别的端口，按设备分组。
2. 未识别进程：等待声明认领的监听进程。
3. 对不上与清单过期：没有对应当前实测的声明，以及清单过期的设备。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			todos, err := softwareinventory.New(s).Todos(softwareinventory.AllDevicesVisible())
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(todos)
			}
			printInventoryTodos(cmd.OutOrStdout(), todos)
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOutput, "json", false, "以一行 JSON 输出全部待办")
	return c
}

func printInventoryTodos(w io.Writer, todos softwareinventory.TodoResult) {
	fmt.Fprintln(w, "待确认暴露")
	if len(todos.PendingExposure) == 0 {
		fmt.Fprintln(w, "  没有")
	}
	for _, device := range todos.PendingExposure {
		fmt.Fprintf(w, "  设备：%s\n", device.DeviceName)
		for _, port := range device.Ports {
			fmt.Fprintf(w, "    %s:%d/%s（%s；实测稳定键：%s）\n",
				port.Address, port.Port, port.Protocol, port.Exposure.Label(), port.ObservedStableKey)
		}
	}

	fmt.Fprintln(w, "未识别进程")
	if len(todos.UnidentifiedProcesses) == 0 {
		fmt.Fprintln(w, "  没有")
	}
	for _, process := range todos.UnidentifiedProcesses {
		fmt.Fprintf(w, "  设备：%s，进程：%s，可执行文件路径：%s，实测稳定键：%s\n",
			process.DeviceName, processDisplayName(process), process.ExecutablePath,
			process.ObservedStableKey)
		for _, port := range process.ListeningPorts {
			fmt.Fprintf(w, "    %s:%d/%s（%s）\n",
				port.Address, port.Port, port.Protocol, port.Exposure.Label())
		}
	}

	fmt.Fprintln(w, "对不上与清单过期")
	if len(todos.Mismatches) == 0 && len(todos.StaleDevices) == 0 {
		fmt.Fprintln(w, "  没有")
	}
	for _, mismatch := range todos.Mismatches {
		fmt.Fprintf(w, "  对不上：设备：%s，声明：%s（%s）\n",
			mismatch.DeviceName, mismatch.Name, mismatch.Reason)
	}
	for _, stale := range todos.StaleDevices {
		reason := "上次成功采集超过 7 天"
		if stale.NeverCollected {
			reason = "从未成功采集"
		}
		lastSuccess := "无"
		if stale.LastSuccessAt != nil {
			lastSuccess = stale.LastSuccessAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "  清单过期：设备：%s，上次成功采集：%s（%s）\n",
			stale.DeviceName, lastSuccess, reason)
	}
}

func processDisplayName(process softwareinventory.UnidentifiedProcessTodo) string {
	if process.ProcessName != "" {
		return process.ProcessName
	}
	return process.Name
}
