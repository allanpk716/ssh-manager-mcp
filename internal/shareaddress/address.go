package shareaddress

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// Address 是解析后的共享地址。主机与共享名转为小写供识别使用；子路径保
// 留原有大小写，并把分隔符统一成正斜杠。
type Address struct {
	Host      string
	ShareName string
	SubPath   string
}

// Resolved 是在调用方给定的共享存储集合里识别出的唯一登记，保留解析后
// 的子路径供后续翻译使用。
type Resolved struct {
	Address Address
	Share   *models.SharedStorage
}

// Parse 把 `\\<主机>\<共享名>\<子路径>` 与
// `smb://<主机>/<共享名>/<子路径>` 解析成同一种共享地址指称。
func Parse(input string) (Address, error) {
	value := strings.TrimSpace(input)
	if strings.HasPrefix(value, `\\`) {
		return parseSegments(value[2:])
	}
	if lower := strings.ToLower(value); strings.HasPrefix(lower, "smb://") {
		return parseSegments(value[len("smb://"):])
	}
	return Address{}, errors.New("共享地址必须写成 \\\\主机\\共享名\\子路径或 smb://主机/共享名/子路径")
}

// Resolve 按「主机加共享名」在给定共享存储中识别唯一登记。主机只匹配
// 提供方设备的网络成员地址；匹配到多处登记时报错并列出安全候选。
func Resolve(input string, shares []*models.SharedStorage) (*Resolved, error) {
	address, err := Parse(input)
	if err != nil {
		return nil, err
	}
	var candidates []*models.SharedStorage
	for _, share := range shares {
		if share == nil || !strings.EqualFold(share.Name, address.ShareName) {
			continue
		}
		for _, candidate := range share.CandidateAddresses {
			if strings.EqualFold(candidate, address.Host) {
				candidates = append(candidates, share)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("共享地址没有匹配当前会话可见的共享存储")
	}
	if len(candidates) > 1 {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].DeviceName != candidates[j].DeviceName {
				return candidates[i].DeviceName < candidates[j].DeviceName
			}
			return candidates[i].Name < candidates[j].Name
		})
		labels := make([]string, 0, len(candidates))
		for _, share := range candidates {
			labels = append(labels, fmt.Sprintf("%s：%s", share.DeviceName, share.Name))
		}
		return nil, fmt.Errorf("共享地址匹配多个共享存储，请 owner 澄清：%s", strings.Join(labels, "、"))
	}
	return &Resolved{Address: address, Share: candidates[0]}, nil
}

// PathOnServer 按服务器系统类型把共享子路径翻译成实际路径。Linux 与
// macOS 使用统一挂载点；Windows 使用共享存储登记的固定盘符。
func (resolved *Resolved) PathOnServer(shellType models.ShellType) string {
	mountPoint := store.SharedStorageMountPoint(resolved.Share.Drive, resolved.Share.Name, shellType)
	if resolved.Address.SubPath == "" {
		if shellType.IsWindows() {
			return mountPoint + `\`
		}
		return mountPoint
	}
	if shellType.IsWindows() {
		return mountPoint + `\` + strings.ReplaceAll(resolved.Address.SubPath, "/", `\`)
	}
	return mountPoint + "/" + resolved.Address.SubPath
}

func parseSegments(value string) (Address, error) {
	normalized := strings.ReplaceAll(value, `\`, "/")
	segments := make([]string, 0, 3)
	for _, segment := range strings.Split(normalized, "/") {
		if segment == "" {
			continue
		}
		segments = append(segments, segment)
	}
	if len(segments) < 2 {
		return Address{}, errors.New("共享地址缺少主机或共享名")
	}
	for _, segment := range segments[2:] {
		if segment == "." || segment == ".." {
			return Address{}, errors.New("共享地址的子路径不能包含「.」或「..」")
		}
	}
	return Address{
		Host:      strings.ToLower(segments[0]),
		ShareName: strings.ToLower(segments[1]),
		SubPath:   strings.Join(segments[2:], "/"),
	}, nil
}
