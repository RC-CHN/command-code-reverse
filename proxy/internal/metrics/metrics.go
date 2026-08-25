// Package metrics provides a minimal zero-dependency Prometheus exporter:
// labelled counters plus sum/count pairs rendered in the text exposition
// format. Suffices for request/token/cost accounting without pulling in
// a client library.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter is a monotonic labelled counter family.
type Counter struct {
	name string
	help string
	mu   sync.Mutex
	vals map[string]*atomic.Int64
}

// Summary accumulates count+sum for latency/cost style metrics.
type Summary struct {
	name string
	help string
	mu   sync.Mutex
	vals map[string]*summaryVal
}

type summaryVal struct {
	count atomic.Int64
	sum   atomic.Int64 // milliseconds; integer precision is plenty
}

// Registry groups all exported families.
type Registry struct {
	Requests     *Counter // labels: model,stream,result
	Tokens       *Counter // label: kind (input|output|cached)
	LatencyMs    *Summary // labels: model,stream
	UpstreamErr  *Counter // label: class
	CostMicroUSD *Counter // label: model; micro-dollars (1e-6 USD)
}

// New builds the registry with all families.
func New() *Registry {
	return &Registry{
		Requests:     newCounter("commandcode_proxy_requests_total", "Chat completion requests by model/stream/result."),
		Tokens:       newCounter("commandcode_proxy_tokens_total", "Tokens served, by kind (input/output/cached)."),
		LatencyMs:    newSummary("commandcode_proxy_latency_ms", "End-to-end request latency (milliseconds)."),
		UpstreamErr:  newCounter("commandcode_proxy_upstream_errors_total", "Upstream failures by class."),
		CostMicroUSD: newCounter("commandcode_proxy_cost_microdollars_total", "Upstream cost in micro-dollars, by model."),
	}
}

// ── MetricsRecorder implementation (server-facing) ──────────────────

// IncRequest records one finished request.
func (r *Registry) IncRequest(model string, streamed bool, result string) {
	r.Requests.Inc(Labels("model", model, "stream", boolLabel(streamed), "result", result))
}

// AddTokens accumulates served tokens of one kind (input|output|cached).
func (r *Registry) AddTokens(kind string, n int) {
	r.Tokens.AddN(Labels("kind", kind), int64(n))
}

// ObserveLatency records end-to-end latency.
func (r *Registry) ObserveLatency(model string, streamed bool, ms int64) {
	r.LatencyMs.Observe(Labels("model", model, "stream", boolLabel(streamed)), ms)
}

// ObserveCost accumulates upstream cost (converted to micro-dollars).
func (r *Registry) ObserveCost(model string, usd float64) {
	if usd <= 0 {
		return
	}
	r.CostMicroUSD.AddN(Labels("model", model), int64(usd*1_000_000))
}

// IncUpstreamError records one upstream failure of the given class.
func (r *Registry) IncUpstreamError(class string) {
	r.UpstreamErr.Inc(Labels("class", class))
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func newCounter(name, help string) *Counter {
	return &Counter{name: name, help: help, vals: map[string]*atomic.Int64{}}
}

func newSummary(name, help string) *Summary {
	return &Summary{name: name, help: help, vals: map[string]*summaryVal{}}
}

// Inc increments the counter for the given label set.
func (c *Counter) Inc(labels string) {
	c.mu.Lock()
	v, ok := c.vals[labels]
	if !ok {
		v = &atomic.Int64{}
		c.vals[labels] = v
	}
	c.mu.Unlock()
	v.Add(1)
}

// AddN adds n to the counter for the given label set.
func (c *Counter) AddN(labels string, n int64) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	v, ok := c.vals[labels]
	if !ok {
		v = &atomic.Int64{}
		c.vals[labels] = v
	}
	c.mu.Unlock()
	v.Add(n)
}

// Observe records one summary sample.
func (s *Summary) Observe(labels string, ms int64) {
	s.mu.Lock()
	v, ok := s.vals[labels]
	if !ok {
		v = &summaryVal{}
		s.vals[labels] = v
	}
	s.mu.Unlock()
	v.count.Add(1)
	v.sum.Add(ms)
}

// Labels formats a label set from key/value pairs: {k="v",...}.
func Labels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", kv[i], kv[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

// Render emits the Prometheus text exposition format for all families.
func (r *Registry) Render() string {
	var b strings.Builder
	renderCounter(&b, r.Requests)
	renderCounter(&b, r.Tokens)
	renderCounter(&b, r.UpstreamErr)
	renderCounter(&b, r.CostMicroUSD)
	renderSummary(&b, r.LatencyMs)
	return b.String()
}

func renderCounter(b *strings.Builder, c *Counter) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	c.mu.Lock()
	keys := make([]string, 0, len(c.vals))
	for k := range c.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s%s %d\n", c.name, k, c.vals[k].Load())
	}
	c.mu.Unlock()
}

func renderSummary(b *strings.Builder, s *Summary) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s summary\n", s.name, s.help, s.name)
	s.mu.Lock()
	keys := make([]string, 0, len(s.vals))
	for k := range s.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := s.vals[k]
		fmt.Fprintf(b, "%s_count%s %d\n", s.name, k, v.count.Load())
		fmt.Fprintf(b, "%s_sum%s %d\n", s.name, k, v.sum.Load())
	}
	s.mu.Unlock()
}
