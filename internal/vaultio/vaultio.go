// Package vaultio is the portable, passphrase-encrypted envelope for vault
// exports (and, later, Synology backups / client-cache pulls). It is deliberately
// independent of internal/store: callers hand it plaintext bytes + a passphrase,
// get back magic‖salt‖nonce‖ciphertext. The key is Argon2id(passphrase, salt)
// with the SAME parameters the vault's own passphrase mode uses
// (internal/store/masterkey.go DeriveFromPassphrase: time=1, memory=64MiB,
// parallelism=4, 32-byte out), so a uniform cost is applied across the project.
package vaultio

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"

	"golang.org/x/crypto/argon2"
)

var (
	magic = []byte("SSHMGRV1") // 8 bytes — format identifier + future versioning

	// ErrBadMagic: blob does not start with the expected magic header.
	ErrBadMagic = errors.New("vaultio: bad magic (not an ssh-manager export)")
	// ErrTruncated: blob too short to contain magic+salt+nonce+tag.
	ErrTruncated = errors.New("vaultio: truncated blob")
)

const (
	saltLen   = 16
	nonceLen  = 12 // AES-GCM standard nonce
	keyLen    = 32 // AES-256
	argonTime = 1
	argonMem  = 64 * 1024 // KiB → 64 MiB
	argonPar  = 4
)

// Encrypt derives a key from passphrase+random-salt (Argon2id), AES-256-GCM-seals
// plaintext, and returns magic‖salt‖nonce‖ciphertext.
func Encrypt(passphrase, plaintext []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return sealWithSalt(passphrase, salt, plaintext)
}

// sealWithSalt is split out so tests can fix the salt for determinism if needed.
func sealWithSalt(passphrase, salt, plaintext []byte) ([]byte, error) {
	key := argon2.IDKey(passphrase, salt, argonTime, argonMem, argonPar, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(magic)+saltLen+nonceLen+len(ct))
	out = append(out, magic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Decrypt parses magic‖salt‖nonce‖ciphertext, re-derives the key, and AES-GCM-opens.
// Wrong passphrase or any tampering → a non-nil error (GCM authentication failure).
func Decrypt(passphrase, blob []byte) ([]byte, error) {
	minLen := len(magic) + saltLen + nonceLen // + at least the 16-byte GCM tag, checked implicitly by Open
	if len(blob) < minLen {
		return nil, ErrTruncated
	}
	if !bytes.Equal(blob[:len(magic)], magic) {
		return nil, ErrBadMagic
	}
	off := len(magic)
	salt := blob[off : off+saltLen]
	off += saltLen
	nonce := blob[off : off+nonceLen]
	off += nonceLen
	ct := blob[off:]

	key := argon2.IDKey(passphrase, salt, argonTime, argonMem, argonPar, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err // wrong passphrase OR tamper — do not distinguish (no oracle)
	}
	return pt, nil
}
