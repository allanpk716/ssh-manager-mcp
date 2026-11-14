package sharemount

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/sshbroker"
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

// sharedStorageMarkerOptions 生成 macOS 映射文件与撤销文件共用的标记文本：
//
//	sshmgr-share=<百分号编码的共享名>;sshmgr-version=<十进制版本号>
//
// 共享名按网址路径规则做百分号编码，因此逗号、分号、等号、斜杠与空格都
// 不会破坏字段边界。macOS autofs 可以把同一段文本作为独立标记行写入。
func sharedStorageMarkerOptions(shareName string, version int64) string {
	return "sshmgr-share=" + url.PathEscape(shareName) +
		";sshmgr-version=" + strconv.FormatInt(version, 10)
}

// linuxSharedStorageFstabMarkerOptions 生成 Linux fstab 里的管理标记：
//
//	x-sshmgr-share=<百分号编码的共享名>,x-sshmgr-version=<十进制版本号>
//
// util-linux mount(8) 约定 x- 前缀选项只属于用户空间，不会传给挂载帮助
// 程序和内核；直接使用 sshmgr- 前缀会让 CIFS 收到未知参数。
func linuxSharedStorageFstabMarkerOptions(shareName string, version int64) string {
	return "x-" + strings.ReplaceAll(
		sharedStorageMarkerOptions(shareName, version), ";", ",x-")
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

// revokedMarkerGuardScript 生成撤销标记检查片段。固定挂载命令必须在同一
// 把服务器级挂载锁内读取标记；标记版本号不低于本次共享存储版本号时拒
// 绝。标记不合法同样拒绝，避免把无法识别的撤销状态误当成可覆盖。
func revokedMarkerGuardScript(markerPath, outputMarker string, revokedExitCode, invalidExitCode int) string {
	return fmt.Sprintf(`sshmgr_revoked_marker=%s
sshmgr_revoked_version=0
if [ -r "$sshmgr_revoked_marker" ]; then
  IFS= read -r sshmgr_revoked_content < "$sshmgr_revoked_marker"
  sshmgr_revoked_prefix="sshmgr-share=${sshmgr_expected_share};sshmgr-version="
  case "$sshmgr_revoked_content" in
    "$sshmgr_revoked_prefix"*) ;;
    *)
      echo `+outputMarker+` invalid-revoked-marker >&2
      exit `+strconv.Itoa(invalidExitCode)+`
      ;;
  esac
  sshmgr_revoked_version=${sshmgr_revoked_content#"$sshmgr_revoked_prefix"}
  sshmgr_revoked_version=${sshmgr_revoked_version%;sshmgr-state=revoked}
  case "$sshmgr_revoked_version" in
    ''|*[!0-9]*)
      echo `+outputMarker+` invalid-revoked-marker >&2
      exit `+strconv.Itoa(invalidExitCode)+`
      ;;
  esac
fi
if [ "$sshmgr_revoked_version" -gt 0 ] && [ "$sshmgr_revoked_version" -ge "$sshmgr_expected_version" ]; then
  echo `+outputMarker+` share-revoked >&2
  exit `+strconv.Itoa(revokedExitCode)+`
fi
`, sshbroker.POSIXShellQuote(markerPath))
}
