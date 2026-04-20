Based on your clarifications and my research, I'll now synthesize the PLAN.md. The research confirms Forgejo supports all required capabilities: webhooks with JSON payloads [^1] [^3], repository-scoped tokens [^5], `workflow_dispatch` for manual triggering [^3], branch protection [^4], and the Actions reference provides event types including `issues` and `pull_request` with configurable triggers [^2] [^2].

---

# PLAN.md: Asynchronous Agent System for Forgejo

## 1. Architecture Overview

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Forgejo        │────▶│  Webhook Router  │────▶│  Session Manager│
│  (Events)       │     │  (ACP Endpoint)    │     │  (Per-Issue)    │
└─────────────────┘     └──────────────────┘     └────────┬────────┘
                                                          │
                              ┌─────────────────────────┼─────────────────────────┐
                              ▼                         ▼                         ▼
                        ┌─────────┐               ┌─────────┐               ┌─────────┐
                        │ Agent A │               │ Agent B │               │ Agent C │
                        │(Issue 42)│              │(Issue 43)│              │(Issue 44)│
                        └────┬────┘               └────┬────┘               └────┬────┘
                             │                           │                           │
                             └───────────────────────────┼───────────────────────────┘
                                                         ▼
                                                  ┌─────────────┐
                                                  │  Git Log    │
                                                  │  (Write-Only)│
                                                  └─────────────┘
```

## 2. Core Components

### 2.1 Event Ingestion (Webhook → ACP)

**Forgejo Configuration:**
- Webhook URL: `https://agent-controller.example.com/acp/v1/events`
- Events: `issues`, `issue_comment`, `pull_request`, `pull_request_review_comment`
- Secret: HMAC validation for authenticity [^1]

**ACP Message Format:**
```json
{
  "event_id": "uuid-v7-timestamped",
  "type": "issue_comment.created",
  "repository": "org/repo",
  "issue_number": 42,
  "sender": "username",
  "payload": { /* full Forgejo webhook payload */ },
  "session_key": "org/repo/issues/42"
}
```

**Session Affinity Rule:** Events with identical `session_key` route to the same agent process. Concurrent events queue; agent processes serially with steering (not parallel).

### 2.2 Session Manager

**Responsibilities:**
- Spawn/kill agent processes bound to `session_key`
- Maintain routing table: `session_key → (pid, last_activity)`
- Health check: terminate sessions idle > 4 hours (configurable)
- Emoji reaction protocol: agent adds 👀 on event receipt, ⏳ on processing, ✅ on completion

**Implementation:** Python FastAPI service with Unix domain sockets for local agent IPC, or TCP for distributed deployment.

### 2.3 Agent Runtime (Per-Session)

**Environment:**
- Pi agent with Forgejo API tools
- `FORGEJO_TOKEN` from repository secrets [^7]
- Deploy key for git operations (read/write)
- Working directory: ephemeral clone of target repository

**Agent Loop:**
1. Receive ACP message (steering or new event)
2. Query git log for context: `git log --all --grep="issue/42" --format="%H %s %b"`
3. Query compacted docs: `docs/issues/0042-summary.md` if exists
4. Generate response (code, comment, or workflow)
5. Execute via API tools or git commit
6. Write reasoning trace to git log: `git notes add -m '{"agent_thought": "...", "tools_used": [...]}'`
7. Send completion status to Session Manager

### 2.4 Git Log as Memory

**Write-Only Schema:**
```
commit message: "[agent:issue/42] action:comment_reply reason:clarification"
body: JSON with full context, tool outputs, token usage
```

**Nightly Compaction (scheduled workflow):**
- Trigger: `schedule: cron: '0 2 * * *'` [^3]
- Input: Git log filtered by date
- Output: `docs/issues/XXXX-summary.md` per active issue
- Format: Markdown with embedded decision log, code snippets, unresolved questions

**Query Pattern:** Agents use `git log --grep` with issue prefix; acceptable latency per your guidance.

## 3. Loop Prevention

**Strategy: Prefix + Branch Protection**

| Layer | Mechanism |
|-------|-----------|
| Workflow trigger | `types: [opened]` only (not `edited`) for PRs that modify `.forgejo/workflows/` [^2] |
| File naming | All agent-triggered workflows prefixed `agent-*.yml` |
| Branch protection | `.forgejo/workflows/` requires 1 human approval via CODEOWNERS [^4] |
| Commit filtering | Agent commits include `[agent-automation]` in message; workflows skip if present |

