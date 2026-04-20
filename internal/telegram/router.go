package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tb "gopkg.in/telebot.v4"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
)

// Router receives Telegram messages, normalizes them to events, and publishes
// to the event bus. It also manages forum topic creation and mapping.
type Router struct {
	cfg   *config.Config
	bus   *event.Bus
	bot   *tb.Bot
	store *MappingStore
	ctx   context.Context
}

// NewRouter creates a new Telegram router.
// Returns nil if Telegram is not enabled in config.
func NewRouter(cfg *config.Config, bus *event.Bus) (*Router, error) {
	if !cfg.Telegram.Enabled {
		return nil, nil
	}

	pollTimeout := 10 * time.Second
	if cfg.Telegram.PollTimeout > 0 {
		pollTimeout = time.Duration(cfg.Telegram.PollTimeout) * time.Second
	}

	bot, err := tb.NewBot(tb.Settings{
		Token:  cfg.Telegram.Token,
		Poller: &tb.LongPoller{Timeout: pollTimeout},
	})
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}

	dbPath := filepath.Join(cfg.Agent.WorkDir, "telegram", "mappings.db")
	store, err := NewMappingStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("topic store init: %w", err)
	}

	r := &Router{
		cfg:   cfg,
		bus:   bus,
		bot:   bot,
		store: store,
	}

	r.registerHandlers()
	return r, nil
}

// Start begins long polling for Telegram updates.
func (r *Router) Start(ctx context.Context) {
	r.ctx = ctx
	go func() {
		<-ctx.Done()
		r.bot.Stop()
	}()

	slog.Info("telegram bot starting", "bot_username", r.bot.Me.Username)
	r.bot.Start()
}

// Close releases resources held by the router.
func (r *Router) Close() error {
	return r.store.Close()
}

// Bot returns the underlying telebot.Bot (for topic creation from other components).
func (r *Router) Bot() *tb.Bot {
	return r.bot
}

// Store returns the topic mapping store.
func (r *Router) Store() *MappingStore {
	return r.store
}

func (r *Router) registerHandlers() {
	// /start command
	r.bot.Handle("/start", func(c tb.Context) error {
		if !r.cfg.IsChatAllowed(c.Chat().ID) {
			return c.Reply("This bot is not configured for this chat.")
		}
		return c.Reply("Fordjent agent harness. Use /status to check health.\n" +
			"Send messages in forum topics to interact with the agent.")
	})

	// /status command
	r.bot.Handle("/status", func(c tb.Context) error {
		if !r.cfg.IsChatAllowed(c.Chat().ID) {
			return nil
		}
		return c.Reply("✅ Fordjent is running.")
	})

	// /bind <repo> — bind this chat to a Forgejo repository
	r.bot.Handle("/bind", func(c tb.Context) error {
		if !r.cfg.IsChatAllowed(c.Chat().ID) {
			return c.Reply("Not allowed.")
		}
		args := strings.TrimSpace(c.Message().Payload)
		if args == "" {
			return c.Reply("Usage: /bind <owner/repo>")
		}
		// This is an admin command — in a real deployment, check user permissions.
		// For now, we just acknowledge it. The real binding is via config.
		return c.Reply("Bindings are configured in fordjent.yaml and require a restart.\n" +
			fmt.Sprintf("Add telegram.chat_bindings.%d.repository: \"%s\"", c.Chat().ID, args))
	})

	// All text messages (in topics or general)
	r.bot.Handle(tb.OnText, func(c tb.Context) error {
		return r.handleMessage(c)
	})
}

