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
