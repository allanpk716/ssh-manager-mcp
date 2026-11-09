package webui

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// InitializationRole 是初始化向导里的本机角色取值。Web UI 包不导入角色解析
// 包，避免 vault 适配层与页面层形成导入环；命令行适配器负责与 role.json 的
// 取值互相转换。
type InitializationRole string

const (
	// InitializationRoleStandalone 表示单机：一台机器自持 vault。
	InitializationRoleStandalone InitializationRole = "standalone"
	// InitializationRoleServer 表示权威端：多机部署里常驻权威 vault 的机器。
	InitializationRoleServer InitializationRole = "server"
	// InitializationRoleClient 表示客户端：只持本地只读快照的机器。
	InitializationRoleClient InitializationRole = "client"
)

// InitializationStep 是向导构造时读到的入口步骤。
type InitializationStep int

const (
	// InitializationStepChooseRole 从「选择本机角色」开始。
	InitializationStepChooseRole InitializationStep = iota
	// InitializationStepCreateVault 从「创建 vault」续做。
	InitializationStepCreateVault
	// InitializationStepSetAdminPassword 从「设置管理员口令」续做。
	InitializationStepSetAdminPassword
	// InitializationStepInstallServe 从「安装 serve」续做。
	InitializationStepInstallServe
)

// InitializationSnapshot 是向导需要的本机状态快照。
type InitializationSnapshot struct {
	Role InitializationRole
	Step InitializationStep
}

// InitializationResult 是初始化操作返回给页面的结果。
type InitializationResult struct {
	// MasterKeyPath 是随机主密钥文件的位置。
	MasterKeyPath string
}

// ClientInitializationGuideLines 返回客户端入网指引。页面与 `sshmgr ui`
// 的终端输出共用同一组文字，保证两边给 owner 的下一步完全一致。
func ClientInitializationGuideLines() []string {
	return []string{
		"sshmgr pair --instance <名字>",
		"--url <权威端地址>：不使用自动发现，直接指定权威端地址。",
		"--pin <证书公钥指纹>：校验权威端证书，防止连接被冒充。",
		"--write-mcp <文件路径>：把配对生成的 MCP 配置文件另存到指定路径。",
		"配对发起后，到权威端 Web UI 的配对页批准；本页不执行配对，也不会写入任何文件。",
	}
}

// InitializationWriteSource 描述初始化向导写操作的来源地址。
type InitializationWriteSource struct {
	RemoteAddress string
}

// ServeWebUIProbe 是 serve 独立 Web UI 的运行探测结果。
type ServeWebUIProbe struct {
	Known             bool
	Port              int
	Addresses         []string
	NeedsManualListen bool
	Running           bool
}

// InitializationCompletion 是权威端向导完成时交给命令行适配器的地址结果。
type InitializationCompletion struct {
	Port      int
	Addresses []string
}

// ShutdownReason 说明触发优雅关闭的页面。命令行适配器按它决定退出前要打印
// 的终端内容，不再反推本机状态。
type ShutdownReason int

const (
	// ShutdownReasonClientGuide 表示客户端入网指引页已完整返回。
	ShutdownReasonClientGuide ShutdownReason = iota
	// ShutdownReasonServerFinish 表示权威端初始化完成页已完整返回。
	ShutdownReasonServerFinish
	// ShutdownReasonStandaloneUpgrade 表示单机升级为权威端的成功页已完整返回。
	ShutdownReasonStandaloneUpgrade
)

// InitializationServeAddressView 是完成页里的一条正式 Web UI 地址。
type InitializationServeAddressView struct {
	URL       string
	LocalOnly bool
}

// InitializationStepView 是步骤条中的一步。
type InitializationStepView struct {
	Label   string
	Current bool
	Done    bool
}

