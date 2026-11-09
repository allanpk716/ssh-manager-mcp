package clientops

// 票 #188：声明草稿提交的客户端转发器。它挂在软件清单在线查询的同一个
// 转发器上（固定证书校验、不跟随重定向、设备码认证、超时上限）；权威端
// 不可达时固定报错，不排队、不留本地状态。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"ssh-manager-mcp/internal/mcpserver"
)

// SoftwareDeclarationDraftsUnreachableText 是提交声明草稿时「权威端不可
// 达」的固定错误文本。
const SoftwareDeclarationDraftsUnreachableText = "提交声明草稿无法访问权威端：没有写任何本地数据；请在权威端可达后重试"

// softwareDeclarationDraftsPlaintextText 是明文权威端的拒绝文本。
const softwareDeclarationDraftsPlaintextText = "提交声明草稿要求权威端使用已固定证书的 HTTPS 地址；不会用明文连接提交"

// softwareDeclarationDraftsRejectedText 是设备码被权威端拒绝时的文本。
const softwareDeclarationDraftsRejectedText = "权威端拒绝了这次声明草稿提交的设备码；请联系 owner 检查设备码与绑定 profile"

// softwareDeclarationDraftsForbiddenText 是范围外设备被拒绝时的文本。
const softwareDeclarationDraftsForbiddenText = "权威端拒绝提交声明草稿：草稿涉及的一台设备不在本设备码绑定 profile 的可见范围内"

// softwareDeclarationDraftsUnsupportedText 是旧权威端没有这个接口时的文
// 本。
const softwareDeclarationDraftsUnsupportedText = "权威端还不支持声明草稿提交；请 owner 把权威端升级到支持该功能的版本后重试"

// emptyDraftOutput 是失败分支的固定输出形态：草稿编号恒为数组。
func emptyDraftOutput() mcpserver.DraftSoftwareDeclarationsOutput {
	return mcpserver.DraftSoftwareDeclarationsOutput{DraftIDs: []string{}}
}

// draftUnreachableError 构造「权威端不可达」分支错误。
func draftUnreachableError(err error) error {
	return &softwareInventoryError{
		text: SoftwareDeclarationDraftsUnreachableText, err: err, unreachable: true,
	}
}

// DraftSoftwareDeclarations 把一批声明草稿经「提交声明草稿」受审计转发
// 写送到权威端，返回每条的草稿编号。它同时满足 mcpserver 的注入接口。
func (f *SoftwareInventoryForwarder) DraftSoftwareDeclarations(
	in mcpserver.DraftSoftwareDeclarationsInput,
) (mcpserver.DraftSoftwareDeclarationsOutput, error) {
	if !f.capable {
		if f.noCred {
			return emptyDraftOutput(), draftUnreachableError(nil)
		}
		return emptyDraftOutput(), &softwareInventoryError{text: softwareDeclarationDraftsPlaintextText}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return emptyDraftOutput(), draftUnreachableError(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, f.url+"/software-declaration-drafts", bytes.NewReader(body),
	)
	if err != nil {
		return emptyDraftOutput(), draftUnreachableError(err)
	}
	setAuthorityRequestHeaders(request, f.code)
	response, err := f.client.Do(request)
	if err != nil {
		return emptyDraftOutput(), draftUnreachableError(err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		var payload struct {
			DraftIDs []string `json:"draft_ids"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
			return emptyDraftOutput(), draftUnreachableError(err)
		}
		if payload.DraftIDs == nil {
			payload.DraftIDs = []string{}
		}
		return mcpserver.DraftSoftwareDeclarationsOutput{DraftIDs: payload.DraftIDs}, nil
	case http.StatusBadRequest:
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&payload)
		text := payload.Error
		if text == "" {
			text = "权威端拒绝了这批声明草稿"
		}
		return emptyDraftOutput(), &softwareInventoryError{
			text: text,
			err:  fmt.Errorf("authority returned %s", http.StatusText(response.StatusCode)),
		}
	case http.StatusUnauthorized:
		return emptyDraftOutput(), &softwareInventoryError{text: softwareDeclarationDraftsRejectedText}
	case http.StatusForbidden:
		return emptyDraftOutput(), &softwareInventoryError{text: softwareDeclarationDraftsForbiddenText}
	case http.StatusNotFound:
		return emptyDraftOutput(), &softwareInventoryError{text: softwareDeclarationDraftsUnsupportedText}
	default:
		return emptyDraftOutput(), &softwareInventoryError{
			text:        SoftwareDeclarationDraftsUnreachableText,
			err:         fmt.Errorf("authority returned %s", http.StatusText(response.StatusCode)),
			unreachable: true,
		}
	}
}
