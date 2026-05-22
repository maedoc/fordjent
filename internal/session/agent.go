package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/fordjent/fordjent/internal/agent"
	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/cost"
	"github.com/fordjent/fordjent/internal/policy"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
	"github.com/fordjent/fordjent/internal/memory"
	"github.com/fordjent/fordjent/internal/mergequeue"
	"github.com/fordjent/fordjent/internal/metrics"
	"github.com/fordjent/fordjent/internal/provider"
	"github.com/fordjent/fordjent/internal/sandbox"
	"github.com/fordjent/fordjent/internal/sentinel"
	"github.com/fordjent/fordjent/internal/tool"
)

type turnSignature struct {
	tools string
}

// Agent is the per-session agent that processes events via LLM + tools.
type Agent struct {
	cfg              *config.Config
	sess             *Session
	forgejo          *forgejo.Client
	llm              provider.ChatCompleter
	tools            *tool.Registry
	mem              *memory.Memory
	costTracker      *cost.Tracker
	executor         *agent.TurnExecutor
	lc               *lifecycle.Lifecycle
	role             string
	analysisMode     bool
	analysisModeSet  bool
	automerge        bool
	automergeSet     bool
	pmFollowUp       bool
	triggeringIssue  int
	policy           policy.Policy
	policySet        bool
	policyDetector   *policy.CachedDetector
}

func NewAgent(cfg *config.Config, sess *Session, mq *mergequeue.Client, ct *cost.Tracker, lc *lifecycle.Lifecycle, role string, sandboxReporter sandbox.ErrorReporter, sched tool.DependencyChecker, policyDetector *policy.CachedDetector) *Agent {
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

	// Detect repo policy (cached)
	var repoPolicy policy.Policy
	if policyDetector != nil {
		repoPolicy = policyDetector.Detect(context.Background(), sess.Repository)
	} else {
		repoPolicy = policy.DefaultPolicy()
	}
	_ = repoPolicy // used in buildRoleRegistry

	sessionInfo := &sessionInfoAdapter{workDir: sess.WorkDir, repoDir: sess.RepoDir}
	forgejoAdapter := tool.NewForgejoAdapter(cfg.Forgejo.URL, cfg.Forgejo.Token)
	isScaffold := strings.HasPrefix(strings.ToLower(sess.IssueTitle), "[scaffold]") || strings.HasPrefix(strings.ToLower(sess.IssueTitle), "scaffold")
	agentCfg := &agentConfigAdapter{cfg: cfg, isScaffold: isScaffold}

	registry := buildRoleRegistry(forgejoAdapter, mq, sess, sessionInfo, agentCfg, role, cfg, sandboxReporter, sched, repoPolicy)

	executor := agent.NewTurnExecutor(cfg, llmClient, registry, ct, sess.Key, sess.Repository)

	return &Agent{
		cfg:             cfg,
		sess:            sess,
		forgejo:         forgejoClient,
		llm:             llmClient,
		tools:           registry,
		mem:             mem,
		costTracker:    ct,
		executor:        executor,
		lc:              lc,
		role:            role,
		pmFollowUp:      sess.IsPMFollowUp,
		triggeringIssue: sess.TriggeringIssue,
		policyDetector:  policyDetector,
	}
}

