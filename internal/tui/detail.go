package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
)

type TransitionRow struct {
	SessionKey string
	FromState  string
	ToState    string
	Reason     string
	Timestamp  string
}

type DetailModel struct {
	client      *forgejo.Client
	repo        string
	fordjentURL string
	issue       *forgejo.Issue
	prDetail    PRDetailModel
	mergeDialog MergeDialog
	comments    []forgejo.Comment
	turns       []TurnRow
	transitions []TransitionRow
	width       int
	height      int
	scroll      int
	loading     bool
}

func NewDetailModel(client *forgejo.Client, repo, fordjentURL string) DetailModel {
	return DetailModel{
		client:      client,
		repo:        repo,
		fordjentURL: fordjentURL,
		prDetail:    NewPRDetailModel(client, repo),
		mergeDialog: NewMergeDialog(),
	}
}

func (m DetailModel) loadComments() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		comments, _ := m.client.ListComments(ctx, m.repo, m.issue.Number)
		return commentsLoadedMsg{issueNumber: m.issue.Number, comments: comments}
	}
}

func (m *DetailModel) SetItem(issue *forgejo.Issue) tea.Cmd {
	m.issue = issue
	m.comments = nil
	m.turns = nil
	m.transitions = nil
	m.scroll = 0
	m.loading = true
	m.mergeDialog = m.mergeDialog.Hide()
	if issue != nil && issue.PullRequest != nil {
		pr := &forgejo.PullRequest{Number: issue.Number, Title: issue.Title, State: issue.State}
		m.prDetail = m.prDetail.SetPR(pr)
		cmd := m.prDetail.LoadPR()
		return tea.Batch(m.loadComments(), cmd)
	}
	m.prDetail = m.prDetail.SetPR(nil)
	return m.loadComments()
}

func (m *DetailModel) SetComments(comments []forgejo.Comment) {
	m.comments = comments
	m.loading = false
}

func (m *DetailModel) SetTurns(turns []TurnRow) {
	m.turns = turns
}

func (m *DetailModel) SetTransitions(transitions []TransitionRow) {
	m.transitions = transitions
}

func (m DetailModel) SetLifecycleData(turns []TurnRow, transitions []TransitionRow) DetailModel {
	if m.issue == nil {
		return m
	}
	issueSuffix := fmt.Sprintf("/issues/%d", m.issue.Number)
	prSuffix := fmt.Sprintf("/pulls/%d", m.issue.Number)
	var filteredTurns []TurnRow
	for _, t := range turns {
		if strings.HasSuffix(t.SessionKey, issueSuffix) || strings.HasSuffix(t.SessionKey, prSuffix) {
			filteredTurns = append(filteredTurns, t)
		}
	}
	var filteredTransitions []TransitionRow
	for _, t := range transitions {
		if strings.HasSuffix(t.SessionKey, issueSuffix) || strings.HasSuffix(t.SessionKey, prSuffix) {
			filteredTransitions = append(filteredTransitions, t)
		}
	}
	m.turns = filteredTurns
	m.transitions = filteredTransitions
	return m
}

