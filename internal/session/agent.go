package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/fordjent/fordjent/internal/agent"
	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"github.com/fordjent/fordjent/internal/memory"
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/sentinel"
	"github.com/fordjent/fordjent/internal/tool"
)

type turnSignature struct {
	tools string
}

// Agent is the per-session agent that processes events via LLM + tools.
type Agent struct {
	cfg         *config.Config
	sess        *Session
	forgejo     *forgejo.Client
	llm         provider.ChatCompleter
	tools       *tool.Registry
	mem         *memory.Memory
	costTracker *cost.Tracker
	executor    *agent.TurnExecutor
	lc          *lifecycle.Lifecycle
	role        string
}

func NewAgent(cfg *config.Config, sess *Session, mq *mergequeue.Client, ct *cost.Tracker, lc *lifecycle.Lifecycle, role string) *Agent {
	forgejoClient := forgejo.NewClient(cfg.Forgejo.URL, cfg.Forgejo.Token)
	prov := cfg.ProviderForRole(role)
	var llmClient provider.ChatCompleter = provider.NewClient(prov)

	if cfg.Agent.FallbackProvider != "" {
		fallbackProv := cfg.ProviderByName(cfg.Agent.FallbackProvider)
		if fallbackProv != nil && fallbackProv.Name != prov.Name {
			fallbackClient := provider.NewClient(fallbackProv)
			llmClient = provider.NewFallbackClient(llmClient.(*provider.Client), fallbackClient)
		}
	}

	mem := memory.New(cfg, sess.WorkDir, forgejoClient)

	sessionInfo := &sessionInfoAdapter{workDir: sess.WorkDir, repoDir: sess.RepoDir}
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	isScaffold := strings.HasPrefix(strings.ToLower(sess.IssueTitle), "[scaffold]") || strings.HasPrefix(strings.ToLower(sess.IssueTitle), "scaffold")
	agentCfg := &agentConfigAdapter{cfg: cfg, isScaffold: isScaffold}

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
		lc:          lc,
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

	// Step 2: Detect analysis-only mode and FSM state
	analysisMode := a.detectAnalysisMode(ctx, evt)
	fsmState := a.detectIssueState(ctx, evt)

	// Step 3: Build context for the LLM
	systemPrompt := a.buildSystemPrompt(ctx, evt, analysisMode, a.role, fsmState)
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

	maxTurns := a.effectiveMaxTurns()

	consecutiveErrors := 0
	maxConsecutiveErrors := 3

	recentSigs := make([]turnSignature, 0, 3)

	reflectEvery := a.cfg.Agent.ReflectionInterval
	if reflectEvery <= 0 {
		reflectEvery = 5
	}

	for turn := 0; turn < maxTurns; turn++ {
		slog.Info("LLM turn begin",
			"session_key", a.sess.Key,
			"turn", turn,
			"messages", len(messages),
		)

		result, updatedMessages, err := a.executor.Run(ctx, systemPrompt, messages)
		messages = updatedMessages

		if err != nil {
			consecutiveErrors++
			slog.Warn("turn failed",
				"session_key", a.sess.Key,
				"turn", turn,
				"consecutive_errors", consecutiveErrors,
				"error", err,
			)
			if consecutiveErrors >= maxConsecutiveErrors {
				a.addReaction(ctx, evt, "x")
				return fmt.Errorf("aborted after %d consecutive failures: %w", consecutiveErrors, err)
			}
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: fmt.Sprintf("[System] The previous turn failed: %s. Adjust your approach and try again.", err),
			})
			continue
		}

		consecutiveErrors = 0

		if turn > 0 && turn%reflectEvery == 0 {
			messages = append(messages, provider.Message{
				Role: "user",
				Content: `[System] REFLECTION CHECKPOINT

Pause and reflect on your progress:
1. What has been accomplished so far?
2. What's working well?
3. What's not working or blocking progress?
4. Should the approach be adjusted?
5. What are the next priorities?

Update the issue comment with your reflection, then continue working.`,
			})
			slog.Info("reflection checkpoint injected", "session_key", a.sess.Key, "turn", turn)
		}

		// Track metrics
		metrics.IncLLMCalls()
		if result.Usage != nil {
			metrics.AddTokens(int64(result.Usage.PromptTokens), int64(result.Usage.CompletionTokens))
		}
		if result.CostUSD > 0 {
			metrics.AddCost(result.CostUSD)
		}

		// Record turn progress in lifecycle DB for diagnostics
		if a.lc != nil {
			var turnErr error
			tokensIn, tokensOut := 0, 0
			if result.Usage != nil {
				tokensIn = result.Usage.PromptTokens
				tokensOut = result.Usage.CompletionTokens
			}
			a.lc.RecordTurn(ctx, a.sess.Key, turn, len(result.Response.ToolCalls),
				int(result.Latency.Milliseconds()), tokensIn, tokensOut, turnErr)
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

			// FSM state: block implementation tools in planning/blocked states
			if (fsmState == lifecycle.StatePlanning || fsmState == lifecycle.StateFSMBlocked) && isImplementationTool(tc.Function.Name) {
				slog.Info("blocked implementation tool in FSM state", "tool", tc.Function.Name, "state", string(fsmState), "session_key", a.sess.Key)
				var blockMsg string
				switch fsmState {
				case lifecycle.StateFSMBlocked:
					blockMsg = "Error: This issue is Blocked. Do not attempt implementation. Post a comment explaining the blocker."
				case lifecycle.StatePlanning:
					blockMsg = "Error: This issue is in Planning state. You may only use read-only and planning tools. Post your plan as a comment, then STOP."
				}
				messages = append(messages, provider.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    blockMsg,
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

		sig := buildTurnSignature(result.Response.ToolCalls)
		recentSigs = append(recentSigs, sig)
		if len(recentSigs) > 3 {
			recentSigs = recentSigs[1:]
		}
		if len(recentSigs) == 3 && allSameSignature(recentSigs) {
			messages = append(messages, provider.Message{
				Role:    "user",
				Content: "[System] WARNING: Your last 3 turns performed identical actions. You may be stuck in a loop. Try a completely different approach, or describe the blocker and stop.",
			})
			slog.Warn("stall detected", "session_key", a.sess.Key, "turn", turn)
		}
	}

	slog.Warn("max turns reached", "session_key", a.sess.Key)
	a.addReaction(ctx, evt, "warning")
	return fmt.Errorf("max turns (%d) reached: %w", maxTurns, agent.ErrMaxTurnsReached)
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

// effectiveMaxTurns returns the turn limit based on role.
func (a *Agent) effectiveMaxTurns() int {
	switch a.role {
	case "pm":
		if a.cfg.Agent.MaxTurnsPM > 0 {
			return a.cfg.Agent.MaxTurnsPM
		}
	case "implementer":
		if a.cfg.Agent.MaxTurnsImplementer > 0 {
			return a.cfg.Agent.MaxTurnsImplementer
		}
	}
	return a.cfg.Agent.MaxTurns
}

func (a *Agent) buildSystemPrompt(ctx context.Context, evt *event.Event, analysisMode bool, role string, fsmState lifecycle.IssueState) string {
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
3. Only create sub-issues for work that can be done with currently available code.
4. **CRITICAL**: In each sub-issue body, add a line 'Depends on: #N' (where #N is the parent issue or another sub-issue that must be completed first). This enables the scheduler to automatically unblock issues when their dependencies are merged. Example:
   - If issue #2 must be done before #3, add 'Depends on: #2' in #3's body.
   - If #4 depends on both #2 and #3, add 'Depends on: #2, #3'.`
	case "reviewer":
		modeInstructions += `

## ROLE: Code Reviewer
You are in Code Review mode. You do NOT write code. Your job is:
- Read the PR diff carefully using bash (git diff origin/main...HEAD).
- Check for correctness, style, test coverage, and edge cases.
- If the PR was created by a bot (fordjent-bot) and the code is correct, call forgejo_merge_pr IMMEDIATELY.
- If issues found, post a comment describing what needs to change.
- DO NOT leave PRs open indefinitely — either merge or request changes.`

		hasAutomerge := false
		if evt.IssueNumber > 0 {
			issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
			if err == nil && issue != nil {
				for _, l := range issue.Labels {
					if l.Name == "automerge" {
						hasAutomerge = true
						break
					}
				}
			}
		}
		if hasAutomerge {
			modeInstructions += `

- This PR has the 'automerge' label. Review the diff, verify build and tests pass.
- If the code is correct and there are no conflicts, call forgejo_merge_pr immediately.
- If issues are found, post a comment describing them and remove the 'automerge' label.`
		}
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

	stateInstructions := issueStateInstructions(fsmState)

	return fmt.Sprintf(`You are Fordjent, an autonomous coding agent that helps with software development tasks on a Forgejo instance.

## Current Context
- Repository: %s
- Event: %s (action: %s)
- Sender: @%s
- Target: %s
%s
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
11. **For large tasks**, analyze the work and use 'forgejo_create_issue' to break it into smaller, specific sub-issues. Include concrete file paths in sub-issue bodies. Always check whether referenced packages exist in the clone before creating sub-issues. Do NOT add 'blocked' labels to sub-issues — the scheduler will manage blocking/unblocking automatically based on 'Depends on: #N' declarations.
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
		stateInstructions,
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

// detectIssueState derives the FSM state from the issue's current labels.
func (a *Agent) detectIssueState(ctx context.Context, evt *event.Event) lifecycle.IssueState {
	if evt.IssueNumber == 0 {
		return lifecycle.StateOpened
	}
	issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
	if err != nil || issue == nil {
		return lifecycle.StateOpened
	}
	labelNames := make([]string, len(issue.Labels))
	for i, l := range issue.Labels {
		labelNames[i] = l.Name
	}
	return lifecycle.StateFromLabels(labelNames)
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
	registry.Register(tool.NewBashTool(sessionInfo, agentCfg))
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
		registry.Register(tool.NewGitTool(sessionInfo, agentCfg))
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

func buildTurnSignature(calls []provider.ToolCall) turnSignature {
	var parts []string
	for _, tc := range calls {
		h := sha256.Sum256([]byte(tc.Function.Arguments))
		parts = append(parts, tc.Function.Name+"("+hex.EncodeToString(h[:4])+")")
	}
	sort.Strings(parts)
	return turnSignature{tools: strings.Join(parts, ",")}
}

func allSameSignature(sigs []turnSignature) bool {
	if len(sigs) < 2 {
		return false
	}
	for i := 1; i < len(sigs); i++ {
		if sigs[i].tools != sigs[0].tools {
			return false
		}
	}
	return true
}

func issueStateInstructions(state lifecycle.IssueState) string {
	switch state {
	case lifecycle.StatePlanning:
		return `

## STATE: Planning
This issue is in planning mode. You MUST:
1. Read and understand the codebase
2. Propose a concrete implementation plan
3. Break into sub-issues if needed
4. Post a summary comment
5. STOP — do not write code

Implementation tools (write_file, git, forgejo_create_pr, forgejo_merge_pr) are BLOCKED in this state.`
	case lifecycle.StateFSMBlocked:
		return `

## STATE: Blocked
This issue has a 'blocked' label. Before giving up:
1. Check the issue body for 'Depends on: #N' references
2. For each dependency, use forgejo_get_issue to check if it's actually blocking:
   - If the dependency issue is CLOSED or has a MERGED PR → it's resolved, proceed
   - If the dependency has NO associated PR → it's a coordination issue, not a code dependency — proceed
   - If the dependency has an OPEN PR → it's genuinely blocking, post a comment explaining the blocker and STOP
3. If all dependencies are resolved, remove the 'blocked' label and proceed with implementation

Implementation tools (write_file, git, forgejo_create_pr, forgejo_merge_pr) are BLOCKED while the 'blocked' label is present. If you verify dependencies are resolved, you may use forgejo tools to remove the 'blocked' label and add 'ready'.`
	}
	return ""
}
