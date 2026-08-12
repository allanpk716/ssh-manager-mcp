package mcpserver

import (
	"bytes"
	"encoding/json"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestListServersSurfacesMetadata verifies the structured fields + Tags + Description
// are projected into ServerInfo (and thus into the agent's list_servers payload).
// Lightweight: no sshd needed — ListServersForProfile only reads the store.
func TestListServersSurfacesMetadata(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	sid, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "10.0.0.5", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		Role: "prod ml", Services: "jupyter, trainer",
		Caveats:  "do not reboot 02-03:00",
		Location: "dc2 rack14", Hardware: "8x A100",
		Tags: []string{"gpu"}, Description: "owner notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := st.AddProfile("p")
	if err := st.GrantServers(pid, []string{sid}); err != nil {
		t.Fatal(err)
	}

	infos, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("len = %d, want 1", len(infos))
	}
	info := infos[0]
	if info.Role != "prod ml" || info.Services != "jupyter, trainer" ||
		info.Caveats != "do not reboot 02-03:00" || info.Location != "dc2 rack14" ||
		info.Hardware != "8x A100" || info.Description != "owner notes" {
		t.Fatalf("ServerInfo missing structured fields: %+v", info)
	}
	if len(info.Tags) != 1 || info.Tags[0] != "gpu" {
		t.Fatalf("Tags = %v", info.Tags)
	}

	// snake_case JSON keys reach the agent.
	b, _ := json.Marshal(info)
	for _, key := range []string{`"role"`, `"services"`, `"caveats"`, `"location"`, `"hardware"`, `"tags"`, `"description"`} {
		if !bytes.Contains(b, []byte(key)) {
			t.Fatalf("JSON missing %s: %s", key, b)
		}
	}
}

// TestListServersHidesOutOfProfileMetadata: a server NOT granted to the profile must
// not appear at all (iron rule intact — fields ride the existing profile projection).
func TestListServersHidesOutOfProfileMetadata(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	inID, _ := st.AddServer(&models.Server{
		Name: "in", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Role: "visible", Caveats: "in-caveat",
	})
	_, _ = st.AddServer(&models.Server{
		Name: "out", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Role: "hidden", Caveats: "out-caveat",
	})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{inID})

	infos, _ := ListServersForProfile(st, pid)
	if len(infos) != 1 || infos[0].Name != "in" {
		t.Fatalf("only the granted server should appear: %+v", infos)
	}
	if infos[0].Role != "visible" {
		t.Fatalf("granted server's fields missing: %+v", infos[0])
	}
}

// TestListServersCoalescesNilTags pins the nil-Tags → []string{} coalesce in
// ListServersForProfile. A server created with NO Tags must surface to the agent
// as `"tags":[]` (not `"tags":null`): the MCP SDK validates the marshaled output
// JSON against the generated jsonschema, and a nil slice marshals to `null`,
// which fails the `"type":"array"` constraint and breaks list_servers end-to-end.
// This test guards the coalesce against a future refactor silently deleting it
// (and validates the in-code comment's claim).
func TestListServersCoalescesNilTags(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	// NO Tags set — srv.Tags is nil.
	sid, err := st.AddServer(&models.Server{
		Name: "bare", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := st.AddProfile("p")
	if err := st.GrantServers(pid, []string{sid}); err != nil {
		t.Fatal(err)
	}

	infos, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("len = %d, want 1", len(infos))
	}

	// The projected ServerInfo must carry a non-nil Tags slice so its JSON
	// encoding is `[]`, never `null`.
	b, err := json.Marshal(infos[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"tags":[]`)) {
		t.Fatalf("nil-Tags server must marshal to tags:[]; got %s", b)
	}
	if bytes.Contains(b, []byte(`"tags":null`)) {
		t.Fatalf("nil-Tags server must NOT marshal to tags:null; got %s", b)
	}
}
