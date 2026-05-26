// Package eval provides end-to-end benchmark infrastructure for Fordjent.
// It starts Forgejo and Fordjent locally, creates benchmark repos, issues
// test tasks, and verifies outcomes against known-correct solutions.
package eval

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fordjent/fordjent/internal/forgejo"
)

// Harness manages local Forgejo + Fordjent instances for benchmark runs.
type Harness struct {
	t            *testing.T
	ForgejoURL   string
	FordjentURL  string
	ForgejoToken string
	AdminToken   string
	AdminUser    string
	AdminPass    string
	WebhookSecret string
	LocalDir     string
	ForgejoPID   int
	FordjentPID  int
	Client       *forgejo.Client
	skipSetup    bool
	skipTearDown bool
	forgejoBin   string
}

// HarnessConfig holds configuration for creating a new Harness.
type HarnessConfig struct {
	ForgejoPort    int
	FordjentPort   int
	WaferAPIKey    string
	ScalewayAPIKey string
	SkipSetup      bool
	SkipTearDown   bool
}

// DefaultHarnessConfig returns a config with sensible defaults,
// reading from environment variables.
func DefaultHarnessConfig() HarnessConfig {
	return HarnessConfig{
		ForgejoPort:    getEnvInt("EVAL_FORGEJO_PORT", 3000),
		FordjentPort:   getEnvInt("EVAL_FORDJENT_PORT", 8080),
		WaferAPIKey:    os.Getenv("EVAL_WAFER_API_KEY"),
		ScalewayAPIKey: os.Getenv("EVAL_SCALEWAY_API_KEY"),
		SkipSetup:      os.Getenv("EVAL_SKIP_SETUP") == "true",
		SkipTearDown:   os.Getenv("EVAL_SKIP_TEARDOWN") == "true",
	}
}

// NewHarness creates a new Harness, starts services, and sets up the
// Forgejo instance with admin user and tokens.
func NewHarness(t *testing.T) *Harness {
	return NewHarnessWithConfig(t, DefaultHarnessConfig())
}

// NewHarnessWithConfig creates a Harness with the given configuration.
func NewHarnessWithConfig(t *testing.T, cfg HarnessConfig) *Harness {
	t.Helper()

	h := &Harness{
		t:             t,
		ForgejoURL:    fmt.Sprintf("http://127.0.0.1:%d", cfg.ForgejoPort),
		FordjentURL:   fmt.Sprintf("http://127.0.0.1:%d", cfg.FordjentPort),
		AdminUser:     "fjadmin",
		AdminPass:     randomHex(12),
		WebhookSecret: randomHex(16),
		skipSetup:     cfg.SkipSetup,
		skipTearDown:  cfg.SkipTearDown,
	}

	if h.skipSetup {
		t.Log("EVAL_SKIP_SETUP=true: connecting to existing services")
		// Use existing tokens from environment
		h.ForgejoToken = os.Getenv("FORGEJO_TOKEN")
		h.AdminToken = os.Getenv("FORGEJO_ADMIN_TOKEN")
		if h.ForgejoToken == "" || h.AdminToken == "" {
			t.Fatal("EVAL_SKIP_SETUP requires FORGEJO_TOKEN and FORGEJO_ADMIN_TOKEN environment variables")
		}
		h.LocalDir = os.Getenv("FORDJENT_LOCAL_DIR")
		if h.LocalDir == "" {
			h.LocalDir = filepath.Join(os.TempDir(), "fordjent-local")
		}
	} else {
		// Create random workdir
		h.LocalDir = filepath.Join(os.TempDir(), "fordjent-eval-"+randomHex(6))
		if err := os.MkdirAll(h.LocalDir, 0755); err != nil {
			t.Fatalf("failed to create workdir: %v", err)
		}
		t.Logf("workdir: %s", h.LocalDir)

		// Find Forgejo binary
		h.forgejoBin = findForgejo(t)

		// Start Forgejo
		h.startForgejo(cfg)
		h.createAdmin(cfg)
		h.startFordjent(cfg)
	}

	// Create Forgejo client
	h.Client = forgejo.NewClient(h.ForgejoURL, h.ForgejoToken)

	return h
}

