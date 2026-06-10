package ralph

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Progress represents a ralph iteration progress record.
type Progress struct {
	PRNumber  int       `json:"pr_number"`
	Iteration int       `json:"iteration"`
	Stage     string    `json:"stage"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// WriteProgress writes a progress markdown file to
// .ralph/progress/pr-{N}-iteration-{M}.md in the repository directory.
// Creates the directory and file if they don't exist.
// Overwrites if the file already exists (idempotent per iteration).
func WriteProgress(repoDir string, prNum, iter int, stageMessages map[string]string) (string, error) {
	dir := filepath.Join(repoDir, ".ralph", "progress")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create progress dir: %w", err)
	}

	filename := fmt.Sprintf("pr-%d-iteration-%d.md", prNum, iter)
	filePath := filepath.Join(dir, filename)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Ralph Progress: PR #%d, Iteration %d\n\n", prNum, iter))
	sb.WriteString(fmt.Sprintf("**Timestamp**: %s\n\n", time.Now().UTC().Format(time.RFC3339)))

	// Write stage summaries
	stageOrder := []string{StageAwareness, StageAct, StageAssert, StageAppend}
	for _, stage := range stageOrder {
		msg, ok := stageMessages[stage]
		if ok {
			sb.WriteString(fmt.Sprintf("## %s\n\n%s\n\n", strings.Title(stage), msg))
		}
	}

	if err := os.WriteFile(filePath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("write progress file: %w", err)
	}

	return filePath, nil
}

// ReadProgress reads a progress file for a specific PR and iteration.
func ReadProgress(repoDir string, prNum, iter int) (*Progress, error) {
	filename := fmt.Sprintf("pr-%d-iteration-%d.md", prNum, iter)
	filePath := filepath.Join(repoDir, ".ralph", "progress", filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read progress file: %w", err)
	}

	// Parse basic info from the file header
	p := &Progress{
		PRNumber:  prNum,
		Iteration: iter,
		Timestamp: time.Now().UTC(),
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "**Timestamp**: ") {
			ts := strings.TrimPrefix(line, "**Timestamp**: ")
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				p.Timestamp = t
			}
		}
	}
	p.Stage = StageAppend // default to append since the file is written at append time

	return p, nil
}

// ListProgress returns all progress files for a given PR number.
func ListProgress(repoDir string, prNum int) ([]*Progress, error) {
	dir := filepath.Join(repoDir, ".ralph", "progress")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list progress dir: %w", err)
	}

	var results []*Progress
	prefix := fmt.Sprintf("pr-%d-iteration-", prNum)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}

		// Parse iteration number from filename
		// Format: pr-{N}-iteration-{M}.md
		parts := strings.TrimSuffix(entry.Name(), ".md")
		iterStr := strings.TrimPrefix(parts, prefix)
		var iter int
		fmt.Sscanf(iterStr, "%d", &iter)

		p, err := ReadProgress(repoDir, prNum, iter)
		if err != nil {
			continue
		}
		results = append(results, p)
	}

	return results, nil
}
