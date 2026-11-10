package mcpserver

// 票 #185：权威端「查询软件清单」只读接口。POST /software-inventory 沿
// 用 /server-metadata 的路由注册、设备码认证与守卫顺序；按设备码绑定的
// profile 计算可见设备范围后，执行与本地相同的领域查询。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ssh-manager-mcp/internal/softwareinventory"
)

// softwareInventoryMaxBodyBytes 是查询请求体上限。过滤条件都很小，这个
// 上限只挡异常客户端。
const softwareInventoryMaxBodyBytes = 4 << 10

// softwareInventoryRequest 是查询请求体。字段与
// softwareinventory.QueryOptions 的过滤条件一一对应；零值表示不限。
type softwareInventoryRequest struct {
	Device                string `json:"device"`
	Name                  string `json:"name"`
	MinimumExposure       string `json:"minimum_exposure"`
	Category              string `json:"category"`
	IncludeSystemPackages bool   `json:"include_system_packages"`
}

func writeSoftwareInventoryJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleSoftwareInventory 执行一次设备码认证的软件清单查询。守卫顺序与
// handleServerMetadata 一致：方法 → 请求体 → 设备码 → profile 绑定 →
// profile 可见范围 → 领域查询。
func (r *ServeRunner) handleSoftwareInventory(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, softwareInventoryMaxBodyBytes)
	var in softwareInventoryRequest
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

	// 设备码认证：TokenInfo → GetCacheToken，存储故障是 500 而不是 403。
	ti := auth.TokenInfoFromContext(req.Context())
	if ti == nil || ti.UserID == "" {
		http.Error(w, "no authenticated cache token", http.StatusForbidden)
		return
	}
	ct, err := r.st.GetCacheToken(ti.UserID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory cache token lookup %s: %v\n", ti.UserID, err)
		http.Error(w, "cache token lookup failed", http.StatusInternalServerError)
		return
	}
	if ct == nil {
		http.Error(w, "no authenticated cache token", http.StatusForbidden)
		return
	}
	if ct.ProfileID == "" {
		http.Error(w, "device code not bound to a profile — owner: run `sshmgr cache-tokens bind "+ct.Name+" <profile>` on the server", http.StatusForbidden)
		return
	}
	profile, err := r.st.GetProfile(ct.ProfileID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory profile lookup %s: %v\n", ct.ProfileID, err)
		http.Error(w, "profile lookup failed", http.StatusInternalServerError)
		return
	}
	if profile == nil {
		http.Error(w, "device code's profile no longer exists", http.StatusForbidden)
		return
	}

	// profile 可见范围：这个设备码被授权的服务器条目所在的受管设备。
	granted, err := r.st.ServersForProfile(ct.ProfileID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-inventory grant walk %s: %v\n", ct.ProfileID, err)
		http.Error(w, "grant walk failed", http.StatusInternalServerError)
		return
	}
	result, err := softwareinventory.New(r.st).Query(softwareinventory.QueryOptions{
		Device:                in.Device,
		Name:                  in.Name,
		MinimumExposure:       softwareinventory.Exposure(in.MinimumExposure),
		Category:              softwareinventory.Category(in.Category),
		IncludeSystemPackages: in.IncludeSystemPackages,
		Visibility:            softwareinventory.DevicesVisibleThroughServers(granted...),
	})
	result.ProfileName = profile.Name
	switch {
	case err == nil:
		writeSoftwareInventoryJSON(w, http.StatusOK, result)
	case errors.Is(err, softwareinventory.ErrDeviceNotVisible):
		http.Error(w, "device is not visible to this device code's profile", http.StatusForbidden)
	default:
		writeSoftwareInventoryJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}
