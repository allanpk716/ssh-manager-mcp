package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/models"
)

// Snapshot is a portable, master-key-independent capture of the entire vault.
// Credential Secret/Passphrase hold DECRYPTED plaintext; the serialized form
// MUST be encrypted (vaultio.Encrypt) before touching disk. Version = format version.
type Snapshot struct {
	Version int `json:"version"`
	// SnapshotRevision 是权威端全库统一的快照修订号,两条导出
	// 路径都携带。旧快照没有该字段时解码为零值 0;客户端把 0 视为
	// 「修订号未知」,第一次比较必然触发一次全量拉取。
	SnapshotRevision int64 `json:"snapshot_revision"`
	// RecommendedVersion 是导出这份快照的二进制版本。多机客户端用它比较
	// 自己是否落后；旧快照与旧客户端分别解码、忽略该字段。
	RecommendedVersion string               `json:"recommended_version"`
	Credentials        []SnapshotCredential `json:"credentials"`
	// Sites / Networks 是网络拓扑里的站点与网络。全量快照全部携带；
	// profile 裁剪快照也全部携带，让授权服务器引用的名字不因授权裁剪丢失。
	Sites    []SnapshotSite    `json:"sites,omitempty"`
	Networks []SnapshotNetwork `json:"networks,omitempty"`
	// Devices / NetworkMemberships 是网络拓扑里的设备与设备在各网络里的
	// 地址。全量快照全部携带；profile 裁剪快照按设备授权范围裁剪。
	Devices                 []SnapshotDevice                 `json:"devices,omitempty"`
	NetworkMemberships      []SnapshotNetworkMembership      `json:"network_memberships,omitempty"`
	DeviceLayoutCoordinates []SnapshotDeviceLayoutCoordinate `json:"device_layout_coordinates,omitempty"`
	Servers                 []SnapshotServer                 `json:"servers"`
	Profiles                []SnapshotProfile                `json:"profiles"`
	Grants                  []SnapshotGrant                  `json:"grants"`
	Projects                []SnapshotProject                `json:"projects"`
	HostKeys                []SnapshotHostKey                `json:"host_keys"`
	// EntryHostKeys（票 #58）是按服务器条目存放的锚；HostKeys 仍为兜底地址
	// 推导出的旧格式行，旧客户端靠它继续校验。旧快照解码时本字段为空。
	EntryHostKeys []SnapshotEntryHostKey `json:"entry_host_keys,omitempty"`
	// Secrets / SecretAllows（票 #59）在全量导出与声明了客户端版本的
	// profile 裁剪快照中携带：备份与导出文件本身整体加密，快照段里按
	// 明文字段携带值，靠外层加密保护；导入时用目标库的主密钥重新加密。
	Secrets      []SnapshotSecret      `json:"secrets,omitempty"`
	SecretAllows []SnapshotSecretAllow `json:"secret_allows,omitempty"`
	// SharedStorages 是共享存储登记。可挂载范围不在这里重复携带；它由
	// 机密项的可写入服务器关联推导。旧快照没有该段时导入零行。
	SharedStorages []SnapshotSharedStorage `json:"shared_storages,omitempty"`
	// SoftwareDeclarations / SoftwareDeclarationPorts 只进入 owner 的全量
	// 导出与加密备份，绝不进入 profile 裁剪的客户端快照；实测条目、实测
	// 监听端口、采集状态、sudo 开关与变化历史同样只走 owner 全量导出。
	SoftwareDeclarations        []SnapshotSoftwareDeclaration        `json:"software_inventory,omitempty"`
	SoftwareDeclarationPorts    []SnapshotSoftwareDeclarationPort    `json:"software_declaration_ports,omitempty"`
	SoftwareObservedItems       []SnapshotSoftwareObservedItem       `json:"software_observed_items,omitempty"`
	SoftwareObservedPorts       []SnapshotSoftwareObservedPort       `json:"software_observed_ports,omitempty"`
	SoftwareCollectionStatuses  []SnapshotSoftwareCollectionStatus   `json:"software_collection_statuses,omitempty"`
	SoftwareCollectSudoSettings []SnapshotSoftwareCollectSudoSetting `json:"software_collect_sudo_settings,omitempty"`
	SoftwareChangeHistory       []SnapshotSoftwareChangeHistory      `json:"software_change_history,omitempty"`
	Audit                       []SnapshotAudit                      `json:"audit"`
	// 命令策略段 (ticket #15):自定义规则与被停用
	// 的默认规则编号。规则全局一套,两条导出路径都携带,不做 profile 裁剪。
	// 旧快照(策略段尚不存在)解码为 nil 切片 → 导入零行 → 内置默认规则
	// 照常生效,由判定内核兑底。
	PolicyRules            []SnapshotPolicyRule `json:"policy_rules"`
	PolicyDisabledDefaults []string             `json:"policy_disabled_defaults"`
}

