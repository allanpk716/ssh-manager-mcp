package mcpserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/secretmanage"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiaccess"
	"ssh-manager-mcp/internal/webuiops"
	"ssh-manager-mcp/internal/webuisecrets"
	"ssh-manager-mcp/internal/webuiservers"
	"ssh-manager-mcp/internal/webuishares"
	"ssh-manager-mcp/internal/webuisoftwareinventory"
	"ssh-manager-mcp/internal/webuitopology"
)

// serveWebUIServerSource 直接读取权威端 vault 的服务器列表。
type serveWebUIServerSource struct {
	store *store.Store
}

func (s serveWebUIServerSource) Servers(string) ([]webui.ServerRow, error) {
	servers, err := s.store.ListServers()
	if err != nil {
		return nil, err
	}
	rows := make([]webui.ServerRow, 0, len(servers))
	for _, srv := range servers {
		rows = append(rows, webui.ServerRow{
			ID: srv.ID, Name: srv.Name, User: srv.User, Host: srv.Host, Port: srv.Port,
			NeedsAttention: models.ServerNeedsAttention(srv.CredentialID, srv.Role, srv.Tags),
		})
	}
	return rows, nil
}

// ServerList 返回带注意原因的服务器列表，供权威端服务器页过滤视图使用；
// 权威端直接读取本机 vault，忽略客户端实例名。
func (s serveWebUIServerSource) ServerList(_ string, attentionOnly bool) ([]webui.ServerListEntry, error) {
	return webuiservers.ListEntries(servermanage.New(s.store), attentionOnly)
}

// ServerDetail 返回一台服务器的详情；权威端忽略客户端实例名。
func (s serveWebUIServerSource) ServerDetail(_ string, identifier string) (*webui.ServerDetailView, error) {
	return webuiservers.DetailView(servermanage.New(s.store), identifier)
}

// webUIListenPlan 每次重算时读取拓扑虚拟局域网成员地址、本机网卡地址与
// serve.ui_listen 设置，交给纯推导函数得出本次监听集合。
func webUIListenPlan(st *store.Store, manualListen string) (webui.ListenPlan, error) {
	topologyAddresses, err := st.AllVirtualNetworkAddresses()
	if err != nil {
		return webui.ListenPlan{}, err
	}
	interfaceAddresses, err := localInterfaceAddresses()
	if err != nil {
		return webui.ListenPlan{}, err
	}
	return webui.DeriveListenAddresses(topologyAddresses, interfaceAddresses, manualListen)
}

func localInterfaceAddresses() ([]string, error) {
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(interfaces))
	for _, address := range interfaces {
		if ipNet, ok := address.(*net.IPNet); ok && ipNet.IP != nil {
			out = append(out, ipNet.IP.String())
		}
	}
	return out, nil
}

