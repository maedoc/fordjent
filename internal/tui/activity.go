package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fordjent/fordjent/internal/lifecycle"
)

type ActivityModel struct {
	deliveries  []ActivityDelivery
	transitions []ActivityTransition
	turns       []ActivityTurn
	width       int
	height      int
	scroll      int
}

type ActivityDelivery struct {
	EventType string
	Action    string
	Repo      string
	Number    int
	Sender    string
	Status    string
	Timestamp string
}

type ActivityTransition struct {
	SessionKey string
	FromState  string
	ToState    string
	Reason     string
	Timestamp  string
}

type ActivityTurn struct {
	SessionKey string
	Turn       int
	ToolCalls  int
	LatencyMs  int
	TokensIn   int
	TokensOut  int
	Timestamp  string
}

func NewActivityModel() ActivityModel {
	return ActivityModel{}
}

func (m ActivityModel) SetSize(w, h int) ActivityModel {
	m.width = w
	m.height = h
	return m
}

func (m ActivityModel) AddSSEEvent(evt lifecycle.SSEEvent) ActivityModel {
	switch evt.Type {
	case "delivery":
		var d ActivityDelivery
		if v, ok := parseJSONString(evt.Data, "event_type"); ok {
			d.EventType = v
		}
		if v, ok := parseJSONString(evt.Data, "action"); ok {
			d.Action = v
		}
		if v, ok := parseJSONString(evt.Data, "repository"); ok {
			d.Repo = v
		}
		if v, ok := parseJSONInt(evt.Data, "number"); ok {
			d.Number = v
		}
		if v, ok := parseJSONString(evt.Data, "sender"); ok {
			d.Sender = v
		}
		if v, ok := parseJSONString(evt.Data, "status"); ok {
			d.Status = v
		}
		if v, ok := parseJSONString(evt.Data, "timestamp"); ok {
			d.Timestamp = v
		}
		m.deliveries = prependLimited(m.deliveries, d, 50)
	case "transition":
		var t ActivityTransition
		if v, ok := parseJSONString(evt.Data, "session_key"); ok {
			t.SessionKey = v
		}
		if v, ok := parseJSONString(evt.Data, "from_state"); ok {
			t.FromState = v
		}
		if v, ok := parseJSONString(evt.Data, "to_state"); ok {
			t.ToState = v
		}
		if v, ok := parseJSONString(evt.Data, "reason"); ok {
			t.Reason = v
		}
		if v, ok := parseJSONString(evt.Data, "timestamp"); ok {
			t.Timestamp = v
		}
		m.transitions = prependLimited(m.transitions, t, 50)
	case "turn":
		var t ActivityTurn
		if v, ok := parseJSONString(evt.Data, "session_key"); ok {
			t.SessionKey = v
		}
		if n, ok := parseJSONInt(evt.Data, "turn"); ok {
			t.Turn = n
		}
		if n, ok := parseJSONInt(evt.Data, "tool_calls"); ok {
			t.ToolCalls = n
		}
		if n, ok := parseJSONInt(evt.Data, "latency_ms"); ok {
			t.LatencyMs = n
		}
		if n, ok := parseJSONInt(evt.Data, "tokens_in"); ok {
			t.TokensIn = n
		}
		if n, ok := parseJSONInt(evt.Data, "tokens_out"); ok {
			t.TokensOut = n
		}
		if v, ok := parseJSONString(evt.Data, "timestamp"); ok {
			t.Timestamp = v
		}
		m.turns = prependLimited(m.turns, t, 50)
	}
	return m
}

func (m ActivityModel) View() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Activity Feed"))
	b.WriteString("\n\n")

	if len(m.transitions) > 0 {
		b.WriteString(dimStyle.Render("── Transitions ──"))
		b.WriteString("\n")
		for _, t := range m.transitions {
			key := t.SessionKey
			if idx := strings.LastIndex(key, "/"); idx >= 0 {
				key = key[idx+1:]
			}
			b.WriteString(fmt.Sprintf("  %s  %s → %s",
				dimStyle.Render(shortTime(t.Timestamp)),
				StateBadge(t.FromState),
				StateBadge(t.ToState)))
			if t.Reason != "" {
				b.WriteString(dimStyle.Render(fmt.Sprintf(" (%s)", t.Reason)))
			}
			b.WriteString(dimStyle.Render(fmt.Sprintf("  %s", key)))
			b.WriteString("\n")
		}
	}

	if len(m.turns) > 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("── Turns ──"))
		b.WriteString("\n")
		for _, t := range m.turns {
			key := t.SessionKey
			if idx := strings.LastIndex(key, "/"); idx >= 0 {
				key = key[idx+1:]
			}
			b.WriteString(fmt.Sprintf("  %s  %s turn %d  🔧%d  %dms  %s/%s tkn\n",
				dimStyle.Render(shortTime(t.Timestamp)),
				dimStyle.Render(key),
				t.Turn,
				t.ToolCalls,
				t.LatencyMs,
				formatNum(int64(t.TokensIn)),
				formatNum(int64(t.TokensOut))))
		}
	}

	if len(m.deliveries) > 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("── Webhooks ──"))
		b.WriteString("\n")
		for _, d := range m.deliveries {
			b.WriteString(fmt.Sprintf("  %s  %s.%s  #%d  %s\n",
				dimStyle.Render(shortTime(d.Timestamp)),
				d.EventType,
				d.Action,
				d.Number,
				activeStyle.Render(d.Status)))
		}
	}

	if len(m.transitions) == 0 && len(m.turns) == 0 && len(m.deliveries) == 0 {
		b.WriteString(dimStyle.Render("No activity yet. Events will appear here in real-time via SSE."))
	}

	return detailStyle.Width(m.width - 2).Render(b.String())
}

func prependLimited[T any](slice []T, item T, max int) []T {
	result := append([]T{item}, slice...)
	if len(result) > max {
		result = result[:max]
	}
	return result
}

func shortTime(ts string) string {
	if len(ts) > 19 {
		return ts[11:19]
	}
	return ts
}

func parseJSONString(data, key string) (string, bool) {
	search := fmt.Sprintf(`"%s":`, key)
	idx := strings.Index(data, search)
	if idx < 0 {
		return "", false
	}
	start := idx + len(search)
	if start >= len(data) {
		return "", false
	}
	if data[start] == '"' {
		start++
	} else {
		return "", false
	}
	end := strings.IndexByte(data[start:], '"')
	if end < 0 {
		return "", false
	}
	return data[start : start+end], true
}

func parseJSONInt(data, key string) (int, bool) {
	search := fmt.Sprintf(`"%s":`, key)
	idx := strings.Index(data, search)
	if idx < 0 {
		return 0, false
	}
	start := idx + len(search)
	for start < len(data) && data[start] == ' ' {
		start++
	}
	if start >= len(data) {
		return 0, false
	}
	end := start
	for end < len(data) && ((data[end] >= '0' && data[end] <= '9') || data[end] == '-') {
		end++
	}
	if end == start {
		return 0, false
	}
	n, err := strconv.Atoi(data[start:end])
	if err != nil {
		return 0, false
	}
	return n, true
}