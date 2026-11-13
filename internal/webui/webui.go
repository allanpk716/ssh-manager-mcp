// Package webui 提供 sshmgr 的 Web 管理界面。回环模式由命令行把一次性启动
// 令牌换成进程内会话，再进入服务器列表；本包不直接读写 vault 或客户端快
// 照，页面数据与访问管理操作一律经配置注入的来源接口取得。
package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Role 是 Web UI 需要区分的本机角色子集：本包只服务单机与客户端两种回环
// 模式；权威端 Web UI 的独立监听功能上线时再引入它的构造参数。
type Role int

const (
	// RoleStandalone 表示单机角色：自持 vault，页面能力标记为全功能可写。
	RoleStandalone Role = iota
	// RoleClient 表示客户端角色：只读查看本地快照，修改入口一律不出现。
	RoleClient
	// RoleServer 表示权威端角色：由独立 HTTPS 监听提供全功能可写页面。
	RoleServer
)

// ServerRow 是服务器列表的展示行：名称、用户、主机、端口与「需要注意」标记。
type ServerRow struct {
	ID             string
	Name           string
	User           string
	Host           string
	Port           int
	NeedsAttention bool
}

// ServerSource 是 Web UI 读取服务器数据的唯一入口，由命令行按本机角色提供
// 单机 vault 来源或客户端快照来源。instance 是客户端缓存实例名，空串表示
// 默认实例；单机与权威端来源忽略它。Servers 供访问区域的授权表单复用基本
// 行；ServerList 与 ServerDetail 供服务器页展示注意原因与详情。
type ServerSource interface {
	Servers(instance string) ([]ServerRow, error)
	ServerList(instance string, attentionOnly bool) ([]ServerListEntry, error)
	ServerDetail(instance, identifier string) (*ServerDetailView, error)
}

// SecretRow 是机密项页的一行展示数据。字段与命令行机密项列表一致，只含
// 可见元数据，绝不包含值、值的哈希或长度。
type SecretRow struct {
	ID              string
	Name            string
	Note            string
	EnvironmentName string
	AllowedServers  []string
	CreatedAt       int64
	ValueUpdatedAt  int64
}

// SecretSource 是 Web UI 读取机密项可见数据的唯一入口。单机与权威端由机
// 密项管理模块适配；客户端由本机加密快照适配。两条路径都不读取值。
type SecretSource interface {
	ListSecrets(instance, query, server string) ([]SecretRow, error)
}

// SecretDeliverySecretRow 是页面展示的一个下发记录机密项引用。
type SecretDeliverySecretRow struct {
	Name         string
	ValueVersion int64
	Deleted      bool
}

// SecretDeliveryRecordRow 是机密项详情页的一条下发记录展示行。
type SecretDeliveryRecordRow struct {
	ServerName             string
	Path                   string
	Template               string
	FileSHA256             string
	WrittenAt              time.Time
	InitiatingDeviceName   string
	InitiatingInstanceName string
	Secrets                []SecretDeliverySecretRow
}

// SecretDeliveryRecordSource 由持有权威 vault 或本机 vault 的机密项来源实
// 现；客户端快照来源不实现，也不提供权威端下发记录查询。
type SecretDeliveryRecordSource interface {
	SecretDeliveryRecords(secretName string) ([]SecretDeliveryRecordRow, error)
}

// SecretWriteSource 描述一次机密项写请求的来源地址，适配层把它写进审计行。
type SecretWriteSource struct {
	RemoteAddress string
}

// SecretNotFoundError 表示路径里的机密项不存在。
type SecretNotFoundError struct {
	Identifier string
}

func (e *SecretNotFoundError) Error() string {
	return "机密项 " + e.Identifier + " 不存在"
}

