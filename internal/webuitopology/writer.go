package webuitopology

import (
	"errors"
	"fmt"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// openerWriter 每次写请求重新打开 vault，写完关闭，不长期持有存储句柄。
type openerWriter struct {
	open func() (*store.Store, func(), error)
}

// WriterFromOpener 返回按请求打开权威 vault 的拓扑写入口，供单机适配层
// 使用。
func WriterFromOpener(open func() (*store.Store, func(), error)) webui.TopologyWriter {
	return openerWriter{open: open}
}

func (w openerWriter) AddNetworkMembership(
	device, network, address string, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		write, err := st.AddNetworkMembershipWithSourceAndWarning(
			device, network, address, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", err
		}
		membership := write.Membership
		return fmt.Sprintf("已把设备 %s 加入网络 %s：%s",
			membership.DeviceName, membership.NetworkName, membership.Address) +
			membershipWriteWarningSuffix(write.Warning), nil
	})
}

func (w openerWriter) EditNetworkMembership(
	device, network, address string, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		write, err := st.EditNetworkMembershipWithSourceAndWarning(
			device, network, address, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", err
		}
		membership := write.Membership
		return fmt.Sprintf("已修改设备 %s 在网络 %s 的地址：%s",
			membership.DeviceName, membership.NetworkName, membership.Address) +
			membershipWriteWarningSuffix(write.Warning), nil
	})
}

func (w openerWriter) RemoveNetworkMembership(
	device, network string, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		membership, err := st.RemoveNetworkMembershipWithSource(
			device, network, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已把设备 %s 移出网络 %s", membership.DeviceName, membership.NetworkName), nil
	})
}

func (w openerWriter) MoveNetworkMembership(
	device, fromNetwork, toNetwork, address string, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		write, err := st.MoveNetworkMembershipWithSourceAndWarning(
			device, fromNetwork, toNetwork, address, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", err
		}
		membership := write.Membership
		return fmt.Sprintf("已把设备 %s 从网络 %s 移到网络 %s：%s",
			membership.DeviceName, fromNetwork, membership.NetworkName, membership.Address) +
			membershipWriteWarningSuffix(write.Warning), nil
	})
}

// membershipWriteWarningSuffix 把存储层警告追加到成功提示后，拓扑页把它
// 显示在同一个保存结果提示里。
func membershipWriteWarningSuffix(warning string) string {
	if warning == "" {
		return ""
	}
	return "\n" + warning
}

func (w openerWriter) RemovePhysicalNetworkMemberships(
	device string, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		removed, err := st.RemovePhysicalNetworkMembershipsWithSource(
			device, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已删除设备 %s 的全部物理局域网成员，共 %d 条；虚拟局域网成员保留",
			device, len(removed)), nil
	})
}

func (w openerWriter) AddPhysicalNetwork(name, site string, source webui.TopologyWriteSource) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		network, err := st.AddNetworkWithSource(models.Network{
			Name: name, Type: models.NetworkPhysical, SiteName: site,
		}, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已登记网络 %s（物理局域网）", network.Name), nil
	})
}

// mapTopologyStoreError 把存储层的地点与网络错误换成 Web UI 的分类错
// 误：不存在按 404 处理，其余输入与守卫错误按 400 处理。
func mapTopologyStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrSiteNotFound), errors.Is(err, store.ErrNetworkNotFound):
		return &webui.TopologyNameNotFoundError{Message: err.Error()}
	case errors.Is(err, store.ErrSiteNameExists), errors.Is(err, store.ErrNetworkNameTaken):
		return &webui.TopologyInvalidInputError{Message: err.Error()}
	}
	var validation *store.TopologyValidationError
	if errors.As(err, &validation) {
		return &webui.TopologyInvalidInputError{Message: validation.Message}
	}
	return err
}

// sitePathText 重新读取地点树并返回指定地点的完整路径，供写操作提示使用。
func sitePathText(st *store.Store, siteID string) (string, error) {
	sites, err := st.ListSites()
	if err != nil {
		return "", err
	}
	return models.NewSiteTree(sites).PathString(siteID), nil
}

func (w openerWriter) AddLocation(
	name, parent, note string, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		site, err := st.AddSiteWithParentAndSource(name, note, parent, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", mapTopologyStoreError(err)
		}
		path, pathErr := sitePathText(st, site.ID)
		if pathErr != nil {
			return "", pathErr
		}
		return "已新建地点 " + path, nil
	})
}