// TearDown stops services and cleans up the workdir.
func (h *Harness) TearDown() {
	if h.skipTearDown {
		h.t.Logf("EVAL_SKIP_TEARDOWN=true: preserving workdir %s", h.LocalDir)
		h.t.Logf("  Forgejo PID: %d", h.ForgejoPID)
		h.t.Logf("  Fordjent PID: %d", h.FordjentPID)
		return
	}

	if h.skipSetup {
		return // Don't touch services we didn't start
	}

	// Kill Fordjent
	if h.FordjentPID > 0 {
		if err := killProcess(h.FordjentPID); err != nil {
			h.t.Logf("failed to kill fordjent (pid %d): %v", h.FordjentPID, err)
		}
	}

	// Kill Forgejo
	if h.ForgejoPID > 0 {
		if err := killProcess(h.ForgejoPID); err != nil {
			h.t.Logf("failed to kill forgejo (pid %d): %v", h.ForgejoPID, err)
		}
	}

	// Remove workdir
	if h.LocalDir != "" && !h.skipSetup {
		if err := os.RemoveAll(h.LocalDir); err != nil {
			h.t.Logf("failed to remove workdir: %v", err)
		}
	}
}

func (h *Harness) startForgejo(cfg HarnessConfig) {
	t := h.t

	// Generate app.ini
	appIni := filepath.Join(h.LocalDir, "app.ini")
	h.writeForgejoConfig(appIni, cfg)

	// Start Forgejo
	args := []string{
		"-f", filepath.Join(h.LocalDir, "forgejo.sb"),
		h.forgejoBin, "web",
		"--work-path", filepath.Join(h.LocalDir, "forgejo-data"),
		"--config", appIni,
	}

	cmd := exec.Command("sandbox-exec", args...)
	cmd.Dir = h.LocalDir
	logPath := filepath.Join(h.LocalDir, "logs", "forgejo-stdout.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatalf("failed to create log dir: %v", err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("failed to create log file: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start forgejo: %v", err)
	}
	h.ForgejoPID = cmd.Process.Pid
	t.Logf("Forgejo started (pid %d)", h.ForgejoPID)

	// Wait for Forgejo to be ready
	if err := h.waitForURL(h.ForgejoURL+"/api/v1/version", 30*time.Second); err != nil {
		t.Fatalf("Forgejo did not become ready: %v", err)
	}
	t.Log("Forgejo is ready")
}

func (h *Harness) createAdmin(cfg HarnessConfig) {
	t := h.t

	// First, run database migration explicitly
	migrateCmd := exec.Command(h.forgejoBin,
		"migrate",
		"--work-path", filepath.Join(h.LocalDir, "forgejo-data"),
		"--config", filepath.Join(h.LocalDir, "app.ini"),
	)
	if out, err := migrateCmd.CombinedOutput(); err != nil {
		t.Logf("forgejo migrate warning: %v\n%s", err, string(out))
		// Non-fatal — Forgejo may have already migrated
	}

	// Stop Forgejo briefly to create admin user (SQLite lock)
	if h.ForgejoPID > 0 {
		if err := killProcess(h.ForgejoPID); err != nil {
			t.Logf("warning: failed to stop forgejo for admin creation: %v", err)
		}
		time.Sleep(2 * time.Second)
	}

	// Create admin user + generate tokens using Forgejo CLI
	tokenCmd := exec.Command(h.forgejoBin,
		"admin", "user", "create",
		"--work-path", filepath.Join(h.LocalDir, "forgejo-data"),
		"--config", filepath.Join(h.LocalDir, "app.ini"),
		"--username", h.AdminUser,
		"--password", h.AdminPass,
		"--email", "admin@eval.local",
		"--admin",
		"--must-change-password=false",
		"--access-token",
		"--access-token-name", "eval-bot",
		"--access-token-scopes", "all",
	)
	tokenOut, err := tokenCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to create admin user: %v\noutput: %s", err, tokenOut)
	}
	h.ForgejoToken = extractToken(string(tokenOut))

	// Generate admin token
	adminCmd := exec.Command(h.forgejoBin,
		"admin", "user", "generate-access-token",
		"--work-path", filepath.Join(h.LocalDir, "forgejo-data"),
		"--config", filepath.Join(h.LocalDir, "app.ini"),
		"--username", h.AdminUser,
		"--token-name", "eval-admin",
		"--scopes", "all",
		"--raw",
	)
	adminOut, err := adminCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to generate admin token: %v\noutput: %s", err, adminOut)
	}
	h.AdminToken = strings.TrimSpace(string(adminOut))

	// Restart Forgejo
	h.startForgejoAfterRestart(cfg)
}

func (h *Harness) startForgejoAfterRestart(cfg HarnessConfig) {
	t := h.t
	appIni := filepath.Join(h.LocalDir, "app.ini")

	args := []string{
		"-f", filepath.Join(h.LocalDir, "forgejo.sb"),
		h.forgejoBin, "web",
		"--work-path", filepath.Join(h.LocalDir, "forgejo-data"),
		"--config", appIni,
	}

	cmd := exec.Command("sandbox-exec", args...)
	cmd.Dir = h.LocalDir
	logPath := filepath.Join(h.LocalDir, "logs", "forgejo-stdout.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to restart forgejo: %v", err)
	}
	h.ForgejoPID = cmd.Process.Pid

	if err := h.waitForURL(h.ForgejoURL+"/api/v1/version", 30*time.Second); err != nil {
		t.Fatalf("Forgejo did not restart: %v", err)
	}
	t.Logf("Forgejo restarted (pid %d)", h.ForgejoPID)
}

