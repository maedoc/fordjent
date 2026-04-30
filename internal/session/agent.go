package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/fordjent/fordjent/internal/agent"
	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/memory"
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/tool"
)

// Agent is the per-session agent that processes events via LLM + tools.
type Agent struct {
	cfg         *config.Config
	sess        *Session
	forgejo     *forgejo.Client
	llm         *provider.Client
	tools       *tool.Registry
	mem         *memory.Memory
	costTracker *cost.Tracker
	executor    *agent.TurnExecutor
}

func NewAgent(cfg *config.Config, sess *Session, mq *mergequeue.Client, ct *cost.Tracker) *Agent {
	forgejoClient := forgejo.NewClient(cfg.Forgejo.URL, cfg.Forgejo.Token)
	prov := cfg.DefaultProvider()
	llmClient := provider.NewClient(prov)
	mem := memory.New(cfg, sess.WorkDir, forgejoClient)

	registry := tool.NewRegistry()
	sessionInfo := &sessionInfoAdapter{workDir: sess.WorkDir, repoDir: sess.RepoDir}
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	agentCfg := &agentConfigAdapter{cfg: cfg}

	// Register tools
	registry.Register(tool.NewCommentTool(forgejoAdapter))
	registry.Register(tool.NewCreateIssueTool(forgejoAdapter))
	registry.Register(tool.NewListIssuesTool(forgejoAdapter))
	registry.Register(tool.NewGetIssueTool(forgejoAdapter))
	registry.Register(tool.NewCreatePRTool(forgejoAdapter, mq, sess.RepoDir))
	registry.Register(tool.NewSearchCodeTool(forgejoAdapter))
	registry.Register(tool.NewAddReactionTool(forgejoAdapter))
	registry.Register(tool.NewMergePRTool(forgejoAdapter))
	registry.Register(tool.NewBashTool(sessionInfo))
	registry.Register(tool.NewReadFileTool(sessionInfo))
	registry.Register(tool.NewWriteFileTool(sessionInfo, agentCfg))
	registry.Register(tool.NewGitTool(sessionInfo))

	executor := agent.NewTurnExecutor(cfg, llmClient, registry, ct, sess.Key, sess.Repository)

	return &Agent{
		cfg:         cfg,
		sess:        sess,
		forgejo:     forgejoClient,
		llm:         llmClient,
		tools:       registry,
		mem:         mem,
		costTracker: ct,
		executor:    executor,
	}
}

