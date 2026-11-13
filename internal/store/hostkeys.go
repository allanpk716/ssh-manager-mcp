package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Pin formats（锚的格式）。密钥字节格式存 SSH 公钥序列化字节；指纹格式存
// OpenSSH 的「SHA256:<base64>」指纹串本身。
const (
	PinFormatBlob        = "blob"
	PinFormatFingerprint = "fingerprint"
)

// Pin sources（锚的来源）。首次连接即信任、设备转发、owner 手工锚定。
const (
	PinSourceTofu    = "tofu"
	PinSourceForward = "forward"
	PinSourceManual  = "manual"
)

// Pin is a stored host-key anchor: its value bytes and format.
type Pin struct {
	Blob   []byte
	Format string
}

// Matches reports whether the presented marshaled host key satisfies this pin
// under the dual-mode semantics — the ONE equality definition for the whole
// system. blob anchors compare bytewise; fingerprint anchors compare the
// presented key's SHA256 fingerprint with the stored string. An unknown/empty
// format (possible only in a hand-edited DB) degrades to the blob comparison.
// A nil anchor matches nothing.
func (p *Pin) Matches(presented []byte) bool {
	if p == nil {
		return false
	}
	if p.Format == PinFormatFingerprint {
		pub, err := ssh.ParsePublicKey(presented)
		if err != nil {
			return false
		}
		return ssh.FingerprintSHA256(pub) == string(p.Blob)
	}
	return bytes.Equal(p.Blob, presented)
}

// ErrPinExists is UpsertManualEntryPin's refusal outcome: the entry already
// carries a pin and --force was not passed. The *PinMeta returned alongside IS
// the incumbent — the CLI shows its fingerprint and source and writes nothing.
var ErrPinExists = errors.New("pin already present")

// ErrEntryPinMismatch means a first-trust save lost a concurrent race to an
// already-stored anchor whose key differs from the presented key. The caller
// must reject that address as a host-key mismatch.
var ErrEntryPinMismatch = errors.New("entry host key pin mismatch")

// ParseFingerprintPin 校验 owner 输入的 OpenSSH 主机密钥指纹串：必须形如
// SHA256:<base64>，base64 用标准字母表、不带填充，且解码结果恰好是 32 字
// 节的 SHA-256 摘要。返回可入库的指纹串字节与展示串；命令行与 Web UI 共
// 用这一套判定，保证两个入口接受同一批指纹。
func ParseFingerprintPin(input string) ([]byte, string, error) {
	fingerprint := strings.TrimSpace(input)
	rest, ok := strings.CutPrefix(fingerprint, "SHA256:")
	if !ok {
		return nil, "", fmt.Errorf("fingerprint must be the OpenSSH form SHA256:<base64, unpadded> (got %q)", fingerprint)
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return nil, "", fmt.Errorf("fingerprint base64 is invalid (%v) — expected the unpadded standard-alphabet form ssh-keygen -lf prints", err)
	}
	if len(raw) != 32 {
		return nil, "", fmt.Errorf("fingerprint decodes to %d bytes, want exactly 32 (a SHA-256 digest) — is the string truncated or padded?", len(raw))
	}
	if base64.RawStdEncoding.EncodeToString(raw) != rest {
		return nil, "", fmt.Errorf("fingerprint is not in canonical SHA256:<base64, unpadded> form — re-check for stray characters")
	}
	return []byte(fingerprint), fingerprint, nil
}

// PinMeta is an anchor plus its provenance columns: everything the
// owner-facing surfaces — display, --list, refusal hints, audit rows — need
// beyond the compared value carried by the embedded Pin.
type PinMeta struct {
	Pin
	ServerID      string
	Source        string
	Device        string
	HandshakeAddr string
	CreatedAt     int64
}
