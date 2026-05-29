package speccycle

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
)

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

	// Convert map to list for logging
	names := make([]string, 0, len(changeNames))
	for n := range changeNames {
		names = append(names, n)
	}

	slog.Info("detected spec PR",
		"repo", repo,
		"pr", prNumber,
		"changes", names,
	)

	// Return the first (and typically only) change name
	for n := range changeNames {
		return &SpecPRInfo{
			IsSpecPR:   true,
			ChangeName: n,
		}, nil
	}

	return &SpecPRInfo{IsSpecPR: false}, nil
}

// isSpecFilePath checks if a file path is within openspec/changes/.
func isSpecFilePath(path string) bool {
	clean := filepath.Clean(path)
	// Match: openspec/changes/<name>/...
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