// SnapshotPolicyRule 是快照里的自定义命令策略规则:结构化字段与可选
// 正则,形态即 policy.CustomRule 的传输形态。处置用 policy.Disposition
// 的存储代号(allow/confirm/block)。
type SnapshotPolicyRule struct {
	ID           string   `json:"id"`
	Shells       []string `json:"shells"`        // shell 类型代号 (posix/powershell/cmd);空=全部
	Commands     []string `json:"commands"`      // 命令名及别名
	Args         []string `json:"args"`          // 参数条件(逐字)
	PathCategory string   `json:"path_category"` // ""=不限;critical;disk
	Regex        string   `json:"regex"`         // 可选正则;空=不用
	Disposition  string   `json:"disposition"`   // allow/confirm/block
	Explanation  string   `json:"explanation"`
	Alternative  string   `json:"alternative"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

type SnapshotCredential struct {
	ID         string `json:"id"`
	Type       string `json:"type"`       // models.CredentialType string value
	Secret     []byte `json:"secret"`     // DECRYPTED plaintext
	Passphrase []byte `json:"passphrase"` // DECRYPTED plaintext; nil/empty if none
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// SnapshotSoftwareDeclaration 是全量备份里的软件声明行。字段按数据库原样
// 恢复，版本不做任何规格化。
type SnapshotSoftwareDeclaration struct {
	ID                 string `json:"id"`
	DeviceID           string `json:"device_id"`
	Category           string `json:"category"`
	Name               string `json:"name"`
	Version            string `json:"version"`
	PackageURL         string `json:"purl,omitempty"`
	CPE                string `json:"cpe,omitempty"`
	BelongsTo          string `json:"belongs_to,omitempty"`
	Note               string `json:"note,omitempty"`
	ClaimedObservedKey string `json:"claimed_observed_key,omitempty"`
	Status             string `json:"status"`
	ConfirmedAt        int64  `json:"confirmed_at"`
	CreatedSource      string `json:"created_source,omitempty"`
	LastSource         string `json:"last_source,omitempty"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

// SnapshotSoftwareDeclarationPort 是全量备份里的声明监听端口行。
type SnapshotSoftwareDeclarationPort struct {
	ID            string `json:"id"`
	DeclarationID string `json:"declaration_id"`
	DeviceID      string `json:"device_id"`
	Address       string `json:"address"`
	Port          int    `json:"port"`
	Protocol      string `json:"protocol"`
	Exposure      string `json:"exposure"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// SnapshotSite 是全量快照里的站点段。
type SnapshotSite struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// SnapshotNetwork 是网络拓扑这次新增的全量快照网络段。CIDR 是快照 JSON
// 字段名，语义是「网段，只给人看」。
type SnapshotNetwork struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	SiteID        string `json:"site_id"`
	Note          string `json:"note"`
	CIDR          string `json:"cidr"`
	BandwidthMbps int64  `json:"bandwidth_mbps"`
	Metered       bool   `json:"metered"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// SnapshotDevice 是全量快照里的设备段。客户端实例凭据不随快照携带，
// 标识符与名字成对保存，导入后关联信息不丢。
type SnapshotDevice struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Note          string `json:"note"`
	ClientTokenID string `json:"client_token_id"`
	ClientName    string `json:"client_name"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// SnapshotNetworkMembership 是全量快照里的网络成员段。
type SnapshotNetworkMembership struct {
	DeviceID          string `json:"device_id"`
	NetworkID         string `json:"network_id"`
	Address           string `json:"address"`
	RegistrationOrder int64  `json:"registration_order"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type SnapshotServer struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	User             string `json:"user"`
	AuthMethod       string `json:"auth_method"`
	CredentialID     string `json:"credential_id"`
	SudoCredentialID string `json:"sudo_credential_id"` // "" if none (NULL coalesced)
	TagsRaw          string `json:"tags"`               // raw DB TEXT (JSON array string) — preserved verbatim
	Description      string `json:"description"`
	Location         string `json:"location"`
	Hardware         string `json:"hardware"`
	Services         string `json:"services"`
	Role             string `json:"role"`
	Caveats          string `json:"caveats"`
	ExposeHost       bool   `json:"expose_host"`
	// DeviceID 是服务器条目的所属设备引用，随备份、导出与 profile 裁剪
	// 快照一起携带。
	DeviceID string `json:"device_id,omitempty"`
	// ShellType (命令策略 ticket #14) 是连接信息的一员,随两条导出路径
	// 下发。旧快照(无字段)解码为零值,ImportSnapshot 归一为 posix——
	// 与 Revision 的「缺字段无损导入」同一语义。
	ShellType string `json:"shell_type"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// Revision (Plan 51) rides both export paths and MUST survive
	// ImportSnapshot verbatim — the cache store's CAS tokens have to agree
	// with the broker's or every pull would reset them to permanent 409s.
	// Old snapshots (no field) import as 0 ("never written") losslessly.
	Revision int64 `json:"revision"`
}

type SnapshotProfile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type SnapshotGrant struct {
	ProfileID string `json:"profile_id"`
	ServerID  string `json:"server_id"`
}

type SnapshotProject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TokenHash   []byte `json:"token_hash"` // verbatim — preserves original-token validity
	TokenSalt   []byte `json:"token_salt"`
	TokenPrefix string `json:"token_prefix"`
	ProfileID   string `json:"profile_id"`
	Status      string `json:"status"` // models.ProjectStatus string value
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type SnapshotHostKey struct {
	HostPort  string `json:"host_port"` // "{host}:{port}"
	KeyBlob   []byte `json:"key_blob"`
	CreatedAt int64  `json:"created_at"`
	// Plan 48 pin metadata, all omitempty: a v0.14-shaped snapshot carries no
	// pin keys, decodes to "", and ImportSnapshot normalizes "" to blob/tofu
	// (the column DEFAULTs govern DB rows and back-fills, not explicit inserts).
	PinFormat string `json:"pin_format,omitempty"`
	PinSource string `json:"pin_source,omitempty"`
	PinDevice string `json:"pin_device,omitempty"`
}

// SnapshotEntryHostKey 是快照里按服务器条目存放的主机密钥锚：每个条目至多
// 一行，携带条目标识符与学习锚时使用的握手地址。
type SnapshotEntryHostKey struct {
	ServerID      string `json:"server_id"`
	KeyBlob       []byte `json:"key_blob"`
	CreatedAt     int64  `json:"created_at"`
	PinFormat     string `json:"pin_format,omitempty"`
	PinSource     string `json:"pin_source,omitempty"`
	PinDevice     string `json:"pin_device,omitempty"`
	HandshakeAddr string `json:"handshake_addr,omitempty"`
}

type SnapshotAudit struct {
	ID         int64  `json:"id"`
	TS         int64  `json:"ts"`
	ProjectID  string `json:"project_id"`
	ServerID   string `json:"server_id"`
	Action     string `json:"action"`
	Command    string `json:"command"`
	Sudo       bool   `json:"sudo"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Source     string `json:"source,omitempty"`
	SourceIP   string `json:"source_ip,omitempty"`
	// InjectedEnvironmentNames 是命令执行审计行附带的环境变量名，不含值。
	InjectedEnvironmentNames []string `json:"injected_environment_names,omitempty"`
}

// SnapshotSecret 是快照里的机密项行。Value 与 PreviousValue 是解密后的原始
// 字节；序列化后的快照必须整体加密后才能落盘。
type SnapshotSecret struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Note            string `json:"note"`
	EnvironmentName string `json:"environment_name,omitempty"`
	Value           []byte `json:"value"`
	PreviousValue   []byte `json:"previous_value"`
	ValueVersion    int64  `json:"value_version"`
	CreatedAt       int64  `json:"created_at"`
	ValueUpdatedAt  int64  `json:"value_updated_at"`
}

// SnapshotSecretAllow 是机密项与服务器条目的可写入关联，按双方的内部标识符
// 记录，导入后由目标库重新解析。
type SnapshotSecretAllow struct {
	SecretID string `json:"secret_id"`
	ServerID string `json:"server_id"`
}

// SnapshotSharedStorage 是快照里的一处共享存储登记。提供方设备名字、
// 机密项名字、候选访问地址与可挂载服务器都按标识符或关联表推导；例外
// 是按 profile 裁剪时设备行可能不下发，因此这里携带提供方设备名供客户端
// 查看使用。
type SnapshotSharedStorage struct {
	ID               string                          `json:"id"`
	Name             string                          `json:"name"`
	DeviceID         string                          `json:"device_id"`
	DeviceName       string                          `json:"device_name,omitempty"`
	MountUser        string                          `json:"mount_user"`
	PasswordSecretID string                          `json:"password_secret_id"`
	Drive            string                          `json:"drive"`
	Directories      []models.SharedStorageDirectory `json:"directories"`
	Note             string                          `json:"note"`
	Version          int64                           `json:"version"`
	ChangeNote       string                          `json:"change_note"`
	CreatedAt        int64                           `json:"created_at"`
	UpdatedAt        int64                           `json:"updated_at"`
}

// normalizeSnapshotPinColumns 把旧快照缺失的锚格式与来源归一为默认值。
func normalizeSnapshotPinColumns(format, source string) (string, string) {
	if format == "" {
		format = PinFormatBlob
	}
	if source == "" {
		source = PinSourceTofu
	}
	return format, source
}

// snapshotEntryPins 读取全部按条目存放的锚并转成快照行（按条目标识符排序）。
func (s *Store) snapshotEntryPins() ([]SnapshotEntryHostKey, error) {
	pins, err := s.ListEntryPins()
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotEntryHostKey, 0, len(pins))
	for _, p := range pins {
		out = append(out, SnapshotEntryHostKey{
			ServerID:      p.ServerID,
			KeyBlob:       p.Blob,
			CreatedAt:     p.CreatedAt,
			PinFormat:     p.Format,
			PinSource:     p.Source,
			PinDevice:     p.Device,
			HandshakeAddr: p.HandshakeAddr,
		})
	}
	return out, nil
}

