package tool

import "github.com/fordjent/fordjent/internal/config"

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

// SessionAdapter adapts config and session info for tool construction.
type SessionAdapter struct {
	workDir string
	repoDir string
	Cfg     *config.Config
}

func NewSessionAdapter(workDir, repoDir string, cfg *config.Config) *SessionAdapter {
	return &SessionAdapter{workDir: workDir, repoDir: repoDir, Cfg: cfg}
}

func (s *SessionAdapter) WorkDir() string            { return s.workDir }
func (s *SessionAdapter) RepoDir() string             { return s.repoDir }
func (s *SessionAdapter) CommitPrefix() string        { return s.Cfg.Agent.CommitPrefix }
func (s *SessionAdapter) ProtectedBranches() []string { return s.Cfg.Security.ProtectedBranches }
func (s *SessionAdapter) RequirePRForWorkflows() bool { return s.Cfg.Security.RequirePRForWorkflows }
