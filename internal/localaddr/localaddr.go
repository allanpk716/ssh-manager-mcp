// Package localaddr 判断一个地址是否属于本机。配对登记接口与配对批准
// 入口必须使用同一套规则，避免客户端登记时与 owner 批准时得到不同结论。
package localaddr

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// PublicAddressesSettingKey 是「本机对外地址」设置项的键名。地址判定与设置
// 校验共用这一个常量，避免两处写出不同的键名。
const PublicAddressesSettingKey = "serve.public_addresses"

// RegisteredAddress 是 owner 登记的本机对外地址。Port 为零表示任意端口。
type RegisteredAddress struct {
	Host string
	Port int
}

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

// ParseRegisteredAddresses 解析逗号分隔的本机对外地址列表。条目可以是主
// 机名、IP 地址、主机名:端口、[IPv6 地址] 或 [IPv6 地址]:端口；返回规范化
// 后的列表。空列表是合法值，表示没有登记任何对外地址。
func ParseRegisteredAddresses(value string) ([]RegisteredAddress, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		if value == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("设置项 %s 的地址列表不能是空白", PublicAddressesSettingKey)
	}
	parts := strings.Split(trimmed, ",")
	out := make([]RegisteredAddress, 0, len(parts))
	for _, part := range parts {
		address, err := parseRegisteredAddress(part)
		if err != nil {
			return nil, err
		}
		out = append(out, address)
	}
	return out, nil
}

// FormatRegisteredAddresses 把解析后的地址列表转回设置存储使用的规范文本。
func FormatRegisteredAddresses(addresses []RegisteredAddress) string {
	parts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		host := address.Host
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		if address.Port == 0 {
			parts = append(parts, host)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d", host, address.Port))
	}
	return strings.Join(parts, ",")
}

func parseRegisteredAddress(value string) (RegisteredAddress, error) {
	entry := strings.TrimSpace(value)
	if entry == "" {
		return RegisteredAddress{}, fmt.Errorf("设置项 %s 的地址列表不能有空条目", PublicAddressesSettingKey)
	}
	host := entry
	port := 0
	switch {
	case strings.HasPrefix(entry, "[") && strings.HasSuffix(entry, "]"):
		host = strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]")
	case strings.HasPrefix(entry, "["):
		parsedHost, portText, err := net.SplitHostPort(entry)
		if err != nil {
			return RegisteredAddress{}, fmt.Errorf("设置项 %s 的 IPv6 地址必须成对使用方括号或写成 [地址]:端口，当前输入 %q", PublicAddressesSettingKey, entry)
		}
		parsedPort, err := parseRegisteredPort(portText)
		if err != nil {
			return RegisteredAddress{}, err
		}
		host, port = parsedHost, parsedPort
	case strings.HasSuffix(entry, "]"):
		return RegisteredAddress{}, fmt.Errorf("设置项 %s 的 IPv6 地址必须成对使用方括号，当前输入 %q", PublicAddressesSettingKey, entry)
	case strings.Count(entry, ":") == 1:
		parsedHost, portText, err := net.SplitHostPort(entry)
		if err != nil {
			return RegisteredAddress{}, fmt.Errorf("设置项 %s 的地址 %q 不合法", PublicAddressesSettingKey, entry)
		}
		parsedPort, err := parseRegisteredPort(portText)
		if err != nil {
			return RegisteredAddress{}, err
		}
		host, port = parsedHost, parsedPort
	case strings.Count(entry, ":") > 1:
		// 不带方括号的 IPv6 地址没有端口，整串就是主机。
	default:
		// 普通主机名或 IP 地址，没有端口。
	}
	if host == "" || strings.ContainsAny(host, "/?# \t\r\n") {
		return RegisteredAddress{}, fmt.Errorf("设置项 %s 的地址 %q 不合法", PublicAddressesSettingKey, entry)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return RegisteredAddress{}, fmt.Errorf("设置项 %s 不能登记环回或未指定地址，当前输入 %q", PublicAddressesSettingKey, entry)
		}
		host = ip.String()
	} else if strings.Contains(host, ":") {
		return RegisteredAddress{}, fmt.Errorf("设置项 %s 的 IPv6 地址必须能解析，当前输入 %q", PublicAddressesSettingKey, entry)
	} else {
		host = strings.ToLower(host)
		if host == "localhost" {
			return RegisteredAddress{}, fmt.Errorf("设置项 %s 不能登记环回或未指定地址，当前输入 %q", PublicAddressesSettingKey, entry)
		}
	}
	return RegisteredAddress{Host: host, Port: port}, nil
}

func parseRegisteredPort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("设置项 %s 的端口必须是 1 到 65535，当前输入 %q", PublicAddressesSettingKey, value)
	}
	return port, nil
}

// ForeignTarget 判断 targetURL 是否不属于本机地址集合。集合包括本机非环回
// IP、本机主机名，以及调用方传入的已登记对外地址。解析失败或主机为空时按
// 不属于本机处理；环回地址永远不算配对目标里的本机地址，也不能登记。
func ForeignTarget(targetURL string, registered ...RegisteredAddress) bool {
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
		for _, address := range registered {
			registeredIP := net.ParseIP(address.Host)
			if registeredIP != nil && registeredIP.Equal(ip) && portMatches(parsed.Port(), address.Port) {
				return false
			}
		}
		return true
	}
	for _, address := range registered {
		if net.ParseIP(address.Host) == nil && strings.EqualFold(host, address.Host) &&
			portMatches(parsed.Port(), address.Port) {
			return false
		}
	}
	hostname, err := os.Hostname()
	return err != nil || hostname == "" || !strings.EqualFold(host, hostname)
}

func portMatches(targetPort string, registeredPort int) bool {
	if registeredPort == 0 {
		return true
	}
	port, err := strconv.Atoi(targetPort)
	return err == nil && port == registeredPort
}
