package metrics

import (
	"fmt"
	"strings"
	"testing"
)

func TestModelCardinalityBoundPreservesAccounting(t *testing.T) {
	r := New()
	for i := 0; i < 5000; i++ {
		model := fmt.Sprintf("model-%d", i)
		r.IncRequest(model, true, "ok")
		r.ObserveLatency(model, true, 20)
		r.ObserveCost(model, .5)
	}
	r.IncRequest(strings.Repeat("x", 10000), true, "ok")
	if len(r.models) != 256 || len(r.Requests.vals) != 257 || len(r.LatencyMs.vals) != 257 || len(r.CostMicroUSD.vals) != 257 {
		t.Fatal("unbounded metric labels")
	}
	var requests, cost, latency int64
	for _, value := range r.Requests.vals {
		requests += value.Load()
	}
	for _, value := range r.CostMicroUSD.vals {
		cost += value.Load()
	}
	for _, value := range r.LatencyMs.vals {
		latency += value.sum.Load()
	}
	if requests != 5001 || cost != 2_500_000_000 || latency != 100000 {
		t.Fatal("overflow samples were dropped")
	}
}
