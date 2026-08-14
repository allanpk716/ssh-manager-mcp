package tui

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestDeleteServer_Action(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	id, _ := st.AddServer(&models.Server{Name: "tmp", Host: "h", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err := st.DeleteServer(id); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.GetServerByName("tmp"); g != nil {
		t.Fatal("still present")
	}
}
