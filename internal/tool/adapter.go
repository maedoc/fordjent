package tool

// SessionInfo provides session directory information for tools.
type SessionInfo interface {
	WorkDir() string
	RepoDir() string
}

// AgentConfig provides agent configuration for tools.
type AgentConfig interface {
	CommitPrefix() string
	ProtectedBranches() []string
	RequirePRForWorkflows() bool
	DryRun() bool
	AllowProtectedPush() bool // scaffold sessions may push to protected branches
	IsScaffold() bool        // true when the session is for a scaffold issue
}
