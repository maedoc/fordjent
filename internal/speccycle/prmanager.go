package speccycle

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/fordjent/fordjent/internal/forgejo"
)

// PRFilesLister is the interface for listing files in a pull request.
// This matches forgejo.Client.GetPRFiles signature exactly.
type PRFilesLister interface {
	GetPRFiles(ctx context.Context, repo string, number int) ([]forgejo.PRFile, error)
}

// SpecPRManager detects spec PRs via the Forgejo PR files API.
type SpecPRManager struct {
	filesClient PRFilesLister
}

// NewSpecPRManager creates a new SpecPRManager backed by a PRFilesLister.
func NewSpecPRManager(filesClient PRFilesLister) *SpecPRManager {
	return &SpecPRManager{filesClient: filesClient}
}

// IsSpecPR checks whether a PR contains openspec/changes/ files.
func (spm *SpecPRManager) IsSpecPR(ctx context.Context, repo string, prNumber int) (*SpecPRInfo, error) {
	return isSpecPR(ctx, spm.filesClient, repo, prNumber)
}

// isSpecPR checks whether a PR contains openspec/changes/ files.
func isSpecPR(ctx context.Context, filesClient PRFilesLister, repo string, prNumber int) (*SpecPRInfo, error) {
	files, err := filesClient.GetPRFiles(ctx, repo, prNumber)
	if err != nil {
		return &SpecPRInfo{IsSpecPR: false}, err
	}

	changeNames := make(map[string]bool)
	for _, f := range files {
		if isSpecFilePath(f.Filename) {
			changeName := extractChangeName(f.Filename)
			if changeName != "" {
				changeNames[changeName] = true
			}
		}
	}

	if len(changeNames) == 0 {
		return &SpecPRInfo{IsSpecPR: false}, nil
	}

	names := make([]string, 0, len(changeNames))
	for n := range changeNames {
		names = append(names, n)
	}

	slog.Info("detected spec PR",
		"repo", repo, "pr", prNumber, "changes", names,
	)

	for n := range changeNames {
		return &SpecPRInfo{IsSpecPR: true, ChangeName: n}, nil
	}

	return &SpecPRInfo{IsSpecPR: false}, nil
}

// isSpecFilePath checks if a file path is within openspec/changes/.
func isSpecFilePath(path string) bool {
	clean := filepath.Clean(path)
	parts := strings.SplitN(clean, string(filepath.Separator), 4)
	if len(parts) >= 3 {
		return parts[0] == "openspec" && parts[1] == "changes" && parts[2] != "archive"
	}
	return false
}

// extractChangeName extracts the change name from a spec file path.
// openspec/changes/<name>/... → <name>
func extractChangeName(path string) string {
	clean := filepath.Clean(path)
	parts := strings.SplitN(clean, string(filepath.Separator), 4)
	if len(parts) >= 3 && parts[0] == "openspec" && parts[1] == "changes" && parts[2] != "archive" {
		return parts[2]
	}
	return ""
}