// ProcessEvent handles a single event: builds context, runs LLM loop with compaction/retry/cost tracking, executes tools.
func (a *Agent) ProcessEvent(ctx context.Context, evt *event.Event) error {
	// Enforce overall session timeout
	sessionTimeout := a.cfg.Agent.SessionTimeout
	sessCtx, sessCancel := context.WithTimeout(ctx, sessionTimeout)
	defer sessCancel()
	ctx = sessCtx

	// Step 1: Acknowledge with 👀 reaction
	a.addReaction(ctx, evt, "eyes")

	// Step 2: Detect analysis-only mode, FSM state, and repo policy
	analysisMode := a.detectAnalysisMode(ctx, evt)
	fsmState := a.detectIssueState(ctx, evt)

	// Detect repo-level policy (cached per session)
	if !a.policySet {
		if a.policyDetector != nil {
			a.policy = a.policyDetector.Detect(ctx, evt.Repository)
		} else {
			a.policy = policy.DefaultPolicy()
		}
		a.policySet = true
		slog.Info("detected repo policy", "repo", evt.Repository, "policy", a.policy.String(), "session_key", a.sess.Key)
	}

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
		var firstToolErr error
		consecutiveBlocks := 0
		for _, tc := range result.Response.ToolCalls {
			// Analysis mode: block implementation tools
			if analysisMode && isImplementationTool(tc.Function.Name) {
				slog.Info("blocked implementation tool in analysis mode", "tool", tc.Function.Name, "session_key", a.sess.Key)
				consecutiveBlocks++
				blockMsg := "Error: Analysis mode is active. You may only use planning tools (read_file, bash ls/cat, forgejo_list_issues, forgejo_create_issue, forgejo_comment). Please post your analysis and decomposition plan as a comment instead."
				if consecutiveBlocks >= 3 {
					blockMsg = "Error: STOP attempting implementation tools. You are in analysis mode. Call forgejo_comment NOW to post your plan, then end the session."
				}
				messages = append(messages, provider.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    blockMsg,
				})
				continue
			}

			// FSM state: block implementation tools in planning/blocked states
			if (fsmState == lifecycle.StatePlanning || fsmState == lifecycle.StateFSMBlocked) && isImplementationTool(tc.Function.Name) {
				slog.Info("blocked implementation tool in FSM state", "tool", tc.Function.Name, "state", string(fsmState), "session_key", a.sess.Key)
				consecutiveBlocks++
				var blockMsg string
				switch fsmState {
				case lifecycle.StateFSMBlocked:
					blockMsg = "Error: This issue is Blocked. Do not attempt implementation. Post a comment explaining the blocker."
				case lifecycle.StatePlanning:
					blockMsg = "Error: This issue is in Planning state. You may only use read-only and planning tools. Post your plan as a comment, then STOP."
				}
				if consecutiveBlocks >= 3 {
					parentNum := evt.IssueNumber
					blockMsg = fmt.Sprintf("Error: You have been blocked %d times. STOP attempting implementation tools. This issue is in %s state — you CANNOT write code. Call forgejo_comment to post a message explaining: (1) why you cannot proceed, and (2) what label or action is needed. For planning state: add 'plan-approved' to the parent issue (#%d) to unblock. Then end the session.", consecutiveBlocks, fsmState, parentNum)
				}
				messages = append(messages, provider.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    blockMsg,
				})
				continue
			}

			// Policy: block forgejo_merge_pr when policy says so
			if tc.Function.Name == "forgejo_merge_pr" {
				if a.policy.NoAutoMerge && a.role == "reviewer" {
					slog.Info("blocked merge by policy: no-auto-merge", "tool", tc.Function.Name, "session_key", a.sess.Key)
					messages = append(messages, provider.Message{
						Role:       "tool",
						ToolCallID: tc.ID,
					Content:    "Error: This repo has a no-auto-merge policy. Post your review as a comment and wait for a human to merge the PR. Do NOT call forgejo_merge_pr.",
					})
					continue
				}
				if a.policy.RequireReview {
					// Check if PR has 'approved' label
					if evt.PRNumber > 0 {
						prIssue, prErr := a.forgejo.GetIssue(ctx, evt.Repository, evt.PRNumber)
						if prErr == nil && prIssue != nil {
							hasApproved := false
							for _, l := range prIssue.Labels {
								if l.Name == "approved" {
									hasApproved = true
									break
								}
							}
						if !hasApproved {
							slog.Info("blocked merge by policy: require-review (no approved label)", "pr", evt.PRNumber, "session_key", a.sess.Key)
							messages = append(messages, provider.Message{
								Role:       "tool",
								ToolCallID: tc.ID,
								Content:    "Error: This repo requires human review before merging. The PR must have an 'approved' label. Post your review as a comment and wait for a human to add the 'approved' label.",
							})
							continue
							}
						}
					}
				}
			}

			slog.Info("executing tool",
				"tool", tc.Function.Name,
				"session_key", a.sess.Key,
			)

			metrics.IncToolCalls()

			res, terr := a.tools.Execute(ctx, tc.Function.Name, tc.Function.Arguments)
			if terr != nil {
				if firstToolErr == nil {
					firstToolErr = terr
				}
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

		// Record turn progress in lifecycle DB for diagnostics (after tool execution)
		if a.lc != nil {
			tokensIn, tokensOut := 0, 0
			if result.Usage != nil {
				tokensIn = result.Usage.PromptTokens
				tokensOut = result.Usage.CompletionTokens
			}
			a.lc.RecordTurn(ctx, a.sess.Key, turn, len(result.Response.ToolCalls),
				int(result.Latency.Milliseconds()), tokensIn, tokensOut, firstToolErr)
		}
	}

	slog.Warn("max turns reached", "session_key", a.sess.Key)
	a.addReaction(ctx, evt, "warning")
	return fmt.Errorf("max turns (%d) reached: %w", maxTurns, sentinel.ErrMaxTurnsReached)
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
	case "reviewer":
		if a.cfg.Agent.MaxTurnsReviewer > 0 {
			return a.cfg.Agent.MaxTurnsReviewer
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

		if a.policy.PlanFirst {
			modeInstructions += `

- This repo has a plan-first policy. After posting your decomposition plan, tell the human to add the 'plan-approved' label to this parent issue to unblock the sub-issues for implementation. The sub-issues will remain in 'planning' state until this label is added.`
		}

		if a.pmFollowUp {
			modeInstructions += fmt.Sprintf(`

## PM FOLLOW-UP MODE
You are in PM Follow-up mode. All sub-issues of this parent PM issue have been completed (the last one was #%d).
Your job is:
1. Use forgejo_get_sub_issues to fetch the status of all sub-issues.
2. Summarize the progress: which sub-issues are done, which are still open (if any).
3. Identify next steps: create new issues if needed, or close the parent if all work is complete.
4. If all sub-issues are complete and no new work is needed, post a completion summary comment on the parent issue using forgejo_comment and suggest closing it.
5. If you identify additional work, use forgejo_create_issue to file new sub-issues.
6. Do NOT write code. Do NOT create PRs.
7. Post your follow-up summary as a comment on the parent issue.`, a.triggeringIssue)
		}
	case "reviewer":
		modeInstructions += `

## ROLE: Code Reviewer
You are in Code Review mode. You do NOT write code. Your job is:
- Read the PR diff carefully using bash (git diff origin/main...HEAD).
- Check for correctness, style, test coverage, and edge cases.
- If issues found, post a comment describing what needs to change.
- DO NOT leave PRs open indefinitely — either merge or request changes.`

		// Policy-aware merge instructions
		if a.policy.NoAutoMerge {
			modeInstructions += `

- IMPORTANT: This repo has a no-auto-merge policy. You MUST NOT call forgejo_merge_pr. Post your review as a comment and let a human decide when to merge.`
		} else if a.policy.RequireReview {
			modeInstructions += `

- IMPORTANT: This repo requires human review before merging. You MUST NOT call forgejo_merge_pr unless the PR has an 'approved' label. Post your review as a comment and wait for a human to add the 'approved' label.`
		} else {
			modeInstructions += `

- If the PR was created by a bot (fordjent-bot) and the code is correct, call forgejo_merge_pr IMMEDIATELY.`
		}

		hasAutomerge := a.detectAutomerge(ctx, evt)
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
- Create PRs with test-only changes.

If you encounter ambiguity or need clarification on requirements, use forgejo_ping_parent to ask the PM a question on the parent issue. Find the parent issue number from the 'Depends on: #N' reference in your issue body. Include your specific question and any relevant context about what's blocking you.`
	case "implementer":
		modeInstructions += `

## ROLE: Implementer
You are in Implementer mode. Your focus is writing production code:
- Read and understand the requirements from the issue and parent context.
- Implement the required functionality with clean, minimal changes.
- Write tests for your implementation.
- Create a PR when done.

If you encounter ambiguity or need clarification on requirements, use forgejo_ping_parent to ask the PM a question on the parent issue. Find the parent issue number from the 'Depends on: #N' reference in your issue body. Include your specific question and any relevant context about what's blocking you. The PM will respond on the parent issue.`
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

			// Parent context: if this issue references a parent, fetch it
			if parentRef := extractParentRef(issue.Body); parentRef > 0 && parentRef != evt.IssueNumber {
				parent, parentErr := a.forgejo.GetIssue(ctx, evt.Repository, parentRef)
				if parentErr == nil && parent != nil {
					excerpt := parent.Body
					if len(excerpt) > 2000 {
						excerpt = excerpt[:2000] + "\n\n... (truncated)"
					}
					messages = append(messages, provider.Message{
						Role:    "user",
						Content: fmt.Sprintf("[Parent Context — issue #%d]\nTitle: %s\n\n%s", parent.Number, parent.Title, excerpt),
					})

					// Also fetch parent's first 5 comments
					parentComments, pcErr := a.forgejo.ListComments(ctx, evt.Repository, parentRef)
					if pcErr == nil {
						for i, pc := range parentComments {
							if i >= 5 {
								break
							}
							messages = append(messages, provider.Message{
								Role:    "user",
								Content: fmt.Sprintf("[Parent #%d — comment by @%s] %s", parentRef, pc.User, pc.Body),
							})
						}
					}
				}
			}
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

	// For PR review sessions, fetch parent issue context from closing references in the PR body
	if evt.PRNumber > 0 {
		var prBody string
		// Try to get PR body from webhook payload first
		if pr, ok := evt.Payload["pull_request"].(map[string]interface{}); ok {
			if body, ok := pr["body"].(string); ok {
				prBody = body
			}
		}
		// If no body in payload, fetch from API
		if prBody == "" {
			pr, err := a.forgejo.GetPR(ctx, evt.Repository, evt.PRNumber)
			if err == nil && pr != nil {
				prBody = pr.Body
			}
		}
		if prBody != "" {
			if parentCtx := a.fetchParentContext(ctx, evt.Repository, prBody); parentCtx != "" {
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: parentCtx,
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

	// Check for previous session memory to provide retry context
	if evt.IssueNumber > 0 && evt.Type == event.IssueOpened {
		sessionKey := fmt.Sprintf("%s/issues/%d", evt.Repository, evt.IssueNumber)
		prevWorkDir := filepath.Join(a.cfg.Agent.WorkDir, sessionKey)
		memFile := filepath.Join(prevWorkDir, "memory.jsonl")
		if data, err := os.ReadFile(memFile); err == nil && len(data) > 0 {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			start := 0
			if len(lines) > 5 {
				start = len(lines) - 5
			}
			var summary []string
			for _, line := range lines[start:] {
				var entry map[string]interface{}
				if json.Unmarshal([]byte(line), &entry) == nil {
					if role, ok := entry["role"].(string); ok && role == "assistant" {
						if content, ok := entry["content"].(string); ok {
							excerpt := content
							if len(excerpt) > 200 {
								excerpt = excerpt[:200] + "..."
							}
							summary = append(summary, excerpt)
						}
					}
				}
			}
			if len(summary) > 0 {
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: fmt.Sprintf("[Previous Session Context]\nThis issue was previously attempted. Here is a summary of the last session's work:\n%s\n\nPick up where the previous session left off, but try a different approach if the same strategy failed.", strings.Join(summary, "\n")),
				})
			}
		}
	}

	// PM follow-up: inject a system directive to guide the follow-up session
	if a.pmFollowUp && evt.IssueNumber > 0 {
		messages = append([]provider.Message{{
			Role:    "user",
			Content: fmt.Sprintf("[SYSTEM] This is a PM follow-up session. The sub-issue #%d has just been completed. Use forgejo_get_sub_issues to check the status of all sub-issues for parent issue #%d, then post a follow-up summary.", a.triggeringIssue, evt.IssueNumber),
		}}, messages...)
	}

	return messages, nil
}

// extractParentRef parses the first "Depends on: #N" or "Closes: #N" reference
// from an issue body and returns the issue number, or 0 if none found.
func extractParentRef(body string) int {
	for _, prefix := range []string{"Depends on: #", "depends on: #", "Closes: #", "closes: #"} {
		idx := strings.Index(body, prefix)
		if idx >= 0 {
			rest := body[idx+len(prefix):]
			var num int
			for i := 0; i < len(rest) && rest[i] >= '0' && rest[i] <= '9'; i++ {
				num = num*10 + int(rest[i]-'0')
			}
			if num > 0 {
				return num
			}
		}
	}
	return 0
}

// closingRefRe matches standard closing keywords: Closes, Fixes, Resolves, Close, Fix, Resolve
// followed by an optional colon, optional whitespace, and #N. Case-insensitive.
var closingRefRe = regexp.MustCompile(`(?i)(?:close(?:s)?|fix(?:es)?|resolve(?:s)?)\s*:?\s*#(\d+)`)

// parseClosingRefs extracts all issue numbers referenced by closing keywords
// in the given text (e.g., "Closes #5", "Fixes: #5, #6", "resolves #5").
func parseClosingRefs(text string) []int {
	matches := closingRefRe.FindAllStringSubmatch(text, -1)
	seen := make(map[int]bool)
	var refs []int
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		var num int
		for _, ch := range m[1] {
			if ch >= '0' && ch <= '9' {
				num = num*10 + int(ch-'0')
			}
		}
		if num > 0 && !seen[num] {
			seen[num] = true
			refs = append(refs, num)
		}
	}
	return refs
}

