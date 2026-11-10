package mcpserver

// 客户端实例转发「立即采集」的权威端接口。设备码认证、可见范围与错误
// 分支沿用软件清单在线查询；采集语义复用领域层的统一分派入口。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ssh-manager-mcp/internal/softwareinventory"
)

// softwareInventoryImmediateRequest 是 POST /software-inventory/collect 的
// 请求体。
type softwareInventoryImmediateRequest struct {
	Device string `json:"device"`
}

// softwareInventoryImmediateResponse 与客户端 MCP 工具的立即采集输出保持
// 同一结构。
type softwareInventoryImmediateResponse struct {
	DeviceName    string                              `json:"device_name"`
	Requested     bool                                `json:"requested"`
	Collector     string                              `json:"collector,omitempty"`
	CollectorName string                              `json:"collector_name,omitempty"`
	Report        *softwareinventory.CollectionReport `json:"report,omitempty"`
}

// handleSoftwareInventoryImmediateCollection 执行一次设备码认证的立即采集。
func (r *ServeRunner) handleSoftwareInventoryImmediateCollection(
	w http.ResponseWriter, req *http.Request,
) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, softwareInventoryMaxBodyBytes)
	var in softwareInventoryImmediateRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeSoftwareInventoryJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unparseable request",
		})
		return
	}
	ti := auth.TokenInfoFromContext(req.Context())
	if ti == nil || ti.UserID == "" {
		http.Error(w, "no authenticated cache token", http.StatusForbidden)
		return
	}
	ct, err := r.st.GetCacheToken(ti.UserID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory collect token lookup %s: %v\n", ti.UserID, err)
		http.Error(w, "cache token lookup failed", http.StatusInternalServerError)
		return
	}
	if ct == nil || ct.ProfileID == "" {
		http.Error(w, "device code not bound to a profile", http.StatusForbidden)
		return
	}
	granted, err := r.st.ServersForProfile(ct.ProfileID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory collect grant walk %s: %v\n", ct.ProfileID, err)
		http.Error(w, "grant walk failed", http.StatusInternalServerError)
		return
	}
	outcome, err := softwareinventory.New(r.st).CollectForScope(
		req.Context(), in.Device,
		softwareinventory.DevicesVisibleThroughServers(granted...),
		softwareInventoryCollectService(r.st),
		softwareinventory.ForwardedSource(ct.ID),
	)
	switch {
	case err == nil:
		if outcome.Report != nil && outcome.Report.Notes == nil {
			outcome.Report.Notes = []string{}
		}
		writeSoftwareInventoryJSON(w, http.StatusOK, softwareInventoryImmediateResponse{
			DeviceName: outcome.DeviceName, Requested: outcome.Requested,
			Collector: outcome.Collector, CollectorName: outcome.CollectorName,
			Report: outcome.Report,
		})
	case errors.Is(err, softwareinventory.ErrDeviceNotVisible):
		http.Error(w, "device is not visible to this device code's profile", http.StatusForbidden)
	default:
		writeSoftwareInventoryJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}
