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

func (w openerWriter) AddNetworkMembership(device, network, address string) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		membership, err := st.AddNetworkMembership(device, network, address)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已把设备 %s 加入网络 %s：%s",
			membership.DeviceName, membership.NetworkName, membership.Address), nil
	})
}

func (w openerWriter) EditNetworkMembership(device, network, address string) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		membership, err := st.EditNetworkMembership(device, network, address)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已修改设备 %s 在网络 %s 的地址：%s",
			membership.DeviceName, membership.NetworkName, membership.Address), nil
	})
}

func (w openerWriter) RemoveNetworkMembership(device, network string) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		membership, err := st.RemoveNetworkMembership(device, network)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已把设备 %s 移出网络 %s", membership.DeviceName, membership.NetworkName), nil
	})
}

func (w openerWriter) MoveNetworkMembership(device, fromNetwork, toNetwork, address string) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		membership, err := st.MoveNetworkMembership(device, fromNetwork, toNetwork, address)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已把设备 %s 从网络 %s 移到网络 %s：%s",
			membership.DeviceName, fromNetwork, membership.NetworkName, membership.Address), nil
	})
}

func (w openerWriter) RemovePhysicalNetworkMemberships(device string) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		removed, err := st.RemovePhysicalNetworkMemberships(device)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已删除设备 %s 的全部物理局域网成员，共 %d 条；虚拟局域网成员保留",
			device, len(removed)), nil
	})
}

func (w openerWriter) AddPhysicalNetwork(name, site string) (string, error) {
	return withStoreWriter(w.open, func(st *store.Store) (string, error) {
		network, err := st.AddNetwork(models.Network{
			Name: name, Type: models.NetworkPhysical, SiteName: site,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已登记网络 %s（物理局域网）", network.Name), nil
	})
}

// storeWriter 直接写一个已打开的 vault，供 serve 进程复用长期存储句柄。
type storeWriter struct {
	store *store.Store
}

// WriterFromStore 返回绑定指定存储的拓扑写入口。
func WriterFromStore(st *store.Store) webui.TopologyWriter {
	return storeWriter{store: st}
}

func (w storeWriter) AddNetworkMembership(device, network, address string) (string, error) {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.AddNetworkMembership(device, network, address)
}

func (w storeWriter) EditNetworkMembership(device, network, address string) (string, error) {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.EditNetworkMembership(device, network, address)
}

func (w storeWriter) RemoveNetworkMembership(device, network string) (string, error) {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.RemoveNetworkMembership(device, network)
}

func (w storeWriter) MoveNetworkMembership(device, fromNetwork, toNetwork, address string) (string, error) {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.MoveNetworkMembership(device, fromNetwork, toNetwork, address)
}

func (w storeWriter) RemovePhysicalNetworkMemberships(device string) (string, error) {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.RemovePhysicalNetworkMemberships(device)
}

func (w storeWriter) AddPhysicalNetwork(name, site string) (string, error) {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.AddPhysicalNetwork(name, site)
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

// SaveTopologyLayout 在已打开的存储上保存一个视图的完整布局。
func (w storeWriter) SaveTopologyLayout(save webui.TopologyLayoutSave, source webui.TopologyWriteSource) error {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.SaveTopologyLayout(save, source)
}

// ResetTopologyLayout 在已打开的存储上清空一个视图保存的全部坐标。
func (w storeWriter) ResetTopologyLayout(view string, source webui.TopologyWriteSource) error {
	return openerWriter{open: func() (*store.Store, func(), error) {
		return w.store, func() {}, nil
	}}.ResetTopologyLayout(view, source)
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
	switch {
	case errors.Is(err, store.ErrDeviceNotFound),
		errors.Is(err, store.ErrNetworkNotFound),
		errors.Is(err, store.ErrSiteNotFound),
		errors.Is(err, store.ErrNetworkMembershipNotFound):
		return &webui.TopologyNameNotFoundError{Message: err.Error()}
	default:
		return err
	}
}
