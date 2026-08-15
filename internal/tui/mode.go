package tui

import (
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

type Mode int

const (
	ModeBroker Mode = iota
	ModeClient
)

// The vault-probe helpers (vaultStorePath / vaultExists / vaultUnlocked) MOVED
// to internal/roles (roles.VaultExists / roles.VaultUnlocked + private
// vaultStorePath) — Plan 19 T1 makes roles the single launch-resolution
// authority. The unexported wrappers below keep tui's own call sites and
// mode_test.go's probe tests working unchanged; behavior is identical
// (stat-first probe, no OpenStore create-on-open side effect).

// vaultExists reports whether a store.db file exists at the vault location.
func vaultExists() bool { return roles.VaultExists() }

// vaultUnlocked reports whether an UNLOCKED vault is reachable.
func vaultUnlocked() bool { return roles.VaultUnlocked() }

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
// App; client runs the standalone clientModel (single screen, no tabs).
func Run(mode Mode) error {
	if !isTTY() {
		return errors.New("tui requires a terminal (in mintty run via `winpty ssh-manager tui`, or use Windows Terminal)")
	}
	if mode == ModeClient {
		p := tea.NewProgram(newClientModel())
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
