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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/roles"
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

// runUICommand 组装 ui 命令：按既有角色判定入口解析本机角色，单机与客户端
// 构造 Web UI Handler 并监听回环随机端口，权威端与未初始化角色直接拒绝且
// 不启动任何监听。
func runUICommand(cmd *cobra.Command, instance string) error {
	launch, err := resolveMachineRole("")
	if err != nil {
		return err
	}
	if instance != "" && launch.Kind == roles.LaunchBroker &&
		(launch.Role == roles.RoleStandalone || launch.Role == roles.RoleServer) {
		return NewExitCodeError(1, errors.New("`--instance` 只选择客户端缓存实例；本机自持 vault 的 Web UI 直接读取 vault，请去掉 `--instance`"))
	}

	var (
		webRole          webui.Role
		servers          webui.ServerSource
		accessSource     webui.AccessStore
		opsSource        webui.OpsStore
		resolvedInstance string
	)
	switch {
	case launch.Kind == roles.LaunchBroker && !launch.ResumeSetup && launch.Role == roles.RoleStandalone:
		webRole = webui.RoleStandalone
		servers = standaloneUIServerSource{}
		accessSource = standaloneUIAccessStore()
		opsSource = standaloneUIOpsStore()
	case launch.Kind == roles.LaunchClient && !launch.ResumeSetup:
		resolvedInstance, err = resolveClientCacheInstance(instance)
		if err != nil {
			return err
		}
		webRole = webui.RoleClient
		servers = clientUIServerSource{instance: resolvedInstance}
		opsSource = webuiops.ClientAuditStore()
	case launch.Kind == roles.LaunchBroker && launch.Role == roles.RoleServer:
		st, err := openUnlockedStore()
		if err != nil {
			return err
		}
		defer st.Close()
		fmt.Fprintln(cmd.OutOrStdout(), "权威端 Web UI 由 `serve` 进程的独立监听提供：")
		return writeAuthorityWebUIStatus(cmd.OutOrStdout(), st)
	default:
		return NewExitCodeError(1, errors.New("本机尚未完成初始化，无法启动 Web UI。请先运行 `sshmgr tui` 完成初始化"))
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
	handler, startup, err := webui.New(webui.Config{
		Role:           webRole,
		Servers:        servers,
		VersionHint:    versionHint,
		Port:           port,
		Access:         accessSource,
		Ops:            opsSource,
		ClientInstance: resolvedInstance,
	})
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
