package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	infoCredential = "ssh-manager/v1/credential"
	saltLen        = 16
	nonceLen       = 12
	keyLen         = 32
)

// seal encrypts pt under masterKey with a fresh random salt+nonce.
// Output: salt(16) || nonce(12) || ciphertext.
func seal(masterKey, pt []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	gcm, err := newGCM(masterKey, salt)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, pt, nil)
	out := make([]byte, 0, saltLen+nonceLen+len(ct))
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// open decrypts a blob produced by seal.
func open(masterKey, blob []byte) ([]byte, error) {
	if len(blob) < saltLen+nonceLen {
		return nil, errors.New("ciphertext too short")
	}
	salt := blob[:saltLen]
	nonce := blob[saltLen : saltLen+nonceLen]
	ct := blob[saltLen+nonceLen:]
	gcm, err := newGCM(masterKey, salt)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM(masterKey, salt []byte) (cipher.AEAD, error) {
	dek, err := deriveKey(masterKey, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func deriveKey(masterKey, salt []byte) ([]byte, error) {
	k := make([]byte, keyLen)
	r := hkdf.New(sha256.New, masterKey, salt, []byte(infoCredential))
	if _, err := io.ReadFull(r, k); err != nil {
		return nil, err
	}
	return k, nil
}
