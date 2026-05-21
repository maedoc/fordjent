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

type focusPanel int

const (
	focusSidebar focusPanel = iota
	focusDetail
	focusPrompt
)

type viewMode int

const (
	viewMain viewMode = iota
	viewMetrics
	viewActivity
	viewHelp
)

type Model struct {
	forgejoClient   *forgejo.Client
	fordjentURL     string
	repo            string
	keys            KeyBinds
	tuiCfg          TUIConfig
	width           int
	height          int
	focus           focusPanel
	prevFocus       focusPanel
	view            viewMode
	filter          string
	sidebar         SidebarModel
	detail          DetailModel
	prompt          PromptModel
	handler         *CommandHandler
	metrics         MetricsModel
	activity        ActivityModel
	statusClient    *StatusClient
	sseClient       *SSEClient
	createForm      CreateFormModel
	quitting        bool
	lastStatus      map[string]interface{}
	lastStatusData  *StatusResponse
	sseConnected    bool
	lastCmdResult   string
	err             error
	connError       string
}

type Config struct {
	ForgejoURL   string
	ForgejoToken string
	FordjentURL  string
	Repo         string
	PollInterval time.Duration
}

func NewModel(cfg Config) Model {
	client := forgejo.NewClient(cfg.ForgejoURL, cfg.ForgejoToken)
	statusCli := NewStatusClient(cfg.FordjentURL)
	sseCli := NewSSEClient(cfg.FordjentURL)
	tuiCfg := LoadTUIConfig()
	pollInterval := tuiCfg.PollDuration()
	if cfg.PollInterval > 0 {
		pollInterval = cfg.PollInterval
	}
	tuiCfg.PollInterval = pollInterval.String()
	return Model{
		forgejoClient: client,
		fordjentURL:   cfg.FordjentURL,
		repo:          cfg.Repo,
		keys:          tuiCfg.ApplyKeybinds(DefaultKeyBinds()),
		tuiCfg:        tuiCfg,
		focus:         focusSidebar,
		prevFocus:     focusSidebar,
		view:          viewMain,
		filter:        "all",
		sidebar:       NewSidebarModel(client, cfg.Repo),
		detail:        NewDetailModel(client, cfg.Repo, cfg.FordjentURL),
		prompt:        NewPromptModel(client, cfg.Repo),
		handler:       NewCommandHandler(client, cfg.Repo),
		metrics:       NewMetricsModel(),
		activity:      NewActivityModel(),
		statusClient:  statusCli,
		sseClient:     sseCli,
		createForm:    NewCreateFormModel(client, cfg.Repo),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		pollForgejo(m),
		pollStatus(m),
		connectSSE(m),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.sidebar.width = sidebarWidth(msg.Width)
		m.sidebar.height = msg.Height - 5
		m.detail.width = msg.Width - sidebarWidth(msg.Width) - 2
		m.detail.height = msg.Height - 5
		m.prompt = m.prompt.SetWidth(msg.Width)
		m.createForm = m.createForm.SetWidth(msg.Width)
		return m, nil

	case tea.KeyMsg:
		if m.createForm.Visible() {
			var cmd tea.Cmd
			m.createForm, cmd = m.createForm.Update(msg)
			return m, cmd
		}
		if m.focus == focusPrompt {
			return m.handlePromptKey(msg)
		}
		return m.handleKey(msg)

	case statusTickMsg:
		return m, tea.Batch(pollForgejo(m), pollStatus(m))

	case forgejoTickErrorMsg:
		m.connError = "Forgejo: connection error"
		return m, nil

	case itemsLoadedMsg:
		m.connError = ""
		m.sidebar.SetItems(msg.issues, msg.prs)
		return m, nil

	case statusTickErrorMsg:
		m.connError = "Status: connection error"
		return m, nil

	case statusLoadedMsg:
		m.connError = ""
		m.lastStatus = msg.data
		if msg.statusResp != nil {
			m.lastStatusData = msg.statusResp
			m.metrics = m.metrics.SetStatus(msg.statusResp)
		}
		if msg.tokenHist != nil {
			m.metrics = m.metrics.SetTokenHistory(msg.tokenHist)
		}
		if m.detail.issue != nil {
			var turns []TurnRow
			if msg.statusResp != nil {
				turns = msg.statusResp.Lifecycle.RecentTurns
			}
			m.detail = m.detail.SetLifecycleData(turns, nil)
		}
		return m, nil

	case prDetailLoadedMsg:
		m.detail.prDetail = m.detail.prDetail.SetWidth(m.detail.width - 2)
		m.detail.prDetail = m.detail.prDetail.SetPR(msg.pr)
		m.detail.prDetail = m.detail.prDetail.SetFiles(msg.files)
		m.detail.prDetail = m.detail.prDetail.SetReviews(msg.reviews)
		return m, nil

	case commentsLoadedMsg:
		if m.detail.issue != nil && msg.issueNumber == m.detail.issue.Number {
			m.detail.SetComments(msg.comments)
		}
		return m, nil

	case sseConnectedMsg:
		m.sseConnected = true
		return m, waitForSSEEvent(m.sseClient.Events)

	case sseEventMsg:
		m.sseConnected = true
		m.sidebar = m.sidebar.UpdateSSE(msg.evt)
		m.detail = m.detail.UpdateSSE(msg.evt)
		m.activity = m.activity.AddSSEEvent(msg.evt)
		if m.sidebar.Stale() {
			m.sidebar = m.sidebar.ClearStale()
			return m, tea.Batch(waitForSSEEvent(m.sseClient.Events), pollForgejo(m))
		}
		return m, waitForSSEEvent(m.sseClient.Events)

	case sseDisconnectedMsg:
		m.sseConnected = false
		return m, tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
			return sseReconnectMsg{}
		})

	case sseReconnectMsg:
		return m, reconnectSSE(m)

	case errorMsg:
		m.err = msg.err
		return m, nil

	case commandResultMsg:
		m.lastCmdResult = msg.output
		if msg.err != nil {
			m.lastCmdResult = "error: " + msg.err.Error()
		}
		if m.createForm.Visible() {
			m.createForm = m.createForm.Hide()
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.sidebar, cmd = m.sidebar.Update(msg)
	m.detail, cmd = m.detail.Update(msg)
	if m.prompt.Active() {
		m.prompt, _ = m.prompt.Update(msg)
	}
	return m, cmd
}

