# Fordjent: Forgejo-Driven Agent Harness

> A Go implementation of an asynchronous agent system, informed by the designs of Forgejo's webhook/actions infrastructure, OpenCode's ACP protocol, and Pi's SDK/extension architecture.

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
│   /status            │                │
└─────────────────────┘                ▼
                            ┌─────────────────────┐
                            │   Session Manager   │
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
                            │  │ (role-routed)  ││ │
                            │  └───────┬────────┘│ │
                            │          │         │ │
                            │          ▼         │ │
                            │  ┌────────────────┐│ │
                            │  │ Tool Registry  ││ │
                            │  │ ┌────────────┐ ││ │
                            │  │ │ Forgejo API │ ││ │
                            │  │ │ Bash        │ ││ │
                            │  │ │ Git         │ ││ │
                            │  │ │ Read/Write  │ ││ │
                            │  │ │ Search      │ ││ │
                            │  │ │ Reactions   │ ││ │
                            │  │ │ Branches    │ ││ │
                            │  │ │ Hooks       │ ││ │
                            │  │ │ Files       │ ││ │
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

### Coordination Layer

```
┌──────────────────────────────────────────────────────┐
│                  Session Manager                       │
│                                                       │
│  IssueOpened ──▶ Scaffold Detection (empty repo?)     │
│                       │                               │
│                  Session Start ──▶ Lifecycle (state)  │
│                       │                               │
│  forgejo_create_pr ──▶ Stale Gate (auto-rebase)       │
│                       │                               │
│                  Merge Queue (file overlap check)     │
│                       │                               │
│  PR Merged ──▶ Scheduler (unblock dependents)         │
│                                                       │
└──────────────────────────────────────────────────────┘
```

## Design Decisions

### Why Go?

1. **Forgejo alignment** — Forgejo is Go; shared language enables future Forgejo plugin/mod integration
2. **Single binary** — No runtime dependencies, easy deployment alongside Forgejo
3. **Concurrency** — Goroutines map naturally to the session-per-issue model
4. **Performance** — Low memory footprint for many concurrent sessions

### Key Design Patterns

| Pattern | Fordjent Implementation |
|---------|------------------------|
| ACP endpoint | `/acp/v1/events` webhook receiver |
| Event bus fanout | `event.Bus` with buffered channels + backpressure |
| Session affinity | `SessionManager` maps `session_key → Agent` |
| Serial event processing | Per-session event channel with queuing |
| Role-based provider routing | `ProviderForRole(role)` selects LLM per detected role |
| Context compaction | `ContextTracker` truncates at configurable threshold |
| Emoji reaction protocol | 👀 → ⏳ → ✅ (or ❌) via Forgejo reactions API |
| Git log as memory | JSONL + git notes, queryable by session key |
| Tool registry | `tool.Registry` with `Tool` interface (20 tools) |
| OpenAI-compatible provider | `provider.Client` with tool calling + retry |
| Loop prevention | `<!-- ford -->` marker + sender identity + branch protection |
| Branch protection | `bash`/`git` tools block push to protected branches |
| Stale gate + auto-rebase | `stalegate.IsStale()` detects + rebase + force-push |
| Merge queue | File-gate checks overlap with open PRs |
| Dependency scheduler | Parses `Depends on: #N`, transitions `blocked` → `ready` |
| Scaffold detection | Blocks issues on empty repos, creates scaffold issue |
| Lifecycle state machine | SQLite transitions, auto-labels on failure |
| Cost tracking + budget | Per-session/repo/month SQLite, enforceable limits |
| Idle session reaping | Configurable timeout with periodic reaper |

### Session Key Affinity

Events are routed by `session_key` (format: `org/repo/issues/42` or `org/repo/pulls/7`). All events for the same key go to the same agent session, processed serially. This prevents race conditions where two agent instances try to comment on the same issue simultaneously.

### Role Detection

The session manager detects the agent's role from issue labels (`role:pm`, `role:implementer`, etc.) or title prefixes (`[pm]`, `[decompose]`). Each role can be routed to a different LLM provider via `role_providers` in config. Roles also get different turn budgets (`max_turns_pm`, `max_turns_implementer`).

### PR Creation Pipeline

Before `forgejo_create_pr` posts to the Forgejo API, it passes through multiple gates:

1. **Stale gate** — `git merge-base --is-ancestor origin/main HEAD` checks if the branch is behind; if so, auto-rebase + force-push
2. **Auto-push** — Ensures the branch exists on the remote
3. **Merge queue** — Compares changed files against all open PRs; blocks if any file overlap
4. **Build/test/lint verify** — Runs `go build`, `go test`, `golangci-lint`; blocks on failure

