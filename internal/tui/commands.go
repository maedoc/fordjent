package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/fordjent/fordjent/internal/forgejo"
)

type Command struct {
	Name string
	Args []string
	Raw  string
}

func ParseCommand(input string) *Command {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	parts := strings.Fields(input)
	name := strings.TrimPrefix(parts[0], "/")
	args := parts[1:]
	return &Command{Name: name, Args: args, Raw: input}
}

type CommandHandler struct {
	client *forgejo.Client
	repo   string
}

func NewCommandHandler(client *forgejo.Client, repo string) *CommandHandler {
	return &CommandHandler{client: client, repo: repo}
}

func (h *CommandHandler) Execute(ctx context.Context, cmd *Command, issueNumber int, prNumber int) string {
	switch cmd.Name {
	case "comment":
		return h.handleComment(ctx, cmd, issueNumber, prNumber)
	case "label":
		return h.handleLabel(ctx, cmd, issueNumber)
	case "unlabel":
		return h.handleUnlabel(ctx, cmd, issueNumber)
	case "start":
		return h.handleStart(ctx, issueNumber)
	case "role":
		return h.handleRole(ctx, cmd, issueNumber)
	case "approve":
		return h.handleApprove(ctx, issueNumber)
	case "merge":
		return h.handleMerge(ctx, prNumber)
	case "close":
		return h.handleClose(ctx, issueNumber, prNumber)
	case "reopen":
		return h.handleReopen(ctx, issueNumber)
	case "retry":
		return h.handleRetry(ctx, issueNumber)
	case "unblock":
		return h.handleUnblock(ctx, issueNumber)
	case "create":
		return "Tip: Press n to create a new issue with the form"
	case "help":
		return h.handleHelp()
	default:
		return fmt.Sprintf("unknown command: /%s  (type /help for list)", cmd.Name)
	}
}

func (h *CommandHandler) handleHelp() string {
	lines := []string{
		"Commands:",
		"  /comment <text>         Post a comment",
		"  /label <name> [...]    Add label(s)",
		"  /unlabel <name> [...]   Remove label(s)",
		"  /start                  Add 'ready' label",
		"  /role <role>           Set role label (implementer|pm|devops|tester|reviewer)",
		"  /approve                Add 'plan-approved' label",
		"  /merge                  Merge selected PR",
		"  /close                  Close selected issue or PR",
		"  /reopen                 Reopen selected issue",
		"  /retry                  Remove failed labels, add 'ready'",
		"  /unblock                Remove 'blocked', add 'ready'",
		"  /create                 Create a new issue (hint: press n)",
	}
	return strings.Join(lines, "\n")
}

func (h *CommandHandler) handleComment(ctx context.Context, cmd *Command, issueNum, prNum int) string {
	body := strings.Join(cmd.Args, " ")
	if body == "" {
		return "usage: /comment <text>"
	}
	num := issueNum
	if num <= 0 {
		num = prNum
	}
	if num <= 0 {
		return "no issue or PR selected"
	}
	if err := h.client.PostIssueComment(ctx, h.repo, num, body); err != nil {
		return fmt.Sprintf("error posting comment: %v", err)
	}
	return "comment posted"
}

func (h *CommandHandler) handleLabel(ctx context.Context, cmd *Command, issueNum int) string {
	if len(cmd.Args) == 0 || issueNum <= 0 {
		return "usage: /label <name>  (select an issue first)"
	}
	if err := h.client.AddIssueLabels(ctx, h.repo, issueNum, cmd.Args); err != nil {
		return fmt.Sprintf("error adding label: %v", err)
	}
	return fmt.Sprintf("added labels: %s", strings.Join(cmd.Args, ", "))
}

func (h *CommandHandler) handleUnlabel(ctx context.Context, cmd *Command, issueNum int) string {
	if len(cmd.Args) == 0 || issueNum <= 0 {
		return "usage: /unlabel <name>  (select an issue first)"
	}
	var results []string
	for _, label := range cmd.Args {
		if err := h.client.RemoveIssueLabel(ctx, h.repo, issueNum, label); err != nil {
			results = append(results, fmt.Sprintf("%s: error: %v", label, err))
		} else {
			results = append(results, fmt.Sprintf("%s: removed", label))
		}
	}
	return strings.Join(results, "; ")
}

