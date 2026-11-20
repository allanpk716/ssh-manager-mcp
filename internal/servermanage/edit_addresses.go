package servermanage

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// EditAddressesRequest 是一次服务器详情地址列表保存。OtherAddress 为空表
// 示这台服务器条目没有其他地址；Memberships 是设备的最终网络成员清单。
type EditAddressesRequest struct {
	Identifier       string
	ExpectedRevision int64
	CheckRevision    bool
	OtherAddress     string
	Memberships      []NetworkMembershipInput
}

// EditAddressesResult 是地址列表保存后的完整结果。
type EditAddressesResult struct {
	Server      *models.Server
	Memberships []*models.NetworkMembership
	Warning     string
}

// EditAddresses 保存整份地址列表。校验错误使用页面可显示文案；修订号冲突
// 使用服务器编辑的现有冲突类型。审计与数据写入由存储层同事务完成。
func (m *Manager) EditAddresses(req EditAddressesRequest, src Source) (*EditAddressesResult, error) {
	if err := validateEditAddresses(req); err != nil {
		return nil, err
	}
	srv, err := m.st.ResolveServer(req.Identifier)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, fmt.Errorf("server %q not found", req.Identifier)
	}
	memberships := make([]store.NetworkMembershipWrite, 0, len(req.Memberships))
	for _, membership := range req.Memberships {
		memberships = append(memberships, store.NetworkMembershipWrite{
			Network: membership.Network, Address: membership.Address,
		})
	}
	result, err := m.st.ReplaceDeviceAddressList(store.ReplaceDeviceAddressListRequest{
		ServerID: srv.ID, ExpectedRevision: req.ExpectedRevision, CheckRevision: req.CheckRevision,
		OtherAddress: req.OtherAddress, Memberships: memberships, Source: storeWriteSource(src),
	})
	if err != nil {
		var stale *store.ErrStaleRevision
		if errors.As(err, &stale) {
			current, currentErr := m.st.GetServer(srv.ID)
			if currentErr != nil {
				return nil, currentErr
			}
			return nil, &RevisionConflict{Current: current}
		}
		return nil, err
	}
	return &EditAddressesResult{
		Server: result.Server, Memberships: result.Memberships, Warning: result.Warning,
	}, nil
}

func validateEditAddresses(req EditAddressesRequest) error {
	otherAddress := strings.TrimSpace(req.OtherAddress)
	if otherAddress != "" && containsAddressSpace(otherAddress) {
		return &FieldError{
			Field: "host", CLI: "host contains whitespace",
			Page: "地址不能包含空白字符。",
		}
	}
	seen := make(map[string]struct{}, len(req.Memberships))
	for _, membership := range req.Memberships {
		network := strings.TrimSpace(membership.Network)
		address := strings.TrimSpace(membership.Address)
		if network == "" || address == "" {
			return &FieldError{
				Field: "memberships", CLI: "network membership requires network and address",
				Page: "每行网络地址必须同时选择网络并填写地址。",
			}
		}
		if containsAddressSpace(address) {
			return &FieldError{
				Field: "memberships", CLI: "network membership address contains whitespace",
				Page: "地址不能包含空白字符。",
			}
		}
		if _, duplicate := seen[network]; duplicate {
			return &FieldError{
				Field: "memberships", CLI: "duplicate network in request",
				Page: "同一设备在同一网络里只能有一个网络成员。",
			}
		}
		seen[network] = struct{}{}
	}
	if otherAddress == "" && len(req.Memberships) == 0 {
		return &FieldError{
			Field: "host", CLI: "at least one address is required",
			Page: "至少填写一个地址。",
		}
	}
	return nil
}

func containsAddressSpace(value string) bool {
	return strings.ContainsFunc(value, unicode.IsSpace)
}