// fetchParentContext fetches the bodies and comments of issues referenced by
// closing keywords in the PR body. Returns a formatted string for the reviewer's
// context, or empty string if no references found or on API errors (non-fatal).
func (a *Agent) fetchParentContext(ctx context.Context, repo string, prBody string) string {
	refs := parseClosingRefs(prBody)
	if len(refs) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n## Parent Issue Context\nThis PR references the following issues:\n")

	for _, issueNum := range refs {
		issue, err := a.forgejo.GetIssue(ctx, repo, issueNum)
		if err != nil {
			slog.Warn("fetchParentContext: failed to get issue", "issue", issueNum, "error", err)
			continue
		}
		if issue == nil {
			continue
		}

		body := issue.Body
		if len(body) > 2000 {
			body = body[:2000] + "\n\n... (truncated)"
		}

		sb.WriteString(fmt.Sprintf("\n### Issue #%d: %s\n**Body**: %s\n", issueNum, issue.Title, body))

		comments, err := a.forgejo.ListComments(ctx, repo, issueNum)
		if err != nil {
			slog.Warn("fetchParentContext: failed to list comments", "issue", issueNum, "error", err)
			continue
		}
		if len(comments) > 0 {
			sb.WriteString("**Comments**:\n")
			limit := 5
			if len(comments) < limit {
				limit = len(comments)
			}
			for i := 0; i < limit; i++ {
				c := comments[i]
				cBody := c.Body
				if len(cBody) > 500 {
					cBody = cBody[:500] + "..."
				}
				sb.WriteString(fmt.Sprintf("- @%s: %s\n", c.User, cBody))
			}
			if len(comments) > 5 {
				sb.WriteString(fmt.Sprintf("- ... and %d more comments\n", len(comments)-5))
			}
		}
	}

	return sb.String()
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
	} else if err == nil && len(payloadJSON) >= 5000 {
		slog.Warn("payload truncated for context window",
			"payload_size", len(payloadJSON),
			"event_type", evt.Type,
			"issue", evt.IssueNumber,
			"pr", evt.PRNumber,
		)
		sb.WriteString(fmt.Sprintf("\n<details>\n<summary>Full payload (truncated — %d chars)</summary>\n\n```json\n%s\n[...]\n```\n</details>", len(payloadJSON), string(payloadJSON[:500])))
	}

	return sb.String()
}

