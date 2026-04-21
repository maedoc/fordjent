# Fordjent

A Forgejo-driven AI agent harness written in Go. Fordjent listens for Forgejo webhook events (and Telegram messages), spawns per-issue agent sessions, and uses an LLM with tool-calling to autonomously triage, comment, and create pull requests.

## Features

- **Webhook-driven** — Receives Forgejo events via HMAC-validated HTTP webhooks
- **Telegram interface** — Chat with the agent via Telegram forum topics mapped to issues/PRs
- **Session affinity** — Events for the same issue/PR route to the same agent session for serial processing
- **Tool calling** — 10 built-in tools (Forgejo API, bash, file I/O, git, code search, reactions)
- **Loop prevention** — Multi-layer defense: commit prefix filtering, bot sender filtering, branch protection
- **Memory system** — JSONL audit log + git notes for persistent agent reasoning traces
- **Single binary** — Pure Go, no CGO required (SQLite via modernc.org/sqlite)
- **Docker-ready** — Multi-stage Dockerfile and Compose stack included

## Quick Start

### Binary

```bash
go build -o fordjent ./cmd/fordjent

# Set required secrets
export FORGEJO_TOKEN=your-repo-scoped-token
export OPENAI_API_KEY=your-api-key

# Edit config (set forgejo URL, repository, etc.)
cp fordjent.yaml my-config.yaml

./fordjent -config my-config.yaml
```

### Docker Compose

```bash
cp .env.example .env
# Edit .env with your tokens

# Edit fordjent.yaml to point at your Forgejo instance
docker compose up -d
```

### Forgejo Setup

1. Go to **Repository → Settings → Webhooks → Add Webhook**
2. Set **Target URL** to `http://your-host:8080/acp/v1/events`
3. Set **Secret** to match `webhook.secret` in your config
4. Select events: `issues`, `issue_comment`, `pull_request`, `pull_request_review_comment`
5. Create a repository-scoped access token and set it as `FORGEJO_TOKEN`

### Telegram Setup (optional)

