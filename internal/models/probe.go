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

// probeErrorCategoryTable 是错误分类代号与呈现短语的单一对照表。呈现层
// 用它查中文短语，输出解析用它识别已知代号；空分类（连接成功）不在表
// 里，输入的空代号按「其他」处理。
var probeErrorCategoryTable = []struct {
	Category ProbeErrorCategory
	Label    string
}{
	{ProbeErrorConnectionRefused, "连接被拒绝"},
	{ProbeErrorTimeout, "连接超时"},
	{ProbeErrorNoRoute, "无路由"},
	{ProbeErrorDNSFailure, "名字解析失败"},
	{ProbeErrorMissingTool, "服务器缺少探测工具"},
	{ProbeErrorNoResult, "服务器未返回结果"},
	{ProbeErrorIncomplete, "探测未完成"},
	{ProbeErrorOther, "其他"},
}

// ProbeErrorCategoryLabel 返回错误分类的中文呈现短语；连接成功返回空字
// 符串，未知分类按「其他」。
func ProbeErrorCategoryLabel(category ProbeErrorCategory) string {
	if category == ProbeErrorNone {
		return ""
	}
	for _, item := range probeErrorCategoryTable {
		if item.Category == category {
			return item.Label
		}
	}
	return ProbeErrorCategoryLabel(ProbeErrorOther)
}

// NormalizeProbeErrorCategory 把远端输出或存储里的分类代号归一化为已知
// 值；未知代号（包括空字符串）归入「其他」。
func NormalizeProbeErrorCategory(value string) ProbeErrorCategory {
	category := ProbeErrorCategory(value)
	for _, item := range probeErrorCategoryTable {
		if item.Category == category {
			return category
		}
	}
	return ProbeErrorOther
}
