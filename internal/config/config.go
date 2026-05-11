package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig     `yaml:"server"`
	Webhook  WebhookConfig    `yaml:"webhook"`
	Forgejo  ForgejoConfig    `yaml:"forgejo"`
	Agent    AgentConfig      `yaml:"agent"`
	Budget   BudgetConfig     `yaml:"budget"`
	Providers []ProviderConfig `yaml:"providers"`
	Events   []string         `yaml:"events"`
	SessionKeyTemplate string `yaml:"session_key_template"`
	Security SecurityConfig   `yaml:"security"`
	Memory   MemoryConfig     `yaml:"memory"`
	Database DatabaseConfig   `yaml:"database"`
	LogLevel string           `yaml:"log_level"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type WebhookConfig struct {
	Secret string `yaml:"secret"`
}

type ForgejoConfig struct {
	URL        string `yaml:"url"`
	Token      string `yaml:"token"`
	AdminToken string `yaml:"admin_token"`
	RateLimit  int    `yaml:"rate_limit"`
}

type AgentConfig struct {
	MaxSessions             int           `yaml:"max_sessions"`
	IdleTimeout             time.Duration `yaml:"idle_timeout"`
	WorkDir                 string        `yaml:"workdir"`
	MaxTurns                int           `yaml:"max_turns"`
	MaxTurnsPM              int           `yaml:"max_turns_pm"`
	MaxTurnsImplementer     int           `yaml:"max_turns_implementer"`
	CommitPrefix            string        `yaml:"commit_prefix"`
	ContextWindow           int           `yaml:"context_window"`
	CompactionThreshold     float64       `yaml:"compaction_threshold"`
	CompactionKeepTurns     int           `yaml:"compaction_keep_turns"`
	EnableLifecycle         bool          `yaml:"enable_lifecycle"`
	EnableStaleGate         bool          `yaml:"enable_stale_gate"`
	EnableScaffoldDetection bool          `yaml:"enable_scaffold_detection"`
	EnableSessionRecovery   bool          `yaml:"enable_session_recovery"`
	EnableContextInjection  bool          `yaml:"enable_context_injection"`
	EnableAutoCollaborator  bool          `yaml:"enable_auto_collaborator"`
	DryRun                  bool          `yaml:"dry_run"`
	AllowProtectedPush      bool          `yaml:"allow_protected_push"`
	SessionTimeout          time.Duration `yaml:"session_timeout"`
	GitName                 string        `yaml:"git_name"`
	GitEmail                string        `yaml:"git_email"`
	FastProvider            string        `yaml:"fast_provider"` // DEPRECATED: use role_providers instead
	RoleProviders           map[string]string `yaml:"role_providers"` // role → provider name, e.g. {"pm": "kimi-k2.6", "reviewer": "glm-5.1"}
}

type BudgetConfig struct {
	Enabled        bool    `yaml:"enabled"`
	MaxSessionCost float64 `yaml:"max_session_cost"`
	MaxMonthlyCost float64 `yaml:"max_monthly_cost"`
}

type ProviderConfig struct {
	Name                  string  `yaml:"name"`
	APIBase               string  `yaml:"api_base"`
	APIKey                string  `yaml:"api_key"`
	Model                 string  `yaml:"model"`
	MaxTokens             int     `yaml:"max_tokens"`
	RequestTimeout        time.Duration `yaml:"request_timeout"`
	MaxRetries            int     `yaml:"max_retries"`
	RetryBaseDelay        time.Duration `yaml:"retry_base_delay"`
	RetryMaxDelay         time.Duration `yaml:"retry_max_delay"`
	MaxConcurrentLLMCalls int           `yaml:"max_concurrent_llm_calls"`
	CostPer1MInputTokens  float64 `yaml:"cost_per_1m_input_tokens"`
	CostPer1MOutputTokens float64 `yaml:"cost_per_1m_output_tokens"`
}

type SecurityConfig struct {
	ProtectedBranches     []string `yaml:"protected_branches"`
	RequirePRForWorkflows bool     `yaml:"require_pr_for_workflows"`
	FilterAgentEvents     bool     `yaml:"filter_agent_events"`
}

type MemoryConfig struct {
	Enabled        bool   `yaml:"enabled"`
	CompactionCron string `yaml:"compaction_cron"`
	CompactionPath string `yaml:"compaction_path"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Expand environment variables in the raw YAML before parsing.
	expanded := os.ExpandEnv(string(data))

	cfg := &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Agent: AgentConfig{
			MaxSessions:             10,
			IdleTimeout:             4 * time.Hour,
			WorkDir:                 "/tmp/fordjent/work",
			MaxTurns:                25,
			MaxTurnsPM:              15,
			MaxTurnsImplementer:     50,
			CommitPrefix:            "[agent-automation]",
			ContextWindow:           128000,
			CompactionThreshold:     0.80,
			CompactionKeepTurns:     8,
			EnableLifecycle:         true,
			EnableStaleGate:         true,
			EnableScaffoldDetection: true,
			EnableSessionRecovery:   true,
			EnableContextInjection:  true,
			EnableAutoCollaborator:  true,
			SessionTimeout:          30 * time.Minute,
			GitName:                 "Fordjent Agent",
			GitEmail:                "fordjent@forgejo.local",
		},
		Budget: BudgetConfig{
			Enabled:        false,
			MaxSessionCost: 0,
			MaxMonthlyCost: 0,
		},
		Forgejo: ForgejoConfig{
			RateLimit: 60,
		},
		Events: []string{"issues", "issue_comment", "pull_request", "pull_request_review_comment"},
		SessionKeyTemplate: "{{.Repository}}/issues/{{.IssueNumber}}",
		Security: SecurityConfig{
			ProtectedBranches:     []string{"main", "master"},
			RequirePRForWorkflows: true,
			FilterAgentEvents:     true,
		},
		Memory: MemoryConfig{
			Enabled:        true,
			CompactionCron: "0 2 * * *",
			CompactionPath: "docs/issues",
		},
		Database: DatabaseConfig{Path: ""},
		LogLevel: "info",
	}

	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func (c *Config) validate() error {
	var errs []string

	// Forgejo
	if c.Forgejo.URL == "" {
		errs = append(errs, "forgejo.url is required")
	} else if !strings.HasPrefix(c.Forgejo.URL, "http://") && !strings.HasPrefix(c.Forgejo.URL, "https://") {
		errs = append(errs, "forgejo.url must start with http:// or https://")
	}
	if c.Forgejo.Token == "" {
		errs = append(errs, "forgejo.token is required")
	}

	// Webhook
	if c.Webhook.Secret == "" || c.Webhook.Secret == "change-me-in-production" {
		errs = append(errs, "webhook.secret is required and must not be the default value")
	}

	// Providers
	if len(c.Providers) == 0 {
		errs = append(errs, "at least one provider is required")
	}
	for i, p := range c.Providers {
		if p.Name == "" {
			errs = append(errs, fmt.Sprintf("providers[%d].name is required", i))
		}
		if p.APIBase == "" {
			errs = append(errs, fmt.Sprintf("providers[%d].api_base is required", i))
		} else if !strings.HasPrefix(p.APIBase, "http://") && !strings.HasPrefix(p.APIBase, "https://") {
			errs = append(errs, fmt.Sprintf("providers[%d].api_base must start with http:// or https://", i))
		}
		if p.Model == "" {
			errs = append(errs, fmt.Sprintf("providers[%d].model is required", i))
		}
		if p.MaxTokens <= 0 {
			errs = append(errs, fmt.Sprintf("providers[%d].max_tokens must be > 0", i))
		}
		if p.MaxConcurrentLLMCalls <= 0 {
			c.Providers[i].MaxConcurrentLLMCalls = 3 // default
		}
	}

	// Agent
	if c.Agent.WorkDir == "" {
		errs = append(errs, "agent.workdir is required")
	}
	if c.Agent.MaxSessions <= 0 {
		errs = append(errs, "agent.max_sessions must be > 0")
	}
	if c.Agent.MaxTurns <= 0 {
		errs = append(errs, "agent.max_turns must be > 0")
	}
	if c.Agent.ContextWindow <= 0 {
		errs = append(errs, "agent.context_window must be > 0")
	}
	if c.Agent.IdleTimeout <= 0 {
		errs = append(errs, "agent.idle_timeout must be > 0")
	}
	if c.Agent.SessionTimeout <= 0 {
		errs = append(errs, "agent.session_timeout must be > 0")
	}
	if c.Agent.CompactionThreshold <= 0 || c.Agent.CompactionThreshold > 1 {
		errs = append(errs, "agent.compaction_threshold must be between 0 and 1")
	}

	// Budget
	if c.Budget.Enabled {
		if c.Budget.MaxSessionCost <= 0 {
			errs = append(errs, "budget.max_session_cost must be > 0 when budget is enabled")
		}
		if c.Budget.MaxMonthlyCost <= 0 {
			errs = append(errs, "budget.max_monthly_cost must be > 0 when budget is enabled")
		}
	}

	// Fast provider reference (legacy)
	if c.Agent.FastProvider != "" {
		found := false
		for _, p := range c.Providers {
			if p.Name == c.Agent.FastProvider {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("agent.fast_provider %q not found in providers list", c.Agent.FastProvider))
		}
	}

	// Role providers references
	for role, name := range c.Agent.RoleProviders {
		found := false
		for _, p := range c.Providers {
			if p.Name == name {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("agent.role_providers[%s] = %q not found in providers list", role, name))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// DefaultProvider returns the first configured provider.
func (c *Config) DefaultProvider() *ProviderConfig {
	if len(c.Providers) == 0 {
		return nil
	}
	return &c.Providers[0]
}

// ProviderForRole returns the provider to use for a given agent role.
// Checks role_providers map first, then fast_provider for pm/reviewer, then default.
func (c *Config) ProviderForRole(role string) *ProviderConfig {
	// Per-role mapping takes priority
	if c.Agent.RoleProviders != nil {
		if name, ok := c.Agent.RoleProviders[role]; ok {
			for _, p := range c.Providers {
				if p.Name == name {
					return &p
			}
		}
		}
	}
	// Legacy fast_provider fallback for pm/reviewer
	if (role == "pm" || role == "reviewer") && c.Agent.FastProvider != "" {
		for _, p := range c.Providers {
			if p.Name == c.Agent.FastProvider {
				return &p
			}
		}
	}
	return c.DefaultProvider()
}

// Watcher periodically reloads config from disk when the file changes.
type Watcher struct {
	path     string
	modTime  time.Time
	OnChange func(*Config)
}

// NewWatcher creates a config file watcher. Call Run() in a goroutine.
func NewWatcher(path string, initial *Config) *Watcher {
	info, _ := os.Stat(path)
	mod := time.Time{}
	if info != nil {
		mod = info.ModTime()
	}
	return &Watcher{path: path, modTime: mod}
}

// Run checks for config changes every interval and calls OnChange if modified.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(w.path)
			if err != nil || info.ModTime().Equal(w.modTime) {
				continue
			}
			w.modTime = info.ModTime()
			cfg, err := Load(w.path)
			if err != nil {
				slog.Warn("config hot-reload failed", "error", err)
				continue
			}
			slog.Info("config hot-reloaded", "path", w.path)
			if w.OnChange != nil {
				w.OnChange(cfg)
			}
		}
	}
}
