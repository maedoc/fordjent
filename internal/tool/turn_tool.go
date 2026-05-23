package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// TurnGetter provides current and max turn counts.
type TurnGetter func() (current, max int)

// NewTurnTool creates a tool that reports turn budget usage.
func NewTurnTool(get TurnGetter) Tool {
	return &turnTool{get: get}
}

type turnTool struct {
	get TurnGetter
}

func (t *turnTool) Name() string        { return "turn" }
func (t *turnTool) Description() string { return "Check how many turns you have used and how many remain. Use this periodically to monitor your progress and avoid running out of turns." }
func (t *turnTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func (t *turnTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	current, max := t.get()
	pct := float64(current) / float64(max) * 100
	remaining := max - current

	var urgency string
	switch {
	case pct >= 90:
		urgency = "🚨 CRITICAL: Only a few turns left. Commit and create a PR immediately if you have code."
	case pct >= 80:
		urgency = "⚠️ Most of your budget is used. Stop exploring. Commit your work now."
	case pct >= 60:
		urgency = "You've used over half your turns. Focus on completing your task and creating a PR."
	case pct >= 40:
		urgency = "Good progress. Keep working steadily toward completion."
	default:
		urgency = "Plenty of turns remaining. Work at a comfortable pace."
	}

	return fmt.Sprintf("Turn %d/%d (%.0f%%, %d remaining).\n%s", current, max, pct, remaining, urgency), nil
}