// Config 是 Web UI Handler 的构造参数。Port 同时用于会话 cookie 命名与
// Host、Origin 校验；Now 是可选时钟，缺省使用标准时间。
type Config struct {
	Role    Role
	Servers ServerSource
	// ServerWriter 是服务器页写操作来源。单机与权威端必须提供；客户端不提
	// 供，写路由不存在。
	ServerWriter ServerWriter
	// ServerConnectionTester 启动并读取新增服务器后的自动连接测试任务。
	ServerConnectionTester ServerConnectionTester
	// Secrets 是机密项页读取来源；三种角色都必须提供。
	Secrets SecretSource
	// SecretsWriter 是机密项写操作来源。单机与权威端必须提供；客户端不提
	// 供，写路由不存在。
	SecretsWriter SecretWriter
	// Shares 是共享存储页读取来源；三种角色都必须提供。
	Shares ShareSource
	// SharesWriter 是共享存储写操作来源。单机与权威端必须提供；客户端
	// 不提供，写路由不存在。
	SharesWriter ShareWriter
	// SharesRemoteTasks 启动并读取共享存储现场探测与单台挂载任务。单机
	// 与权威端提供；客户端不提供，任务路由不存在。
	SharesRemoteTasks ShareRemoteTasks
	// ShareUnmountRunner 启动并读取共享存储单台卸载任务。单机与权威端提
	// 供；客户端不提供，卸载路由不存在。
	ShareUnmountRunner ShareUnmountRunner
	// ShareReselectRunner 启动并读取共享存储重选地址任务。单机与权威端
	// 提供；客户端不提供，重选路由不存在。
	ShareReselectRunner ShareReselectRunner
	// ShareResidueRunner 启动并读取共享存储残留检查任务。单机与权威端
	// 提供；客户端不提供，残留检查路由不存在。
	ShareResidueRunner ShareResidueRunner
	// SoftwareInventory 是软件清单页来源。单机与权威端提供；客户端不提
	// 供，导航与路由都不出现。
	SoftwareInventory SoftwareInventorySource
	// Topology 是网络拓扑页来源。三种角色都提供；初始化向导等没有拓扑
	// 数据的构造不提供，导航与路由都不出现。
	Topology TopologySource
	// TopologyWriter 是网络拓扑页写操作来源：布局保存与重置、拖动归类与
	// 网络成员编辑。单机与权威端提供；客户端不提供，写路由也不注册。
	TopologyWriter TopologyWriter
	// TopologyProbe 是网络拓扑页按需触发连通探测与读取本地探测结果的来
	// 源。三种角色都提供；客户端适配层固定本机起点。
	TopologyProbe TopologyProbe
	Access        AccessStore
	Ops           OpsStore
	// MachineInit 是单机升级为权威端时调用的初始化模块适配器。适配器内部
	// 调用 machineinit.Initialize 并把权威端作为目标角色；本包不写 role.json。
	MachineInit MachineInitializer
	// RequestShutdown 在升级成功页、客户端配对指引页或权威端完成页完整写
	// 给浏览器后触发，由 `sshmgr ui` 的服务循环注入，用于优雅关闭本次回环
	// Web UI 进程。参数说明触发关闭的页面，命令行适配器按它决定终端输出。
	RequestShutdown func(ShutdownReason)
	// InitializationOperatingSystem 与 InitializationExecutablePath 只用于
	// 权威端向导的安装页；命令行适配器注入当前操作系统与可执行文件绝对路径。
	InitializationOperatingSystem string
	InitializationExecutablePath  string
	// InitializationComplete 在权威端初始化完成页完整写给浏览器后触发；
	// 命令行适配器用它打印正式地址并注入优雅退出。
	InitializationComplete func(InitializationCompletion)
	// ServiceRestart 是权威端显式注入的服务重启能力；不注入时不注册重启
	// 路由，也不显示重启表单。
	ServiceRestart OpsRestartStore
	VersionHint    func() string
	Port           int
	// ServePort 是权威端主监听端口，设备码拉取命令按它拼地址；未提供时
	// 按默认端口兜底。单机与客户端不使用这个字段。
	ServePort int
	// ClientInstance 是客户端角色启动时选定的客户端缓存实例名；空串表示默认
	// 实例。浏览器会话以它为初始值，切换只保存在进程内会话里。
	ClientInstance string
	Now            func() time.Time
	// ReverifyWindow 是权威端敏感写操作要求重新验证管理员口令的窗口；
	// 零值使用默认 5 分钟。端到端浏览器测试可用环境开关把它缩短。
	ReverifyWindow         time.Duration
	Auth                   AuthStore
	CertificateFingerprint string
	AllowedHosts           []string
	Sleep                  func(time.Duration)
	Log                    func(format string, arguments ...any)
}

