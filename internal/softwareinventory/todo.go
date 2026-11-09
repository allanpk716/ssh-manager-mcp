package softwareinventory

import (
	"sort"
	"time"
)

// PendingExposurePort 是一条需要 owner 确认真实暴露级别的实测监听端口。
type PendingExposurePort struct {
	ListeningPort
	ObservedStableKey string `json:"observed_stable_key"`
}

// PendingExposureDevice 是按设备分组后的待确认暴露待办。
type PendingExposureDevice struct {
	DeviceID   string                `json:"device_id"`
	DeviceName string                `json:"device_name"`
	Ports      []PendingExposurePort `json:"ports"`
}

// UnidentifiedProcessTodo 是一个尚未被声明认领的未识别进程。
type UnidentifiedProcessTodo struct {
	DeviceID          string          `json:"device_id"`
	DeviceName        string          `json:"device_name"`
	ObservedStableKey string          `json:"observed_stable_key"`
	Name              string          `json:"name"`
	ProcessName       string          `json:"process_name,omitempty"`
	ExecutablePath    string          `json:"executable_path,omitempty"`
	ListeningPorts    []ListeningPort `json:"listening_ports,omitempty"`
}

// MismatchTodo 是受管设备上一条没有对应当前实测的生效声明。
type MismatchTodo struct {
	DeviceID           string `json:"device_id"`
	DeviceName         string `json:"device_name"`
	DeclarationID      string `json:"declaration_id"`
	Name               string `json:"name"`
	ClaimedObservedKey string `json:"claimed_observed_key,omitempty"`
	Reason             string `json:"reason"`
}

