package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ssh-manager-mcp/internal/clientcache"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// ClientInstance 是客户端页实例列表的一行。Name 空串表示默认实例；
// MissingMaterials 使用材料的中文名，页面直接展示。
type ClientInstance struct {
	Name             string
	DisplayName      string
	SnapshotAge      time.Duration
	HasSnapshot      bool
	Complete         bool
	MissingMaterials []string
}

// ClientStatus 是客户端页顶部的连接状态。各字段来自设备码凭据、快照元数据
// 与本地加密快照；材料缺失时由调用方展示 ReadError。
type ClientStatus struct {
	Instance               string
	AuthorityURL           string
	CertificateFingerprint string
	Profile                string
	ServerCount            int
	PulledAt               time.Time
	SnapshotRevision       int64
	RecommendedVersion     string
	ReadError              string
}

// ClientPullRequest 是运维模块交给底层拉取函数的参数。底层适配器必须把它
// 映射到与 `cache pull` 相同的实现，不允许新增明文拉取路径。
type ClientPullRequest struct {
	Instance string
	URL      string
	Token    string
	Pin      string
	Timeout  time.Duration
}

// ClientPullFunction 是可注入的底层拉取接缝；生产适配器传入 clientops.DoPull。
type ClientPullFunction func(ClientPullRequest) error

// ClientSyncErrorKind 是立即同步失败的用户可见分类。
type ClientSyncErrorKind string

const (
	ClientSyncErrorUnreachable            ClientSyncErrorKind = "unreachable"
	ClientSyncErrorDeviceCodeRevoked      ClientSyncErrorKind = "device_code_revoked"
	ClientSyncErrorCertificateFingerprint ClientSyncErrorKind = "certificate_fingerprint"
	ClientSyncErrorOther                  ClientSyncErrorKind = "other"
)

// ClientSyncError 带分类的立即同步失败；Err 保留底层错误供页面显示细节。
type ClientSyncError struct {
	Kind ClientSyncErrorKind
	Err  error
}

func (e *ClientSyncError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return "立即同步失败"
	}
	return e.Err.Error()
}

func (e *ClientSyncError) Unwrap() error { return e.Err }

// ClientInstanceIncompleteError 表示目标实例材料不齐全，不能切换。
type ClientInstanceIncompleteError struct {
	Instance         string
	MissingMaterials []string
}

func (e *ClientInstanceIncompleteError) Error() string {
	return fmt.Sprintf("实例 %s 缺少%s，不能切换", ClientInstanceDisplayName(e.Instance), strings.Join(e.MissingMaterials, "、"))
}

// ListClientInstances 列出本机可选择的客户端实例，并按四项材料判断能否切换：
// 设备码凭据、快照、快照元数据、数据加密密钥。单槽环境变量覆盖时只显示默认
// 实例，因为此时实例名无法路由到独立目录。
func ListClientInstances(now time.Time) ([]ClientInstance, error) {
	names := []string{""}
	if !clientcache.SingleSlotOverrideSet() {
		named, err := clientcache.ListInstanceSlots()
		if err != nil {
			return nil, fmt.Errorf("列出命名客户端实例失败：%w", err)
		}
		names = append(names, named...)
	}

	out := make([]ClientInstance, 0, len(names))
	for _, name := range names {
		instance, err := readClientInstance(name, now)
		if err != nil {
			return nil, err
		}
		out = append(out, instance)
	}
	return out, nil
}

// SwitchClientInstance 校验目标实例材料齐全后，把实例切换动作写入目标实例
// 的本机审计日志文件。这里只写审计；浏览器会话中的当前实例由 Web 处理器保存。
func SwitchClientInstance(from, to string, source Source) error {
	instances, err := ListClientInstances(time.Now())
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if instance.Name != to {
			continue
		}
		if !instance.Complete {
			return &ClientInstanceIncompleteError{Instance: to, MissingMaterials: instance.MissingMaterials}
		}
		row := ownerAuditRow("instance.switch", map[string]any{
			"from_instance": from, "to_instance": to,
		}, false, source)
		return writeClientPageAudit(to, row)
	}
	return fmt.Errorf("实例 %s 不存在", to)
}

