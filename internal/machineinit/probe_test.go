package machineinit_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func TestProbeServeWebUI(t *testing.T) {
	withMachineEnv(t)
	if _, err := machineinit.Initialize(roles.RoleServer); err != nil {
		t.Fatal(err)
	}
	masterKey, err := os.ReadFile(os.Getenv("SSHMGR_FILEKEY_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	closedPort := freeLocalPort(t)
	if err := st.SetSetting("serve.ui_port", strconv.Itoa(closedPort)); err != nil {
		t.Fatal(err)
	}
	silent, err := machineinit.ProbeServeWebUI()
	if err != nil {
		t.Fatal(err)
	}
	if !silent.Known || silent.Port != closedPort {
		t.Fatalf("无服务时的探测结果应有端口：%+v", silent)
	}
	if silent.Running {
		t.Fatalf("端口未监听时不应显示在运行：%+v", silent)
	}
	if !containsString(silent.Addresses, "127.0.0.1") {
		t.Fatalf("地址列表应包含 127.0.0.1：%v", silent.Addresses)
	}

	// 本地 HTTPS 测试服务器：自签证书，覆盖「有响应」分支。
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	runningPort := server.Listener.Addr().(*net.TCPAddr).Port
	if err := st.SetSetting("serve.ui_port", strconv.Itoa(runningPort)); err != nil {
		t.Fatal(err)
	}
	responding, err := machineinit.ProbeServeWebUI()
	if err != nil {
		t.Fatal(err)
	}
	if !responding.Running {
		t.Fatalf("HTTPS 测试服务器有响应时应显示在运行：%+v", responding)
	}
	if responding.Port != runningPort {
		t.Fatalf("端口 = %d，应为 %d", responding.Port, runningPort)
	}
}
