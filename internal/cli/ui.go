package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiops"
)

// uiOpenBrowser 是「用系统默认浏览器打开网址」的可替换依赖；测试注入替身，
// 不真正打开浏览器。
var uiOpenBrowser = openURLInBrowser

// newUICmd 构造 `sshmgr ui` 命令：单机与客户端角色在本机回环地址的随机端口
// 上按需启动 Web UI，命令退出即停止。
func newUICmd() *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "ui",
		Short: "Start the web management interface on the loopback address",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUICommand(cmd, instance)
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "client cache instance name (same rules as cache commands)")
	return c
}

// runUICommand 组装 ui 命令：按本机状态快照判定去向；空机器、单机续做与
// 未完成初始化的权威端进入初始化向导，单机与客户端构造正式 Web UI 并监听
// 回环随机端口，已运行 serve 的权威端只打印独立监听的状态。
func runUICommand(cmd *cobra.Command, instance string) error {
	snapshot, err := machineinit.ReadSnapshot()
	if err != nil {
		return err
	}
	destination, err := machineinit.DecideUIDestination(snapshot)
	if err != nil {
		return err
	}
	if instance != "" && (destination.Kind == machineinit.UIDestinationStandaloneWebUI ||
		destination.Kind == machineinit.UIDestinationInitializationWizard ||
		destination.Kind == machineinit.UIDestinationServerWizard) {
		return NewExitCodeError(1, errors.New("`--instance` 只选择客户端缓存实例；本机自持 vault 的 Web UI 直接读取 vault，请去掉 `--instance`"))
	}

	var (
		webRole                 webui.Role
		servers                 webui.ServerSource
		serverWriter            webui.ServerWriter
		accessSource            webui.AccessStore
		opsSource               webui.OpsStore
		secretSource            webui.SecretSource
		secretWriter            webui.SecretWriter
		softwareInventorySource webui.SoftwareInventorySource
		resolvedInstance        string
		machineInit             webui.MachineInitializer
	)
	initializing := false
	uiShutdown := make(chan struct{})
	var shutdownOnce sync.Once
	serverWizard := destination.Kind == machineinit.UIDestinationServerWizard
	requestShutdown := func(reason webui.ShutdownReason) {
		if reason == webui.ShutdownReasonClientGuide {
			writeClientInitializationGuide(cmd.OutOrStdout())
		}
		shutdownOnce.Do(func() { close(uiShutdown) })
	}
	switch {
	case destination.Kind == machineinit.UIDestinationInitializationWizard ||
		destination.Kind == machineinit.UIDestinationStandaloneWebUI || serverWizard:
		webRole = webui.RoleStandalone
		servers = standaloneUIServerSource{}
		serverWriter = standaloneUIServerWriter()
		accessSource = standaloneUIAccessStore()
		secretSource = standaloneUISecretStore()
		secretWriter = standaloneUISecretWriter()
		softwareInventorySource = standaloneUISoftwareInventoryStore()
		opsSource = standaloneUIOpsStoreWithUpdates(uiUpdateService(webui.RoleStandalone, ""))
		machineInit = standaloneMachineInitializer{}
		initializing = destination.Kind == machineinit.UIDestinationInitializationWizard || serverWizard
	case destination.Kind == machineinit.UIDestinationClientWebUI:
		resolvedInstance, err = resolveClientCacheInstance(instance)
		if err != nil {
			return err
		}
		webRole = webui.RoleClient
		servers = clientUIServerSource{}
		secretSource = clientUISecretSource{}
		opsSource = webuiops.ClientPageStoreWithUpdates(
			uiUpdateService(webui.RoleClient, resolvedInstance), uiClientPull)
	default:
		// 权威端正式页面由 serve 的独立监听提供；这里只打印状态，不另起
		// 回环监听。
		st, err := openUnlockedStore()
		if err != nil {
			return err
		}
		defer st.Close()
		fmt.Fprintln(cmd.OutOrStdout(), "权威端 Web UI 由 `serve` 进程的独立监听提供：")
		return writeAuthorityWebUIStatus(cmd.OutOrStdout(), st)
	}

	versionHint := func() string { return "" }
	if webRole == webui.RoleClient {
		hintInstance := resolvedInstance
		versionHint = func() string { return uiVersionHintSource(hintInstance) }
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("监听本机回环地址失败：%w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	config := webui.Config{
		Role:              webRole,
		Servers:           servers,
		ServerWriter:      serverWriter,
		Secrets:           secretSource,
		SecretsWriter:     secretWriter,
		SoftwareInventory: softwareInventorySource,
		VersionHint:       versionHint,
		Port:              port,
		Access:            accessSource,
		Ops:               opsSource,
		MachineInit:       machineInit,
		RequestShutdown:   requestShutdown,
		ClientInstance:    resolvedInstance,
	}
	if initializing {
		executablePath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("读取 sshmgr 可执行文件绝对路径失败：%w", err)
		}
		config.InitializationOperatingSystem = runtime.GOOS
		config.InitializationExecutablePath = executablePath
		completions := make(chan webui.InitializationCompletion, 1)
		config.InitializationComplete = func(completion webui.InitializationCompletion) {
			completions <- completion
		}
		defer func() {
			select {
			case completion := <-completions:
				fmt.Fprintln(cmd.OutOrStdout(), "权威端初始化完成，正式 Web UI 地址：")
				for _, address := range completion.Addresses {
					localOnly := ""
					if net.ParseIP(address).IsLoopback() {
						localOnly = "（仅本机）"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "https://%s/ui/%s\n", net.JoinHostPort(address, strconv.Itoa(completion.Port)), localOnly)
				}
			default:
			}
		}()
	}
	var handler http.Handler
	var startup webui.Startup
	if initializing {
		handler, startup, err = webui.NewInitializationWizard(config, machineInitializer{})
	} else {
		handler, startup, err = webui.New(config)
	}
	if err != nil {
		_ = listener.Close()
		return err
	}

	server := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	startupURL := startup.URL(fmt.Sprintf("127.0.0.1:%d", port))
	fmt.Fprintf(cmd.OutOrStdout(), "Web UI 已启动：%s\n", startupURL)
	if err := uiOpenBrowser(startupURL); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "自动打开浏览器失败（%v）。请把上面的网址手动复制到浏览器打开。\n", err)
	}

	// Ctrl+C、终止信号或标准输入结束（例如终端关闭）都会触发优雅关闭。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stdinDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, cmd.InOrStdin())
		close(stdinDone)
	}()

	var listenerErr error
	select {
	case <-ctx.Done():
	case <-stdinDone:
	case <-uiShutdown:
	case err := <-serveErr:
		listenerErr = err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if listenerErr != nil {
		return fmt.Errorf("Web UI 监听异常退出：%w", listenerErr)
	}
	if err := shutdownErr; err != nil {
		return fmt.Errorf("关闭 Web UI 失败：%w", err)
	}
	return nil
}

func writeClientInitializationGuide(output io.Writer) {
	fmt.Fprintln(output, "客户端入网指引：")
	for _, line := range webui.ClientInitializationGuideLines() {
		fmt.Fprintf(output, "  %s\n", line)
	}
}

// openURLInBrowser 用系统默认浏览器打开网址，按操作系统选择打开命令；无图形
// 桌面或命令缺失时返回错误，由调用方提示手动复制网址。
func openURLInBrowser(target string) error {
	var name string
	args := []string{target}
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", target}
	default:
		name = "xdg-open"
	}
	return exec.Command(name, args...).Run()
}