// SyncClient 立即拉取一个客户端实例的快照，成功后重新读取本地状态并写审计。
func SyncClient(instance string, source Source, pull ClientPullFunction) (ClientStatus, error) {
	credential, err := clientcache.ReadCredentialFor(instance)
	if err != nil {
		return ClientStatus{}, err
	}
	if credential == nil {
		err := errors.New("设备码凭据不存在")
		_ = writeClientSyncAudit(instance, source, ClientStatus{}, "error", err.Error())
		return ClientStatus{}, err
	}
	if credential.Pin == "" {
		err := errors.New("设备码凭据缺少证书公钥指纹，拒绝明文拉取")
		_ = writeClientSyncAudit(instance, source, ClientStatus{}, "error", err.Error())
		return ClientStatus{}, err
	}
	parsed, err := url.Parse(credential.URL)
	if err != nil || parsed.Scheme != "https" {
		err = fmt.Errorf("权威端地址必须是 HTTPS：%s", credential.URL)
		_ = writeClientSyncAudit(instance, source, ClientStatus{}, "error", err.Error())
		return ClientStatus{}, err
	}

	request := ClientPullRequest{
		Instance: instance, URL: credential.URL, Token: credential.Token,
		Pin: credential.Pin, Timeout: 10 * time.Second,
	}
	if err := pull(request); err != nil {
		syncErr := classifyClientSyncError(err)
		_ = writeClientSyncAudit(instance, source, ClientStatus{}, "error", syncErr.Error())
		return ClientStatus{}, syncErr
	}
	status, statusErr := ReadClientStatus(instance, time.Now())
	if statusErr != nil {
		_ = writeClientSyncAudit(instance, source, ClientStatus{}, "error", statusErr.Error())
		return status, statusErr
	}
	if auditErr := writeClientSyncAudit(instance, source, status, "ok", ""); auditErr != nil {
		return status, auditErr
	}
	return status, nil
}

// readClientInstance 只用文件存在性与修改时间判断一个实例，不读取密钥内容。
func readClientInstance(name string, now time.Time) (ClientInstance, error) {
	instance := ClientInstance{Name: name, DisplayName: ClientInstanceDisplayName(name)}
	_, snapshotPath, metadataPath, _, err := clientcache.PathsFor(name)
	if err != nil {
		return ClientInstance{}, err
	}
	credentialPath, err := clientcache.CredentialPathFor(name)
	if err != nil {
		return ClientInstance{}, err
	}
	keyPath, err := paths.CacheDekPathFor(name)
	if err != nil {
		return ClientInstance{}, err
	}

	missing := make([]string, 0, 4)
	appendMissing := func(path, label string) {
		if _, statErr := os.Stat(path); statErr != nil {
			missing = append(missing, label)
		}
	}
	appendMissing(credentialPath, "设备码凭据")
	appendMissing(snapshotPath, "快照")
	appendMissing(metadataPath, "快照元数据")
	appendMissing(keyPath, "数据加密密钥")

	if info, statErr := os.Stat(snapshotPath); statErr == nil {
		instance.HasSnapshot = true
		instance.SnapshotAge = now.Sub(info.ModTime())
	}
	instance.MissingMaterials = missing
	instance.Complete = len(missing) == 0
	return instance, nil
}

// writeClientPageAudit 把一条 JSON 行追加到实例自己的本机审计日志文件。
func writeClientPageAudit(instance string, row store.AuditRow) error {
	_, _, _, auditPath, err := clientcache.PathsFor(instance)
	if err != nil {
		return err
	}
	data, err := json.Marshal(row.JSONMap())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		return fmt.Errorf("创建实例目录失败：%w", err)
	}
	file, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开实例审计日志失败：%w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("写入实例审计日志失败：%w", err)
	}
	return nil
}

// writeClientSyncAudit 写立即同步的审计行；摘要只含结果、快照修订号与服务
// 器数量，不包含设备码。
func writeClientSyncAudit(instance string, source Source, status ClientStatus, result, detail string) error {
	summary := map[string]any{
		"result": result, "snapshot_revision": status.SnapshotRevision,
		"server_count": status.ServerCount,
	}
	if detail != "" {
		summary["detail"] = detail
	}
	row := ownerAuditRow("cache.pull", summary, result != "ok", source)
	return writeClientPageAudit(instance, row)
}

