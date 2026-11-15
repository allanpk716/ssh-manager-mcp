package cli

import (
	"bytes"
	"encoding/csv"
	"strconv"
	"strings"
)

// windowsTaskListHasServeProcess parses tasklist CSV output and reports
// whether a process other than the current one is named sshmgr.exe. tasklist
// includes the process that invoked it, so its process number must be ignored.
func windowsTaskListHasServeProcess(output []byte, currentProcessID int) bool {
	reader := csv.NewReader(bytes.NewReader(output))
	reader.FieldsPerRecord = -1 // 无匹配时 tasklist 只输出一行提示，字段数不固定
	records, err := reader.ReadAll()
	if err != nil {
		return false
	}
	for _, fields := range records {
		if len(fields) < 2 || !strings.EqualFold(strings.TrimSpace(fields[0]), "sshmgr.exe") {
			continue
		}
		processID, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			continue
		}
		if processID != currentProcessID {
			return true
		}
	}
	return false
}
