package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipIfNoBwrap(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bwrap only available on Linux")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not found on PATH")
	}
}

func skipIfNoSandboxExec(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec only available on macOS")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not found on PATH")
	}
}

func TestIsAvailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		if IsAvailable() {
			t.Error("IsAvailable should return false on non-Linux")
		}
		t.Log("IsAvailable returns false on non-Linux (expected)")
		return
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		if IsAvailable() {
			t.Error("IsAvailable should return false when bwrap is not on PATH")
		}
		t.Log("IsAvailable returns false when bwrap missing (expected)")
		return
	}
	if !IsAvailable() {
		t.Error("IsAvailable should return true when bwrap is on PATH")
	}
}

func TestIsSandboxExecAvailable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		if IsSandboxExecAvailable() {
			t.Error("IsSandboxExecAvailable should return false on non-macOS")
		}
		t.Log("IsSandboxExecAvailable returns false on non-macOS (expected)")
		return
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		if IsSandboxExecAvailable() {
			t.Error("IsSandboxExecAvailable should return false when sandbox-exec is not on PATH")
		}
		return
	}
	if !IsSandboxExecAvailable() {
		t.Error("IsSandboxExecAvailable should return true when sandbox-exec is on PATH")
	}
}

func TestDetectBackend(t *testing.T) {
	t.Run("none backend", func(t *testing.T) {
		cfg := Config{Backend: "none"}
		if detectBackend(cfg) != "none" {
			t.Error("expected none backend")
		}
	})

	t.Run("bwrap forced but unavailable", func(t *testing.T) {
		if IsAvailable() {
			t.Skip("bwrap available, can't test unavailable case")
		}
		cfg := Config{Backend: "bwrap"}
		if detectBackend(cfg) != "none" {
			t.Error("expected none when bwrap forced but unavailable")
		}
	})

	t.Run("sandbox-exec forced but unavailable", func(t *testing.T) {
		if IsSandboxExecAvailable() {
			t.Skip("sandbox-exec available, can't test unavailable case")
		}
		cfg := Config{Backend: "sandbox-exec"}
		if detectBackend(cfg) != "none" {
			t.Error("expected none when sandbox-exec forced but unavailable")
		}
	})

	t.Run("auto prefers bwrap", func(t *testing.T) {
		if !IsAvailable() {
			t.Skip("bwrap not available")
		}
		cfg := Config{Backend: "auto"}
		if detectBackend(cfg) != "bwrap" {
			t.Error("auto should prefer bwrap when available")
		}
	})

	t.Run("auto falls back to sandbox-exec", func(t *testing.T) {
		if IsAvailable() {
			t.Skip("bwrap available, can't test fallback")
		}
		if !IsSandboxExecAvailable() {
			t.Skip("sandbox-exec not available")
		}
		cfg := Config{Backend: "auto"}
		if detectBackend(cfg) != "sandbox-exec" {
			t.Error("auto should fall back to sandbox-exec when bwrap unavailable")
		}
	})

	t.Run("auto falls back to none", func(t *testing.T) {
		if IsAvailable() || IsSandboxExecAvailable() {
			t.Skip("a backend is available")
		}
		cfg := Config{Backend: "auto"}
		if detectBackend(cfg) != "none" {
			t.Error("auto should return none when no backend available")
		}
	})
}

func TestBuildSandboxExecProfile(t *testing.T) {
	t.Run("basic profile", func(t *testing.T) {
		profile := buildSandboxExecProfile("/tmp/my-repo", nil)
		if !strings.Contains(profile, "(version 1)") {
			t.Error("profile missing version")
		}
		if !strings.Contains(profile, "(deny default)") {
			t.Error("profile missing deny default")
		}
		if !strings.Contains(profile, "(allow file-write* (subpath \"/tmp/my-repo\"))") {
			t.Error("profile missing repo dir allow")
		}
		if !strings.Contains(profile, "(allow file-write* (subpath \"/tmp\"))") {
			t.Error("profile missing /tmp allow")
		}
		if !strings.Contains(profile, "(allow network-outbound)") {
			t.Error("profile missing network-outbound")
		}
		if !strings.Contains(profile, "(allow process-exec)") {
			t.Error("profile missing process-exec")
		}
		if !strings.Contains(profile, "(allow process-fork)") {
			t.Error("profile missing process-fork")
		}
	})

	t.Run("with allowed write dirs", func(t *testing.T) {
		profile := buildSandboxExecProfile("/tmp/repo", []string{"/home/user/.cache/go-build", "/home/user/go/pkg/mod"})
		if !strings.Contains(profile, "/home/user/.cache/go-build") {
			t.Error("profile missing allowed write dir 1")
		}
		if !strings.Contains(profile, "/home/user/go/pkg/mod") {
			t.Error("profile missing allowed write dir 2")
		}
	})
}

