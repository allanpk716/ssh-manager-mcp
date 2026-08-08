package sshbroker

import (
	"errors"

	"golang.org/x/crypto/ssh"
)

// PasswordAuth builds the SSH password auth method.
func PasswordAuth(pw string) ssh.AuthMethod {
	return ssh.Password(pw)
}

// PrivateKeyAuth builds a public-key auth method from a PEM private key.
// If the key is encrypted, passphrase must be supplied; it is ignored for unencrypted keys.
func PrivateKeyAuth(keyPEM []byte, passphrase []byte) (ssh.AuthMethod, error) {
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err == nil {
		return ssh.PublicKeys(signer), nil
	}
	var e *ssh.PassphraseMissingError
	if errors.As(err, &e) && len(passphrase) > 0 {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(keyPEM, passphrase)
		if err != nil {
			return nil, err
		}
		return ssh.PublicKeys(signer), nil
	}
	return nil, err
}
