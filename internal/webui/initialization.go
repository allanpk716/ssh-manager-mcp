package webui

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

// Initializer 是初始化向导改动本机状态的唯一入口。生产实现由命令行适配
// machineinit 包；测试使用内存替身。
type Initializer interface {
	// ReadSnapshot 读取向导入口需要的状态。
	ReadSnapshot() (InitializationSnapshot, error)
	// SelectRoleWithoutVault 只写入所选角色，不创建 vault。
	SelectRoleWithoutVault(role InitializationRole) error
	// Initialize 创建 vault 并把所选角色标记为完成初始化。
	Initialize(role InitializationRole) (InitializationResult, error)
}

// initializationWizard 先只服务向导路由；单机初始化完成后，把同一个进程内
// 会话交给单机正式页面处理器，浏览器不需要重新获取启动令牌。
type initializationWizard struct {
	wizard      *handler
	formal      *handler
	initializer Initializer
	completed   atomic.Bool
	entryRole   InitializationRole
	entryStep   InitializationStep
}

// initializationClientShutdownDelay 给浏览器留出接收完整指引页的时间；页面
// 先带 Content-Length 写出并刷新，随后才触发优雅关闭。
const initializationClientShutdownDelay = 500 * time.Millisecond

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
	if snapshot.Step == InitializationStepCreateVault && snapshot.Role != InitializationRoleStandalone {
		return nil, Startup{}, errors.New("只有单机初始化支持从「创建 vault」续做")
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
		h.mux.HandleFunc("/ui/init/server", h.requireSession(destination.handleServerPlaceholder))
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
		http.Redirect(response, request, "/ui/init/server", http.StatusSeeOther)
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

func (w *initializationWizard) handleServerPlaceholder(response http.ResponseWriter, request *http.Request) {
	w.render(response, request, http.StatusOK, "initialization-placeholder.tmpl", pageData{
		Title: "权威端初始化", InitializationRoleLabel: "权威端",
	})
}

func (w *initializationWizard) handleClientPairingGuide(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		w.wizard.renderError(response, request, http.StatusMethodNotAllowed, "方法不允许", "客户端配对指引只接受 GET 请求。")
		return
	}
	if !w.writeCompleteClientPairingPage(response, request) {
		return
	}
	if w.wizard.cfg.RequestShutdown != nil {
		shutdown := w.wizard.cfg.RequestShutdown
		go func() {
			if w.wizard.cfg.Sleep != nil {
				w.wizard.cfg.Sleep(initializationClientShutdownDelay)
			} else {
				time.Sleep(initializationClientShutdownDelay)
			}
			shutdown()
		}()
	}
}

func (w *initializationWizard) writeCompleteClientPairingPage(response http.ResponseWriter, request *http.Request) bool {
	data := pageData{
		Title: "客户端入网", Assets: w.wizard.assets, Caps: w.wizard.caps,
		VersionHint:                    w.wizard.cfg.versionHint(),
		InitializationClientGuideLines: ClientInitializationGuideLines(),
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
