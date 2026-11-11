package clientfeatures

import "errors"

// ErrTopologyAndSharedStorageAuthorityTooOld 是新客户端连着旧权威端时，网
// 络拓扑与共享存储入口共用的固定错误。
var ErrTopologyAndSharedStorageAuthorityTooOld = errors.New(
	"权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端",
)

// AuthorityTooOld 是客户端判断权威端是否过旧的唯一入口。当前实例的缓存
// 快照没有推荐版本，就说明权威端来自不写这个字段的旧版本。
func AuthorityTooOld(recommendedVersion string) bool {
	return recommendedVersion == ""
}
