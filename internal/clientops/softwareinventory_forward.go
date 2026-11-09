package clientops

// 票 #185：软件清单在线查询的客户端转发器。客户端的软件清单不随快照
// 下发，查询必须在线访问权威端；这里沿用元数据编辑转发器的构造与传输
// 安全写法（固定证书校验、不跟随重定向、设备码认证、超时上限），权威端
// 不可达时返回固定错误文本，绝不回退本地数据。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/softwareinventory"
)

// softwareInventoryForwardTimeout 限制一次在线查询。查询是普通工具调用，
// 30 秒足以穿过繁忙的权威端，又不会挂住工具调用方。
const softwareInventoryForwardTimeout = 30 * time.Second

// SoftwareInventoryUnreachableText 是「权威端不可达」的固定错误文本：
// 传输失败、服务端故障、缺凭据都用这一句，供命令行映射专门退出码。
const SoftwareInventoryUnreachableText = "软件清单查询无法访问权威端：没有读取任何本地数据；请在权威端可达后重试"

// softwareInventoryPlaintextText 是明文权威端的拒绝文本：软件清单查询只
// 走固定传输层安全。
const softwareInventoryPlaintextText = "软件清单查询要求权威端使用已固定证书的 HTTPS 地址；不会用明文连接查询"

// softwareInventoryRejectedText 是设备码被权威端拒绝时的文本。
const softwareInventoryRejectedText = "权威端拒绝了这次软件清单查询的设备码；请联系 owner 检查设备码与绑定 profile"

// softwareInventoryForbiddenText 是范围外设备被拒绝时的文本。
const softwareInventoryForbiddenText = "权威端拒绝查询：这台设备不在本设备码绑定 profile 的可见范围内"

// softwareInventoryUnsupportedText 是旧权威端没有这个接口时的文本。
const softwareInventoryUnsupportedText = "权威端还不支持软件清单查询；请 owner 把权威端升级到支持该功能的版本后重试"

// softwareInventoryError 是分支映射后的失败。text 是固定契约文本，err 保
// 留原始原因；unreachable 供命令行判定专门退出码。
type softwareInventoryError struct {
	text        string
	err         error
	unreachable bool
}

func (e *softwareInventoryError) Error() string { return e.text }
func (e *softwareInventoryError) Unwrap() error { return e.err }

// IsSoftwareInventoryUnreachable 判断一个软件清单查询错误是否属于「权威
// 端不可达」分支（命令行据此返回专门退出码）。
func IsSoftwareInventoryUnreachable(err error) bool {
	var target *softwareInventoryError
	return errors.As(err, &target) && target.unreachable
}

// softwareInventoryRequest 是请求体；字段名是权威端处理器的契约。
type softwareInventoryRequest struct {
	Device                string `json:"device"`
	Name                  string `json:"name"`
	MinimumExposure       string `json:"minimum_exposure"`
	Category              string `json:"category"`
	IncludeSystemPackages bool   `json:"include_system_packages"`
}

// SoftwareInventoryForwarder 把软件清单查询转发到权威端的
// POST /software-inventory。能力判定与元数据转发器相同，发生在构造时：
//   - 有固定证书与 https 地址：可用，固定传输层安全 + 30 秒上限；
//   - 没有固定证书：拒绝明文查询；
//   - 没有凭据：与不可达同样固定报错，绝不查本地数据。
type SoftwareInventoryForwarder struct {
	url     string
	code    string
	client  *http.Client
	capable bool
	noCred  bool
}

