// Package knownhosts parses and renders OpenSSH known_hosts lines. It exists
// as a neutral, testing-free package because production code depends on it
// (the servers pin-hostkey --from-keyscan path): these functions used to live
// in internal/conformance, whose docker/ssh helpers import "testing" — and a
// single production import dragged the testing package into the sshmgr
// binary (size + hygiene debt, Plan 48 final-review residual).
package knownhosts

import (
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ParseKnownHostsLine parses one OpenSSH known_hosts line:
//
//	patterns type base64-key
//
// For a non-default port the patterns field is the bracketed form "[host]:port".
// It returns the host patterns, the key-type string, and the parsed public key.
func ParseKnownHostsLine(line string) (patterns string, keyType string, key ssh.PublicKey, err error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", "", nil, errors.New("known_hosts line needs patterns, type, key")
	}
	patterns, keyType, b64 := fields[0], fields[1], fields[2]
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", "", nil, err
	}
	key, err = ssh.ParsePublicKey(blob)
	if err != nil {
		return "", "", nil, err
	}
	return patterns, keyType, key, nil
}

// FormatKnownHostsLine renders an OpenSSH known_hosts line for the given host
// patterns and key. For a non-default port, patterns must already be in the
// "[host]:port" form (callers construct it per OpenSSH convention).
func FormatKnownHostsLine(patterns string, key ssh.PublicKey) string {
	return patterns + " " + key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
}
