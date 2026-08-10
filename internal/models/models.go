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

// ProjectStatus is the lifecycle state of an agent project. Only an active project
// admits its token at VerifyToken (next mcp spawn). Lazy: a running session is not
// live-killed; the status takes effect on the next mcp process spawn.
type ProjectStatus string

const (
	ProjectActive   ProjectStatus = "active"   // default; token admitted
	ProjectDisabled ProjectStatus = "disabled" // suspended; token rejected, reversible via enable
	ProjectRevoked  ProjectStatus = "revoked"  // permanent; token rejected, hidden from default ls
)

// AuthMethodForServer maps a credential type to the server's auth_method.
func (c CredentialType) AuthMethodForServer() AuthMethod {
	if c == CredPrivateKey {
		return AuthPrivateKey
	}
	return AuthPassword
}

// Server is an SSH target. Credential holds the login secret; SudoCredential (optional) holds a password for sudo -S.
// Description is free-text owner notes (hardware/purpose); it is owner-only — never surfaced to the agent.
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
	Description      string // owner notes; not exposed via MCP tools
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
// Status gates admission (only active admits); rotate replaces the token in place (same id/profile).
type Project struct {
	ID          string
	Name        string
	TokenHash   []byte
	TokenSalt   []byte
	TokenPrefix string
	ProfileID   string
	Status      ProjectStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
