package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
server:
  host: "0.0.0.0"
  port: 9090
webhook:
  secret: "test-secret"
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
    max_tokens: 4096
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Forgejo.URL != "https://forgejo.example.com" {
		t.Errorf("unexpected forgejo URL: %s", cfg.Forgejo.URL)
	}
	if len(cfg.Providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(cfg.Providers))
	}
	if cfg.DefaultProvider().Name != "openai" {
		t.Errorf("expected openai, got %s", cfg.DefaultProvider().Name)
	}
}

func TestLoadMissingForgejoURL(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
forgejo:
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
    max_tokens: 4096
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Error("expected error for missing forgejo URL")
	}
}

func TestLoadMissingProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Error("expected error for missing providers")
	}
}

func TestDefaultValues(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
webhook:
  secret: "test-secret"
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
    max_tokens: 4096
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	// Check defaults
	if cfg.Agent.MaxSessions != 10 {
		t.Errorf("expected default max_sessions 10, got %d", cfg.Agent.MaxSessions)
	}
	if cfg.Agent.MaxTurns != 25 {
		t.Errorf("expected default max_turns 25, got %d", cfg.Agent.MaxTurns)
	}
	if cfg.Agent.CommitPrefix != "[agent-automation]" {
		t.Errorf("unexpected commit prefix: %s", cfg.Agent.CommitPrefix)
	}
	if cfg.Memory.CompactionPath != "docs/issues" {
		t.Errorf("unexpected compaction path: %s", cfg.Memory.CompactionPath)
	}
}

func TestEnvExpansion(t *testing.T) {
	os.Setenv("TEST_FORGEJO_TOKEN", "env-token-123")
	os.Setenv("TEST_OPENAI_KEY", "env-key-456")
	defer os.Unsetenv("TEST_FORGEJO_TOKEN")
	defer os.Unsetenv("TEST_OPENAI_KEY")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
webhook:
  secret: "test-secret"
forgejo:
  url: "https://forgejo.example.com"
  token: "${TEST_FORGEJO_TOKEN}"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "${TEST_OPENAI_KEY}"
    model: "gpt-4o"
    max_tokens: 4096
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Forgejo.Token != "env-token-123" {
		t.Errorf("expected env-expanded token, got %s", cfg.Forgejo.Token)
	}
	if cfg.Providers[0].APIKey != "env-key-456" {
		t.Errorf("expected env-expanded key, got %s", cfg.Providers[0].APIKey)
	}
}

func TestLoad_ExpandsAllEnvVars(t *testing.T) {
	os.Setenv("TEST_SERVER_HOST", "1.2.3.4")
	os.Setenv("TEST_WEBHOOK_SECRET", "wh-secret")
	os.Setenv("TEST_FORGEJO_URL", "https://forgejo.env.example.com")
	os.Setenv("TEST_AGENT_PREFIX", "[env-prefix]")
	os.Setenv("TEST_LOG_LEVEL", "debug")
	defer func() {
		os.Unsetenv("TEST_SERVER_HOST")
		os.Unsetenv("TEST_WEBHOOK_SECRET")
		os.Unsetenv("TEST_FORGEJO_URL")
		os.Unsetenv("TEST_AGENT_PREFIX")
		os.Unsetenv("TEST_LOG_LEVEL")
	}()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
server:
  host: "${TEST_SERVER_HOST}"
  port: 9090
webhook:
  secret: "${TEST_WEBHOOK_SECRET}"
forgejo:
  url: "${TEST_FORGEJO_URL}"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
    max_tokens: 4096
agent:
  commit_prefix: "${TEST_AGENT_PREFIX}"
log_level: "${TEST_LOG_LEVEL}"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	if cfg.Server.Host != "1.2.3.4" {
		t.Errorf("expected server host '1.2.3.4', got %s", cfg.Server.Host)
	}
	if cfg.Webhook.Secret != "wh-secret" {
		t.Errorf("expected webhook secret 'wh-secret', got %s", cfg.Webhook.Secret)
	}
	if cfg.Forgejo.URL != "https://forgejo.env.example.com" {
		t.Errorf("expected forgejo url 'https://forgejo.env.example.com', got %s", cfg.Forgejo.URL)
	}
	if cfg.Agent.CommitPrefix != "[env-prefix]" {
		t.Errorf("expected commit prefix '[env-prefix]', got %s", cfg.Agent.CommitPrefix)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log level 'debug', got %s", cfg.LogLevel)
	}
}

func TestLoad_ValidatesWebhookSecret(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	cases := []struct {
		name   string
		secret string
	}{
		{"empty", ""},
		{"default", "change-me-in-production"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := fmt.Sprintf(`
webhook:
  secret: %q
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
    max_tokens: 4096
`, tc.secret)
			if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
				t.Fatalf("failed to write config: %v", err)
			}
			_, err := Load(cfgPath)
			if err == nil {
				t.Errorf("expected error for webhook secret %q", tc.secret)
			}
		})
	}
}