// Initializer 是初始化向导改动本机状态的唯一入口。生产实现由命令行适配
// machineinit 包；测试使用内存替身。
type Initializer interface {
	// ReadSnapshot 读取向导入口需要的状态。
	ReadSnapshot() (InitializationSnapshot, error)
	// SelectRoleWithoutVault 只写入所选角色，不创建 vault。
	SelectRoleWithoutVault(role InitializationRole) error
	// Initialize 创建 vault 并把所选角色标记为完成初始化。
	Initialize(role InitializationRole) (InitializationResult, error)
	// ValidateAdminPassword 使用与命令行一致的管理员口令规则做提交前校验。
	ValidateAdminPassword(password, confirmation []byte) error
	// SetAdminPassword 设置权威端 Web UI 管理员口令。
	SetAdminPassword(password, confirmation []byte, source InitializationWriteSource) (int64, error)
	// ProbeServeWebUI 重新探测 serve 的独立 Web UI 是否已在运行。
	ProbeServeWebUI() (ServeWebUIProbe, error)
}

// initializationWizard 先只服务向导路由；单机初始化完成后，把同一个进程内
// 会话交给单机正式页面处理器，浏览器不需要重新获取启动令牌。
type initializationWizard struct {
	wizard            *handler
	formal            *handler
	initializer       Initializer
	completed         atomic.Bool
	shutdownScheduled atomic.Bool
	entryRole         InitializationRole
	entryStep         InitializationStep
}

// initializationClientShutdownDelay 给浏览器留出接收完整指引页的时间；页面
// 先带 Content-Length 写出并刷新，随后才触发优雅关闭。
const initializationClientShutdownDelay = 500 * time.Millisecond

// initializationServerShutdownDelay 给浏览器留出接收权威端完成页的时间；
// 页面先带 Content-Length 写出并刷新，随后才触发优雅关闭。
const initializationServerShutdownDelay = 500 * time.Millisecond

// NewInitializationWizard 构造回环初始化向导。cfg 是初始化完成后要切换到的
// 单机正式页面配置；在切换前，路由表里只有向导页面。
func NewInitializationWizard(cfg Config, initializer Initializer) (http.Handler, Startup, error) {
	if cfg.Role != RoleStandalone {
		return nil, Startup{}, errors.New("初始化向导当前只支持切换到单机正式页面")
	}
	if initializer == nil {
		return nil, Startup{}, errors.New("初始化向导必须提供初始化接口")
	}
	snapshot, err := initializer.ReadSnapshot()
	if err != nil {
		return nil, Startup{}, err
	}
	if snapshot.Step == InitializationStepCreateVault &&
		snapshot.Role != InitializationRoleStandalone && snapshot.Role != InitializationRoleServer {
		return nil, Startup{}, errors.New("只有单机或权威端初始化支持从「创建 vault」续做")
	}
	if (snapshot.Step == InitializationStepSetAdminPassword || snapshot.Step == InitializationStepInstallServe) &&
		snapshot.Role != InitializationRoleServer {
		return nil, Startup{}, errors.New("只有权威端初始化包含设置管理员口令与安装 serve 步骤")
	}

	formalHandler, _, err := New(cfg)
	if err != nil {
		return nil, Startup{}, err
	}
	destination := &initializationWizard{}
	wizardHandler, startup, err := newLoopbackHandler(cfg, func(h *handler) {
		h.mux.HandleFunc(startPath, h.handleStart)
		h.mux.HandleFunc("/ui/assets/", h.handleAsset)
		h.mux.HandleFunc(listPath, h.requireSession(destination.handleRoot))
		h.mux.HandleFunc("/ui/init/role", h.requireSession(destination.handleRoleChoice))
		h.mux.HandleFunc("/ui/init/standalone", h.requireSession(destination.handleStandaloneConfirmation))
		h.mux.HandleFunc("/ui/init/standalone/create", h.requireSession(destination.handleStandaloneCreate))
		h.mux.HandleFunc("/ui/init/standalone/finish", h.requireSession(destination.handleStandaloneFinish))
		h.mux.HandleFunc("/ui/init/server", h.requireSession(destination.handleServerEntry))
		h.mux.HandleFunc("/ui/init/server/vault", h.requireSession(destination.handleServerVault))
		h.mux.HandleFunc("/ui/init/server/vault/create", h.requireSession(destination.handleServerVaultCreate))
		h.mux.HandleFunc("/ui/init/server/password", h.requireSession(destination.handleServerPassword))
		h.mux.HandleFunc("/ui/init/server/password/set", h.requireSession(destination.handleServerPasswordSet))
		h.mux.HandleFunc("/ui/init/server/install", h.requireSession(destination.handleServerInstall))
		h.mux.HandleFunc("/ui/init/server/probe", h.requireSession(destination.handleServerProbe))
		h.mux.HandleFunc("/ui/init/server/finish", h.requireSession(destination.handleServerFinish))
		h.mux.HandleFunc("/ui/init/client", h.requireSession(destination.handleClientPairingGuide))
	})
	if err != nil {
		return nil, Startup{}, err
	}
	wizard := wizardHandler.(*handler)
	wizard.caps = capabilities{RoleLabel: "初始化向导"}
	destination.wizard = wizard
	destination.formal = formalHandler.(*handler)
	destination.initializer = initializer
	destination.entryRole = snapshot.Role
	destination.entryStep = snapshot.Step
	return destination, startup, nil
}