If any gate fails, the agent receives an error message explaining what went wrong and can fix it.

### Tool System

The tool system follows Pi's design: each tool implements a `Tool` interface with `Name()`, `Description()`, `Parameters()` (JSON Schema), and `Execute()`. Tools are registered in a `Registry` and exposed to the LLM as OpenAI function-calling tools.

**Available tools (20):**

| Category | Tools |
|----------|-------|
| Forgejo API | `forgejo_comment`, `forgejo_create_issue`, `forgejo_list_issues`, `forgejo_get_issue`, `forgejo_create_pr`, `forgejo_merge_pr`, `forgejo_list_prs`, `forgejo_search_code`, `forgejo_add_reaction`, `forgejo_list_branches`, `forgejo_delete_branch`, `forgejo_list_hooks`, `forgejo_create_hook`, `forgejo_delete_hook`, `forgejo_list_files`, `forgejo_pr_files`, `forgejo_list_collabs`, `forgejo_version`, `forgejo_user`, `forgejo_create_token` |
| Local | `bash`, `read_file`, `write_file`, `git` |

### Memory System

Three-layer memory:

1. **JSONL log** (`memory.jsonl`) — Every reasoning trace and tool call recorded as JSON lines
2. **Git notes** — Agent thoughts attached to commits via `git notes --ref=fordjent`
3. **Compaction** — Nightly compaction to `docs/issues/XXXX-summary.md`

### Loop Prevention

Multi-layer defense:

1. **`<!-- ford -->` marker** — Hidden HTML marker appended to every comment/PR body; webhook router detects and drops self-originated events
2. **Sender identity** — Events from `fordjent[bot]` are filtered
3. **Branch protection** — `bash` and `git` tools block push to `main`/`master`
4. **Commit prefix filter** — Events from commits with `[agent-automation]` prefix are dropped

### LLM Provider

Uses OpenAI-compatible API (works with OpenAI, Ollama Cloud, Wafer, LiteLLM, any compatible endpoint). Supports:
- Tool calling (function calling)
- Multi-turn conversations with tool results
- Configurable model, max tokens, temperature per provider
- Role-based provider routing via `role_providers`
- Retry with exponential backoff + jitter for transient errors
- Concurrency semaphore to limit parallel LLM calls
- `reasoning_content` fallback for models that return reasoning in a separate field

## File Structure

```
fordjent/
├── cmd/fordjent/main.go           # CLI entry point
├── fordjent.yaml                   # Configuration file
├── internal/
│   ├── agent/
│   │   ├── context.go              # Context window tracking + compaction
│   │   └── turn.go                 # Per-turn execution with cost/latency logging
│   ├── config/config.go            # YAML config with env expansion, hot-reload
│   ├── cost/cost.go                # SQLite cost tracker + budget enforcement
│   ├── event/event.go              # Event types and bus
│   ├── forgejo/client.go           # Forgejo REST API client
│   ├── lifecycle/lifecycle.go      # Session state machine, failure labeling
│   ├── memory/memory.go            # JSONL + git notes memory
│   ├── mergequeue/queue.go         # File-gate merge queue
│   ├── metrics/metrics.go          # Prometheus counters + JSON snapshot
│   ├── provider/
│   │   ├── client.go               # OpenAI-compatible LLM client
│   │   └── retry.go                # Exponential backoff with jitter
│   ├── scaffold/scaffold.go        # Empty-repo protection
│   ├── scheduler/scheduler.go      # Dependency parser, label transitions
│   ├── sentinel/sentinel.go        # Typed sentinel errors
│   ├── session/
│   │   ├── manager.go              # Session lifecycle, key affinity
│   │   ├── store.go                # SQLite-backed session persistence
│   │   └── agent.go                # Agent loop — LLM turns, tool dispatch
│   ├── stalegate/stalegate.go      # Git-plumbing staleness + auto-rebase
│   ├── tool/
│   │   ├── registry.go             # Tool interface and registry
│   │   ├── adapter.go              # Session/config adapters
│   │   ├── forgejo_tools.go        # 20 Forgejo API tools
│   │   └── local_tools.go          # 4 local tools
│   ├── webhook/router.go           # HTTP webhook receiver, /status, /metrics
│   └── webui/webui.go             # HTML admin dashboard
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

- [ ] **Multi-node** — Redis event bus for horizontal scaling
- [ ] **CI integration** — Forgejo Actions runner for test-gated merges
- [ ] **Summarization compaction** — LLM-based context summarization instead of truncation
- [ ] **Subagent orchestration** — Spawn explore/research agents for complex tasks
- [ ] **Skill system** — Like Pi's skill files for specialized agent capabilities
