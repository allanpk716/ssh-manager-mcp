package tui

import (
	tea "charm.land/bubbletea/v2"
)

// app.go (minimal stub for this task; Task 3 replaces it)
type app struct{ mode Mode }

func newApp(m Mode) app { return app{mode: m} }
func (a app) Init() tea.Cmd                                { return nil }
func (a app) Update(msg tea.Msg) (tea.Model, tea.Cmd)      { return a, nil }
func (a app) View() tea.View                                { return tea.NewView("tui loading… (q to quit)") }
