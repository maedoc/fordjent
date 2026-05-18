package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// TODO: Network filtering is not implemented in this phase.
// Future work: add --unshare-net or nftables-based egress filtering
// to prevent exfiltration of secrets via network during tool execution.

// Config configures the sandbox for a single tool execution.
type Config struct {
	Enabled               bool
	Backend               string // "auto", "bwrap", "sandbox-exec", "none"
	RepoDir               string
	AllowedReadDirs       []string
	AllowedWriteDirs      []string
	TmpfsSizeMB           int
	KeepProfilesOnFailure bool
	ViolationCommentThreshold int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig(repoDir string) Config {
	return Config{
		Enabled:               true,
		Backend:               "auto",
		RepoDir:               repoDir,
		TmpfsSizeMB:           64,
		KeepProfilesOnFailure: true,
		ViolationCommentThreshold: 3,
	}
}

// SandboxError provides enriched error information from sandboxed executions.
type SandboxError struct {
	Command       string
	ExitCode      int
	ProfilePath   string
	SandboxStderr string
	Violated      bool
	Backend       string // "bwrap" or "sandbox-exec"
}

func (e *SandboxError) Error() string {
	if e.Violated {
		return fmt.Sprintf("sandbox policy violation (backend=%s): command %q blocked, profile preserved at %s, stderr: %s",
			e.Backend, e.Command, e.ProfilePath, e.SandboxStderr)
	}
	return fmt.Sprintf("sandboxed command failed (backend=%s): %q exit %d, profile at %s",
		e.Backend, e.Command, e.ExitCode, e.ProfilePath)
}

// IsAvailable checks whether the bwrap binary exists on PATH.
func IsAvailable() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}

// IsSandboxExecAvailable checks whether the sandbox-exec binary exists on PATH.
func IsSandboxExecAvailable() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

// detectBackend resolves the "auto" backend setting to a concrete backend.
func detectBackend(cfg Config) string {
	switch cfg.Backend {
	case "bwrap":
		if IsAvailable() {
			return "bwrap"
		}
		slog.Warn("bwrap backend requested but not available")
		return "none"
	case "sandbox-exec":
		if IsSandboxExecAvailable() {
			return "sandbox-exec"
		}
		slog.Warn("sandbox-exec backend requested but not available")
		return "none"
	case "none":
		return "none"
	default: // "auto" or empty
		if IsAvailable() {
			return "bwrap"
		}
		if IsSandboxExecAvailable() {
			return "sandbox-exec"
		}
		return "none"
	}
}

// Run executes cmd inside a sandbox.
// Dispatch logic: bwrap → sandbox-exec → unsandboxed with warning.
func Run(ctx context.Context, cfg Config, cmd string, args ...string) ([]byte, error) {
	if !cfg.Enabled {
		return runUnsandboxed(ctx, cmd, args...)
	}

	backend := detectBackend(cfg)
	switch backend {
	case "bwrap":
		return runBwrap(ctx, cfg, cmd, args...)
	case "sandbox-exec":
		return runSandboxExec(ctx, cfg, cmd, args...)
	default:
		slog.Warn("no sandbox backend available, running unsandboxed", "cmd", cmd)
		return runUnsandboxed(ctx, cmd, args...)
	}
}

// RunShell executes a shell command string inside a sandbox.
// This is the primary entry point for the bash tool integration.
func RunShell(ctx context.Context, cfg Config, shellCmd string) ([]byte, error) {
	shell := "sh"
	if _, err := exec.LookPath("bash"); err == nil {
		shell = "bash"
	}
	return Run(ctx, cfg, shell, "-c", shellCmd)
}

func runUnsandboxed(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, cmd, args...)
	return c.CombinedOutput()
}

// runBwrap executes cmd inside a bubblewrap sandbox.
func runBwrap(ctx context.Context, cfg Config, cmd string, args ...string) ([]byte, error) {
	bwrapArgs := buildBwrapArgs(cfg, cmd, args...)
	c := exec.CommandContext(ctx, "bwrap", bwrapArgs...)
	c.Dir = cfg.RepoDir
	out, err := c.CombinedOutput()
	if err != nil {
		if isBwrapViolation(err, out) {
			return out, &SandboxError{
				Command:       cmd,
				ExitCode:      exitCode(err),
				ProfilePath:   "",
				SandboxStderr: string(out),
				Violated:      true,
				Backend:       "bwrap",
			}
		}
		return out, &SandboxError{
			Command:       cmd,
			ExitCode:      exitCode(err),
			ProfilePath:   "",
			SandboxStderr: string(out),
			Violated:      false,
			Backend:       "bwrap",
		}
	}
	return out, nil
}

