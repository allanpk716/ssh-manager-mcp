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

// ShellType 标注在服务器条目上的 shell 类型,决定命令策略按哪种语法
// 拆解该服务器上执行的命令(POSIX / PowerShell / cmd,默认 POSIX)。它是
// 连接信息的一员——不属于服务器元数据,agent 的元数据编辑工具摸不到它;
// 取值与 internal/policy 的 ShellType 字面值一一对应。
type ShellType string

const (
	ShellPOSIX      ShellType = "posix"      // Linux 与 macOS 的 shell(bash / sh / zsh 等)
	ShellPowerShell ShellType = "powershell" // Windows PowerShell
	ShellWindowsCmd ShellType = "cmd"        // Windows cmd
)

// NormalizeShellType 把存储或快照里的 shell 类型值归一化:空值(旧快照、
// 迁移前的旧行、未设置的字段)按默认 POSIX;其余值原样返回——无法识别
// 的值不改写,留给策略判定内核走保守分支(需确认),不悄悄放行。
func NormalizeShellType(s string) ShellType {
	if s == "" {
		return ShellPOSIX
	}
	return ShellType(s)
}

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
	ID               string
	Name             string
	Host             string
	Port             int
	User             string
	AuthMethod       AuthMethod
	CredentialID     string
	SudoCredentialID string // empty if none
	// ShellType (命令策略) 标注该服务器命令按哪种语法拆解;连接信息的一
	// 员,随快照下发,默认 POSIX(空值经 NormalizeShellType 归一)。
	ShellType   ShellType
	Tags        []string
	Description string // owner free-text notes; surfaced to agent (supplementary to structured fields below)
	Location    string // where deployed: datacenter/region/rack/tenant
	Hardware    string // hardware config: CPU/RAM/disk/GPU
	Services    string // what is deployed/running here
	Role        string // this server's purpose (e.g. "prod pg primary")
	Caveats     string // operational gotchas; agent reads before acting
	// ExposeHost (Plan 31): owner opt-in to return the plaintext host in
	// list_servers. Default false = the projection masks it as "hidden".
	// Never affects the broker's own dialing — the vault always stores the
	// real host.
	ExposeHost bool
	// Revision (Plan 51) is the monotonic optimistic-lock token for the six
	// metadata fields above: every UPDATE servers path bumps it, and a
	// client-side metadata edit (POST /server-metadata) must echo the value it
	// read via list_servers — a mismatch is a 409 carrying the current row.
	// 0 = never written (legacy rows back-fill to 0).
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
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
// PairGenerated (Plan 42) marks identities minted BY the SAS pairing flow as
// "pair-<name>" — only such rows are reused (token-rotated) at re-pair; owner-created
// same-name projects are never hijacked by the pairing flow.
type Project struct {
	ID            string
	Name          string
	TokenHash     []byte
	TokenSalt     []byte
	TokenPrefix   string
	ProfileID     string
	Status        ProjectStatus
	PairGenerated bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CacheTokenStatus is the lifecycle state of a device-auth-code (for offline cache pull).
// Only active admits its token at VerifyCacheToken. Lazy: status takes effect on the next pull.
type CacheTokenStatus string

const (
	CacheTokenActive  CacheTokenStatus = "active"  // default; token admitted for /snapshot
	CacheTokenRevoked CacheTokenStatus = "revoked" // permanent; token rejected (device lost/rotated)
)

// CacheToken is a per-device authorization code for offline-cache pulls, BOUND to
// exactly one profile (Plan 39): /snapshot serves only that profile's authorized
// servers + their credentials. Disjoint from project tokens, and NEVER carried in
// a Snapshot (server-side only). TokenHash/Salt verify the presented code; the
// plaintext is shown once at AddCacheToken and never stored. LastPullAt is zero
// until the device's first successful pull.
type CacheToken struct {
	ID          string
	Name        string
	TokenPrefix string
	Status      CacheTokenStatus
	// ProfileID is the binding that scopes /snapshot for this device. Empty
	// ("unbound") exists ONLY as the pre-Plan-39 legacy-DB migration state —
	// serve refuses unbound pulls with 403 (not 401: a pinned 401 destroys the
	// client cache under Plan 34) until `cache-tokens bind` repairs it.
	ProfileID  string
	LastPullAt time.Time // zero value if last_pull_at was NULL (never pulled)
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
