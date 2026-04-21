# Intelligence Phase: Research Brief & Implementation Plan

> Subagent orchestration, plan mode, and context compaction for Fordjent

## 1. Subagent Orchestration

### 1.1 Landscape Analysis

I examined the source code of six agent frameworks to understand how they handle delegation, context isolation, and result aggregation:

| Framework | Pattern | Context Isolation | Result Aggregation | Error Handling |
|-----------|---------|-------------------|---------------------|----------------|
| **OpenAI Swarm** | Agent-as-return-value handoff | Shared `context_variables` dict, copied per run | `Result(value, agent, context_variables)` — tool return becomes next agent's input | Missing tools logged, skipped; type errors propagated |
| **Aider** | Two-pass architect→editor | Separate Coder instances, architect passes output string to editor | Editor runs with architect's text as input; costs merged back | Architect can decline (user confirms); editor failure isolated |
| **CrewAI** | A2A delegation protocol | Remote agent gets `task_description + context` only | `TaskStateResult(status, result, error, history, agent_card)` | Failed delegations emit events, exceptions propagated |
| **AutoGen Swarm** | HandoffMessage routing | Each agent has own state; shared thread | Latest handoff target determines next speaker | Handoff validation against participant list |
| **AutoGen Magentic-One** | Ledger-based orchestration | Shared facts/plan ledger; per-agent instructions | Orchestrator synthesizes via `progress_ledger` JSON | Stall counter with re-planning loop (max_stalls → outer loop reset) |
| **Letta/MemGPT** | Memory-tiered self-editing | Core memory (in-context blocks), archival (embedding search), recall (conversation history) | Agent self-edits core memory via `core_memory_replace/append` tools | Char limits enforced per block; truncation with warning |

### 1.2 Key Patterns for Fordjent

#### Pattern A: Function-Call Delegation (Swarm-style)

The simplest model: a tool that returns an `Agent` object triggers a handoff.

```python
# Swarm's approach (Python)
def transfer_to_spanish_agent():
    return spanish_agent  # returning an Agent = handoff
```

**How it works**: The orchestrator loop checks `Result.agent`. If non-nil, `active_agent = result.agent` and the loop continues with the new agent's instructions, functions, and model. The conversation history is shared — only the system prompt changes.

**Fordjent adaptation**: Instead of switching agents, a `delegate_task` tool spawns a new goroutine with its own session key (e.g., `org/repo/issues/42/sub/explore`), isolated context, and reduced tool set. The parent waits for completion or continues asynchronously.

#### Pattern B: Two-Pass Architect→Editor (Aider-style)

Aider's `ArchitectCoder` is elegant:
1. Architect agent receives the request + repo map (no file editing tools)
2. Architect outputs a natural-language description of changes
3. User confirms ("Edit the files?")
4. A new `EditorCoder` is created with the architect's output as its task
5. Editor has file editing tools but `map_tokens=0` (no repo map)
6. Costs are merged back: `self.total_cost = editor_coder.total_cost`

**Key insight**: The two agents have **complementary tool sets**. The architect reads and plans; the editor writes and commits. Neither has both capabilities.

#### Pattern C: Ledger-Based Orchestration (Magentic-One)

The most sophisticated pattern. The orchestrator maintains:

1. **Task Ledger** (created once, updated on stall):
   - **Facts**: Given, to-lookup, to-derive, educated guesses
   - **Plan**: Bullet-point action plan
2. **Progress Ledger** (updated every turn):
   - `is_request_satisfied`: bool
   - `is_in_loop`: bool (detects repeated actions)
   - `is_progress_being_made`: bool
   - `next_speaker`: which agent
   - `instruction_or_question`: what to tell them

Stall detection: if `!is_progress_being_made || is_in_loop` for `max_stalls` consecutive rounds, trigger **outer loop reset**: update facts, rewrite plan, re-broadcast to all agents.

### 1.3 Recommendation for Fordjent

**Adopt Pattern B (Aider-style) for the MVP, with hooks for Pattern C later.**

Rationale:
- Fordjent sessions are per-issue, not per-team. The architect→editor split maps naturally to "plan changes → implement changes"
- Magentic-One's ledger pattern is overkill for single-issue sessions but becomes relevant when Fordjent supports multi-repository workflows
- Swarm's shared-context handoff is clean but assumes multiple agents; Fordjent currently has one agent per session

