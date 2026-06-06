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

// SpecLifecycleScenario tests the spec-driven implementer workflow by
// seeding a repo with a merged spec and firing an implementer issue.
var SpecLifecycleScenario = Scenario{
	Name:        "spec-lifecycle",
	Description: "Implement a stringutil function from an OpenSpec spec",
	RepoName:    "bench-spec-lifecycle",
	IssueTitle:   "[implementer] stringutil: implement ToUpper",
	IssueBody:  "Spec: stringutil\nImplement the ToUpper function as specified in the spec.\nTask: 1 - Implement ToUpper function",
	SeedFiles: map[string]string{
		"go.mod":     "module bench-spec-lifecycle\n\ngo 1.26",
		".gitignore": "*.o\n*.exe\nstringutil\n",
		"README.md":  "# bench-spec-lifecycle\n\nA string utility library.\n",
		// OpenSpec change directory with spec artifacts
		"openspec/changes/stringutil/proposal.md": "# Why\n\nWe need a ToUpper function.\n\n# What Changes\n\n- Add ToUpper to pkg/stringutil\n\n# Capabilities\n\n### New Capabilities\n- `string-util`: string utility functions\n\n# Impact\n\npkg/stringutil/ only\n",
		"openspec/changes/stringutil/tasks.md": "## 1. Implementation\n\n- [ ] Implement ToUpper function\n- [ ] Add unit tests for ToUpper\n",
		"openspec/changes/stringutil/specs/string-util/spec.md": "## ADDED Requirements\n\n### Requirement: ToUpper converts to uppercase\nThe system SHALL provide a ToUpper function that converts a string to uppercase.\n\n#### Scenario: Basic conversion\n- **WHEN** ToUpper(\"hello\") is called\n- **THEN** the result SHALL be \"HELLO\"\n\n#### Scenario: Already uppercase\n- **WHEN** ToUpper(\"HELLO\") is called\n- **THEN** the result SHALL be \"HELLO\"\n\n#### Scenario: Empty string\n- **WHEN** ToUpper(\"\") is called\n- **THEN** the result SHALL be \"\"\n",
		// Existing code stub
		"pkg/stringutil/toupper.go": "package stringutil\n",
	},
	Verify:  SpecLifecycleVerify,
	Timeout: 15 * time.Minute,
}