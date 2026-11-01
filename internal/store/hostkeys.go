package store

import (
	"bytes"
	"errors"

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
