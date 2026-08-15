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

// launchTarget is the pure dispatch table behind Run (Plan 19 T2): first run →
// wizard; completed setups → broker/client; an INCOMPLETE standalone/server
// setup (ResumeSetup) re-enters the wizard. A resuming client stays on the
// client panel in this task — Task 5 gives the client wizard its entry form.
func launchTarget(l roles.Launch) string {
	if l.ResumeSetup && l.Kind != roles.LaunchClient {
		return "wizard"
	}
	switch l.Kind {
	case roles.LaunchWizard:
		return "wizard"
	case roles.LaunchBroker:
		return "broker"
	default:
		return "client"
	}
}

// Run starts the console. modeFlag (from `tui --mode`) passes straight into
// roles.ResolveMode — the single launch-decision authority (spec §1.2) — and
// the resulting Launch dispatches to the wizard, the broker App, or the client
// panel. ResumeSetup routes standalone/server back into the wizard with the
// role preselected; when the wizard finishes/quits, the next launch resumes
// normally (Tasks 3-5 chain wizard completion straight into the consoles).
func Run(modeFlag string) error {
	if !isTTY() {
		return errors.New("tui requires a terminal (in mintty run via `winpty ssh-manager tui`, or use Windows Terminal)")
	}
	l, err := roles.ResolveMode(modeFlag)
	if err != nil {
		return err
	}
	switch launchTarget(l) {
	case "wizard":
		m := newWizard(l)
		if l.ResumeSetup && (l.Role == roles.RoleStandalone || l.Role == roles.RoleServer) {
			m = newWizardForRole(l)
		}
		fm, werr := tea.NewProgram(m).Run()
		if werr != nil {
			return werr
		}
		// Handoff sentinel (T3): a COMPLETED wizard exits with done=true and
		// the wizard's store still open — chain straight into the target
		// console instead of dropping the user back to the shell.
		if wm, ok := fm.(wizardModel); ok {
			if wm.done && wm.next == "broker" && wm.st != nil {
				app, aerr := NewBrokerApp(wm.st)
				if aerr != nil {
					wm.closeStore()
					return aerr
				}
				_, werr = tea.NewProgram(app).Run()
			}
			wm.closeStore()
		}
		return werr
	case "client":
		p := tea.NewProgram(newClientModel())
		_, err = p.Run()
		return err
	default: // broker
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
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// closeStore closes the wizard's vault store — the ONE cleanup path Run uses
// after the wizard program exits. st is nil on every early exit (first-screen
// q before choosing a role, the T4/T5 placeholder pages, stepVaultErr), so the
// nil guard is what makes quit-from-anywhere safe.
func (wm *wizardModel) closeStore() {
	if wm.st != nil {
		wm.st.Close()
	}
}
