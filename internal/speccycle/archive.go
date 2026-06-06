package speccycle

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func archiveChange(repoDir, name string) error {
	src := changeDir(repoDir, name)

	// Verify source exists
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return fmt.Errorf("change %q not found at %s", name, src)
	}

	dst := archivePathFor(repoDir, name)

	// Check that archive target doesn't already exist
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("archive target already exists: %s", dst)
	}

	// Create archive parent directory
	archiveParent := archiveDir(repoDir)
	if err := os.MkdirAll(archiveParent, 0755); err != nil {
		return fmt.Errorf("create archive dir %s: %w", archiveParent, err)
	}

	// Step 1: Sync delta specs to openspec/specs/<capability>/ before moving
	if err := syncDeltaSpecs(src, repoDir); err != nil {
		slog.Warn("failed to sync delta specs during archive",
			"change", name, "error", err)
		// Non-fatal: continue with move
	}

	// Step 2: Move the change directory to archive
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move %s to %s: %w", src, dst, err)
	}

	slog.Info("archived change",
		"change", name,
		"from", src,
		"to", dst,
	)

	return nil
}

// syncDeltaSpecs copies spec files from a change's specs/ directory to openspec/specs/.
// For each capability directory in <change>/specs/<capability>/, if it contains spec.md,
// copy it to openspec/specs/<capability>/spec.md.
func syncDeltaSpecs(changeDir, repoDir string) error {
	specsSrc := filepath.Join(changeDir, "specs")
	entries, err := os.ReadDir(specsSrc)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No specs to sync
		}
		return fmt.Errorf("read change specs dir %s: %w", specsSrc, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		capability := entry.Name()
		srcFile := filepath.Join(specsSrc, capability, "spec.md")
		if _, err := os.Stat(srcFile); os.IsNotExist(err) {
			continue
		}

		dstDir := filepath.Join(repoDir, "openspec", "specs", capability)
		dstFile := filepath.Join(dstDir, "spec.md")

		if err := os.MkdirAll(dstDir, 0755); err != nil {
			return fmt.Errorf("create spec dir %s: %w", dstDir, err)
		}

		if err := copyFile(srcFile, dstFile); err != nil {
			return fmt.Errorf("copy spec %s → %s: %w", srcFile, dstFile, err)
		}

		slog.Info("synced delta spec",
			"capability", capability,
			"from", srcFile,
			"to", dstFile,
		)
	}

	return nil
}

// copyFile copies a file from src to dst, creating directories as needed.
func copyFile(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	dstF, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstF.Close()

	if _, err := io.Copy(dstF, srcF); err != nil {
		return err
	}
	return dstF.Close()
}

// ChangeExists checks whether a change directory exists.
func ChangeExists(repoDir, name string) bool {
	dir := changeDir(repoDir, name)
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// ReadChangeFile reads a file relative to a change's directory.
// The filePath must be relative and not escape the change directory.
func ReadChangeFile(repoDir, name, filePath string) (string, error) {
	changeRoot := changeDir(repoDir, name)
	fullPath := filepath.Join(changeRoot, filepath.Clean(filePath))

	// Security: ensure the resolved path is within the change directory
	if !strings.HasPrefix(fullPath, changeRoot+string(filepath.Separator)) && fullPath != changeRoot {
		return "", fmt.Errorf("path %q escapes change directory", filePath)
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Push sessions and other non-code sessions may not have the repo
			// cloned with openspec/changes directories. Return a helpful message
			// instead of an error that breaks the tool loop.
			return fmt.Sprintf("Change %q or file %q not found in this checkout. "+
				"This session may not have the spec directory available. "+
				"Use openspec_read_spec to read merged specs instead.", name, filePath), nil
		}
		return "", fmt.Errorf("read %s: %w", fullPath, err)
	}
	return string(data), nil
}

// ReadSpecFile reads a spec file from openspec/specs/<capability>/spec.md.
// If the spec isn't found there, it searches all active changes for it.
func ReadSpecFile(repoDir, capability string) (string, error) {
	// First check the merged specs directory
	mergedPath := filepath.Join(repoDir, "openspec", "specs", capability, "spec.md")
	if data, err := os.ReadFile(mergedPath); err == nil {
		return string(data), nil
	}

	// Fall back to searching active changes
	changes, err := listChanges(repoDir)
	if err != nil {
		return "", fmt.Errorf("list changes: %w", err)
	}

	for _, ch := range changes {
		changeSpecPath := filepath.Join(changeDir(repoDir, ch.Name), "specs", capability, "spec.md")
		if data, err := os.ReadFile(changeSpecPath); err == nil {
			return string(data), nil
		}
	}

	return "", fmt.Errorf("spec %q not found in merged specs or active changes", capability)
}
