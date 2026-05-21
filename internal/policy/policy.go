// Package policy implements repo-level automation control for Fordjent.
// Policies are detected from Forgejo repo topics (e.g. "fordjent-yolo",
// "fordjent-plan-first") and control how aggressively the agent acts.
package policy

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Policy controls how much automation Fordjent applies to a repo.
type Policy struct {
	// PlanFirst requires human approval before implementers start coding.
	// PM sub-issues are created in "planning" state; human must add
	// "plan-approved" label to the parent issue to unblock them.
	PlanFirst bool

	// NoAutoMerge prevents the reviewer from calling forgejo_merge_pr.
	// The reviewer posts its review and waits for human action.
	NoAutoMerge bool

	// RequireReview requires a PR to have an "approved" label before
	// forgejo_merge_pr can be called, regardless of who triggers it.
	RequireReview bool

	// Yolo enables full automation: PM sub-issues fire immediately,
	// reviewer can auto-merge. This is the "everything goes" mode.
	// If Yolo is true, all other policies are ignored.
	Yolo bool
}

// DefaultPolicy returns the policy used when no repo topics are set.
// The default is the safer "plan-first + no-auto-merge" mode.
func DefaultPolicy() Policy {
	return Policy{
		PlanFirst:   true,
		NoAutoMerge: true,
	}
}

// YoloPolicy returns the full-automation policy.
func YoloPolicy() Policy {
	return Policy{Yolo: true}
}

// FromTopics derives a Policy from a list of Forgejo repo topics.
// Topics use the "fordjent-" prefix (e.g. "fordjent-yolo", "fordjent-plan-first").
// If "fordjent-yolo" is present, all other policies are off.
func FromTopics(topics []string) Policy {
	p := DefaultPolicy() // start with safe defaults

	for _, t := range topics {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "fordjent-yolo":
			return YoloPolicy() // yolo overrides everything
		case "fordjent-plan-first":
			p.PlanFirst = true
		case "fordjent-no-auto-merge":
			p.NoAutoMerge = true
		case "fordjent-require-review":
			p.RequireReview = true
		}
	}

	return p
}

// String returns a human-readable summary of the policy.
func (p Policy) String() string {
	if p.Yolo {
		return "yolo (full automation)"
	}
	var parts []string
	if p.PlanFirst {
		parts = append(parts, "plan-first")
	}
	if p.NoAutoMerge {
		parts = append(parts, "no-auto-merge")
	}
	if p.RequireReview {
		parts = append(parts, "require-review")
	}
	if len(parts) == 0 {
		return "none (no restrictions)"
	}
	return strings.Join(parts, "+")
}

// TopicList returns the Forgejo repo topics that correspond to this policy.
func (p Policy) TopicList() []string {
	if p.Yolo {
		return []string{"fordjent-yolo"}
	}
	var topics []string
	if p.PlanFirst {
		topics = append(topics, "fordjent-plan-first")
	}
	if p.NoAutoMerge {
		topics = append(topics, "fordjent-no-auto-merge")
	}
	if p.RequireReview {
		topics = append(topics, "fordjent-require-review")
	}
	return topics
}

// TopicsFetcher fetches repo topics from Forgejo.
type TopicsFetcher interface {
	GetRepoTopics(ctx context.Context, repo string) ([]string, error)
}

// CachedDetector caches per-repo policy detection.
type CachedDetector struct {
	fetcher TopicsFetcher
	cache   map[string]Policy
	mu      sync.RWMutex
}

// NewCachedDetector creates a policy detector that caches results per repo.
func NewCachedDetector(fetcher TopicsFetcher) *CachedDetector {
	return &CachedDetector{
		fetcher: fetcher,
		cache:   make(map[string]Policy),
	}
}

// Detect returns the policy for a repo. Results are cached after first call.
func (cd *CachedDetector) Detect(ctx context.Context, repo string) Policy {
	cd.mu.RLock()
	if p, ok := cd.cache[repo]; ok {
		cd.mu.RUnlock()
		return p
	}
	cd.mu.RUnlock()

	// Slow path: fetch from Forgejo
	topics, err := cd.fetcher.GetRepoTopics(ctx, repo)
	if err != nil {
		slog.Warn("failed to fetch repo topics for policy detection, using default", "repo", repo, "error", err)
		return DefaultPolicy()
	}

	p := FromTopics(topics)

	cd.mu.Lock()
	cd.cache[repo] = p
	cd.mu.Unlock()

	slog.Info("detected repo policy", "repo", repo, "policy", p.String(), "topics", topics)
	return p
}

// Invalidate clears the cached policy for a repo (e.g. after topics change).
func (cd *CachedDetector) Invalidate(repo string) {
	cd.mu.Lock()
	delete(cd.cache, repo)
	cd.mu.Unlock()
}

// InvalidateAll clears all cached policies.
func (cd *CachedDetector) InvalidateAll() {
	cd.mu.Lock()
	cd.cache = make(map[string]Policy)
	cd.mu.Unlock()
}