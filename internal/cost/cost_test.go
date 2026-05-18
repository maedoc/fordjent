package cost

import (
	"testing"
	"time"
)

func TestTrackerRecordAndQuery(t *testing.T) {
	tr, err := NewTracker(":memory:")
	if err != nil {
		t.Fatalf("new tracker: %v", err)
	}
	defer tr.Close()

	r := &UsageRecord{
		SessionKey:   "sess-1",
		ProviderName: "ollama-cloud",
		Model:        "minimax-m2.5",
		Repository:   "fjadmin/gogit",
		InputTokens:  1000,
		OutputTokens: 500,
		TotalTokens:  1500,
		CostUSD:      0.003,
		Timestamp:    time.Now(),
	}
	if err := tr.Record(r); err != nil {
		t.Fatalf("record: %v", err)
	}

	tokens, cost, err := tr.GetSessionCost("sess-1")
	if err != nil {
		t.Fatalf("get session cost: %v", err)
	}
	if tokens != 1500 {
		t.Fatalf("expected 1500 tokens, got %d", tokens)
	}
	if cost != 0.003 {
		t.Fatalf("expected cost 0.003, got %f", cost)
	}

	repoCost := tr.GetRepoCost("fjadmin/gogit")
	if repoCost != 0.003 {
		t.Fatalf("expected repo cost 0.003, got %f", repoCost)
	}
}

func TestTrackerBudgetCheck(t *testing.T) {
	tr, err := NewTracker(":memory:")
	if err != nil {
		t.Fatalf("new tracker: %v", err)
	}
	defer tr.Close()

	// Record a record that puts session cost over the limit
	_ = tr.Record(&UsageRecord{
		SessionKey:   "sess-2",
		ProviderName: "p",
		Model:        "m",
		InputTokens:  1000,
		OutputTokens: 0,
		TotalTokens:  1000,
		CostUSD:      1.50,
		Timestamp:    time.Now(),
	})

	allowed, reason := tr.CheckBudget("sess-2", true, 1.00, 100.00)
	if allowed {
		t.Fatal("expected budget exceeded")
	}
	if reason == "" {
		t.Fatal("expected reason for budget failure")
	}

	// Under budget should pass
	allowed, _ = tr.CheckBudget("sess-2", true, 2.00, 100.00)
	if !allowed {
		t.Fatal("expected budget ok")
	}

	allowed, _ = tr.CheckBudget("sess-2", false, 0.001, 0.001)
	if !allowed {
		t.Fatal("expected pass when budget disabled")
	}
}

func TestGetPerModelUsage(t *testing.T) {
	tr, err := NewTracker(":memory:")
	if err != nil {
		t.Fatalf("new tracker: %v", err)
	}
	defer tr.Close()

	_ = tr.Record(&UsageRecord{
		SessionKey: "s1", ProviderName: "wafer-qwen", Model: "qwen",
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CostUSD: 0.001, Timestamp: time.Now(),
	})
	_ = tr.Record(&UsageRecord{
		SessionKey: "s2", ProviderName: "wafer-qwen", Model: "qwen",
		InputTokens: 200, OutputTokens: 100, TotalTokens: 300, CostUSD: 0.002, Timestamp: time.Now(),
	})
	_ = tr.Record(&UsageRecord{
		SessionKey: "s3", ProviderName: "wafer-glm", Model: "glm",
		InputTokens: 300, OutputTokens: 150, TotalTokens: 450, CostUSD: 0.003, Timestamp: time.Now(),
	})

	results, err := tr.GetPerModelUsage(time.Time{})
	if err != nil {
		t.Fatalf("get per model usage: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	var qwenFound, glmFound bool
	for _, m := range results {
		switch m.Model {
		case "qwen":
			qwenFound = true
			if m.Calls != 2 {
				t.Fatalf("expected qwen calls=2, got %d", m.Calls)
			}
			if m.Provider != "wafer-qwen" {
				t.Fatalf("expected provider wafer-qwen, got %s", m.Provider)
			}
		case "glm":
			glmFound = true
			if m.Calls != 1 {
				t.Fatalf("expected glm calls=1, got %d", m.Calls)
			}
			if m.Provider != "wafer-glm" {
				t.Fatalf("expected provider wafer-glm, got %s", m.Provider)
			}
		}
	}
	if !qwenFound || !glmFound {
		t.Fatal("missing model in results")
	}

	future := time.Now().Add(24 * time.Hour)
	results, err = tr.GetPerModelUsage(future)
	if err != nil {
		t.Fatalf("get per model usage future: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for future time, got %d", len(results))
	}
}

func TestGetSessionModelUsage(t *testing.T) {
	tr, err := NewTracker(":memory:")
	if err != nil {
		t.Fatalf("new tracker: %v", err)
	}
	defer tr.Close()

	_ = tr.Record(&UsageRecord{
		SessionKey: "s1", ProviderName: "wafer-qwen", Model: "qwen",
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CostUSD: 0.001, Timestamp: time.Now(),
	})
	_ = tr.Record(&UsageRecord{
		SessionKey: "s1", ProviderName: "wafer-glm", Model: "glm",
		InputTokens: 200, OutputTokens: 100, TotalTokens: 300, CostUSD: 0.002, Timestamp: time.Now(),
	})
	_ = tr.Record(&UsageRecord{
		SessionKey: "s2", ProviderName: "wafer-qwen", Model: "qwen",
		InputTokens: 50, OutputTokens: 25, TotalTokens: 75, CostUSD: 0.0005, Timestamp: time.Now(),
	})

	results, err := tr.GetSessionModelUsage("s1")
	if err != nil {
		t.Fatalf("get session model usage s1: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for s1, got %d", len(results))
	}

	results, err = tr.GetSessionModelUsage("s2")
	if err != nil {
		t.Fatalf("get session model usage s2: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for s2, got %d", len(results))
	}
	if results[0].Model != "qwen" {
		t.Fatalf("expected model qwen, got %s", results[0].Model)
	}

	results, err = tr.GetSessionModelUsage("nonexistent")
	if err != nil {
		t.Fatalf("get session model usage nonexistent: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for nonexistent session, got %d", len(results))
	}
}
