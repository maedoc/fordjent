package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/fordjent/fordjent/internal/agent"
	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/memory"
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/sentinel"
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
	role        string // pm, reviewer, devops, tester, implementer
}

func NewAgent(cfg *config.Config, sess *Session, mq *mergequeue.Client, ct *cost.Tracker, role string) *Agent {
	forgejoClient := forgejo.NewClient(cfg.Forgejo.URL, cfg.Forgejo.Token)
	prov := cfg.ProviderForRole(role)
	llmClient := provider.NewClient(prov)
	mem := memory.New(cfg, sess.WorkDir, forgejoClient)

	sessionInfo := &sessionInfoAdapter{workDir: sess.WorkDir, repoDir: sess.RepoDir}
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	agentCfg := &agentConfigAdapter{cfg: cfg}

	registry := buildRoleRegistry(forgejoAdapter, mq, sess, sessionInfo, agentCfg, role)

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
		role:        role,
	}
}

// ProcessEvent handles a single event: builds context, runs LLM loop with compaction/retry/cost tracking, executes tools.
func (a *Agent) ProcessEvent(ctx context.Context, evt *event.Event) error {
	// Enforce overall session timeout
	sessionTimeout := a.cfg.Agent.SessionTimeout
	if sessionTimeout == 0 {
		sessionTimeout = 30 * time.Minute
	}
	sessCtx, sessCancel := context.WithTimeout(ctx, sessionTimeout)
	defer sessCancel()
	ctx = sessCtx

	// Step 1: Acknowledge with 👀 reaction
	a.addReaction(ctx, evt, "eyes")

	// Step 2: Detect analysis-only mode
	analysisMode := a.detectAnalysisMode(ctx, evt)

	// Step 3: Build context for the LLM
	systemPrompt := a.buildSystemPrompt(evt, analysisMode, a.role)
	contextMessages, err := a.buildContext(ctx, evt)
	if err != nil {
		slog.Warn("failed to build full context", "error", err)
	}

	// If this is a scheduler unblock comment, inject a system directive to proceed.
	if evt.Type == event.IssueCommentCreated && evt.IssueNumber > 0 {
		if comment, ok := evt.Payload["comment"].(map[string]interface{}); ok {
			if body, ok := comment["body"].(string); ok {
				if strings.Contains(body, "is now merged") && strings.Contains(body, "unblocked") {
					contextMessages = append([]provider.Message{{
						Role:    "user",
						Content: "[SYSTEM] This issue is now unblocked. Proceed with implementation immediately.",
					}}, contextMessages...)
					slog.Info("injected unblock directive into context", "session_key", a.sess.Key)
				}
			}
		}
	}

	// Step 4: If this is a PR review comment, fetch PR and checkout its branch
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

	// Step 5: Build the user message from the event
	userMessage := a.eventToUserMessage(evt)

	// Step 6: LLM loop (max turns)
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
			// Analysis mode: block implementation tools
			if analysisMode && isImplementationTool(tc.Function.Name) {
				slog.Info("blocked implementation tool in analysis mode", "tool", tc.Function.Name, "session_key", a.sess.Key)
				messages = append(messages, provider.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "Error: Analysis mode is active. You may only use planning tools (read_file, bash ls/cat, forgejo_list_issues, forgejo_create_issue, forgejo_comment). Please post your analysis and decomposition plan as a comment instead.",
				})
				continue
			}

			slog.Info("executing tool",
				"tool", tc.Function.Name,
				"session_key", a.sess.Key,
			)

			metrics.IncToolCalls()

			res, terr := a.tools.Execute(ctx, tc.Function.Name, tc.Function.Arguments)
			if terr != nil {
				if errors.Is(terr, sentinel.ErrBlocked) {
					// Merge queue block signals the session should stop cleanly.
					body := fmt.Sprintf("This issue is blocked by the merge queue. %v\n\n<!-- ford -->", terr)
					_ = a.forgejo.PostIssueComment(ctx, evt.Repository, evt.IssueNumber, body)
					_ = a.forgejo.AddIssueLabels(ctx, evt.Repository, evt.IssueNumber, []string{"blocked"})
					a.addReaction(ctx, evt, "no_entry_sign")
					return fmt.Errorf("merge queue block: %w", sentinel.ErrBlocked)
				}
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

func (a *Agent) buildSystemPrompt(evt *event.Event, analysisMode bool, role string) string {
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

	if analysisMode {
		modeInstructions += `

## ANALYSIS-ONLY MODE (STRICT)
This issue is tagged for analysis only. You MUST NOT write any files, create branches, run git commands, or create pull requests. Your job is to:
- Read and understand the codebase thoroughly.
- Propose a concrete implementation plan.
- Break the task into specific sub-issues using forgejo_create_issue.
- Post a comprehensive summary comment with your plan.
- STOP after posting the comment.

All implementation tools (write_file, git, forgejo_create_pr, forgejo_merge_pr) are BLOCKED in this mode.`
	}

		switch role {
	case "pm":
		modeInstructions += `

## ROLE: Project Manager
You are in PM mode. You do NOT write code. Your job is:
- Understand the request and the existing codebase.
- Decompose the work into specific, trackable sub-issues.
- Use forgejo_create_issue to file each sub-issue.
- Post a summary comment with your decomposition plan.
- STOP after posting the comment. Do not implement.

Before creating sub-issues that depend on code from parent issues:
1. Use bash or read_file to check if the referenced package exists in the current clone (e.g., ls pkg/ or cat pkg/x/doc.go).
2. If the package does NOT exist, do NOT create the sub-issue yet. Post a comment explaining the dependency and expected unblock flow.
3. Only create sub-issues for work that can be done with currently available code.`
	case "reviewer":
		modeInstructions += `

## ROLE: Code Reviewer
You are in Code Review mode. You do NOT write code. Your job is:
- Read the PR diff carefully.
- Check for correctness, style, test coverage, and edge cases.
- Leave specific, actionable review comments.
- If satisfied and the PR is mergeable, call forgejo_merge_pr.
- If issues found, request changes via comment.`
	case "devops":
		modeInstructions += `

## ROLE: DevOps / Infrastructure
You are in DevOps mode. Your focus is deployment, CI/CD, and operational concerns:
- Read existing infrastructure configs.
- Propose minimal, correct changes to Docker, CI, or deploy scripts.
- Prefer changes under docker/, .forgejo/workflows/, scripts/.
- Create PRs with infrastructure-only changes.`
	case "tester":
		modeInstructions += `

## ROLE: Test Engineer
You are in Test Engineering mode. Your focus is test quality and coverage:
- Read existing code and current tests.
- Write comprehensive tests for new or existing functionality.
- Run tests and report results.
- Identify edge cases and failure modes.
- Create PRs with test-only changes.`
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
5. **Scaffold issues on empty repos**: If the repository has no commits yet (check with 'git branch -a' or verify origin/main does not exist), commit your changes and push directly to the default branch using 'git push origin HEAD:main' (or HEAD:master). Then post a comment saying the scaffold is complete. Do NOT call forgejo_create_pr — PRs require a base branch, which does not yet exist.
6. Workflow file changes (.forgejo/workflows/) MUST go through PRs.
7. When done, post a summary comment on the issue/PR.
8. **Pre-flight check**: Before writing code, verify the repo state using bash or read_file. Check what packages exist, recent commits on origin/main, your current branch, and whether listed dependencies are merged. If dependencies aren't merged, post a comment and STOP.
9. **ALWAYS rebase before creating a PR.** Before calling forgejo_create_pr, first run 'git fetch origin' and then 'git rebase origin/main' on your feature branch using the git tool (two separate calls) or the bash tool (combined). This prevents merge conflicts.
10. **Do NOT create a new PR if one already exists** for the current branch. Push to the existing branch instead.
11. **For large tasks**, analyze the work and use 'forgejo_create_issue' to break it into smaller, specific sub-issues. Sub-issues are auto-tagged 'blocked' when their parent code hasn't been merged yet. Include concrete file paths in sub-issue bodies. Always check whether referenced packages exist in the clone before creating sub-issues.
12. **When you create sub-issues via forgejo_create_issue, STOP implementing.** Your role is to decompose and coordinate — post a summary comment on the parent issue, then stop. Let the dedicated sub-issue sessions handle the actual implementation.
13. **If a comment says this issue is unblocked** (e.g. 'Dependency #N is now merged. This issue is unblocked'), check git status, verify dependencies are satisfied, and proceed with implementation immediately.
14. **Merged PRs show state "closed" in the API.** When checking if a dependency PR is resolved, look at the 'merged' field (true = merged), not the 'state' field. A PR with 'state: closed' may still be merged and ready.

### Pre-Flight Checklist (RUN FIRST)
Before writing ANY code, use bash or read_file to check:
1. What packages/directories exist: 'ls -la pkg/' or 'find . -name "*.go" -type f | head -20'
2. Recent main commits: 'git log --oneline origin/main -5'
3. Your current branch: 'git branch --show-current'
4. If the issue body says "Depends on: #N", check: 'git log --oneline origin/main | grep -c "#N"'

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

// detectAnalysisMode checks whether this issue is flagged for planning-only work.
func (a *Agent) detectAnalysisMode(ctx context.Context, evt *event.Event) bool {
	if evt.IssueNumber == 0 {
		return false
	}
	issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
	if err != nil {
		return false
	}

	// Check title for [analyze-only] or [plan-only]
	titleLower := strings.ToLower(issue.Title)
	if strings.Contains(titleLower, "[analyze-only]") || strings.Contains(titleLower, "[plan-only]") {
		slog.Info("analysis mode detected from title", "title", issue.Title, "session_key", a.sess.Key)
		return true
	}

	// Check labels for analyze-only / plan-only
	for _, l := range issue.Labels {
		name := strings.ToLower(l.Name)
		if name == "analyze-only" || name == "plan-only" {
			slog.Info("analysis mode detected from label", "label", l.Name, "session_key", a.sess.Key)
			return true
		}
	}

	return false
}

// isImplementationTool returns true for tools that write code or create PRs.
func isImplementationTool(name string) bool {
	switch name {
	case "write_file", "git", "forgejo_create_pr", "forgejo_merge_pr":
		return true
	}
	return false
}

// buildRoleRegistry constructs a tool registry filtered to the agent's role.
func buildRoleRegistry(
	forgejoAdapter *tool.ForgejoAdapter,
	mq *mergequeue.Client,
	sess *Session,
	sessionInfo tool.SessionInfo,
	agentCfg tool.AgentConfig,
	role string,
) *tool.Registry {
	registry := tool.NewRegistry()

	// Common tools: every role gets these
	registry.Register(tool.NewCommentTool(forgejoAdapter))
	registry.Register(tool.NewListIssuesTool(forgejoAdapter))
	registry.Register(tool.NewGetIssueTool(forgejoAdapter))
	registry.Register(tool.NewSearchCodeTool(forgejoAdapter))
	registry.Register(tool.NewAddReactionTool(forgejoAdapter))
	registry.Register(tool.NewBashTool(sessionInfo))
	registry.Register(tool.NewReadFileTool(sessionInfo))

	// New common tools (branches, PRs, files, hooks, etc.)
	registry.Register(tool.NewListBranchesTool(forgejoAdapter))
	registry.Register(tool.NewListPRsTool(forgejoAdapter))
	registry.Register(tool.NewPRFilesTool(forgejoAdapter))
	registry.Register(tool.NewListFilesTool(forgejoAdapter))
	registry.Register(tool.NewListHooksTool(forgejoAdapter))
	registry.Register(tool.NewListCollabsTool(forgejoAdapter))
	registry.Register(tool.NewGetVersionTool(forgejoAdapter))
	registry.Register(tool.NewGetUserTool(forgejoAdapter))

	// Role-specific tools
	switch role {
	case "pm":
		registry.Register(tool.NewCreateIssueTool(forgejoAdapter, sess.IssueNumber, 5))
		// PM cannot write code, create PRs, or merge
	case "reviewer":
		registry.Register(tool.NewMergePRTool(forgejoAdapter, true))
		// Reviewer can read, search, comment, and merge — but not write code or create PRs
	case "devops", "tester", "implementer":
		fallthrough
	default:
		registry.Register(tool.NewCreateIssueTool(forgejoAdapter, sess.IssueNumber, 0))
		registry.Register(tool.NewWriteFileTool(sessionInfo, agentCfg))
		registry.Register(tool.NewGitTool(sessionInfo))
		registry.Register(tool.NewCreatePRTool(forgejoAdapter, mq, sess.RepoDir))
		registry.Register(tool.NewMergePRTool(forgejoAdapter, false))
		// Admin tools for implementer role
		registry.Register(tool.NewDeleteBranchTool(forgejoAdapter))
		registry.Register(tool.NewCreateHookTool(forgejoAdapter))
		registry.Register(tool.NewDeleteHookTool(forgejoAdapter))
		registry.Register(tool.NewCreateTokenTool(forgejoAdapter))
	}

	return registry
}
