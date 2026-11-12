package clientops

// 下发记录登记的客户端转发器。文件已经写入目标服务器后，这里把记录在
// 线交给权威端；任何失败都不排队、不写本地状态，也不触碰本地缓存。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
)

// secretDeliveryForwardTimeout 限制一次登记请求。登记是普通工具调用的
// 收尾动作，30 秒足以穿过繁忙权威端，又不会挂住调用方。
const secretDeliveryForwardTimeout = 30 * time.Second

const SecretDeliveryUnreachableText = "下发记录无法访问权威端：没有登记，也没有保存本地状态；请在权威端可达后重试"

const secretDeliveryPlaintextText = "下发记录登记要求权威端使用已固定证书的 HTTPS 地址；不会用明文连接登记"

const secretDeliveryDeviceCodeRejectedText = "权威端拒绝了这次下发记录登记的设备码；本地缓存未受影响，请联系 owner 检查设备码"

const secretDeliveryUnsupportedText = "权威端还不支持下发记录登记；请 owner 升级权威端后重试"

type SecretDeliveryRegistrationStatus string

const (
	SecretDeliveryRecorded    SecretDeliveryRegistrationStatus = "recorded"
	SecretDeliveryRefused     SecretDeliveryRegistrationStatus = "refused"
	SecretDeliveryUnreachable SecretDeliveryRegistrationStatus = "unreachable"
)

// SecretDeliveryRegistrationResult 把登记结果分成三类：成功、权威端明确
// 拒绝、无法完成登记。授权拒绝时 RefusalReason 是稳定机器可读原因，
// Message 是可以直接呈现的文本。
type SecretDeliveryRegistrationResult struct {
	Status        SecretDeliveryRegistrationStatus
	Replaced      bool
	RefusalReason store.SecretDeliveryRefusalReason
	Message       string
}

// secretDeliveryError 保留结果里的呈现文本，Unwrap 只携带诊断原因，避免
// 机密值或传输细节泄漏到用户可见错误。
type secretDeliveryError struct {
	result SecretDeliveryRegistrationResult
	err    error
}

func (e *secretDeliveryError) Error() string { return e.result.Message }
func (e *secretDeliveryError) Unwrap() error { return e.err }

// secretDeliveryRequest 是权威端处理器的请求契约。身份与授权字段会随请求
// 发送，但权威端只用设备码认证结果推导，绝不采信这里的自报值。
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

type secretDeliveryRefusalResponse struct {
	Error         string                            `json:"error"`
	RefusalReason store.SecretDeliveryRefusalReason `json:"refusal_reason,omitempty"`
}

// SecretDeliveryForwarder 只负责把记录提交给权威端；它没有本地存储句柄，
// 因此结构上就不可能在客户端生成第二份下发记录。
type SecretDeliveryForwarder struct {
	url     string
	code    string
	client  *http.Client
	capable bool
	noCred  bool
}