// StaleDeviceTodo 是一台清单过期或从未成功采集的受管设备。
type StaleDeviceTodo struct {
	DeviceID       string     `json:"device_id"`
	DeviceName     string     `json:"device_name"`
	NeverCollected bool       `json:"never_collected"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt  *time.Time `json:"last_failure_at,omitempty"`
	FailureReason  string     `json:"failure_reason,omitempty"`
}

// TodoResult 是软件清单的三类待办。
type TodoResult struct {
	PendingExposure       []PendingExposureDevice   `json:"pending_exposure"`
	UnidentifiedProcesses []UnidentifiedProcessTodo `json:"unidentified_processes"`
	Mismatches            []MismatchTodo            `json:"mismatches"`
	StaleDevices          []StaleDeviceTodo         `json:"stale_devices"`
}

// Todos 按可见设备范围计算三类待办。
func (m *Manager) Todos(visibility Visibility) (TodoResult, error) {
	overviews, err := m.st.SoftwareDeviceOverviews(visibility)
	if err != nil {
		return TodoResult{}, err
	}
	declarations, err := m.st.SearchSoftwareDeclarations("", visibility)
	if err != nil {
		return TodoResult{}, err
	}
	observedInventories, err := m.st.SoftwareObservedItemsForScope(visibility)
	if err != nil {
		return TodoResult{}, err
	}

	overviewByID := make(map[string]DeviceOverview, len(overviews))
	declarationByKey := make(map[string]SoftwareDeclarationView)
	for _, overview := range overviews {
		overviewByID[overview.ID] = overview
	}
	for _, declaration := range declarations {
		if declaration.ClaimedObservedKey != "" {
			declarationByKey[declaration.DeviceID+"\x1f"+declaration.ClaimedObservedKey] = declaration
		}
	}

	result := TodoResult{
		PendingExposure: []PendingExposureDevice{}, UnidentifiedProcesses: []UnidentifiedProcessTodo{},
		Mismatches: []MismatchTodo{}, StaleDevices: []StaleDeviceTodo{},
	}
	pendingByDevice := make(map[string]int)
	for _, overview := range overviews {
		inventory := observedInventories[overview.ID]
		for _, item := range inventory.Items {
			if item.Category == CategoryUnidentified {
				claimKey := overview.ID + "\x1f" + item.StableKey
				if _, claimed := declarationByKey[claimKey]; !claimed {
					result.UnidentifiedProcesses = append(result.UnidentifiedProcesses, UnidentifiedProcessTodo{
						DeviceID: overview.ID, DeviceName: overview.Name,
						ObservedStableKey: item.StableKey, Name: item.Name,
						ProcessName: item.ProcessName, ExecutablePath: item.ExecutablePath,
						ListeningPorts: append([]ListeningPort(nil), item.ListeningPorts...),
					})
				}
			}
			declaration := declarationByKey[overview.ID+"\x1f"+item.StableKey]
			for _, port := range item.ListeningPorts {
				if port.Exposure != ExposureAllAddresses || hasDeclaredExposure(declaration, port) {
					continue
				}
				position, ok := pendingByDevice[overview.ID]
				if !ok {
					result.PendingExposure = append(result.PendingExposure, PendingExposureDevice{
						DeviceID: overview.ID, DeviceName: overview.Name, Ports: []PendingExposurePort{},
					})
					position = len(result.PendingExposure) - 1
					pendingByDevice[overview.ID] = position
				}
				result.PendingExposure[position].Ports = append(
					result.PendingExposure[position].Ports,
					PendingExposurePort{ListeningPort: port, ObservedStableKey: item.StableKey},
				)
			}
		}
	}

	observedByKey := make(map[string]bool)
	for _, inventory := range observedInventories {
		for _, item := range inventory.Items {
			observedByKey[inventory.DeviceID+"\x1f"+item.StableKey] = true
		}
	}
	for _, declaration := range declarations {
		overview, ok := overviewByID[declaration.DeviceID]
		if !ok || !overview.Managed {
			continue
		}
		if observedByKey[declaration.DeviceID+"\x1f"+declaration.ClaimedObservedKey] {
			continue
		}
		reason := "声明没有认领实测条目"
		if declaration.ClaimedObservedKey != "" {
			reason = "认领的实测条目已消失"
		}
		result.Mismatches = append(result.Mismatches, MismatchTodo{
			DeviceID: declaration.DeviceID, DeviceName: declaration.DeviceName,
			DeclarationID: declaration.ID, Name: declaration.Name,
			ClaimedObservedKey: declaration.ClaimedObservedKey, Reason: reason,
		})
	}
	for _, overview := range overviews {
		if !overview.Managed {
			continue
		}
		status := m.collectionStatusFor(overview.Name)
		if !status.Stale {
			continue
		}
		result.StaleDevices = append(result.StaleDevices, StaleDeviceTodo{
			DeviceID: overview.ID, DeviceName: overview.Name,
			NeverCollected: status.NeverCollected, LastSuccessAt: status.LastSuccessAt,
			LastFailureAt: status.LastFailureAt, FailureReason: status.LastFailureReason,
		})
	}

	sort.Slice(result.PendingExposure, func(i, j int) bool {
		return result.PendingExposure[i].DeviceName < result.PendingExposure[j].DeviceName
	})
	sort.Slice(result.UnidentifiedProcesses, func(i, j int) bool {
		if result.UnidentifiedProcesses[i].DeviceName != result.UnidentifiedProcesses[j].DeviceName {
			return result.UnidentifiedProcesses[i].DeviceName < result.UnidentifiedProcesses[j].DeviceName
		}
		return result.UnidentifiedProcesses[i].ObservedStableKey < result.UnidentifiedProcesses[j].ObservedStableKey
	})
	sort.Slice(result.Mismatches, func(i, j int) bool {
		if result.Mismatches[i].DeviceName != result.Mismatches[j].DeviceName {
			return result.Mismatches[i].DeviceName < result.Mismatches[j].DeviceName
		}
		return result.Mismatches[i].Name < result.Mismatches[j].Name
	})
	sort.Slice(result.StaleDevices, func(i, j int) bool {
		return result.StaleDevices[i].DeviceName < result.StaleDevices[j].DeviceName
	})
	return result, nil
}

// hasDeclaredExposure 判断声明是否给同一个实测端口提供了暴露级别。
func hasDeclaredExposure(declaration SoftwareDeclarationView, port ListeningPort) bool {
	for _, declared := range declaration.ListeningPorts {
		if declared.Port == port.Port && declared.Protocol == port.Protocol {
			return true
		}
	}
	return false
}