func (h *Harness) startFordjent(cfg HarnessConfig) {
	t := h.t

	// Write Fordjent config
	configPath := filepath.Join(h.LocalDir, "fordjent.yaml")
	h.writeFordjentConfig(configPath, cfg)

	// Build Fordjent binary
	projectDir := filepath.Join(filepath.Dir(h.LocalDir), "src", "fordjent")
	// Try to find project directory
	cwd, _ := os.Getwd()
	for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "cmd", "fordjent", "main.go")); err == nil {
			projectDir = dir
			break
		}
	}

	binPath := filepath.Join(h.LocalDir, "fordjent-bin")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/fordjent")
	buildCmd.Dir = projectDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build fordjent: %v\noutput: %s", err, out)
	}

	// Write sandbox profile
	h.writeFordjentSandboxProfile(filepath.Join(h.LocalDir, "fordjent.sb"))

	// Start Fordjent
	args := []string{
		"-f", filepath.Join(h.LocalDir, "fordjent.sb"),
		binPath,
		"-config", configPath,
	}

	cmd := exec.Command("sandbox-exec", args...)
	cmd.Dir = h.LocalDir
	logPath := filepath.Join(h.LocalDir, "logs", "fordjent-stdout.log")
	fordjentLog, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to create fordjent log: %v", err)
	}
	cmd.Stdout = fordjentLog
	cmd.Stderr = fordjentLog

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start fordjent: %v", err)
	}
	h.FordjentPID = cmd.Process.Pid
	t.Logf("Fordjent started (pid %d)", h.FordjentPID)

	// Wait for Fordjent to be healthy
	if err := h.waitForURL(h.FordjentURL+"/healthz", 30*time.Second); err != nil {
		t.Fatalf("Fordjent did not become healthy: %v", err)
	}
	t.Log("Fordjent is healthy")
}

// writeForgejoConfig generates the app.ini for the Forgejo instance.
func (h *Harness) writeForgejoConfig(path string, cfg HarnessConfig) {
	content := fmt.Sprintf(`APP_NAME = Forgejo Eval
RUN_MODE = prod

[server]
DOMAIN = localhost
ROOT_URL = http://localhost:%d/
HTTP_PORT = %d

[database]
DB_TYPE = sqlite3
PATH = %s/forgejo-data/forgejo.db

[service]
DISABLE_REGISTRATION = true

[webhook]
ALLOWED_HOST_LIST = *

[repository]
DEFAULT_PRIVATE = public
DEFAULT_BRANCH = main

[security]
INSTALL_LOCK = true
INTERNAL_TOKEN = eval-internal-token-not-for-production
SECRET_KEY = eval-secret-key-not-for-production

[log]
MODE = file
LEVEL = Info
ROOT_PATH = %s/logs

[log.file]
FILE_NAME = forgejo.log

[mailer]
ENABLED = false

[session]
PROVIDER = memory

[cache]
ADAPTER = memory

[packages]
ENABLED = false

[actions]
ENABLED = false
`, cfg.ForgejoPort, cfg.ForgejoPort, h.LocalDir, h.LocalDir)

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		h.t.Fatalf("failed to write forgejo config: %v", err)
	}
}

