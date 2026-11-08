package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// serviceFixture 组装一台单机部署的测试存储：本机设备是「笔记本」，目标
// 设备是「NAS」，两台设备同在家A局域网与 NetBird；地址与端口由测试注入
// 拨号结论。
type serviceFixture struct {
	store      *store.Store
	resultsDir string
	selfDevice *models.Device
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(dir, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "NetBird", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	laptop, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(laptop.ID); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"NAS", "家A局域网", "192.0.2.10"},
		{"NAS", "NetBird", "100.64.10.5"},
		{"笔记本", "家A局域网", "192.0.2.2"},
		{"笔记本", "NetBird", "100.64.10.2"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	return &serviceFixture{store: st, resultsDir: dir, selfDevice: laptop}
}

// fakeLocalDial 把「地址:端口」映射到固定结论，并记录收到的超时与峰值并
// 发，未登记的地址返回错误。
type fakeLocalDial struct {
	mu       sync.Mutex
	outcomes map[string]dialResult
	timeouts []time.Duration
	inFlight atomic.Int64
	peak     int64
	delay    time.Duration
}

type dialResult struct {
	err      error
	duration time.Duration
}

func (f *fakeLocalDial) dial(_ context.Context, address string, port int, timeout time.Duration) (time.Duration, error) {
	current := f.inFlight.Add(1)
	for {
		peak := f.peak
		if current <= peak || atomic.CompareAndSwapInt64(&f.peak, peak, current) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	f.mu.Lock()
	f.timeouts = append(f.timeouts, timeout)
	outcome, present := f.outcomes[fmt.Sprintf("%s:%d", address, port)]
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if !present {
		return 0, fmt.Errorf("unexpected dial target %s:%d", address, port)
	}
	return outcome.duration, outcome.err
}

func (f *fakeLocalDial) set(address string, port int, err error, duration time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outcomes == nil {
		f.outcomes = map[string]dialResult{}
	}
	f.outcomes[fmt.Sprintf("%s:%d", address, port)] = dialResult{err: err, duration: duration}
}

func (f *fakeLocalDial) observedTimeouts() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.timeouts...)
}

func (f *serviceFixture) service(dial LocalDialFunc) (*Service, *proberesults.FileProbeResults) {
	results := proberesults.NewFileProbeResults(filepath.Join(f.resultsDir, "probe-results.json"))
	return &Service{
		Store: f.store, LocalDial: dial, Results: results,
		Now: func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}, results
}