**Implementation design:**

```
┌─────────────────────────────────────────────┐
│              Session Manager                 │
│                                             │
│  session "org/repo/issues/42"              │
│    └── Agent (coordinator)                  │
│          ├── tools: delegate_task           │
│          │   └── spawns SubAgent with       │
│          │       - reduced tool set         │
│          │       - scoped context           │
│          │       - max_turns limit          │
│          │       - session_key suffix       │
│          └── collects SubAgent.Result       │
│                                             │
│  SubAgent types:                            │
│    "explore"  → read-only tools only        │
│    "edit"     → write tools, no planning    │
│    "review"   → read + comment, no write    │
└─────────────────────────────────────────────┘
```

#### SubAgent Tool Schema

```go
// delegate_task spawns a subagent to handle a focused task.
// Returns the subagent's output for inclusion in the parent's context.
{
  "name": "delegate_task",
  "parameters": {
    "task_type": "explore|edit|review",
    "description": "Natural language description of the subtask",
    "files": ["list of files to include in context"],
    "max_turns": 5
  }
}
```

#### Context Isolation Rules

1. Subagents receive only: the task description, specified files, and relevant issue/PR context
2. Subagents do NOT receive: the parent's full conversation history, memory log, or other subagent outputs
3. Subagent results are returned as tool-call results to the parent, who decides what to do with them
4. Subagents write to the same JSONL memory but with a `subagent` flag and parent session key

#### Error Handling

- Subagent timeout: configurable per task type (explore: 60s, edit: 120s, review: 30s)
- Subagent failure: parent receives error as tool result, can retry or work around
- Subagent loop detection: if subagent exceeds `max_turns`, return partial result with warning

---

## 2. Plan Mode

### 2.1 Landscape Analysis

| Framework | Plan Representation | Validation | Transition |
|-----------|---------------------|------------|------------|
| **Aider** | Natural language output from architect | User confirmation ("Edit the files?") | Architect output becomes editor's input |
| **Claude Code** | `planMode` flag disables write tools | User approves plan via UI | Flag toggle, context preserved |
| **Magentic-One** | Structured bullet-point plan in task ledger | Progress ledger checks `is_request_satisfied` | Outer loop reset rewrites plan on stall |
| **CrewAI** | Task objects with `expected_output` and `callback` | Human-before/after callbacks | Sequential task completion triggers next |

### 2.2 Key Findings

**Aider's approach is the most practical for Fordjent:**

1. **Read-only phase**: The agent has access to read tools (`read_file`, `forgejo_get_issue`, `forgejo_search_code`, `bash` with read-only restriction) but NOT write tools (`write_file`, `git`, `forgejo_create_pr`)
2. **Plan output**: Natural language description of intended changes — not structured JSON, not pseudocode. The LLM describes what files to change and how.
3. **Approval gate**: User must explicitly approve (in Forgejo: emoji reaction 👍 on the plan comment; in Telegram: explicit `/approve` command)
4. **Execution phase**: A fresh agent instance receives the plan as context and has write tools enabled

**Why not structured plans?**
- Magentic-One's JSON ledger works when you control all agents and their schemas
- In a Forgejo context, the "user" is commenting on an issue — they need to read and approve a plan, not parse JSON
- Natural language plans are more flexible and don't require schema versioning

### 2.3 Recommendation for Fordjent

**Two-phase plan mode with approval gate:**

```
┌───────────────────────────────────────────────────┐
│ Phase 1: Planning (read-only)                     │
│                                                   │
│  Agent has:                                       │
│    ✅ read_file, bash (read-only)                 │
│    ✅ forgejo_get_issue, forgejo_list_issues      │
│    ✅ forgejo_search_code                         │
│    ❌ write_file, git, forgejo_create_pr          │
│    ❌ forgejo_comment (except plan comment)        │
│                                                   │
│  Output: Comment on issue with plan               │
│    "## Plan\n\n1. Read foo.go\n2. Modify bar()..."│
│    + 👀 reaction for tracking                     │
├───────────────────────────────────────────────────┤
│ Approval Gate                                     │
│                                                   │
│  Forgejo: User adds 👍 reaction to plan comment   │
│  Telegram: User sends /approve in the topic       │
│  Config: auto_approve: false (default)            │
│  Timeout: configurable (default 1h)               │
├───────────────────────────────────────────────────┤
│ Phase 2: Execution (read-write)                   │
│                                                   │
│  Fresh agent receives:                            │
│    - Original event                               │
│    - Plan comment as context                      │
│    - Full tool set                                │
│                                                   │
│  Output: Changes + summary comment                │
│    + ✅ reaction                                  │
└───────────────────────────────────────────────────┘
```

