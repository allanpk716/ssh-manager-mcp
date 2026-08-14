package tui

import (
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

type Mode int

const (
	ModeBroker Mode = iota
	ModeClient
)

// vaultStorePath mirrors cli's storePath (env override > default) WITHOUT importing cli.
// Returns "" only when the default path itself is unresolvable (no vault could
// exist there anyway).
func vaultStorePath() string {
	if p := os.Getenv("SSHMGR_STORE"); p != "" {
		return p
	}
	p, err := store.DefaultStorePath()
	if err != nil {
		return ""
	}
	return p
}

// vaultExists reports whether a store.db file exists at the vault location.
// Stat-first so probing a machine with NO vault never triggers OpenStore's
// create-on-open side effect (a fresh empty store.db).
func vaultExists() bool {
	p := vaultStorePath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// vaultUnlocked reports whether an UNLOCKED vault is reachable. A vault that
// EXISTS but cannot be opened (locked / key unreadable) is distinguished by the
// caller via vaultExists so detection never silently degrades a locked broker
// machine into client mode (spec §6).
func vaultUnlocked() bool {
	if !vaultExists() {
		return false
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return false
	}
	st.Close()
	return true
}

// cachePresent reports whether this machine is an enrolled client.
func cachePresent() bool {
	cred, err := clientops.ReadCacheCred()
	return err == nil && cred != nil
}

// DetectModeWith resolves the run mode: force flag wins, else vault→broker,
// else cache→client, else a guided error.
func DetectModeWith(force string, hasVault, hasCache func() bool) (Mode, error) {
	switch force {
	case "broker":
		return ModeBroker, nil
	case "client":
		return ModeClient, nil
	case "":
	default:
		return 0, fmt.Errorf("invalid --mode %q (want broker|client)", force)
	}
	if hasVault() {
		return ModeBroker, nil
	}
	if hasCache() {
		return ModeClient, nil
	}
	return 0, errors.New("no vault and no cache on this machine: initialize a vault here (broker) or run `cache pull` (client) first")
}

func DetectMode(force string) (Mode, error) {
	switch force {
	case "broker", "client", "":
	default:
		return 0, fmt.Errorf("invalid --mode %q (want broker|client)", force)
	}
	if vaultExists() && !vaultUnlocked() {
		return 0, errors.New("本机 vault 存在但锁定或不可读：先运行 `ssh-manager unlock`（不会降级为 client 模式）")
	}
	return DetectModeWith(force, vaultUnlocked, cachePresent)
}

// Run starts the console for mode. Broker opens the vault and runs the tabbed
// app; client keeps a placeholder until Task 8.
func Run(mode Mode) error {
	if !isTTY() {
		return errors.New("tui requires a terminal (in mintty run via `winpty ssh-manager tui`, or use Windows Terminal)")
	}
	if mode == ModeClient {
		p := tea.NewProgram(clientPlaceholder{})
		_, err := p.Run()
		return err
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		return err
	}
	defer st.Close()
	app, err := NewBrokerApp(st)
	if err != nil {
		return err
	}
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