// ProcessEvent handles a single event: builds context, runs LLM loop with compaction/retry/cost tracking, executes tools.
func (a *Agent) ProcessEvent(ctx context.Context, evt *event.Event) error {
	// Step 1: Acknowledge with 👀 reaction
	a.addReaction(ctx, evt, "eyes")

	// Step 2: Build context for the LLM
	systemPrompt := a.buildSystemPrompt(evt)
	contextMessages, err := a.buildContext(ctx, evt)
	if err != nil {
		slog.Warn("failed to build full context", "error", err)
	}

	// Step 3: If this is a PR review comment, fetch PR and checkout its branch
	if evt.PRNumber > 0 && (evt.Type == event.IssueCommentCreated || evt.Type == event.PullRequestReviewComment) {
		pr, err := a.forgejo.GetPR(ctx, evt.Repository, evt.PRNumber)
		if err == nil && pr.Head.Ref != "" {
			repoDir := a.sess.RepoDir
			fetchCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "origin", pr.Head.Ref)
			if _, err := fetchCmd.CombinedOutput(); err != nil {
				slog.Warn("failed to fetch PR branch", "branch", pr.Head.Ref, "error", err)
			}
			checkoutCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "checkout", "-B", pr.Head.Ref, "origin/"+pr.Head.Ref)
			if _, err := checkoutCmd.CombinedOutput(); err != nil {
				slog.Warn("failed to checkout PR branch", "branch", pr.Head.Ref, "error", err)
			}
			slog.Info("checked out PR branch for review", "branch", pr.Head.Ref, "session_key", a.sess.Key)
			contextMessages = append(contextMessages, provider.Message{
				Role: "user",
				Content: fmt.Sprintf("[Context] Responding to review on PR #%d '%s'. You are now on branch '%s'. Make changes on this branch, commit, and push to it. Do NOT create a new PR.",
					pr.Number, pr.Title, pr.Head.Ref),
			})
		} else if err != nil {
			slog.Warn("failed to get PR details", "pr", evt.PRNumber, "error", err)
		}
	}

	// Step 4: Build the user message from the event
	userMessage := a.eventToUserMessage(evt)

	// Step 5: LLM loop (max turns)
	messages := append(contextMessages, provider.Message{
		Role:    "user",
		Content: userMessage,
	})

	// Update reaction to ⏳
	a.addReaction(ctx, evt, "hourglass_flowing_sand")

	for turn := 0; turn < a.cfg.Agent.MaxTurns; turn++ {
		slog.Info("LLM turn begin",
			"session_key", a.sess.Key,
			"turn", turn,
			"messages", len(messages),
		)

		result, updatedMessages, err := a.executor.Run(ctx, systemPrompt, messages)
		messages = updatedMessages

		if err != nil {
			slog.Error("LLM turn failed", "session_key", a.sess.Key, "turn", turn, "error", err)
			a.addReaction(ctx, evt, "x")
			return fmt.Errorf("turn %d failed: %w", turn, err)
		}

		// Track metrics
		metrics.IncLLMCalls()
		if result.Usage != nil {
			metrics.AddTokens(int64(result.Usage.PromptTokens), int64(result.Usage.CompletionTokens))
		}
		if result.CostUSD > 0 {
			metrics.AddCost(result.CostUSD)
		}

		// If no tool calls, we're done
		if len(result.Response.ToolCalls) == 0 {
			messages = append(messages, provider.Message{
				Role:    "assistant",
				Content: result.Response.Content,
			})

			if a.cfg.Memory.Enabled {
				a.mem.Record(ctx, evt, result.Response.Content, turn)
			}

			a.addReaction(ctx, evt, "white_check_mark")
			return nil
		}

		// Add assistant message with tool calls
		messages = append(messages, provider.Message{
			Role:      "assistant",
			Content:   result.Response.Content,
			ToolCalls: result.Response.ToolCalls,
		})

		// Execute tool calls
		for _, tc := range result.Response.ToolCalls {
			slog.Info("executing tool",
				"tool", tc.Function.Name,
				"session_key", a.sess.Key,
			)

			metrics.IncToolCalls()

			res, terr := a.tools.Execute(ctx, tc.Function.Name, tc.Function.Arguments)
			if terr != nil {
				slog.Error("tool execution failed", "tool", tc.Function.Name, "error", terr)
				res = fmt.Sprintf("Error: %s", terr)
			}

			if a.cfg.Memory.Enabled {
				a.mem.RecordToolCall(ctx, evt, tc.Function.Name, tc.Function.Arguments, res)
			}

			messages = append(messages, provider.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    res,
			})
		}
	}

	slog.Warn("max turns reached", "session_key", a.sess.Key)
	a.addReaction(ctx, evt, "warning")
	return fmt.Errorf("max turns (%d) reached: %w", a.cfg.Agent.MaxTurns, agent.ErrMaxTurnsReached)
}

func (a *Agent) addReaction(ctx context.Context, evt *event.Event, emoji string) {
	commentID := 0
	if raw, ok := evt.Payload["comment"].(map[string]interface{}); ok {
		if id, ok := raw["id"].(float64); ok {
			commentID = int(id)
		}
	}

	if err := a.forgejo.AddReaction(ctx, evt.Repository, evt.IssueNumber, commentID, emoji); err != nil {
		slog.Debug("failed to add reaction", "emoji", emoji, "error", err)
	}
}

