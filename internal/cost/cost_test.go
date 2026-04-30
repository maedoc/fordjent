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

	// Disabled budget should always pass
	allowed, _ = tr.CheckBudget("sess-2", false, 0.001, 0.001)
	if !allowed {
		t.Fatal("expected pass when budget disabled")
	}
}
