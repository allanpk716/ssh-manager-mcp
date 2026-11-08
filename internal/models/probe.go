package models

// ProbeErrorCategory 是连通探测与真实 SSH 连接顺带记录共用的错误分类。
// 本机 TCP 拨号与经服务器执行的固定命令输出同一套分类，便于呈现与后续
// 拓扑查询统一翻译。
type ProbeErrorCategory string

const (
	// ProbeErrorNone 表示连接成功，没有错误分类。
	ProbeErrorNone ProbeErrorCategory = ""
	// ProbeErrorConnectionRefused 表示目标端口明确拒绝连接。
	ProbeErrorConnectionRefused ProbeErrorCategory = "connection-refused"
	// ProbeErrorTimeout 表示在逐地址超时内没有完成 TCP 建连。
	ProbeErrorTimeout ProbeErrorCategory = "timeout"
	// ProbeErrorNoRoute 表示本机或起点服务器没有到目标网络的路由。
	ProbeErrorNoRoute ProbeErrorCategory = "no-route"
	// ProbeErrorDNSFailure 表示域名地址无法解析。
	ProbeErrorDNSFailure ProbeErrorCategory = "dns-failure"
	// ProbeErrorMissingTool 表示起点服务器缺少执行固定探测命令所需的能力。
	ProbeErrorMissingTool ProbeErrorCategory = "missing-tool"
	// ProbeErrorNoResult 表示服务器没有为某个地址返回结果行。
	ProbeErrorNoResult ProbeErrorCategory = "no-result"
	// ProbeErrorIncomplete 表示整次探测超过总时限，目标没有测完。
	ProbeErrorIncomplete ProbeErrorCategory = "incomplete"
	// ProbeErrorOther 表示未归入以上类别的失败，例如主机密钥不同。
	ProbeErrorOther ProbeErrorCategory = "other"
)

// ParseProbeErrorCategory 把存储或远端输出里的分类代号归一化为已知值；
// 未知代号保留原样，呈现层负责按「其他」处理。
func ParseProbeErrorCategory(value string) ProbeErrorCategory {
	return ProbeErrorCategory(value)
}
