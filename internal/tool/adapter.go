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
}
