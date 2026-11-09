package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestSecretEnvironmentValuesForInjectionReportsPerVariableReadFailure(t *testing.T) {
	s := newTestStore(t)
	serverID, err := s.AddServer(&models.Server{Name: "target", Host: "192.0.2.10", Port: 22, User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecretWithEnvironmentName("GOOD_SECRET", "", []byte("good-secret-value"), "GOOD_TOKEN", CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecretWithEnvironmentName("BAD_SECRET", "", []byte("bad-secret-value"), "BAD_TOKEN", CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"GOOD_SECRET", "BAD_SECRET"}, []string{"target"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE secrets SET value_blob=? WHERE name=?`, []byte("not-encrypted"), "BAD_SECRET"); err != nil {
		t.Fatal(err)
	}

	values, failures, err := s.SecretEnvironmentValuesForInjection()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].EnvironmentName != "GOOD_TOKEN" || string(values[0].Value) != "good-secret-value" {
		t.Fatalf("values = %#v, want only GOOD_TOKEN", values)
	}
	if len(values[0].WritableServerIDs) != 1 || values[0].WritableServerIDs[0] != serverID {
		t.Fatalf("writable server ids = %#v, want %q", values[0].WritableServerIDs, serverID)
	}
	if len(failures) != 1 || failures[0].EnvironmentName != "BAD_TOKEN" {
		t.Fatalf("failures = %#v, want BAD_TOKEN", failures)
	}
	if len(failures[0].WritableServerIDs) != 1 || failures[0].WritableServerIDs[0] != serverID {
		t.Fatalf("failure writable server ids = %#v, want %q", failures[0].WritableServerIDs, serverID)
	}
}