// runSandboxExec executes cmd inside a macOS sandbox-exec sandbox.
func runSandboxExec(ctx context.Context, cfg Config, cmd string, args ...string) ([]byte, error) {
	profile := buildSandboxExecProfile(cfg.RepoDir, cfg.AllowedWriteDirs)

	tmpDir := os.TempDir()
	profileFile, err := os.CreateTemp(tmpDir, "fordjent-sandbox-*.sb")
	if err != nil {
		return nil, fmt.Errorf("sandbox-exec: failed to create profile file: %w", err)
	}
	profilePath := profileFile.Name()

	if _, err := profileFile.WriteString(profile); err != nil {
		profileFile.Close()
		os.Remove(profilePath)
		return nil, fmt.Errorf("sandbox-exec: failed to write profile: %w", err)
	}
	profileFile.Close()

	// Validate profile syntax before executing
	if err := validateSandboxExecProfile(profilePath); err != nil {
		os.Remove(profilePath)
		return nil, fmt.Errorf("sandbox-exec: %w", err)
	}

	execArgs := append([]string{"-f", profilePath, "--", cmd}, args...)
	c := exec.CommandContext(ctx, "sandbox-exec", execArgs...)
	c.Dir = cfg.RepoDir

	var stdout strings.Builder
	var stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr

	err = c.Run()
	stdoutStr := stdout.String()
	stderrStr := stderr.String()
	combined := []byte(stdoutStr)
	if stderrStr != "" {
		combined = append(combined, []byte("\n[sandbox-exec stderr]\n"+stderrStr)...)
	}

	if err != nil {
		violated := isSandboxExecViolation(stderrStr)
		keepProfile := cfg.KeepProfilesOnFailure
		if keepProfile {
			slog.Warn("sandbox-exec: preserving profile on failure", "profile", profilePath, "cmd", cmd)
		} else {
			os.Remove(profilePath)
			profilePath = ""
		}
		return combined, &SandboxError{
			Command:       cmd,
			ExitCode:      exitCode(err),
			ProfilePath:   profilePath,
			SandboxStderr: stderrStr,
			Violated:      violated,
			Backend:       "sandbox-exec",
		}
	}

	os.Remove(profilePath)
	return combined, nil
}

// buildSandboxExecProfile generates a macOS sandbox-exec profile (.sb format).
// Uses (deny default) as the base for tightest security, then explicitly
// allows only what's needed for command execution.
func buildSandboxExecProfile(repoDir string, allowedWriteDirs []string) string {
	absRepo, _ := filepath.Abs(repoDir)
	var sb strings.Builder

	sb.WriteString("(version 1)\n")
	sb.WriteString("(deny default)\n")
	sb.WriteString("(allow process-exec)\n")
	sb.WriteString("(allow process-fork)\n")
	sb.WriteString("(allow file-read*)\n")
	sb.WriteString("(allow file-write* (subpath \"")
	sb.WriteString(absRepo)
	sb.WriteString("\"))\n")
	sb.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	sb.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")

	for _, dir := range allowedWriteDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			absDir = dir
		}
		sb.WriteString("(allow file-write* (subpath \"")
		sb.WriteString(absDir)
		sb.WriteString("\"))\n")
	}

	sb.WriteString("(allow network-outbound)\n")
	sb.WriteString("(allow signal)\n")
	sb.WriteString("(allow mach-bootstrap)\n")
	sb.WriteString("(allow ipc-posix-sem)\n")
	sb.WriteString("(allow ipc-posix-shm)\n")
	sb.WriteString("(allow sysctl-read)\n")

	return sb.String()
}

// validateSandboxExecProfile validates a sandbox profile by running a
// trivial command with the profile. sandbox-exec has no dry-run/syntax-only
// flag, so we run "true" (exit 0) with the profile to check it's accepted.
func validateSandboxExecProfile(profilePath string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	cmd := exec.Command("sandbox-exec", "-f", profilePath, "--", "true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("invalid sandbox profile: %s: %w", string(out), err)
	}
	return nil
}

