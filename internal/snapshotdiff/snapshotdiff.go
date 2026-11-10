// Package snapshotdiff 比较同一 profile 可见的新旧两份快照,产出给 agent
// 看的变更提示。比较环节由可追加的比较项组成:本包默认带服务器集合、网
// 络拓扑、机密项、命令策略与共享存储比较项,后续新领域只需往默认比较器
// 里加自己的比较项,不必改动换快照与提示通道。
package snapshotdiff

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// Item 是一个比较项:比较换下的旧快照与换上的新快照,返回零条或多条
// 差异描述;每条差异会成为变更提示里的一个分句。
type Item interface {
	Describe(old, fresh *store.Snapshot) []string
}

// Comparer 按注册顺序执行比较项,把差异拼成一条变更提示。
type Comparer struct {
	items []Item
}

// NewComparer 用给定比较项构造比较器。
func NewComparer(items ...Item) *Comparer {
	return &Comparer{items: append([]Item(nil), items...)}
}

// Default 返回默认比较器:当前 profile 可见的服务器集合、网络拓扑、机密
// 项、命令策略与共享存储。
func Default() *Comparer {
	return NewComparer(ServerSet{}, NetworkTopology{}, SecretSet{}, CommandPolicy{}, SharedStorageSet{})
}

// Hint 比较新旧快照并返回变更提示;没有任何差异时返回空字符串。提示只
// 含服务器名、机密项名字、机密项环境变量名、修订号与固定的网络拓扑、
// 命令策略说明,以及共享名、共享存储版本号、共享存储变更说明,不含地
// 址、凭据、机密项的值或共享存储挂载账号用户名。
func (c *Comparer) Hint(old, fresh *store.Snapshot) string {
	if c == nil || old == nil || fresh == nil {
		return ""
	}
	var parts []string
	for _, item := range c.items {
		parts = append(parts, item.Describe(old, fresh)...)
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：%s",
		old.SnapshotRevision, fresh.SnapshotRevision, strings.Join(parts, "；"))
}
