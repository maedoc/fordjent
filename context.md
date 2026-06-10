# Code Context — LLM API Request Construction & Prompt Caching Analysis

## Files Retrieved

1. `internal/provider/client.go` (full) - API request/response construction, message serialization
2. `internal/session/agent.go` (lines 1-800) - System prompt construction, context building
3. `internal/agent/context.go` (full) - Context compaction logic
4. `internal/agent/turn.go` (full) - Turn execution, steering messages, tool exclusion
5. `internal/config/config.go` (full) - Provider/agent configuration
6. `internal/tool/registry.go` (full) - Tool schema registration, exclusion logic
7. `internal/provider/retry.go` (full) - Retry policy (no caching impact)
8. `internal/memory/memory.go` (full) - Session memory (not involved in API requests)

---

## Key Code

### 1. API Request Construction (`internal/provider/client.go`)

**Message assembly order** (lines 230-245):
```go
// chatOnce builds the API request
var reqMessages []messageJSON
reqMessages = append(reqMessages, messageJSON{
    Role:    "user",  // ← System prompt uses "user" role, NOT "system"
    Content: systemPrompt,
})

for _, msg := range messages {
    mj := messageJSON{
        Role:       msg.Role,
        Content:    msg.Content,
        ToolCallID: msg.ToolCallID,
    }
    // ... tool_calls handling ...
    reqMessages = append(reqMessages, mj)
}
```

**Request structure** (lines 259-269):
```go
reqBody := openAIRequest{
    Model:       c.cfg.Model,
    Messages:    reqMessages,
    MaxTokens:   c.cfg.MaxTokens,
    Temperature: 0.3,
}
if len(reqTools) > 0 {
    reqBody.Tools = reqTools
}
```

**No `cache_control` hints anywhere** - the codebase does not send any prompt caching directives to the LLM API.

### 2. System Prompt Construction (`internal/session/agent.go`)

**`buildSystemPrompt` signature** (line 645):
```go
func (a *Agent) buildSystemPrompt(ctx context.Context, evt *event.Event, analysisMode bool, role string, fsmState lifecycle.IssueState, turn, maxTurns int) string
```

**Dynamic content that changes every turn**:
```go
// Turn budget info injected into prompt (line 906)
turnBudgetInfo := fmt.Sprintf("\n\n## Turn Budget\nYou have %d turns total and are currently on turn %d. ...", maxTurns, turn)
```

**Dynamic scope prefixes** (lines 778-780):
```go
scopeDetail := ""
if len(a.scopePrefixes) > 0 {
    scopeDetail = fmt.Sprintf("\n- Your allowed paths: %s", strings.Join(a.scopePrefixes, ", "))
}
```

**Tool descriptions can change** (line 647):
```go
toolsDesc := a.tools.Descriptions()
```

Tools can be excluded dynamically via `executor.SetExcludeTools()` (e.g., `forgejo_comment` after hitting the comment limit).

### 3. Context Compaction (`internal/agent/context.go`)

**Compaction prepends a marker message** (lines 72-84):
```go
func (t *ContextTracker) Compact(messages []provider.Message) []provider.Message {
    // ...
    var compacted []provider.Message
    compacted = append(compacted, provider.Message{
        Role:    "user",
        Content: "[Context Compacted] Earlier conversation history has been removed to stay within token limits. Continue from the latest context below.",
    })
    compacted = append(compacted, messages[keepStart:]...)
    // ...
}
```

**This changes the message prefix** - after compaction, message[0] is the compaction marker instead of the original first user message.

### 4. TurnExecutor Steering (`internal/agent/turn.go`)

**Steering messages are appended dynamically** (lines 246-292):
```go
func (te *TurnExecutor) ApplySteering(messages []provider.Message, ...) []provider.Message {
    // Per-tool repeat nudges
    if nudge := te.PerToolRepeatNudge(); nudge != "" {
        messages = append(messages, provider.Message{
            Role:    "user",
            Content: "[Fordjent Steering] " + nudge,
        })
    }

    // Bug reproduction steering
    if te.isBugReport && !te.hasReproduced && te.turnCount >= 2 && te.turnCount <= 5 {
        messages = append(messages, provider.Message{...})
    }

    // Turn budget thresholds (40%, 60%, 80%, 90%)
    thresholds := map[int]string{
        40: "[Turn 40%] ...",
        60: "[Turn 60%] ...",
        // ...
    }
}
```

**Tool exclusion changes schemas** (lines 86-89):
```go
func (te *TurnExecutor) Run(...) {
    // ...
    toolDefs := te.tools.ToolsExcluding(te.excludeTools)
    response, usage, err := te.llm.Chat(ctx, systemPrompt, messages, toolDefs)
}
```

### 5. Config (`internal/config/config.go`)

