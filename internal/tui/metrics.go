package tui

import (
	"fmt"
	"strings"
)

type MetricsModel struct {
	status    *StatusResponse
	tokenHist []TokenMinute
	width     int
	height    int
}

func NewMetricsModel() MetricsModel {
	return MetricsModel{}
}

func (m MetricsModel) SetStatus(s *StatusResponse) MetricsModel {
	m.status = s
	return m
}

func (m MetricsModel) SetTokenHistory(hist []TokenMinute) MetricsModel {
	m.tokenHist = hist
	return m
}

func (m MetricsModel) SetSize(w, h int) MetricsModel {
	m.width = w
	m.height = h
	return m
}

func (m MetricsModel) View() string {
	if m.status == nil {
		return dimStyle.Render("Loading metrics...")
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("Metrics"))
	b.WriteString("\n\n")

	s := m.status

	b.WriteString(fmt.Sprintf("Sessions: %d active  │  Failed: %d",
		s.Lifecycle.ActiveSessions, s.Lifecycle.FailedSessions))
	b.WriteString("\n")

	b.WriteString(fmt.Sprintf("LLM calls: %d  │  Retries: %d",
		s.Metrics.LLMCallsTotal, s.Metrics.LLMRetriesTotal))
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("Tokens: %s in / %s out  │  Cost: $%.4f",
		formatNum(s.Metrics.InputTokens), formatNum(s.Metrics.OutputTokens), s.Metrics.CostUSD))
	b.WriteString("\n")

	if len(s.ByModel) > 0 {
		b.WriteString("\n")
		b.WriteString(titleStyle.Render("Per Model"))
		b.WriteString("\n")
		for _, row := range s.ByModel {
			b.WriteString(fmt.Sprintf("  %-20s  %d calls  %s/%s  $%.4f\n",
				row.Provider+"/"+row.Model,
				row.Calls,
				formatNum(row.InputTokens),
				formatNum(row.OutputTokens),
				row.CostUSD))
		}
	}

	if len(s.Lifecycle.RecentTurns) > 0 {
		b.WriteString("\n")
		b.WriteString(titleStyle.Render("Recent Turns"))
		b.WriteString("\n")
		shown := s.Lifecycle.RecentTurns
		if len(shown) > 10 {
			shown = shown[:10]
		}
		for _, t := range shown {
			key := t.SessionKey
			if idx := strings.LastIndex(key, "/"); idx >= 0 {
				key = key[idx+1:]
			}
			b.WriteString(fmt.Sprintf("  %s  turn %d  %d tools  %dms  %s/%s tkn\n",
				dimStyle.Render(key),
				t.Turn,
				t.ToolCalls,
				t.LatencyMs,
				formatNum(int64(t.TokensIn)),
				formatNum(int64(t.TokensOut))))
		}
	}

	if len(m.tokenHist) > 0 {
		b.WriteString("\n")
		b.WriteString(titleStyle.Render("Tokens/min (last hour)"))
		b.WriteString("\n")
		var vals []int64
		for _, tm := range m.tokenHist {
			vals = append(vals, tm.TotalTokens)
		}
		b.WriteString("  ")
		b.WriteString(Sparkline(vals, m.width-4))
		b.WriteString("\n")
	}

	return detailStyle.Width(m.width - 2).Render(b.String())
}