// TestServiceSelfProbeGroupsResultsByText: 从本机起点的探测按目标分组，每
// 个共同网络的每个地址一行，地址用网络名加序号指称，成功失败带耗时与相
// 对时间。
func TestServiceSelfProbeGroupsResultsByText(t *testing.T) {
	fixture := newServiceFixture(t)
	dial := &fakeLocalDial{}
	dial.set("192.0.2.10", 445, nil, 3*time.Millisecond)
	dial.set("100.64.10.5", 445, &timeoutError{}, 1500*time.Millisecond)
	service, results := fixture.service(dial.dial)

	report, err := service.Probe(context.Background(), Request{
		From: "self", Targets: []string{"NAS:445"}, Source: proberesults.SourceProbe,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := report.Text()
	for _, want := range []string{
		"NAS:445  家A局域网 第1个地址  ✓3ms 刚刚",
		"NAS:445  NetBird 第1个地址  ✗连接超时 刚刚",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "192.0.2.10") || strings.Contains(text, "100.64.10.5") {
		t.Fatalf("text must not expose addresses:\n%s", text)
	}
	observations, err := results.List(fixture.selfDevice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %+v, want one per address", observations)
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

// TestServiceTargetParsingDefaultsAndLimits: 目标按服务器编号优先解析；端口
// 缺省用服务器条目端口或 22；重复目标只探测一次；超过 10 个目标报错并写
// 明上限；写错的设备名给出「拓扑中没有这台设备」。
func TestServiceTargetParsingDefaultsAndLimits(t *testing.T) {
	fixture := newServiceFixture(t)
	credentialID, err := fixture.store.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	serverResult, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "NAS SSH", Host: "192.0.2.10", Port: 2202, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "NAS")
	if err != nil {
		t.Fatal(err)
	}
	dial := &fakeLocalDial{}
	dial.set("192.0.2.10", 2202, nil, 2*time.Millisecond)
	dial.set("192.0.2.10", 22, nil, 4*time.Millisecond)
	dial.set("100.64.10.5", 2202, nil, 2*time.Millisecond)
	dial.set("100.64.10.5", 22, nil, 4*time.Millisecond)
	service, _ := fixture.service(dial.dial)

	report, err := service.Probe(context.Background(), Request{
		From: "self", Targets: []string{serverResult.ServerID, serverResult.ServerID, "NAS"}, Source: proberesults.SourceProbe,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := report.Text()
	if !strings.Contains(text, serverResult.ServerID+":2202") {
		t.Fatalf("server target must use the entry port:\n%s", text)
	}
	if !strings.Contains(text, "NAS:22  家A局域网 第1个地址  ✓4ms 刚刚") {
		t.Fatalf("device target must default to port 22:\n%s", text)
	}
	if strings.Count(text, serverResult.ServerID+":2202") != 2 {
		t.Fatalf("duplicate target must be probed once:\n%s", text)
	}

	tooMany := make([]string, 11)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("NAS:%d", 6000+index)
	}
	if _, err := service.Probe(context.Background(), Request{From: "self", Targets: tooMany}); err == nil || !strings.Contains(err.Error(), "10") {
		t.Fatalf("more than 10 targets must fail with the limit, got %v", err)
	}
	if _, err := service.Probe(context.Background(), Request{From: "self", Targets: []string{"不存在的设备"}}); err == nil ||
		!strings.Contains(err.Error(), "拓扑中没有这台设备") {
		t.Fatalf("unknown device error = %v", err)
	}
}

// TestServiceNoCommonNetworkAndMismatchNotes: 没有共同网络时写「无共同网络，
// 未探测」；物理局域网全部失败且虚拟局域网可达时，两条说明都要出现。
func TestServiceNoCommonNetworkAndMismatchNotes(t *testing.T) {
	fixture := newServiceFixture(t)
	if _, err := fixture.store.AddDevice("云端机器", "", ""); err != nil {
		t.Fatal(err)
	}
	dial := &fakeLocalDial{}
	dial.set("192.0.2.10", 445, syscall.ECONNREFUSED, 0)
	dial.set("100.64.10.5", 445, nil, 9*time.Millisecond)
	service, _ := fixture.service(dial.dial)

	report, err := service.Probe(context.Background(), Request{
		From: "self", Targets: []string{"NAS:445", "云端机器:22"}, Source: proberesults.SourceProbe,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := report.Text()
	if !strings.Contains(text, "云端机器:22  无共同网络，未探测") {
		t.Fatalf("text missing the no-common-network line:\n%s", text)
	}
	if !strings.Contains(text, "登记与实际不一致：家A局域网（连接被拒绝）") {
		t.Fatalf("text missing the registration mismatch note:\n%s", text)
	}
	if !strings.Contains(text, "物理局域网不通，实际可经 NetBird 到达") {
		t.Fatalf("text missing the virtual network note:\n%s", text)
	}
}

// TestServiceSelfWithoutRegisteredDeviceFails: 本机没有登记设备时以 self 起
// 点直接报错，不退回兜底地址。
func TestServiceSelfWithoutRegisteredDeviceFails(t *testing.T) {
	fixture := newServiceFixture(t)
	if _, err := roles.ClearLocalDevice(); err != nil {
		t.Fatal(err)
	}
	service, _ := fixture.service((&fakeLocalDial{}).dial)
	if _, err := service.Probe(context.Background(), Request{From: "self", Targets: []string{"NAS"}}); err == nil ||
		!strings.Contains(err.Error(), "本机未登记设备") {
		t.Fatalf("self without a local device error = %v", err)
	}
}

// TestServiceManagedDeviceTargetRequiresProfileGrant: 关联了服务器条目的设
// 备按设备名当目标时，同样要过授权检查；不受管设备与客户端实例设备不受
// 授权范围限制。
func TestServiceManagedDeviceTargetRequiresProfileGrant(t *testing.T) {
	fixture := newServiceFixture(t)
	credentialID, err := fixture.store.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	grantedResult, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "已授权条目", Host: "192.0.2.40", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "NAS")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "范围外条目", Host: "192.0.2.41", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "笔记本"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AddDevice("不受管设备", "", ""); err != nil {
		t.Fatal(err)
	}
	profileID, err := fixture.store.AddProfile("探测授权")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.AddCacheToken("client-instance", profileID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AddDevice("客户端设备", "", "client-instance"); err != nil {
		t.Fatal(err)
	}
	service, _ := fixture.service((&fakeLocalDial{}).dial)
	service.GateServer = func(serverID string) error {
		if serverID == grantedResult.ServerID {
			return nil
		}
		return fmt.Errorf("server %s not in your profile", serverID)
	}

	// NAS 关联了已授权条目：设备名目标照常解析。
	if _, err := service.Probe(context.Background(), Request{From: "self", Targets: []string{"NAS"}}); err != nil {
		t.Fatalf("managed device with a granted entry must pass: %v", err)
	}
	// 笔记本关联的条目全部在范围外：设备名目标必须被拒绝。
	_, err = service.Probe(context.Background(), Request{From: "self", Targets: []string{"笔记本"}})
	if err == nil || !strings.Contains(err.Error(), "not in your profile") {
		t.Fatalf("managed device without a granted entry must be rejected, got %v", err)
	}
	// 不受管设备与客户端实例不受授权范围限制。
	for _, target := range []string{"不受管设备", "客户端设备"} {
		if _, err := service.Probe(context.Background(), Request{From: "self", Targets: []string{target}}); err != nil {
			t.Fatalf("target %s must not be gated: %v", target, err)
		}
	}
}

// TestServiceTargetServerIDCaseCorrectionHint: 目标服务器编号写错大小写
// 时，与起点一样给出大小写纠正提示，而不是落到设备名报错。
func TestServiceTargetServerIDCaseCorrectionHint(t *testing.T) {
	fixture := newServiceFixture(t)
	credentialID, err := fixture.store.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	grantedResult, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "NAS SSH", Host: "192.0.2.10", Port: 2202, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "NAS")
	if err != nil {
		t.Fatal(err)
	}
	wrongCase := strings.ToUpper(grantedResult.ServerID)
	if wrongCase == grantedResult.ServerID {
		t.Fatalf("server id %q has no case variant", grantedResult.ServerID)
	}
	service, _ := fixture.service((&fakeLocalDial{}).dial)
	service.GateServer = func(serverID string) error {
		if serverID == grantedResult.ServerID {
			return nil
		}
		if strings.EqualFold(serverID, grantedResult.ServerID) {
			return fmt.Errorf("server %s not in your profile (did you mean %q? server ids are case-sensitive)",
				serverID, grantedResult.ServerID)
		}
		return fmt.Errorf("server %s not in your profile", serverID)
	}

	_, err = service.Probe(context.Background(), Request{From: "self", Targets: []string{wrongCase}})
	if err == nil || !strings.Contains(err.Error(), "did you mean") || !strings.Contains(err.Error(), grantedResult.ServerID) {
		t.Fatalf("wrong-case target must carry the case correction hint, got %v", err)
	}
}

// TestServiceUsesAddressTimeoutSetting: 逐地址超时复用统一拨号入口的环境变
// 量设置。
func TestServiceUsesAddressTimeoutSetting(t *testing.T) {
	fixture := newServiceFixture(t)
	t.Setenv("SSHMGR_SSH_ADDRESS_TIMEOUT", "250ms")
	dial := &fakeLocalDial{}
	dial.set("192.0.2.10", 445, nil, time.Millisecond)
	dial.set("100.64.10.5", 445, nil, time.Millisecond)
	service, _ := fixture.service(dial.dial)
	if _, err := service.Probe(context.Background(), Request{From: "self", Targets: []string{"NAS:445"}}); err != nil {
		t.Fatal(err)
	}
	for _, timeout := range dial.observedTimeouts() {
		if timeout != 250*time.Millisecond {
			t.Fatalf("dial timeout = %v, want the environment override", timeout)
		}
	}
}

// TestServiceConcurrencyAndTotalDeadline: 最多 5 个目标并发；总时限到达后
// 未完成的目标标为「探测未完成」。
func TestServiceConcurrencyAndTotalDeadline(t *testing.T) {
	fixture := newServiceFixture(t)
	dial := &fakeLocalDial{delay: 30 * time.Millisecond}
	service, _ := fixture.service(dial.dial)
	service.TotalDeadline = 80 * time.Millisecond
	targets := make([]string, 0, 10)
	for index := 0; index < 10; index++ {
		targets = append(targets, fmt.Sprintf("NAS:%d", 5000+index))
		dial.set("192.0.2.10", 5000+index, nil, time.Millisecond)
		dial.set("100.64.10.5", 5000+index, nil, time.Millisecond)
	}
	report, err := service.Probe(context.Background(), Request{From: "self", Targets: targets})
	if err != nil {
		t.Fatal(err)
	}
	if peak := atomic.LoadInt64(&dial.peak); peak > MaxConcurrency {
		t.Fatalf("peak concurrency = %d, want at most %d", peak, MaxConcurrency)
	}
	if !strings.Contains(report.Text(), "探测未完成") {
		t.Fatalf("text must mark unfinished targets:\n%s", report.Text())
	}
}

// TestServiceTotalDeadlineMarksDialingAddressIncomplete: 到总时限那一刻正
// 在拨号或还没轮到的地址都记「探测未完成」，呈现不带相对时间，也不写进
// 探测结果文件。
func TestServiceTotalDeadlineMarksDialingAddressIncomplete(t *testing.T) {
	fixture := newServiceFixture(t)
	dial := &fakeLocalDial{}
	service, results := fixture.service(dial.dial)
	service.TotalDeadline = 80 * time.Millisecond
	service.LocalDial = func(ctx context.Context, address string, port int, timeout time.Duration) (time.Duration, error) {
		// 第一个地址一直拨到总时限，返回的正是总时限取消错误。
		<-ctx.Done()
		return 0, ctx.Err()
	}

	report, err := service.Probe(context.Background(), Request{From: "self", Targets: []string{"NAS:445"}})
	if err != nil {
		t.Fatal(err)
	}
	text := report.Text()
	if got := strings.Count(text, "✗探测未完成"); got != 2 {
		t.Fatalf("both interrupted addresses must be incomplete:\n%s", text)
	}
	for _, unwanted := range []string{"探测未完成 刚刚", "探测未完成 旧", "连接超时"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("text must not contain %q:\n%s", unwanted, text)
		}
	}
	observations, err := results.List(fixture.selfDevice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 0 {
		t.Fatalf("interrupted addresses must not be recorded: %+v", observations)
	}
}

// TestServiceRemoteProbeUsesFixedCommandAndParsesOutput: 经服务器起点在一
// 次 SSH 会话里执行一条固定命令，输出按序号回填到各目标；起点服务器连接
// 失败时结果开头说明「无法连接起点服务器」。
func TestServiceRemoteProbeUsesFixedCommandAndParsesOutput(t *testing.T) {
	fixture := newServiceFixture(t)
	credentialID, err := fixture.store.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	originResult, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "笔记本 SSH", Host: "192.0.2.20", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID, ShellType: models.ShellPowerShell,
	}, nil, nil, "笔记本")
	if err != nil {
		t.Fatal(err)
	}
	service, results := fixture.service((&fakeLocalDial{}).dial)
	var capturedCommand string
	service.RemoteExec = func(_ context.Context, server *models.Server, shell models.ShellType, command string) (string, error) {
		if server.ID != originResult.ServerID || shell != models.ShellPowerShell {
			return "", fmt.Errorf("unexpected executor input %s %s", server.ID, shell)
		}
		capturedCommand = command
		return "SSHMGR-PROBE 0 ok 3 \nSSHMGR-PROBE 1 fail 1500 timeout\n", nil
	}

	report, err := service.Probe(context.Background(), Request{
		From: originResult.ServerID, Targets: []string{"NAS:445"}, Source: proberesults.SourceProbe,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := report.Text()
	if !strings.Contains(text, "NAS:445  家A局域网 第1个地址  ✓3ms 刚刚") ||
		!strings.Contains(text, "NAS:445  NetBird 第1个地址  ✗连接超时 刚刚") {
		t.Fatalf("remote probe text =\n%s", text)
	}
	if !strings.Contains(capturedCommand, "SSHMGR-PROBE") || !strings.Contains(capturedCommand, "192.0.2.10") {
		t.Fatalf("fixed command =\n%s", capturedCommand)
	}
	observations, err := results.List(fixture.selfDevice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %+v, want both addresses", observations)
	}

	service.RemoteExec = func(context.Context, *models.Server, models.ShellType, string) (string, error) {
		return "", &originConnectionError{err: errors.New("dial timeout")}
	}
	failed, err := service.Probe(context.Background(), Request{
		From: originResult.ServerID, Targets: []string{"NAS:445"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failed.Text(), "无法连接起点服务器") {
		t.Fatalf("origin failure text =\n%s", failed.Text())
	}
}

// TestServiceNonConnectionResultsDoNotTriggerMismatchNote: 服务器缺少探测
// 工具、服务器未返回结果这类非连接结论不代表登记与实际不一致；物理局域
// 网全是这类结果时不标注。
func TestServiceNonConnectionResultsDoNotTriggerMismatchNote(t *testing.T) {
	fixture := newServiceFixture(t)
	credentialID, err := fixture.store.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	originResult, err := fixture.store.AddServerWithCredentialsToDevice(&models.Server{
		Name: "笔记本 SSH", Host: "192.0.2.20", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID, ShellType: models.ShellPOSIX,
	}, nil, nil, "笔记本")
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		// 家A局域网缺少探测工具，NetBird 没有返回结果行。
		"SSHMGR-PROBE 0 fail 1 missing-tool\nSSHMGR-PROBE 1 fail 1 missing-tool\n",
		"",
	} {
		service, _ := fixture.service((&fakeLocalDial{}).dial)
		service.RemoteExec = func(context.Context, *models.Server, models.ShellType, string) (string, error) {
			return output, nil
		}
		report, err := service.Probe(context.Background(), Request{
			From: originResult.ServerID, Targets: []string{"NAS:445"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(report.Text(), "登记与实际不一致") {
			t.Fatalf("non-connection results must not trigger the mismatch note:\n%s", report.Text())
		}
	}
}

// TestServiceRemoteCommandTimeoutKeepsPartialOutput: 会话建立后固定命令超
// 时，不再误报「无法连接起点服务器」；已返回的结果行照常呈现，缺行记
// 「探测未完成」且不写结果文件。
func TestServiceRemoteCommandTimeoutKeepsPartialOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(dir, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "NetBird", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	laptop, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(laptop.ID); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		// 笔记本的登记地址就是进程内 SSH 服务器所在的回环地址，起点拨号
		// 第一次尝试即建立会话。
		{"笔记本", "家A局域网", "127.0.0.1"},
		{"笔记本", "NetBird", "127.0.0.1"},
		{"NAS", "家A局域网", "192.0.2.10"},
		{"NAS", "NetBird", "100.64.10.5"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}

	execServer, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		ExecStream: func(_ string, _ io.Reader, stdout, _ io.Writer) int {
			_, _ = stdout.Write([]byte("SSHMGR-PROBE 0 fail 1 connection-refused\n"))
			// 只输出第一行后挂住，等客户端执行超时后拆掉会话。
			time.Sleep(5 * time.Second)
			return 0
		},
	})
	defer cleanup()
	host, portText, err := net.SplitHostPort(execServer)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	originResult, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "笔记本 SSH", Host: host, Port: port, User: "u",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "笔记本")
	if err != nil {
		t.Fatal(err)
	}
	origin, err := st.GetServer(originResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(origin, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}

	results := proberesults.NewFileProbeResults(filepath.Join(dir, "probe-results.json"))
	service := &Service{
		Store: st, Results: results, TotalDeadline: 2 * time.Second,
		Now: func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}
	report, err := service.Probe(context.Background(), Request{
		From: origin.ID, Targets: []string{"NAS:445"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := report.Text()
	if strings.Contains(text, "无法连接起点服务器") || strings.Contains(text, "未探测") {
		t.Fatalf("command timeout must not be reported as an origin failure:\n%s", text)
	}
	if !strings.Contains(text, "NAS:445  家A局域网 第1个地址  ✗连接被拒绝 刚刚") {
		t.Fatalf("returned line must be presented:\n%s", text)
	}
	if !strings.Contains(text, "NAS:445  NetBird 第1个地址  ✗探测未完成") {
		t.Fatalf("missing line must be incomplete:\n%s", text)
	}
	nasDevice, err := st.GetDevice("NAS")
	if err != nil {
		t.Fatal(err)
	}
	observations, err := results.List(laptop.ID, nasDevice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Category != models.ProbeErrorConnectionRefused {
		t.Fatalf("only the returned line must be recorded: %+v", observations)
	}
}

// TestServiceRemoteOriginWithoutDeviceFails: 起点服务器条目没有关联拓扑设
// 备时报错，提示 owner 补登记。
func TestServiceRemoteOriginWithoutDeviceFails(t *testing.T) {
	fixture := newServiceFixture(t)
	credentialID, _ := fixture.store.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	serverID, err := fixture.store.AddServer(&models.Server{
		Name: "孤立条目", Host: "192.0.2.30", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, _ := fixture.service((&fakeLocalDial{}).dial)
	if _, err := service.Probe(context.Background(), Request{From: serverID, Targets: []string{"NAS"}}); err == nil ||
		!strings.Contains(err.Error(), "起点服务器未关联拓扑设备") {
		t.Fatalf("origin without device error = %v", err)
	}
}