// detectAnalysisMode checks whether this issue is flagged for planning-only work.
// Result is cached after first call to avoid per-turn API latency.
func (a *Agent) detectAnalysisMode(ctx context.Context, evt *event.Event) bool {
	if a.analysisModeSet {
		return a.analysisMode
	}
	a.analysisModeSet = true

	if evt.IssueNumber == 0 {
		return false
	}
	issue, err := a.forgejo.GetIssue(ctx, evt.Repository, evt.IssueNumber)
	if err != nil {
		return false
	}

	// Cache automerge status from the same API call
	for _, l := range issue.Labels {
		if l.Name == "automerge" {
			a.automerge = true
		}
	}
	a.automergeSet = true

	// Check title for [analyze-only] or [plan-only]
	titleLower := strings.ToLower(issue.Title)
	if strings.Contains(titleLower, "[analyze-only]") || strings.Contains(titleLower, "[plan-only]") {
		slog.Info("analysis mode detected from title", "title", issue.Title, "session_key", a.sess.Key)
		a.analysisMode = true
		return true
	}

	// Check labels for analyze-only / plan-only
	for _, l := range issue.Labels {
		name := strings.ToLower(l.Name)
		if name == "analyze-only" || name == "plan-only" {
			slog.Info("analysis mode detected from label", "label", l.Name, "session_key", a.sess.Key)
			a.analysisMode = true
			return true
		}
	}

	return false
}

