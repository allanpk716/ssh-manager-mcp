package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"ssh-manager-mcp/internal/clientfeatures"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// resolveMachineRole 是命令测试替换的角色判定接缝，生产实现读取本机角色状态。
var resolveMachineRole = roles.ResolveMode

// errAmbiguousClientCacheInstance 表示默认实例没有缓存且存在多个命名实例，
// 调用方必须让使用者显式指定实例。
var errAmbiguousClientCacheInstance = errors.New("无法确定唯一实例，请用 `--instance` 指定")

// clientVersionState 表示一次客户端版本读取结果。IsClient 为 false 时没有
// 推荐版本可读；Snapshot 为 nil 时表示客户端缓存不可读。
type clientVersionState struct {
	IsClient bool
	Snapshot *store.Snapshot
}

// AuthorityTooOld 封装命令行版本提示与诊断展示共用的旧权威端判断；非客
// 户端或缓存不可读时恒为否。条件本身定义在
// clientfeatures.AuthorityTooOld。
func (s clientVersionState) AuthorityTooOld() bool {
	return s.IsClient && s.Snapshot != nil && clientfeatures.AuthorityTooOld(s.Snapshot.RecommendedVersion)
}

// readClientVersionState 统一三处客户端推荐版本的读取口径：先判定本机角色，
// 判定失败按非客户端处理；客户端角色再读取指定实例的本地加密快照。实例为空
// 时优先读默认实例；默认实例没有缓存且只存在一个命名实例时读那个命名实例，
// 存在多个命名实例时无法替使用者选择。
func readClientVersionState(instance string, errw io.Writer) (clientVersionState, error) {
	role := resolveClientRole(errw)
	if role != roles.RoleClient {
		return clientVersionState{}, nil
	}

	instance, err := resolveClientCacheInstance(instance)
	if err != nil {
		return clientVersionState{IsClient: true}, err
	}
	snap, err := clientops.LoadCacheSnapshotFor(instance)
	if err != nil {
		return clientVersionState{IsClient: true}, err
	}
	return clientVersionState{IsClient: true, Snapshot: snap}, nil
}

// resolveClientRole 只判定本机是否为客户端；判定失败时按非客户端处理。
func resolveClientRole(errw io.Writer) roles.Role {
	launch, err := resolveMachineRole("")
	if err != nil {
		fmt.Fprintf(errw, "警告: 判定本机角色失败(%v)，本次按非客户端处理\n", err)
		return roles.RoleStandalone
	}
	return launch.Role
}

func isClientRole(errw io.Writer) bool {
	return resolveClientRole(errw) == roles.RoleClient
}

// resolveClientCacheInstance 保持与现有缓存命令一致的实例选择：显式实例
// 直接校验；未显式指定时默认实例优先，只有默认实例为空时才查看命名实例。
func resolveClientCacheInstance(instance string) (string, error) {
	if err := checkInstanceFlag(instance); err != nil {
		return "", err
	}
	if instance != "" || clientops.SingleSlotOverrideEnvSet() || cachePresentFor("") {
		return instance, nil
	}
	names, err := clientops.ListInstances()
	if err != nil {
		return "", err
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if len(names) > 1 {
		return "", fmt.Errorf("默认实例没有缓存，但有 %d 个命名实例(%s)：%w", len(names), strings.Join(names, ", "), errAmbiguousClientCacheInstance)
	}
	return "", nil
}
