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
	Telegram TelegramConfig `yaml:"telegram"`
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

type TelegramConfig struct {
	Enabled      bool                       `yaml:"enabled"`
	Token        string                     `yaml:"token"`
	PollTimeout  int                        `yaml:"poll_timeout"`
	AllowedChats []int64                    `yaml:"allowed_chats"`
	ChatBindings map[int64]TelegramChatBind `yaml:"chat_bindings"`
}

type TelegramChatBind struct {
	Repository   string  `yaml:"repository"`
	AllowedUsers []string `yaml:"allowed_users"`
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
	c.Telegram.Token = os.ExpandEnv(c.Telegram.Token)
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

// RepositoryForChat returns the bound repository for a Telegram chat ID.
// Returns ("", false) if no binding exists.
func (c *Config) RepositoryForChat(chatID int64) (string, bool) {
	if c.Telegram.ChatBindings == nil {
		return "", false
	}
	bind, ok := c.Telegram.ChatBindings[chatID]
	if !ok || bind.Repository == "" {
		return "", false
	}
	return bind.Repository, true
}

// IsChatAllowed returns true if the chat ID is in the allowed list.
// An empty list means all chats are allowed.
func (c *Config) IsChatAllowed(chatID int64) bool {
	if len(c.Telegram.AllowedChats) == 0 {
		return true
	}
	for _, id := range c.Telegram.AllowedChats {
		if id == chatID {
			return true
		}
	}
	return false
}

// IsUserAllowed returns true if the user is allowed to trigger the agent in the given chat.
// An empty AllowedUsers list means everyone is allowed.
func (c *Config) IsUserAllowed(chatID int64, username string) bool {
	if c.Telegram.ChatBindings == nil {
		return true
	}
	bind, ok := c.Telegram.ChatBindings[chatID]
	if !ok || len(bind.AllowedUsers) == 0 {
		return true
	}
	for _, u := range bind.AllowedUsers {
		if u == username {
			return true
		}
	}
	return false
}