// deriveLegacyHostKeys 为旧客户端推导旧格式锚行：同一兜底「Host:端口」上
// 所有已锚定条目的密钥都相同时生成一行（行内容取条目标识符最小的锚），
// 密钥不同时不生成这一行，避免旧客户端导入时主键冲突。
func deriveLegacyHostKeys(servers []SnapshotServer, entryPins []SnapshotEntryHostKey) []SnapshotHostKey {
	byServer := make(map[string]SnapshotEntryHostKey, len(entryPins))
	for _, p := range entryPins {
		byServer[p.ServerID] = p
	}
	groups := make(map[string][]SnapshotEntryHostKey)
	for _, sv := range servers {
		p, ok := byServer[sv.ID]
		if !ok {
			continue
		}
		hp := fallbackAddr(sv.Host, sv.Port)
		groups[hp] = append(groups[hp], p)
	}
	hosts := make([]string, 0, len(groups))
	for hp := range groups {
		hosts = append(hosts, hp)
	}
	sort.Strings(hosts)
	out := make([]SnapshotHostKey, 0, len(hosts))
	for _, hp := range hosts {
		group := groups[hp]
		sort.Slice(group, func(i, j int) bool { return group[i].ServerID < group[j].ServerID })
		first := group[0]
		identity := pinKeyIdentity(first.KeyBlob, first.PinFormat, fmt.Sprintf("raw:%x", first.KeyBlob))
		same := true
		for _, p := range group[1:] {
			if pinKeyIdentity(p.KeyBlob, p.PinFormat, fmt.Sprintf("raw:%x", p.KeyBlob)) != identity {
				same = false
				break
			}
		}
		if !same {
			continue
		}
		out = append(out, SnapshotHostKey{
			HostPort:  hp,
			KeyBlob:   first.KeyBlob,
			CreatedAt: first.CreatedAt,
			PinFormat: first.PinFormat,
			PinSource: first.PinSource,
			PinDevice: first.PinDevice,
		})
	}
	return out
}

// ExportSnapshot captures every table. Credentials are DECRYPTED under s.masterKey.
// Requires the store to be unlocked. The caller must encrypt the serialized form.
func (s *Store) ExportSnapshot() (*Snapshot, error) {
	revision, err := s.SnapshotRevision()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Version: 1, SnapshotRevision: revision, RecommendedVersion: buildinfo.Version}

	// servers (COALESCE the nullable text cols to '' — credential_id is nullable
	// since Plan 20 C0: a credential-less server carries "" in the snapshot)
	rs, err := s.db.Query(`SELECT id,name,host,port,user,auth_method,COALESCE(credential_id,''),COALESCE(sudo_credential_id,''),shell_type,COALESCE(tags,''),description,location,hardware,services,role,caveats,expose_host,COALESCE(device_id,''),created_at,updated_at,revision FROM servers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var sv SnapshotServer
		if err := rs.Scan(&sv.ID, &sv.Name, &sv.Host, &sv.Port, &sv.User, &sv.AuthMethod,
			&sv.CredentialID, &sv.SudoCredentialID, &sv.ShellType, &sv.TagsRaw, &sv.Description, &sv.Location,
			&sv.Hardware, &sv.Services, &sv.Role, &sv.Caveats, &sv.ExposeHost, &sv.DeviceID, &sv.CreatedAt, &sv.UpdatedAt, &sv.Revision); err != nil {
			rs.Close()
			return nil, err
		}
		snap.Servers = append(snap.Servers, sv)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}

	// credentials (decrypt each blob under s.masterKey)
	rc, err := s.db.Query(`SELECT id,type,secret_blob,COALESCE(passphrase_blob,''),created_at,updated_at FROM credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rc.Next() {
		var c SnapshotCredential
		var secretBlob, passBlob []byte
		if err := rc.Scan(&c.ID, &c.Type, &secretBlob, &passBlob, &c.CreatedAt, &c.UpdatedAt); err != nil {
			rc.Close()
			return nil, err
		}
		pt, err := open(s.masterKey, secretBlob)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("decrypt credential %s: %w", c.ID, err)
		}
		c.Secret = pt
		if len(passBlob) > 0 {
			pp, err := open(s.masterKey, passBlob)
			if err != nil {
				rc.Close()
				return nil, fmt.Errorf("decrypt passphrase %s: %w", c.ID, err)
			}
			c.Passphrase = pp
		}
		snap.Credentials = append(snap.Credentials, c)
	}
	rc.Close()
	if err := rc.Err(); err != nil {
		return nil, err
	}

	// 网络拓扑（票 #123）：站点先导出，网络段携带站点标识符；导入时按
	// 同一依赖顺序写回。全量快照与按 profile 裁剪的快照共用同一段读取
	// 逻辑，只让设备选择在这里分叉。
	topology, err := s.readSnapshotTopologySection("", "")
	if err != nil {
		return nil, err
	}
	snap.Sites = topology.Sites
	snap.Networks = topology.Networks
	snap.Devices = topology.Devices
	snap.NetworkMemberships = topology.NetworkMemberships
	snap.DeviceLayoutCoordinates = topology.DeviceLayoutCoordinates

	// profiles
	rp, err := s.db.Query(`SELECT id,name,created_at,updated_at FROM profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rp.Next() {
		var p SnapshotProfile
		if err := rp.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rp.Close()
			return nil, err
		}
		snap.Profiles = append(snap.Profiles, p)
	}
	rp.Close()
	if err := rp.Err(); err != nil {
		return nil, err
	}

	// grants (profile_servers)
	rg, err := s.db.Query(`SELECT profile_id, server_id FROM profile_servers ORDER BY profile_id, server_id`)
	if err != nil {
		return nil, err
	}
	for rg.Next() {
		var g SnapshotGrant
		if err := rg.Scan(&g.ProfileID, &g.ServerID); err != nil {
			rg.Close()
			return nil, err
		}
		snap.Grants = append(snap.Grants, g)
	}
	rg.Close()
	if err := rg.Err(); err != nil {
		return nil, err
	}

	// projects — RAW SQL for token_hash/salt (ListProjects/GetProject omit them)
	rj, err := s.db.Query(`SELECT id,name,token_hash,token_salt,token_prefix,COALESCE(profile_id,''),status,created_at,updated_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rj.Next() {
		var p SnapshotProject
		if err := rj.Scan(&p.ID, &p.Name, &p.TokenHash, &p.TokenSalt, &p.TokenPrefix, &p.ProfileID, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rj.Close()
			return nil, err
		}
		snap.Projects = append(snap.Projects, p)
	}
	rj.Close()
	if err := rj.Err(); err != nil {
		return nil, err
	}

	// 按条目的锚 + 由条目推导的旧格式兜底行（票 #58）
	snap.EntryHostKeys, err = s.snapshotEntryPins()
	if err != nil {
		return nil, err
	}
	snap.HostKeys = deriveLegacyHostKeys(snap.Servers, snap.EntryHostKeys)

	// 机密项（票 #59）：当前值与上一个值都在这里解密，快照序列化后必须由
	// 调用方整体加密。
	if snap.Secrets, snap.SecretAllows, err = s.snapshotSecrets(); err != nil {
		return nil, err
	}
	if snap.SharedStorages, err = s.snapshotSharedStorages(""); err != nil {
		return nil, err
	}
	// 软件清单只走 owner 全量备份；profile 导出函数从不填充这两个字段。
	if snap.SoftwareDeclarations, snap.SoftwareDeclarationPorts, err = s.snapshotSoftwareInventory(); err != nil {
		return nil, err
	}
	// 实测与变化历史各表同样只进入 owner 全量备份。
	snap.SoftwareObservedItems, snap.SoftwareObservedPorts,
		snap.SoftwareCollectionStatuses, snap.SoftwareCollectSudoSettings,
		snap.SoftwareChangeHistory, err = s.snapshotSoftwareCollectionBackup()
	if err != nil {
		return nil, err
	}

	// policy (ticket #15): custom rules + disabled default-rule ids — global,
	// no profile scoping (the rules are maintained once on the authoritative
	// side and reach every client with the snapshot).
	snap.PolicyRules, snap.PolicyDisabledDefaults, err = s.exportPolicySections()
	if err != nil {
		return nil, err
	}

	// audit — RAW SQL (AuditRows clamps limit<=0 to 1; dump all)
	ra, err := s.db.Query(`SELECT id,ts,COALESCE(project_id,''),COALESCE(server_id,''),action,COALESCE(command,''),sudo,COALESCE(status,''),COALESCE(exit_code,0),COALESCE(duration_ms,0),COALESCE(source,''),COALESCE(source_ip,''),COALESCE(injected_environment_names,'') FROM audit_log ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for ra.Next() {
		var a SnapshotAudit
		var injectedNames string
		if err := ra.Scan(&a.ID, &a.TS, &a.ProjectID, &a.ServerID, &a.Action, &a.Command, &a.Sudo, &a.Status, &a.ExitCode, &a.DurationMS, &a.Source, &a.SourceIP, &injectedNames); err != nil {
			ra.Close()
			return nil, err
		}
		if injectedNames != "" {
			names, err := decodeAuditInjectedEnvironmentNames(injectedNames)
			if err != nil {
				ra.Close()
				return nil, fmt.Errorf("decode audit injected environment names: %w", err)
			}
			a.InjectedEnvironmentNames = names
		}
		snap.Audit = append(snap.Audit, a)
	}
	ra.Close()
	if err := ra.Err(); err != nil {
		return nil, err
	}

	return snap, nil
}

// snapshotSecrets 读取全部机密项及其可写入服务器关联并转成快照行（机密项
// 按内部标识符排序，关联按双方标识符排序）。
func (s *Store) snapshotSecrets() ([]SnapshotSecret, []SnapshotSecretAllow, error) {
	rs, err := s.db.Query(`
		SELECT id,name,note,env_name,value_blob,COALESCE(previous_value_blob,''),value_version,created_at,value_updated_at
		FROM secrets ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	secrets := make([]SnapshotSecret, 0)
	for rs.Next() {
		var row SnapshotSecret
		var valueBlob, previousBlob []byte
		if err := rs.Scan(&row.ID, &row.Name, &row.Note, &row.EnvironmentName, &valueBlob, &previousBlob, &row.ValueVersion, &row.CreatedAt, &row.ValueUpdatedAt); err != nil {
			rs.Close()
			return nil, nil, err
		}
		if row.Value, err = open(s.masterKey, valueBlob); err != nil {
			rs.Close()
			return nil, nil, fmt.Errorf("decrypt secret %s: %w", row.ID, err)
		}
		if len(previousBlob) > 0 {
			if row.PreviousValue, err = open(s.masterKey, previousBlob); err != nil {
				rs.Close()
				return nil, nil, fmt.Errorf("decrypt previous secret %s: %w", row.ID, err)
			}
		}
		secrets = append(secrets, row)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, nil, err
	}

	ra, err := s.db.Query(`SELECT secret_id, server_id FROM secret_server_allow ORDER BY secret_id, server_id`)
	if err != nil {
		return nil, nil, err
	}
	allows := make([]SnapshotSecretAllow, 0)
	for ra.Next() {
		var row SnapshotSecretAllow
		if err := ra.Scan(&row.SecretID, &row.ServerID); err != nil {
			ra.Close()
			return nil, nil, err
		}
		allows = append(allows, row)
	}
	ra.Close()
	return secrets, allows, ra.Err()
}

// snapshotSharedStorages 读取共享存储登记。profileID 非空时按挂载密码
// 机密项的可写入服务器列表与该 profile 授权服务器列表的交集裁剪。
func (s *Store) snapshotSharedStorages(profileID string) ([]SnapshotSharedStorage, error) {
	query := `
		SELECT shared.id,shared.name,shared.device_id,COALESCE(device.name,''),shared.mount_user,
		       shared.password_secret_id,shared.drive,shared.note,shared.directories,
		       shared.version,shared.change_note,shared.created_at,shared.updated_at
		FROM shared_storages shared
		LEFT JOIN devices device ON device.id=shared.device_id`
	arguments := make([]any, 0, 1)
	if profileID != "" {
		query += `
			WHERE EXISTS (
				SELECT 1
				FROM secret_server_allow allow
				JOIN profile_servers grant_row ON grant_row.server_id=allow.server_id
					WHERE allow.secret_id=shared.password_secret_id
				  AND grant_row.profile_id=?)`
		arguments = append(arguments, profileID)
	}
	query += ` ORDER BY shared.name,shared.id`
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SnapshotSharedStorage, 0)
	for rows.Next() {
		var row SnapshotSharedStorage
		var directoriesJSON string
		if err := rows.Scan(&row.ID, &row.Name, &row.DeviceID, &row.DeviceName, &row.MountUser,
			&row.PasswordSecretID, &row.Drive, &row.Note, &directoriesJSON,
			&row.Version, &row.ChangeNote, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, err
		}
		if row.DeviceName == "" {
			row.DeviceName = s.sharedStorageDeviceNames[row.ID]
		}
		directories, err := decodeSharedStorageDirectories(directoriesJSON)
		if err != nil {
			return nil, fmt.Errorf("读取共享存储 %s 的目录约定失败: %w", row.Name, err)
		}
		row.Directories = directories
		out = append(out, row)
	}
	return out, rows.Err()
}