func (m Model) handlePromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		value := m.prompt.Value()
		m.prompt = m.prompt.SetValue("")
		m.focus = m.prevFocus
		m.prompt = m.prompt.Blur()

		issueNum := 0
		prNum := 0
		if item := m.sidebar.SelectedItem(); item != nil {
			issueNum = item.Number
			if item.PullRequest != nil {
				prNum = item.Number
			}
		}

		cmd := ParseCommand(value)
		if cmd != nil {
			if cmd.Name == "merge" && prNum > 0 {
				m.detail.mergeDialog = m.detail.mergeDialog.Show(prNum)
				return m, nil
			}
			return m, m.executeCommand(cmd, issueNum, prNum)
		}
		if strings.TrimSpace(value) != "" {
			num := issueNum
			if num <= 0 {
				num = prNum
			}
			if num > 0 {
				return m, m.postComment(value, num)
			}
			m.lastCmdResult = "no issue or PR selected"
		}
		return m, nil

	case "esc":
		m.prompt = m.prompt.SetValue("")
		m.focus = m.prevFocus
		m.prompt = m.prompt.Blur()
		return m, nil

	case "ctrl+c":
		m.prompt = m.prompt.SetValue("")
		m.focus = m.prevFocus
		m.prompt = m.prompt.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return m, cmd
}

func (m Model) executeCommand(cmd *Command, issueNum, prNum int) tea.Cmd {
	handler := m.handler
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		output := handler.Execute(ctx, cmd, issueNum, prNum)
		return commandResultMsg{output: output}
	}
}

