package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
)

var filterOrder = []string{"all", "open", "ready", "implementing", "review", "blocked", "done"}

type SidebarModel struct {
	client       *forgejo.Client
	repo         string
	issues       []forgejo.Issue
	prs          []forgejo.PullRequest
	filteredIssueIndices []int
	cursor       int
	filter       string
	width        int
	height       int
	scroll       int
	stale        bool
}

func NewSidebarModel(client *forgejo.Client, repo string) SidebarModel {
	return SidebarModel{
		client: client,
		repo:   repo,
		filter: "all",
	}
}

func (m SidebarModel) SetItems(issues []forgejo.Issue, prs []forgejo.PullRequest) SidebarModel {
	m.issues = issues
	m.prs = prs
	m.filteredIssueIndices = m.computeFilteredIssues()
	if m.cursor >= len(m.filteredIssueIndices)+len(m.prs) {
		m.cursor = len(m.filteredIssueIndices) + len(m.prs) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.adjustScroll()
	return m
}

func (m SidebarModel) computeFilteredIssues() []int {
	if m.filter == "all" {
		indices := make([]int, len(m.issues))
		for i := range m.issues {
			indices[i] = i
		}
		return indices
	}
	var indices []int
	for i, issue := range m.issues {
		if issueState(issue) == lifecycle.IssueState(m.filter) {
			indices = append(indices, i)
		}
	}
	if indices == nil {
		indices = []int{}
	}
	return indices
}

func (m SidebarModel) SelectedItem() *forgejo.Issue {
	totalItems := len(m.filteredIssueIndices) + len(m.prs)
	if m.cursor < 0 || m.cursor >= totalItems {
		return nil
	}
	if m.cursor < len(m.filteredIssueIndices) {
		idx := m.filteredIssueIndices[m.cursor]
		return &m.issues[idx]
	}
	prIdx := m.cursor - len(m.filteredIssueIndices)
	if prIdx < len(m.prs) {
		pr := m.prs[prIdx]
		issue := &forgejo.Issue{
			Number:      pr.Number,
			Title:       pr.Title,
			State:       pr.State,
			PullRequest: &forgejo.PRRef{URL: fmt.Sprintf("%d", pr.Number)},
		}
		return issue
	}
	return nil
}

func (m SidebarModel) SelectedPR() *forgejo.PullRequest {
	if m.cursor < len(m.filteredIssueIndices) {
		return nil
	}
	prIdx := m.cursor - len(m.filteredIssueIndices)
	if prIdx < 0 || prIdx >= len(m.prs) {
		return nil
	}
	return &m.prs[prIdx]
}

func (m SidebarModel) CycleFilter(dir int) SidebarModel {
	idx := 0
	for i, f := range filterOrder {
		if f == m.filter {
			idx = i
			break
		}
	}
	idx = (idx + dir) % len(filterOrder)
	if idx < 0 {
		idx += len(filterOrder)
	}
	m.filter = filterOrder[idx]
	m.filteredIssueIndices = m.computeFilteredIssues()
	m.cursor = 0
	m.scroll = 0
	return m
}

func (m SidebarModel) FilterLabel() string {
	return m.filter
}

func (m SidebarModel) UpdateSSE(evt lifecycle.SSEEvent) SidebarModel {
	if evt.Type == "transition" || evt.Type == "delivery" || evt.Type == "turn" {
		m.stale = true
	}
	return m
}

func (m SidebarModel) Stale() bool {
	return m.stale
}

func (m SidebarModel) ClearStale() SidebarModel {
	m.stale = false
	return m
}

func (m SidebarModel) Update(msg tea.Msg) (SidebarModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.adjustScroll()
			}
		case "down", "j":
			maxCursor := len(m.filteredIssueIndices) + len(m.prs) - 1
			if m.cursor < maxCursor {
				m.cursor++
				m.adjustScroll()
			}
		}
	}
	return m, nil
}

func (m *SidebarModel) adjustScroll() {
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	visibleHeight := m.visibleHeight()
	if m.cursor >= m.scroll+visibleHeight {
		m.scroll = m.cursor - visibleHeight + 1
	}
}

