package clientops

// 软件清单各转发接口共用的 HTTP 请求入口：统一携带上下文、设备码与固定
// 请求头；传输失败和异常服务端状态都标记为「权威端不可达」。

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
)

type softwareInventoryStatusClassifier func(response *http.Response) error

func softwareInventoryForwardRequest(
	ctx context.Context,
	client *http.Client,
	url, code, path string,
	body []byte,
	unreachableText string,
	classify softwareInventoryStatusClassifier,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url+path, bytes.NewReader(body),
	)
	if err != nil {
		return nil, &softwareInventoryError{text: unreachableText, err: err, unreachable: true}
	}
	setAuthorityRequestHeaders(request, code)
	response, err := client.Do(request)
	if err != nil {
		return nil, &softwareInventoryError{text: unreachableText, err: err, unreachable: true}
	}
	if err := classify(response); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	return response, nil
}

func softwareInventoryHTTPUnreachable(response *http.Response, text string) error {
	return &softwareInventoryError{
		text:        text,
		err:         fmt.Errorf("authority returned %s", http.StatusText(response.StatusCode)),
		unreachable: true,
	}
}

func httpStatusError(status int) error {
	return fmt.Errorf("authority returned %s", http.StatusText(status))
}