#### Configuration

```yaml
agent:
  plan_mode: "always"     # always | complex_only | never
  plan_approval: "reaction"  # reaction | comment | auto
  plan_timeout: "1h"
  # "complex_only": plan only if the issue has label "needs-plan"
  #                or the task involves >3 files
```

#### Plan Comment Format

```markdown
## 📋 Plan for Issue #42

**Trigger**: Comment from @user "Fix the authentication bypass"

### Analysis
- Current auth middleware in `internal/middleware/auth.go:45` skips token validation for OPTIONS requests
- Tests in `internal/middleware/auth_test.go` don't cover CORS preflight

### Proposed Changes
1. **`internal/middleware/auth.go`** — Remove the OPTIONS bypass on line 47; add explicit CORS handler after auth
2. **`internal/middleware/auth_test.go`** — Add test case for OPTIONS with invalid token
3. **`internal/config/config.go`** — No changes needed

### Risk Assessment
- **Impact**: Medium — affects all authenticated endpoints
- **Breaking**: No — CORS preflight will still work, just validated
- **Tests**: 1 new test case, all existing should pass

---
*React with 👍 to approve this plan. Auto-approved in 1h if unattended.*
```

#### Implementation in Agent Loop

```go
func (a *Agent) ProcessEvent(ctx context.Context, evt *event.Event) error {
    if a.shouldPlan(evt) {
        return a.planThenExecute(ctx, evt)
    }
    return a.executeDirectly(ctx, evt)
}

func (a *Agent) planThenExecute(ctx context.Context, evt *event.Event) error {
    // Phase 1: Plan with read-only tools
    planResult, err := a.runWithToolSet(ctx, evt, a.readOnlyTools())
    if err != nil {
        return err
    }

    // Post plan as comment
    planCommentID := a.postPlanComment(ctx, evt, planResult)

    // Wait for approval
    approved, err := a.waitForApproval(ctx, evt, planCommentID)
    if !approved {
        a.addReaction(ctx, evt, "no_entry") // 🚫
        return nil
    }

    // Phase 2: Execute with plan as context
    return a.runWithPlan(ctx, evt, planResult)
}
```

---

## 3. Context Compaction / Summarization

### 3.1 Landscape Analysis

| Framework | Trigger | Retention | Tool Output Handling | Implementation |
|-----------|---------|-----------|---------------------|----------------|
| **Aider** | Token count exceeds `max_tokens` (1024 default) | Keep recent tail (50% of max); summarize head | Tool outputs included in summarization | Background thread; recursive head/tail split with depth limit of 3 |
| **Letta/MemGPT** | Context window % full (configurable via `sliding_window_percentage`) | Core memory always retained; recent messages protected (approvals) | Moved to archival memory (embedding-searchable) | Self-summarization: agent summarizes own conversation via LLM call |
| **Claude Code** | Conversation exceeds threshold | Keep system prompt + recent N turns | Large tool outputs truncated to last N chars | "Compact" command available; auto-triggered on context overflow |

### 3.2 Deep Dive: Aider's ChatSummary

Aider's approach is particularly relevant because it handles the exact problem Fordjent faces: long tool-calling sessions that bloat the context window.

**Algorithm** (from `aider/history.py`):

```
func summarize(messages, depth=0):
    if token_count(messages) <= max_tokens and depth == 0:
        return messages  // no compaction needed
    
    if len(messages) <= 4 or depth > 3:
        return summarize_all(messages)  // nuclear option
    
    // Split: keep recent half, summarize older half
    split_index = find_split(messages):
        - walk backwards from end
        - accumulate tokens until we reach 50% of max_tokens
        - ensure split falls on an assistant message
    
    head = messages[:split_index]
    tail = messages[split_index:]
    
    // Fit head within model's context window
    keep = fit_within_limit(head, model_max_tokens - 512)
    
    // Summarize the kept portion
    summary = summarize_all(keep)  // LLM call
    
    // Recurse if summary + tail still too big
    if token_count(summary) + token_count(tail) > max_tokens:
        return summarize(summary + tail, depth + 1)
    
    return summary + tail
```

