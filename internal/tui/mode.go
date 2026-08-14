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

// vaultPresent reports whether an UNLOCKED vault is reachable on this machine.
// A locked vault is deliberately NOT treated as client-mode (spec §2): probing a
// locked store returns an error we distinguish from "absent".
func vaultPresent() bool {
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
	return DetectModeWith(force, vaultPresent, cachePresent)
}

// Run starts the console for mode (placeholder view until Task 3).
func Run(mode Mode) error {
	if !isTTY() {
		return errors.New("tui requires a terminal (in mintty run via `winpty ssh-manager tui`, or use Windows Terminal)")
	}
	p := tea.NewProgram(newApp(mode))
	_, err := p.Run()
	return err
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
