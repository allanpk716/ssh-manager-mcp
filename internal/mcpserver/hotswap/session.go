package hotswap

import (
	"errors"
	"fmt"
	"strings"
)

// EnvPrefix 是领养模式会话参数在环境变量里的统一前缀。
const EnvPrefix = "SSHMGR_HOTSWAP_"

// EnvReadyPath 是就绪文件路径的环境变量名——前缀下唯一有固定语义的键。
const EnvReadyPath = EnvPrefix + "READY"

// ErrInvalidSessionKey 报告 Session 键不合法:含保留键 READY、非法字符,
// 以及大小写归一后互相碰撞的重复键。
var ErrInvalidSessionKey = errors.New("invalid hotswap session key")

// Session 是领养会话参数集:已协商的 MCP 协议版本、宿主能力、桥形态与
// 实例参数等。库只搬运不解释——键与值对换手核心是不透明数据。
// 键仅允许字母、数字与下划线,序列化为 SSHMGR_HOTSWAP_<大写键>=<值>。
type Session map[string]string

// readyEnvName 是就绪文件路径键去掉前缀后的名字,Session 键不得占用它。
var readyEnvName = strings.TrimPrefix(EnvReadyPath, EnvPrefix)

// BuildEnv 组装继任进程的环境变量切片:先剥掉 base 里既有的全部
// SSHMGR_HOTSWAP_* 条目(上一轮领养的遗留不得泄漏给下一代),再附加
// sess 的各键与就绪文件路径。base 里同键条目后值覆盖前值(精确匹配键名),
// 测试仪表可借此覆盖父环境同名键。
func BuildEnv(base []string, sess Session, readyPath string) ([]string, error) {
	if readyPath == "" {
		return nil, errors.New("hotswap: ready file path must not be empty")
	}
	out := make([]string, 0, len(base)+len(sess)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, EnvPrefix) {
			continue
		}
		if i := indexOfKey(out, kv); i >= 0 {
			out[i] = kv // 同键后值覆盖前值
			continue
		}
		out = append(out, kv)
	}
	seen := make(map[string]bool, len(sess))
	for k, v := range sess {
		name, err := envName(k)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("hotswap: session keys collide on environment name %s%q: %w", EnvPrefix, name, ErrInvalidSessionKey)
		}
		seen[name] = true
		out = append(out, EnvPrefix+name+"="+v)
	}
	out = append(out, EnvReadyPath+"="+readyPath)
	return out, nil
}

// ParseEnv 是 BuildEnv 的对称解析,供继任侧使用:抽出会话参数与就绪文件
// 路径。adopted 报告这是否一次领养启动(环境里存在任一 SSHMGR_HOTSWAP_*
// 变量即为真);就绪键按大小写不敏感识别(序列化侧恒为大写,Windows
// 环境变量本身大小写不敏感)。
func ParseEnv(env []string) (sess Session, readyPath string, adopted bool) {
	sess = Session{}
	for _, kv := range env {
		if !strings.HasPrefix(kv, EnvPrefix) {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name := kv[len(EnvPrefix):eq]
		if name == "" {
			continue
		}
		adopted = true
		val := kv[eq+1:]
		if strings.EqualFold(name, readyEnvName) {
			readyPath = val
			continue
		}
		sess[strings.ToLower(name)] = val
	}
	return sess, readyPath, adopted
}

// envName 校验 Session 键并归一为环境变量名(大写);保留键 READY 拒绝。
func envName(k string) (string, error) {
	if k == "" {
		return "", fmt.Errorf("hotswap: session key is empty: %w", ErrInvalidSessionKey)
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return "", fmt.Errorf("hotswap: session key %q: only letters, digits and underscores are allowed: %w", k, ErrInvalidSessionKey)
		}
	}
	up := strings.ToUpper(k)
	if up == readyEnvName {
		return "", fmt.Errorf("hotswap: session key %q is reserved for the ready-file path: %w", k, ErrInvalidSessionKey)
	}
	return up, nil
}

// indexOfKey 返回 entries 里键与 kv 相同(精确匹配)的下标,无则 -1。
func indexOfKey(entries []string, kv string) int {
	k := keyOf(kv)
	if k == "" {
		return -1
	}
	for i, e := range entries {
		if keyOf(e) == k {
			return i
		}
	}
	return -1
}

// keyOf 取环境变量条目的键(等号前的部分;无等号的条目整体视作键)。
func keyOf(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}
