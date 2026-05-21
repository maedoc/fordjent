package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fordjent/fordjent/internal/forgejo"
)

type formField int

const (
	fieldRole formField = iota
	fieldTitle
	fieldBody
)

var roles = []string{"implementer", "pm", "devops", "tester", "reviewer"}

type CreateFormModel struct {
	client  *forgejo.Client
	repo    string
	visible bool
	field   formField
	roleIdx int
	title   textinput.Model
	body    textarea.Model
	width   int
	height  int
}

func NewCreateFormModel(client *forgejo.Client, repo string) CreateFormModel {
	ti := textinput.New()
	ti.Placeholder = "Issue title (role tag auto-added)"
	ti.Prompt = ""
	ti.CharLimit = 200
	ti.Focus()

	ta := textarea.New()
	ta.Placeholder = "Issue body... (Depends on: #N for dependencies)"
	ta.Prompt = ""
	ta.CharLimit = 5000
	ta.SetHeight(6)

	return CreateFormModel{
		client:  client,
		repo:    repo,
		roleIdx: 0,
		title:   ti,
		body:    ta,
	}
}

func (m CreateFormModel) Show() CreateFormModel {
	m.visible = true
	m.field = fieldRole
	m.title.SetValue("")
	m.body.SetValue("")
	m.title.Focus()
	return m
}

func (m CreateFormModel) Hide() CreateFormModel {
	m.visible = false
	m.title.Blur()
	m.body.Blur()
	return m
}

func (m CreateFormModel) Visible() bool {
	return m.visible
}

func (m CreateFormModel) SetWidth(w int) CreateFormModel {
	m.width = w
	m.title.Width = w - 4
	m.body.SetWidth(w - 4)
	return m
}

func (m CreateFormModel) Update(msg tea.Msg) (CreateFormModel, tea.Cmd) {
	if !m.visible {
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.field == fieldRole {
			switch msg.String() {
			case "left", "up":
				if m.roleIdx > 0 {
					m.roleIdx--
				}
				return m, nil
			case "right", "down", "tab":
				if m.roleIdx < len(roles)-1 {
					m.roleIdx++
				} else {
					m.field = fieldTitle
					m.title.Focus()
				}
				return m, nil
			case "enter":
				m.field = fieldTitle
				m.title.Focus()
				return m, nil
			case "esc":
				return m.Hide(), nil
			}
			return m, nil
		}

		if m.field == fieldTitle {
			switch msg.String() {
			case "enter":
				m.field = fieldBody
				m.title.Blur()
				m.body.Focus()
				return m, nil
			case "esc":
				return m.Hide(), nil
			case "tab":
				m.field = fieldBody
				m.title.Blur()
				m.body.Focus()
				return m, nil
			}
			var cmd tea.Cmd
			m.title, cmd = m.title.Update(msg)
			return m, cmd
		}

		if m.field == fieldBody {
			switch msg.String() {
			case "ctrl+s":
				return m, m.submitForm()
			case "esc":
				return m.Hide(), nil
			}
			var cmd tea.Cmd
			m.body, cmd = m.body.Update(msg)
			return m, cmd
		}
	}

	var cmd tea.Cmd
	if m.field == fieldTitle {
		m.title, cmd = m.title.Update(msg)
	} else if m.field == fieldBody {
		m.body, cmd = m.body.Update(msg)
	}
	return m, cmd
}

func (m CreateFormModel) submitForm() tea.Cmd {
	role := roles[m.roleIdx]
	title := fmt.Sprintf("[%s] %s", role, m.title.Value())
	body := m.body.Value()

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := m.client.CreateIssue(ctx, m.repo, title, body)
		if err != nil {
			return commandResultMsg{output: "", err: err}
		}
		return commandResultMsg{output: fmt.Sprintf("Created issue: %s", title), err: nil}
	}
}

func (m CreateFormModel) View() string {
	if !m.visible {
		return ""
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("New Issue"))
	b.WriteString("\n\n")

	b.WriteString("Role:  ")
	for i, r := range roles {
		if i == m.roleIdx && m.field == fieldRole {
			b.WriteString(selectedStyle.Render(fmt.Sprintf("[%s]", r)))
		} else if i == m.roleIdx {
			b.WriteString(activeStyle.Render(fmt.Sprintf("[%s]", r)))
		} else {
			b.WriteString(dimStyle.Render(fmt.Sprintf(" %s ", r)))
		}
		b.WriteString(" ")
	}
	b.WriteString("\n\n")

	b.WriteString("Title: ")
	if m.field == fieldTitle {
		b.WriteString(m.title.View())
	} else if m.title.Value() == "" {
		b.WriteString(dimStyle.Render(m.title.Placeholder))
	} else {
		b.WriteString(m.title.Value())
	}
	b.WriteString("\n\n")

	b.WriteString("Body:")
	b.WriteString("\n")
	b.WriteString(m.body.View())
	b.WriteString("\n\n")

	b.WriteString(dimStyle.Render("Tab: next field  │  Ctrl+S: submit  │  Esc: cancel"))

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(m.width - 4)

	return boxStyle.Render(b.String())
}