package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
)

func (s *Store) AddServer(srv *models.Server) (string, error) {
	id := newID()
	ts := now()
	tagsJSON, _ := json.Marshal(srv.Tags)
	sudo := nullableString(srv.SudoCredentialID)
	_, err := s.db.Exec(
		`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), srv.Description,
		srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, ts, ts,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// UpdateServer writes the full row (id-preserving). The caller loads the server, applies only
// the fields being edited, and writes it back — so re-credential is just setting a new
// CredentialID + AuthMethod. name is mutable (rename). Returns an error if the id is absent.
func (s *Store) UpdateServer(srv *models.Server) error {
	tagsJSON, _ := json.Marshal(srv.Tags)
	sudo := nullableString(srv.SudoCredentialID)
	res, err := s.db.Exec(
		`UPDATE servers SET name=?,host=?,port=?,user=?,auth_method=?,credential_id=?,sudo_credential_id=?,tags=?,description=?,location=?,hardware=?,services=?,role=?,caveats=?,updated_at=? WHERE id=?`,
		srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), srv.Description,
		srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, now(), srv.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("server id %q not found", srv.ID)
	}
	return nil
}

func (s *Store) GetServer(id string) (*models.Server, error) {
	row := s.db.QueryRow(
		`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at FROM servers WHERE id=?`, id,
	)
	return scanServer(row)
}

func (s *Store) GetServerByName(name string) (*models.Server, error) {
	row := s.db.QueryRow(
		`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at FROM servers WHERE name=?`, name,
	)
	srv, err := scanServer(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return srv, err
}

func (s *Store) ListServers() ([]*models.Server, error) {
	rows, err := s.db.Query(
		`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Server
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

func (s *Store) DeleteServer(id string) error {
	_, err := s.db.Exec(`DELETE FROM servers WHERE id=?`, id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanServer(sc scanner) (*models.Server, error) {
	var (
		srv              models.Server
		authMethod       string
		tagsJSON         string
		sudoCredentialID sql.NullString
		createdAt        int64
		updatedAt        int64
	)
	if err := sc.Scan(&srv.ID, &srv.Name, &srv.Host, &srv.Port, &srv.User, &authMethod, &srv.CredentialID, &sudoCredentialID, &tagsJSON, &srv.Description, &srv.Location, &srv.Hardware, &srv.Services, &srv.Role, &srv.Caveats, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	srv.AuthMethod = models.AuthMethod(authMethod)
	srv.SudoCredentialID = sudoCredentialID.String
	srv.CreatedAt = time.Unix(createdAt, 0)
	srv.UpdatedAt = time.Unix(updatedAt, 0)
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &srv.Tags)
	}
	return &srv, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