// classifyClientSyncError 按现有拉取错误文本与网络错误接口分类。
func classifyClientSyncError(err error) error {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "quarantined"):
		return &ClientSyncError{Kind: ClientSyncErrorDeviceCodeRevoked, Err: err}
	case strings.Contains(message, "fingerprint mismatch"), strings.Contains(message, "server presented no certificate"):
		return &ClientSyncError{Kind: ClientSyncErrorCertificateFingerprint, Err: err}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return &ClientSyncError{Kind: ClientSyncErrorUnreachable, Err: err}
	}
	if strings.Contains(message, "connection refused") || strings.Contains(message, "no route to host") ||
		strings.Contains(message, "timeout") {
		return &ClientSyncError{Kind: ClientSyncErrorUnreachable, Err: err}
	}
	return &ClientSyncError{Kind: ClientSyncErrorOther, Err: err}
}

// ReadClientStatus 读取一个客户端实例的连接状态与本地快照摘要。
func ReadClientStatus(instance string, now time.Time) (ClientStatus, error) {
	status := ClientStatus{Instance: instance}
	credential, err := clientcache.ReadCredentialFor(instance)
	if err != nil {
		return status, err
	}
	if credential == nil {
		return status, errors.New("设备码凭据不存在")
	}
	status.AuthorityURL = credential.URL
	status.CertificateFingerprint = credential.Pin

	_, _, metadataPath, _, err := clientcache.PathsFor(instance)
	if err != nil {
		return status, err
	}
	metadata, err := readClientPageMetadata(metadataPath)
	if err != nil {
		return status, err
	}
	status.PulledAt = time.Unix(metadata.PulledAt, 0)

	snapshot, err := decryptClientSnapshot(instance)
	if err != nil {
		return status, err
	}
	status.ServerCount = len(snapshot.Servers)
	status.SnapshotRevision = snapshot.SnapshotRevision
	status.RecommendedVersion = snapshot.RecommendedVersion
	if metadata.Scoped && len(snapshot.Profiles) == 1 {
		status.Profile = snapshot.Profiles[0].Name
	}
	return status, nil
}

// clientPageMetadata 是客户端页需要的快照元数据子集。
type clientPageMetadata struct {
	PulledAt       int64 `json:"pulled_at"`
	ServerAnchored bool  `json:"server_anchored"`
	Scoped         bool  `json:"scoped"`
}

func readClientPageMetadata(path string) (clientPageMetadata, error) {
	var metadata clientPageMetadata
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return metadata, errors.New("快照元数据不存在")
	}
	if err != nil {
		return metadata, fmt.Errorf("读取快照元数据失败：%w", err)
	}
	if err := json.Unmarshal(blob, &metadata); err != nil {
		return metadata, fmt.Errorf("解析快照元数据失败：%w", err)
	}
	return metadata, nil
}

// decryptClientSnapshot 用实例的数据加密密钥解开本地快照。这里只做页面展示，
// 不套离线上限检查；命令行与桥继续使用各自加载入口。
func decryptClientSnapshot(instance string) (*store.Snapshot, error) {
	_, snapshotPath, _, _, err := clientcache.PathsFor(instance)
	if err != nil {
		return nil, err
	}
	keyPath, err := paths.CacheDekPathFor(instance)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取数据加密密钥失败：%w", err)
	}
	encrypted, err := os.ReadFile(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("读取快照失败：%w", err)
	}
	plain, err := vaultio.DecryptWithKey(key, encrypted)
	if err != nil {
		return nil, fmt.Errorf("解开快照失败：%w", err)
	}
	var snapshot store.Snapshot
	if err := json.Unmarshal(plain, &snapshot); err != nil {
		return nil, fmt.Errorf("解析快照失败：%w", err)
	}
	return &snapshot, nil
}

// ClientInstanceDisplayName 返回页面上的实例名；默认实例不用空字符串展示。
func ClientInstanceDisplayName(name string) string {
	if name == "" {
		return "默认实例"
	}
	return name
}
