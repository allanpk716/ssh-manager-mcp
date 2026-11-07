package sshbroker

import (
	"context"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
)

// ServerDialRequest 是按服务器条目拨号统一入口的输入。网络拓扑信息与成功
// 地址缓存由规格 #132 的后续票在入口内部接入；本票的候选地址只有条目的
// 兜底「主机:端口」，连接行为与原先在调用处直接拨号完全一致。
type ServerDialRequest struct {
	// Server 是目标服务器条目。本票只使用它的兜底主机、端口与登录用户。
	Server *models.Server
	// Auth 是登录目标服务器使用的认证方式。
	Auth ssh.AuthMethod
	// HostKeys 是主机密钥存储。统一入口按目标服务器条目在这里构建
	// 首次信任（TOFU）回调，锚跟随条目；缓存模式下调用方传入锚定
	// 转发的包装存储即可。
	HostKeys HostKeyStore
	// KeepAlive 为 true 时等价于原先的 ConnectKeepAlive：连接挂周期保活
	// 循环（后台长任务与中继使用）。默认与原先的 Connect 相同，不挂循环。
	KeepAlive bool
	// AddressTimeout 是每个候选地址的拨号超时。候选只有一个地址时
	// 不套用（规格 #132 的既定行为），而本票恒只有一个兜底地址，因此
	// 该值暂不生效——所有连接仍只受调用方上下文的期限约束，与原先一致。
	AddressTimeout time.Duration
}

// lowLevelDial 是统一入口的低层连接函数：按「主机、端口」拨一个地址并完成
// 认证。默认值是现有的 connectWith（Connect 与 ConnectKeepAlive 共用的底层
// 函数）；测试注入它，把候选地址映射到进程内 SSH 服务器。
var lowLevelDial = connectWith

// ConnectServerEntry 是 sshmgr 发起 SSH 连接的统一生产入口：按目标服务器
// 条目拨号，返回已建立的客户端与连接说明。本票候选地址只有条目的兜底
// 「主机:端口」，连接说明恒为空；后续票在入口内部扩展多地址选址，不改变
// 调用方。
func ConnectServerEntry(ctx context.Context, req ServerDialRequest) (*Client, []string, error) {
	hkCb, err := HostKeyTOFU(req.HostKeys, req.Server)
	if err != nil {
		return nil, nil, err
	}
	ka := keepAliveSpec{}
	if req.KeepAlive {
		ka = backgroundKeepAlive
	}
	cli, err := lowLevelDial(ctx, req.Server.Host, req.Server.Port, req.Server.User, req.Auth, hkCb, ka)
	if err != nil {
		return nil, nil, err
	}
	return cli, nil, nil
}
