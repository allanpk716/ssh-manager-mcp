package store

import (
	"os"
	"path/filepath"
	"strings"
)

// privateKeyFileNames is the set of default OpenSSH private-key basenames we warn about.
var privateKeyFileNames = map[string]bool{
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
}

// CheckResidualKeys scans ~/.ssh for default private-key files (best-effort).
// Returns the paths found. Errors (e.g. no ~/.ssh) return (nil, nil) — the check never blocks startup.
func CheckResidualKeys() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	return checkResidualKeysIn(filepath.Join(home, ".ssh"))
}

func checkResidualKeysIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // missing dir is not an error
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if privateKeyFileNames[name] {
			found = append(found, filepath.Join(dir, name))
			continue
		}
		// also catch "id_rsa" + ".pub" pattern edge or custom like "id_ed25519_github": skip .pub
		if strings.HasSuffix(name, ".pub") {
			continue
		}
		if strings.HasPrefix(name, "id_") {
			found = append(found, filepath.Join(dir, name))
		}
	}
	return found, nil
}