func (w openerWriter) EditLocation(
	ref string, changes webui.TopologyLocationChanges, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		site, err := st.EditSiteWithSource(ref, store.SiteChanges{
			Name: changes.Name, Note: changes.Note, Parent: changes.Parent,
		}, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", mapTopologyStoreError(err)
		}
		path, pathErr := sitePathText(st, site.ID)
		if pathErr != nil {
			return "", pathErr
		}
		return "已保存地点 " + path, nil
	})
}

func (w openerWriter) DeleteLocation(ref string, source webui.TopologyWriteSource) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		if err := st.DeleteSiteWithSource(ref, store.WebUISource(source.RemoteAddress)); err != nil {
			return "", mapTopologyStoreError(err)
		}
		return "已删除地点 " + ref, nil
	})
}

func (w openerWriter) AddManagedNetwork(
	input webui.TopologyNetworkInput, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		networkType, err := models.ParseNetworkType(input.Type)
		if err != nil {
			return "", &webui.TopologyInvalidInputError{Message: err.Error()}
		}
		network, err := st.AddNetworkWithSource(models.Network{
			Name: input.Name, Type: networkType, SiteName: input.Site,
			NetworkRange: input.NetworkRange, BandwidthMbps: input.BandwidthMbps,
			Metered: input.Metered,
		}, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", mapTopologyStoreError(err)
		}
		return fmt.Sprintf("已保存网络 %s（%s）", network.Name, networkTypeLabel(networkType)), nil
	})
}

func (w openerWriter) EditManagedNetwork(
	name string, changes webui.TopologyNetworkChanges, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		network, err := st.EditNetworkWithSource(name, store.NetworkChanges{
			Name: changes.Name, Site: changes.Site, NetworkRange: changes.NetworkRange,
			BandwidthMbps: changes.BandwidthMbps, Metered: changes.Metered,
		}, store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", mapTopologyStoreError(err)
		}
		return fmt.Sprintf("已保存网络 %s（%s）", network.Name, networkTypeLabel(network.Type)), nil
	})
}

func (w openerWriter) EditDeviceSite(
	device, siteRef string, source webui.TopologyWriteSource,
) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		site := siteRef
		edited, err := st.EditDeviceWithSource(device, store.DeviceChanges{Site: &site},
			store.WebUISource(source.RemoteAddress))
		if err != nil {
			return "", topologyWriteError(err)
		}
		if edited.SiteName == "" {
			return fmt.Sprintf("已清除设备 %s 的地点", edited.Name), nil
		}
		return fmt.Sprintf("已把设备 %s 的地点改为 %s", edited.Name, edited.SiteName), nil
	})
}

func (w openerWriter) MoveServerToDevice(
	serverIdentifier, device string, source webui.TopologyWriteSource,
) (webui.TopologyServerMoveResult, error) {
	var result webui.TopologyServerMoveResult
	_, err := withStoreWriter(w.open, func(st *store.Store) (string, error) {
		move, err := st.MoveServerToDevice(serverIdentifier, device)
		if err != nil {
			return "", topologyWriteError(err)
		}
		result = webui.TopologyServerMoveResult{
			ServerName:        move.Server.Name,
			FromDevice:        deviceNameOrEmpty(move.FromDevice),
			ToDevice:          move.ToDevice.Name,
			AnchorNotice:      anchorCopyNotice(move.AnchorCopy),
			AnchorCopyOutcome: anchorCopyOutcome(move.AnchorCopy),
		}
		return "", nil
	})
	return result, err
}

func deviceNameOrEmpty(device *models.Device) string {
	if device == nil {
		return ""
	}
	return device.Name
}

func anchorCopyNotice(anchor *store.ServerAnchorCopy) string {
	if anchor == nil {
		return ""
	}
	if anchor.AnchorConflict {
		return "同机同端口的其他账号主机密钥指纹不一致，未复制主机密钥锚。"
	}
	if anchor.Copied {
		return "已复制同机账号 " + anchor.FromServerName + " 的主机密钥锚。"
	}
	return ""
}

