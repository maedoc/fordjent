package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type MergeDialog struct {
	prNumber int
	selected int
	styles   []string
	visible  bool
}

func NewMergeDialog() MergeDialog {
	return MergeDialog{
		styles:   []string{"merge", "rebase-merge", "squash-merge"},
		selected: 0,
	}
}

func (m MergeDialog) Show(prNumber int) MergeDialog {
	m.visible = true
	m.prNumber = prNumber
	m.selected = 0
	return m
}

func (m MergeDialog) Hide() MergeDialog {
	m.visible = false
	return m
}

func (m MergeDialog) Visible() bool {
	return m.visible
}

func (m MergeDialog) Style() string {
	if m.selected >= 0 && m.selected < len(m.styles) {
		return m.styles[m.selected]
	}
	return "merge"
}

func (m MergeDialog) PRNumber() int {
	return m.prNumber
}

func (m MergeDialog) Update(key string) MergeDialog {
	switch key {
	case "up", "left":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "right":
		if m.selected < len(m.styles)-1 {
			m.selected++
		}
	}
	return m
}

func (m MergeDialog) View() string {
	if !m.visible {
		return ""
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Merge PR #%d", m.prNumber)))
	b.WriteString("\n\n")
	b.WriteString("Select merge style:\n\n")

	labels := []string{"merge commit", "rebase merge", "squash merge"}
	for i, label := range labels {
		prefix := "  "
		if i == m.selected {
			prefix = selectedStyle.Render("▶ ")
		}
		b.WriteString(prefix)
		b.WriteString(label)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Enter to confirm  │  Esc to cancel"))

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2)

	return boxStyle.Render(b.String())
}