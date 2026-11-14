package sshbroker

import (
	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/store"
)

// hostKeyAlgorithmsForPin 把锚记录的算法转换成 SSH 客户端可用的主机密钥
// 算法列表。证书算法不在本次支持范围内；未知算法不施加约束，仍由指纹比
// 对守住安全边界。
func hostKeyAlgorithmsForPin(pin *store.Pin) []string {
	if pin == nil {
		return nil
	}
	algorithm := pin.EffectiveKeyAlgorithm()
	switch algorithm {
	case ssh.KeyAlgoED25519:
		return []string{ssh.KeyAlgoED25519}
	case "ecdsa":
		return []string{ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521}
	case ssh.KeyAlgoECDSA256:
		return []string{ssh.KeyAlgoECDSA256}
	case ssh.KeyAlgoECDSA384:
		return []string{ssh.KeyAlgoECDSA384}
	case ssh.KeyAlgoECDSA521:
		return []string{ssh.KeyAlgoECDSA521}
	case "rsa", ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	default:
		return nil
	}
}

// hostKeyAlgorithmFamilyAttempts 是无算法指纹锚的探测顺序。每个元素限定一
// 个算法族；任何一次呈现密钥的指纹与锚完全一致即接受。
func hostKeyAlgorithmFamilyAttempts() [][]string {
	return [][]string{
		{ssh.KeyAlgoED25519},
		{ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521},
		{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA},
	}
}
