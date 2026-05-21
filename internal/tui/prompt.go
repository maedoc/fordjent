package tui

import (
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbletea"
	"github.com/fordjent/fordjent/internal/forgejo"
)

type PromptModel struct {
	input  textinput.Model
	repo   string
	client *forgejo.Client
	active bool
	width  int
}

func NewPromptModel(client *forgejo.Client, repo string) PromptModel {
	ti := textinput.New()
	ti.Placeholder = "Type a comment, or /command...  (Enter to send)"
	ti.Prompt = "> "
	ti.CharLimit = 500
	return PromptModel{
		input:  ti,
		repo:   repo,
		client: client,
	}
}

func (m PromptModel) Focus() PromptModel {
	m.active = true
	m.input.Focus()
	return m
}

func (m PromptModel) Blur() PromptModel {
	m.active = false
	m.input.Blur()
	return m
}

func (m PromptModel) Active() bool {
	return m.active
}

func (m PromptModel) Value() string {
	return m.input.Value()
}

func (m PromptModel) SetValue(s string) PromptModel {
	m.input.SetValue(s)
	return m
}

func (m PromptModel) SetCursorPos(n int) PromptModel {
	m.input.SetCursor(n)
	return m
}

func (m PromptModel) SetWidth(w int) PromptModel {
	m.width = w
	m.input.Width = w - 4
	if m.input.Width < 1 {
		m.input.Width = 1
	}
	return m
}

func (m PromptModel) Update(msg tea.Msg) (PromptModel, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m PromptModel) View() string {
	return promptStyle.Width(m.width).Render(m.input.View())
}