package webui

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// ListenPlan 是监听地址推导结果。NeedsManualListen 为 true 时，日志与命令
// 行应提示 owner 使用 serve.ui_listen 手动指定地址。
type ListenPlan struct {
	Addresses         []string
	NeedsManualListen bool
}

// DeriveListenAddresses 推导权威端 Web UI 的监听地址：未设置手动地址时取
// 拓扑虚拟局域网成员地址与本机网卡地址的交集；无论哪种情况都并入回环地址。
func DeriveListenAddresses(topologyAddresses, interfaceAddresses []string, manualListen string) (ListenPlan, error) {
	addresses := make(map[string]struct{})
	if strings.TrimSpace(manualListen) != "" {
		for _, field := range strings.Split(manualListen, ",") {
			value := strings.TrimSpace(field)
			if value == "" {
				continue
			}
			ip := net.ParseIP(value)
			if ip == nil {
				return ListenPlan{}, fmt.Errorf("serve.ui_listen 含非法 IP 地址 %q", value)
			}
			addresses[ip.String()] = struct{}{}
		}
	} else {
		local := canonicalIPSet(interfaceAddresses)
		for _, address := range topologyAddresses {
			ip := net.ParseIP(address)
			if ip == nil {
				continue
			}
			if _, ok := local[ip.String()]; ok {
				addresses[ip.String()] = struct{}{}
			}
		}
	}
	loopback := net.ParseIP("127.0.0.1")
	addresses[loopback.String()] = struct{}{}
	out := make([]string, 0, len(addresses))
	for address := range addresses {
		out = append(out, address)
	}
	sort.Strings(out)
	return ListenPlan{
		Addresses:         out,
		NeedsManualListen: strings.TrimSpace(manualListen) == "" && len(out) == 1,
	}, nil
}

func canonicalIPSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if ip := net.ParseIP(value); ip != nil {
			out[ip.String()] = struct{}{}
		}
	}
	return out
}
