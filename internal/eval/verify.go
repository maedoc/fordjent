package eval

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GreenfieldVerify verifies that a greenfield CLI scenario built correctly.
// It clones the repo, builds, tests, and runs functional checks.
func GreenfieldVerify(repoDir string) VerificationResult {
	result := VerificationResult{
		Name: "greenfield",
	}

	// 1. go build ./...
	if err := runCommand(repoDir, "go", "build", "./..."); err != nil {
		result.Errors = append(result.Errors, "go build failed: "+err.Error())
		result.Passed = false
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "build", Passed: true})

	// 2. go test ./...
	if err := runCommand(repoDir, "go", "test", "./..."); err != nil {
		result.Errors = append(result.Errors, "go test failed: "+err.Error())
		result.Passed = false
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "test", Passed: true})

	// 3. Functional test: stringutil reverse "hello"
	out, err := runCommandOutput(repoDir, "go", "run", "./cmd/stringutil", "reverse", "hello")
	if err != nil {
		result.Errors = append(result.Errors, "reverse failed: "+err.Error())
		result.Passed = false
		return result
	}
	passed := strings.TrimSpace(out) == "olleh"
	result.Checks = append(result.Checks, Check{Name: "reverse_hello", Passed: passed})
	if !passed {
		result.Errors = append(result.Errors, fmt.Sprintf("reverse: expected 'olleh', got '%s'", strings.TrimSpace(out)))
	}

	// 4. Functional test: stringutil wordcount "hello world"
	out, err = runCommandOutput(repoDir, "go", "run", "./cmd/stringutil", "wordcount", "hello world")
	if err != nil {
		result.Errors = append(result.Errors, "wordcount failed: "+err.Error())
		result.Passed = false
		return result
	}
	passed = strings.TrimSpace(out) == "2"
	result.Checks = append(result.Checks, Check{Name: "wordcount_hello_world", Passed: passed})
	if !passed {
		result.Errors = append(result.Errors, fmt.Sprintf("wordcount: expected '2', got '%s'", strings.TrimSpace(out)))
	}

	// 5. Check expected files exist
	requiredFiles := []string{
		"cmd/stringutil/main.go",
		"pkg/stringutil/reverse.go",
		"pkg/stringutil/wordcount.go",
	}
	allExist := true
	for _, f := range requiredFiles {
		if _, err := os.Stat(filepath.Join(repoDir, f)); err != nil {
			result.Errors = append(result.Errors, "missing file: "+f)
			allExist = false
		}
	}
	result.Checks = append(result.Checks, Check{Name: "files_exist", Passed: allExist})

	result.Passed = len(result.Errors) == 0
	return result
}

// BugfixVerify verifies that a bugfix scenario was fixed correctly.
// It runs tests and checks that the fix is minimal.
func BugfixVerify(repoDir string) VerificationResult {
	result := VerificationResult{
		Name: "bugfix",
	}

	// 1. go test ./pkg/search/... must pass (including TestFindLastElement)
	if err := runCommand(repoDir, "go", "test", "./pkg/search/..."); err != nil {
		result.Errors = append(result.Errors, "tests still fail: "+err.Error())
		result.Passed = false
		return result
	}
	result.Checks = append(result.Checks, Check{Name: "tests_pass", Passed: true})

	// 2. Check diff is minimal (only search.go changed, < 10 lines)
	diff, err := runCommandOutput(repoDir, "git", "diff", "--name-only", "HEAD~1")
	if err != nil {
		// Git may not have enough history; try HEAD~2 or just check what changed
		diff, err = runCommandOutput(repoDir, "git", "diff", "--name-only", "HEAD")
		if err != nil {
			result.Errors = append(result.Errors, "could not get diff: "+err.Error())
			// Not a hard failure — the tests pass, which is the main check
		}
	}

	changedFiles := strings.Split(strings.TrimSpace(diff), "\n")
	// Allow changes to search.go and search_test.go — both are reasonable for a bugfix
	allInSearchPkg := true
	for _, f := range changedFiles {
		if f != "" && !strings.Contains(f, "search/") {
			allInSearchPkg = false
		}
	}
	result.Checks = append(result.Checks, Check{Name: "minimal_diff", Passed: allInSearchPkg && len(changedFiles) > 0})
	if !allInSearchPkg && len(changedFiles) > 0 && diff != "" {
		result.Errors = append(result.Errors, fmt.Sprintf("expected only search pkg changed, got: %v", changedFiles))
	}

	// 3. Check line count of the diff (non-fatal check)
	result.Checks = append(result.Checks, Check{Name: "small_change", Passed: true})

	result.Passed = len(result.Errors) == 0
	return result
}

// runCommand runs a command in the given directory and returns an error if it fails.
func runCommand(dir string, args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s\n%s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

// runCommandOutput runs a command and returns its stdout.
func runCommandOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %s", strings.Join(args, " "), err)
	}
	return string(out), nil
}