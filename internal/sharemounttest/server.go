package sharemounttest

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func requestedVersion(command string) int64 {
	for _, field := range strings.Fields(command) {
		if value, found := strings.CutPrefix(field, "sshmgr_version="); found {
			if parsed, err := strconv.ParseInt(strings.Trim(value, "'"), 10, 64); err == nil {
				return parsed
			}
		}
	}
	return 0
}

func revokedMarker(
	shareName string, currentVersion int64, command, directory string,
) (content, path string, version int64) {
	version = requestedVersion(command)
	if version == 0 {
		version = currentVersion + 1
	}
	encodedName := url.PathEscape(shareName)
	content = fmt.Sprintf(
		"sshmgr-share=%s;sshmgr-version=%d;sshmgr-state=revoked", encodedName, version,
	)
	return content, directory + "/" + encodedName + ".revoked", version
}
