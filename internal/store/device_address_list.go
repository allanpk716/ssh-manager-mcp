package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"ssh-manager-mcp/internal/models"
)

// 地址列表校验的五种错误码；入口层按错误码映射字段归属与界面文案。
const (
	TopologyCodeOtherAddressWhitespace      = "other-address-whitespace"
	TopologyCodeMembershipIncomplete        = "membership-incomplete"
	TopologyCodeMembershipAddressWhitespace = "membership-address-whitespace"
	TopologyCodeDuplicateNetwork            = "duplicate-network"
	TopologyCodeAddressRequired             = "address-required"
)

// ReplaceDeviceAddressListRequest 描述一次服务器详情地址列表保存：网络成员
// 是最终清单，OtherAddress 是这台服务器条目的其他地址最终值。
type ReplaceDeviceAddressListRequest struct {
	ServerID         string
	ExpectedRevision int64
	CheckRevision    bool
	OtherAddress     string
	Memberships      []NetworkMembershipWrite
	Source           WriteSource
}

// ReplaceDeviceAddressListResult 返回保存后的服务器条目与网络成员清单。
type ReplaceDeviceAddressListResult struct {
	Server      *models.Server
	Memberships []*models.NetworkMembership
	Warning     string
}

// ReplaceDeviceAddressList 用一份最终地址清单替换设备的网络成员，并保存同
// 一台服务器条目的其他地址。清单校验、数据行与审计行都在一个事务里完成。
func (s *Store) ReplaceDeviceAddressList(
	request ReplaceDeviceAddressListRequest,
) (*ReplaceDeviceAddressListResult, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	otherAddress, memberships, err := normalizeDeviceAddressList(request.OtherAddress, request.Memberships)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	srv, err := getServerTx(tx, request.ServerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("server id %q not found", request.ServerID)
		}
		return nil, err
	}
	if request.CheckRevision && srv.Revision != request.ExpectedRevision {
		return nil, &ErrStaleRevision{Current: srv.Revision}
	}
	if srv.DeviceID == "" {
		return nil, &TopologyValidationError{Message: "这台服务器条目还没有关联机器，不能保存地址列表"}
	}
	device, err := deviceByIDTx(tx, srv.DeviceID)
	if err != nil {
		return nil, err
	}
	existing, err := networkMembershipsForDeviceTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	existingByNetworkID := make(map[string]*models.NetworkMembership, len(existing))
	for _, membership := range existing {
		existingByNetworkID[membership.NetworkID] = membership
	}
	targets := make([]*models.NetworkMembership, 0, len(memberships))
	targetByNetworkID := make(map[string]*models.NetworkMembership, len(memberships))
	for _, write := range memberships {
		network, err := networkByNameTx(tx, write.Network)
		if err != nil {
			return nil, err
		}
		if _, duplicate := targetByNetworkID[network.ID]; duplicate {
			return nil, &TopologyValidationError{Message: fmt.Sprintf(
				"同一台机器 %s 在 %s 里只能登记一个地址", device.Name, network.Name)}
		}
		membership := &models.NetworkMembership{
			DeviceID: device.ID, DeviceName: device.Name,
			NetworkID: network.ID, NetworkName: network.Name,
			NetworkType: network.Type, SiteName: network.SiteName, Address: write.Address,
		}
		targets = append(targets, membership)
		targetByNetworkID[network.ID] = membership
	}
	for _, membership := range existing {
		if _, keep := targetByNetworkID[membership.NetworkID]; keep {
			continue
		}
		if _, err := tx.Exec(
			`DELETE FROM network_memberships WHERE device_id=? AND network_id=?`,
			membership.DeviceID, membership.NetworkID,
		); err != nil {
			return nil, err
		}
		if err := writeTopologyAuditSourceTx(
			tx, s.nowTime(), "device.member-rm", membershipAuditSummary(membership), request.Source,
		); err != nil {
			return nil, err
		}
		delete(existingByNetworkID, membership.NetworkID)
	}
	for _, target := range targets {
		current := existingByNetworkID[target.NetworkID]
		if current == nil {
			write, err := s.addNetworkMembershipTx(
				tx, device.Name, target.NetworkName, target.Address, request.Source,
			)
			if err != nil {
				return nil, err
			}
			*target = *write.Membership
			continue
		}
		if current.Address == target.Address {
			*target = *current
			continue
		}
		if err := ensureMembershipAddressAvailableTx(tx, target); err != nil {
			return nil, err
		}
		target.RegistrationOrder = current.RegistrationOrder
		target.CreatedAt = current.CreatedAt
		target.UpdatedAt = s.nowTime()
		if _, err := tx.Exec(
			`UPDATE network_memberships SET address=?,address_lower=?,updated_at=?
			 WHERE device_id=? AND network_id=?`,
			target.Address, strings.ToLower(target.Address), target.UpdatedAt.Unix(),
			target.DeviceID, target.NetworkID,
		); err != nil {
			return nil, err
		}
		if err := writeTopologyAuditSourceTx(
			tx, target.UpdatedAt, "device.member-edit", membershipAuditSummary(target), request.Source,
		); err != nil {
			return nil, err
		}
	}
	if srv.Host != otherAddress {
		result, err := tx.Exec(
			`UPDATE servers SET host=?,updated_at=?,revision=revision+1 WHERE id=? AND revision=?`,
			otherAddress, now(), srv.ID, srv.Revision,
		)
		if err != nil {
			return nil, err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			currentRevision := int64(-1)
			if err := tx.QueryRow(`SELECT revision FROM servers WHERE id=?`, srv.ID).
				Scan(&currentRevision); err != nil {
				return nil, err
			}
			return nil, &ErrStaleRevision{Current: currentRevision}
		}
		audit := OwnerAuditRowAt(s.nowTime(), "server.edit", map[string]any{
			"name": srv.Name, "changed": []string{"host"},
		})
		audit.Source, audit.SourceIP = request.Source.kind, request.Source.address
		if err := writeAuditTx(tx, audit); err != nil {
			return nil, err
		}
	}
	savedServer, err := getServerTx(tx, srv.ID)
	if err != nil {
		return nil, err
	}
	savedMemberships, err := networkMembershipsForDeviceTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	warning := ""
	for _, membership := range savedMemberships {
		message, err := physicalVirtualAddressConflictWarningTx(tx, membership)
		if err != nil {
			return nil, err
		}
		if message != "" {
			warning = message
			break
		}
	}
	return &ReplaceDeviceAddressListResult{
		Server: savedServer, Memberships: savedMemberships, Warning: warning,
	}, tx.Commit()
}