func (w *initializationWizard) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if w.completed.Load() {
		w.formal.ServeHTTP(response, request)
		return
	}
	w.wizard.ServeHTTP(response, request)
}

func (w *initializationWizard) render(response http.ResponseWriter, request *http.Request, status int, page string, data pageData) {
	w.wizard.renderPage(response, request, status, page, data)
}

func (w *initializationWizard) handleRoot(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path != listPath {
		w.wizard.renderError(response, request, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if w.entryRole == InitializationRoleServer && w.entryStep != InitializationStepChooseRole {
		http.Redirect(response, request, w.serverEntryPath(), http.StatusSeeOther)
		return
	}
	if w.entryStep == InitializationStepCreateVault {
		http.Redirect(response, request, "/ui/init/standalone", http.StatusSeeOther)
		return
	}
	w.render(response, request, http.StatusOK, "initialization-role-choice.tmpl", pageData{Title: "选择本机角色"})
}

func (w *initializationWizard) handleRoleChoice(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "选择角色只接受提交表单。")
		return
	}
	data := pageData{Title: "选择本机角色"}
	role := InitializationRole(request.PostFormValue("role"))
	switch role {
	case InitializationRoleStandalone:
		if err := w.initializer.SelectRoleWithoutVault(role); err != nil {
			data.InitializationError = initializationErrorWithAdvice(err)
			w.render(response, request, http.StatusBadRequest, "initialization-role-choice.tmpl", data)
			return
		}
		http.Redirect(response, request, "/ui/init/standalone", http.StatusSeeOther)
	case InitializationRoleServer:
		if err := w.initializer.SelectRoleWithoutVault(role); err != nil {
			data.InitializationError = initializationErrorWithAdvice(err)
			w.render(response, request, http.StatusBadRequest, "initialization-role-choice.tmpl", data)
			return
		}
		http.Redirect(response, request, "/ui/init/server/vault", http.StatusSeeOther)
	case InitializationRoleClient:
		http.Redirect(response, request, "/ui/init/client", http.StatusSeeOther)
	default:
		data.InitializationError = "请选择单机、权威端或客户端中的一个角色。"
		w.render(response, request, http.StatusBadRequest, "initialization-role-choice.tmpl", data)
	}
}

func (w *initializationWizard) handleStandaloneConfirmation(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "确认页只接受 GET 请求。")
		return
	}
	w.render(response, request, http.StatusOK, "initialization-standalone.tmpl", pageData{Title: "初始化单机"})
}

func (w *initializationWizard) handleStandaloneCreate(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "创建 vault 只接受提交表单。")
		return
	}
	result, err := w.initializer.Initialize(InitializationRoleStandalone)
	if err != nil {
		w.render(response, request, http.StatusInternalServerError, "initialization-standalone.tmpl", pageData{
			Title: "初始化单机", InitializationError: initializationErrorWithAdvice(err),
		})
		return
	}
	w.render(response, request, http.StatusOK, "initialization-standalone-complete.tmpl", pageData{
		Title: "vault 已创建", InitializationMasterKeyPath: result.MasterKeyPath,
	})
}

