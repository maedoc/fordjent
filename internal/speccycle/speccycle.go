package speccycle

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func createChange(repoDir, name string) error {
	if name == "" {
		return fmt.Errorf("change name is required")
	}
	changeDir := filepath.Join(repoDir, "openspec", "changes", name)
	if _, err := os.Stat(changeDir); err == nil {
		return fmt.Errorf("change %q already exists at %s", name, changeDir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check change dir %s: %w", changeDir, err)
	}

	if err := os.MkdirAll(changeDir, 0755); err != nil {
		return fmt.Errorf("create change dir %s: %w", changeDir, err)
	}

	// Also create specs subdirectory
	specsDir := filepath.Join(changeDir, "specs")
	if err := os.MkdirAll(specsDir, 0755); err != nil {
		return fmt.Errorf("create specs dir %s: %w", specsDir, err)
	}

	return nil
}

func listChanges(repoDir string) ([]Change, error) {
	changesDir := filepath.Join(repoDir, "openspec", "changes")
	entries, err := os.ReadDir(changesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read changes dir %s: %w", changesDir, err)
	}

	var changes []Change
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Skip archive directory
		if entry.Name() == "archive" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		changes = append(changes, Change{
			Name:         entry.Name(),
			LastModified: info.ModTime(),
		})
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].Name < changes[j].Name
	})

	return changes, nil
}

// changeDir returns the absolute path to a change directory.
func changeDir(repoDir, name string) string {
	return filepath.Join(repoDir, "openspec", "changes", name)
}

// tasksPath returns the absolute path to tasks.md for a change.
func tasksPath(repoDir, name string) string {
	return filepath.Join(changeDir(repoDir, name), "tasks.md")
}

// archiveDir returns the absolute path to the archive directory.
func archiveDir(repoDir string) string {
	return filepath.Join(repoDir, "openspec", "changes", "archive")
}

// archivePathFor returns the archive path for a completed change.
func archivePathFor(repoDir, name string) string {
	date := time.Now().UTC().Format("2006-01-02")
	return filepath.Join(archiveDir(repoDir), fmt.Sprintf("%s-%s", date, name))
}