**Critical Rule:** Agent-generated workflow changes must be PR'd from feature branch, never direct push to default branch. This satisfies your "streamline, not bypass" requirement.

## 4. Subagent Orchestration

**Model: In-Process with Tool Context Switching**

- Single Pi instance per session
- "Spawn subagent" = tool call with isolated context, not new process
- Subagent outputs appended to parent reasoning trace
- Failure: parent catches exception, marks issue with ❌ reaction, logs to git log, terminates session (no retry loop)

**Alternative (heavy tasks):** Use `workflow_dispatch` API [^3] to trigger separate runner job for code generation; parent polls completion via API. This isolates resource-heavy work but adds latency.

## 5. Security Boundaries

| Secret | Scope | Usage |
|--------|-------|-------|
| `FORGEJO_TOKEN` | Repository-scoped [^5] | API calls (comment, PR, issue) |
| `PI_API_KEY` | External provider | LLM inference |
| Deploy key | Read+write | Git commits, branch creation |

**Runner Isolation:** Self-hosted Forgejo runner with Docker [^6]; network egress restricted to Forgejo instance and Pi API endpoint.

## 6. Implementation Phases

### Phase 0: Bootstrap (Week 1)
- [ ] Create `agent-infrastructure` repository
- [ ] Implement Webhook Router (FastAPI, HMAC validation)
- [ ] Implement Session Manager (process table, emoji protocol)
- [ ] Basic Pi agent with Forgejo API tools (list issues, comment, create PR)

### Phase 1: Memory (Week 2)
- [ ] Git log schema and commit conventions
- [ ] Nightly compaction workflow
- [ ] Agent context retrieval from git log
- [ ] `docs/` generation and indexing

### Phase 2: Workflow Generation (Week 3)
- [ ] Agent creates `.forgejo/workflows/` files
- [ ] PR creation with `[agent-automation]` prefix
- [ ] Branch protection and CODEOWNERS configuration
- [ ] Loop detection validation (test suite)

### Phase 3: Scale & Harden (Week 4)
- [ ] Session Manager HA (SQLite persistence of routing table)
- [ ] Rate limiting (Forgejo API, Pi API)
- [ ] Observability (metrics, trace correlation via `event_id`)
- [ ] Documentation for human operators

## 7. Open Questions for Coding Agent

1. **ACP transport:** Unix domain sockets (single node) or HTTP (distributed)? Affects Session Manager implementation.
2. **Pi tool interface:** Does Pi support structured JSON tool outputs, or do we wrap shell calls? Affects API client design.
3. **Compaction trigger:** Nightly scheduled workflow in same repo, or external cron? Affects secret exposure.
4. **Emoji protocol:** Custom reactions (👀/⏳/✅) or labels? Forgejo supports both; labels more searchable.

---

**Persistent technique discovered:** Git log as write-only audit trail with nightly compaction to structured docs. This satisfies "the mind is moving" — the log captures every flicker of attention, the docs capture the settled understanding. Recommend adding to your custom instructions for future research engineering contexts.

Ready to hand off to coding agent?

[^1]: [Webhooks | Forgejo – Beyond coding. We forge.](https://forgejo.org/docs/next/user/webhooks/) (26%)
[^2]: [Forgejo Actions | Reference | Forgejo – Beyond coding. We forge.](https://forgejo.org/docs/latest/user/actions/) (25%)
[^3]: [Forgejo Actions | Reference | Forgejo – Beyond coding. We forge.](https://forgejo.org/docs/next/user/actions/reference/) (17%)
[^4]: [Branch and tag protection | Forgejo – Beyond coding. We forge.](https://forgejo.org/docs/next/user/protection/) (12%)
[^5]: [Forgejo v15.0 is available](https://forgejo.org/2026-04-release-v15-0/) (8%)
[^6]: [Utilizing Docker within Actions | Forgejo – Beyond coding. We forge.](https://forgejo.org/docs/next/admin/actions/docker-access/) (7%)
[^7]: [Forgejo Actions | Reference | Forgejo – Beyond coding. We forge.](https://forgejo.org/docs/latest/user/actions/reference/) (5%)