// servePortFromAddr 从主监听地址里取端口；解析失败或端口为 0 时返回 0，
// 由 Web UI 按默认端口兜底。
func servePortFromAddr(addr string) int {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// startWebUIListenerManager 构造权威端 Web UI Handler 与监听管理者，并同步
// 执行第一次地址推导。绑定失败由管理者记录并重试，不阻断主监听。
func startWebUIListenerManager(ctx context.Context, st *store.Store, tlsCert, tlsKey, fingerprint, addr string, serveOpts ServeOpts) (*webUIListenerManager, error) {
	port, manualListen, err := ReadWebUIConfig(st)
	if err != nil {
		return nil, err
	}
	loopbackPlan := webui.ListenPlan{Addresses: []string{"127.0.0.1"}, NeedsManualListen: true}
	plan, planErr := webUIListenPlan(st, manualListen)
	logStartup := func(format string, arguments ...any) {
		fmt.Fprintf(os.Stderr, "sshmgr serve: "+format+"\n", arguments...)
	}
	if planErr != nil {
		logStartup("推导 Web UI 监听地址失败，先只监听 127.0.0.1：%v", planErr)
		plan = loopbackPlan
	}
	opsManager := ops.NewWithSettingInputs(st, webUISettingInputs(serveOpts))
	updates := ops.NewUpdateService(ops.UpdateDependencies{Audit: opsManager.WriteUpdateAudit})
	openServeWebUIStore := func() (*store.Store, func(), error) {
		return st, func() {}, nil
	}
	restarts := ops.NewRestartService(ops.RestartDependencies{
		NewService: serveOpts.ServiceRestarterFactory,
		Audit:      opsManager.WriteRestartAudit,
	})
	handler, control, err := webui.NewAuthority(webui.Config{
		Role: webui.RoleServer, Port: port,
		Servers:                serveWebUIServerSource{store: st},
		ServerWriter:           webuiservers.WriterFromManager(servermanage.New(st)),
		ServerConnectionTester: webuiservers.ConnectionTesterFromManagerWithContext(servermanage.New(st), ctx),
		Secrets:                webuisecrets.FromManager(secretmanage.New(st)),
		SecretsWriter:          webuisecrets.WriterFromManager(secretmanage.New(st)),
		Shares:                 webuishares.FromStore(openServeWebUIStore),
		SharesWriter:           webuishares.WriterFromStore(openServeWebUIStore),
		SharesRemoteTasks:      webuishares.RemoteTasksFromStoreWithContext(openServeWebUIStore, ctx),
		ShareUnmountRunner:     webuishares.UnmountTasksFromStoreWithContext(openServeWebUIStore, ctx),
		ShareRevokeRunner:      webuishares.RevokeTasksFromStoreWithContext(openServeWebUIStore, ctx),
		ShareReselectRunner:    webuishares.ReselectTasksFromStoreWithContext(openServeWebUIStore, ctx),
		ShareResidueRunner:     webuishares.ResidueTasksFromStoreWithContext(openServeWebUIStore, ctx),
		SoftwareInventory: webuisoftwareinventory.FromOpener(func() (*store.Store, func(), error) {
			return st, func() {}, nil
		}),
		Topology:       webuitopology.FromStore(st),
		TopologyWriter: webuitopology.WriterFromStore(st),
		TopologyProbe:  webuitopology.ProbeFromStore(st),
		Access: webuiaccess.FromManager(access.New(st, access.WithServeCertFingerprint(func() (string, error) {
			return fingerprint, nil
		}))),
		Ops:                    webuiops.FromManagerWithAuthorityUpdates(opsManager, updates, restarts),
		ServiceRestart:         webuiops.NewRestartStore(restarts),
		Auth:                   vault.NewWebUIAuthStore(st),
		CertificateFingerprint: fingerprint,
		AllowedHosts:           plan.Addresses,
		// 设备码拉取命令要指向权威端主监听（浏览器访问的是 Web UI 自己
		// 的端口），这里把主监听端口交给页面。
		ServePort:      servePortFromAddr(addr),
		ReverifyWindow: endToEndReverifyWindow(),
	})
	if err != nil {
		return nil, err
	}
	firstPlan := true
	manager := NewWebUIListenerManager(WebUIListenerOptions{
		Port: port,
		Plan: func() (webui.ListenPlan, error) {
			_, currentManual, err := ReadWebUIConfig(st)
			if err != nil {
				return webui.ListenPlan{}, err
			}
			currentPlan, err := webUIListenPlan(st, currentManual)
			if err != nil && firstPlan {
				firstPlan = false
				return loopbackPlan, nil
			}
			firstPlan = false
			return currentPlan, err
		},
		Serve: func(listener net.Listener) error {
			server := &http.Server{Handler: handler}
			return server.ServeTLS(listener, tlsCert, tlsKey)
		},
		SetAllowedHosts: control.SetAllowedHosts,
		Log:             logStartup,
	})
	if _, err := manager.Sync(time.Now()); err != nil {
		_ = manager.Close()
		return nil, err
	}
	go func() {
		if err := manager.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "sshmgr serve: web UI listener manager: %v\n", err)
		}
	}()
	return manager, nil
}

func webUISettingInputs(opts ServeOpts) ops.SettingInputs {
	inputs := ops.SettingInputsFromEnv()
	inputs.PairingFlag = opts.PairingFlag
	inputs.DiscoveryFlag = opts.DiscoveryFlag
	inputs.MetadataEditFlag = opts.MetadataEditFlag
	return inputs
}
