// Package pairing 实现 SAS 配对协议的纯加密原语(零 IO)。
//
// 冻结契约(Plan 42 spec):域分隔串 "sshmgr-pair-v2" / "sshmgr-sas-v2"、
// 回退递推串 "again"、拒采阈值 4,294,000,000(⌊2³²/10⁶⌋×10⁶);
// HKDF-SHA256 info="sshmgr-pair-v2"、salt=SHA256(transcript)、L=64,
// 拆 K_ack=[0:32] / K_creds=[32:64]。两侧实现不得漂移。
package pairing

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// TranscriptParts 把各段按 4-byte 小端长度前缀 + 内容拼接成 transcript 字节串。
func TranscriptParts(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		var l [4]byte
		binary.LittleEndian.PutUint32(l[:], uint32(len(p)))
		b.Write(l[:])
		b.Write(p)
	}
	return b.Bytes()
}

// DeriveKeys 以 ikm 为输入密钥材料,salt=SHA256(transcript)、
// info="sshmgr-pair-v2" 做 HKDF-SHA256 展开 64 字节,
// 前半为 K_ack(确认通道),后半为 K_creds(凭据通道)。
func DeriveKeys(ikm, transcript []byte) (kAck, kCreds [32]byte) {
	salt := sha256.Sum256(transcript)
	var master [64]byte
	rdr := hkdf.New(sha256.New, ikm, salt[:], []byte("sshmgr-pair-v2"))
	if _, err := io.ReadFull(rdr, master[:]); err != nil {
		panic(err)
	}
	copy(kAck[:], master[:32])
	copy(kCreds[:], master[32:])
	return
}

// sasRejectBelow 是 SAS 拒绝采样阈值:⌊2³²/10⁶⌋×10⁶,保证 6 位取模无偏。
const sasRejectBelow = 4_294_000_000

// SAS 从 transcript 与 kCreds(K_master 代表,冻结取 64B 的后 32B)
// 派生 6 位零填充十进制短认证串。
func SAS(transcript []byte, kCreds [32]byte) string {
	input := append(append([]byte("sshmgr-sas-v2"), transcript...), kCreds[:]...)
	return sasFromR(sha256.Sum256(input))
}

// sasFromR 对 32B 随机串 R(8 个 BigEndian 4 字节块)做拒绝采样:
// 取首个 <sasRejectBelow 的块模 10⁶ 零填充;8 块全拒(概率≈6.6e-30)时
// 以 SHA256(r‖"again") 递推重入。
func sasFromR(r [32]byte) string {
	for {
		for i := 0; i+4 <= len(r); i += 4 {
			if v := binary.BigEndian.Uint32(r[i : i+4]); v < sasRejectBelow {
				return fmt.Sprintf("%06d", v%1_000_000)
			}
		}
		r = sha256.Sum256(append(r[:], []byte("again")...))
	}
}

// FinishAck 计算 HMAC-SHA256(kAck, "finish"‖id) 作为配对完成确认标签。
func FinishAck(kAck [32]byte, id []byte) []byte {
	m := hmac.New(sha256.New, kAck[:])
	m.Write([]byte("finish"))
	m.Write(id)
	return m.Sum(nil)
}

// SealCreds 用 K_creds 做 AES-256-GCM 加密,随机 12B nonce 前置随文,
// 返回 nonce‖ciphertext‖tag。
func SealCreds(kCreds [32]byte, pt []byte) ([]byte, error) {
	c, err := aes.NewCipher(kCreds[:])
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(c)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, pt, nil), nil
}

// OpenCreds 解 SealCreds 的输出(nonce 前置 12B);错钥/密文损坏返回错误。
func OpenCreds(kCreds [32]byte, sealed []byte) ([]byte, error) {
	c, err := aes.NewCipher(kCreds[:])
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(c)
	if err != nil {
		return nil, err
	}
	ns := g.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("pairing: sealed data shorter than nonce")
	}
	return g.Open(nil, sealed[:ns], sealed[ns:], nil)
}