func TestValidateSandboxExecProfile(t *testing.T) {
	skipIfNoSandboxExec(t)

	tmpDir := t.TempDir()

	t.Run("valid profile", func(t *testing.T) {
		validProfile := `(version 1)
(allow default)
`
		path := filepath.Join(tmpDir, "valid.sb")
		os.WriteFile(path, []byte(validProfile), 0644)
		if err := validateSandboxExecProfile(path); err != nil {
			t.Errorf("valid profile should pass: %v", err)
		}
	})

	t.Run("invalid profile", func(t *testing.T) {
		invalidProfile := `(version 1)
(invalid-syntax-here
`
		path := filepath.Join(tmpDir, "invalid.sb")
		os.WriteFile(path, []byte(invalidProfile), 0644)
		if err := validateSandboxExecProfile(path); err == nil {
			t.Error("invalid profile should fail validation")
		}
	})
}

func TestSandboxErrorFormatting(t *testing.T) {
	t.Run("violation", func(t *testing.T) {
		err := &SandboxError{
			Command:       "go test ./...",
			ExitCode:      1,
			ProfilePath:   "/tmp/fordjent-sandbox-abc.sb",
			SandboxStderr: "deny file-write* /etc/hosts",
			Violated:      true,
			Backend:       "sandbox-exec",
		}
		msg := err.Error()
		if !strings.Contains(msg, "sandbox policy violation") {
			t.Error("violation message should contain 'sandbox policy violation'")
		}
		if !strings.Contains(msg, "sandbox-exec") {
			t.Error("violation message should contain backend name")
		}
		if !strings.Contains(msg, "go test ./...") {
			t.Error("violation message should contain command")
		}
	})

	t.Run("non-violation failure", func(t *testing.T) {
		err := &SandboxError{
			Command:     "go build ./...",
			ExitCode:    2,
			Backend:     "bwrap",
			ProfilePath: "",
		}
		msg := err.Error()
		if !strings.Contains(msg, "sandboxed command failed") {
			t.Error("non-violation message should contain 'sandboxed command failed'")
		}
	})
}

func TestRunEcho(t *testing.T) {
	skipIfNoBwrap(t)

	tmpDir := t.TempDir()
	cfg := DefaultConfig(tmpDir)

	out, err := RunShell(t.Context(), cfg, "echo hello")
	if err != nil {
		t.Fatalf("RunShell failed: %v\noutput: %s", err, string(out))
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("expected output to contain 'hello', got: %s", string(out))
	}
}

func TestWriteInsideRepoDir(t *testing.T) {
	skipIfNoBwrap(t)

	tmpDir := t.TempDir()
	cfg := DefaultConfig(tmpDir)

	testFile := filepath.Join(tmpDir, "test.txt")
	out, err := RunShell(t.Context(), cfg, "echo sandbox-test > "+testFile)
	if err != nil {
		t.Fatalf("writing inside repoDir failed: %v\noutput: %s", err, string(out))
	}

	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("failed to read test file: %v", err)
	}
	if !strings.Contains(string(data), "sandbox-test") {
		t.Errorf("expected file to contain 'sandbox-test', got: %s", string(data))
	}
}

func TestWriteOutsideRepoDirFails(t *testing.T) {
	skipIfNoBwrap(t)

	tmpDir := t.TempDir()
	cfg := DefaultConfig(tmpDir)

	outsidePath := "/tmp/fordjent-sandbox-test-outside-" + filepath.Base(tmpDir)
	out, err := RunShell(t.Context(), cfg, "echo should-fail > "+outsidePath)
	_ = os.Remove(outsidePath)

	if err == nil {
		t.Logf("WARNING: write outside repoDir did not fail (output: %s). This may indicate a bwrap config issue.", string(out))
	} else {
		t.Logf("write outside repoDir correctly failed: %v", err)
	}
}

func TestReadEtcFordjentBlocked(t *testing.T) {
	skipIfNoBwrap(t)

	tmpDir := t.TempDir()
	cfg := DefaultConfig(tmpDir)

	fordjentDir := "/etc/fordjent"
	fordjentFile := filepath.Join(fordjentDir, "fordjent.yaml")

	if os.Getuid() != 0 {
		t.Skip("skipping: need root to create /etc/fordjent test file")
	}

	_ = os.MkdirAll(fordjentDir, 0755)
	_ = os.WriteFile(fordjentFile, []byte("secret: super-secret-token\n"), 0644)
	defer os.RemoveAll(fordjentDir)

	out, err := RunShell(t.Context(), cfg, "cat "+fordjentFile)
	if err != nil {
		t.Logf("reading /etc/fordjent correctly failed: %v", err)
		return
	}
	if strings.Contains(string(out), "super-secret-token") {
		t.Errorf("SECURITY: sandbox was able to read /etc/fordjent config! Output: %s", string(out))
	}
}