func (c Config) validate() error {
	if c.Role != RoleStandalone && c.Role != RoleClient && c.Role != RoleServer {
		return fmt.Errorf("未知的 Web UI 角色：%d", c.Role)
	}
	if c.Servers == nil {
		return errors.New("Web UI 必须提供服务器列表来源")
	}
	if c.Role != RoleClient && c.Access == nil {
		return errors.New("可写角色的 Web UI 必须提供访问管理来源")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("Web UI 监听端口非法：%d", c.Port)
	}
	if c.Role == RoleServer {
		if c.Auth == nil {
			return errors.New("权威端 Web UI 必须提供鉴权存储")
		}
		if len(c.AllowedHosts) == 0 {
			return errors.New("权威端 Web UI 必须提供允许的 Host 集合")
		}
	}
	return nil
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// versionHint 读取版本提示来源；空串表示不显示。
func (c Config) versionHint() string {
	if c.VersionHint == nil {
		return ""
	}
	return c.VersionHint()
}

// sortNeedsAttentionFirst 稳定地把需要注意的服务器排到最前，与终端图形
// 界面的排序口径一致。
func sortNeedsAttentionFirst(rows []ServerRow) []ServerRow {
	sorted := append([]ServerRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].NeedsAttention && !sorted[j].NeedsAttention
	})
	return sorted
}

// capabilities 是页面能力开关：模板按它决定显示哪些标记与入口。写操作页面
// 上线时扩展这个结构，不另造机制。
type capabilities struct {
	// RoleLabel 是页头展示的本机角色标记。
	RoleLabel string
	// CanWrite 表示本机角色是否可写 vault；客户端恒为 false。
	CanWrite bool
	// ReadOnlyNotice 非空时在页头下方显示只读指引；单机角色为空串。
	ReadOnlyNotice string
	// ShowAuthorityActions 控制权威端页头的会话与口令入口。
	ShowAuthorityActions bool
	// ShowAccessArea 控制是否显示「访问」区域入口。三种角色都有入口；
	// 客户端入口只显示权威端管理提示。
	ShowAccessArea bool
	// ShowSecretsArea 控制是否显示「机密项」区域入口；只有配置了机密项
	// 读取来源时才显示。
	ShowSecretsArea bool
	// ShowSoftwareInventoryArea 控制是否显示「软件清单」区域入口；只有
	// 单机或权威端配置软件清单来源时才显示。
	ShowSoftwareInventoryArea bool
	// ShowTopologyArea 控制是否显示「网络拓扑」区域入口；三种角色只要提
	// 供拓扑来源就显示，客户端入口同样可见但页面只读。
	ShowTopologyArea bool
	// CanEditTopology 控制拓扑页的拖动归类入口与网络成员编辑表单；只有
	// 提供拓扑写来源的角色为真。
	CanEditTopology bool
	// ShowTopologyProbe 控制拓扑页的探测起点选择与叠加入口；只有提供探测
	// 来源时显示。
	ShowTopologyProbe bool
	// CanChooseProbeServerOrigin 控制拓扑页能否选择受管服务器作为探测起
	// 点；客户端固定本机。
	CanChooseProbeServerOrigin bool
	// CanManageProfiles 控制是否显示 Profile 页入口与写操作。
	CanManageProfiles bool
	// CanManageProjects 控制是否显示项目令牌页入口与写操作。
	CanManageProjects bool
	// CanManageSecrets 控制机密项写操作；客户端只保留只读列表入口。
	CanManageSecrets bool
	// ShowSharesArea 控制是否显示「共享存储」区域入口；只有配置了共享存
	// 储读取来源时才显示。
	ShowSharesArea bool
	// CanManageShares 控制共享存储写操作；客户端只保留只读列表入口。
	CanManageShares bool
	// CanUnmountShares 控制共享存储详情页的单台卸载操作；只有提供卸载
	// 任务视图的可写角色可用。
	CanUnmountShares bool
	// CanReselectShares 控制共享存储详情页的重选地址操作；只有提供重选
	// 任务视图的可写角色可用。
	CanReselectShares bool
	// CanCheckShareResidue 控制共享存储详情页的残留检查；只有提供残留
	// 检查任务视图的可写角色可用。
	CanCheckShareResidue bool
	// CanUseShareDesktopGuide 控制共享存储详情页的桌面手动添加引导；只有
	// 单机与权威端且提供拓扑来源时可用。
	CanUseShareDesktopGuide bool
	// HasPairingAndDeviceCodes 表示本机角色是否显示配对与设备码入口。
	HasPairingAndDeviceCodes bool
	// ShowOpsArea 控制是否显示「运维」区域入口。
	ShowOpsArea bool
	// CanManageBackups 控制是否显示备份页入口与写操作。
	CanManageBackups bool
	// CanManageSettings 控制是否显示设置页入口与写操作。
	CanManageSettings bool
	// CanUpgradeBinary 控制是否显示升级页入口与替换二进制操作。
	CanUpgradeBinary bool
	// CanRestartService 只对权威端开放，控制升级后的重启服务操作。
	CanRestartService bool
	// CanQueryVaultAudit 控制单机与权威端的权威 vault 审计页。
	CanQueryVaultAudit bool
	// CanUseClientOps 控制客户端页（连接状态、立即同步与实例切换）。
	CanUseClientOps bool
	// CanQueryClientAudit 只对客户端开放，控制本机审计页。
	CanQueryClientAudit bool
	// CanUpgradeToServer 只对提供了初始化模块的单机开放，控制升级为权威
	// 端的入口与路由。
	CanUpgradeToServer bool
}