// writeFordjentConfig generates the fordjent.yaml for the eval instance.
func (h *Harness) writeFordjentConfig(path string, cfg HarnessConfig) {
	apiKey := cfg.WaferAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("WAFER_API_KEY")
	}
	if apiKey == "" {
		apiKey = "placeholder"
	}

	content := fmt.Sprintf(`server:
  host: "0.0.0.0"
  port: %d

webhook:
  secret: "%s"

forgejo:
  url: "%s"
  token: "%s"
  admin_token: "%s"
  rate_limit: 60

agent:
  max_sessions: 25
  idle_timeout: "4h"
  workdir: "%s/fordjent-work"
  max_turns: 75
  max_turns_pm: 15
  max_turns_implementer: 50
  role_providers:
    pm: "wafer-qwen"
    reviewer: "wafer-glm"
    implementer: "wafer-qwen"
    tester: "wafer-qwen"
    devops: "wafer-qwen"
  fallback_provider: "wafer-qwen"
  commit_prefix: "[eval]"
  context_window: 131072
  compaction_threshold: 0.85
  compaction_keep_turns: 8
  enable_lifecycle: true
  enable_stale_gate: true
  enable_scaffold_detection: true
  require_role_tag: true
  session_timeout: "30m"
  git_name: "Eval Agent"
  git_email: "eval@fordjent.local"

sandbox:
  enabled: false

providers:
  - name: "wafer-qwen"
    api_base: "https://pass.wafer.ai/v1"
    api_key: "%s"
    model: "Qwen3.5-397B-A17B"
    max_tokens: 32768
    request_timeout: "90s"
    max_retries: 5
    retry_base_delay: "3s"
    retry_max_delay: "60s"
  - name: "wafer-glm"
    api_base: "https://pass.wafer.ai/v1"
    api_key: "%s"
    model: "GLM-5.1"
    max_tokens: 32768
    request_timeout: "120s"
    max_retries: 5

events:
  - "issues"
  - "issue_comment"
  - "pull_request"
  - "pull_request_review_comment"

session_key_template: "{{.Repository}}/issues/{{.IssueNumber}}"

security:
  protected_branches: ["main", "master"]
  require_pr_for_workflows: true
  filter_agent_events: true

memory:
  enabled: true

database:
  path: ""

log_level: "info"
`, cfg.FordjentPort, h.WebhookSecret, h.ForgejoURL, h.ForgejoToken, h.AdminToken, h.LocalDir, apiKey, apiKey)

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		h.t.Fatalf("failed to write fordjent config: %v", err)
	}
}

// writeFordjentSandboxProfile writes a minimal sandbox-exec profile for Fordjent.
func (h *Harness) writeFordjentSandboxProfile(path string) {
	content := fmt.Sprintf(`(version 1)
(allow default)
(allow network-inbound (local tcp))
(allow network-bind (local tcp))
(allow network-outbound)
(allow process-exec (literal "/usr/bin/git"))
(allow process-exec (literal "/usr/local/bin/git"))
(allow process-exec (subpath "/opt/homebrew/bin"))
(allow file-read* (subpath "/opt/homebrew"))
(allow file-read* (subpath "/usr"))
(allow file-read* (subpath "/System"))
(allow file-read* (subpath "/Library"))
(allow file-read* (subpath "/tmp"))
(allow file-read* (literal "/etc"))
(allow file-read* (subpath "%s/src"))
(allow file-write* (subpath "%s"))
`, h.LocalDir, h.LocalDir)

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		h.t.Fatalf("failed to write fordjent sandbox profile: %v", err)
	}
}

// waitForURL polls a URL until it returns 200 or timeout.
func (h *Harness) waitForURL(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", url)
}

// FetchStatus retrieves the Fordjent /status endpoint.
func (h *Harness) FetchStatus() (*StatusResponse, error) {
	resp, err := http.Get(h.FordjentURL + "/status")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch status: %w", err)
	}
	defer resp.Body.Close()

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode status: %w", err)
	}
	return &status, nil
}

// waitForIdle waits for Fordjent to have no active sessions.
func (h *Harness) waitForIdle(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := h.FetchStatus()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		active, _ := status.Lifecycle["active_sessions"].(float64)
		if active == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for Fordjent to become idle")
}

// Helper functions

func findForgejo(t *testing.T) string {
	t.Helper()
	// Check common locations
	locations := []string{
		"/opt/homebrew/opt/forgejo/bin/forgejo",
		"/opt/homebrew/bin/forgejo",
		"/usr/local/bin/forgejo",
	}
	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			return loc
		}
	}
	// Try PATH
	path, err := exec.LookPath("forgejo")
	if err == nil {
		return path
	}
	t.Fatal("forgejo binary not found. Install with: brew install forgejo")
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func extractToken(output string) string {
	// Extract a 40-char hex token from CLI output
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) == 40 && isHex(line) {
			return line
		}
	}
	// Fallback: look for token in the output
	parts := strings.Fields(output)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) == 40 && isHex(part) {
			return part
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func killProcess(pid int) error {
	// Try SIGTERM first, then SIGKILL
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(os.Interrupt); err != nil {
		_ = process.Kill()
	}
	time.Sleep(1 * time.Second)
	// Check if still running
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return nil // already dead
	}
	_ = process.Kill()
	return nil
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &defaultVal); err != nil || n != 1 {
			return defaultVal
		}
	}
	return defaultVal
}

func (h *Harness) doForgejoRequest(method, path string, body interface{}) (string, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = strings.NewReader(string(data))
	}

	req, err := http.NewRequest(method, h.ForgejoURL+"/api/v1"+path, bodyReader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+h.AdminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(respBody), nil
}

// WaitForCondition polls until a condition is met or timeout.
func (h *Harness) WaitForCondition(condition func() bool, interval, timeout time.Duration, msg string) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("timeout: %s", msg)
}