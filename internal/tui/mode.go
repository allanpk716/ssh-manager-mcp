package tui

import (
	"errors"
	"os"

	tea "charm.land/bubbletea/v2"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// The vault-probe helpers (vaultStorePath / vaultExists / vaultUnlocked) MOVED
// to internal/roles (roles.VaultExists / roles.VaultUnlocked + private
// vaultStorePath) — Plan 19 T1 makes roles the single launch-resolution
// authority. The unexported wrappers below exist for mode_test.go's probe
// tests; behavior is identical (stat-first probe, no OpenStore create-on-open
// side effect). DetectMode/DetectModeWith (and the Mode/ModeBroker/ModeClient
// constants only they used) were deleted in Plan 20 T1 — roles.ResolveMode is
// the only mode resolution path left.

// vaultExists reports whether a store.db file exists at the vault location.
func vaultExists() bool { return roles.VaultExists() }

// vaultUnlocked reports whether an UNLOCKED vault is reachable.
func vaultUnlocked() bool { return roles.VaultUnlocked() }

// launchTarget is the pure dispatch table behind Run (Plan 19 T2): first run →
// wizard; completed setups → broker/client; an INCOMPLETE setup
// (ResumeSetup) of ANY role re-enters the wizard — since Plan 42 批1 T8 the
// client's wizard step is the pair-guidance page (the connection-form flow is
// retired).
func launchTarget(l roles.Launch) string {
	if l.ResumeSetup {
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
// panel. ResumeSetup routes ANY role back into the wizard with the role
// preselected; when the wizard finishes/quits, the next launch resumes
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
		if l.ResumeSetup {
			m = newWizardForRole(l)
		}
		fm, werr := tea.NewProgram(m).Run()
		if werr != nil {
			return werr
		}
		// Handoff sentinel (T3): a COMPLETED wizard exits with done=true and
		// chains straight into the broker console instead of dropping the user
		// back to the shell — the broker App reusing the wizard's still-open
		// store. Plan 42 批1 T8 (dispatch 收窄): the client handoff is GONE —
		// the client role's wizard step is now a pair-guidance page whose next
		// action is running `ssh-manager pair` in the shell; the client panel
		// opens on the NEXT `tui` via launchTarget (role.json is completed by
		// the guidance page).
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
	// GetConsoleMode on Windows — NUL is a character device and must NOT
	// pass (Plan 20 A4: `tui < NUL` used to slip past a stat-based check and
	// then hang); char-device stat elsewhere. See istty_windows.go/istty_other.go.
	return IsTerminal(os.Stdin.Fd())
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
