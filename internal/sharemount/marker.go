package sharemount

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ErrInvalidSharedStorageMarker 表示远端配置里的 sshmgr 标记缺失或格式不
// 对。这时不能判断条目属于哪个共享或哪个版本，绝不能改写它。
var ErrInvalidSharedStorageMarker = errors.New("远端配置里的 sshmgr 标记不合法")

// SharedStorageMarker 是写进远端持久配置的机器可读标记。Linux fstab、
// macOS autofs 与后续现场检测都使用同一套字段和编码。
type SharedStorageMarker struct {
	Share   string
	Version int64
}

// sharedStorageMarkerOptions 生成跨操作系统共用的标记文本：
//
//	sshmgr-share=<百分号编码的共享名>;sshmgr-version=<十进制版本号>
//
// 共享名按网址路径规则做百分号编码，因此逗号、分号、等号、斜杠与空格都
// 不会破坏字段边界。Linux fstab 使用逗号分隔选项，会把这两个字段拆成两
// 个选项；macOS autofs 可以把同一段文本作为独立标记行写入。
func sharedStorageMarkerOptions(shareName string, version int64) string {
	return "sshmgr-share=" + url.PathEscape(shareName) +
		";sshmgr-version=" + strconv.FormatInt(version, 10)
}

// parseSharedStorageMarkerOptions 解析 sharedStorageMarkerOptions 生成的标
// 记。解析保持严格：字段顺序固定、共享名必须能按百分号编码还原、版本号
// 必须是非负十进制整数。任何不合法标记都让调用方停止改写，避免误碰
// owner 手写配置。
func parseSharedStorageMarkerOptions(marker string) (SharedStorageMarker, error) {
	fields := strings.Split(marker, ";")
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "sshmgr-share=") ||
		!strings.HasPrefix(fields[1], "sshmgr-version=") {
		return SharedStorageMarker{}, fmt.Errorf("%w：%q", ErrInvalidSharedStorageMarker, marker)
	}
	shareValue := strings.TrimPrefix(fields[0], "sshmgr-share=")
	shareName, err := url.PathUnescape(shareValue)
	if err != nil || shareName == "" || shareValue == shareName && strings.ContainsAny(shareName, ",;=/ \t") {
		return SharedStorageMarker{}, fmt.Errorf("%w：%q", ErrInvalidSharedStorageMarker, marker)
	}
	versionValue := strings.TrimPrefix(fields[1], "sshmgr-version=")
	version, err := strconv.ParseInt(versionValue, 10, 64)
	if err != nil || version < 0 || versionValue != strconv.FormatInt(version, 10) {
		return SharedStorageMarker{}, fmt.Errorf("%w：%q", ErrInvalidSharedStorageMarker, marker)
	}
	return SharedStorageMarker{Share: shareName, Version: version}, nil
}