func (w *initializationWizard) handleStandaloneFinish(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "进入正式页面只接受提交表单。")
		return
	}
	session := w.wizard.currentSession(request)
	if session == nil {
		w.wizard.renderSessionRequired(response, request)
		return
	}
	w.formal.adoptLoopbackSession(session)
	w.completed.Store(true)
	http.Redirect(response, request, listPath, http.StatusSeeOther)
}

func (w *initializationWizard) handleServerEntry(response http.ResponseWriter, request *http.Request) {
	http.Redirect(response, request, "/ui/init/server/vault", http.StatusSeeOther)
}

func (w *initializationWizard) serverEntryPath() string {
	switch w.entryStep {
	case InitializationStepSetAdminPassword:
		return "/ui/init/server/password"
	case InitializationStepInstallServe:
		return "/ui/init/server/install"
	default:
		return "/ui/init/server/vault"
	}
}

func (w *initializationWizard) handleServerVault(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "建 vault 步骤只接受 GET 请求。")
		return
	}
	w.render(response, request, http.StatusOK, "initialization-server-vault.tmpl", w.serverPageData("建 vault"))
}

func (w *initializationWizard) handleServerVaultCreate(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "创建 vault 只接受提交表单。")
		return
	}
	running, err := w.serveRunning()
	if err != nil {
		data := w.serverPageData("建 vault")
		data.InitializationError = initializationErrorWithAdvice(err)
		w.render(response, request, http.StatusInternalServerError, "initialization-server-vault.tmpl", data)
		return
	}
	if running {
		w.renderServeAlreadyRunning(response, request, "initialization-server-vault.tmpl", "建 vault")
		return
	}
	if _, err := w.initializer.Initialize(InitializationRoleServer); err != nil {
		data := w.serverPageData("建 vault")
		data.InitializationError = initializationErrorWithAdvice(err)
		w.render(response, request, http.StatusInternalServerError, "initialization-server-vault.tmpl", data)
		return
	}
	http.Redirect(response, request, "/ui/init/server/password", http.StatusSeeOther)
}

func (w *initializationWizard) handleServerPassword(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "设置管理员口令只接受提交表单。")
		return
	}
	w.render(response, request, http.StatusOK, "initialization-server-password.tmpl", w.serverPageData("设置管理员口令"))
}

func (w *initializationWizard) handleServerPasswordSet(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "设置管理员口令只接受提交表单。")
		return
	}
	password := []byte(request.PostFormValue("password"))
	confirmation := []byte(request.PostFormValue("password_confirm"))
	data := w.serverPageData("设置管理员口令")
	running, err := w.serveRunning()
	if err != nil {
		data.InitializationError = initializationErrorWithAdvice(err)
		w.render(response, request, http.StatusInternalServerError, "initialization-server-password.tmpl", data)
		return
	}
	if running {
		w.renderServeAlreadyRunning(response, request, "initialization-server-password.tmpl", "设置管理员口令")
		return
	}
	if err := w.initializer.ValidateAdminPassword(password, confirmation); err != nil {
		data.InitializationError = err.Error()
		w.render(response, request, http.StatusBadRequest, "initialization-server-password.tmpl", data)
		return
	}
	if _, err := w.initializer.SetAdminPassword(password, confirmation, InitializationWriteSource{
		RemoteAddress: remoteIP(request),
	}); err != nil {
		data.InitializationError = initializationErrorWithAdvice(err)
		w.render(response, request, http.StatusInternalServerError, "initialization-server-password.tmpl", data)
		return
	}
	http.Redirect(response, request, "/ui/init/server/install", http.StatusSeeOther)
}

func (w *initializationWizard) serveRunning() (bool, error) {
	probe, err := w.initializer.ProbeServeWebUI()
	if err != nil {
		return false, err
	}
	return probe.Running, nil
}

