package mcpserver

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/localaddr"
	"ssh-manager-mcp/internal/pairing"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
	"ssh-manager-mcp/internal/webui"
)

func TestRunServeWebUIPairApprovalToDeviceCodeRevocation(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleServer)
	mainListener := openLocalTCPListener(t)
	webUIListener := openLocalTCPListener(t)
	profileID, err := st.AddProfile("integration-ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (vault.NewWebUIAuthStore(st)).SetAdminPasswordWithAudit(
		[]byte("correct-password"),
		webui.AuditEvent{Action: "admin.password_set", Status: "ok"},
	); err != nil {
		t.Fatal(err)
	}
	certPub := serveCertificatePublicKey(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	mainAddress := mainListener.Addr().String()
	// 客户端声明的目标地址用登记过的对外主机名；环回地址不能登记。
	if err := st.SetSetting(localaddr.PublicAddressesSettingKey, "authority.example.test"); err != nil {
		t.Fatal(err)
	}
	go func() {
		done <- RunServe(ctx, st, mainAddress, "", "", serveOptionsForTest(mainListener, webUIListener))
	}()
	waitTCPListener(t, mainAddress, done)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			// 测试使用权威端自签证书，这里只校验传输加密与协议签名，不校验证书签发链。
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Jar:     jar,
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	mainURL := "https://" + mainAddress
	webURL := "https://" + webUIListener.Addr().String()
	csrfToken := loginThroughAuthorityWebUI(t, client, webURL, "correct-password")

	pairClient := newPairClient(t, "integration-laptop", "https://authority.example.test:7878")
	pairClient.hint = "integration-ops"
	enrollResponse := pairPostToServe(t, client, mainURL+"/pair/enroll", pairClient.enrollBody())
	enrollBody := integrationBody(t, enrollResponse)
	if enrollResponse.StatusCode != http.StatusOK {
		t.Fatalf("配对登记状态码=%d 正文：\n%s", enrollResponse.StatusCode, enrollBody)
	}
	ackKey, credentialsKey := pairClient.learnServer(t, enrollBody, certPub)

	pairingPage := integrationGet(t, client, webURL+"/ui/clients/pairing")
	pairingBody := integrationBody(t, pairingPage)
	if pairingPage.StatusCode != http.StatusOK || !strings.Contains(string(pairingBody), "integration-laptop") {
		t.Fatalf("配对页状态码=%d 页面：\n%s", pairingPage.StatusCode, pairingBody)
	}
	csrfToken = hiddenCSRFValue(string(pairingBody))
	approveForm := url.Values{
		"csrf_token": {csrfToken}, "profile": {profileID}, "compared": {"1"},
	}
	approveResponse, err := postAuthorityForm(client, webURL+"/ui/clients/pairing/"+hex.EncodeToString(pairClient.id[:])+"/approve", approveForm)
	if err != nil {
		t.Fatal(err)
	}
	approveBody := integrationBody(t, approveResponse)
	if approveResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("Web UI 批准状态码=%d 页面：\n%s", approveResponse.StatusCode, approveBody)
	}

	finishBodyText := fmt.Sprintf(`{"id":%q,"ack":%q}`, hex.EncodeToString(pairClient.id[:]), pairClient.ackFor(ackKey))
	finishResponse := pairPostToServe(t, client, mainURL+"/pair/finish", finishBodyText)
	finishBody := integrationBody(t, finishResponse)
	if finishResponse.StatusCode != http.StatusOK {
		t.Fatalf("客户端完成配对状态码=%d 正文：\n%s", finishResponse.StatusCode, finishBody)
	}
	var sealedResponse struct {
		Sealed string `json:"sealed"`
	}
	if err := json.Unmarshal(finishBody, &sealedResponse); err != nil {
		t.Fatal(err)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(sealedResponse.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	plainCredentials, err := pairing.OpenCreds(credentialsKey, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var credentials struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(plainCredentials, &credentials); err != nil {
		t.Fatal(err)
	}
	if credentials.DeviceCode == "" {
		t.Fatalf("配对完成信封缺少设备码：%s", plainCredentials)
	}

	snapshot := integrationAuthorizedGet(t, client, mainURL+"/snapshot", credentials.DeviceCode)
	snapshotBody := integrationBody(t, snapshot)
	if snapshot.StatusCode != http.StatusOK {
		t.Fatalf("新设备码第一次拉取快照状态码=%d 正文：\n%s", snapshot.StatusCode, snapshotBody)
	}

	deviceID := deviceCodeIDByName(t, st, "integration-laptop")
	devicePage := integrationGet(t, client, webURL+"/ui/clients/"+deviceID+"/remove")
	devicePageBody := integrationBody(t, devicePage)
	csrfToken = hiddenCSRFValue(string(devicePageBody))
	revokeResponse, err := postAuthorityForm(client, webURL+"/ui/clients/"+deviceID+"/remove", url.Values{
		"csrf_token": {csrfToken}, "confirm": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	revokeBody := integrationBody(t, revokeResponse)
	if revokeResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("Web UI 移除客户端状态码=%d 页面：\n%s", revokeResponse.StatusCode, revokeBody)
	}

	revokedSnapshot := integrationAuthorizedGet(t, client, mainURL+"/snapshot", credentials.DeviceCode)
	revokedSnapshotBody := integrationBody(t, revokedSnapshot)
	if revokedSnapshot.StatusCode != http.StatusUnauthorized {
		t.Fatalf("移除后的客户端凭据再次拉取应返回 401，状态码=%d 正文：\n%s", revokedSnapshot.StatusCode, revokedSnapshotBody)
	}

	foreignClient := newPairClient(t, "integration-foreign-laptop", "https://203.0.113.9:7878")
	foreignClient.hint = "integration-ops"
	foreignEnroll := pairPostToServe(t, client, mainURL+"/pair/enroll", foreignClient.enrollBody())
	foreignEnrollBody := integrationBody(t, foreignEnroll)
	if foreignEnroll.StatusCode != http.StatusOK {
		t.Fatalf("异常地址配对登记状态码=%d 正文：\n%s", foreignEnroll.StatusCode, foreignEnrollBody)
	}
	foreignPage := integrationGet(t, client, webURL+"/ui/clients/pairing")
	foreignPageBody := string(integrationBody(t, foreignPage))
	if !strings.Contains(foreignPageBody, "integration-foreign-laptop") ||
		!strings.Contains(foreignPageBody, "这条请求连接的地址不是本机") ||
		strings.Contains(foreignPageBody, `name="override"`) {
		t.Fatalf("异常地址配对列表应只给拒绝且不要求覆盖文字：\n%s", foreignPageBody)
	}
	foreignCSRF := hiddenCSRFValue(foreignPageBody)
	foreignApprove := url.Values{
		"csrf_token": {foreignCSRF}, "profile": {profileID}, "compared": {"1"},
	}
	foreignResponse, err := postAuthorityForm(client, webURL+"/ui/clients/pairing/"+hex.EncodeToString(foreignClient.id[:])+"/approve", foreignApprove)
	if err != nil {
		t.Fatal(err)
	}
	foreignApproveBody := integrationBody(t, foreignResponse)
	if foreignResponse.StatusCode != http.StatusConflict {
		t.Fatalf("异常地址经 Web UI 批准应被后端拒绝，状态码=%d 页面：\n%s", foreignResponse.StatusCode, foreignApproveBody)
	}
	foreignAudit, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"pair.approve"}, Statuses: []string{"error"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(foreignAudit) != 1 || !strings.Contains(foreignAudit[0].Command, `"name":"integration-foreign-laptop"`) {
		t.Fatalf("异常地址 Web UI 批准应有失败审计：%+v", foreignAudit)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe 关闭失败：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunServe 关闭超时")
	}
}

func serveCertificatePublicKey(t *testing.T) crypto.PublicKey {
	t.Helper()
	certPath, _, _, err := LoadOrCreateServeCert()
	if err != nil {
		t.Fatal(err)
	}
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certBytes)
	if block == nil {
		t.Fatal("权威端证书没有 PEM 块")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate.PublicKey
}

func waitTCPListener(t *testing.T, address string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("权威端主监听不应退出：%v", err)
		default:
		}
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			if err := connection.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待权威端主监听启动超时")
}

func loginThroughAuthorityWebUI(t *testing.T, client *http.Client, baseURI, password string) string {
	t.Helper()
	var body string
	for attempt := 0; attempt < 50; attempt++ {
		response := integrationGet(t, client, baseURI+"/ui/login")
		body = string(integrationBody(t, response))
		if response.StatusCode == http.StatusOK {
			if token := hiddenCSRFValue(body); token != "" {
				loginResponse, err := postAuthorityForm(client, baseURI+"/ui/login", url.Values{
					"csrf_token": {token}, "password": {password},
				})
				if err != nil {
					t.Fatal(err)
				}
				loginBody := integrationBody(t, loginResponse)
				if loginResponse.StatusCode != http.StatusSeeOther {
					t.Fatalf("登录状态码=%d 页面：\n%s", loginResponse.StatusCode, loginBody)
				}
				return token
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("登录页没有打开，最后页面：\n%s", body)
	return ""
}

func pairPostToServe(t *testing.T, client *http.Client, target, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(buildinfo.ClientVersionHeader, buildinfo.Version)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func integrationGet(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func integrationAuthorizedGet(t *testing.T, client *http.Client, target, deviceCode string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+deviceCode)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func integrationBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func deviceCodeIDByName(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	devices, err := st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		if device.Name == name {
			return device.ID
		}
	}
	t.Fatalf("找不到设备码 %s", name)
	return ""
}