**Key design decisions**:
- Splits always end on assistant messages (ensures valid conversation structure)
- Head/tail split preserves recent context verbatim
- Recursive with depth limit (prevents infinite compaction)
- Background thread (`summarize_start()`) — non-blocking
- Summarization prompt writes from user's perspective: "I asked you..."

### 3.3 Deep Dive: Letta's Self-Summarization

Letta takes a different approach: the agent summarizes its own conversation.

**Algorithm** (from `letta/services/summarizer/self_summarizer.py`):

1. Protect system message (always kept)
2. Protect pending approval messages (can't evict active requests)
3. Send the conversation to the agent's own LLM with a "summarize this" prompt
4. The LLM generates a summary in its own voice
5. Replace summarized messages with: `[system] + [summary as user message] + [protected recent messages]`

**Sliding window variant**:
- Calculate `goal_tokens = (1 - window_percentage) * context_window`
- Iteratively increase eviction percentage (10% increments)
- Find cutoff point at an assistant message
- Summarize everything before cutoff
- Keep everything after cutoff verbatim

### 3.4 Recommendation for Fordjent

**Hybrid approach: Aider-style background compaction + Letta-style tool output truncation.**

```
┌─────────────────────────────────────────────────────────┐
│ Context Window Management                               │
│                                                         │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ Always Retained (never compacted)                   │ │
│ │  - System prompt (agent identity, rules, tools)     │ │
│ │  - Current event (what we're responding to)         │ │
│ │  - Issue/PR body + comments (buildContext)           │ │
│ │  - Memory summary (from previous compaction)        │ │
│ └─────────────────────────────────────────────────────┘ │
│                                                         │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ Compacted Region (Aider-style head/tail split)      │ │
│ │                                                     │ │
│ │ [Summary of turns 1..N-k]  ← LLM-summarized        │ │
│ │ [Turns N-k+1..N verbatim]  ← recent context kept   │ │
│ └─────────────────────────────────────────────────────┘ │
│                                                         │
│ ┌─────────────────────────────────────────────────────┐ │
│ │ Tool Output Truncation (before compaction)           │ │
│ │                                                     │ │
│ │ bash output:        last 2000 chars                  │ │
│ │ read_file output:   first 100 lines + "... truncated"│ │
│ │ search results:     top 10 matches                   │ │
│ │ forgejo responses:  full (usually small)             │ │
│ └─────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────┘
```

#### Trigger Conditions

```go
type CompactionConfig struct {
    Enabled           bool          `yaml:"enabled"`
    MaxTokens         int           `yaml:"max_tokens"`          // trigger threshold (default: model's max_tokens * 0.7)
    KeepRecentTurns   int           `yaml:"keep_recent_turns"`   // never compact last N turns (default: 4)
    MaxToolOutputChars int          `yaml:"max_tool_output_chars"` // truncate tool results (default: 2000)
    SummaryMaxTokens  int           `yaml:"summary_max_tokens"`  // max size for summary (default: 1024)
    Background        bool          `yaml:"background"`          // compact in background thread (default: true)
}
```

Compaction triggers when `tokenEstimate(messages) > maxTokens`. Token estimation uses the simple heuristic: `len(text) / 4` (rough approximation for GPT-4 tokenization).

#### Compaction Algorithm (Aider-style)

```go
func Compact(messages []Message, maxTokens int, keepRecent int) []Message {
    totalTokens := estimateTokens(messages)
    if totalTokens <= maxTokens {
        return messages
    }

    // Protect system prompt and recent turns
    splitIdx := len(messages) - keepRecent
    // Adjust splitIdx to fall on an assistant message
    for splitIdx > 0 && messages[splitIdx].Role != "assistant" {
        splitIdx--
    }
    if splitIdx <= 1 {
        // Too few messages to split — summarize everything except system prompt
        return summarizeAll(messages)
    }

    head := messages[:splitIdx]
    tail := messages[splitIdx:]

    // Truncate tool outputs in head before summarizing
    for i := range head {
        if head[i].Role == "tool" {
            head[i].Content = truncateWithEllipsis(head[i].Content, 500)
        }
    }

    // Summarize head via LLM
    summary := summarize(head)

    // Build new message list: summary + tail
    result := []Message{
        {Role: "user", Content: "[Previous context summary]\n" + summary},
        {Role: "assistant", Content: "Understood. I'll continue from where we left off."},
    }
    result = append(result, tail...)

    // Recurse if still too large
    if estimateTokens(result) > maxTokens {
        return Compact(result, maxTokens, keepRecent)
    }

    return result
}
```

#### Summarization Prompt

Based on Aider's approach (user-perspective summary) adapted for agent context:

```
Briefly summarize the following agent session context. Include:
1. What task was the agent working on?
2. What files were read or modified?
3. What tool calls were made and what were the key results?
4. What is the current state of progress?

Start with "The agent was working on..."
Include all file paths and function names referenced.
Do NOT include code blocks.
Focus on the most recent actions; summarize older actions more briefly.
```

#### Tool Output Truncation

Before compaction even triggers, truncate large tool outputs:

```go
func truncateToolOutput(toolName, output string, maxChars int) string {
    if len(output) <= maxChars {
        return output
    }

    switch toolName {
    case "bash":
        // Keep the tail (error messages are usually at the end)
        return "... (truncated, showing last " + strconv.Itoa(maxChars) + " chars)\n" + 
               output[len(output)-maxChars:]
    case "read_file":
        // Keep the head (file beginning is usually more important)
        return output[:maxChars] + "\n... (truncated)"
    case "forgejo_search_code":
        // Keep first N matches
        lines := strings.Split(output, "\n")
        if len(lines) > 20 {
            return strings.Join(lines[:20], "\n") + "\n... (truncated, showing first 20 results)"
        }
        return output
    default:
        return output[:maxChars] + "\n... (truncated)"
    }
}
```

---

## 4. Implementation Plan

### Phase 4A: Tool Output Truncation (1-2 days)

**Why first**: This is the lowest-risk, highest-impact change. Tool outputs are the primary source of context bloat.

**Files to modify**:
- `internal/provider/client.go` — Add token estimation helper
- `internal/session/manager.go` — Add `truncateToolOutput()` to tool result processing in `ProcessEvent()`
- `internal/config/config.go` — Add `CompactionConfig` to agent config
- `fordjent.yaml` — Add compaction configuration

**Tests**: Unit tests for truncation logic, integration test for agent loop with large outputs.

### Phase 4B: Context Compaction (2-3 days)

**Files to create/modify**:
- `internal/session/compactor.go` — `Compactor` struct with `Compact()` and `summarize()`
- `internal/session/compactor_test.go` — Tests for head/tail split, truncation, recursive compaction
- `internal/session/manager.go` — Wire compactor into agent loop; check token count after each turn

**Key decisions**:
- Background compaction (goroutine, like Aider) vs synchronous (simpler, adds latency)
- Start with synchronous for correctness, optimize to background later
- Token estimation: use `len(text) / 4` heuristic initially; add tiktoken-style counting if accuracy matters

**Tests**: Test compaction with synthetic message histories of varying sizes. Verify that split points fall on assistant messages. Verify that summary + tail fits within budget.

### Phase 4C: Plan Mode (2-3 days)

**Files to create/modify**:
- `internal/session/planner.go` — `Planner` struct with `shouldPlan()`, `planThenExecute()`, `waitForApproval()`
- `internal/session/planner_test.go` — Tests for planning logic, approval gate
- `internal/session/manager.go` — Wire planner into `ProcessEvent()`
- `internal/tool/registry.go` — Add `ReadOnlyTools()` and `WriteTools()` filter methods
- `internal/forgejo/client.go` — Add `WaitForReaction()` for approval polling
- `internal/telegram/router.go` — Add `/approve` command handler
- `fordjent.yaml` — Add plan mode configuration

**Key decisions**:
- Approval mechanism: Start with Forgejo reaction polling (check every 30s for 👍)
- Plan timeout: Default 1h, configurable
- `complex_only` heuristic: If the event payload mentions >3 files or has a "needs-plan" label

**Tests**: Test `shouldPlan()` with various events. Test plan comment formatting. Test approval polling with mock Forgejo client.

### Phase 4D: Subagent Orchestration (3-5 days)

**Files to create/modify**:
- `internal/session/subagent.go` — `SubAgent` struct, `DelegateTask()` tool
- `internal/session/subagent_test.go` — Tests for subagent spawning, context isolation, result collection
- `internal/tool/registry.go` — Add `ToolSetForTask(taskType)` method
- `internal/session/manager.go` — Wire subagent into agent loop
- `internal/config/config.go` — Add `SubAgentConfig` with per-type settings

**Subagent types and tool sets**:

| Type | Tools | Max Turns | Timeout |
|------|-------|-----------|---------|
| `explore` | read_file, bash (read-only), forgejo_get_issue, forgejo_search_code | 5 | 60s |
| `edit` | read_file, write_file, bash, git | 10 | 120s |
| `review` | read_file, bash (read-only), forgejo_get_issue, forgejo_comment, forgejo_add_reaction | 3 | 30s |

**Key decisions**:
- Subagents are **synchronous** (parent blocks until subagent completes) for the MVP
- Subagents share the same `WorkDir` but have their own message history
- Subagent results are returned as tool-call results (string) to the parent
- Subagent token usage is accumulated into the parent session's total

**Tests**: Test subagent spawning with different types. Test that tool sets are correctly restricted. Test timeout handling. Test that subagent results are properly returned to parent context.

### Phase 4E: Polish & Integration (1-2 days)

- Wire all components together in the agent loop
- Add metrics: compaction count, plan mode activations, subagent delegations
- Update `DESIGN.md` and `README.md`
- End-to-end test with synthetic Forgejo events
- Race-condition testing with `-race` flag

### Total Estimated Effort: 9-15 days

---

## 5. Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| LLM summarization loses critical context | Agent makes incorrect changes after compaction | Keep recent N turns verbatim; include file paths and function names in summary prompt |
| Plan mode adds latency (approval wait) | Users perceive agent as slow | Configurable timeout; `auto` mode for trusted repositories |
| Subagent context too narrow | Subagent lacks information to complete task | Include relevant issue/PR context; allow subagent to read memory log |
| Tool output truncation hides errors | Agent makes decisions based on incomplete information | Always show last N lines of bash output; show first N lines of file reads |
| Recursive compaction infinite loop | Context never fits within budget | Depth limit of 3 (like Aider); nuclear fallback to summarize-all |
| Subagent spawning exceeds max_sessions | Session manager rejects new sessions | Subagents don't count against `max_sessions`; they're goroutines within a session |

---

## Provenance

### Source Code (read and analyzed)

| Repository | Files Examined | Key Findings |
|------------|----------------|--------------|
| `openai/swarm` | `swarm/core.py`, `swarm/types.py`, `examples/basic/agent_handoff.py` | Agent-as-return-value handoff pattern; shared context_variables |
| `Aider-AI/aider` | `aider/coders/architect_coder.py`, `aider/coders/architect_prompts.py`, `aider/coders/base_coder.py`, `aider/history.py`, `aider/prompts.py`, `aider/repomap.py` | Two-pass architect→editor; background ChatSummary with head/tail split |
| `letta-ai/letta` | `letta/schemas/memory.py`, `letta/services/summarizer/self_summarizer.py` | Three-tier memory (core/archival/recall); sliding window self-summarization |
| `crewAIInc/crewAI` | `lib/crewai/src/crewai/process.py`, `lib/crewai/src/crewai/a2a/utils/delegation.py` | A2A delegation protocol; sequential/hierarchical process enums |
| `microsoft/autogen` | `autogen_agentchat/teams/_group_chat/_swarm_group_chat.py`, `autogen_agentchat/teams/_group_chat/_magentic_one/_magentic_one_orchestrator.py`, `autogen_agentchat/teams/_group_chat/_magentic_one/_prompts.py` | HandoffMessage routing; ledger-based orchestration with facts/plan/progress; stall detection and re-planning |

### Design Influences

- **Aider's `ChatSummary`** → Fordjent's context compaction algorithm (head/tail split, background thread, recursive depth limit)
- **Aider's `ArchitectCoder`** → Fordjent's plan mode (two-pass, complementary tool sets, user approval gate)
- **Swarm's `Result.agent`** → Fordjent's subagent delegation (tool returns agent for handoff)
- **Magentic-One's progress ledger** → Future: multi-agent orchestration with stall detection (not in MVP)
- **Letta's self-summarization** → Alternative compaction strategy (consider for v2)