// detectAutomerge returns whether the issue has an 'automerge' label, cached from the
// first detectAnalysisMode call to avoid an extra Forgejo API hit.
func (a *Agent) detectAutomerge(ctx context.Context, evt *event.Event) bool {
	if !a.automergeSet {
		_ = a.detectAnalysisMode(ctx, evt) // populate cache
	}
	return a.automerge
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
	cfg *config.Config,
	sandboxReporter sandbox.ErrorReporter,
	sched tool.DependencyChecker,
	repoPolicy policy.Policy,
) *tool.Registry {
	registry := tool.NewRegistry()

	sandboxCfg := sandbox.Config{
		Enabled:               cfg.Sandbox.Enabled,
		Backend:               cfg.Sandbox.Backend,
		RepoDir:               sess.RepoDir,
		TmpfsSizeMB:           cfg.Sandbox.TmpfsSizeMB,
		KeepProfilesOnFailure: cfg.Sandbox.KeepProfilesOnFailure,
		ViolationCommentThreshold: cfg.Sandbox.ViolationCommentThreshold,
		AllowedWriteDirs:      cfg.Sandbox.AllowedWriteDirs,
	}

	// Auto-detect Go cache dirs for sandbox allowed-write paths
	goCacheDirs := sandbox.DetectGoCacheDirs()
	if len(goCacheDirs) > 0 {
		sandboxCfg.AllowedWriteDirs = append(sandboxCfg.AllowedWriteDirs, goCacheDirs...)
		slog.Info("auto-detected Go cache dirs for sandbox", "dirs", goCacheDirs)
	}

	// Create violation counter for sandbox errors
	var violCounter *sandbox.ViolationCounter
	if sandboxReporter != nil && cfg.Sandbox.Enabled {
		violCounter = sandbox.NewViolationCounter(
			cfg.Sandbox.ViolationCommentThreshold,
			sandboxReporter,
			sess.Repository,
			sess.IssueNumber,
		)
	}

	// Common tools: every role gets these
	registry.Register(tool.NewCommentTool(forgejoAdapter))
	registry.Register(tool.NewListIssuesTool(forgejoAdapter))
	registry.Register(tool.NewGetIssueTool(forgejoAdapter))
	registry.Register(tool.NewSearchCodeTool(forgejoAdapter))
	registry.Register(tool.NewAddReactionTool(forgejoAdapter))
	bashT := tool.NewBashTool(sessionInfo, agentCfg)
	bashT.SetSandboxConfig(sandboxCfg)
	if violCounter != nil {
		bashT.SetViolationCounter(violCounter, sess.Key)
	}
	registry.Register(bashT)
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
	registry.Register(tool.NewGetSiblingIssuesTool(forgejoAdapter))

	// Role-specific tools
	switch role {
	case "pm":
		cit := tool.NewCreateIssueTool(forgejoAdapter, sess.IssueNumber, 5)
		cit.SetScheduler(sched)
		cit.SetPlanFirst(repoPolicy.PlanFirst)
		registry.Register(cit)
		registry.Register(tool.NewGetSubIssuesTool(forgejoAdapter))
		// PM cannot write code, create PRs, or merge
	case "reviewer":
		registry.Register(tool.NewMergePRTool(forgejoAdapter, true))
		// Reviewer can read, search, comment, and merge — but not write code or create PRs
	case "devops", "tester", "implementer":
		fallthrough
	default:
		cit := tool.NewCreateIssueTool(forgejoAdapter, sess.IssueNumber, 0)
		cit.SetScheduler(sched)
		registry.Register(cit)
		registry.Register(tool.NewWriteFileTool(sessionInfo, agentCfg))
		gitT := tool.NewGitTool(sessionInfo, agentCfg)
		gitT.SetSandboxConfig(sandboxCfg)
		if violCounter != nil {
			gitT.SetViolationCounter(violCounter, sess.Key)
		}
		registry.Register(gitT)
		registry.Register(tool.NewCreatePRTool(forgejoAdapter, mq, sess.RepoDir))
		registry.Register(tool.NewMergePRTool(forgejoAdapter, false))
		// Admin tools for implementer role
		registry.Register(tool.NewDeleteBranchTool(forgejoAdapter))
		registry.Register(tool.NewCreateHookTool(forgejoAdapter))
		registry.Register(tool.NewDeleteHookTool(forgejoAdapter))
	}

	if role == "implementer" || role == "tester" {
		registry.Register(tool.NewPingParentTool(forgejoAdapter))
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
	case lifecycle.StatePlanApproved:
		return `

## STATE: Plan Approved
The plan for this issue has been approved. You MUST:
1. Read the issue and any plan comments
2. Implement the approved plan
3. Create a PR when done

You have full access to implementation tools. Proceed with coding.`
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
