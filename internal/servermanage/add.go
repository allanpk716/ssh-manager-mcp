package servermanage

import (
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// NetworkMembershipInput 是新增服务器时随新设备登记的一条网络成员。
type NetworkMembershipInput struct {
	Network string
	Address string
}

// AddRequest 是一次新增服务器提交。DeviceName 为空表示自动生成同名设备；
// LoginCredential 与 SudoCredential 为空表示本次不写入对应凭据。
type AddRequest struct {
	Server          models.Server
	LoginCredential *models.Credential
	SudoCredential  *models.Credential
	DeviceName      string
	SiteName        string
	// ExistingDevice 为真时 DeviceName 必须已经存在；为假时 DeviceName 为
	// 空表示自动生成同名设备，非空则按这个名字新建设备。
	ExistingDevice bool
	Memberships    []NetworkMembershipInput
	ProfileIDs     []string
}

// AddResult 是新增服务器后的可展示结果；ProfileNames 按保存顺序列出授权
// 成功的 profile 名字。
type AddResult struct {
	ServerID      string
	ServerName    string
	DeviceName    string
	CreatedDevice bool
	ProfileNames  []string
	AnchorCopy    *store.ServerAnchorCopy
}

// Add 保存一台服务器、所属设备、新设备的网络成员与 profile 授权。先做整
// 份校验，再复用存储层与访问管理模块的既有写路径；凭据与服务器条目由存储
// 层在同一个事务里写入。
func (m *Manager) Add(req AddRequest, src Source) (*AddResult, error) {
	prepared, err := m.prepareAdd(req)
	if err != nil {
		_ = m.writeAudit("server.add", addAuditSummary(req), err, src)
		return nil, err
	}
	var added *store.AddServerDeviceResult
	if req.ExistingDevice {
		added, err = m.st.AddServerWithCredentialsToDevice(
			&prepared.server, req.LoginCredential, req.SudoCredential, prepared.deviceName,
		)
	} else {
		added, err = m.st.AddServerWithCredentialsToNewDeviceWithSource(
			&prepared.server, req.LoginCredential, req.SudoCredential, prepared.deviceName, storeWriteSource(src),
		)
	}
	if err != nil {
		_ = m.writeAudit("server.add", addAuditSummary(req), err, src)
		return nil, err
	}
	for _, membership := range prepared.memberships {
		if _, err := m.st.AddNetworkMembershipWithSource(
			added.Device.Name, membership.Network, membership.Address, storeWriteSource(src),
		); err != nil {
			return nil, err
		}
	}
	profileNames := make([]string, 0, len(req.ProfileIDs))
	if len(req.ProfileIDs) > 0 {
		profiles, err := m.st.ListProfiles()
		if err != nil {
			return nil, err
		}
		profileNameByID := make(map[string]string, len(profiles))
		for _, profile := range profiles {
			profileNameByID[profile.ID] = profile.Name
		}
		accessManager := access.New(m.st)
		for _, profileID := range req.ProfileIDs {
			_, err := accessManager.GrantServers(profileID, []string{added.ServerID}, accessSource(src))
			if err != nil {
				return nil, err
			}
			profileNames = append(profileNames, profileNameByID[profileID])
		}
	}
	if err := m.writeAudit("server.add", addAuditSummary(req), nil, src); err != nil {
		return nil, err
	}
	return &AddResult{
		ServerID: added.ServerID, ServerName: prepared.server.Name,
		DeviceName: added.Device.Name, CreatedDevice: added.CreatedDevice,
		ProfileNames: profileNames,
		AnchorCopy:   added.AnchorCopy,
	}, nil
}

func storeWriteSource(src Source) store.WriteSource {
	if src.kind == store.AuditSourceWebUI {
		return store.WebUISource(src.addr)
	}
	return store.CLISource()
}

type preparedAdd struct {
	server      models.Server
	deviceName  string
	memberships []NetworkMembershipInput
}

func (m *Manager) prepareAdd(req AddRequest) (preparedAdd, error) {
	req.Server.Name = strings.TrimSpace(req.Server.Name)
	req.Server.User = strings.TrimSpace(req.Server.User)
	req.Server.Host = strings.TrimSpace(req.Server.Host)
	req.DeviceName = strings.TrimSpace(req.DeviceName)
	if req.Server.Name == "" || req.Server.User == "" {
		return preparedAdd{}, &FieldError{Field: "name", CLI: "server name and user are required", Page: "名称与用户不能为空。"}
	}
	existingServers, err := m.st.ListServers()
	if err != nil {
		return preparedAdd{}, err
	}
	for _, srv := range existingServers {
		if srv.Name == req.Server.Name {
			return preparedAdd{}, &FieldError{
				Field: "name", CLI: "server name already exists",
				Page: "服务器名称已存在：" + req.Server.Name + "。",
			}
		}
	}
	if req.Server.Port == 0 {
		req.Server.Port = 22
	}
	if req.Server.ShellType == "" {
		req.Server.ShellType = models.ShellPOSIX
	}
	if req.Server.Port < 1 || req.Server.Port > 65535 {
		return preparedAdd{}, &FieldError{Field: "port", CLI: "server port is invalid", Page: "端口必须在 1 到 65535 之间。"}
	}
	if _, err := models.ParseShellType(string(req.Server.ShellType)); err != nil {
		return preparedAdd{}, &FieldError{Field: "shell", CLI: err.Error(), Page: "shell 类型无效（可选 posix、powershell、cmd）。"}
	}
	if err := validateAddCredentials(req); err != nil {
		return preparedAdd{}, err
	}
	networks, err := m.st.ListNetworks(store.NetworkFilter{})
	if err != nil {
		return preparedAdd{}, err
	}
	networkByName := make(map[string]models.Network, len(networks))
	for _, network := range networks {
		networkByName[network.Name] = *network
	}
	memberships := make([]NetworkMembershipInput, 0, len(req.Memberships))
	seen := make(map[string]struct{})
	for _, membership := range req.Memberships {
		membership.Network = strings.TrimSpace(membership.Network)
		membership.Address = strings.TrimSpace(membership.Address)
		if membership.Network == "" && membership.Address == "" {
			continue
		}
		if membership.Network == "" || membership.Address == "" {
			return preparedAdd{}, &FieldError{Field: "memberships", CLI: "network membership requires network and address", Page: "网络成员必须同时填写网络与地址。"}
		}
		network, ok := networkByName[membership.Network]
		if !ok {
			return preparedAdd{}, &FieldError{Field: "memberships", CLI: fmt.Sprintf("network %q not found", membership.Network), Page: "网络不存在：" + membership.Network + "。"}
		}
		key := strings.ToLower(network.Name + "/" + membership.Address)
		if _, duplicate := seen[key]; duplicate {
			return preparedAdd{}, &FieldError{Field: "memberships", CLI: "duplicate network address in request", Page: "同一网络里提交了重复地址：" + membership.Address + "。"}
		}
		seen[key] = struct{}{}
		existing, err := m.st.ListNetworkMembershipsForNetwork(network.Name)
		if err != nil {
			return preparedAdd{}, err
		}
		for _, item := range existing {
			if strings.EqualFold(item.Address, membership.Address) {
				return preparedAdd{}, &FieldError{
					Field: "memberships",
					CLI:   fmt.Sprintf("network %s address %s is used by device %s", network.Name, membership.Address, item.DeviceName),
					Page:  fmt.Sprintf("网络 %s 的地址 %s 已被设备 %s 占用。", network.Name, membership.Address, item.DeviceName),
				}
			}
		}
		memberships = append(memberships, membership)
	}
	if req.SiteName != "" {
		physical := ""
		for _, membership := range memberships {
			if network, ok := networkByName[membership.Network]; ok && network.Type == models.NetworkPhysical {
				physical = membership.Network
				if network.SiteName != req.SiteName {
					return preparedAdd{}, &FieldError{
						Field: "site", CLI: "physical network does not belong to site",
						Page: "物理局域网不属于所选站点，请重新选择。",
					}
				}
				break
			}
		}
		if physical == "" {
			return preparedAdd{}, &FieldError{
				Field: "site", CLI: "site requires a physical network membership",
				Page: "填写站点时必须同时填写物理局域网与地址；只填虚拟局域网时请不填站点。",
			}
		}
	}

	deviceMemberships := make([]*models.NetworkMembership, 0)
	deviceName := req.DeviceName
	if !req.ExistingDevice && deviceName != "" {
		existing, err := m.st.GetDevice(deviceName)
		if err != nil && !errors.Is(err, store.ErrDeviceNotFound) {
			return preparedAdd{}, err
		}
		if existing != nil {
			return preparedAdd{}, &FieldError{Field: "device", CLI: "device name already exists", Page: "设备名已存在：" + deviceName + "。"}
		}
	}
	if req.ExistingDevice {
		device, err := m.st.GetDevice(deviceName)
		if err != nil || device == nil {
			return preparedAdd{}, &FieldError{
				Field: "device", CLI: "existing device not found",
				Page: "已有设备不存在，请重新选择。",
			}
		}
		deviceMemberships, err = m.st.ListNetworkMembershipsByRegistration(deviceName)
		if err != nil {
			return preparedAdd{}, err
		}
		if len(memberships) > 0 {
			return preparedAdd{}, &FieldError{Field: "device", CLI: "existing device memberships cannot be replaced", Page: "已有设备沿用全部网络成员，不能再登记新的站点与网络成员。"}
		}
	}
	if req.Server.Host == "" {
		for _, membership := range deviceMemberships {
			if membership.NetworkType == models.NetworkPhysical {
				req.Server.Host = membership.Address
				break
			}
		}
		for _, membership := range memberships {
			if network, ok := networkByName[membership.Network]; ok && network.Type == models.NetworkPhysical {
				req.Server.Host = membership.Address
				break
			}
		}
	}
	if req.Server.Host == "" && len(deviceMemberships) == 0 && len(memberships) == 0 {
		return preparedAdd{}, &FieldError{
			Field: "host", CLI: "host or at least one network membership is required",
			Page: "兜底主机与网络成员地址至少填写一项。",
		}
	}

	if len(req.ProfileIDs) > 0 {
		profiles, err := m.st.ListProfiles()
		if err != nil {
			return preparedAdd{}, err
		}
		profileIDs := make(map[string]struct{}, len(profiles))
		for _, profile := range profiles {
			profileIDs[profile.ID] = struct{}{}
		}
		for _, id := range req.ProfileIDs {
			if _, ok := profileIDs[id]; !ok {
				return preparedAdd{}, &FieldError{
					Field: "profile", CLI: fmt.Sprintf("profile %q not found", id),
					Page: "profile 不存在，请重新选择。",
				}
			}
		}
	}
	return preparedAdd{server: req.Server, deviceName: deviceName, memberships: memberships}, nil
}

func validateAddCredentials(req AddRequest) error {
	if req.LoginCredential != nil {
		switch req.LoginCredential.Type {
		case models.CredPassword:
			if len(req.LoginCredential.Secret) == 0 {
				return &FieldError{Field: "password", CLI: "--password cannot be empty", Page: "密码不能为空。"}
			}
		case models.CredPrivateKey:
			if len(req.LoginCredential.Secret) == 0 {
				return &FieldError{Field: "key", CLI: "--key cannot be empty", Page: "私钥不能为空。"}
			}
			if len(req.LoginCredential.Passphrase) > 0 {
				if err := ValidateKeyPassphrase(req.LoginCredential.Secret, req.LoginCredential.Passphrase); err != nil {
					return &FieldError{Field: "key-passphrase", CLI: err.Error(), Page: "私钥口令校验失败，未保存。"}
				}
			}
		default:
			return &FieldError{Field: "credential", CLI: "unsupported credential type", Page: "登录凭据类型无效。"}
		}
	}
	if req.SudoCredential != nil && len(req.SudoCredential.Secret) == 0 {
		return &FieldError{Field: "sudo-password", CLI: "--sudo-password cannot be empty", Page: "sudo 密码不能为空。"}
	}
	return nil
}

func addAuditSummary(req AddRequest) map[string]any {
	return map[string]any{
		"name": req.Server.Name, "host": req.Server.Host,
		"port": req.Server.Port, "user": req.Server.User,
	}
}
