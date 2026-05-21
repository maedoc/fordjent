package policy

import (
	"context"
	"testing"
)

// stubFetcher implements TopicsFetcher for testing.
type stubFetcher struct {
	topics map[string][]string
	err    map[string]error
}

func (sf *stubFetcher) GetRepoTopics(_ context.Context, repo string) ([]string, error) {
	if err, ok := sf.err[repo]; ok {
		return nil, err
	}
	if topics, ok := sf.topics[repo]; ok {
		return topics, nil
	}
	return []string{}, nil
}

func TestFromTopics_Empty(t *testing.T) {
	p := FromTopics([]string{})
	// Default is plan-first + no-auto-merge
	if !p.PlanFirst {
		t.Error("expected PlanFirst=true by default")
	}
	if !p.NoAutoMerge {
		t.Error("expected NoAutoMerge=true by default")
	}
	if p.RequireReview {
		t.Error("expected RequireReview=false by default")
	}
	if p.Yolo {
		t.Error("expected Yolo=false by default")
	}
}

func TestFromTopics_Yolo(t *testing.T) {
	p := FromTopics([]string{"fordjent-yolo"})
	if !p.Yolo {
		t.Error("expected Yolo=true")
	}
	if p.PlanFirst {
		t.Error("expected PlanFirst=false when Yolo")
	}
	if p.NoAutoMerge {
		t.Error("expected NoAutoMerge=false when Yolo")
	}
}

func TestFromTopics_YoloOverrides(t *testing.T) {
	// Yolo should override plan-first even if both are set
	p := FromTopics([]string{"fordjent-yolo", "fordjent-plan-first"})
	if !p.Yolo {
		t.Error("expected Yolo=true")
	}
	if p.PlanFirst {
		t.Error("expected PlanFirst overridden to false by Yolo")
	}
}

func TestFromTopics_PlanFirst(t *testing.T) {
	p := FromTopics([]string{"fordjent-plan-first"})
	if !p.PlanFirst {
		t.Error("expected PlanFirst=true")
	}
}

func TestFromTopics_NoAutoMerge(t *testing.T) {
	p := FromTopics([]string{"fordjent-no-auto-merge"})
	if !p.NoAutoMerge {
		t.Error("expected NoAutoMerge=true")
	}
}

func TestFromTopics_RequireReview(t *testing.T) {
	p := FromTopics([]string{"fordjent-require-review"})
	if !p.RequireReview {
		t.Error("expected RequireReview=true")
	}
}

func TestFromTopics_Combined(t *testing.T) {
	p := FromTopics([]string{"fordjent-plan-first", "fordjent-require-review"})
	if !p.PlanFirst {
		t.Error("expected PlanFirst=true")
	}
	if !p.RequireReview {
		t.Error("expected RequireReview=true")
	}
	if p.Yolo {
		t.Error("expected Yolo=false")
	}
}

func TestFromTopics_IgnoresUnknown(t *testing.T) {
	p := FromTopics([]string{"go", "docker", "fordjent-plan-first"})
	if !p.PlanFirst {
		t.Error("expected PlanFirst=true despite unknown topics")
	}
}

func TestFromTopics_CaseInsensitive(t *testing.T) {
	p := FromTopics([]string{"Fordjent-YOLO"})
	if !p.Yolo {
		t.Error("expected case-insensitive Yolo detection")
	}
}

func TestPolicy_String(t *testing.T) {
	tests := []struct {
		policy Policy
		want   string
	}{
		{YoloPolicy(), "yolo (full automation)"},
		{Policy{}, "none (no restrictions)"},
		{Policy{PlanFirst: true}, "plan-first"},
		{Policy{PlanFirst: true, NoAutoMerge: true}, "plan-first+no-auto-merge"},
		{Policy{PlanFirst: true, NoAutoMerge: true, RequireReview: true}, "plan-first+no-auto-merge+require-review"},
		{DefaultPolicy(), "plan-first+no-auto-merge"},
	}
	for _, tt := range tests {
		got := tt.policy.String()
		if got != tt.want {
			t.Errorf("Policy.String() = %q, want %q", got, tt.want)
		}
	}
}

func TestPolicy_TopicList(t *testing.T) {
	tests := []struct {
		policy Policy
		want   []string
	}{
		{YoloPolicy(), []string{"fordjent-yolo"}},
		{Policy{}, nil},
		{Policy{PlanFirst: true}, []string{"fordjent-plan-first"}},
		{Policy{PlanFirst: true, NoAutoMerge: true}, []string{"fordjent-plan-first", "fordjent-no-auto-merge"}},
	}
	for _, tt := range tests {
		got := tt.policy.TopicList()
		if len(got) != len(tt.want) {
			t.Errorf("TopicList() = %v, want %v", got, tt.want)
			continue
		}
		for i, v := range got {
			if v != tt.want[i] {
				t.Errorf("TopicList()[%d] = %q, want %q", i, v, tt.want[i])
			}
		}
	}
}

func TestCachedDetector_CacheHit(t *testing.T) {
	sf := &stubFetcher{
		topics: map[string][]string{
			"fjadmin/test": {"fordjent-yolo"},
		},
	}
	cd := NewCachedDetector(sf)

	ctx := context.Background()
	p1 := cd.Detect(ctx, "fjadmin/test")
	if !p1.Yolo {
		t.Error("first call: expected Yolo=true")
	}

	// Change the underlying topics — cache should still return yolo
	sf.topics["fjadmin/test"] = []string{"fordjent-plan-first"}
	p2 := cd.Detect(ctx, "fjadmin/test")
	if !p2.Yolo {
		t.Error("cached call: expected Yolo=true (cached)")
	}
}

func TestCachedDetector_Invalidate(t *testing.T) {
	sf := &stubFetcher{
		topics: map[string][]string{
			"fjadmin/test": {"fordjent-yolo"},
		},
	}
	cd := NewCachedDetector(sf)
	ctx := context.Background()

	cd.Detect(ctx, "fjadmin/test") // populate cache

	// Change topics and invalidate
	sf.topics["fjadmin/test"] = []string{"fordjent-plan-first"}
	cd.Invalidate("fjadmin/test")

	p := cd.Detect(ctx, "fjadmin/test")
	if p.Yolo {
		t.Error("after invalidate: expected Yolo=false")
	}
	if !p.PlanFirst {
		t.Error("after invalidate: expected PlanFirst=true")
	}
}

func TestCachedDetector_FetchError(t *testing.T) {
	sf := &stubFetcher{
		err: map[string]error{
			"fjadmin/broken": context.DeadlineExceeded,
		},
	}
	cd := NewCachedDetector(sf)
	ctx := context.Background()

	p := cd.Detect(ctx, "fjadmin/broken")
	// Should return default policy on error
	if !p.PlanFirst {
		t.Error("expected default policy on fetch error")
	}
}