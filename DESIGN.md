# Fordjent: Forgejo-Driven Agent Harness

> A Go implementation of the asynchronous agent system described in PLAN.md, informed by the designs of Forgejo's webhook/actions infrastructure, OpenCode's ACP protocol, and Pi's SDK/extension architecture.

## Architecture

```
┌─────────────────────┐
│     Forgejo         │
│  (Webhooks, API)    │
└────────┬────────────┘
         │ HTTP POST (HMAC-validated)
         ▼
┌─────────────────────┐     ┌────────────────────────┐
│   Webhook Router    │────▶│     Event Bus          │
│   (HTTP Server)     │     │  (in-memory fanout)    │
│   /acp/v1/events    │     └──────────┬─────────────┘
│   /healthz          │                │
└─────────────────────┘                │
                                       ▼
                            ┌─────────────────────┐
                            │   Session Manager    │
                            │                      │
                            │  session_key ──────┐ │
                            │  "org/repo/issues  │ │
                            │   /42"         │   │ │
                            │                ▼   │ │
                            │  ┌────────────────┐│ │
                            │  │ Agent Session  ││ │
                            │  │ (serial queue) ││ │
                            │  └───────┬────────┘│ │
                            │          │         │ │
                            │          ▼         │ │
                            │  ┌────────────────┐│ │
                            │  │  LLM Provider  ││ │
                            │  │ (OpenAI-compat)││ │
                            │  └───────┬────────┘│ │
                            │          │         │ │
                            │          ▼         │ │
                            │  ┌────────────────┐│ │
                            │  │ Tool Registry  ││ │
                            │  │ ┌────────────┐ ││ │
                            │  │ │ Forgejo API│ ││ │
                            │  │ │ Bash       │ ││ │
                            │  │ │ Git        │ ││ │
                            │  │ │ Read/Write │ ││ │
                            │  │ │ Search     │ ││ │
                            │  │ │ Reactions  │ ││ │
                            │  │ └────────────┘ ││ │
                            │  └────────────────┘│ │
                            └─────────────────────┘ │
                                                    │
                            ┌───────────────────────┘
                            │
                            ▼
                  ┌────────────────────┐
                  │   Memory System    │
                  │ (JSONL + Git Log)  │
                  │                    │
                  │  memory.jsonl      │
                  │  git notes         │
                  │  compaction/       │
                  └────────────────────┘
```

## Design Decisions

### Why Go?

1. **Forgejo alignment** — Forgejo is Go; shared language enables future Forgejo plugin/mod integration
2. **Single binary** — No runtime dependencies, easy deployment alongside Forgejo
3. **Concurrency** — Goroutines map naturally to the session-per-issue model
4. **Performance** — Low memory footprint for many concurrent sessions

### Key Design Patterns (borrowed from research)

| Pattern | Source | Fordjent Implementation |
|---------|--------|------------------------|
| ACP endpoint | OpenCode | `/acp/v1/events` webhook receiver |
| Event bus fanout | OpenCode | `event.Bus` with buffered channels |
| Session affinity | PLAN.md | `SessionManager` maps `session_key → Agent` |
| Serial event processing | PLAN.md | Per-session event channel with queuing |
| Emoji reaction protocol | PLAN.md | 👀 → ⏳ → ✅ (or ❌) via Forgejo reactions API |
| Git log as memory | PLAN.md | JSONL + git notes, queryable by session key |
| Tool registry | Pi SDK | `tool.Registry` with `Tool` interface |
| OpenAI-compatible provider | Pi/opencode | `provider.Client` with tool calling |
| Loop prevention | PLAN.md | Commit prefix filtering + sender identity check |
| Branch protection | PLAN.md | `gitTool` blocks direct push to protected branches |
| Idle session reaping | PLAN.md | 1-minute reaper ticker with configurable timeout |

### Session Key Affinity

Events are routed by `session_key` (format: `org/repo/issues/42` or `org/repo/pulls/7`). All events for the same key go to the same agent session, processed serially. This prevents race conditions where two agent instances try to comment on the same issue simultaneously.

### Tool System

The tool system follows Pi's design: each tool implements a `Tool` interface with `Name()`, `Description()`, `Parameters()` (JSON Schema), and `Execute()`. Tools are registered in a `Registry` and exposed to the LLM as OpenAI function-calling tools.