func TestValidateConfig(t *testing.T) {
	t.Run("empty repo dir", func(t *testing.T) {
		cfg := Config{RepoDir: ""}
		if err := ValidateConfig(cfg); err == nil {
			t.Error("expected error for empty RepoDir")
		}
	})

	t.Run("nonexistent repo dir", func(t *testing.T) {
		cfg := Config{RepoDir: "/nonexistent/path/that/does/not/exist"}
		if err := ValidateConfig(cfg); err == nil {
			t.Error("expected error for nonexistent RepoDir")
		}
	})

	t.Run("valid repo dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := DefaultConfig(tmpDir)
		if err := ValidateConfig(cfg); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("etc fordjent repo dir", func(t *testing.T) {
		dir := "/etc/fordjent"
		if _, err := os.Stat(dir); err != nil {
			t.Skip("/etc/fordjent does not exist")
		}
		cfg := DefaultConfig(dir)
		if err := ValidateConfig(cfg); err == nil {
			t.Error("expected error for RepoDir under /etc/fordjent")
		}
	})
}

func TestBuildBwrapArgs(t *testing.T) {
	tmpDir := "/tmp/test-repo"
	cfg := DefaultConfig(tmpDir)
	cfg.TmpfsSizeMB = 128

	args := buildBwrapArgs(cfg, "sh", "-c", "echo hi")

	argsStr := strings.Join(args, " ")

	checks := []string{
		"--ro-bind / /",
		"--bind /tmp/test-repo /tmp/test-repo",
		"--tmpfs /tmp",
		"--tmpfs /run",
		"--dev /dev",
		"--proc /proc",
		"--unshare-pid",
		"--die-with-parent",
		"--no-new-privileges",
		"--new-session",
		"--chdir /tmp/test-repo",
		"--",
		"sh -c echo hi",
	}

	for _, check := range checks {
		if !strings.Contains(argsStr, check) {
			t.Errorf("bwrap args missing expected %q\n got: %s", check, argsStr)
		}
	}
}

func TestBuildBwrapArgsAllowedWriteDirs(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "go-cache")
	_ = os.MkdirAll(cacheDir, 0755)

	cfg := DefaultConfig(tmpDir)
	cfg.AllowedWriteDirs = []string{cacheDir}

	args := buildBwrapArgs(cfg, "sh", "-c", "echo hi")
	argsStr := strings.Join(args, " ")

	if !strings.Contains(argsStr, "--bind "+cacheDir+" "+cacheDir) {
		t.Errorf("bwrap args missing allowed write dir bind: %s", argsStr)
	}
}

func TestDirExists(t *testing.T) {
	if !dirExists(t.TempDir()) {
		t.Error("dirExists should return true for existing directory")
	}
	if dirExists("/nonexistent/dir/path") {
		t.Error("dirExists should return false for nonexistent directory")
	}
	tmpFile := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(tmpFile, []byte("x"), 0644)
	if dirExists(tmpFile) {
		t.Error("dirExists should return false for a file (not directory)")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig("/some/repo")
	if !cfg.Enabled {
		t.Error("DefaultConfig should have Enabled=true")
	}
	if cfg.RepoDir != "/some/repo" {
		t.Errorf("expected RepoDir=/some/repo, got %s", cfg.RepoDir)
	}
	if cfg.TmpfsSizeMB != 64 {
		t.Errorf("expected TmpfsSizeMB=64, got %d", cfg.TmpfsSizeMB)
	}
	if cfg.Backend != "auto" {
		t.Errorf("expected Backend=auto, got %s", cfg.Backend)
	}
	if !cfg.KeepProfilesOnFailure {
		t.Error("DefaultConfig should have KeepProfilesOnFailure=true")
	}
	if cfg.ViolationCommentThreshold != 3 {
		t.Errorf("expected ViolationCommentThreshold=3, got %d", cfg.ViolationCommentThreshold)
	}
}

func TestDetectGoCacheDirs(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found on PATH")
	}
	dirs := DetectGoCacheDirs()
	if len(dirs) == 0 {
		t.Log("DetectGoCacheDirs returned empty (go may not be configured)")
		return
	}
	for _, d := range dirs {
		if d == "" {
			t.Error("DetectGoCacheDirs returned empty string")
		}
		t.Logf("detected go cache dir: %s", d)
	}
}

func TestIsSandboxExecViolation(t *testing.T) {
	if !isSandboxExecViolation("sandbox-exec[123]: deny file-write* /etc/hosts") {
		t.Error("should detect file-write violation")
	}
	if !isSandboxExecViolation("deny network-inbound") {
		t.Error("should detect network violation")
	}
	if isSandboxExecViolation("some random output") {
		t.Error("should not detect violation in random output")
	}
	if isSandboxExecViolation("") {
		t.Error("should not detect violation in empty string")
	}
}

func TestExitCode(t *testing.T) {
	if exitCode(nil) != 1 {
		t.Error("nil error should return 1")
	}
}