// isSandboxExecViolation checks if sandbox-exec stderr indicates a policy violation.
func isSandboxExecViolation(stderr string) bool {
	return strings.Contains(stderr, "deny") && (strings.Contains(stderr, "file-write") || strings.Contains(stderr, "network"))
}

// isBwrapViolation checks if bwrap output indicates a sandbox policy violation.
func isBwrapViolation(err error, out []byte) bool {
	if err == nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, "Failed to open") ||
		strings.Contains(s, "Permission denied") ||
		strings.Contains(s, "Read-only file system") ||
		strings.Contains(s, "Operation not permitted")
}

// exitCode extracts the exit code from an exec error, or returns 1 if unknown.
func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

// buildBwrapArgs constructs the bwrap argument list.
//
// The strategy is:
//  1. Make the entire root filesystem read-only (--ro-bind / /)
//  2. Overlay read-write mounts for specific directories that need writing
//  3. Mount private tmpfs for /tmp and /run
//  4. Mount /dev for device nodes
//  5. Add namespace/privilege restrictions
//  6. Execute the command
func buildBwrapArgs(cfg Config, cmd string, args ...string) []string {
	tmpfsSizeMB := cfg.TmpfsSizeMB
	if tmpfsSizeMB <= 0 {
		tmpfsSizeMB = 64
	}

	var bwrapArgs []string

	// Read-only root filesystem
	bwrapArgs = append(bwrapArgs, "--ro-bind", "/", "/")

	// Read-write bind for the repo directory
	repoDir := filepath.Clean(cfg.RepoDir)
	bwrapArgs = append(bwrapArgs, "--bind", repoDir, repoDir)

	// Additional read-only directories requested by config
	for _, dir := range cfg.AllowedReadDirs {
		dir = filepath.Clean(dir)
		if dirExists(dir) {
			bwrapArgs = append(bwrapArgs, "--ro-bind", dir, dir)
		}
	}

	// Additional writable directories
	for _, dir := range cfg.AllowedWriteDirs {
		dir = filepath.Clean(dir)
		if dirExists(dir) {
			bwrapArgs = append(bwrapArgs, "--bind", dir, dir)
		}
	}

	// Private tmpfs for /tmp and /run (discarded after execution)
	bwrapArgs = append(bwrapArgs, "--tmpfs", "/tmp")
	bwrapArgs = append(bwrapArgs, "--tmpfs", "/run")

	// /dev for basic device nodes
	if dirExists("/dev") {
		bwrapArgs = append(bwrapArgs, "--dev", "/dev")
	}

	// Proc filesystem
	bwrapArgs = append(bwrapArgs, "--proc", "/proc")

	// Namespace and privilege restrictions
	bwrapArgs = append(bwrapArgs,
		"--unshare-pid",
		"--die-with-parent",
		"--no-new-privileges",
		"--new-session",
	)

	// Working directory inside sandbox
	bwrapArgs = append(bwrapArgs, "--chdir", repoDir)

	// Command separator and the actual command
	fullCmd := append([]string{cmd}, args...)
	bwrapArgs = append(bwrapArgs, "--")
	bwrapArgs = append(bwrapArgs, fullCmd...)

	return bwrapArgs
}

// dirExists checks if a directory exists on the filesystem.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// ValidateConfig checks that the sandbox configuration is valid.
// Returns an error describing the first problem found.
func ValidateConfig(cfg Config) error {
	if cfg.RepoDir == "" {
		return fmt.Errorf("sandbox: RepoDir must not be empty")
	}
	if !dirExists(cfg.RepoDir) {
		return fmt.Errorf("sandbox: RepoDir does not exist: %s", cfg.RepoDir)
	}
	absRepo, err := filepath.Abs(cfg.RepoDir)
	if err != nil {
		return fmt.Errorf("sandbox: failed to resolve RepoDir: %w", err)
	}
	evalRepo, err := filepath.EvalSymlinks(absRepo)
	if err != nil {
		return fmt.Errorf("sandbox: failed to evaluate RepoDir symlinks: %w", err)
	}
	if strings.HasPrefix(evalRepo, "/etc/fordjent") {
		return fmt.Errorf("sandbox: RepoDir must not be under /etc/fordjent: %s", evalRepo)
	}
	return nil
}

// DetectGoCacheDirs auto-detects Go build cache directories that need
// write access in sandboxed environments.
func DetectGoCacheDirs() []string {
	var dirs []string
	if out, err := exec.Command("go", "env", "GOCACHE").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