// capabilitiesForConfig 按构造参数推导页面能力开关：先按本机角色确定基础
// 能力，再按是否注入机密项读取来源决定机密项区域入口。
func capabilitiesForConfig(cfg Config) capabilities {
	caps := capabilitiesForRole(cfg.Role)
	caps.ShowSecretsArea = cfg.Secrets != nil
	caps.ShowSharesArea = cfg.Shares != nil
	caps.CanManageShares = cfg.Shares != nil && cfg.SharesWriter != nil && cfg.Role != RoleClient
	caps.CanUnmountShares = cfg.Shares != nil && cfg.ShareUnmountRunner != nil && cfg.Role != RoleClient
	caps.CanReselectShares = cfg.Shares != nil && cfg.ShareReselectRunner != nil && cfg.Role != RoleClient
	caps.CanCheckShareResidue = cfg.Shares != nil && cfg.ShareResidueRunner != nil && cfg.Role != RoleClient
	caps.CanUseShareDesktopGuide = cfg.Shares != nil && cfg.Topology != nil && cfg.Role != RoleClient
	caps.ShowSoftwareInventoryArea = cfg.Role != RoleClient && cfg.SoftwareInventory != nil
	caps.ShowTopologyArea = cfg.Topology != nil
	caps.CanEditTopology = cfg.Topology != nil && cfg.TopologyWriter != nil
	caps.ShowTopologyProbe = cfg.Topology != nil && cfg.TopologyProbe != nil
	caps.CanChooseProbeServerOrigin = caps.ShowTopologyProbe && cfg.Role != RoleClient
	return caps
}