func (w *initializationWizard) renderServeAlreadyRunning(response http.ResponseWriter, request *http.Request, page, title string) {
	data := w.serverPageData(title)
	data.InitializationError = "serve 已在运行，请到正式 Web UI 操作，或用 `sshmgr serve admin set`。"
	w.render(response, request, http.StatusConflict, page, data)
}

func (w *initializationWizard) handleServerInstall(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "安装 serve 步骤只接受 GET 请求。")
		return
	}
	w.render(response, request, http.StatusOK, "initialization-server-install.tmpl", w.serverPageData("安装 serve"))
}

func (w *initializationWizard) serverPageData(title string) pageData {
	data := pageData{
		Title: title,
		InitializationSteps: []InitializationStepView{
			{Label: "建 vault", Current: title == "建 vault", Done: title != "建 vault"},
			{Label: "设置管理员口令", Current: title == "设置管理员口令", Done: title == "安装 serve"},
			{Label: "安装 serve", Current: title == "安装 serve"},
		},
	}
	if title == "安装 serve" {
		notice, command := initializationInstallCommand(
			w.wizard.cfg.InitializationOperatingSystem,
			w.wizard.cfg.InitializationExecutablePath,
		)
		data.InitializationInstallNotice = notice
		data.InitializationInstallCommand = command
	}
	return data
}

func initializationInstallCommand(operatingSystem, executablePath string) (string, string) {
	if operatingSystem == "" {
		operatingSystem = runtime.GOOS
	}
	if executablePath == "" {
		if resolved, err := os.Executable(); err == nil {
			executablePath = resolved
		} else {
			executablePath = "sshmgr"
		}
	}
	if operatingSystem == "windows" {
		quotedPath := strings.ReplaceAll(executablePath, `"`, `\"`)
		return "在管理员 PowerShell 中运行", `& "` + quotedPath + `" serve install --addr 0.0.0.0:7878`
	}
	quotedPath := "'" + strings.ReplaceAll(executablePath, "'", `'\''`) + "'"
	return "需要管理员权限", "sudo " + quotedPath + " serve install --addr 0.0.0.0:7878"
}

func (w *initializationWizard) handleServerProbe(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "serve 探测只接受 GET 请求。")
		return
	}
	probe, err := w.initializer.ProbeServeWebUI()
	if err != nil {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !probe.Running {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Header.Get("HX-Request") == "true" {
		response.Header().Set("HX-Redirect", "/ui/init/server/finish")
		response.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(response, request, "/ui/init/server/finish", http.StatusSeeOther)
}

func (w *initializationWizard) handleServerFinish(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "完成页只接受 GET 请求。")
		return
	}
	probe, err := w.initializer.ProbeServeWebUI()
	if err != nil {
		w.wizard.renderError(response, request, http.StatusServiceUnavailable, "探测 serve 失败", "探测 serve 的独立 Web UI 失败，请稍后重试。")
		return
	}
	if !probe.Running {
		http.Redirect(response, request, "/ui/init/server/install", http.StatusSeeOther)
		return
	}
	addresses := orderedServeAddresses(probe.Addresses)
	data := w.serverPageData("初始化完成")
	for _, address := range addresses {
		data.InitializationServeAddresses = append(data.InitializationServeAddresses, InitializationServeAddressView{
			URL:       "https://" + net.JoinHostPort(address, strconv.Itoa(probe.Port)) + "/ui/",
			LocalOnly: net.ParseIP(address).IsLoopback(),
		})
	}
	data.InitializationNeedsManualListen = probe.NeedsManualListen
	if !w.writeCompleteServerFinishPage(response, request, data) {
		return
	}
	if request.Method != http.MethodGet {
		return
	}
	if w.wizard.cfg.InitializationComplete == nil && w.wizard.cfg.RequestShutdown == nil {
		return
	}
	w.scheduleShutdown(func() {
		w.sleep(initializationServerShutdownDelay)
		if complete := w.wizard.cfg.InitializationComplete; complete != nil {
			complete(InitializationCompletion{Port: probe.Port, Addresses: addresses})
		}
		if shutdown := w.wizard.cfg.RequestShutdown; shutdown != nil {
			shutdown(ShutdownReasonServerFinish)
		}
	})
}

