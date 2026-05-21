package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

var (
	stateOpened       = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	stateNeedsRole    = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	stateReady        = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	stateInProgress   = lipgloss.NewStyle().Foreground(lipgloss.Color("51"))
	statePlanning     = lipgloss.NewStyle().Foreground(lipgloss.Color("69"))
	statePlanApproved = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))
	stateImplementing = lipgloss.NewStyle().Foreground(lipgloss.Color("87"))
	stateBlocked      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	stateReview       = lipgloss.NewStyle().Foreground(lipgloss.Color("183"))
	stateMerging      = lipgloss.NewStyle().Foreground(lipgloss.Color("198"))
	stateDone         = lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Bold(true)

	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("254"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	activeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	headerStyle   = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236")).Foreground(lipgloss.Color("254")).Padding(0, 1)
	sidebarStyle  = lipgloss.NewStyle().BorderRight(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	detailStyle   = lipgloss.NewStyle().Padding(0, 1)
	statusBarStyle = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("254")).Padding(0, 1)
	selectedStyle  = lipgloss.NewStyle().Background(lipgloss.Color("62")).Foreground(lipgloss.Color("230"))
	promptStyle    = lipgloss.NewStyle().BorderTop(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
)

func stateStyle(state string) lipgloss.Style {
	switch state {
	case "opened":
		return stateOpened
	case "needs-role":
		return stateNeedsRole
	case "ready":
		return stateReady
	case "in_progress":
		return stateInProgress
	case "planning":
		return statePlanning
	case "plan-approved":
		return statePlanApproved
	case "implementing":
		return stateImplementing
	case "blocked":
		return stateBlocked
	case "review":
		return stateReview
	case "merging":
		return stateMerging
	case "done":
		return stateDone
	default:
		return dimStyle
	}
}

func stateIcon(state string) string {
	switch state {
	case "opened":
		return "○"
	case "needs-role":
		return "◎"
	case "ready":
		return "⚑"
	case "in_progress":
		return "▻"
	case "planning":
		return "◈"
	case "plan-approved":
		return "✓"
	case "implementing":
		return "▻"
	case "blocked":
		return "⊘"
	case "review":
		return "◉"
	case "merging":
		return "⊞"
	case "done":
		return "✓"
	default:
		return "·"
	}
}

func StateBadge(state string) string {
	return stateStyle(state).Render(stateIcon(state) + " " + state)
}

func formatNum(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}