// capabilitiesForRole 按构造时声明的本机角色推导页面能力开关。
func capabilitiesForRole(role Role) capabilities {
	if role == RoleClient {
		return capabilities{
			RoleLabel:         "客户端 · 只读",
			CanWrite:          false,
			ReadOnlyNotice:    "这里只读，修改需到权威端 Web UI 去做。",
			ShowAccessArea:    true,
			CanManageProfiles: false,
			CanManageSecrets:  false,
			ShowOpsArea:       true, CanUpgradeBinary: true, CanUseClientOps: true,
			CanQueryClientAudit: true,
		}
	}
	if role == RoleServer {
		return capabilities{
			RoleLabel: "权威端", CanWrite: true, ShowAuthorityActions: true,
			ShowAccessArea: true, CanManageProfiles: true, CanManageProjects: true,
			CanManageSecrets:         true,
			HasPairingAndDeviceCodes: true,
			ShowOpsArea:              true, CanManageBackups: true, CanManageSettings: true,
			CanUpgradeBinary: true, CanQueryVaultAudit: true,
		}
	}
	return capabilities{
		RoleLabel: "单机", CanWrite: true,
		ShowAccessArea: true, CanManageProfiles: true, CanManageProjects: true,
		CanManageSecrets: true,
		ShowOpsArea:      true, CanManageBackups: true, CanManageSettings: true,
		CanUpgradeBinary: true, CanQueryVaultAudit: true,
	}
}

// Startup 携带一次性启动令牌，并按监听地址拼出浏览器要打开的网址。
type Startup struct {
	token string
}

// Token 返回一次性启动令牌本身。
func (s Startup) Token() string { return s.token }

// URL 按传入的「主机:端口」拼出带令牌的启动网址。
func (s Startup) URL(hostPort string) string {
	return "http://" + hostPort + startPath + "?token=" + url.QueryEscape(s.token)
}

// session 是回环模式的进程内会话：标识与防跨站请求伪造令牌都只在内存里，
// 进程退出即全部失效。
type session struct {
	id             string
	csrf           string
	clientInstance string
	accessFlash    *AccessFlash
	opsBackupFlash *OpsBackupFlash
	serverAddDraft *serverAddDraft
	generation     int64
	lastActive     time.Time
	lastVerified   time.Time
}

// handler 是回环模式 Web UI 的根处理器，所有路由都在 /ui/ 前缀下。
type handler struct {
	cfg          Config
	caps         capabilities
	assets       assetURLs
	assetFiles   map[string]string
	pages        map[string]parsedPage
	startDigest  [sha256.Size]byte
	mu           sync.Mutex
	loginMu      sync.Mutex
	startUsed    bool
	session      *session
	sessions     map[string]*session
	allowedHosts map[string]struct{}
	mux          *http.ServeMux
}

const (
	startPath = "/ui/start"
	listPath  = "/ui/"
	loginPath = "/ui/login"

	// contentSecurityPolicy 只允许同源脚本、样式、连接、图片与字体，禁止
	// 内联脚本、内联事件属性、内联样式以及任何被嵌入、跳转的可能。
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
)

// New 构造 Web UI 的根处理器与一次性启动令牌。构造完成后令牌只在第一次
// 有效使用时换取会话，之后立即作废。
func New(cfg Config) (http.Handler, Startup, error) {
	if cfg.Role == RoleServer {
		return nil, Startup{}, errors.New("权威端 Web UI 必须使用 NewAuthority 构造")
	}
	registerRoutes := func(h *handler) {
		h.mux.HandleFunc(startPath, h.handleStart)
		h.mux.HandleFunc("/ui/assets/", h.handleAsset)
		h.mux.HandleFunc(listPath, h.requireSession(h.handleUIRoot))
		h.registerTopologyRoutes()
		h.registerAccessRoutes()
		h.registerServerRoutes()
		h.registerSecretRoutes()
		h.registerShareRoutes()
		h.registerSoftwareInventoryRoutes()
		h.registerOpsRoutes()
	}
	return newLoopbackHandler(cfg, registerRoutes)
}