// NewSoftwareInventoryForwarder 按一台客户端实例的拉取凭据构造转发器。
// 只有「固定证书配了非 https 地址」这种硬性配置错误在构造期返回；其余
// 情况构造成功，查询时按分支固定报错。
func NewSoftwareInventoryForwarder(cred CacheCred) (*SoftwareInventoryForwarder, error) {
	if cred.URL == "" || cred.Token == "" {
		return &SoftwareInventoryForwarder{noCred: true}, nil
	}
	if cred.Pin == "" {
		return &SoftwareInventoryForwarder{}, nil
	}
	if parsed, err := neturl.Parse(cred.URL); err != nil || parsed.Scheme != "https" {
		return nil, fmt.Errorf("软件清单查询要求固定证书时必须使用 https:// 地址（当前为 %q）", cred.URL)
	}
	transport, err := pinningTransport(cred.Pin)
	if err != nil {
		return nil, err
	}
	code := cred.Token
	if bare, _, ok := SplitTokenPin(cred.Token); ok {
		code = bare
	}
	return &SoftwareInventoryForwarder{
		url:  strings.TrimRight(cred.URL, "/"),
		code: code,
		client: &http.Client{
			Transport: transport,
			Timeout:   softwareInventoryForwardTimeout,
			// 不跟随重定向：跟随就会离开固定证书的传输层，而且会重定向的
			// 「权威端」不是权威端。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		capable: true,
	}, nil
}

// Close 释放固定传输层安全连接池里的空闲连接。
func (f *SoftwareInventoryForwarder) Close() {
	if f.client != nil {
		f.client.CloseIdleConnections()
	}
}

// QuerySoftwareInventory 在线查询权威端；它同时满足 mcpserver 的注入接口。
func (f *SoftwareInventoryForwarder) QuerySoftwareInventory(
	in mcpserver.QuerySoftwareInventoryInput,
) (softwareinventory.QueryResult, error) {
	if !f.capable {
		if f.noCred {
			return softwareinventory.QueryResult{}, &softwareInventoryError{
				text: SoftwareInventoryUnreachableText, unreachable: true,
			}
		}
		return softwareinventory.QueryResult{}, &softwareInventoryError{
			text: softwareInventoryPlaintextText,
		}
	}
	body, err := json.Marshal(softwareInventoryRequest{
		Device:                in.Device,
		Name:                  in.Name,
		MinimumExposure:       in.MinimumExposure,
		Category:              in.Category,
		IncludeSystemPackages: in.IncludeSystemPackages,
	})
	if err != nil {
		return softwareinventory.QueryResult{}, &softwareInventoryError{
			text: SoftwareInventoryUnreachableText, err: err, unreachable: true,
		}
	}
	request, err := http.NewRequest(http.MethodPost, f.url+"/software-inventory", bytes.NewReader(body))
	if err != nil {
		return softwareinventory.QueryResult{}, &softwareInventoryError{
			text: SoftwareInventoryUnreachableText, err: err, unreachable: true,
		}
	}
	setAuthorityRequestHeaders(request, f.code)
	response, err := f.client.Do(request)
	if err != nil {
		return softwareinventory.QueryResult{}, &softwareInventoryError{
			text: SoftwareInventoryUnreachableText, err: err, unreachable: true,
		}
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		var result softwareinventory.QueryResult
		if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&result); err != nil {
			return softwareinventory.QueryResult{}, &softwareInventoryError{
				text: SoftwareInventoryUnreachableText, err: err, unreachable: true,
			}
		}
		if result.Devices == nil {
			result.Devices = []softwareinventory.QueryDeviceResult{}
		}
		return result, nil
	case http.StatusUnauthorized:
		return softwareinventory.QueryResult{}, &softwareInventoryError{text: softwareInventoryRejectedText}
	case http.StatusForbidden:
		return softwareinventory.QueryResult{}, &softwareInventoryError{text: softwareInventoryForbiddenText}
	case http.StatusNotFound:
		return softwareinventory.QueryResult{}, &softwareInventoryError{text: softwareInventoryUnsupportedText}
	default:
		return softwareinventory.QueryResult{}, &softwareInventoryError{
			text:        SoftwareInventoryUnreachableText,
			err:         fmt.Errorf("authority returned %s", http.StatusText(response.StatusCode)),
			unreachable: true,
		}
	}
}
