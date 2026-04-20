# Fordjent Telegram Integration: Research & Implementation Plan

## Executive Summary

Add a Telegram bot interface to Fordjent that enables chat-based interaction with the agent alongside the existing Forgejo webhook workflow. A Telegram supergroup (with forum/topics enabled) maps to a repository; each issue or PR gets its own topic thread.

---

## 1. Library Selection

| Library | Forum Topic Support | Dependencies | Handler Pattern | Verdict |
|---------|--------------------|--------------|-----------------|---------|
| `go-telegram-bot-api/telegram-bot-api/v5` | ❌ None | net/http only | Manual channel loop | Rejected — no Forum Topic types |
| `mymmrac/telego` | ✅ Full (`CreateForumTopicParams`, `MessageThreadID`) | fasthttp, sonic | `<-chan Update` + manual dispatch | Viable but heavy deps |
| **`gopkg.in/telebot.v4`** | ✅ Full (`CreateTopic`, `EditTopic`, `ThreadID` in `SendOptions`) | yaml, viper | `b.Handle(endpoint, handler)` middleware | **Chosen** — lightest, cleanest API |

**Decision: `gopkg.in/telebot.v4` (telebot)**

Rationale:
- Full Forum Topic CRUD (`CreateTopic`, `EditTopic`, `CloseTopic`, `ReopenTopic`, `DeleteTopic`)
- `SendOptions.ThreadID` for targeting specific topics
- `Context.Topic()` and `Context.ThreadID()` for receiving topic-scoped messages
- Minimal dependency tree (only yaml + viper)
- Clean handler/middleware pattern matches our event-routing model
- v4 is actively maintained, Go 1.16+

---

## 2. Architecture

```
                         ┌──────────────────────────────────┐
                         │         Fordjent Binary          │
                         │                                  │
  Telegram ◀──long────▶ │  ┌─────────────┐                 │
  Bot API    poll        │  │  Telegram    │                 │
                         │  │  Router      │                 │
                         │  └──────┬──────┘                 │
                         │         │                        │
  Forgejo ◀──webhook──▶ │  ┌──────┴──────┐                 │
  (existing)             │  │  Event Bus  │ ◀─── ─── ─── ── ┤
                         │  └──────┬──────┘                 │
                         │         │                        │
                         │  ┌──────┴──────────────────┐     │
                         │  │   Session Manager        │     │
                         │  │   (per session_key)      │     │
                         │  └──────┬──────────────────┘     │
                         │         │                        │
                         │    ┌────┴────┐                   │
                         │    │  Agent   │                   │
                         │    │  (LLM +  │                   │
                         │    │  Tools)  │                   │
                         │    └─────────┘                   │
                         └──────────────────────────────────┘
```

### Key Design: Event Source Symmetry

Both Forgejo webhooks and Telegram messages normalize to the same `event.Event` type. The `SessionManager` doesn't know or care which source produced the event — it just routes by `session_key`.

New event types added:
```go
TelegramMessage    Type = "telegram.message"
TelegramCommand    Type = "telegram.command"
```

The `Event.Payload` carries source-specific metadata:
- Forgejo: webhook payload
- Telegram: `chat_id`, `thread_id`, `from_user`, `message_id`

### Agent Response Delivery

The agent currently calls `addReaction()` (Forgejo emoji) for status. For Telegram, the response delivery is different — the agent's final text output needs to be posted back to the originating Telegram topic.

**Approach:** Add a `ResponseWriter` interface to the agent. Each source provides its own implementation:

```go
type ResponseWriter interface {
    Acknowledge(ctx context.Context, evt *event.Event) error    // "thinking..." indicator
    Respond(ctx context.Context, evt *event.Event, msg string) error  // Post final response
    Error(ctx context.Context, evt *event.Event, err error) error     // Post error message
}
```

- Forgejo: `Acknowledge` → emoji reaction, `Respond` → issue comment
- Telegram: `Acknowledge` → typing indicator, `Respond` → message to topic thread

---

## 3. Forum Topic Mapping

### Mapping Rule

```
Telegram Supergroup (forum mode)  ←→  Forgejo Repository
  Topic "Issue #42"               ←→  session_key "org/repo/issues/42"
  Topic "PR #7"                   ←→  session_key "org/repo/pulls/7"
  General topic                   ←→  repository-level commands
```

### Topic Lifecycle

1. **Issue/PR opened** (via Forgejo webhook):
   - Agent receives `issues.opened` event
   - Telegram Router creates a topic: `CreateTopic(chat, &Topic{Name: "Issue #42: <title>"})`
   - Stores mapping: `session_key → thread_id` in SQLite

2. **User sends message in topic** (via Telegram):
   - Telegram Router looks up `thread_id → session_key` mapping
   - Creates normalized `Event` with the message
   - Publishes to Event Bus → routed to existing session (affinity)

3. **Agent responds**:
   - Agent calls `ResponseWriter.Respond()`
   - Telegram implementation sends `SendMessage` with `ThreadID` set to the topic's thread ID

### Mapping Storage

```go
type TopicMapping struct {
    ChatID     int64  // Telegram supergroup ID
    ThreadID   int    // Forum topic thread ID
    Repository string // "org/repo"
    SessionKey string // "org/repo/issues/42"
    IssueNumber int   // 0 for PRs
    PRNumber    int   // 0 for issues
}
```

Storage: SQLite table (single file in workdir). Queries by `thread_id` (incoming messages) and `session_key` (outgoing responses).

---

## 4. Configuration