func (m SidebarModel) visibleHeight() int {
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

func (m SidebarModel) View() string {
	if m.width < 10 {
		return ""
	}

	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	totalRows := len(m.filteredIssueIndices) + len(m.prs)
	end := m.scroll + m.visibleHeight()
	if end > totalRows {
		end = totalRows
	}

	for row := m.scroll; row < end; row++ {
		if row < len(m.filteredIssueIndices) {
			issueIdx := m.filteredIssueIndices[row]
			b.WriteString(m.renderIssueRow(m.issues[issueIdx], row))
		} else {
			prIdx := row - len(m.filteredIssueIndices)
			b.WriteString(m.renderPRRow(m.prs[prIdx], row))
		}
		b.WriteString("\n")
	}

	if totalRows == 0 {
		b.WriteString(dimStyle.Render("  no items"))
		b.WriteString("\n")
	}

	return sidebarStyle.Width(m.width).Height(m.height).Render(b.String())
}

func (m SidebarModel) renderHeader() string {
	counts := m.countByState()
	headerParts := []string{
		titleStyle.Render("ISSUES"),
		dimStyle.Render(fmt.Sprintf("(%d)", len(m.filteredIssueIndices))),
	}

	if m.filter != "all" {
		headerParts = append(headerParts, dimStyle.Render("│"), stateStyle(m.filter).Render(m.filter))
	}

	for _, st := range filterOrder[1:] {
		if st == m.filter || st == "open" {
			continue
		}
		if c, ok := counts[st]; ok && c > 0 {
			headerParts = append(headerParts, dimStyle.Render(fmt.Sprintf(" %s:%d", st, c)))
		}
	}

	return lipgloss.NewStyle().Width(m.width - 2).Render(strings.Join(headerParts, " "))
}

func (m SidebarModel) countByState() map[string]int {
	counts := make(map[string]int)
	for _, issue := range m.issues {
		st := string(issueState(issue))
		counts[st]++
	}
	return counts
}

func (m SidebarModel) renderIssueRow(issue forgejo.Issue, row int) string {
	st := string(issueState(issue))
	icon := stateStyle(st).Render(stateIcon(st))
	num := dimStyle.Render(fmt.Sprintf("#%d", issue.Number))

	titleRunes := []rune(issue.Title)
	availWidth := m.width - 12
	if availWidth < 8 {
		availWidth = 8
	}
	if len(titleRunes) > availWidth {
		titleRunes = append(titleRunes[:availWidth-1], '…')
	}
	title := string(titleRunes)

	role := detectRole(issue)
	roleTag := ""
	if role != "" {
		roleTag = dimStyle.Render(" [" + role + "]")
	}

	line := fmt.Sprintf(" %s %s %s%s", icon, num, title, roleTag)
	rendered := lipgloss.NewStyle().Width(m.width - 2).MaxHeight(1).Render(line)

	if row == m.cursor {
		rendered = selectedStyle.Width(m.width - 2).Render(rendered)
	}

	return rendered
}

func (m SidebarModel) renderPRRow(pr forgejo.PullRequest, row int) string {
	var icon string
	if pr.Merged {
		icon = stateStyle("done").Render("◉")
	} else if pr.State == "closed" {
		icon = dimStyle.Render("○")
	} else {
		icon = stateStyle("review").Render("◉")
	}
	num := dimStyle.Render(fmt.Sprintf("#%d", pr.Number))

	titleRunes := []rune(pr.Title)
	availWidth := m.width - 10
	if availWidth < 8 {
		availWidth = 8
	}
	if len(titleRunes) > availWidth {
		titleRunes = append(titleRunes[:availWidth-1], '…')
	}
	title := string(titleRunes)

	line := fmt.Sprintf(" %s %s %s", icon, num, title)
	rendered := lipgloss.NewStyle().Width(m.width - 2).MaxHeight(1).Render(line)

	issueSectionSize := len(m.filteredIssueIndices)
	prCursor := row - issueSectionSize
	if row == m.cursor {
		rendered = selectedStyle.Width(m.width - 2).Render(rendered)
	}
	_ = prCursor

	return rendered
}

func issueState(issue forgejo.Issue) lifecycle.IssueState {
	names := make([]string, len(issue.Labels))
	for i, l := range issue.Labels {
		names[i] = l.Name
	}
	return lifecycle.StateFromLabels(names)
}

func detectRole(issue forgejo.Issue) string {
	lower := strings.ToLower(issue.Title)
	tags := []string{"implementer", "implement", "dev", "developer", "pm", "reviewer", "devops", "test", "tester"}
	for _, tag := range tags {
		if strings.Contains(lower, "["+tag+"]") {
			if tag == "implement" || tag == "dev" {
				return "implementer"
			}
			if tag == "test" {
				return "tester"
			}
			return tag
		}
	}
	for _, l := range issue.Labels {
		name := strings.ToLower(l.Name)
		if strings.HasPrefix(name, "role:") {
			role := strings.TrimPrefix(name, "role:")
			if role == "developer" {
				return "implementer"
			}
			return role
		}
	}
	return ""
}