// NewSecretDeliveryForwarder 按一台客户端实例的拉取凭据构造转发器。只有
// 「固定证书配了非 https 地址」是构造期配置错误；缺少凭据或缺少固定证书
// 都构造成功，并在登记时失败关闭。
func NewSecretDeliveryForwarder(cred CacheCred) (*SecretDeliveryForwarder, error) {
	if cred.URL == "" || cred.Token == "" {
		return &SecretDeliveryForwarder{noCred: true}, nil
	}
	if cred.Pin == "" {
		return &SecretDeliveryForwarder{}, nil
	}
	if parsed, err := neturl.Parse(cred.URL); err != nil || parsed.Scheme != "https" {
		return nil, fmt.Errorf("下发记录登记要求固定证书时必须使用 https:// 地址（当前为 %q）", cred.URL)
	}
	transport, err := pinningTransport(cred.Pin)
	if err != nil {
		return nil, err
	}
	code := cred.Token
	if bare, _, ok := SplitTokenPin(cred.Token); ok {
		code = bare
	}
	return &SecretDeliveryForwarder{
		url: strings.TrimRight(cred.URL, "/"), code: code,
		client: &http.Client{
			Transport: transport, Timeout: secretDeliveryForwardTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		capable: true,
	}, nil
}

// Close 释放固定传输层安全连接池里的空闲连接。
func (f *SecretDeliveryForwarder) Close() {
	if f == nil || f.client == nil {
		return
	}
	f.client.CloseIdleConnections()
}

// RecordSecretDelivery 提交一条已经完成的下发记录。返回值连同错误一起给
// 出三分类结果：调用方应优先读取返回值的 Status 字段；错误文本可直接呈现。
func (f *SecretDeliveryForwarder) RecordSecretDelivery(
	ctx context.Context, input store.SecretDeliveryRecordInput,
) (SecretDeliveryRegistrationResult, error) {
	if !f.capable {
		if f.noCred {
			return secretDeliveryFailure(SecretDeliveryUnreachable, "", SecretDeliveryUnreachableText, nil)
		}
		return secretDeliveryFailure(SecretDeliveryRefused, "", secretDeliveryPlaintextText, nil)
	}
	body, err := json.Marshal(secretDeliveryRequest{
		ServerID: input.ServerID, Path: input.Path, Template: input.Template,
		FileSHA256: input.FileSHA256, Secrets: input.Secrets,
		WrittenAt: input.WrittenAt.Unix(), InitiatingDeviceName: input.InitiatingDeviceName,
		InitiatingInstanceName: input.InitiatingInstanceName,
		AuthorizationProfileID: input.AuthorizationProfileID,
	})
	if err != nil {
		return secretDeliveryFailure(SecretDeliveryUnreachable, "", SecretDeliveryUnreachableText, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url+"/secret-delivery-records", bytes.NewReader(body))
	if err != nil {
		return secretDeliveryFailure(SecretDeliveryUnreachable, "", SecretDeliveryUnreachableText, err)
	}
	setAuthorityRequestHeaders(request, f.code)
	response, err := f.client.Do(request)
	if err != nil {
		return secretDeliveryFailure(SecretDeliveryUnreachable, "", SecretDeliveryUnreachableText, err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusOK:
		var accepted secretDeliveryResponse
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&accepted); err != nil {
			return secretDeliveryFailure(SecretDeliveryUnreachable, "", SecretDeliveryUnreachableText, err)
		}
		return SecretDeliveryRegistrationResult{Status: SecretDeliveryRecorded, Replaced: accepted.Replaced}, nil
	case response.StatusCode == http.StatusUnauthorized:
		return secretDeliveryFailure(
			SecretDeliveryRefused, store.SecretDeliveryRefusalDeviceCodeNotBound,
			secretDeliveryDeviceCodeRejectedText, nil,
		)
	case response.StatusCode == http.StatusForbidden:
		reason, message := secretDeliveryRefusalBody(response.Body)
		return secretDeliveryFailure(SecretDeliveryRefused, reason, message, nil)
	case response.StatusCode == http.StatusNotFound:
		return secretDeliveryFailure(SecretDeliveryUnreachable, "", secretDeliveryUnsupportedText, nil)
	case response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusRequestEntityTooLarge:
		_, message := secretDeliveryRefusalBody(response.Body)
		return secretDeliveryFailure(SecretDeliveryRefused, "", message, nil)
	default:
		return secretDeliveryFailure(SecretDeliveryUnreachable, "", SecretDeliveryUnreachableText, fmt.Errorf("authority returned %d", response.StatusCode))
	}
}

// RecordSecretDeliveryForBroker 把面向 HTTP 接口的转发结果适配成工具层的
// 下发记录登记能力。呈现原因不包含机密值或传输层诊断细节。
func (f *SecretDeliveryForwarder) RecordSecretDeliveryForBroker(
	ctx context.Context, input store.SecretDeliveryRecordInput,
) (mcpserver.SecretDeliveryRegistrationOutcome, error) {
	result, err := f.RecordSecretDelivery(ctx, input)
	if err == nil {
		return mcpserver.SecretDeliveryRegistrationOutcome{Recorded: true}, nil
	}
	return mcpserver.SecretDeliveryRegistrationOutcome{Reason: result.Message}, err
}

func secretDeliveryFailure(
	status SecretDeliveryRegistrationStatus,
	reason store.SecretDeliveryRefusalReason,
	message string, err error,
) (SecretDeliveryRegistrationResult, error) {
	result := SecretDeliveryRegistrationResult{
		Status: status, RefusalReason: reason, Message: message,
	}
	if status == SecretDeliveryRecorded {
		return result, nil
	}
	return result, &secretDeliveryError{result: result, err: err}
}

// secretDeliveryRefusalBody 解析权威端的 JSON 拒绝原因；非 JSON 响应按原文
// 清理后透传，空响应退回固定不可达文本。
func secretDeliveryRefusalBody(body io.Reader) (store.SecretDeliveryRefusalReason, string) {
	blob, _ := io.ReadAll(io.LimitReader(body, 16<<10))
	var payload secretDeliveryRefusalResponse
	if json.Unmarshal(blob, &payload) == nil && payload.Error != "" {
		return payload.RefusalReason, sanitizePassthrough(payload.Error)
	}
	if text := sanitizePassthrough(strings.TrimSpace(string(blob))); text != "" {
		return "", text
	}
	return "", SecretDeliveryUnreachableText
}

// IsSecretDeliveryUnreachable 判断登记错误是否属于「无法完成登记」分支。
// 传输失败、服务端故障、旧版本没有接口或响应不可解析时都为真。
func IsSecretDeliveryUnreachable(err error) bool {
	var target *secretDeliveryError
	return errors.As(err, &target) && target.result.Status == SecretDeliveryUnreachable
}
