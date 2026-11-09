package machineinit

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// ServeWebUIState 是 serve 独立 Web UI 的探测结果。Known 为 false 表示本
// 机没有可打开的 vault，端口与地址无法推导；Running 只在 Known 为 true 时
// 有意义。
type ServeWebUIState struct {
	Known             bool
	Port              int
	Addresses         []string
	NeedsManualListen bool
	Running           bool
}

// ProbeServeWebUI 探测 serve 的独立 Web UI：读取 `serve.ui_port`（默认
// 7879），按现有监听规划推导地址列表，再对本机回环地址上的端口发 HTTPS
// 请求（跳过证书校验，只判断是否在运行）。
func ProbeServeWebUI() (ServeWebUIState, error) {
	if !roles.VaultExists() {
		state := ServeWebUIState{Port: mcpserver.DefaultWebUIPort}
		state.Running = loopbackHTTPSResponds(state.Port)
		return state, nil
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return ServeWebUIState{}, err
	}
	defer st.Close()
	return probeServeWebUI(st)
}

// loopbackHTTPSResponds 对 127.0.0.1 上的端口发一次 HTTPS 请求。任何 HTTP
// 响应（包括 404、401）都算在运行；连接失败或超时算不在运行。
// InsecureSkipVerify 是刻意的：serve 使用自签证书，探测只判断端口是否有
// 服务响应，不校验身份，也不传输任何凭据。
func loopbackHTTPSResponds(port int) bool {
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 见上方说明
		},
	}
	address := fmt.Sprintf("127.0.0.1:%d", port)
	resp, err := client.Get("https://" + address + "/ui/")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return true
}