func anchorCopyOutcome(anchor *store.ServerAnchorCopy) string {
	if anchor == nil {
		return webui.TopologyAnchorCopyNone
	}
	if anchor.AnchorConflict {
		return webui.TopologyAnchorCopyConflict
	}
	if anchor.Copied {
		return webui.TopologyAnchorCopyCopied
	}
	return webui.TopologyAnchorCopyNone
}

func networkTypeLabel(networkType models.NetworkType) string {
	if networkType == models.NetworkVirtual {
		return "虚拟局域网"
	}
	return "物理局域网"
}

// WriterFromStore 返回绑定指定存储的拓扑写入口。
func WriterFromStore(st *store.Store) webui.TopologyWriter {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return st, func() {}, nil
	}}
}

// SaveTopologyLayout 保存一个视图的完整布局，并写一条来源为 Web UI 的审计。
func (w openerWriter) SaveTopologyLayout(save webui.TopologyLayoutSave, source webui.TopologyWriteSource) error {
	_, err := withStoreWriter(w.open, func(st *store.Store) (string, error) {
		input := store.ViewLayoutInput{
			Devices:    make([]store.DeviceLayoutInput, 0, len(save.Devices)),
			Containers: make([]store.ContainerLayoutInput, 0, len(save.Containers)),
		}
		for _, coordinate := range save.Devices {
			input.Devices = append(input.Devices, store.DeviceLayoutInput{
				DeviceID: coordinate.DeviceID, X: coordinate.X, Y: coordinate.Y,
			})
		}
		for _, coordinate := range save.Containers {
			input.Containers = append(input.Containers, store.ContainerLayoutInput{
				EntityType: coordinate.EntityType, EntityID: coordinate.EntityID,
				X: coordinate.X, Y: coordinate.Y,
			})
		}
		if err := st.SaveViewLayout(save.ViewKey, input, source.RemoteAddress); err != nil {
			return "", mapLayoutError(err)
		}
		return "", nil
	})
	return err
}

// ResetTopologyLayout 清空一个视图保存的全部坐标，并写一条来源为 Web UI
// 的审计。
func (w openerWriter) ResetTopologyLayout(view string, source webui.TopologyWriteSource) error {
	_, err := withStoreWriter(w.open, func(st *store.Store) (string, error) {
		if err := st.ResetViewLayout(view, source.RemoteAddress); err != nil {
			return "", mapLayoutError(err)
		}
		return "", nil
	})
	return err
}

// mapLayoutError 把存储层的布局校验错误换算成 Web UI 的内容非法错误；其
// 它错误原样返回，让处理器按服务器错误处理。
func mapLayoutError(err error) error {
	switch {
	case errors.Is(err, store.ErrLayoutViewInvalid),
		errors.Is(err, store.ErrLayoutEntityInvalid),
		errors.Is(err, store.ErrLayoutEntityDuplicate),
		errors.Is(err, store.ErrLayoutCoordinateInvalid),
		errors.Is(err, store.ErrLayoutEntityMissing),
		errors.Is(err, store.ErrLayoutIdentifierInvalid):
		return fmt.Errorf("%w：%v", webui.ErrTopologyLayoutInvalid, err)
	default:
		return err
	}
}

// withStoreWriter 打开存储、执行写操作并统一把存储层的名称不存在错误换
// 成 Web UI 接口的分类错误；其余错误原样返回，让接口层按 400 显示。
func withStoreWriter(open func() (*store.Store, func(), error), run func(*store.Store) (string, error)) (string, error) {
	st, closeStore, err := open()
	if err != nil {
		return "", err
	}
	defer closeStore()
	message, err := run(st)
	if err != nil {
		return "", topologyWriteError(err)
	}
	return message, nil
}

func topologyWriteError(err error) error {
	var validation *store.TopologyValidationError
	switch {
	case errors.Is(err, store.ErrDeviceNotFound),
		errors.Is(err, store.ErrNetworkNotFound),
		errors.Is(err, store.ErrSiteNotFound),
		errors.Is(err, store.ErrNetworkMembershipNotFound):
		return &webui.TopologyNameNotFoundError{Message: err.Error()}
	case errors.As(err, &validation):
		return &webui.TopologyInvalidInputError{Message: validation.Message}
	case errors.Is(err, store.ErrNetworkNameTaken), errors.Is(err, store.ErrSiteNameExists):
		return &webui.TopologyInvalidInputError{Message: err.Error()}
	default:
		return err
	}
}
