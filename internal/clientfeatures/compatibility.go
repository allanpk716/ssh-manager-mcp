package clientfeatures

import "errors"

// ErrTopologyAndSharedStorageAuthorityTooOld 是新客户端连着旧权威端时，网
// 络拓扑与共享存储入口共用的固定错误（ADR 0014：快照没有推荐版本字段
// 时，网络拓扑与共享存储工具整体降级）。
var ErrTopologyAndSharedStorageAuthorityTooOld = errors.New(
	"权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端",
)

// AuthorityTooOld 定义「权威端过旧」的唯一条件：当前实例的缓存快照没有
// 推荐版本，就说明权威端来自不写这个字段的旧版本。各调用方只在补齐自
// 己的上下文（是否客户端、是否只读缓存）后封装这个条件。
func AuthorityTooOld(recommendedVersion string) bool {
	return recommendedVersion == ""
}