// newLoopbackHandler 构造回环模式处理器。registerRoutes 让正式页面与初始
// 化向导复用同一套会话、Host、来源、防跨站请求伪造与安全头基础设施，同时
// 保持两者对外注册的路由不同。
func newLoopbackHandler(cfg Config, registerRoutes func(*handler)) (http.Handler, Startup, error) {
	if err := cfg.validate(); err != nil {
		return nil, Startup{}, err
	}
	token, err := randomToken()
	if err != nil {
		return nil, Startup{}, fmt.Errorf("生成启动令牌失败：%w", err)
	}
	assets, assetFiles, err := loadAssets()
	if err != nil {
		return nil, Startup{}, err
	}
	pages, err := loadPageTemplates()
	if err != nil {
		return nil, Startup{}, err
	}
	h := &handler{
		cfg:         cfg,
		caps:        capabilitiesForConfig(cfg),
		assets:      assets,
		assetFiles:  assetFiles,
		pages:       pages,
		startDigest: sha256.Sum256([]byte(token)),
		mux:         http.NewServeMux(),
	}
	registerRoutes(h)
	return h, Startup{token: token}, nil
}

// requireSession 是所有需要会话的页面的统一包装：未携带有效会话时，单机
// 与客户端回环模式显示会话错误页，权威端跳转登录页；启动入口、静态资源、
// 登录与注销等不需要会话的路径不使用这个包装。
func (h *handler) requireSession(page http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.currentSession(r) == nil {
			if isTopologyAPIRequest(r) {
				message := "本页需要有效会话。请重新登录。"
				if h.cfg.Role != RoleServer {
					message = "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。"
				}
				h.writeTopologyJSONError(w, http.StatusUnauthorized, message)
				return
			}
			if h.cfg.Role == RoleServer {
				http.Redirect(w, r, loginPath, http.StatusSeeOther)
				return
			}
			h.renderSessionRequired(w, r)
			return
		}
		page(w, r)
	}
}

// ServeHTTP 让 handler 直接作为 http.Server 的根处理器使用。所有请求（包括
// 启动入口与静态资源）先校验回环 Host，拒绝域名重绑定后再进入路由。
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 安全头对所有响应生效（静态资源随后覆盖缓存策略）；HTML 默认不缓存，
	// 静态资源在自身处理器里改成长期缓存。
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// same-origin 而不是 no-referrer：no-referrer 会让浏览器在同源表单提交
	// 时发送 Origin: null，写请求的来源校验会拒绝真实用户；same-origin 同
	// 样不向跨源目标透露引用地址。
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	if isSecretValueUpload(r) {
		r.Body = http.MaxBytesReader(w, r.Body, secretUploadMaxRequestBody)
	}
	if isAddFormPost(r) {
		r.Body = http.MaxBytesReader(w, r.Body, serverAddFormMaxRequestBody)
	}
	hostOK := hostAllowed(r.Host, h.cfg.Port)
	if h.cfg.Role == RoleServer {
		hostOK = h.authorityHostAllowed(r.Host)
	}
	if !hostOK {
		message := "Web UI 只允许从本机回环地址访问。请重新运行 `sshmgr ui`。"
		if h.cfg.Role == RoleServer {
			message = "Web UI 只允许从实际监听地址访问。请核对 `sshmgr serve admin status` 输出的地址。"
		}
		h.renderError(w, r, http.StatusMisdirectedRequest, "请求来源不被接受", message)
		return
	}
	if h.cfg.Role == RoleServer {
		if !h.authorityWriteAllowed(w, r) {
			return
		}
	} else if !h.writeRequestAllowed(w, r) {
		return
	}
	h.mux.ServeHTTP(w, r)
}

func isAddFormPost(r *http.Request) bool {
	return r.Method == http.MethodPost &&
		(r.URL.Path == serverAddFormPath || r.URL.Path == deviceAddFormPath)
}

