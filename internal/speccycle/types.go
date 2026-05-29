// Package speccycle implements OpenSpec change lifecycle management for Fordjent.
// It provides the SpecManager (file ops for changes) that the PM, implementer, and
// reviewer roles use to work with OpenSpec artifacts.
//
// Spec files live in the application repo under openspec/changes/<name>/
// and openspec/specs/<capability>/ — identical structure to what pi creates.
package speccycle

import (
	"context"
	"time"
)

// Change represents an OpenSpec change with its metadata and artifacts.
type Change struct {
	Name         string    `json:"name"`
	Schema       string    `json:"schema,omitempty"`
	LastModified time.Time `json:"last_modified"`
}

// Task represents a single task parsed from tasks.md.
type Task struct {
	Index       int    `json:"index"`       // 1-based position in tasks.md
	Description string `json:"description"` // The task description (without checkbox or tags)
	Done        bool   `json:"done"`        // true if - [x], false if - [ ]
	Parallel    bool   `json:"parallel"`    // true if [parallel] tag present
	Raw         string `json:"raw"`         // Original line text
}

// SpecPRInfo holds the result of spec PR detection.
type SpecPRInfo struct {
	IsSpecPR   bool   // Whether the PR contains spec files
	ChangeName string // The change name extracted from file paths
}

// SpecManager handles file-level OpenSpec operations:
// create, list, read, parse tasks, mark tasks complete, archive.
type SpecManager struct {
	// repoDir is the root of the application repository.
	repoDir string
}

// NewSpecManager creates a SpecManager rooted at the application repository.
func NewSpecManager(repoDir string) *SpecManager {
	return &SpecManager{repoDir: repoDir}
}

// CreateChange creates the directory structure for a new change.
func (sm *SpecManager) CreateChange(name string) error {
	return createChange(sm.repoDir, name)
}

// ListChanges scans openspec/changes/ and returns active (non-archived) changes.
func (sm *SpecManager) ListChanges() ([]Change, error) {
	return listChanges(sm.repoDir)
}

// ParseTasks reads and parses tasks.md for a given change.
func (sm *SpecManager) ParseTasks(name string) ([]Task, error) {
	return parseTasks(sm.repoDir, name)
}

// MarkTaskComplete updates a specific checkbox in tasks.md from - [ ] to - [x].
func (sm *SpecManager) MarkTaskComplete(name string, index int) error {
	return markTaskComplete(sm.repoDir, name, index)
}

// ArchiveChange moves a completed change to the archive directory
// and syncs delta specs to openspec/specs/<capability>/.
func (sm *SpecManager) ArchiveChange(name string) error {
	return archiveChange(sm.repoDir, name)
}

// PRFilesLister is the interface for listing files in a pull request.
// This matches forgejo.Client.GetPRFiles signature for easy mocking.
type PRFilesLister interface {
	GetPRFiles(ctx context.Context, repo string, number int) ([]PRFile, error)
}

// PRFile represents a file in a pull request diff (matches forgejo.PRFile shape).
type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// SpecPRManager detects spec PRs via the Forgejo PR files API.
type SpecPRManager struct {
	filesClient PRFilesLister
}

// NewSpecPRManager creates a new SpecPRManager backed by a Forgejo client.
func NewSpecPRManager(filesClient PRFilesLister) *SpecPRManager {
	return &SpecPRManager{filesClient: filesClient}
}

// IsSpecPR checks whether a PR contains openspec/changes/ files.
func (spm *SpecPRManager) IsSpecPR(ctx context.Context, repo string, prNumber int) (*SpecPRInfo, error) {
	return isSpecPR(ctx, spm.filesClient, repo, prNumber)
}
