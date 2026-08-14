package tui

import "charm.land/lipgloss/v2"

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	selStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	detailStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	footerStyle = lipgloss.NewStyle().Faint(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	secretStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)
