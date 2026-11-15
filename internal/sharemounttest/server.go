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

func requestedExpectedShare(command string) string {
	for _, field := range strings.Fields(command) {
		if value, found := strings.CutPrefix(field, "sshmgr_expected_share="); found {
			return strings.Trim(value, "'")
		}
	}
	return ""
}

func revokedMarkerContent(content string) (share string, version int64, valid bool) {
	fields := strings.Split(content, ";")
	if len(fields) != 3 {
		return "", 0, false
	}
	shareValue, found := strings.CutPrefix(fields[0], "sshmgr-share=")
	if !found || shareValue == "" {
		return "", 0, false
	}
	versionValue, found := strings.CutPrefix(fields[1], "sshmgr-version=")
	if !found {
		return "", 0, false
	}
	parsedVersion, err := strconv.ParseInt(versionValue, 10, 64)
	if err != nil || parsedVersion <= 0 || fields[2] != "sshmgr-state=revoked" {
		return "", 0, false
	}
	return shareValue, parsedVersion, true
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
