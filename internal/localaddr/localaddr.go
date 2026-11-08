// Package localaddr 判断一个地址是否属于本机。配对登记接口与配对批准
// 入口必须使用同一套规则，避免客户端登记时与 owner 批准时得到不同结论。
package localaddr

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// NonLoopbackIPs 返回本机全部非环回的单播 IP 地址。
func NonLoopbackIPs() []net.IP {
	var out []net.IP
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, address := range interfaces {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.IsLoopback() {
			continue
		}
		out = append(out, network.IP)
	}
	return out
}

// ForeignTarget 判断 targetURL 是否不属于本机地址。解析失败或主机为空时
// 按不属于本机处理；环回地址刻意不算配对目标里的本机地址。
func ForeignTarget(targetURL string) bool {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return true
	}
	host := parsed.Hostname()
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, local := range NonLoopbackIPs() {
			if local.Equal(ip) {
				return false
			}
		}
		return true
	}
	hostname, err := os.Hostname()
	return err != nil || hostname == "" || !strings.EqualFold(host, hostname)
}
