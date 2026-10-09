package hotswap

// adopt_transport.go — 会话领养的 SDK 缝(规格实施决策第 3 条)。
//
// 继任进程的标准输入输出上不会再收到 initialize(宿主已与前任握过手),
// 会直接收到后续请求;而 go-sdk 服务端对「未 initialize 就收到请求」一律
// 拒绝(server.go 的 handle 检查 InitializeParams)。官方入口是
// Server.Connect 的 ServerSessionOptions.State:把前任握手得到的参数作为
// 会话初始状态注入,继任即以「已握手」状态起服务。本文件做参数的编码
// (前任侧,进 SSHMGR_HOTSWAP_* 环境)与解码(继任侧),并单点封装
// 「起一个 stdio 会话」的 Connect+Wait 形态(fresh 与 adopted 共用)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/buildinfo"
)

// 领养 Session 的键(键约束见 session.go:字母数字下划线)。
const (
	sessionKeyProtocolVersion = "mcp_protocol_version" // 已协商的 MCP 协议版本
	sessionKeyClientCaps      = "mcp_client_capabilities"
	sessionKeyClientInfo      = "mcp_client_info"
)

// SessionFromInitializeParams 把前任握手得到的会话参数编码进领养 Session
// (供 Handover/StartSuccessor 传给继任)。协议版本必填;宿主能力与
// clientInfo 为空时省略键。
func SessionFromInitializeParams(p *mcp.InitializeParams) (Session, error) {
	if p == nil {
		return nil, errors.New("hotswap: nil InitializeParams")
	}
	if p.ProtocolVersion == "" {
		return nil, errors.New("hotswap: negotiated protocol version is empty")
	}
	sess := Session{sessionKeyProtocolVersion: p.ProtocolVersion}
	if p.Capabilities != nil {
		b, err := json.Marshal(p.Capabilities)
		if err != nil {
			return nil, fmt.Errorf("hotswap: encode client capabilities: %w", err)
		}
		sess[sessionKeyClientCaps] = string(b)
	}
	if p.ClientInfo != nil {
		b, err := json.Marshal(p.ClientInfo)
		if err != nil {
			return nil, fmt.Errorf("hotswap: encode client info: %w", err)
		}
		sess[sessionKeyClientInfo] = string(b)
	}
	return sess, nil
}

// AdoptedSessionState 是解码侧:从领养 Session 重建 ServerSessionState
// (已握手 + 已 initialized)。字段缺失/不合法返回错误——领养环境存在但
// 无法解读时,起一个「会拒绝所有请求」的假握手服务没有意义,宁可失败。
func AdoptedSessionState(sess Session) (*mcp.ServerSessionState, error) {
	v, ok := sess[sessionKeyProtocolVersion]
	if !ok || v == "" {
		return nil, fmt.Errorf("hotswap: adoption session lacks %q", sessionKeyProtocolVersion)
	}
	st := &mcp.ServerSessionState{
		InitializeParams: &mcp.InitializeParams{
			ProtocolVersion: v,
			Capabilities:    &mcp.ClientCapabilities{},
			ClientInfo:      &mcp.Implementation{},
		},
		InitializedParams: &mcp.InitializedParams{},
	}
	if raw := sess[sessionKeyClientCaps]; raw != "" {
		if err := json.Unmarshal([]byte(raw), st.InitializeParams.Capabilities); err != nil {
			return nil, fmt.Errorf("hotswap: decode %s: %w", sessionKeyClientCaps, err)
		}
	}
	if raw := sess[sessionKeyClientInfo]; raw != "" {
		if err := json.Unmarshal([]byte(raw), st.InitializeParams.ClientInfo); err != nil {
			return nil, fmt.Errorf("hotswap: decode %s: %w", sessionKeyClientInfo, err)
		}
	}
	return st, nil
}

// ServeSession 在传输 t 上服务恰好一个会话:state 为 nil 时是全新握手
// (fresh 桥),非 nil 时以领养状态直接起服务(继任)。afterConnect 在
// Connect 成功后、等待会话结束前调用一次(继任侧在此写就绪文件、补发
// 清单变更通知)。语义与 mcp.Server.Run 相同(含 ctx 取消时主动关闭会话),
// 差别仅在可注入初始状态与会话挂钩。
func ServeSession(ctx context.Context, srv *mcp.Server, t mcp.Transport, state *mcp.ServerSessionState, afterConnect func(ss *mcp.ServerSession)) error {
	var opts *mcp.ServerSessionOptions
	if state != nil {
		opts = &mcp.ServerSessionOptions{State: state}
	}
	ss, err := srv.Connect(ctx, t, opts)
	if err != nil {
		return err
	}
	if afterConnect != nil {
		afterConnect(ss)
	}
	ssClosed := make(chan error, 1)
	go func() { ssClosed <- ss.Wait() }()
	select {
	case <-ctx.Done():
		ss.Close()
		<-ssClosed
		return ctx.Err()
	case err := <-ssClosed:
		return err
	}
}

// BridgeVersion 是本桥对外自报的版本:生产路径是 buildinfo.Version(ldflags
// 注入);SSHMGR_TEST_VERSION 是测试缝——换手端到端测试用同一测试二进制
// 扮两代,靠它让两代自报不同版本(就绪文件与 reload_self 的 version 字段
// 都取自这里)。生产环境不设此变量,行为即 buildinfo。
func BridgeVersion() string {
	if v := os.Getenv("SSHMGR_TEST_VERSION"); v != "" {
		return v
	}
	return buildinfo.Version
}