// writeRequestAllowed 统一校验所有写请求：非 GET、HEAD、OPTIONS 都必须来自
// 本次监听的回环来源，并携带当前会话绑定的防跨站请求伪造令牌。
func (h *handler) writeRequestAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if !originAllowed(r.Header.Get("Origin"), h.cfg.Port) {
		h.writeRequestError(w, r, http.StatusForbidden, "请求来源校验失败", "请求来源不是本次启动的 Web UI，已拒绝。")
		return false
	}
	sess := h.currentSession(r)
	if sess == nil {
		if isTopologyAPIRequest(r) {
			h.writeTopologyJSONError(w, http.StatusUnauthorized, "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
		} else {
			h.renderSessionRequired(w, r)
		}
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		parseErr := parseFormForCSRF(r)
		if parseErr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(parseErr, &tooLarge) {
				if isAddFormPost(r) {
					if r.URL.Path == deviceAddFormPath {
						h.renderError(w, r, http.StatusRequestEntityTooLarge, "新增其他设备表单过大", "新增其他设备表单过大，已拒绝。")
					} else {
						h.renderError(w, r, http.StatusRequestEntityTooLarge, "新增服务器表单过大", "新增服务器表单过大，已拒绝。")
					}
					return false
				}
				h.renderError(w, r, http.StatusRequestEntityTooLarge, "上传内容过大", "机密项值上传的请求体超过限制，已拒绝。")
				return false
			}
		}
		token = r.PostFormValue("csrf_token")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(sess.csrf)) != 1 {
		h.writeRequestError(w, r, http.StatusForbidden, "防跨站请求伪造校验失败", "防跨站请求伪造令牌缺失或无效，已拒绝。请刷新页面后重试。")
		return false
	}
	return true
}

// isTopologyAPIRequest 判断请求是否访问拓扑图页面的内部 JSON 接口。
func isTopologyAPIRequest(r *http.Request) bool {
	return r != nil && (r.URL.Path == topologyAPIPath || strings.HasPrefix(r.URL.Path, topologyAPIPath+"/"))
}

// writeRequestError 给普通页面渲染错误页；拓扑内部 JSON 接口始终得到 JSON
// 错误体，浏览器脚本才能显示其中的中文提示。
func (h *handler) writeRequestError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	if isTopologyAPIRequest(r) {
		h.writeTopologyJSONError(w, status, message)
		return
	}
	h.renderError(w, r, status, title, message)
}

// logError 记录处理器没有透出的底层错误，方便排查服务器侧故障。
func (h *handler) logError(r *http.Request, context string, err error) {
	if h.cfg.Log == nil || err == nil {
		return
	}
	h.cfg.Log("sshmgr web ui: %s method=%s path=%s remote=%s: %v", context, r.Method, r.URL.Path, remoteIP(r), err)
}

// parseFormForCSRF 解析请求体以便读取表单里的防跨站请求伪造令牌。多部分
// 表单的内存阈值跟随请求自己的上限，保证文件部分不落临时文件；非多部分
// 表单沿用标准解析。
func parseFormForCSRF(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		maxMemory := int64(secretUploadMaxRequestBody)
		if isAddFormPost(r) {
			maxMemory = serverAddFormMaxRequestBody
		}
		return r.ParseMultipartForm(maxMemory)
	}
	return r.ParseForm()
}

// hostAllowed 判断 Host 是否为本次监听端口对应的回环地址：127.0.0.1、
// localhost 或 [::1]，端口必须与构造时声明的监听端口一致。
func hostAllowed(host string, port int) bool {
	name, hostPort, err := net.SplitHostPort(host)
	if err != nil || hostPort != strconv.Itoa(port) {
		return false
	}
	if name == "127.0.0.1" || name == "::1" || strings.EqualFold(name, "localhost") {
		return true
	}
	return false
}

