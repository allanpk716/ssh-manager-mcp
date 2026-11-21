// 本文件实现软件清单适配层的采集轮次接线：把 Web UI 的「立即采集」（全
// 部设备或单台）交给进程内的轮次管理器后台执行，并把领域快照换成页面
// 展示数据。
package webuisoftwareinventory

import (
	"fmt"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/webui"
)

// StartSoftwareInventoryCollectionRound 发起（或在轮次进行中时复用）一轮
// 立即采集：deviceName 空 = 全部设备，非空 = 只含这台设备（设备不存在或
// 不可见返回 SoftwareInventoryDeviceNotFoundError）；轮次在后台执行，不
// 随本次请求结束而中断。
func (s openerSource) StartSoftwareInventoryCollectionRound(
	deviceName string,
	source webui.SoftwareInventoryWriteSource,
) error {
	_, err := s.rounds.Start(deviceName, softwareinventory.WebUISource(source.RemoteAddress))
	return deviceError(deviceName, err)
}

// SoftwareInventoryRound 返回进行中或最近一轮的展示数据；没有轮次时返回
// nil。
func (s openerSource) SoftwareInventoryRound() (*webui.SoftwareInventoryRoundView, error) {
	round := s.rounds.Current()
	if round == nil {
		return nil, nil
	}
	return roundView(round.Snapshot()), nil
}

// roundView 把领域层的轮次快照换成页面展示数据：逐台中文整句、顶部汇总
// 与整轮状态。
func roundView(snapshot softwareinventory.RoundSnapshot) *webui.SoftwareInventoryRoundView {
	view := &webui.SoftwareInventoryRoundView{
		StartedAt:  snapshot.StartedAt,
		Running:    snapshot.Running(),
		Refreshing: snapshot.NeedsRefresh(),
	}
	view.SummaryText = fmt.Sprintf(
		"已完成 %d/%d 台，成功 %d 台，等待客户端 %d 台，失败 %d 台",
		snapshot.Succeeded+snapshot.Failed, len(snapshot.Devices),
		snapshot.Succeeded, snapshot.WaitingClient, snapshot.Failed,
	)
	switch {
	case snapshot.Running():
		view.StatusText = "采集进行中"
	case snapshot.WaitingClient > 0:
		view.StatusText = "本机采集已完成，等待客户端实例回传结果"
	default:
		view.StatusText = "采集已完成"
	}
	for _, device := range snapshot.Devices {
		view.Devices = append(view.Devices, webui.SoftwareInventoryRoundDeviceView{
			DeviceName: device.DeviceName, StatusText: roundDeviceStatusText(device),
		})
	}
	return view
}

// roundDeviceStatusText 把一台设备的轮次状态换成页面整句。等待设备的实
// 例最近在线时间与客户端页同一口径：从未同步或具体时刻。
func roundDeviceStatusText(device softwareinventory.RoundDeviceStatus) string {
	switch device.State {
	case softwareinventory.RoundDeviceQueued:
		return "排队中"
	case softwareinventory.RoundDeviceCollecting:
		return "采集中"
	case softwareinventory.RoundDeviceSucceeded:
		return "成功，共 " + strconv.Itoa(device.ItemCount) + " 条"
	case softwareinventory.RoundDeviceWaitingClient:
		var text strings.Builder
		text.WriteString("已登记，等待客户端实例")
		if device.CollectorName != "" {
			text.WriteString(" ")
			text.WriteString(device.CollectorName)
		}
		text.WriteString(" 执行，")
		if device.CollectorLastPullAt.IsZero() {
			text.WriteString("实例从未同步")
		} else {
			text.WriteString("实例最近在线 ")
			text.WriteString(device.CollectorLastPullAt.Format("2006-01-02 15:04:05"))
		}
		return text.String()
	default:
		if device.Err != "" {
			return "失败，" + device.Err
		}
		return "失败"
	}
}
