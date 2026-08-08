package models

import "time"

type AuthMethod string

const (
	AuthPassword   AuthMethod = "password"
	AuthPrivateKey AuthMethod = "private_key"
)

type CredentialType string

const (
	CredPassword   CredentialType = "password"
	CredPrivateKey CredentialType = "private_key"
)

// Server is an SSH target. Credential holds the login secret; SudoCredential (optional) holds a password for sudo -S.
type Server struct {
	ID               string
	Name             string
	Host             string
	Port             int
	User             string
	AuthMethod       AuthMethod
	CredentialID     string
	SudoCredentialID string // empty if none
	Tags             []string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Credential stores an encrypted secret. Secret and Passphrase are decrypted only in memory by the store.
type Credential struct {
	ID         string
	Type       CredentialType
	Secret     []byte // plaintext, only after store decrypts
	Passphrase []byte // plaintext, only for private_key; nil otherwise
}

type Profile struct {
	ID        string
	Name      string
	ServerIDs []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Project is an agent identity. TokenHash/Salt verify the presented token; ProfileID scopes visible servers.
type Project struct {
	ID          string
	Name        string
	TokenHash   []byte
	TokenSalt   []byte
	TokenPrefix string
	ProfileID   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
