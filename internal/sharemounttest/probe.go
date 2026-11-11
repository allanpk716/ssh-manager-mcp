package sharemounttest

import (
	"fmt"
	"strings"
)

// probeOutput 按固定探测命令输出协议模拟目标服务器上的 TCP 445 探测。
func probeOutput(command string, reachableAddresses []string) string {
	reachable := make(map[string]bool, len(reachableAddresses))
	for _, address := range reachableAddresses {
		reachable[address] = true
	}
	var output strings.Builder
	for _, line := range strings.Split(command, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "sshmgr_probe_one" {
			continue
		}
		if reachable[fields[2]] {
			fmt.Fprintf(&output, "SSHMGR-PROBE %s ok 1\n", fields[1])
		} else {
			fmt.Fprintf(&output, "SSHMGR-PROBE %s fail 1 network\n", fields[1])
		}
	}
	return output.String()
}

// firstCandidateAddress 取出固定重选地址命令里的第一个候选地址。
func firstCandidateAddress(command string) string {
	for _, line := range strings.Split(command, "\n") {
		if value, found := strings.CutPrefix(line, "candidate_1_address="); found {
			return strings.Trim(value, "'")
		}
	}
	return ""
}
