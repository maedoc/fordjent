package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// ViolationCounter tracks consecutive sandbox violations per session and
// reports to the Forgejo issue when the threshold is exceeded.
type ViolationCounter struct {
	mu        sync.Mutex
	counts    map[string]int
	threshold int
	reporter  ErrorReporter
	repo      string
	issueNum  int
}

// NewViolationCounter creates a new ViolationCounter.
func NewViolationCounter(threshold int, reporter ErrorReporter, repo string, issueNum int) *ViolationCounter {
	if threshold <= 0 {
		threshold = 3
	}
	return &ViolationCounter{
		counts:    make(map[string]int),
		threshold: threshold,
		reporter:  reporter,
		repo:      repo,
		issueNum:  issueNum,
	}
}

// OnViolation increments the violation counter for a session.
// If the threshold is reached, calls the reporter and resets the counter.
func (c *ViolationCounter) OnViolation(ctx context.Context, sessionKey string, err SandboxError) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.counts[sessionKey]++
	count := c.counts[sessionKey]

	slog.Warn("sandbox violation recorded",
		"session_key", sessionKey,
		"count", count,
		"threshold", c.threshold,
		"command", err.Command,
		"backend", err.Backend,
	)

	if count >= c.threshold && c.reporter != nil {
		slog.Warn("sandbox violation threshold reached, reporting",
			"session_key", sessionKey,
			"count", count,
			"repo", c.repo,
			"issue", c.issueNum,
		)
		c.reporter.ReportSandboxViolation(ctx, c.repo, c.issueNum, err)
		c.counts[sessionKey] = 0
	}
}

// OnSuccess resets the violation counter for a session.
func (c *ViolationCounter) OnSuccess(sessionKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.counts[sessionKey]; ok {
		c.counts[sessionKey] = 0
	}
}

// BuildViolationComment formats a Forgejo comment for a sandbox violation.
func BuildViolationComment(err SandboxError, count int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Sandbox Policy Violation** (%d consecutive)\n\n", count))
	sb.WriteString(fmt.Sprintf("Command `%s` was blocked by the security sandbox.\n\n", err.Command))

	if err.SandboxStderr != "" {
		denyLines := extractDenyLines(err.SandboxStderr)
		if denyLines != "" {
			sb.WriteString(fmt.Sprintf("**Sandbox stderr**: %s\n", denyLines))
		}
	}
	if err.ProfilePath != "" {
		sb.WriteString(fmt.Sprintf("**Profile preserved**: `%s`\n", err.ProfilePath))
	}

	sb.WriteString("\nPossible fixes:\n")
	sb.WriteString("- Add the blocked path to `allowed_write_dirs` in config\n")
	sb.WriteString("- The agent may be accessing a path it shouldn't\n")
	sb.WriteString("\n<!-- ford -->")

	return sb.String()
}

func extractDenyLines(stderr string) string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "deny") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) > 5 {
		lines = lines[:5]
	}
	return strings.Join(lines, "; ")
}