**Relevant fields**:
```yaml
agent:
  context_window: 128000
  compaction_threshold: 0.80
  compaction_keep_turns: 8
  max_turns: 25
  max_turns_pm: 15
  max_turns_implementer: 50
  max_turns_reviewer: 20
  role_providers:
    pm: "kimi-k2.6"
    reviewer: "glm-5.1"
    implementer: "devstral-2-123b"
```

**No cache-related configuration exists.**

---

## Architecture

### API Request Flow

```
ProcessEvent (agent.go)
    ↓
buildSystemPrompt → includes: turn#, maxTurns, scopePrefixes, toolsDesc, fsmState
    ↓
buildContext → includes: issue body, comments, parent context, previous session memory
    ↓
TurnExecutor.Run (turn.go)
    ↓
ShouldCompact? → if yes, Compact() prepends marker
    ↓
ApplySteering() → appends nudges based on turn count, tool usage
    ↓
ToolsExcluding() → filters out blocked tools (e.g., forgejo_comment after limit)
    ↓
provider.Client.Chat (client.go)
    ↓
chatOnce() → builds openAIRequest with:
    - messages[0] = {role: "user", content: systemPrompt}
    - messages[1..N] = conversation history
    - tools = filtered tool schemas
    ↓
HTTP POST to {api_base}/chat/completions
```

### Message Order (What Gets Sent to API)

| Index | Role | Content | Changes Every Turn? |
|-------|------|---------|---------------------|
| 0 | `user` | System prompt | **YES** - turn#, steering state, tool descriptions |
| 1..N | `user`/`assistant`/`tool` | Conversation history | **YES** - grows each turn |
| N+1 (after compaction) | `user` | `[Context Compacted]` marker | **YES** - only present after compaction fires |

---

## Why Prompt Caching Breaks

### Prefix Changes Between Turns

| Source | How It Breaks Caching |
|--------|----------------------|
| **System prompt (message 0)** | Contains `turn` and `maxTurns` values that increment every API call. Even a 1-digit → 2-digit change (`turn 9` → `turn 10`) modifies the prefix. |
| **Tool descriptions** | `toolsDesc` includes all tool names/descriptions. If `excludeTools` is non-empty (e.g., `forgejo_comment` blocked), the schema changes. |
| **Scope prefixes** | `a.scopePrefixes` injected into system prompt for implementer role. These are extracted per-issue but don't change mid-session. |
| **Compaction marker** | When compaction fires, a new message is PREPENDED at position 0 (actually position 1 after system prompt). This shifts all subsequent message indices. |
| **Steering messages** | Appended to end of messages array at thresholds (40/60/80/90%). Not prefix-breaking, but changes request body. |

### Critical Finding: System Prompt Is Sent As `role: "user"`

The system prompt is **not** sent as `role: "system"` but as `role: "user"` (client.go line 233). This is intentional to support providers like Scaleway that reject system messages after tool responses. However, it means:

1. The first message in EVERY API request is the full system prompt
2. Any change to the system prompt (turn counter, tool descriptions) modifies the very first message
3. Most LLM providers cache based on message prefix stability — changing message[0] invalidates the entire cache

### No `cache_control` Implementation

The codebase has **zero** references to prompt caching features:
- No `cache_control` fields in `messageJSON` struct
- No `cache_control` fields in `openAIRequest` struct
- No configuration options for caching
- Anthropic-style `ephemeral` hints not implemented
- OpenAI `store` parameter not used

---

## Start Here

**File to open first**: `internal/provider/client.go` (lines 225-280) — the `chatOnce` function shows exactly how messages are serialized and sent to the API.

**For prompt caching implementation**:
1. Add `cache_control` field to `messageJSON` struct in `client.go`
2. Modify `Compact()` in `context.go` to mark the compaction message as ephemeral (it's discarded anyway)
3. Stabilize the system prompt by:
   - Removing turn counter from system prompt (move to a separate user message appended later)
   - Or: send turn counter as a separate message after the stable prefix
4. Tool schemas: consider caching the base schema and only delta-patching excluded tools

**Biggest win**: Moving the turn counter out of the system prompt. Currently it's embedded at line 906 in `agent.go`:
```go
turnBudgetInfo := fmt.Sprintf("\n\n## Turn Budget\nYou have %d turns total and are currently on turn %d. ...", maxTurns, turn)
```
This makes message[0] different on every single API call. If moved to a separate `provider.Message` appended after the stable system prompt, providers could cache the first N messages.

---

## Supervisor Coordination

**Decision needed**: Should prompt caching be implemented? If yes:

1. **Approach A (minimal)**: Extract volatile content (turn counter, steering messages) from the system prompt into separate appended messages. Mark compaction marker as ephemeral.

2. **Approach B (comprehensive)**: Implement full `cache_control` support with provider-specific handling (Anthropic `ephemeral`, OpenAI `store`, etc.).

3. **Approach C (defer)**: Prompt caching is a nice-to-have; focus on agent reliability first.

The system prompt changing on every turn (due to turn counter) is the single largest barrier to caching — even with `cache_control` hints, changing message[0] invalidates everything.