// ExportSnapshotForProfile returns the authorization-scoped Snapshot served to a
// profile-BOUND device code at /snapshot (Plan 39): exactly the servers granted
// to profileID, ONLY the credentials those servers reference (credential_id ∪
// sudo_credential_id), the profile row + its grants, the projects bound to it,
// and the host_keys of those servers. Audit rows are NEVER carried — the offline
// audit trail is the client's local cache-audit.log sidecar, so command history
// stays server-side. The JSON shape is the SAME Snapshot envelope (a strict
// subset of ExportSnapshot's rows), so every existing client hydrates it
// unchanged. Unknown profileID is an explicit error (owner misconfiguration must
// be loud, not an empty-but-200 snapshot).
//
// The owner-side whole-vault export and this authorization-scoped assembly stay
// separate so the backup path cannot drift into accidental cropping.
// clientDeclaredVersion 表示这次拉取是否声明了客户端版本；未声明时按
// ADR 0014 去掉全部机密项。initiatingClientTokenID 是发起拉取的客户端
// 实例的设备码标识符；单机部署等没有客户端身份的调用方传空。网络拓扑
// 不按客户端版本裁剪，未声明版本的请求照常收到拓扑段。
func (s *Store) ExportSnapshotForProfile(profileID string, clientDeclaredVersion bool, initiatingClientTokenID string) (*Snapshot, error) {
	if err := requireProfile(s.db, profileID); err != nil {
		return nil, err
	}
	revision, err := s.SnapshotRevision()
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{Version: 1, SnapshotRevision: revision, RecommendedVersion: buildinfo.Version}

	// profile row
	rp, err := s.db.Query(`SELECT id,name,created_at,updated_at FROM profiles WHERE id=?`, profileID)
	if err != nil {
		return nil, err
	}
	for rp.Next() {
		var p SnapshotProfile
		if err := rp.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rp.Close()
			return nil, err
		}
		snap.Profiles = append(snap.Profiles, p)
	}
	rp.Close()
	if err := rp.Err(); err != nil {
		return nil, err
	}

	// grants of THIS profile only
	rg, err := s.db.Query(`SELECT profile_id, server_id FROM profile_servers WHERE profile_id=? ORDER BY server_id`, profileID)
	if err != nil {
		return nil, err
	}
	for rg.Next() {
		var g SnapshotGrant
		if err := rg.Scan(&g.ProfileID, &g.ServerID); err != nil {
			rg.Close()
			return nil, err
		}
		snap.Grants = append(snap.Grants, g)
	}
	rg.Close()
	if err := rg.Err(); err != nil {
		return nil, err
	}

	// servers granted to the profile (same column shape as ExportSnapshot)
	rs, err := s.db.Query(`SELECT id,name,host,port,user,auth_method,COALESCE(credential_id,''),COALESCE(sudo_credential_id,''),shell_type,COALESCE(tags,''),description,location,hardware,services,role,caveats,expose_host,COALESCE(device_id,''),created_at,updated_at,revision
		FROM servers WHERE id IN (SELECT server_id FROM profile_servers WHERE profile_id=?) ORDER BY id`, profileID)
	if err != nil {
		return nil, err
	}
	// referenced credential ids, collected while scanning servers
	credIDs := make(map[string]bool)
	for rs.Next() {
		var sv SnapshotServer
		if err := rs.Scan(&sv.ID, &sv.Name, &sv.Host, &sv.Port, &sv.User, &sv.AuthMethod,
			&sv.CredentialID, &sv.SudoCredentialID, &sv.ShellType, &sv.TagsRaw, &sv.Description, &sv.Location,
			&sv.Hardware, &sv.Services, &sv.Role, &sv.Caveats, &sv.ExposeHost, &sv.DeviceID, &sv.CreatedAt, &sv.UpdatedAt, &sv.Revision); err != nil {
			rs.Close()
			return nil, err
		}
		if sv.CredentialID != "" {
			credIDs[sv.CredentialID] = true
		}
		if sv.SudoCredentialID != "" {
			credIDs[sv.SudoCredentialID] = true
		}
		snap.Servers = append(snap.Servers, sv)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}

	// 站点与网络全部下发，字段与全量快照相同。设备只保留：挂着本次授权
	// 条目的设备、不受管设备，以及发起拉取的客户端实例所在的设备。设备上
	// 的客户端实例关联只对发起方自己的设备保留。
	topology, err := s.readSnapshotTopologySection(profileID, initiatingClientTokenID)
	if err != nil {
		return nil, err
	}
	snap.Sites = topology.Sites
	snap.Networks = topology.Networks
	snap.Devices = topology.Devices
	snap.NetworkMemberships = topology.NetworkMemberships
	snap.DeviceLayoutCoordinates = topology.DeviceLayoutCoordinates

	// ONLY the referenced credentials (decrypted under s.masterKey). The IN
	// clause is built from collected ids — a granted set is small (tens), and
	// placeholders keep it parameterized.
	if len(credIDs) > 0 {
		args := make([]any, 0, len(credIDs))
		ph := make([]string, 0, len(credIDs))
		for id := range credIDs {
			ph = append(ph, "?")
			args = append(args, id)
		}
		rc, err := s.db.Query(
			`SELECT id,type,secret_blob,COALESCE(passphrase_blob,''),created_at,updated_at FROM credentials WHERE id IN (`+
				strings.Join(ph, ",")+`) ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		for rc.Next() {
			var c SnapshotCredential
			var secretBlob, passBlob []byte
			if err := rc.Scan(&c.ID, &c.Type, &secretBlob, &passBlob, &c.CreatedAt, &c.UpdatedAt); err != nil {
				rc.Close()
				return nil, err
			}
			pt, err := open(s.masterKey, secretBlob)
			if err != nil {
				rc.Close()
				return nil, fmt.Errorf("decrypt credential %s: %w", c.ID, err)
			}
			c.Secret = pt
			if len(passBlob) > 0 {
				pp, err := open(s.masterKey, passBlob)
				if err != nil {
					rc.Close()
					return nil, fmt.Errorf("decrypt passphrase %s: %w", c.ID, err)
				}
				c.Passphrase = pp
			}
			snap.Credentials = append(snap.Credentials, c)
		}
		rc.Close()
		if err := rc.Err(); err != nil {
			return nil, err
		}
	}

	// projects bound to THIS profile (raw SQL keeps token_hash/salt verbatim so
	// the machine's own project tokens keep validating offline; other profiles'
	// projects are correctly absent — their tokens fail offline verification)
	rj, err := s.db.Query(`SELECT id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at FROM projects WHERE profile_id=? ORDER BY id`, profileID)
	if err != nil {
		return nil, err
	}
	for rj.Next() {
		var p SnapshotProject
		if err := rj.Scan(&p.ID, &p.Name, &p.TokenHash, &p.TokenSalt, &p.TokenPrefix, &p.ProfileID, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rj.Close()
			return nil, err
		}
		snap.Projects = append(snap.Projects, p)
	}
	rj.Close()
	if err := rj.Err(); err != nil {
		return nil, err
	}

	// 按条目的锚只带被授权条目的；旧格式行由裁剪后的条目推导（票 #58）。
	snap.EntryHostKeys, err = s.snapshotEntryPins()
	if err != nil {
		return nil, err
	}
	granted := make([]SnapshotEntryHostKey, 0, len(snap.Servers))
	for _, sv := range snap.Servers {
		for _, p := range snap.EntryHostKeys {
			if p.ServerID == sv.ID {
				granted = append(granted, p)
			}
		}
	}
	snap.EntryHostKeys = granted
	snap.HostKeys = deriveLegacyHostKeys(snap.Servers, snap.EntryHostKeys)

	// policy (ticket #15): the SAME global policy sections the owner-side
	// export carries — rules are one set for every machine, never cropped by
	// profile (audit below stays intentionally empty).
	snap.PolicyRules, snap.PolicyDisabledDefaults, err = s.exportPolicySections()
	if err != nil {
		return nil, err
	}

	// 机密项只下发给声明了客户端版本的客户端，并且按该 profile 授权的
	// 服务器集合裁剪；审计从不离开权威端。
	if clientDeclaredVersion {
		secrets, allows, err := s.snapshotSecrets()
		if err != nil {
			return nil, err
		}
		grantedServerIDs := make([]string, 0, len(snap.Servers))
		for _, server := range snap.Servers {
			grantedServerIDs = append(grantedServerIDs, server.ID)
		}
		// 裁剪判定与机密项列表共用 secretServersInScope：只保留与 profile
		// 授予的服务器有交集的机密项，关联行只保留交集内的服务器条目。
		cropRows := make([]secretDisplayRow, 0, len(allows))
		for _, allow := range allows {
			cropRows = append(cropRows, secretDisplayRow{
				secretID: allow.SecretID, hasServer: true, serverID: allow.ServerID,
			})
		}
		inScope := secretServersInScope(cropRows, ScopeToServers(grantedServerIDs...))
		scopedAllows := make([]SnapshotSecretAllow, 0, len(allows))
		for _, allow := range allows {
			for _, server := range inScope[allow.SecretID] {
				if server.id == allow.ServerID {
					scopedAllows = append(scopedAllows, allow)
					break
				}
			}
		}
		scopedSecrets := make([]SnapshotSecret, 0, len(secrets))
		for _, secret := range secrets {
			if _, exists := inScope[secret.ID]; exists {
				scopedSecrets = append(scopedSecrets, secret)
			}
		}
		snap.Secrets = scopedSecrets
		snap.SecretAllows = scopedAllows
	}
	if snap.SharedStorages, err = s.snapshotSharedStorages(profileID); err != nil {
		return nil, err
	}
	return snap, nil
}

// snapshotTopologySection 是快照里的五段网络拓扑数据。全量快照与按 profile
// 裁剪的快照都用这个结构承载，避免两类导出的字段读取方式漂移。
type snapshotTopologySection struct {
	Sites                   []SnapshotSite
	Networks                []SnapshotNetwork
	Devices                 []SnapshotDevice
	NetworkMemberships      []SnapshotNetworkMembership
	DeviceLayoutCoordinates []SnapshotDeviceLayoutCoordinate
}

// readSnapshotTopologySection 读取快照需要的五段网络拓扑数据。profileID
// 非空时按该 profile 的授权范围裁剪设备，并把客户端实例关联只保留给发起
// 拉取的客户端实例；为空时读取全量拓扑。站点与网络始终全部读取。
func (s *Store) readSnapshotTopologySection(profileID, initiatingClientTokenID string) (snapshotTopologySection, error) {
	cropToProfile := profileID != ""
	var sites []SnapshotSite
	siteRows, err := s.db.Query(`SELECT id,name,note,created_at,updated_at FROM sites ORDER BY id`)
	if err != nil {
		return snapshotTopologySection{}, err
	}
	for siteRows.Next() {
		var site SnapshotSite
		if err := siteRows.Scan(&site.ID, &site.Name, &site.Note, &site.CreatedAt, &site.UpdatedAt); err != nil {
			siteRows.Close()
			return snapshotTopologySection{}, err
		}
		sites = append(sites, site)
	}
	siteRows.Close()
	if err := siteRows.Err(); err != nil {
		return snapshotTopologySection{}, err
	}

	var networks []SnapshotNetwork
	networkRows, err := s.db.Query(`
		SELECT id,name,type,COALESCE(site_id,''),note,cidr,bandwidth_mbps,metered,created_at,updated_at
		FROM networks ORDER BY id`)
	if err != nil {
		return snapshotTopologySection{}, err
	}
	for networkRows.Next() {
		var network SnapshotNetwork
		if err := networkRows.Scan(&network.ID, &network.Name, &network.Type, &network.SiteID,
			&network.Note, &network.CIDR, &network.BandwidthMbps, &network.Metered,
			&network.CreatedAt, &network.UpdatedAt); err != nil {
			networkRows.Close()
			return snapshotTopologySection{}, err
		}
		networks = append(networks, network)
	}
	networkRows.Close()
	if err := networkRows.Err(); err != nil {
		return snapshotTopologySection{}, err
	}

	deviceQuery := `
		SELECT id,name,note,COALESCE(client_token_id,''),client_name,created_at,updated_at
		FROM devices
		`
	queryArguments := make([]any, 0, 4)
	if cropToProfile {
		deviceQuery += `WHERE id IN (
			          SELECT COALESCE(server.device_id,'')
			          FROM servers server
			          JOIN profile_servers grant_row ON grant_row.server_id=server.id
			          WHERE grant_row.profile_id=?)
			   OR (
			          NOT EXISTS(SELECT 1 FROM servers server WHERE server.device_id=devices.id)
			          AND COALESCE(devices.client_token_id,'')='')
			   OR (?<>'' AND COALESCE(devices.client_token_id,'')=?)
			`
		queryArguments = append(queryArguments, profileID, initiatingClientTokenID, initiatingClientTokenID)
	}
	deviceQuery += `ORDER BY id`
	deviceRows, err := s.db.Query(deviceQuery, queryArguments...)
	if err != nil {
		return snapshotTopologySection{}, err
	}
	var devices []SnapshotDevice
	delivered := make(map[string]bool)
	for deviceRows.Next() {
		var device SnapshotDevice
		if err := deviceRows.Scan(&device.ID, &device.Name, &device.Note, &device.ClientTokenID,
			&device.ClientName, &device.CreatedAt, &device.UpdatedAt); err != nil {
			deviceRows.Close()
			return snapshotTopologySection{}, err
		}
		if cropToProfile && device.ClientTokenID != initiatingClientTokenID {
			device.ClientTokenID, device.ClientName = "", ""
		}
		devices = append(devices, device)
		delivered[device.ID] = true
	}
	deviceRows.Close()
	if err := deviceRows.Err(); err != nil {
		return snapshotTopologySection{}, err
	}

	var memberships []SnapshotNetworkMembership
	membershipRows, err := s.db.Query(`
		SELECT device_id,network_id,address,registration_order,created_at,updated_at
		FROM network_memberships ORDER BY device_id,network_id`)
	if err != nil {
		return snapshotTopologySection{}, err
	}
	for membershipRows.Next() {
		var membership SnapshotNetworkMembership
		if err := membershipRows.Scan(&membership.DeviceID, &membership.NetworkID, &membership.Address,
			&membership.RegistrationOrder, &membership.CreatedAt, &membership.UpdatedAt); err != nil {
			membershipRows.Close()
			return snapshotTopologySection{}, err
		}
		if delivered[membership.DeviceID] {
			memberships = append(memberships, membership)
		}
	}
	membershipRows.Close()
	if err := membershipRows.Err(); err != nil {
		return snapshotTopologySection{}, err
	}

	allCoordinates, err := s.snapshotDeviceLayoutCoordinates()
	if err != nil {
		return snapshotTopologySection{}, err
	}
	coordinates := make([]SnapshotDeviceLayoutCoordinate, 0, len(allCoordinates))
	for _, coordinate := range allCoordinates {
		if delivered[coordinate.DeviceID] {
			coordinates = append(coordinates, coordinate)
		}
	}
	return snapshotTopologySection{
		Sites:                   sites,
		Networks:                networks,
		Devices:                 devices,
		NetworkMemberships:      memberships,
		DeviceLayoutCoordinates: coordinates,
	}, nil
}

// ErrVaultNotEmpty is returned by ImportSnapshot when the target store already
// has servers or topology rows. Import is restore-into-empty only — no silent clobber. Delete
// store.db (or point SSHMGR_STORE at a fresh path) to get an empty vault.
var ErrVaultNotEmpty = errors.New("vault is not empty; import into a fresh/empty store (move/delete store.db first)")

// ImportSnapshot restores an owner-side full backup or export into THIS store in
// one transaction:
//   - credentials are RE-SEALED under this store's master key (master-key-independent file);
//   - rows are inserted id-preserving in foreign-key order (sites -> networks ->
//     devices -> network memberships -> layout coordinates -> credentials ->
//     servers -> profiles -> grants -> secrets -> shared storages -> projects ->
//     host keys -> audit);
//   - projects.token_hash/salt/prefix are written verbatim so the original plaintext token keeps validating.
//
// Refuses a non-empty target (any row in servers, sites, networks, devices,
// network memberships, or layout coordinates) with ErrVaultNotEmpty. 旧备份
// 或旧导出没有拓扑段时，这个 owner 导入入口执行与升级相同的设备生成迁移。
func (s *Store) ImportSnapshot(snap *Snapshot) error {
	return s.importSnapshot(snap, true)
}

// ImportSnapshotForClientCache restores a profile-scoped snapshot delivered by
// the authoritative machine into a fresh client cache. 快照里的网络拓扑与服
// 务器条目的所属设备引用按外键顺序原样写回；本入口永不执行设备生成迁
// 移，也不产生迁移审计。
func (s *Store) ImportSnapshotForClientCache(snap *Snapshot) error {
	if err := s.setForeignKeysEnabled(false); err != nil {
		return err
	}
	defer func() { _ = s.setForeignKeysEnabled(true) }()
	return s.importSnapshot(snap, false)
}

// setForeignKeysEnabled 只在同一个数据库连接上切换 SQLite 的外键检查。
// 客户端缓存允许共享存储引用已被 profile 裁剪掉的提供方设备；权威端与
// 单机部署仍保持外键检查开启。
func (s *Store) setForeignKeysEnabled(enabled bool) error {
	setting := "ON"
	if !enabled {
		setting = "OFF"
	}
	if _, err := s.db.Exec("PRAGMA foreign_keys=" + setting); err != nil {
		return fmt.Errorf("set foreign_keys=%s: %w", setting, err)
	}
	return nil
}

func (s *Store) importSnapshot(snap *Snapshot, ownerImport bool) error {
	if s.readOnly {
		return ErrReadOnly
	}
	var existingRows int
	if err := s.db.QueryRow(`
		SELECT
		  (SELECT COUNT(*) FROM servers) +
		  (SELECT COUNT(*) FROM sites) +
		  (SELECT COUNT(*) FROM networks) +
		  (SELECT COUNT(*) FROM devices) +
		  (SELECT COUNT(*) FROM network_memberships) +
		  (SELECT COUNT(*) FROM device_layout_coordinates)
	`).Scan(&existingRows); err != nil {
		return err
	}
	if existingRows > 0 {
		return ErrVaultNotEmpty
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	var currentRevision int64
	if err = tx.QueryRow(`SELECT revision FROM snapshot_revision WHERE id=1`).Scan(&currentRevision); err != nil {
		return err
	}

	// 0. 网络拓扑（票 #123）：站点先写回，网络随后按站点标识符恢复。
	// 旧快照没有这两段时循环体不执行，导入后仍是零行。
	for _, site := range snap.Sites {
		if _, err := tx.Exec(
			`INSERT INTO sites(id,name,name_lower,note,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			site.ID, site.Name, strings.ToLower(site.Name), site.Note, site.CreatedAt, site.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert site %s: %w", site.ID, err)
		}
	}
	for _, network := range snap.Networks {
		if _, err := tx.Exec(`
			INSERT INTO networks(id,name,name_lower,type,site_id,note,cidr,bandwidth_mbps,metered,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			network.ID, network.Name, strings.ToLower(network.Name), network.Type,
			nullIfEmpty(network.SiteID), network.Note, network.CIDR, network.BandwidthMbps,
			network.Metered, network.CreatedAt, network.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert network %s: %w", network.ID, err)
		}
	}
	for _, device := range snap.Devices {
		if _, err := tx.Exec(`
			INSERT INTO devices(id,name,name_lower,note,client_token_id,client_name,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			device.ID, device.Name, strings.ToLower(device.Name), device.Note,
			nullIfEmpty(device.ClientTokenID), device.ClientName, device.CreatedAt, device.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert device %s: %w", device.ID, err)
		}
	}
	// 旧快照与旧备份没有登记序号时，按这段快照内的出现顺序补齐；新快
	// 照的序号原样保留，保证客户端缓存与权威端的登记顺序一致。
	nextRegistrationOrder := int64(0)
	for _, membership := range snap.NetworkMemberships {
		if membership.RegistrationOrder > nextRegistrationOrder {
			nextRegistrationOrder = membership.RegistrationOrder
		}
		if membership.RegistrationOrder <= 0 {
			nextRegistrationOrder++
			membership.RegistrationOrder = nextRegistrationOrder
		}
		if _, err := tx.Exec(`
			INSERT INTO network_memberships(device_id,network_id,address,address_lower,registration_order,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?)`,
			membership.DeviceID, membership.NetworkID, membership.Address,
			strings.ToLower(membership.Address), membership.RegistrationOrder,
			membership.CreatedAt, membership.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert network membership %s/%s: %w", membership.DeviceID, membership.NetworkID, err)
		}
	}
	if err := importDeviceLayoutCoordinatesTx(tx, snap.DeviceLayoutCoordinates); err != nil {
		return err
	}
	// 软件清单只恢复到 owner 库。客户端缓存导入即使收到带清单段的旧文件
	// 或异常文件，也显式跳过，保证客户端本地永远没有软件清单副本。
	if ownerImport {
		if err := importSoftwareInventoryTx(tx, snap.SoftwareDeclarations, snap.SoftwareDeclarationPorts); err != nil {
			return err
		}
	}

	// 1. credentials (re-seal under THIS store's master key)
	for _, c := range snap.Credentials {
		secretBlob, err := seal(s.masterKey, c.Secret)
		if err != nil {
			return fmt.Errorf("seal credential %s: %w", c.ID, err)
		}
		var passArg any // nil -> SQL NULL
		if len(c.Passphrase) > 0 {
			pb, err := seal(s.masterKey, c.Passphrase)
			if err != nil {
				return fmt.Errorf("seal passphrase %s: %w", c.ID, err)
			}
			passArg = pb
		}
		if _, err := tx.Exec(`INSERT INTO credentials(id,type,secret_blob,passphrase_blob,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			c.ID, c.Type, secretBlob, passArg, c.CreatedAt, c.UpdatedAt); err != nil {
			return fmt.Errorf("insert credential %s: %w", c.ID, err)
		}
	}

	// 2. servers (sudo_credential_id "" -> NULL to mirror the original NULL semantics for empty;
	//    credential_id same since Plan 20 C0 — "" must be NULL or the FK on credentials(id) rejects it)
	for _, sv := range snap.Servers {
		var sudoArg any
		if sv.SudoCredentialID != "" {
			sudoArg = sv.SudoCredentialID
		}
		credArg := nullIfEmpty(sv.CredentialID)
		// 旧快照无 shell_type 字段时为零值——归一为 posix(旧快照导入
		// 即按默认 POSIX 处理)。与 host_keys pin 列的归一化同理:ADD COLUMN
		// DEFAULT 只约束存量行,不管显式插入。
		shellArg := string(models.NormalizeShellType(sv.ShellType))
		if _, err := tx.Exec(`INSERT INTO servers(id,name,host,port,user,auth_method,credential_id,sudo_credential_id,shell_type,tags,description,location,hardware,services,role,caveats,expose_host,device_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sv.ID, sv.Name, sv.Host, sv.Port, sv.User, sv.AuthMethod, credArg, sudoArg, shellArg, sv.TagsRaw, sv.Description, sv.Location, sv.Hardware, sv.Services, sv.Role, sv.Caveats, sv.ExposeHost, nullIfEmpty(sv.DeviceID), sv.CreatedAt, sv.UpdatedAt, sv.Revision); err != nil {
			return fmt.Errorf("insert server %s: %w", sv.ID, err)
		}
	}
	// 软件清单的实测、采集状态、sudo 开关与变化历史在这里恢复：sudo 开
	// 关引用服务器条目，必须等服务器条目写完；仍然只在 owner 全量导入。
	if ownerImport {
		if err := importSoftwareCollectionBackupTx(tx, snap); err != nil {
			return err
		}
	}

	// 3. profiles
	for _, p := range snap.Profiles {
		if _, err := tx.Exec(`INSERT INTO profiles(id,name,created_at,updated_at) VALUES(?,?,?,?)`, p.ID, p.Name, p.CreatedAt, p.UpdatedAt); err != nil {
			return fmt.Errorf("insert profile %s: %w", p.ID, err)
		}
	}

	// 4. grants (profile_servers)
	for _, g := range snap.Grants {
		if _, err := tx.Exec(`INSERT INTO profile_servers(profile_id,server_id) VALUES(?,?)`, g.ProfileID, g.ServerID); err != nil {
			return fmt.Errorf("insert grant %s/%s: %w", g.ProfileID, g.ServerID, err)
		}
	}

	// 4a. 机密项（票 #59）：值用目标库的主密钥重新加密，其余字段原样恢复。
	// 旧快照没有机密项段时循环体不执行，导入后机密项为空。
	for _, sec := range snap.Secrets {
		valueBlob, err := seal(s.masterKey, sec.Value)
		if err != nil {
			return fmt.Errorf("seal secret %s: %w", sec.ID, err)
		}
		var previousArg any // nil -> SQL NULL
		if len(sec.PreviousValue) > 0 {
			previousBlob, err := seal(s.masterKey, sec.PreviousValue)
			if err != nil {
				return fmt.Errorf("seal previous secret %s: %w", sec.ID, err)
			}
			previousArg = previousBlob
		}
		if _, err := tx.Exec(
			`INSERT INTO secrets(id,name,name_lower,note,env_name,env_name_lower,value_blob,previous_value_blob,value_version,created_at,value_updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			sec.ID, sec.Name, strings.ToLower(sec.Name), sec.Note, sec.EnvironmentName, strings.ToLower(sec.EnvironmentName), valueBlob, previousArg, sec.ValueVersion, sec.CreatedAt, sec.ValueUpdatedAt); err != nil {
			return fmt.Errorf("insert secret %s: %w", sec.ID, err)
		}
	}
	for _, a := range snap.SecretAllows {
		if _, err := tx.Exec(`INSERT INTO secret_server_allow(secret_id,server_id) VALUES(?,?)`, a.SecretID, a.ServerID); err != nil {
			return fmt.Errorf("insert secret allow %s/%s: %w", a.SecretID, a.ServerID, err)
		}
	}

	// 未声明客户端版本的快照会保留共享存储、去掉全部机密项。客户端缓存
	// 导入需要满足共享存储对机密项的外键引用；这里只给这个一次性查询库
	// 补标识符占位行，不把它写回快照，也不让机密项查看入口看见。
	if !ownerImport {
		deliveredSecrets := make(map[string]bool, len(snap.Secrets))
		for _, secret := range snap.Secrets {
			deliveredSecrets[secret.ID] = true
		}
		for _, shared := range snap.SharedStorages {
			if deliveredSecrets[shared.PasswordSecretID] {
				continue
			}
			placeholderBlob, err := seal(s.masterKey, []byte{})
			if err != nil {
				return fmt.Errorf("seal placeholder for shared storage secret %s: %w", shared.PasswordSecretID, err)
			}
			if _, err := tx.Exec(`
				INSERT INTO secrets(id,name,name_lower,note,env_name,env_name_lower,value_blob,value_version,created_at,value_updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?)`,
				shared.PasswordSecretID, shared.PasswordSecretID, strings.ToLower(shared.PasswordSecretID),
				"", "", "", placeholderBlob, 1, shared.CreatedAt, shared.UpdatedAt); err != nil {
				return fmt.Errorf("insert placeholder for shared storage secret %s: %w", shared.PasswordSecretID, err)
			}
			deliveredSecrets[shared.PasswordSecretID] = true
		}
	}

	// 4b. 共享存储依赖设备与挂载密码机密项，放在两段之后导入。版本号
	// 与变更说明原样恢复，导入不重算。
	deliveredDevices := make(map[string]bool, len(snap.Devices))
	for _, device := range snap.Devices {
		deliveredDevices[device.ID] = true
	}
	for _, shared := range snap.SharedStorages {
		if !ownerImport && !deliveredDevices[shared.DeviceID] {
			if shared.DeviceName == "" {
				return fmt.Errorf("客户端快照的共享存储 %s 缺少提供方设备名", shared.Name)
			}
			if s.sharedStorageDeviceNames == nil {
				s.sharedStorageDeviceNames = make(map[string]string)
			}
			s.sharedStorageDeviceNames[shared.ID] = shared.DeviceName
		}
		directories := shared.Directories
		if directories == nil {
			directories = []models.SharedStorageDirectory{}
		}
		directoriesJSON, err := json.Marshal(directories)
		if err != nil {
			return fmt.Errorf("encode shared storage %s directories: %w", shared.ID, err)
		}
		if _, err := tx.Exec(`
			INSERT INTO shared_storages(
				id,name,name_lower,device_id,mount_user,password_secret_id,drive,note,directories,version,change_note,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			shared.ID, shared.Name, strings.ToLower(shared.Name), shared.DeviceID, shared.MountUser,
			shared.PasswordSecretID, shared.Drive, shared.Note, string(directoriesJSON),
			shared.Version, shared.ChangeNote, shared.CreatedAt, shared.UpdatedAt); err != nil {
			return fmt.Errorf("insert shared storage %s: %w", shared.ID, err)
		}
	}

	// 5. projects — hash/salt/prefix VERBATIM (original plaintext token keeps validating)
	for _, p := range snap.Projects {
		// 空的 profile 绑定（profile 删除后留下的已吊销项目令牌）按
		// NULL 落库；空字符串过不了外部键。
		if _, err := tx.Exec(`INSERT INTO projects(id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Name, p.TokenHash, p.TokenSalt, p.TokenPrefix, nullableString(p.ProfileID), p.Status, p.CreatedAt, p.UpdatedAt); err != nil {
			return fmt.Errorf("insert project %s: %w", p.ID, err)
		}
	}

	// 6. 锚（票 #58）。带按条目锚的新快照：直接按条目落库；随快照携带的
	// host_keys 是导出时推导的旧格式兼容行，不再作为事实源导入。只有旧格式
	// 锚的旧快照 / 旧备份：先落旧表，再执行与升级相同的迁移规则。
	if len(snap.EntryHostKeys) > 0 {
		for _, p := range snap.EntryHostKeys {
			format, source := normalizeSnapshotPinColumns(p.PinFormat, p.PinSource)
			if _, err := tx.Exec(
				`INSERT INTO entry_host_keys(server_id,key_blob,created_at,pin_format,pin_source,pin_device,handshake_addr) VALUES(?,?,?,?,?,?,?)`,
				p.ServerID, p.KeyBlob, p.CreatedAt, format, source, p.PinDevice, p.HandshakeAddr); err != nil {
				return fmt.Errorf("insert entry host_key %s: %w", p.ServerID, err)
			}
		}
	} else {
		for _, h := range snap.HostKeys {
			format, source := normalizeSnapshotPinColumns(h.PinFormat, h.PinSource)
			if _, err := tx.Exec(`INSERT INTO host_keys(host_port,key_blob,created_at,pin_format,pin_source,pin_device) VALUES(?,?,?,?,?,?)`,
				h.HostPort, h.KeyBlob, h.CreatedAt, format, source, h.PinDevice); err != nil {
				return fmt.Errorf("insert host_key %s: %w", h.HostPort, err)
			}
		}
	}

	// 7. policy (ticket #15) — custom rules + disabled default-rule ids. No FK
	// dependencies; an old snapshot's nil sections import zero rows (the
	// built-in default rules then remain fully in effect).
	for _, r := range snap.PolicyRules {
		if _, err := tx.Exec(`INSERT INTO policy_rules(id,shells,commands,args,path_category,regex,disposition,explanation,alternative,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, marshalStringList(r.Shells), marshalStringList(r.Commands), marshalStringList(r.Args),
			r.PathCategory, r.Regex, r.Disposition, r.Explanation, r.Alternative, r.CreatedAt, r.UpdatedAt); err != nil {
			return fmt.Errorf("insert policy rule %s: %w", r.ID, err)
		}
	}
	for _, id := range snap.PolicyDisabledDefaults {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO policy_disabled_defaults(rule_id, created_at) VALUES(?,?)`, id, now()); err != nil {
			return fmt.Errorf("insert disabled default %s: %w", id, err)
		}
	}

	// 8. audit (id autoincrement — insert without id, letting the target assign; ts preserved)
	for _, a := range snap.Audit {
		injectedNames, err := encodeAuditInjectedEnvironmentNames(a.InjectedEnvironmentNames)
		if err != nil {
			return fmt.Errorf("encode audit injected environment names: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO audit_log(ts,project_id,server_id,action,command,sudo,status,exit_code,duration_ms,source,source_ip,injected_environment_names) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.TS, nullIfEmpty(a.ProjectID), nullIfEmpty(a.ServerID), a.Action, nullIfEmpty(a.Command), a.Sudo, nullIfEmpty(a.Status), a.ExitCode, a.DurationMS, nullIfEmpty(a.Source), nullIfEmpty(a.SourceIP), injectedNames); err != nil {
			return fmt.Errorf("insert audit: %w", err)
		}
	}

	// 旧格式锚迁移必须在恢复快照历史审计之后执行：迁移审计的编号才会排在
	// 历史行后面，保持导入后的历史顺序可读。
	if len(snap.EntryHostKeys) == 0 {
		if err := migrateEntryPinsTx(tx); err != nil {
			return fmt.Errorf("migrate legacy host keys: %w", err)
		}
	}
	// 旧备份与旧导出没有设备段。owner 导入后强制执行与升级相同的设备生成
	// 规则：目标库虽然是新库且打开时已写完成标记，这次恢复的数据仍来自旧
	// 格式；客户端缓存导入显式关闭这条迁移。
	if ownerImport && len(snap.Devices) == 0 && len(snap.NetworkMemberships) == 0 {
		if err := migrateServerDevicesTx(tx, s.nowTime(), true); err != nil {
			return fmt.Errorf("migrate server devices: %w", err)
		}
	}

	// 9. 快照修订号推进到导入前值与快照携带值中较大者的后一个值；配置行
	// 导入过程中触发的加一只影响事务内部状态，最终由这里统一覆盖。它必须
	// 放在包括旧格式锚迁移在内的全部配置写入之后。
	if err := setSnapshotRevisionTx(tx, snap.SnapshotRevision, currentRevision); err != nil {
		return fmt.Errorf("advance snapshot revision after import: %w", err)
	}

	if err := s.commitSecretChange(tx); err != nil {
		return err
	}
	return nil
}

// nullIfEmpty returns nil (-> SQL NULL) for "", else s. Mirrors COALESCE-on-read.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
