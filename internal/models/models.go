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
// Structured metadata fields (Location/Hardware/Services/Role/Caveats) plus Tags and Description are surfaced to
// the agent via list_servers (full-open, reversing Plan-8's owner-only rule — see
// docs/superpowers/specs/2026-08-11-server-structured-metadata-design.md).
type Server struct {
	ID                string
	Name              string
	Host              string
	Port              int
	User              string
	AuthMethod        AuthMethod
	CredentialID      string
	SudoCredentialID string // empty if none
	Tags              []string
	Description       string // owner free-text notes; surfaced to agent (supplementary to structured fields below)
	Location          string // where deployed: datacenter/region/rack/tenant
	Hardware          string // hardware config: CPU/RAM/disk/GPU
	Services          string // what is deployed/running here
	Role              string // this server's purpose (e.g. "prod pg primary")
	Caveats           string // operational gotchas; agent reads before acting
	CreatedAt         time.Time
	UpdatedAt         time.Time
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

// CacheTokenStatus is the lifecycle state of a device-auth-code (for offline cache pull).
// Only active admits its token at VerifyCacheToken. Lazy: status takes effect on the next pull.
type CacheTokenStatus string

const (
	CacheTokenActive  CacheTokenStatus = "active"  // default; token admitted for /snapshot
	CacheTokenRevoked CacheTokenStatus = "revoked" // permanent; token rejected (device lost/rotated)
)

// CacheToken is a per-device authorization code for offline-cache pulls. It is OWNER-level
// (not scoped to a profile), disjoint from project tokens, and NEVER carried in a Snapshot
// (server-side only). TokenHash/Salt verify the presented code; the plaintext is shown once
// at AddCacheToken and never stored. LastPullAt is zero until the device's first successful pull.
type CacheToken struct {
	ID          string
	Name        string
	TokenPrefix string
	Status      CacheTokenStatus
	LastPullAt  time.Time // zero value if last_pull_at was NULL (never pulled)
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