**Available tools:**

| Tool | Purpose |
|------|---------|
| `forgejo_comment` | Post comments on issues/PRs |
| `forgejo_list_issues` | List issues in a repository |
| `forgejo_get_issue` | Get issue/PR details |
| `forgejo_create_pr` | Create pull requests |
| `forgejo_search_code` | Search code in a repository |
| `forgejo_add_reaction` | Add emoji reactions |
| `bash` | Execute shell commands |
| `read_file` | Read file contents |
| `write_file` | Write file contents |
| `git` | Execute git operations |

### Memory System

Three-layer memory:

1. **JSONL log** (`memory.jsonl`) — Every reasoning trace and tool call recorded as JSON lines
2. **Git notes** — Agent thoughts attached to commits via `git notes --ref=fordjent`
3. **Compaction** — Nightly compaction to `docs/issues/XXXX-summary.md` (to be implemented as Forgejo workflow)

### Loop Prevention

Multi-layer defense:

1. **Commit prefix filter** — Events from commits with `[agent-automation]` prefix are dropped
2. **Sender identity** — Events from `fordjent[bot]` are filtered
3. **Branch protection** — `git` tool blocks direct push to `main`/`master`
4. **Workflow PR requirement** — Agent must PR workflow file changes

### LLM Provider

Uses OpenAI-compatible API (works with OpenAI, Ollama, LiteLLM, any compatible endpoint). Supports:
- Tool calling (function calling)
- Multi-turn conversations with tool results
- Configurable model, max tokens, temperature

## File Structure

```
fordjent/
├── cmd/fordjent/main.go           # CLI entry point
├── fordjent.yaml                   # Configuration file
├── internal/
│   ├── config/config.go            # YAML config with env expansion
│   ├── event/event.go              # Event types and bus
│   ├── webhook/router.go           # HTTP webhook receiver
│   ├── session/manager.go          # Session lifecycle + agent loop
│   ├── tool/
│   │   ├── registry.go             # Tool interface and registry
│   │   ├── adapter.go              # Session/config adapters
│   │   ├── forgejo_tools.go        # Forgejo API tools
│   │   └── local_tools.go          # Bash, file, git tools
│   ├── provider/client.go          # OpenAI-compatible LLM client
│   ├── forgejo/client.go           # Forgejo API client
│   └── memory/memory.go            # JSONL + git notes memory
└── tests/                          # Unit tests per package
```

## Running

```bash
# Build
go build -o fordjent ./cmd/fordjent

# Configure
cp fordjent.yaml my-config.yaml
# Edit: set forgejo URL, token, provider API key

# Run
FORGEJO_TOKEN=your-token OPENAI_API_KEY=your-key ./fordjent -config my-config.yaml

# In Forgejo: Add webhook to repository
# URL: http://your-host:8080/acp/v1/events
# Secret: (match webhook.secret in config)
# Events: issues, issue_comment, pull_request
```

## Future Enhancements

### Phase 1: Forgejo Integration
- [ ] Forgejo Actions workflow for nightly compaction
- [ ] Deploy key management for git operations
- [ ] Repository-scoped tokens via Forgejo API

### Phase 2: Observability
- [ ] Prometheus metrics (events processed, tool calls, LLM tokens)
- [ ] OpenTelemetry tracing (event_id correlation)
- [ ] Structured logging to file

### Phase 3: Scale
- [ ] SQLite persistence for routing table (survive restarts)
- [ ] Rate limiting (Forgejo API, LLM API)
- [ ] Multi-node support via Redis event bus
- [ ] Forgejo runner integration (spawn agents as Action jobs)

### Phase 4: Intelligence
- [ ] Subagent orchestration (spawn explore agents for research)
- [ ] Plan mode (read-only agent that creates plans)
- [ ] Context compaction via LLM summarization
- [ ] Skill system (like Pi's skill files for specialized tasks)

### Forgejo Patches (minor)
These would be simple patches to Forgejo to improve agent harness feasibility:
1. **Bot user type** — Add `bot: true` field to user model, filter in webhook delivery
2. **Agent session API** — Expose active agent sessions in Forgejo admin UI
3. **Webhook event filtering** — Allow webhook to filter by commit message prefix
4. **Reaction webhook events** — Emit events when reactions are added (for agent status tracking)