func (m Model) postComment(body string, issueNum int) tea.Cmd {
	client := m.forgejoClient
	repo := m.repo
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.PostIssueComment(ctx, repo, issueNum, body); err != nil {
			return commandResultMsg{output: "error posting comment: " + err.Error()}
		}
		return commandResultMsg{output: "comment posted"}
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case m.keys.Quit:
		m.quitting = true
		return m, tea.Quit
	case m.keys.EnterCommand:
		m.prevFocus = m.focus
		m.focus = focusPrompt
		m.prompt = m.prompt.Focus()
		m.prompt = m.prompt.SetValue("")
		m.lastCmdResult = ""
		return m, nil
	case m.keys.SwitchFocus:
		if m.focus < focusPrompt {
			m.focus++
		} else {
			m.focus = focusSidebar
		}
		return m, nil
	case m.keys.FilterNext:
		m.sidebar.CycleFilter(1)
		return m, nil
	case m.keys.FilterPrev:
		m.sidebar.CycleFilter(-1)
		return m, nil
	case m.keys.Metrics:
		if m.view == viewMetrics {
			m.view = viewMain
		} else {
			m.view = viewMetrics
		}
		return m, nil
	case m.keys.Activity:
		if m.view == viewActivity {
			m.view = viewMain
		} else {
			m.view = viewActivity
		}
		return m, nil
	case m.keys.Help:
		if m.view == viewHelp {
			m.view = viewMain
		} else {
			m.view = viewHelp
		}
		return m, nil
	case m.keys.Open:
		if m.focus == focusSidebar {
			item := m.sidebar.SelectedItem()
			if item != nil {
				cmd := m.detail.SetItem(item)
				if pr := m.sidebar.SelectedPR(); pr != nil {
					m.detail.prDetail = m.detail.prDetail.SetPR(pr)
					loadCmd := m.detail.prDetail.LoadPR()
					cmd = tea.Batch(cmd, loadCmd)
				}
				m.focus = focusDetail
				return m, cmd
			}
		}
		return m, nil
	case m.keys.Back:
		if m.focus == focusDetail {
			m.focus = focusSidebar
		} else if m.view != viewMain {
			m.view = viewMain
		}
		return m, nil
	case m.keys.NewIssue:
		m.createForm = m.createForm.SetWidth(m.width).Show()
		return m, nil
	}
	if m.focus == focusSidebar {
		var cmd tea.Cmd
		m.sidebar, cmd = m.sidebar.Update(msg)
		return m, cmd
	}
	if m.focus == focusDetail {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	promptBar := m.prompt.View()

	var body string
	switch m.view {
	case viewMetrics:
		body = m.renderMetricsOverlay()
	case viewActivity:
		body = m.renderActivityOverlay()
	case viewHelp:
		body = m.renderHelpOverlay()
	default:
		sidebarView := m.sidebar.View()
		detailView := m.detail.View()
		body = lipgloss.JoinHorizontal(lipgloss.Top, sidebarView, detailView)
	}

	if m.lastCmdResult != "" {
		body += "\n" + dimStyle.Render(m.lastCmdResult)
	}

	mainView := lipgloss.JoinVertical(lipgloss.Left, header, body, promptBar, footer)

	if m.createForm.Visible() {
		formView := m.createForm.View()
		formHeight := lipgloss.Height(formView)
		formWidth := lipgloss.Width(formView)
		form := lipgloss.NewStyle().
			Width(formWidth).
			Height(formHeight).
			Render(formView)
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, form, lipgloss.WithWhitespaceChars(" "), lipgloss.WithWhitespaceBackground(lipgloss.Color("235")))
	}

	return mainView
}

func sidebarWidth(totalWidth int) int {
	w := totalWidth / 4
	if w < 24 {
		w = 24
	}
	if w > 40 {
		w = 40
	}
	return w
}

func (m Model) renderHeader() string {
	repo := m.repo
	if repo == "" {
		repo = "no repo"
	}
	active := "0"
	if m.lastStatus != nil {
		if v, ok := m.lastStatus["metrics"].(map[string]interface{}); ok {
			if a, ok := v["sessions_active"].(float64); ok {
				active = fmt.Sprintf("%.0f", a)
			}
		}
	}
	sse := "○"
	if m.sseConnected {
		sse = "◉"
	}
	parts := []string{
		titleStyle.Render("fordjent"),
		dimStyle.Render("│"),
		dimStyle.Render(repo),
		dimStyle.Render("│"),
		activeStyle.Render(active + " active"),
		dimStyle.Render("│"),
		dimStyle.Render(sse + " SSE"),
	}
	if m.connError != "" {
		parts = append(parts, dimStyle.Render("│"), errorStyle.Render("⚠ "+m.connError))
	}
	return headerStyle.Width(m.width).Render(strings.Join(parts, " "))
}