func (r *Router) handleMessage(c tb.Context) error {
	chatID := c.Chat().ID
	msg := c.Message()

	// Enforce chat allowlist
	if !r.cfg.IsChatAllowed(chatID) {
		slog.Debug("telegram: ignoring message from unauthorized chat",
			"chat_id", chatID)
		return nil
	}

	// Enforce user allowlist
	sender := msg.Sender
	if sender != nil && !r.cfg.IsUserAllowed(chatID, sender.Username) {
		slog.Debug("telegram: ignoring message from unauthorized user",
			"username", sender.Username, "chat_id", chatID)
		return nil
	}

	threadID := c.ThreadID()
	text := msg.Text

	// Determine the repository for this chat
	repo, hasBinding := r.cfg.RepositoryForChat(chatID)
	if !hasBinding {
		slog.Debug("telegram: no repository binding for chat", "chat_id", chatID)
		return c.Reply("No repository bound to this chat. Configure in fordjent.yaml.")
	}

	// Extract sender name
	senderName := "unknown"
	if sender != nil {
		senderName = sender.Username
		if senderName == "" {
			senderName = sender.FirstName
		}
	}

	// Build event using pure normalization function
	evt := NormalizeMessage(chatID, threadID, msg.ID, repo, senderName, text, r.store)
	if evt == nil {
		// No session mapped — not routed to agent
		return nil
	}

	slog.Info("telegram: normalized event",
		"event_id", evt.ID,
		"session_key", evt.SessionKey,
		"sender", senderName,
	)

	r.bus.Publish(r.ctx, evt)
	return nil
}

// NormalizeMessage is a pure function that converts a Telegram message into an
// internal Event. Returns nil if the message should not be routed to an agent
// (e.g., no topic mapping exists, or the message is not in a topic thread).
// This function is separated from handleMessage for testability.
func NormalizeMessage(chatID int64, threadID, messageID int, repo, sender, text string, store *MappingStore) *event.Event {
	var sessionKey string
	var issueNumber, prNumber int

	if threadID != 0 {
		mapping, err := store.GetByThread(chatID, threadID)
		if err != nil {
			slog.Error("telegram: failed to lookup topic mapping", "error", err)
			return nil
		}
		if mapping != nil {
			sessionKey = mapping.SessionKey
			issueNumber = mapping.IssueNumber
			prNumber = mapping.PRNumber
		} else {
			// No mapping for this thread — unmapped topic or General topic.
			slog.Debug("telegram: no mapping for thread", "chat_id", chatID, "thread_id", threadID)
			return nil
		}
	}

	if sessionKey == "" {
		return nil
	}

	evt := event.NewEvent(event.TelegramMessage, repo, issueNumber, prNumber, sender, "message")
	evt.SessionKey = sessionKey
	evt.Payload = map[string]interface{}{
		"source":     "telegram",
		"chat_id":    strconv.FormatInt(chatID, 10),
		"thread_id":  strconv.Itoa(threadID),
		"message_id": strconv.Itoa(messageID),
		"from_user":  sender,
		"text":       text,
	}
	return evt
}

// EnsureTopic creates a forum topic for the given session key if one doesn't exist.
// Returns the thread ID of the (existing or new) topic.
func (r *Router) EnsureTopic(ctx context.Context, chatID int64, name, sessionKey, repository string, issueNumber, prNumber int) (int, error) {
	// Check if mapping already exists
	mapping, err := r.store.GetBySessionKey(sessionKey)
	if err != nil {
		return 0, err
	}
	if mapping != nil {
		return mapping.ThreadID, nil
	}

	// Create the topic
	chat := &tb.Chat{ID: chatID}
	topic, err := r.bot.CreateTopic(chat, &tb.Topic{Name: name})
	if err != nil {
		return 0, fmt.Errorf("create topic: %w", err)
	}

	// Store the mapping
	mapping = &TopicMapping{
		ChatID:      chatID,
		ThreadID:   topic.ThreadID,
		Repository: repository,
		SessionKey: sessionKey,
		IssueNumber: issueNumber,
		PRNumber:   prNumber,
	}
	if err := r.store.CreateMapping(mapping); err != nil {
		return 0, fmt.Errorf("store mapping: %w", err)
	}

	slog.Info("telegram: created topic",
		"name", name,
		"thread_id", topic.ThreadID,
		"session_key", sessionKey,
	)

	return topic.ThreadID, nil
}