1. Create a bot via [@BotFather](https://t.me/BotFather)
2. Create a supergroup and enable **Topics** (forum mode)
3. Add the bot as an admin with "Manage Topics" permission
4. Configure in `fordjent.yaml`:

```yaml
telegram:
  enabled: true
  token: "${TELEGRAM_BOT_TOKEN}"
  allowed_chats: [-1001234567890]
  chat_bindings:
    -1001234567890:
      repository: "org/repo"
      allowed_users: ["your_username"]
```

5. When an issue or PR is opened, Fordjent creates a forum topic for it. Messages in that topic are routed to the agent session for that issue.

## Architecture

```
                          ┌──────────────────┐
                          │     Forgejo      │
                          │  (Webhooks, API) │
                          └────────┬─────────┘
                                   │ HTTP POST (HMAC)
                                   ▼
┌──────────────┐         ┌──────────────────┐
│   Telegram   │────────▶│   Event Bus      │
│   (Long Poll)│         │  (fanout + back  │
└──────────────┘         │   pressure)      │
                         └────────┬─────────┘
                                  │
                                  ▼
                         ┌──────────────────┐
                         │ Session Manager   │
                         │                  │
                         │ session_key:     │
                         │ "org/repo/       │
                         │  issues/42"      │
                         │     │            │
                         │     ▼            │
                         │ ┌────────────┐   │
                         │ │   Agent    │   │
                         │ │ (serial q) │   │
                         │ └─────┬──────┘   │
                         │       │          │
                         │       ▼          │
                         │ ┌───────────┐    │
                         │ │   LLM     │    │
                         │ │ (OpenAI)  │    │
                         │ └─────┬─────┘    │
                         │       │          │
                         │       ▼          │
                         │ ┌────────────┐   │
                         │ │   Tools    │   │
                         │ │ (10 built) │   │
                         │ └────────────┘   │
                         └──────────────────┘
                                  │
                                  ▼
                         ┌──────────────────┐
                         │     Memory       │
                         │ (JSONL + git     │
                         │  notes)          │
                         └──────────────────┘
```

### Event Flow

1. **Ingest** — Forgejo webhooks and Telegram messages are normalized into `event.Event` structs
2. **Route** — The session manager maps events to sessions by `session_key` (e.g. `org/repo/issues/42`)
3. **Process** — Each session has a serial queue; the agent processes one event at a time
4. **Act** — The LLM decides which tools to call; tool results feed back into the conversation
5. **Record** — Reasoning traces and tool outputs are written to JSONL and git notes

### Emoji Reaction Protocol

The agent uses Forgejo reactions to communicate status:

| Reaction | Meaning |
|----------|---------|
| 👀 | Agent has seen the event |
| ⏳ | Agent is processing |
| ✅ | Agent finished successfully |
| ❌ | Agent encountered an error |

## Configuration

Configuration is a single YAML file with environment variable expansion via `${VAR}` syntax.

```yaml
server:
  host: "0.0.0.0"
  port: 8080

webhook:
  secret: "change-me-in-production"    # HMAC shared secret

forgejo:
  url: "https://forgejo.example.com"
  token: "${FORGEJO_TOKEN}"            # Repository-scoped token
  rate_limit: 60                       # API requests per minute

telegram:
  enabled: false
  token: "${TELEGRAM_BOT_TOKEN}"
  poll_timeout: 10
  allowed_chats: []                    # Supergroup IDs; empty = allow all
  chat_bindings: {}                    # Maps chat IDs to repositories

agent:
  max_sessions: 10                     # Concurrent agent sessions
  idle_timeout: "4h"                   # Session reaper interval
  workdir: "/tmp/fordjent/work"        # Base directory for clones
  max_turns: 25                        # Max LLM turns per event
  commit_prefix: "[agent-automation]"  # Prefix for agent commits

providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "${OPENAI_API_KEY}"
    model: "gpt-4o"
    max_tokens: 16384

security:
  protected_branches: ["main", "master"]
  require_pr_for_workflows: true
  filter_agent_events: true
```

See [`fordjent.yaml`](fordjent.yaml) for the full reference with comments.

## Tools

The agent has access to 10 tools exposed via OpenAI function calling:

### Forgejo API Tools

| Tool | Description |
|------|-------------|
| `forgejo_comment` | Post comments on issues and pull requests |
| `forgejo_list_issues` | List and filter issues in a repository |
| `forgejo_get_issue` | Get issue or PR details by number |
| `forgejo_create_pr` | Create pull requests from a head branch |
| `forgejo_search_code` | Search code within a repository |
| `forgejo_add_reaction` | Add emoji reactions to issues/comments |

### Local Tools

| Tool | Description |
|------|-------------|
| `bash` | Execute shell commands in the repository working directory |
| `read_file` | Read file contents (with offset/limit for large files) |
| `write_file` | Create or overwrite files in the repository |
| `git` | Execute git operations (push blocked on protected branches) |

All Forgejo API tools sanitize repository names via `url.PathEscape` to prevent URL injection. The `git` tool blocks all push operations — the agent creates PRs via `forgejo_create_pr` instead.

## Telegram Integration

Fordjent can act as a Telegram bot, mapping forum topics to Forgejo issues and PRs:

- **Supergroup (forum mode)** maps to a repository
- **Forum topic** maps to a specific issue or PR session
- Messages in a topic are normalized to events and routed through the same event bus
- The agent can respond in both Forgejo (comments) and Telegram (topic replies)

### Telegram Commands

| Command | Description |
|---------|-------------|
| `/start` | Show help message |
| `/status` | Health check |
| `/bind <repo>` | Show how to bind this chat to a repository |

### Topic Lifecycle

1. When an issue/PR is opened, Fordjent auto-creates a forum topic (Phase 2)
2. Messages in that topic are routed to the agent session for that issue
3. Agent responses are posted back to the topic (Phase 3)

## Project Structure

```
fordjent/
├── cmd/fordjent/main.go              # Entry point — wires all components
├── fordjent.yaml                      # Reference configuration
├── Dockerfile                         # Multi-stage Go build
├── docker-compose.yaml                # Compose stack with env secrets
├── .env.example                       # Template for environment variables
├── internal/
│   ├── config/config.go               # YAML config with ${VAR} expansion
│   ├── event/event.go                 # Event types, bus (fanout + backpressure)
│   ├── webhook/router.go              # HTTP server, HMAC validation, normalization
│   ├── session/
│   │   ├── manager.go                 # Session lifecycle, key affinity, idle reaping
│   │   └── agent.go                   # Agent loop — LLM turns, tool dispatch
│   ├── provider/client.go             # OpenAI-compatible LLM client
│   ├── forgejo/client.go              # Forgejo REST API client
│   ├── tool/
│   │   ├── registry.go                # Tool interface and registry
│   │   ├── adapter.go                 # Session info and agent config adapters
│   │   ├── forgejo_tools.go           # Forgejo API tools (6 tools)
│   │   └── local_tools.go             # Shell, file, git tools (4 tools)
│   ├── memory/memory.go               # JSONL log + git notes memory
│   └── telegram/
│       ├── router.go                  # Long polling, message→event normalization
│       ├── topics.go                  # SQLite-backed topic↔session mapping store
│       └── responder.go              # Message splitting, acknowledge/error (Phase 3)
├── docs/
│   └── telegram-plan.md               # Telegram integration design notes
└── DESIGN.md                          # Detailed architecture document
```

## Security

### Loop Prevention

Four layers prevent the agent from triggering itself in an infinite loop:

1. **Commit prefix filter** — Events from commits with `[agent-automation]` prefix are dropped
2. **Bot sender filter** — Events from `fordjent[bot]` are ignored
3. **Branch protection** — The `git` tool blocks all push operations; agent uses `forgejo_create_pr`
4. **Workflow PR requirement** — Agent must create a PR for any workflow file changes

### Input Sanitization

- Repository names from LLM tool calls are sanitized via `url.PathEscape` before URL construction
- Shell arguments in the `bash` tool are passed via `exec.Command` argument vector (no shell injection)
- Webhook payloads are validated via HMAC-SHA256 before processing

### Secret Management

| Secret | Scope | Purpose |
|--------|-------|---------|
| `FORGEJO_TOKEN` | Repository-scoped | Forgejo API calls |
| `OPENAI_API_KEY` | LLM provider | Model inference |
| `TELEGRAM_BOT_TOKEN` | Bot identity | Telegram long polling |
| `webhook.secret` | Shared HMAC | Webhook authenticity |

Secrets are injected via environment variables and expanded in config with `${VAR}` syntax. Never commit secrets to the repository.

## Development

### Prerequisites

- Go 1.25+
- A running Forgejo instance (for integration testing)
- An OpenAI-compatible LLM endpoint

### Building & Testing

```bash
go build -o fordjent ./cmd/fordjent

# Run all tests with race detector
go test -race -count=1 ./...

# Run a specific package
go test -v -race ./internal/session/...
```

The test suite includes **105 tests** covering all packages with `-race` clean.

### Adding a Tool

1. Create a struct implementing the `tool.Tool` interface:

```go
type myTool struct {
    adapter *tool.ForgejoAdapter
}

func (t *myTool) Name() string         { return "my_tool" }
func (t *myTool) Description() string  { return "Does something useful" }
func (t *myTool) Parameters() map[string]interface{} { /* JSON Schema */ }
func (t *myTool) Execute(ctx context.Context, params map[string]interface{}, info tool.SessionInfo) (*tool.Result, error) {
    // Implementation
}
```

2. Register it in `tool.NewRegistry()` (in `registry.go`)
3. The tool is automatically exposed to the LLM via function calling

### Running with Docker

```bash
# Build and start
docker compose up -d

# View logs
docker compose logs -f

# Health check
curl http://localhost:8080/healthz
```

## Roadmap

- [ ] **Phase 2** — Auto-create Telegram topics when issues/PRs open; `ResponseWriter` interface for dual output
- [ ] **Phase 3** — Topic lifecycle (close/reopen with issues), streaming responses, rate limiting
- [ ] **Observability** — Prometheus metrics, OpenTelemetry tracing, structured log file output
- [ ] **Persistence** — SQLite-backed session state for crash recovery
- [ ] **Scale** — Redis event bus for multi-node, Forgejo runner integration
- [ ] **Intelligence** — Subagent orchestration, plan mode, context compaction via summarization

## License

MIT
