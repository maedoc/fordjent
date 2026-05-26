package eval

import "time"

// Scenario defines a benchmark scenario with seed content, issue definition,
// timeout, and verification function.
type Scenario struct {
	Name        string
	Description string
	RepoName    string
	IssueTitle  string
	IssueBody   string
	SeedFiles   map[string]string
	Verify      func(string) VerificationResult
	Timeout     time.Duration
}