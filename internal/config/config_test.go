package config

import (
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
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
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

func TestTelegramConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Telegram.Enabled {
		t.Error("expected telegram to be disabled by default")
	}
}

func TestRepositoryForChat(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
telegram:
  enabled: true
  token: "test-token"
  chat_bindings:
    -1001234567890:
      repository: "myorg/myrepo"
      allowed_users: ["admin"]
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	repo, ok := cfg.RepositoryForChat(-1001234567890)
	if !ok {
		t.Error("expected binding to exist")
	}
	if repo != "myorg/myrepo" {
		t.Errorf("expected 'myorg/myrepo', got '%s'", repo)
	}

	_, ok = cfg.RepositoryForChat(-100999)
	if ok {
		t.Error("expected no binding for unknown chat")
	}
}

func TestIsChatAllowed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test.yaml")

	content := `
forgejo:
  url: "https://forgejo.example.com"
  token: "test-token"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "test-key"
    model: "gpt-4o"
telegram:
  enabled: true
  token: "test-token"
  allowed_chats: [-1001234567890]
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if !cfg.IsChatAllowed(-1001234567890) {
		t.Error("expected chat to be allowed")
	}
	if cfg.IsChatAllowed(-100999) {
		t.Error("expected chat to be denied")
	}
}

func TestIsChatAllowedEmpty(t *testing.T) {
	cfg := &Config{Telegram: TelegramConfig{AllowedChats: nil}}
	if !cfg.IsChatAllowed(12345) {
		t.Error("empty allowlist should allow all")
	}
}

func TestIsUserAllowed(t *testing.T) {
	cfg := &Config{
		Telegram: TelegramConfig{
			ChatBindings: map[int64]TelegramChatBind{
				-100: {Repository: "org/repo", AllowedUsers: []string{"admin", "dev"}},
			},
		},
	}

	if !cfg.IsUserAllowed(-100, "admin") {
		t.Error("expected admin to be allowed")
	}
	if !cfg.IsUserAllowed(-100, "dev") {
		t.Error("expected dev to be allowed")
	}
	if cfg.IsUserAllowed(-100, "random") {
		t.Error("expected random to be denied")
	}
}

func TestIsUserAllowedEmpty(t *testing.T) {
	cfg := &Config{
		Telegram: TelegramConfig{
			ChatBindings: map[int64]TelegramChatBind{
				-100: {Repository: "org/repo", AllowedUsers: nil},
			},
		},
	}

	if !cfg.IsUserAllowed(-100, "anyone") {
		t.Error("empty allowlist should allow all")
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
forgejo:
  url: "https://forgejo.example.com"
  token: "${TEST_FORGEJO_TOKEN}"
providers:
  - name: "openai"
    api_base: "https://api.openai.com/v1"
    api_key: "${TEST_OPENAI_KEY}"
    model: "gpt-4o"
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
