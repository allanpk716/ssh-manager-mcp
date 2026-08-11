package store

import (
	"fmt"
)

// Snapshot is a portable, master-key-independent capture of the entire vault.
// Credential Secret/Passphrase hold DECRYPTED plaintext; the serialized form
// MUST be encrypted (vaultio.Encrypt) before touching disk. Version = format version.
type Snapshot struct {
	Version     int                  `json:"version"`
	Credentials []SnapshotCredential `json:"credentials"`
	Servers     []SnapshotServer     `json:"servers"`
	Profiles    []SnapshotProfile    `json:"profiles"`
	Grants      []SnapshotGrant      `json:"grants"`
	Projects    []SnapshotProject    `json:"projects"`
	HostKeys    []SnapshotHostKey    `json:"host_keys"`
	Audit       []SnapshotAudit      `json:"audit"`
}

type SnapshotCredential struct {
	ID         string `json:"id"`
	Type       string `json:"type"`       // models.CredentialType string value
	Secret     []byte `json:"secret"`     // DECRYPTED plaintext
	Passphrase []byte `json:"passphrase"` // DECRYPTED plaintext; nil/empty if none
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

type SnapshotServer struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	User             string `json:"user"`
	AuthMethod       string `json:"auth_method"`
	CredentialID     string `json:"credential_id"`
	SudoCredentialID string `json:"sudo_credential_id"` // "" if none (NULL coalesced)
	TagsRaw          string `json:"tags"`               // raw DB TEXT (JSON array string) — preserved verbatim
	Description      string `json:"description"`
	Location         string `json:"location"`
	Hardware         string `json:"hardware"`
	Services         string `json:"services"`
	Role             string `json:"role"`
	Caveats          string `json:"caveats"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

type SnapshotProfile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type SnapshotGrant struct {
	ProfileID string `json:"profile_id"`
	ServerID  string `json:"server_id"`
}

type SnapshotProject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TokenHash   []byte `json:"token_hash"` // verbatim — preserves original-token validity
	TokenSalt   []byte `json:"token_salt"`
	TokenPrefix string `json:"token_prefix"`
	ProfileID   string `json:"profile_id"`
	Status      string `json:"status"` // models.ProjectStatus string value
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type SnapshotHostKey struct {
	HostPort  string `json:"host_port"` // "{host}:{port}"
	KeyBlob   []byte `json:"key_blob"`
	CreatedAt int64  `json:"created_at"`
}

type SnapshotAudit struct {
	ID         int64  `json:"id"`
	TS         int64  `json:"ts"`
	ProjectID  string `json:"project_id"`
	ServerID   string `json:"server_id"`
	Action     string `json:"action"`
	Command    string `json:"command"`
	Sudo       bool   `json:"sudo"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

// ListHostKeys returns every host_keys row (point lookups already exist; this is the dump path).
func (s *Store) ListHostKeys() ([]SnapshotHostKey, error) {
	rows, err := s.db.Query(`SELECT host_port, key_blob, created_at FROM host_keys ORDER BY host_port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotHostKey
	for rows.Next() {
		var h SnapshotHostKey
		if err := rows.Scan(&h.HostPort, &h.KeyBlob, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ExportSnapshot captures every table. Credentials are DECRYPTED under s.masterKey.
// Requires the store to be unlocked. The caller must encrypt the serialized form.
func (s *Store) ExportSnapshot() (*Snapshot, error) {
	snap := &Snapshot{Version: 1}

	// servers (COALESCE the two nullable text cols to '')
	rs, err := s.db.Query(`SELECT id,name,host,port,user,auth_method,credential_id,COALESCE(sudo_credential_id,''),COALESCE(tags,''),description,location,hardware,services,role,caveats,created_at,updated_at FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var sv SnapshotServer
		if err := rs.Scan(&sv.ID, &sv.Name, &sv.Host, &sv.Port, &sv.User, &sv.AuthMethod,
			&sv.CredentialID, &sv.SudoCredentialID, &sv.TagsRaw, &sv.Description, &sv.Location,
			&sv.Hardware, &sv.Services, &sv.Role, &sv.Caveats, &sv.CreatedAt, &sv.UpdatedAt); err != nil {
			rs.Close()
			return nil, err
		}
		snap.Servers = append(snap.Servers, sv)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}

	// credentials (decrypt each blob under s.masterKey)
	rc, err := s.db.Query(`SELECT id,type,secret_blob,COALESCE(passphrase_blob,''),created_at,updated_at FROM credentials`)
	if err != nil {
		return nil, err
	}
	for rc.Next() {
		var c SnapshotCredential
		var secretBlob, passBlob []byte
		if err := rc.Scan(&c.ID, &c.Type, &secretBlob, &passBlob, &c.CreatedAt, &c.UpdatedAt); err != nil {
			rc.Close()
			return nil, err
		}
		pt, err := open(s.masterKey, secretBlob)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("decrypt credential %s: %w", c.ID, err)
		}
		c.Secret = pt
		if len(passBlob) > 0 {
			pp, err := open(s.masterKey, passBlob)
			if err != nil {
				rc.Close()
				return nil, fmt.Errorf("decrypt passphrase %s: %w", c.ID, err)
			}
			c.Passphrase = pp
		}
		snap.Credentials = append(snap.Credentials, c)
	}
	rc.Close()
	if err := rc.Err(); err != nil {
		return nil, err
	}

	// profiles
	rp, err := s.db.Query(`SELECT id,name,created_at,updated_at FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rp.Next() {
		var p SnapshotProfile
		if err := rp.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rp.Close()
			return nil, err
		}
		snap.Profiles = append(snap.Profiles, p)
	}
	rp.Close()
	if err := rp.Err(); err != nil {
		return nil, err
	}

	// grants (profile_servers)
	rg, err := s.db.Query(`SELECT profile_id, server_id FROM profile_servers`)
	if err != nil {
		return nil, err
	}
	for rg.Next() {
		var g SnapshotGrant
		if err := rg.Scan(&g.ProfileID, &g.ServerID); err != nil {
			rg.Close()
			return nil, err
		}
		snap.Grants = append(snap.Grants, g)
	}
	rg.Close()
	if err := rg.Err(); err != nil {
		return nil, err
	}

	// projects — RAW SQL for token_hash/salt (ListProjects/GetProject omit them)
	rj, err := s.db.Query(`SELECT id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at FROM projects`)
	if err != nil {
		return nil, err
	}
	for rj.Next() {
		var p SnapshotProject
		if err := rj.Scan(&p.ID, &p.Name, &p.TokenHash, &p.TokenSalt, &p.TokenPrefix, &p.ProfileID, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rj.Close()
			return nil, err
		}
		snap.Projects = append(snap.Projects, p)
	}
	rj.Close()
	if err := rj.Err(); err != nil {
		return nil, err
	}

	// host_keys
	snap.HostKeys, err = s.ListHostKeys()
	if err != nil {
		return nil, err
	}

	// audit — RAW SQL (AuditRows clamps limit<=0 to 1; dump all)
	ra, err := s.db.Query(`SELECT id,ts,COALESCE(project_id,''),COALESCE(server_id,''),action,COALESCE(command,''),sudo,COALESCE(status,''),COALESCE(exit_code,0),COALESCE(duration_ms,0) FROM audit_log ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for ra.Next() {
		var a SnapshotAudit
		if err := ra.Scan(&a.ID, &a.TS, &a.ProjectID, &a.ServerID, &a.Action, &a.Command, &a.Sudo, &a.Status, &a.ExitCode, &a.DurationMS); err != nil {
			ra.Close()
			return nil, err
		}
		snap.Audit = append(snap.Audit, a)
	}
	ra.Close()
	if err := ra.Err(); err != nil {
		return nil, err
	}

	return snap, nil
}
