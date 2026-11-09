package mcpserver

// 票 #188：权威端「提交声明草稿」受审计转发写接口。POST
// /software-declaration-drafts 沿用 /software-inventory 的路由注册、设备
// 码认证与守卫顺序；按设备码绑定的 profile 核对每台设备都在可见范围内，
// 只写草稿，不改生效声明；审计摘要记录设备码编号。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ssh-manager-mcp/internal/softwareinventory"
)

// softwareDeclarationDraftsMaxBodyBytes 是提交声明草稿的请求体上限。一
// 批草稿每条都很小，这个上限挡异常客户端与放大请求。
const softwareDeclarationDraftsMaxBodyBytes = 256 << 10

// softwareDeclarationDraftsRequest 是提交声明草稿的请求体。declarations
// 的字段与 draft_software_declarations 工具输入一一对应。
type softwareDeclarationDraftsRequest struct {
	Declarations []softwareinventory.DeclarationDraft `json:"declarations"`
}

// softwareDeclarationDraftsResponse 是 200 响应体：按输入顺序返回草稿编
// 号。
type softwareDeclarationDraftsResponse struct {
	DraftIDs []string `json:"draft_ids"`
}

// handleSoftwareDeclarationDrafts 落地一次设备码认证的声明草稿提交。守
// 卫顺序与 handleSoftwareInventory 一致：方法 → 请求体 → 设备码 →
// profile 绑定 → profile 可见范围 → 领域写入。
func (r *ServeRunner) handleSoftwareDeclarationDrafts(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, softwareDeclarationDraftsMaxBodyBytes)
	var in softwareDeclarationDraftsRequest
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
	if len(in.Declarations) == 0 {
		writeSoftwareInventoryJSON(w, http.StatusBadRequest, map[string]string{
			"error": "declarations must carry at least one declaration draft",
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
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-declaration-drafts cache token lookup %s: %v\n", ti.UserID, err)
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

	// profile 可见范围：只有至少有一个被授权服务器条目的受管设备才能
	// 被起草；越界拒绝文案不区分设备不存在与不可见。
	granted, err := r.st.ServersForProfile(ct.ProfileID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: software-declaration-drafts grant walk %s: %v\n", ct.ProfileID, err)
		http.Error(w, "grant walk failed", http.StatusInternalServerError)
		return
	}
	ids, err := softwareinventory.New(r.st).SubmitDeclarationDrafts(
		in.Declarations,
		softwareinventory.DevicesVisibleThroughServers(granted...),
		softwareinventory.ForwardedSource(ct.Name),
	)
	switch {
	case err == nil:
		if ids == nil {
			ids = []string{}
		}
		writeSoftwareInventoryJSON(w, http.StatusOK, softwareDeclarationDraftsResponse{DraftIDs: ids})
	case errors.Is(err, softwareinventory.ErrDeviceNotVisible):
		http.Error(w, "a device in this draft batch is not visible to this device code's profile", http.StatusForbidden)
	default:
		writeSoftwareInventoryJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}