// handleStart 是启动入口：有效令牌首次使用即作废、签发会话并 303 重定向到
// 不带令牌的服务器列表页；令牌缺失、错误或已用都返回 403 中文错误页。
func (h *handler) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "只允许 GET 访问启动入口", http.StatusMethodNotAllowed)
		return
	}
	digest := sha256.Sum256([]byte(r.URL.Query().Get("token")))
	sess, err := newSession()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "无法建立会话", "生成会话标识失败，请重新运行 `sshmgr ui`。")
		return
	}
	h.mu.Lock()
	valid := !h.startUsed && subtle.ConstantTimeCompare(digest[:], h.startDigest[:]) == 1
	if valid {
		h.startUsed = true
		sess.clientInstance = h.cfg.ClientInstance
		h.session = sess
	}
	h.mu.Unlock()
	if !valid {
		h.renderError(w, r, http.StatusForbidden, "启动链接无效", "启动链接无效或已使用。请关闭本页，重新运行 `sshmgr ui` 获取新的启动链接。")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(h.cfg.Port),
		Value:    sess.id,
		Path:     "/ui/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, listPath, http.StatusSeeOther)
}

// handleUIRoot 是 /ui/ 前缀的总入口：精确命中服务器列表页时要求有效会话，
// 其余地址按会话状态返回 401 或中文 404 页。
func (h *handler) handleUIRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != listPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	h.handleServerList(w, r)
}

// requestClientInstance 返回当前浏览器会话选中的客户端实例；没有会话时退回
// `sshmgr ui` 启动时解析的实例。非客户端角色不使用这个值。
func (h *handler) requestClientInstance(r *http.Request) string {
	if h.cfg.Role != RoleClient {
		return ""
	}
	if sess := h.currentSession(r); sess != nil {
		h.mu.Lock()
		instance := sess.clientInstance
		h.mu.Unlock()
		return instance
	}
	return h.cfg.ClientInstance
}

// setSessionClientInstance 更新当前浏览器会话中的客户端实例。
func (h *handler) setSessionClientInstance(r *http.Request, instance string) {
	if sess := h.currentSession(r); sess != nil {
		h.mu.Lock()
		sess.clientInstance = instance
		h.mu.Unlock()
	}
}

// renderSessionRequired 输出需要重新启动 Web UI 的会话错误页。
func (h *handler) renderSessionRequired(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusUnauthorized, "需要启动会话", "本页需要有效会话。请重新运行 `sshmgr ui` 获取新的启动链接。")
}

// currentSession 校验请求携带的会话 cookie，返回当前会话；无会话、cookie
// 缺失或值不符都返回 nil。
func (h *handler) currentSession(r *http.Request) *session {
	if h.cfg.Role == RoleServer {
		return h.currentAuthoritySession(r)
	}
	c, err := r.Cookie(sessionCookieName(h.cfg.Port))
	if err != nil || c.Value == "" {
		return nil
	}
	h.mu.Lock()
	sess := h.session
	h.mu.Unlock()
	if sess == nil {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(sess.id)) != 1 {
		return nil
	}
	return sess
}

// adoptLoopbackSession 让另一个回环处理器复用当前会话。两个处理器的监听
// 端口一致，cookie 名与防跨站请求伪造令牌不需要变化。
func (h *handler) adoptLoopbackSession(sess *session) {
	h.mu.Lock()
	h.session = sess
	h.mu.Unlock()
}

// sessionCookieName 带上监听端口：浏览器的 cookie 不按端口隔离，同一台
// 机器同时开两个 Web UI 实例时才能互不覆盖。
func sessionCookieName(port int) string {
	return fmt.Sprintf("sshmgr_ui_%d", port)
}

func newSession() (*session, error) {
	id, err := generateSessionToken()
	if err != nil {
		return nil, err
	}
	csrf, err := generateSessionToken()
	if err != nil {
		return nil, err
	}
	return &session{id: id, csrf: csrf}, nil
}

// generateSessionToken 是会话随机数生成接缝，测试用它模拟系统随机数失败。
var generateSessionToken = randomToken

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// originAllowed 校验写请求的 Origin：必须存在，且等于由配置端口生成的任一
// 回环来源，不信任请求自带的 Host。
func originAllowed(origin string, port int) bool {
	portText := strconv.Itoa(port)
	return origin == "http://"+net.JoinHostPort("127.0.0.1", portText) ||
		origin == "http://"+net.JoinHostPort("localhost", portText) ||
		origin == "http://"+net.JoinHostPort("::1", portText)
}