func (m DetailModel) UpdateSSE(evt lifecycle.SSEEvent) DetailModel {
	if m.issue == nil {
		return m
	}
	sessionKey := m.repo + "/issues/" + fmt.Sprintf("%d", m.issue.Number)
	sessionKeyPR := m.repo + "/pulls/" + fmt.Sprintf("%d", m.issue.Number)
	switch evt.Type {
	case "turn":
		var tr TurnRow
		for _, f := range strings.Split(evt.Data, ",") {
			kv := strings.SplitN(f, ":", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.Trim(kv[0], " \"")
			v := strings.Trim(kv[1], " \"")
			switch k {
			case "session_key":
				tr.SessionKey = v
			case "turn":
				fmt.Sscanf(v, "%d", &tr.Turn)
			case "tool_calls":
				fmt.Sscanf(v, "%d", &tr.ToolCalls)
			case "latency_ms":
				fmt.Sscanf(v, "%d", &tr.LatencyMs)
			case "tokens_in":
				fmt.Sscanf(v, "%d", &tr.TokensIn)
			case "tokens_out":
				fmt.Sscanf(v, "%d", &tr.TokensOut)
			case "error":
				tr.Error = v
			case "timestamp":
				tr.Timestamp = v
			}
		}
		if tr.SessionKey == sessionKey || tr.SessionKey == sessionKeyPR {
			m.turns = append(m.turns, tr)
		}
	case "transition":
		var tr TransitionRow
		for _, f := range strings.Split(evt.Data, ",") {
			kv := strings.SplitN(f, ":", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.Trim(kv[0], " \"")
			v := strings.Trim(kv[1], " \"")
			switch k {
			case "session_key":
				tr.SessionKey = v
			case "from_state":
				tr.FromState = v
			case "to_state":
				tr.ToState = v
			case "reason":
				tr.Reason = v
			case "timestamp":
				tr.Timestamp = v
			}
		}
		if tr.SessionKey == sessionKey || tr.SessionKey == sessionKeyPR {
			m.transitions = append(m.transitions, tr)
		}
	}
	return m
}

func (m DetailModel) Update(msg tea.Msg) (DetailModel, tea.Cmd) {
	if m.mergeDialog.Visible() {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "esc":
				m.mergeDialog = m.mergeDialog.Hide()
				return m, nil
			case "enter":
				m2, cmd := m.confirmMerge()
				return m2, cmd
			case "up", "down", "left", "right":
				m.mergeDialog = m.mergeDialog.Update(msg.String())
				return m, nil
			}
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up":
			if m.scroll > 0 {
				m.scroll--
			}
		case "down":
			maxScroll := m.maxScroll()
			if m.scroll < maxScroll {
				m.scroll++
			}
		case "pgup":
			half := m.height / 2
			if half < 1 {
				half = 1
			}
			m.scroll -= half
			if m.scroll < 0 {
				m.scroll = 0
			}
		case "pgdown":
			half := m.height / 2
			if half < 1 {
				half = 1
			}
			maxScroll := m.maxScroll()
			m.scroll += half
			if m.scroll > maxScroll {
				m.scroll = maxScroll
			}
		}
	case prDetailLoadedMsg:
		m.prDetail = m.prDetail.SetWidth(m.width - 2)
		m.prDetail = m.prDetail.SetPR(msg.pr)
		m.prDetail = m.prDetail.SetFiles(msg.files)
		m.prDetail = m.prDetail.SetReviews(msg.reviews)
	}
	return m, nil
}

func (m DetailModel) confirmMerge() (DetailModel, tea.Cmd) {
	prNum := m.mergeDialog.PRNumber()
	style := m.mergeDialog.Style()
	m.mergeDialog = m.mergeDialog.Hide()
	if prNum <= 0 {
		return m, nil
	}
	client := m.client
	repo := m.repo
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.MergePR(ctx, repo, prNum, style); err != nil {
			return commandResultMsg{output: fmt.Sprintf("error merging PR: %v", err)}
		}
		return commandResultMsg{output: fmt.Sprintf("PR #%d merged (%s)", prNum, style)}
	}
}

func (m DetailModel) maxScroll() int {
	lines := len(strings.Split(m.content(), "\n"))
	max := lines - m.height
	if max < 0 {
		max = 0
	}
	return max
}

func (m DetailModel) View() string {
	if m.issue == nil {
		return detailStyle.Width(m.width).Height(m.height).Render(
			dimStyle.Render("Select an issue from the sidebar"),
		)
	}

	content := m.content()
	lines := strings.Split(content, "\n")
	totalLines := len(lines)
	visibleHeight := m.height
	if visibleHeight < 1 {
		visibleHeight = 1
	}

	if m.scroll >= totalLines {
		m.scroll = totalLines - 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}

	end := m.scroll + visibleHeight
	if end > totalLines {
		end = totalLines
	}

	visible := strings.Join(lines[m.scroll:end], "\n")
	rendered := detailStyle.Width(m.width).Height(m.height).Render(visible)

	if m.mergeDialog.Visible() {
		return lipgloss.JoinVertical(lipgloss.Center, rendered, m.mergeDialog.View())
	}

	return rendered
}

