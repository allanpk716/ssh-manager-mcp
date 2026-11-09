package store

import "testing"

func TestSecretValuesForMaskingFailsWhenValueCannotBeDecrypted(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddSecret("GOOD_SECRET", "", []byte("good-masking-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("BAD_SECRET", "", []byte("bad-masking-secret-value")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE secrets SET value_blob=? WHERE name=?`, []byte("not-encrypted"), "BAD_SECRET"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SecretValuesForMasking(); err == nil {
		t.Fatal("undecryptable secret value must make the masking value set fail instead of being skipped")
	}
}
