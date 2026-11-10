package softwareinventory

// 这个文件承载所有操作系统解析器共用的固定命令集输出协议层。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// ParsedCollection 是一份固定命令集输出的解析结果。某一节缺失、非零退
// 出码或输出被截断只影响该节：已解析出的条目保留，整体记为不完整。
type ParsedCollection struct {
	CommandSetVersion string
	SystemType        string
	Incomplete        bool
	Notes             []string
	Items             []models.SoftwareObservedItem
}

// sectionOutcome 描述一节输出在协议层的状态。
type sectionOutcome string

const (
	sectionComplete   sectionOutcome = "complete"
	sectionAbsent     sectionOutcome = "absent"
	sectionIncomplete sectionOutcome = "incomplete"
)

type parsedSection struct {
	name    string
	outcome sectionOutcome
	lines   []string
}

// sectionOrEmpty 返回一节的值；节缺失时返回零值，让后续构造函数按“不完整
// 的节”处理。
func sectionOrEmpty(sections map[string]*parsedSection, name string) parsedSection {
	if section := sections[name]; section != nil {
		return *section
	}
	return parsedSection{}
}

// splitInventorySections 按行首标记把输出切成节。返回值依次是：节名到节
// 的映射、开头标记里的命令集版本、是否看到结尾标记。
func splitInventorySections(output string) (map[string]*parsedSection, string, bool, error) {
	sections := map[string]*parsedSection{}
	version := ""
	sawBegin, sawEnd := false, false
	var current *parsedSection
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == InventoryMarker {
			switch fields[1] {
			case "BEGIN":
				if len(fields) < 3 {
					continue
				}
				sawBegin = true
				version = fields[2]
			case "END":
				sawEnd = true
				current = nil
			case "SECTION":
				if len(fields) < 3 {
					continue
				}
				name := fields[2]
				sections[name] = &parsedSection{name: name, outcome: sectionIncomplete}
				current = sections[name]
				continue
			case "STATUS":
				if len(fields) >= 4 {
					section, present := sections[fields[2]]
					if !present {
						continue
					}
					if code, err := strconv.Atoi(fields[3]); err == nil && code == 0 {
						section.outcome = sectionComplete
					} else {
						section.outcome = sectionIncomplete
					}
				}
				current = nil
			case "ABSENT":
				if len(fields) < 3 {
					continue
				}
				name := fields[2]
				sections[name] = &parsedSection{name: name, outcome: sectionAbsent}
				current = nil
			}
			continue
		}
		if current != nil {
			current.lines = append(current.lines, line)
		}
	}
	if !sawBegin {
		return nil, "", false, fmt.Errorf("固定命令集输出缺少 %s BEGIN 开头标记", InventoryMarker)
	}
	return sections, version, sawEnd, nil
}

// sortObservedPortsByName 给监听端口一个稳定顺序，避免同一份数据两次采集
// 因为端口顺序不同而被当成变化。
func sortObservedPortsByName(ports []models.SoftwareListeningPort) {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		if ports[i].Protocol != ports[j].Protocol {
			return ports[i].Protocol < ports[j].Protocol
		}
		return ports[i].Address < ports[j].Address
	})
}
