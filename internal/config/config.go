package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Webhook  WebhookConfig  `yaml:"webhook"`
	Forgejo  ForgejoConfig  `yaml:"forgejo"`
	Agent    AgentConfig    `yaml:"agent"`
	Providers []ProviderConfig `yaml:"providers"`
	Events   []string       `yaml:"events"`
	SessionKeyTemplate string `yaml:"session_key_template"`
	Security SecurityConfig `yaml:"security"`
	Memory   MemoryConfig   `yaml:"memory"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type WebhookConfig struct {
	Secret string `yaml:"secret"`
}

type ForgejoConfig struct {
	URL       string `yaml:"url"`
	Token     string `yaml:"token"`
	RateLimit int    `yaml:"rate_limit"`
}

type AgentConfig struct {
	MaxSessions  int           `yaml:"max_sessions"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	WorkDir      string        `yaml:"workdir"`
	MaxTurns     int           `yaml:"max_turns"`
	CommitPrefix string        `yaml:"commit_prefix"`
}

type ProviderConfig struct {
	Name      string `yaml:"name"`
	APIBase   string `yaml:"api_base"`
	APIKey    string `yaml:"api_key"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
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

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Agent: AgentConfig{
			MaxSessions:  10,
			IdleTimeout:  4 * time.Hour,
			WorkDir:      "/tmp/fordjent/work",
			MaxTurns:     25,
			CommitPrefix: "[agent-automation]",
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
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Expand environment variables in sensitive fields
	cfg.expandEnv()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func (c *Config) expandEnv() {
	c.Webhook.Secret = os.ExpandEnv(c.Webhook.Secret)
	c.Forgejo.Token = os.ExpandEnv(c.Forgejo.Token)
	c.Forgejo.URL = os.ExpandEnv(c.Forgejo.URL)
	for i := range c.Providers {
		c.Providers[i].APIKey = os.ExpandEnv(c.Providers[i].APIKey)
		c.Providers[i].APIBase = os.ExpandEnv(c.Providers[i].APIBase)
	}
}

func (c *Config) validate() error {
	if c.Forgejo.URL == "" {
		return fmt.Errorf("forgejo.url is required")
	}
	if c.Forgejo.Token == "" {
		return fmt.Errorf("forgejo.token is required")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider is required")
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
