package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	tb "gopkg.in/telebot.v4"

	"github.com/fordjent/fordjent/internal/event"
)

// Responder delivers agent responses to Telegram topics.
// NOTE: This is Phase 3 scaffolding. It will be wired into the agent's
// ResponseWriter interface once that is implemented.
type Responder struct {
	bot    *tb.Bot
	store  *MappingStore
}

// NewResponder creates a new Telegram response writer.
func NewResponder(bot *tb.Bot, store *MappingStore) *Responder {
	return &Responder{bot: bot, store: store}
}

// Acknowledge sends a "thinking" indicator by posting a placeholder message.
// TODO: respect ctx cancellation for graceful shutdown.
func (r *Responder) Acknowledge(ctx context.Context, evt *event.Event) error {
	mapping, err := r.store.GetBySessionKey(evt.SessionKey)
	if err != nil || mapping == nil {
		return nil // No Telegram binding for this session
	}

	chat := &tb.Chat{ID: mapping.ChatID}
	_, err = r.bot.Send(chat, "⏳ Thinking...",
		&tb.SendOptions{ThreadID: mapping.ThreadID})
	if err != nil {
		return fmt.Errorf("telegram acknowledge: %w", err)
	}
	return nil
}

// SendResponse posts the agent's final response to the originating topic.
// Splits messages longer than 4000 characters into multiple parts.
// TODO: respect ctx cancellation for graceful shutdown.
func (r *Responder) SendResponse(ctx context.Context, evt *event.Event, msg string) error {
	mapping, err := r.store.GetBySessionKey(evt.SessionKey)
	if err != nil || mapping == nil {
		return nil
	}

	chat := &tb.Chat{ID: mapping.ChatID}
	opts := &tb.SendOptions{ThreadID: mapping.ThreadID}

	// Split long messages
	parts := splitMessage(msg, 4000)
	for _, part := range parts {
		if _, err := r.bot.Send(chat, part, opts); err != nil {
			slog.Error("telegram respond failed", "error", err, "session_key", evt.SessionKey)
			return fmt.Errorf("telegram respond: %w", err)
		}
	}
	return nil
}

// Error posts an error message to the originating topic.
// TODO: respect ctx cancellation for graceful shutdown.
func (r *Responder) Error(ctx context.Context, evt *event.Event, agentErr error) error {
	mapping, err := r.store.GetBySessionKey(evt.SessionKey)
	if err != nil || mapping == nil {
		return nil
	}

	chat := &tb.Chat{ID: mapping.ChatID}
	errMsg := fmt.Sprintf("❌ Agent error: %s", agentErr.Error())
	if len(errMsg) > 4000 {
		errMsg = errMsg[:4000]
	}

	_, err = r.bot.Send(chat, errMsg,
		&tb.SendOptions{ThreadID: mapping.ThreadID})
	if err != nil {
		return fmt.Errorf("telegram error: %w", err)
	}
	return nil
}

// splitMessage splits a message into parts of at most maxLen characters,
// trying to break at newline boundaries.
func splitMessage(msg string, maxLen int) []string {
	if len(msg) <= maxLen {
		return []string{msg}
	}

	var parts []string
	for len(msg) > maxLen {
		// Try to find a newline near the limit
		splitAt := strings.LastIndex(msg[:maxLen], "\n")
		if splitAt < maxLen/2 {
			// No good newline break, just cut at limit
			splitAt = maxLen
		}
		parts = append(parts, msg[:splitAt])
		msg = msg[splitAt:]
		// Trim leading newlines
		msg = strings.TrimPrefix(msg, "\n")
	}
	if len(msg) > 0 {
		parts = append(parts, msg)
	}
	return parts
}