// scheduleShutdown 只排程一次优雅关闭：完成页或指引页重复刷新时，第一个
// 排程仍然有效，后续调用不再启动新的协程。
func (w *initializationWizard) scheduleShutdown(action func()) {
	if w.shutdownScheduled.CompareAndSwap(false, true) {
		go action()
	}
}

// sleep 经配置注入的睡眠函数等待，未注入时使用标准库；测试注入替身避免真实
// 等待退出延迟。
func (w *initializationWizard) sleep(duration time.Duration) {
	if w.wizard.cfg.Sleep != nil {
		w.wizard.cfg.Sleep(duration)
		return
	}
	time.Sleep(duration)
}

func (w *initializationWizard) writeCompleteServerFinishPage(response http.ResponseWriter, request *http.Request, data pageData) bool {
	data.Title = "权威端初始化完成"
	data.Assets = w.wizard.assets
	data.Caps = w.wizard.caps
	if session := w.wizard.currentSession(request); session != nil {
		data.CSRFToken = session.csrf
		data.Authenticated = true
	}
	parsed := w.wizard.pages["initialization-server-finish.tmpl"]
	if parsed.full == nil {
		w.wizard.renderError(response, request, http.StatusInternalServerError, "完成页渲染失败", "页面模板加载失败。")
		return false
	}
	var body bytes.Buffer
	if err := parsed.full.ExecuteTemplate(&body, "layout", data); err != nil {
		w.wizard.renderError(response, request, http.StatusInternalServerError, "完成页渲染失败", "页面渲染失败。")
		return false
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	response.WriteHeader(http.StatusOK)
	if _, err := response.Write(body.Bytes()); err != nil {
		return false
	}
	if flusher, ok := response.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func orderedServeAddresses(values []string) []string {
	out := make([]string, 0, len(values))
	var loopback []string
	for _, value := range values {
		if ip := net.ParseIP(value); ip != nil && ip.IsLoopback() {
			loopback = append(loopback, value)
			continue
		}
		out = append(out, value)
	}
	return append(out, loopback...)
}

func (w *initializationWizard) handleClientPairingGuide(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "客户端配对指引只接受 GET 请求。")
		return
	}
	if !w.writeCompleteClientPairingPage(response, request) {
		return
	}
	if request.Method != http.MethodGet {
		return
	}
	if w.wizard.cfg.RequestShutdown != nil {
		shutdown := w.wizard.cfg.RequestShutdown
		w.scheduleShutdown(func() {
			w.sleep(initializationClientShutdownDelay)
			shutdown(ShutdownReasonClientGuide)
		})
	}
}

func (w *initializationWizard) writeCompleteClientPairingPage(response http.ResponseWriter, request *http.Request) bool {
	data := pageData{
		Title: "客户端入网", Assets: w.wizard.assets, Caps: w.wizard.caps,
		VersionHint:      w.wizard.cfg.versionHint(),
		ClientGuideLines: ClientInitializationGuideLines(),
	}
	if session := w.wizard.currentSession(request); session != nil {
		data.CSRFToken = session.csrf
		data.Authenticated = true
	}
	parsed := w.wizard.pages["initialization-client.tmpl"]
	if parsed.full == nil {
		w.wizard.renderError(response, request, http.StatusInternalServerError, "客户端入网失败", "页面模板加载失败。")
		return false
	}
	var body bytes.Buffer
	if err := parsed.full.ExecuteTemplate(&body, "layout", data); err != nil {
		w.wizard.renderError(response, request, http.StatusInternalServerError, "客户端入网失败", "页面渲染失败。")
		return false
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	response.WriteHeader(http.StatusOK)
	if _, err := response.Write(body.Bytes()); err != nil {
		return false
	}
	if flusher, ok := response.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func initializationErrorWithAdvice(err error) string {
	return fmt.Sprintf("%v。可运行 `sshmgr doctor` 检查本机 vault 与密钥文件；确认要重试时回到上一页。", err)
}
