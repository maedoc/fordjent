package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/fordjent/fordjent/internal/forgejo"
)

type PRDetailModel struct {
	client  *forgejo.Client
	repo    string
	pr      *forgejo.PullRequest
	files   []forgejo.PRFile
	reviews []forgejo.Review
	width   int
	height  int
	loading bool
}

func NewPRDetailModel(client *forgejo.Client, repo string) PRDetailModel {
	return PRDetailModel{client: client, repo: repo}
}

func (m PRDetailModel) SetPR(pr *forgejo.PullRequest) PRDetailModel {
	m.pr = pr
	m.loading = pr != nil
	m.files = nil
	m.reviews = nil
	return m
}

func (m PRDetailModel) SetWidth(w int) PRDetailModel {
	m.width = w
	return m
}

func (m PRDetailModel) SetFiles(files []forgejo.PRFile) PRDetailModel {
	m.files = files
	return m
}

func (m PRDetailModel) SetReviews(reviews []forgejo.Review) PRDetailModel {
	m.reviews = reviews
	m.loading = false
	return m
}

func (m PRDetailModel) LoadPR() tea.Cmd {
	if m.pr == nil || m.pr.Number <= 0 {
		return nil
	}
	pr := m.pr
	client := m.client
	repo := m.repo
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		fullPR, err := client.GetPR(ctx, repo, pr.Number)
		if err != nil {
			fullPR = pr
		}
		files, _ := client.GetPRFiles(ctx, repo, pr.Number)
		reviews, _ := client.ListPRReviews(ctx, repo, pr.Number)
		if files == nil {
			files = []forgejo.PRFile{}
		}
		if reviews == nil {
			reviews = []forgejo.Review{}
		}
		return prDetailLoadedMsg{pr: fullPR, files: files, reviews: reviews}
	}
}

func (m PRDetailModel) View() string {
	if m.pr == nil {
		return dimStyle.Render("Select a PR from the sidebar")
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render(fmt.Sprintf("PR #%d: %s", m.pr.Number, m.pr.Title)))
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("base: %s  ←  head: %s\n", m.pr.Base.Ref, m.pr.Head.Ref))

	mergeable := "not mergeable"
	if m.pr.Mergeable && !m.pr.HasConflicts {
		mergeable = activeStyle.Render("mergeable")
	} else if m.pr.Merged {
		mergeable = stateDone.Render("merged")
	} else if m.pr.HasConflicts {
		mergeable = errorStyle.Render("has conflicts")
	}
	b.WriteString(fmt.Sprintf("Status: %s\n", mergeable))

	if m.loading {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("Loading files and reviews..."))
	}

	if len(m.files) > 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("── Changed files (%d) ──", len(m.files))))
		b.WriteString("\n")
		for _, f := range m.files {
			statusChar := "M"
			switch f.Status {
			case "added":
				statusChar = stateReady.Render("A")
			case "deleted":
				statusChar = errorStyle.Render("D")
			case "renamed":
				statusChar = dimStyle.Render("R")
			}
			b.WriteString(fmt.Sprintf("  %s  %-40s  +%d -%d\n",
				statusChar, truncate(f.Filename, 40), f.Additions, f.Deletions))
		}
	}

	if len(m.reviews) > 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("── Reviews ──"))
		b.WriteString("\n")
		for _, r := range m.reviews {
			if r.User != nil {
				reviewState := r.State
				switch reviewState {
				case "APPROVED":
					reviewState = activeStyle.Render(reviewState)
				case "REQUEST_CHANGES":
					reviewState = errorStyle.Render(reviewState)
				case "PENDING":
					reviewState = dimStyle.Render(reviewState)
				default:
					reviewState = dimStyle.Render(reviewState)
				}
				b.WriteString(fmt.Sprintf("  %s  %s", r.User.Login, reviewState))
				if r.Body != "" {
					b.WriteString(fmt.Sprintf("  %q", truncate(r.Body, 60)))
				}
				b.WriteString("\n")
			}
		}
	}

	return detailStyle.Width(m.width - 2).Render(b.String())
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}