func (h *CommandHandler) handleStart(ctx context.Context, issueNum int) string {
	if issueNum <= 0 {
		return "no issue selected"
	}
	if err := h.client.AddIssueLabels(ctx, h.repo, issueNum, []string{"ready"}); err != nil {
		return fmt.Sprintf("error adding ready label: %v", err)
	}
	return "added 'ready' label — agent will pick this up via scanner or webhook"
}

func (h *CommandHandler) handleRole(ctx context.Context, cmd *Command, issueNum int) string {
	if len(cmd.Args) == 0 || issueNum <= 0 {
		return "usage: /role <implementer|pm|devops|tester|reviewer>"
	}
	role := strings.ToLower(cmd.Args[0])
	label := "role:" + role
	if err := h.client.AddIssueLabels(ctx, h.repo, issueNum, []string{label}); err != nil {
		return fmt.Sprintf("error adding role label: %v", err)
	}
	return fmt.Sprintf("assigned role: %s", role)
}

func (h *CommandHandler) handleApprove(ctx context.Context, issueNum int) string {
	if issueNum <= 0 {
		return "no issue selected"
	}
	if err := h.client.AddIssueLabels(ctx, h.repo, issueNum, []string{"plan-approved"}); err != nil {
		return fmt.Sprintf("error approving plan: %v", err)
	}
	return "plan approved — added 'plan-approved' label"
}

func (h *CommandHandler) handleMerge(ctx context.Context, prNum int) string {
	if prNum <= 0 {
		return "no PR selected"
	}
	if err := h.client.MergePR(ctx, h.repo, prNum, "merge"); err != nil {
		return fmt.Sprintf("error merging PR: %v", err)
	}
	return fmt.Sprintf("PR #%d merged", prNum)
}

func (h *CommandHandler) handleClose(ctx context.Context, issueNum, prNum int) string {
	if prNum > 0 {
		if err := h.client.ClosePR(ctx, h.repo, prNum); err != nil {
			return fmt.Sprintf("error closing PR: %v", err)
		}
		return fmt.Sprintf("PR #%d closed", prNum)
	}
	if issueNum > 0 {
		if err := h.client.CloseIssue(ctx, h.repo, issueNum); err != nil {
			return fmt.Sprintf("error closing issue: %v", err)
		}
		return fmt.Sprintf("issue #%d closed", issueNum)
	}
	return "no issue or PR selected"
}

func (h *CommandHandler) handleReopen(ctx context.Context, issueNum int) string {
	if issueNum <= 0 {
		return "no issue selected"
	}
	if err := h.client.ReopenIssue(ctx, h.repo, issueNum); err != nil {
		return fmt.Sprintf("error reopening issue: %v", err)
	}
	return fmt.Sprintf("issue #%d reopened", issueNum)
}

func (h *CommandHandler) handleRetry(ctx context.Context, issueNum int) string {
	if issueNum <= 0 {
		return "no issue selected"
	}
	_ = h.client.RemoveIssueLabel(ctx, h.repo, issueNum, "fordjent/failed:max-turns")
	_ = h.client.RemoveIssueLabel(ctx, h.repo, issueNum, "fordjent/failed:max-retries")
	_ = h.client.RemoveIssueLabel(ctx, h.repo, issueNum, "blocked")
	if err := h.client.AddIssueLabels(ctx, h.repo, issueNum, []string{"ready"}); err != nil {
		return fmt.Sprintf("error retrying: %v", err)
	}
	return "removed failed labels, added 'ready' — agent will retry"
}

func (h *CommandHandler) handleUnblock(ctx context.Context, issueNum int) string {
	if issueNum <= 0 {
		return "no issue selected"
	}
	_ = h.client.RemoveIssueLabel(ctx, h.repo, issueNum, "blocked")
	if err := h.client.AddIssueLabels(ctx, h.repo, issueNum, []string{"ready"}); err != nil {
		return fmt.Sprintf("error unblocking: %v", err)
	}
	return "unblocked — removed 'blocked', added 'ready'"
}