func normalizeDeviceAddressList(
	otherAddress string, requested []NetworkMembershipWrite,
) (string, []NetworkMembershipWrite, error) {
	otherAddress = strings.TrimSpace(otherAddress)
	if otherAddress != "" && containsUnicodeSpace(otherAddress) {
		return "", nil, &TopologyValidationError{
			Message: "地址不能包含空白字符", Code: TopologyCodeOtherAddressWhitespace,
		}
	}
	memberships := make([]NetworkMembershipWrite, 0, len(requested))
	seenNetworks := make(map[string]struct{}, len(requested))
	for _, membership := range requested {
		membership.Network = strings.TrimSpace(membership.Network)
		membership.Address = strings.TrimSpace(membership.Address)
		if membership.Network == "" || membership.Address == "" {
			return "", nil, &TopologyValidationError{
				Message: "每行网络地址必须同时选择网络并填写地址", Code: TopologyCodeMembershipIncomplete,
			}
		}
		if containsUnicodeSpace(membership.Address) {
			return "", nil, &TopologyValidationError{
				Message: "地址不能包含空白字符", Code: TopologyCodeMembershipAddressWhitespace,
			}
		}
		if _, duplicate := seenNetworks[membership.Network]; duplicate {
			return "", nil, &TopologyValidationError{
				Message: "同一台机器在同一个网络里只能登记一个地址", Code: TopologyCodeDuplicateNetwork,
			}
		}
		seenNetworks[membership.Network] = struct{}{}
		memberships = append(memberships, membership)
	}
	if otherAddress == "" && len(memberships) == 0 {
		return "", nil, &TopologyValidationError{
			Message: "至少填写一个地址", Code: TopologyCodeAddressRequired,
		}
	}
	return otherAddress, memberships, nil
}

func containsUnicodeSpace(value string) bool {
	return strings.ContainsFunc(value, unicode.IsSpace)
}
