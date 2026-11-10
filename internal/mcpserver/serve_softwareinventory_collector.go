package mcpserver

// 票 #187：客户端实例作为采集执行者的两个设备码认证接口之一——只读「取
// 本实例的采集分派」。返回设备、采集用服务器条目、sudo 开关、立即采集请
// 求与到期判断；服务器条目已不在当前 profile 时不下发。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

type softwareInventoryAssignmentsRequest struct{}

type softwareInventoryAssignmentsResponse struct {
	Devices []softwareInventoryAssignmentJSON `json:"devices"`
}

type softwareInventoryAssignmentJSON struct {
	DeviceID         string `json:"device_id"`
	DeviceName       string `json:"device_name"`
	ServerID         string `json:"server_id"`
	ServerName       string `json:"server_name"`
	Sudo             bool   `json:"sudo"`
	PendingImmediate bool   `json:"pending_immediate"`
	LastSuccessAt    int64  `json:"last_success_at"`
	Due              bool   `json:"due"`
}

func (r *ServeRunner) handleSoftwareInventoryAssignments(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, softwareInventoryMaxBodyBytes)
	var in softwareInventoryAssignmentsRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeSoftwareInventoryJSON(w, http.StatusBadRequest, map[string]string{"error": "unparseable request"})
		return
	}
	ti := auth.TokenInfoFromContext(req.Context())
	if ti == nil || ti.UserID == "" {
		http.Error(w, "no authenticated cache token", http.StatusForbidden)
		return
	}
	ct, err := r.st.GetCacheToken(ti.UserID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory assignments token lookup %s: %v\n", ti.UserID, err)
		http.Error(w, "cache token lookup failed", http.StatusInternalServerError)
		return
	}
	if ct == nil || ct.ProfileID == "" {
		http.Error(w, "device code not bound to a profile", http.StatusForbidden)
		return
	}
	granted, err := r.st.ServersForProfile(ct.ProfileID)
	if err != nil {
		http.Error(w, "grant walk failed", http.StatusInternalServerError)
		return
	}
	grantedSet := make(map[string]bool, len(granted))
	for _, id := range granted {
		grantedSet[id] = true
	}
	assignments, err := r.st.SoftwareCollectionAssignments(models.ClientCollector(ct.ID), time.Now())
	if err != nil {
		http.Error(w, "assignment lookup failed", http.StatusInternalServerError)
		return
	}
	out := make([]softwareInventoryAssignmentJSON, 0, len(assignments))
	for _, assignment := range assignments {
		if !grantedSet[assignment.ServerID] {
			continue
		}
		out = append(out, softwareInventoryAssignmentJSON{
			DeviceID: assignment.DeviceID, DeviceName: assignment.DeviceName,
			ServerID: assignment.ServerID, ServerName: assignment.ServerName,
			Sudo: assignment.Sudo, PendingImmediate: assignment.PendingImmediate,
			LastSuccessAt: assignment.LastSuccessAt.Unix(), Due: assignment.Due,
		})
	}
	writeSoftwareInventoryJSON(w, http.StatusOK, softwareInventoryAssignmentsResponse{Devices: out})
}

// softwareInventorySubmissionMaxBodyBytes 是采集结果请求体上限。原始输出
// 上限为 16 MiB，JSON 包装后留 1 MiB 余量；更大的请求直接拒绝。
const softwareInventorySubmissionMaxBodyBytes = 17 << 20

type softwareInventorySubmissionResponse struct {
	Status string `json:"status"`
}

// handleSoftwareInventoryResults 是受审计转发写的新成员「上交采集结果」。
// 守卫顺序：请求体上限与格式 → 设备码 → profile → 登记的采集执行者 →
// 服务器条目仍在设备与 profile 里 → 复用实测落库事务。
func (r *ServeRunner) handleSoftwareInventoryResults(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, softwareInventorySubmissionMaxBodyBytes)
	var in softwareinventory.CollectionSubmission
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeSoftwareInventoryJSON(w, http.StatusBadRequest, map[string]string{"error": "unparseable request"})
		return
	}
	ti := auth.TokenInfoFromContext(req.Context())
	if ti == nil || ti.UserID == "" {
		http.Error(w, "no authenticated cache token", http.StatusForbidden)
		return
	}
	ct, err := r.st.GetCacheToken(ti.UserID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory results token lookup %s: %v\n", ti.UserID, err)
		http.Error(w, "cache token lookup failed", http.StatusInternalServerError)
		return
	}
	if ct == nil || ct.ProfileID == "" {
		http.Error(w, "device code not bound to a profile", http.StatusForbidden)
		return
	}
	granted, err := r.st.ServersForProfile(ct.ProfileID)
	if err != nil {
		http.Error(w, "grant walk failed", http.StatusInternalServerError)
		return
	}
	grantedSet := make(map[string]bool, len(granted))
	for _, id := range granted {
		grantedSet[id] = true
	}
	device, err := r.st.GetDevice(in.DeviceName)
	if err != nil || device == nil {
		http.Error(w, "上交结果里的设备不存在或不可访问", http.StatusForbidden)
		return
	}
	if in.DeviceID != "" && in.DeviceID != device.ID {
		http.Error(w, "上交结果里的设备标识与设备名不匹配", http.StatusForbidden)
		return
	}
	status, err := r.st.SoftwareCollectionStatusForDevice(in.DeviceName)
	if err != nil || status == nil {
		http.Error(w, "collection status lookup failed", http.StatusInternalServerError)
		return
	}
	if !status.Collector.IsClient() || status.Collector.CacheTokenID != ct.ID {
		http.Error(w, "设备码 "+ct.Name+" 不是这台设备登记的采集执行者", http.StatusForbidden)
		return
	}
	if !grantedSet[in.ServerID] {
		http.Error(w, "采集用服务器条目已不在该设备码绑定 profile 里", http.StatusForbidden)
		return
	}
	server, err := r.st.GetServer(in.ServerID)
	if err != nil || server == nil || server.DeviceID != device.ID {
		http.Error(w, "采集用服务器条目不在这台设备上", http.StatusForbidden)
		return
	}
	if in.Failed {
		if err := r.st.RecordSoftwareCollectionFailureFromCollector(
			device.Name, models.ClientCollector(ct.ID), in.Error, server.ID, in.Sudo,
			store.ForwardedWriteSource(ct.ID),
		); err != nil {
			http.Error(w, "collection failure landing failed", http.StatusInternalServerError)
			return
		}
		writeSoftwareInventoryJSON(w, http.StatusOK, softwareInventorySubmissionResponse{Status: "failed"})
		return
	}
	if err := r.st.ReplaceSoftwareObservedItems(device.Name, store.SoftwareObservedWrite{
		Collector: models.ClientCollector(ct.ID), Items: nonNilItems(in.Items), ServerID: server.ID,
		Sudo: in.Sudo, CommandSetVersion: in.CommandSetVersion,
		SystemType: in.SystemType, Incomplete: in.Incomplete,
	}, store.ForwardedWriteSource(ct.ID)); err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory result landing %s: %v\n", device.Name, err)
		http.Error(w, "collection result landing failed", http.StatusInternalServerError)
		return
	}
	result := "ok"
	if in.Incomplete {
		result = "incomplete"
	}
	writeSoftwareInventoryJSON(w, http.StatusOK, softwareInventorySubmissionResponse{Status: result})
}

func nonNilItems(items []models.SoftwareObservedItem) []models.SoftwareObservedItem {
	if items == nil {
		return []models.SoftwareObservedItem{}
	}
	return items
}