func (m Model) renderFooter() string {
	filterLabel := m.sidebar.FilterLabel()
	parts := []string{
		dimStyle.Render("[" + filterLabel + "]"),
		dimStyle.Render("│"),
		dimStyle.Render("? help"),
		dimStyle.Render("m metrics"),
		dimStyle.Render("a activity"),
		dimStyle.Render("q quit"),
	}
	return statusBarStyle.Width(m.width).Render(strings.Join(parts, "  "))
}

func (m Model) renderMetricsOverlay() string {
	return m.metrics.SetSize(m.width, m.height-4).SetStatus(m.lastStatusData).View()
}

func (m Model) renderActivityOverlay() string {
	return m.activity.SetSize(m.width, m.height-4).View()
}

func (m Model) renderHelpOverlay() string {
	help := []string{
		"Navigation:",
		"  ↑/↓     Move selection",
		"  Enter   Open selected item",
		"  Esc     Back / unfocus",
		"  Tab     Switch focus (sidebar ↔ detail)",
		"",
		"Filtering:",
		"  [ / ]   Cycle FSM state filter",
		"",
		"Views:",
		"  m       Toggle metrics",
		"  a       Toggle activity feed",
		"  ?       Toggle this help",
		"",
		"Actions:",
		"  n       New issue",
		"  /       Enter command/comment mode",
		"",
		"Commands (in the prompt bar):",
		"  <text>           Post a comment (just type and Enter)",
		"  /comment <text>   Post a comment",
		"  /create           Create a new issue (hint)",
		"  /label <name>     Add a label",
		"  /unlabel <name>   Remove a label",
		"  /start            Mark issue as ready",
		"  /role <role>      Assign role label",
		"  /approve          Approve a plan",
		"  /merge            Merge the selected PR",
		"  /close            Close issue or PR",
		"  /reopen           Reopen a closed issue",
		"  /retry            Retry a failed session",
		"  /unblock          Unblock an issue",
		"  /help             Show command help",
		"",
		"Other:",
		"  q       Quit",
	}
	return detailStyle.Width(m.width - 4).Height(m.height - 4).Render(strings.Join(help, "\n"))
}

func pollForgejo(m Model) tea.Cmd {
	return tea.Tick(m.tuiCfg.PollDuration(), func(t time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		issues, iErr := m.forgejoClient.ListOpenIssues(ctx, m.repo)
		prs, pErr := m.forgejoClient.ListPRs(ctx, m.repo, "open")
		if iErr != nil || pErr != nil {
			return forgejoTickErrorMsg{issueErr: iErr, prErr: pErr}
		}
		if issues == nil {
			issues = []forgejo.Issue{}
		}
		if prs == nil {
			prs = []forgejo.PullRequest{}
		}
		return itemsLoadedMsg{issues: issues, prs: prs}
	})
}

func pollStatus(m Model) tea.Cmd {
	return tea.Tick(m.tuiCfg.PollDuration(), func(t time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data := make(map[string]interface{})
		var statusResp *StatusResponse
		var tokenHist []TokenMinute
		if m.statusClient != nil {
			resp, err := m.statusClient.FetchStatus(ctx)
			if err != nil {
				return statusTickErrorMsg{err: err}
			}
			statusResp = resp
			if hist, err := m.statusClient.FetchTokensPerMinute(ctx, 1); err == nil {
				tokenHist = hist
			}
		}
		return statusLoadedMsg{data: data, statusResp: statusResp, tokenHist: tokenHist}
	})
}

func connectSSE(m Model) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := m.sseClient.Connect(ctx); err != nil {
			return errorMsg{err: fmt.Errorf("SSE: %w", err)}
		}
		return sseConnectedMsg{}
	}
}

func reconnectSSE(m Model) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.sseClient.Reconnect(ctx); err != nil {
			return sseDisconnectedMsg{}
		}
		return sseConnectedMsg{}
	}
}

func waitForSSEEvent(ch chan lifecycle.SSEEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return sseDisconnectedMsg{}
		}
		return sseEventMsg{evt: evt}
	}
}

func Run(cfg Config) error {
	m := NewModel(cfg)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}