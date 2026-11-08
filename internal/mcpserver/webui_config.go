package mcpserver

import (
	"fmt"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

const (
	settingWebUIPort   = "serve.ui_port"
	settingWebUIListen = "serve.ui_listen"
	defaultWebUIPort   = 7879
)

// ReadWebUIConfig 读取权威端 Web UI 的端口与手动监听地址设置。端口未设置时
// 使用默认值 7879；手动地址保持原样，由地址推导纯函数统一校验。
func ReadWebUIConfig(st *store.Store) (int, string, error) {
	portText, ok, err := st.GetSetting(settingWebUIPort)
	if err != nil {
		return 0, "", err
	}
	port := defaultWebUIPort
	if ok && strings.TrimSpace(portText) != "" {
		port, err = strconv.Atoi(strings.TrimSpace(portText))
		if err != nil || port <= 0 || port > 65535 {
			return 0, "", fmt.Errorf("serve.ui_port 必须是 1 到 65535 的端口：%q", portText)
		}
	}
	manualListen, ok, err := st.GetSetting(settingWebUIListen)
	if err != nil {
		return 0, "", err
	}
	if !ok {
		manualListen = ""
	}
	return port, manualListen, nil
}

// ReadWebUIListenPlan 读取设置并按当前拓扑与本机网卡推导 Web UI 地址，供
// serve 进程、`serve admin status` 与 `ui` 命令显示同一个结果。
func ReadWebUIListenPlan(st *store.Store) (int, webui.ListenPlan, error) {
	port, manualListen, err := ReadWebUIConfig(st)
	if err != nil {
		return 0, webui.ListenPlan{}, err
	}
	plan, err := webUIListenPlan(st, manualListen)
	return port, plan, err
}
