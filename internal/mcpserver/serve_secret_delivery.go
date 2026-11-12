package mcpserver

// 下发记录登记的权威端接口。设备码认证与受审计转发写的守卫顺序一致；
// 授权校验、落库、替换与审计全部委托给 Store.RecordSecretDelivery 的单个
// 事务完成。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ssh-manager-mcp/internal/store"
)

// secretDeliveryRequest 是 POST /secret-delivery-records 的请求体。设备、
// 实例与授权 profile 字段会随请求发送，但权威端一律从设备码认证结果推导。
type secretDeliveryRequest struct {
	ServerID               string                       `json:"server_id"`
	Path                   string                       `json:"path"`
	Template               []byte                       `json:"template"`
	FileSHA256             string                       `json:"file_sha256"`
	Secrets                []store.SecretDeliverySecret `json:"secrets"`
	WrittenAt              int64                        `json:"written_at"`
	InitiatingDeviceName   string                       `json:"initiating_device_name"`
	InitiatingInstanceName string                       `json:"initiating_instance_name"`
	AuthorizationProfileID string                       `json:"authorization_profile_id"`
}

type secretDeliveryResponse struct {
	Replaced bool `json:"replaced"`
}

type secretDeliveryErrorResponse struct {
	Error         string                            `json:"error"`
	RefusalReason store.SecretDeliveryRefusalReason `json:"refusal_reason,omitempty"`
}

// writeSecretDeliveryJSON 统一写 JSON 响应；状态码与响应体都来自明确分支。
func writeSecretDeliveryJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// handleSecretDeliveryRecord 接收一台客户端实例提交的下发记录。设备码认
// 证失败由外层认证中间件返回 401；认证成功后所有授权结论都来自登记事务。
func (r *ServeRunner) handleSecretDeliveryRecord(w http.ResponseWriter, req *http.Request) {
	serverID := "-"
	device := "-"
	status := http.StatusInternalServerError
	defer func() {
		fmt.Fprintf(os.Stderr, "sshmgr serve: secret-delivery-record %q -> %d (device %q)\n", serverID, status, device)
	}()
	if req.Method != http.MethodPost {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	if req.ContentLength > r.secretDeliveryMaxBodyBytes {
		status = http.StatusRequestEntityTooLarge
		http.Error(w, "request body too large", status)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, r.secretDeliveryMaxBodyBytes)
	var input secretDeliveryRequest
	if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
			http.Error(w, "request body too large", status)
			return
		}
		status = http.StatusBadRequest
		writeSecretDeliveryJSON(w, status, secretDeliveryErrorResponse{Error: "unparseable request"})
		return
	}
	if input.ServerID == "" {
		status = http.StatusBadRequest
		writeSecretDeliveryJSON(w, status, secretDeliveryErrorResponse{Error: "server_id must not be empty"})
		return
	}
	serverID = input.ServerID
	if input.WrittenAt <= 0 {
		status = http.StatusBadRequest
		writeSecretDeliveryJSON(w, status, secretDeliveryErrorResponse{Error: "written_at must be after 1970-01-01"})
		return
	}

	tokenInfo := auth.TokenInfoFromContext(req.Context())
	if tokenInfo == nil || tokenInfo.UserID == "" {
		status = http.StatusForbidden
		http.Error(w, "no authenticated cache token", status)
		return
	}
	cacheToken, err := r.st.GetCacheToken(tokenInfo.UserID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr serve: secret-delivery-record cache token lookup %s: %v\n", tokenInfo.UserID, err)
		status = http.StatusInternalServerError
		http.Error(w, "cache token lookup failed", status)
		return
	}
	if cacheToken == nil {
		status = http.StatusForbidden
		http.Error(w, "no authenticated cache token", status)
		return
	}
	device = cacheToken.Name

	result, err := r.st.RecordSecretDelivery(store.SecretDeliveryRecordInput{
		ServerID: input.ServerID, Path: input.Path, Template: input.Template,
		FileSHA256: input.FileSHA256, Secrets: input.Secrets,
		WrittenAt:              time.Unix(input.WrittenAt, 0),
		InitiatingDeviceName:   input.InitiatingDeviceName,
		InitiatingInstanceName: input.InitiatingInstanceName,
		AuthorizationProfileID: input.AuthorizationProfileID,
		TemplateLimitBytes:     r.uploadContentCap,
	}, store.ForwardedWriteSource(cacheToken.ID))
	var refusal *store.SecretDeliveryRefusalError
	var tooLarge *store.SecretDeliveryTemplateTooLargeError
	switch {
	case err == nil:
		status = http.StatusOK
		writeSecretDeliveryJSON(w, status, secretDeliveryResponse{Replaced: result.Replaced})
	case errors.As(err, &refusal):
		status = http.StatusForbidden
		writeSecretDeliveryJSON(w, status, secretDeliveryErrorResponse{
			Error: refusal.Error(), RefusalReason: refusal.Reason,
		})
	case errors.As(err, &tooLarge):
		status = http.StatusRequestEntityTooLarge
		writeSecretDeliveryJSON(w, status, secretDeliveryErrorResponse{Error: tooLarge.Error()})
	case errors.Is(err, store.ErrSecretDeliveryInvalidPath):
		status = http.StatusBadRequest
		writeSecretDeliveryJSON(w, status, secretDeliveryErrorResponse{Error: err.Error()})
	default:
		fmt.Fprintf(os.Stderr, "sshmgr serve: secret-delivery-record write %s: %v\n", input.ServerID, err)
		status = http.StatusInternalServerError
		http.Error(w, "delivery record registration failed", status)
	}
}
