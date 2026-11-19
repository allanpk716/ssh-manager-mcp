package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func clientTestServer(t *testing.T, st *Store, name string) string {
	t.Helper()
	server, err := st.AddServer(&models.Server{Name: name, Host: "192.0.2.10", AuthMethod: models.AuthPassword})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestChangeClientProfileMovesPairedAccessIdentity(t *testing.T) {
	st := newTestStore(t)
	web := clientTestServer(t, st, "web")
	db := clientTestServer(t, st, "db")
	oldProfile, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	newProfile, err := st.AddProfile("数据库")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(oldProfile, []string{web}); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(newProfile, []string{db}); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	deviceCode, _, err := st.MintPairingCredentials(tx, "laptop-client", oldProfile, false)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tokens, err := st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 {
		t.Fatalf("客户端实例数量 = %d，应为 1", len(tokens))
	}

	change, err := st.ChangeClientProfile(tokens[0].ID, oldProfile, newProfile)
	if err != nil {
		t.Fatal(err)
	}
	if change.ClientName != "laptop-client" || change.OldProfileID != oldProfile || change.NewProfileID != newProfile || !change.PairedProject {
		t.Fatalf("更换结果不正确：%+v", change)
	}
	tokens, err = st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].ProfileID != newProfile {
		t.Fatalf("客户端绑定 profile = %q，应为 %q", tokens[0].ProfileID, newProfile)
	}
	project, err := st.GetProjectByName("pair-laptop-client")
	if err != nil {
		t.Fatal(err)
	}
	if project.ProfileID != newProfile || !project.PairGenerated {
		t.Fatalf("配对生成的访问身份没有同步换绑：%+v", project)
	}
	_ = deviceCode
}

func TestRemoveClientRevokesCredentialsAndClearsMachine(t *testing.T) {
	st := newTestStore(t)
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.MintPairingCredentials(tx, "laptop-client", profileID, false); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "", "laptop-client")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}

	removal, err := st.RemoveClient(tokens[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if removal.ClientName != "laptop-client" || removal.MachineName != "工作站" || !removal.RevokedDeviceCode || !removal.RevokedPairProject {
		t.Fatalf("移除结果不正确：%+v", removal)
	}
	tokens, err = st.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].Status != models.CacheTokenRevoked {
		t.Fatalf("移除后客户端凭据状态 = %q", tokens[0].Status)
	}
	project, err := st.GetProjectByName("pair-laptop-client")
	if err != nil {
		t.Fatal(err)
	}
	if project.Status != models.ProjectRevoked {
		t.Fatalf("移除后配对生成的访问身份状态 = %q", project.Status)
	}
	machine, err := st.GetDevice(device.Name)
	if err != nil {
		t.Fatal(err)
	}
	if machine.ClientTokenID != "" || machine.ClientName != "" {
		t.Fatalf("移除后机器不应残留客户端关联：%+v", machine)
	}
}
