# Provenance: Intelligence Phase Research

## Sources Read (Full Text)

### /tmp/swarm/swarm/core.py
- **Why**: Understand Swarm's agent handoff mechanism (returning Agent objects from tool calls)
- **Key finding**: `handle_function_result()` — returning an `Agent` from a tool call triggers a handoff; `context_variables` dict is shared across handoffs; `active_agent` updated in the run loop
- **Lines analyzed**: Full file (~230 lines)

### /tmp/swarm/swarm/types.py
- **Why**: Understand Agent, Response, Result data models
- **Key finding**: `Result(value, agent, context_variables)` — the agent field enables delegation; context_variables enable shared state
- **Lines analyzed**: Full file (~40 lines)

### /tmp/swarm/examples/basic/agent_handoff.py
- **Why**: Minimal example of agent-to-agent handoff
- **Key finding**: Handoff is just `return spanish_agent` from a tool function — elegantly simple

### /tmp/aider/aider/coders/architect_coder.py
- **Why**: Understand Aider's two-pass planning approach
- **Key finding**: `reply_completed()` creates an `EditorCoder` with `Coder.create(**new_kwargs)`, passes architect's output as `with_message=content`, merges costs back. Key: `map_tokens=0` for editor (no repo map needed)
- **Lines analyzed**: Full file (~55 lines)

### /tmp/aider/aider/coders/architect_prompts.py
- **Why**: Understand architect's system prompt and role separation
- **Key finding**: "Act as an expert architect engineer and provide direction to your editor engineer... DO NOT show the entire updated function/file/etc!" — architect describes changes, doesn't write code
- **Lines analyzed**: Full file (~30 lines)

### /tmp/aider/aider/coders/base_coder.py
- **Why**: Understand context management, summarization triggers, repo map integration
- **Key findings**: 
  - `summarize_start()` triggered after every `move_back_cur_messages()` call
  - Background thread for summarization (`summarizer_thread`)
  - `summarize_end()` joins thread and replaces `done_messages`
  - `repo_map` generates tree-sitter-based code overview within token budget
- **Lines analyzed**: Lines 95-280, 700-760, 1000-1050

### /tmp/aider/aider/history.py
- **Why**: Core summarization algorithm
- **Key findings**:
  - `summarize_real()` implements head/tail split with recursive compaction
  - Split falls on assistant messages for valid conversation structure
  - Depth limit of 3 prevents infinite recursion
  - `summarize_all()` concatenates all messages and calls LLM
  - Prompt writes from user perspective: "I asked you..."
- **Lines analyzed**: Full file (~140 lines)

### /tmp/aider/aider/prompts.py
- **Why**: Summarization prompt template
- **Key finding**: "Include less detail about older parts and more detail about the most recent messages... The summary MUST include the function names, libraries, packages... MUST include the filenames... MUST NOT include fenced code blocks"
- **Lines analyzed**: Lines 46-65

### /tmp/letta/letta/schemas/memory.py
- **Why**: Understand MemGPT's memory tier architecture
- **Key findings**:
  - `Memory` class with `blocks: List[Block]` — each block has label, value, description, limit, read_only
  - `ChatMemory` initializes with `persona` and `human` blocks
  - `BasicBlockMemory` provides `core_memory_append/replace` tools
  - `ContextWindowOverview` tracks token counts per section (system, core_memory, summary, messages, functions, directories)
  - XML-based rendering with `<memory_blocks>`, `<description>`, `<metadata>`, `<value>` tags
- **Lines analyzed**: Full file (~550 lines)

### /tmp/letta/letta/services/summarizer/self_summarizer.py
- **Why**: Understand Letta's self-summarization approach
- **Key findings**:
  - `self_summarize_all()`: sends conversation to agent's own LLM with "summarize" prompt
  - `_get_protected_messages()`: protects pending approvals from eviction
  - `self_summarize_sliding_window()`: iterative eviction with increasing percentage (10% increments)
  - Goal tokens: `(1 - window_percentage) * context_window`
  - Falls back to complete summarization if no valid cutoff found
- **Lines analyzed**: Full file (~230 lines)

### /tmp/crewai/lib/crewai/src/crewai/process.py
- **Why**: Understand CrewAI's process model
- **Key finding**: Simple enum: `sequential | hierarchical` (consensual TODO)

### /tmp/crewai/lib/crewai/src/crewai/a2a/utils/delegation.py
- **Why**: Understand CrewAI's agent-to-agent delegation protocol
- **Key findings**:
  - A2A (Agent-to-Agent) protocol with JSONRPC/gRPC/HTTP transport negotiation
  - `execute_a2a_delegation()` sends `task_description + context + conversation_history` to remote agent
  - Multi-turn support with `turn_number` tracking
  - Event emission for delegation lifecycle (started/completed)
- **Lines analyzed**: Lines 1-150, 200-350

### /tmp/autogen/.../swarm_group_chat.py
- **Why**: Understand AutoGen's swarm pattern
- **Key findings**:
  - `SwarmGroupChatManager` selects speaker based on `HandoffMessage.target`
  - First participant is initial speaker; handoff messages route to next
  - State saved as `SwarmManagerState(current_speaker, message_thread, current_turn)`
- **Lines analyzed**: Full file (~160 lines)

### /tmp/autogen/.../magentic_one_orchestrator.py
- **Why**: Understand Magentic-One's ledger-based orchestration
- **Key findings**:
  - Two loops: outer loop creates task ledger (facts + plan), inner loop executes via progress ledger
  - `_orchestrate_step()`: generates `progress_ledger` JSON via LLM call
  - Progress ledger fields: `is_request_satisfied`, `is_in_loop`, `is_progress_being_made`, `next_speaker`, `instruction_or_question`
  - Stall counter: incremented on no-progress or loop detection, decremented on progress
  - `max_stalls` exceeded → `_update_task_ledger()` + `_reenter_outer_loop()` (facts update + plan rewrite)
  - Reset: clears all agent states, re-broadcasts updated ledger
- **Lines analyzed**: Full file (~300 lines)

### /tmp/autogen/.../magentic_one/_prompts.py
- **Why**: Understand Magentic-One's prompt engineering for planning
- **Key findings**:
  - Facts prompt: 4-section survey (given, to-lookup, to-derive, educated guesses)
  - Plan prompt: "devise a short bullet-point plan"
  - Progress ledger prompt: structured JSON with reasoning + boolean answers
  - Facts update prompt: "rewrite the fact sheet... update educated guesses"
  - Plan update prompt: "explain what went wrong... come up with a new plan"
- **Lines analyzed**: Full file (~120 lines)

## Methodology

1. Cloned 5 reference repositories (swarm, aider, letta, crewai, autogen) with `--depth 1`
2. Located relevant source files via `grep -rl` for key terms (subagent, plan_mode, compact, summarize, delegate, handoff)
3. Read full source files for core implementations, skipping test/example files
4. Extracted algorithms, data structures, and design patterns
5. Cross-referenced patterns across frameworks to identify common approaches
6. Mapped patterns to Fordjent's Go codebase and session-per-issue architecture
7. Produced implementation plan ordered by dependency and risk (truncation → compaction → plan mode → subagents)
