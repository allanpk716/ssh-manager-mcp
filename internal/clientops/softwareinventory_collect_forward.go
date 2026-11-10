package clientops

// 客户端实例上的 collect_software_inventory 工具经权威端受审计转发写执行。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/softwareinventory"
)

var _ mcpserver.SoftwareInventoryCollector = (*SoftwareInventoryForwarder)(nil)

// softwareInventoryImmediateForwardTimeout 覆盖单次固定采集命令的超时，另加
// 权威端解析、落库与网络往返的余量。
const softwareInventoryImmediateForwardTimeout = softwareinventory.DefaultCollectTimeout + 30*time.Second

// CollectSoftwareInventory 把立即采集请求转发到权威端；权威端按设备码的
// 可见范围执行或登记客户端采集请求。
func (f *SoftwareInventoryForwarder) CollectSoftwareInventory(
	ctx context.Context, in mcpserver.CollectSoftwareInventoryInput,
) (mcpserver.CollectSoftwareInventoryOutput, error) {
	if !f.capable {
		if f.noCred {
			return mcpserver.CollectSoftwareInventoryOutput{}, &softwareInventoryError{
				text: SoftwareInventoryCollectionUnreachableText, unreachable: true,
			}
		}
		return mcpserver.CollectSoftwareInventoryOutput{}, &softwareInventoryError{
			text: softwareInventoryPlaintextText,
		}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return mcpserver.CollectSoftwareInventoryOutput{}, &softwareInventoryError{
			text: SoftwareInventoryCollectionUnreachableText, err: err, unreachable: true,
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, softwareInventoryImmediateForwardTimeout)
	defer cancel()
	client := *f.client
	client.Timeout = 0
	response, requestErr := softwareInventoryForwardRequest(
		requestCtx, &client, f.url, f.code, "/software-inventory/collect", body,
		SoftwareInventoryCollectionUnreachableText,
		func(response *http.Response) error {
			switch response.StatusCode {
			case http.StatusOK:
				return nil
			case http.StatusBadRequest:
				reason := softwareInventoryResponseReason(response.Body)
				if reason == "" {
					reason = "权威端拒绝立即采集"
				}
				return &softwareInventoryError{
					text: reason, err: httpStatusError(response.StatusCode),
				}
			case http.StatusUnauthorized:
				return &softwareInventoryError{text: softwareInventoryRejectedText}
			case http.StatusForbidden:
				return &softwareInventoryError{
					text: "权威端拒绝立即采集：这台设备不在本设备码绑定 profile 的可见范围内",
				}
			case http.StatusNotFound:
				return &softwareInventoryError{
					text: "权威端还不支持转发立即采集；请升级权威端后重试",
				}
			default:
				return softwareInventoryHTTPUnreachable(
					response, SoftwareInventoryCollectionUnreachableText,
				)
			}
		},
	)
	if requestErr != nil {
		return mcpserver.CollectSoftwareInventoryOutput{}, requestErr
	}
	defer response.Body.Close()
	var outcome softwareinventory.ImmediateCollectionOutcome
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&outcome); err != nil {
		return mcpserver.CollectSoftwareInventoryOutput{}, &softwareInventoryError{
			text: SoftwareInventoryCollectionUnreachableText, err: err, unreachable: true,
		}
	}
	if outcome.Report != nil && outcome.Report.Notes == nil {
		outcome.Report.Notes = []string{}
	}
	return mcpserver.CollectSoftwareInventoryOutput{
		DeviceName: outcome.DeviceName, Requested: outcome.Requested,
		Collector: outcome.Collector, CollectorName: outcome.CollectorName,
		Report: outcome.Report,
	}, nil
}