func (a *Agent) buildSystemPrompt(evt *event.Event) string {
	toolsDesc := a.tools.Descriptions()

	var modeInstructions string
	if evt.PRNumber > 0 && (evt.Type == event.IssueCommentCreated || evt.Type == event.PullRequestReviewComment) {
		modeInstructions = `
## PR Review Mode (IMPORTANT)
You are responding to a review comment on an existing pull request.
- You are already on the PR branch (check git status if unsure).
- Make your fixes directly on this branch.
- After fixing, commit and push to the SAME branch.
- Do NOT create a new PR — the PR already exists.
- Post a comment confirming which issues were fixed.
- If the PR is mergeable with no conflicts, you may call forgejo_merge_pr to merge it automatically.`
	} else if evt.PRNumber > 0 {
		modeInstructions = `
## PR Context
You are working on a pull request. Create a feature branch, implement the changes, push the branch, and then use forgejo_create_pr to open the PR.`
	}

	return fmt.Sprintf(`You are Fordjent, an autonomous coding agent that helps with software development tasks on a Forgejo instance.

## Current Context
- Repository: %s
- Event: %s (action: %s)
- Sender: @%s
- Target: %s
%s

## Your Capabilities
You have access to the following tools:
%s

## Rules
1. Always read existing code before making changes.
2. Make minimal, focused changes.
3. All commit messages must start with "%s".
4. NEVER push directly to protected branches (%s). Create a feature branch and PR instead.
5. Workflow file changes (.forgejo/workflows/) MUST go through PRs.
6. When done, post a summary comment on the issue/PR.
7. Be helpful, concise, and correct.
8. **ALWAYS rebase before creating a PR.** Before calling forgejo_create_pr, first run 'git fetch origin' and then 'git rebase origin/main' on your feature branch using the git tool (two separate calls) or the bash tool (combined). This prevents merge conflicts.
9. **Do NOT create a new PR if one already exists** for the current branch. Push to the existing branch instead.
10. **For large tasks**, analyze the work and use 'forgejo_create_issue' to break it into smaller, specific sub-issues if the current scope is too big to complete in one go. This helps track progress and avoids max-turns exhaustion. Always include the line 'Depends on: #{parent_issue_number}' in the body of any sub-issue you create, so the scheduler can track dependencies.
11. **When you create sub-issues via forgejo_create_issue, STOP implementing.** Your role is to decompose and coordinate — post a summary comment on the parent issue, then stop. Let the dedicated sub-issue sessions handle the actual implementation.

## Response Format
Respond in plain text. Use tools to interact with the repository and Forgejo API.`,
		evt.Repository,
		evt.Type,
		evt.Action,
		evt.Sender,
		a.targetDescription(evt),
		modeInstructions,
		toolsDesc,
		a.cfg.Agent.CommitPrefix,
		strings.Join(a.cfg.Security.ProtectedBranches, ", "),
	)
}

func (a *Agent) targetDescription(evt *event.Event) string {
	if evt.PRNumber > 0 {
		return fmt.Sprintf("Pull Request #%d", evt.PRNumber)
	}
	if evt.IssueNumber > 0 {
		return fmt.Sprintf("Issue #%d", evt.IssueNumber)
	}
	return "Repository"
}

func (a *Agent) buildContext(ctx context.Context, evt *event.Event) ([]provider.Message, error) {
	var messages []provider.Message

	if evt.IssueNumber > 0 {
		issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
		if err == nil && issue != nil {
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: fmt.Sprintf("[Context] Issue #%d: %s\n\n%s", evt.IssueNumber, issue.Title, issue.Body),
			})
		}

		comments, err := a.forgejo.ListComments(ctx, evt.Repository, evt.IssueNumber)
		if err == nil {
			for _, c := range comments {
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: fmt.Sprintf("[Comment by @%s] %s", c.User, c.Body),
				})
			}
		}
	}

	if a.cfg.Memory.Enabled {
		summary, err := a.mem.Query(ctx, evt)
		if err == nil && summary != "" {
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: fmt.Sprintf("[Previous Agent Context]\n%s", summary),
			})
		}
	}

	return messages, nil
}

func (a *Agent) eventToUserMessage(evt *event.Event) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("New event: %s (action: %s) from @%s\n", evt.Type, evt.Action, evt.Sender))

	switch evt.Type {
	case event.IssueCommentCreated, event.PullRequestReviewComment:
		if comment, ok := evt.Payload["comment"].(map[string]interface{}); ok {
			if body, ok := comment["body"].(string); ok {
				sb.WriteString(fmt.Sprintf("\nComment:\n%s\n", body))
			}
		}
	case event.IssueOpened:
		if issue, ok := evt.Payload["issue"].(map[string]interface{}); ok {
			if body, ok := issue["body"].(string); ok {
				sb.WriteString(fmt.Sprintf("\nIssue body:\n%s\n", body))
			}
		}
	case event.PullRequestOpened, event.PullRequestSync:
		if pr, ok := evt.Payload["pull_request"].(map[string]interface{}); ok {
			if body, ok := pr["body"].(string); ok {
				sb.WriteString(fmt.Sprintf("\nPR body:\n%s\n", body))
			}
		}
	}

	// Include full payload as JSON for detailed context
	payloadJSON, err := json.MarshalIndent(evt.Payload, "", "  ")
	if err == nil && len(payloadJSON) < 5000 {
		sb.WriteString(fmt.Sprintf("\n<details>\n<summary>Full payload</summary>\n\n```json\n%s\n```\n</details>", string(payloadJSON)))
	}

	return sb.String()
}
