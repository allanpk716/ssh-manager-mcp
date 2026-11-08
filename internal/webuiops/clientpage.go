package webuiops

import (
	"errors"
	"time"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/webui"
)

// errClientOpsUnavailable 表示当前角色不是客户端，没有客户端页的数据来源。
var errClientOpsUnavailable = errors.New("当前角色没有客户端页入口")

// errClientPullNotWired 表示构造适配器时没有注入底层拉取函数。
var errClientPullNotWired = errors.New("客户端拉取函数未接线")

func clientInstances() ([]webui.OpsClientInstance, error) {
	rows, err := ops.ListClientInstances(time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]webui.OpsClientInstance, 0, len(rows))
	for _, row := range rows {
		out = append(out, webui.OpsClientInstance{
			Name: row.Name, DisplayName: row.DisplayName, SnapshotAge: row.SnapshotAge,
			HasSnapshot: row.HasSnapshot, Complete: row.Complete,
			MissingMaterials: row.MissingMaterials,
		})
	}
	return out, nil
}

func clientStatus(instance string) (webui.OpsClientStatus, error) {
	status, err := ops.ReadClientStatus(instance, time.Now())
	if err != nil {
		return webui.OpsClientStatus{}, err
	}
	return webui.OpsClientStatus{
		Instance: status.Instance, InstanceDisplay: ops.ClientInstanceDisplayName(status.Instance),
		AuthorityURL: status.AuthorityURL, CertificateFingerprint: status.CertificateFingerprint,
		Profile: status.Profile, ServerCount: status.ServerCount, PulledAt: status.PulledAt,
		SnapshotRevision: status.SnapshotRevision, RecommendedVersion: status.RecommendedVersion,
		ReadError: status.ReadError,
	}, nil
}

func switchClientInstance(from, instance, sourceIP string) error {
	return ops.SwitchClientInstance(from, instance, ops.WebUISource(sourceIP))
}

func syncClient(instance, sourceIP string, pull ops.ClientPullFunction) (webui.OpsClientStatus, error) {
	if pull == nil {
		return webui.OpsClientStatus{}, errClientPullNotWired
	}
	status, err := ops.SyncClient(instance, ops.WebUISource(sourceIP), pull)
	if err != nil {
		var syncErr *ops.ClientSyncError
		if errors.As(err, &syncErr) {
			return webui.OpsClientStatus{}, &webui.OpsClientSyncError{
				Kind: string(syncErr.Kind), Message: syncErr.Error(),
			}
		}
		return webui.OpsClientStatus{}, err
	}
	return webui.OpsClientStatus{
		Instance: status.Instance, InstanceDisplay: ops.ClientInstanceDisplayName(status.Instance),
		AuthorityURL: status.AuthorityURL, CertificateFingerprint: status.CertificateFingerprint,
		Profile: status.Profile, ServerCount: status.ServerCount, PulledAt: status.PulledAt,
		SnapshotRevision: status.SnapshotRevision, RecommendedVersion: status.RecommendedVersion,
	}, nil
}

func (clientAuditStore) ListClientInstances() ([]webui.OpsClientInstance, error) {
	return clientInstances()
}

func (clientAuditStore) ClientStatus(instance string) (webui.OpsClientStatus, error) {
	return clientStatus(instance)
}

func (clientAuditStore) SwitchClientInstance(from, instance, sourceIP string) error {
	return switchClientInstance(from, instance, sourceIP)
}

func (s clientAuditStore) SyncClient(instance, sourceIP string) (webui.OpsClientStatus, error) {
	return syncClient(instance, sourceIP, s.pull)
}

func (managerStore) ListClientInstances() ([]webui.OpsClientInstance, error) {
	return nil, errClientOpsUnavailable
}

func (managerStore) ClientStatus(string) (webui.OpsClientStatus, error) {
	return webui.OpsClientStatus{}, errClientOpsUnavailable
}

func (managerStore) SwitchClientInstance(string, string, string) error {
	return errClientOpsUnavailable
}

func (managerStore) SyncClient(string, string) (webui.OpsClientStatus, error) {
	return webui.OpsClientStatus{}, errClientOpsUnavailable
}

func (openerStore) ListClientInstances() ([]webui.OpsClientInstance, error) {
	return nil, errClientOpsUnavailable
}

func (openerStore) ClientStatus(string) (webui.OpsClientStatus, error) {
	return webui.OpsClientStatus{}, errClientOpsUnavailable
}

func (openerStore) SwitchClientInstance(string, string, string) error {
	return errClientOpsUnavailable
}

func (openerStore) SyncClient(string, string) (webui.OpsClientStatus, error) {
	return webui.OpsClientStatus{}, errClientOpsUnavailable
}