```yaml
telegram:
  enabled: true
  token: "${TELEGRAM_BOT_TOKEN}"
  # Allowed chats (supergroup IDs). Empty = allow all.
  allowed_chats:
    - -1001234567890  # supergroup ID for org/repo
  # Repository binding per chat
  chat_bindings:
    -1001234567890:
      repository: "org/repo"
      # Who can trigger the agent (empty = everyone in group)
      allowed_users: []
  # Long polling timeout in seconds
  poll_timeout: 10
```

---

## 5. File Structure

```
internal/
├── telegram/
│   ├── router.go         # Bot init, long polling, message → Event normalization
│   ├── topics.go         # Forum topic CRUD + mapping storage
│   ├── responder.go      # ResponseWriter impl for Telegram
│   ├── router_test.go    # Unit tests
│   └── topics_test.go    # Topic mapping tests
├── session/
│   ├── manager.go        # Modified: ResponseWriter injection
│   └── manager_test.go
└── config/
    └── config.go         # Modified: add TelegramConfig
```

---

## 6. Implementation Phases

### Phase 1: Foundation (core integration)

**Step 1.1: Config + dependency**
- Add `TelegramConfig` struct to `config.go`
- `go get gopkg.in/telebot.v4`
- Add telegram section to `fordjent.yaml`

**Step 1.2: Telegram Router (`internal/telegram/router.go`)**
- `Router` struct wrapping `telebot.Bot`
- Long polling initialization
- `Start(ctx)` / `Stop()` lifecycle
- Message handler: normalize incoming messages to `event.Event`
- Command handler: `/start`, `/bind <repo>`, `/status`
- Chat allowlist enforcement

**Step 1.3: Event normalization**
- Map `telebot.Context` → `event.Event`:
  - `Event.Type` = `telegram.message`
  - `Event.Sender` = `c.Sender().Username`
  - `Event.SessionKey` = lookup from topic mapping
  - `Event.Payload` = `{chat_id, thread_id, message_id, text}`

**Step 1.4: Wire into main.go**
- If `telegram.enabled`, create `telegram.Router`, start long polling
- Both routers feed the same `event.Bus`

### Phase 2: Forum Topics

**Step 2.1: Topic mapping storage (`internal/telegram/topics.go`)**
- SQLite schema: `topic_mappings(chat_id, thread_id, repository, session_key, issue_number, pr_number)`
- `CreateMapping`, `GetMappingByThread`, `GetMappingBySessionKey`
- `AutoCreateTopic`: called when Forgejo webhook fires for a new issue/PR

**Step 2.2: Topic auto-creation**
- Hook into existing `session.Manager` session creation
- When a new session is created and a Telegram binding exists for the repo, create a topic
- Store the `thread_id → session_key` mapping

**Step 2.3: Bidirectional sync**
- Forgejo comment on issue → agent responds → Telegram message to topic
- Telegram message in topic → agent processes → Forgejo comment on issue (via existing tool)

### Phase 3: Response Delivery

**Step 3.1: ResponseWriter interface**
- Define `ResponseWriter` in `session` package
- Forgejo implementation: wraps existing `addReaction` + comment posting
- Telegram implementation: `c.Bot().Send()` with `SendOptions.ThreadID`

**Step 3.2: Streaming responses**
- Telegram doesn't support true streaming, but we can:
  - Send "thinking..." message, edit it when done
  - Or split long responses into multiple messages (4096 char limit)

### Phase 4: Harden & Polish

- Rate limiting (Telegram API: 30 msg/sec to same chat)
- Message length splitting (4096 char limit)
- Markdown formatting (Telegram MarkdownV2 vs agent output)
- `/status` command showing active sessions
- Admin commands for binding/unbinding repos
- Error recovery (topic not found, chat kicked, etc.)

---

## 7. Security Considerations

| Concern | Mitigation |
|---------|-----------|
| Unauthorized chat access | `allowed_chats` allowlist, ignore all other chats |
| Unauthorized user commands | `allowed_users` per chat binding |
| Bot token exposure | Same env-var expansion as existing secrets |
| Prompt injection via Telegram | Agent treats Telegram input same as Forgejo input (user-controlled) |
| Rate limiting | Telegram API has built-in rate limits; add backoff on 429 |

---

## 8. Testing Strategy

| Component | Approach |
|-----------|----------|
| `telegram.Router` | Mock `telebot.Bot` interface; test message → Event normalization |
| `topics.MappingStore` | SQLite in-memory; test CRUD, lookup by thread and session key |
| `telegram.Responder` | Mock bot; verify `Send` called with correct `ThreadID` |
| Integration | Manual test with real Telegram bot + test supergroup |
| Race detector | All tests run with `-race` |

---

## 9. Dependencies Added

```
gopkg.in/telebot.v4      # Telegram Bot API client
github.com/mattn/go-sqlite3  # SQLite for topic mapping (or modernc.org/sqlite for pure Go)
```

Only 2 new dependencies. `telebot.v4` pulls in `goccy/go-yaml` and `spf13/viper` (already common in Go projects). SQLite can use `modernc.org/sqlite` (pure Go, no CGO) to keep the single-binary deployment model.

---

## 10. Open Questions

1. **Message threading model:** Should the agent reply as a direct message in the topic, or as a reply to the specific user message? Reply-to gives better context but clutters.
2. **Multi-repo per group:** Should one Telegram group support multiple repos (prefix-based routing like `@bot issue org/repo#42`), or one group per repo?
3. **Edit vs new message:** When the agent's response is long and being streamed, should we edit a single message or post multiple?
4. **Cross-posting:** Should Forgejo webhook events always create/update Telegram topics, or only when explicitly subscribed?

---

Ready for review. If approved, I'll implement Phase 1 + Phase 2 (the core bidirectional flow) as a single commit.
