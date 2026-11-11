// Package clientfeatures 维护客户端各功能的起始支持版本，并按设备码最近
// 声明的客户端版本推算旧版标签。
package clientfeatures

import (
	"strings"

	"ssh-manager-mcp/internal/updater"
)

// Feature 描述一项客户端功能与第一个支持它的正式发布版本。
type Feature struct {
	Name         string
	SinceVersion string
}

// DefaultFeatures 是功能起始支持版本的集中配置点。列表顺序就是旧版标签
// 里的功能展示顺序；后续新功能只在这里追加一行。
var DefaultFeatures = []Feature{
	{Name: "机密项", SinceVersion: "v0.22.0"},
	{Name: "网络拓扑", SinceVersion: "v0.22.0"},
	{Name: "共享存储", SinceVersion: "v0.22.0"},
}

// ClientVersionLabel 返回设备码列表使用的客户端版本展示文字。
func ClientVersionLabel(clientVersion string) string {
	return ClientVersionLabelWithFeatures(clientVersion, DefaultFeatures)
}

// ClientVersionLabelWithFeatures 用调用方传入的功能表推算展示文字，测试
// 可以用不同起始版本覆盖边界；生产展示必须使用 DefaultFeatures。
func ClientVersionLabelWithFeatures(clientVersion string, features []Feature) string {
	if clientVersion == "" {
		return oldClientLabel(features)
	}
	client, err := updater.ParseVersion(clientVersion)
	if err != nil {
		return clientVersion
	}
	missing := make([]string, 0, len(features))
	for _, feature := range features {
		since, err := updater.ParseVersion(feature.SinceVersion)
		if err != nil {
			return clientVersion
		}
		if updater.CompareVersions(client, since) < 0 {
			missing = append(missing, feature.Name)
		}
	}
	if len(missing) == 0 {
		return clientVersion
	}
	return clientVersion + "（旧版客户端:不支持" + strings.Join(missing, "、") + "）"
}

func oldClientLabel(features []Feature) string {
	names := make([]string, 0, len(features))
	for _, feature := range features {
		names = append(names, feature.Name)
	}
	return "旧版客户端:不支持" + strings.Join(names, "、")
}
