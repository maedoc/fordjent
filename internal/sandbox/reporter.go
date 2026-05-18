package sandbox

import "context"

// ErrorReporter posts sandbox violation comments to the Forgejo issue.
type ErrorReporter interface {
	ReportSandboxViolation(ctx context.Context, repo string, issueNumber int, err SandboxError)
}