func (m DetailModel) content() string {
	var b strings.Builder

	title := m.issue.Title
	state := issueState(*m.issue)
	role := detectRole(*m.issue)

	b.WriteString(titleStyle.Render(title))
	b.WriteString(" ")
	b.WriteString(StateBadge(string(state)))
	if role != "" {
		b.WriteString(" ")
		b.WriteString(dimStyle.Render("[" + role + "]"))
	}
	b.WriteString("\n")

	var infoParts []string
	infoParts = append(infoParts, fmt.Sprintf("#%d", m.issue.Number))
	infoParts = append(infoParts, "State: "+string(state))
	if role != "" {
		infoParts = append(infoParts, "Role: "+role)
	}
	if m.issue.PullRequest != nil {
		infoParts = append(infoParts, "PR")
	}
	var labelNames []string
	for _, l := range m.issue.Labels {
		labelNames = append(labelNames, l.Name)
	}
	if len(labelNames) > 0 {
		infoParts = append(infoParts, "Labels: "+strings.Join(labelNames, ", "))
	}
	b.WriteString(dimStyle.Render(strings.Join(infoParts, " │ ")))
	b.WriteString("\n")

	if m.issue.PullRequest != nil {
		b.WriteString("\n")
		b.WriteString(m.prDetail.View())
		b.WriteString("\n")
	}

	if m.loading {
		b.WriteString(dimStyle.Render("Loading comments..."))
		b.WriteString("\n")
	} else if len(m.comments) > 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("── Comments ──"))
		b.WriteString("\n")
		contentWidth := m.width - 4
		if contentWidth < 20 {
			contentWidth = 20
		}
		for _, c := range m.comments {
			isBot := strings.Contains(c.Body, "<!-- ford -->")
			body := c.Body
			body = strings.ReplaceAll(body, "\n", " ")
			body = strings.ReplaceAll(body, "\r", "")
			if len(body) > contentWidth {
				body = body[:contentWidth-3] + "..."
			}
			prefix := c.User + ": "
			if isBot {
				prefix = "🤖 " + prefix
			}
			line := prefix + "\"" + body + "\""
			if isBot {
				line = dimStyle.Render(line)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	} else if !m.loading {
		b.WriteString(dimStyle.Render("No comments"))
		b.WriteString("\n")
	}

	if len(m.turns) > 0 || len(m.transitions) > 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("── Agent Activity ──"))
		b.WriteString("\n")

		for _, t := range m.turns {
			parts := []string{}
			if t.Timestamp != "" {
				parts = append(parts, "["+t.Timestamp+"]")
			}
			parts = append(parts, "🔧")
			if t.ToolCalls > 0 {
				parts = append(parts, fmt.Sprintf("tool_calls=%d", t.ToolCalls))
			}
			if t.LatencyMs > 0 {
				parts = append(parts, fmt.Sprintf("latency=%dms", t.LatencyMs))
			}
			parts = append(parts, fmt.Sprintf("tokens=%s/%s", formatNum(int64(t.TokensIn)), formatNum(int64(t.TokensOut))))
			if t.Error != "" {
				parts = append(parts, errorStyle.Render("err: "+t.Error))
			}
			b.WriteString(strings.Join(parts, " "))
			b.WriteString("\n")
		}

		for _, tr := range m.transitions {
			parts := []string{}
			if tr.Timestamp != "" {
				parts = append(parts, "["+tr.Timestamp+"]")
			}
			parts = append(parts, fmt.Sprintf("%s → %s", tr.FromState, tr.ToState))
			if tr.Reason != "" {
				parts = append(parts, "("+tr.Reason+")")
			}
			b.WriteString(strings.Join(parts, " "))
			b.WriteString("\n")
		}
	}

	return b.String()
}

