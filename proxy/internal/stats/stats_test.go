package stats

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestBoundedRetentionKeepsLifetimeAccounting(t *testing.T) {
	s := New(3, 2, "instance")
	for i := 0; i < 5000; i++ {
		ctx, r := s.Begin(context.Background(), "llm")
		SetModel(ctx, fmt.Sprintf("model-%d", i), true)
		SelectKey(ctx, "key-1")
		AttemptFailed(ctx)
		SelectKey(ctx, "key-2")
		r.Usage = &Tokens{Input: 10, Output: 3, Cached: 8, CacheWrite: 2, Reasoning: 1}
		cost := 0.25
		r.CostUSD = &cost
		r.Finish(200, false, false)
	}
	out := s.Snapshot("llm")
	if len(out.Recent) != 3 || out.Recent[0].Model != "model-4999" || out.Recent[2].Model != "model-4997" {
		t.Fatalf("ring order/retention: %+v", out.Recent)
	}
	if len(out.Models) != maxModels+1 || len(s.buckets["llm"].keys) != 3 {
		t.Fatal("unbounded aggregate dimensions")
	}
	if out.Totals.Requests != 5000 || out.Totals.Tokens.Input != 50000 || out.Totals.CostUSD != 1250 || out.Totals.InFlight != 0 {
		t.Fatalf("lifetime totals lost: %+v", out.Totals)
	}
	first, second := out.Keys[0], out.Keys[1]
	if first.ID != "key-1" || first.Attempts != 5000 || first.Requests != 0 || first.CostUSD != 0 || first.InFlight != 0 {
		t.Fatalf("rejected key charged: %+v", first)
	}
	if second.ID != "key-2" || second.Requests != 5000 || second.CostUSD != 1250 || second.InFlight != 0 {
		t.Fatalf("actual serving key not charged: %+v", second)
	}
}

func TestConcurrentSnapshotsAndInFlight(t *testing.T) {
	s := New(100, 1, "instance")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, r := s.Begin(context.Background(), "llm")
			SetModel(ctx, "m", true)
			SelectKey(ctx, "key-1")
			_ = s.Snapshot("llm")
			r.Finish(200, false, false)
		}()
	}
	wg.Wait()
	out := s.Snapshot("llm")
	if out.Totals.Requests != 100 || out.Totals.InFlight != 0 || out.Keys[0].InFlight != 0 {
		t.Fatalf("concurrent totals: %+v", out)
	}
}

func TestBoundedAttemptsAndNoDynamicCredentialBuckets(t *testing.T) {
	s := New(1, 80, "instance")
	ctx, r := s.Begin(context.Background(), "jev")
	for i := 0; i < 80; i++ {
		SelectKey(ctx, KeyID(i))
		AttemptFailed(ctx)
	}
	SelectKey(ctx, "caller-secret-must-not-be-stored")
	r.Finish(502, false, false)
	out := s.Snapshot("jev")
	if len(out.Recent[0].Attempts) != 32 || out.Recent[0].OmittedAttempts != 48 || out.Recent[0].KeyID != "key-80" {
		t.Fatalf("attempt bounds or final key lost: %+v", out.Recent[0])
	}
	if out.Totals.Attempts != 80 || len(out.Keys) != 81 || s.Snapshot("llm").Totals.Requests != 0 {
		t.Fatal("unexpected key buckets or endpoint mixing")
	}
}
