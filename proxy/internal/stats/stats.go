// Package stats retains bounded, process-local inference accounting. It never
// receives credentials, prompts, responses, or reasoning text.
package stats

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxModels = 256

type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
}

type Aggregate struct {
	Requests     int64   `json:"requests"`
	Succeeded    int64   `json:"succeeded"`
	Failed       int64   `json:"failed"`
	Canceled     int64   `json:"canceled"`
	Attempts     int64   `json:"attempts"`
	InFlight     int64   `json:"inFlight"`
	Tokens       Tokens  `json:"tokens"`
	UsageReports int64   `json:"usageReports"`
	CostUSD      float64 `json:"costUSD"`
	CostReports  int64   `json:"costReports"`
	LatencyMS    int64   `json:"latencyMs"`
}

type Attempt struct {
	KeyID  string `json:"keyId"`
	Result string `json:"result"`
}

type Record struct {
	ID              string    `json:"id"`
	StartedAt       time.Time `json:"startedAt"`
	Endpoint        string    `json:"endpoint"`
	Model           string    `json:"model"`
	Stream          bool      `json:"stream"`
	KeyID           string    `json:"keyId"`
	Attempts        []Attempt `json:"attempts"`
	OmittedAttempts int       `json:"omittedAttempts"`
	Result          string    `json:"result"`
	Status          int       `json:"status"`
	LatencyMS       int64     `json:"latencyMs"`
	Usage           *Tokens   `json:"usage"`
	CostUSD         *float64  `json:"costUSD"`
}

type ModelStats struct {
	Model string `json:"model"`
	Aggregate
}

type KeyStats struct {
	ID string `json:"id"`
	Aggregate
}

type Snapshot struct {
	InstanceID string       `json:"instanceId"`
	StartedAt  time.Time    `json:"startedAt"`
	Now        time.Time    `json:"now"`
	Endpoint   string       `json:"endpoint"`
	Totals     Aggregate    `json:"totals"`
	Keys       []KeyStats   `json:"keys"`
	Models     []ModelStats `json:"models"`
	Recent     []Record     `json:"recent"`
	Capacity   int          `json:"capacity"`
}

type bucket struct {
	total  Aggregate
	keys   map[string]*Aggregate
	models map[string]*Aggregate
}

// Store's maps are bounded by configured keys and maxModels; completed request
// records occupy a fixed ring. Nothing grows with uptime or distinct callers.
type Store struct {
	mu         sync.Mutex
	instanceID string
	startedAt  time.Time
	sequence   uint64
	allowed    map[string]bool
	buckets    map[string]*bucket
	recent     []Record
	next, size int
}

func New(capacity, keyCount int, instanceID string) *Store {
	if capacity < 1 {
		capacity = 100
	}
	if capacity > 1000 {
		capacity = 1000
	}
	s := &Store{instanceID: instanceID, startedAt: time.Now(), allowed: map[string]bool{"passthrough": true}, buckets: map[string]*bucket{}, recent: make([]Record, capacity)}
	for i := 0; i < keyCount; i++ {
		s.allowed[KeyID(i)] = true
	}
	for _, endpoint := range []string{"llm", "jev"} {
		b := &bucket{keys: map[string]*Aggregate{}, models: map[string]*Aggregate{}}
		for id := range s.allowed {
			b.keys[id] = &Aggregate{}
		}
		s.buckets[endpoint] = b
	}
	return s
}

func KeyID(index int) string { return fmt.Sprintf("key-%d", index+1) }

type contextKey struct{}

// Request is owned by the handler goroutine. Only counters under Store.mu are
// read by concurrent snapshots; the in-progress record is never shared.
type Request struct {
	store *Store
	Record
}

func (s *Store) Begin(ctx context.Context, endpoint string) (context.Context, *Request) {
	s.mu.Lock()
	s.sequence++
	r := &Request{store: s, Record: Record{ID: fmt.Sprintf("%s-%d", s.instanceID, s.sequence), StartedAt: time.Now(), Endpoint: endpoint, Attempts: []Attempt{}}}
	s.buckets[endpoint].total.InFlight++
	s.mu.Unlock()
	return context.WithValue(ctx, contextKey{}, r), r
}

func FromContext(ctx context.Context) *Request {
	r, _ := ctx.Value(contextKey{}).(*Request)
	return r
}

func SetModel(ctx context.Context, model string, streamed bool) {
	if r := FromContext(ctx); r != nil {
		// Clone so a short model slice cannot retain a large decoded JSON buffer.
		if len(model) > 160 {
			model = "[long model ID]"
		}
		r.Model, r.Stream = strings.Clone(model), streamed
	}
}

// SelectKey observes the actual key lease, not a pre-request routing guess.
func SelectKey(ctx context.Context, id string) {
	r := FromContext(ctx)
	if r == nil || !r.store.allowed[id] {
		return
	}
	s := r.store
	s.mu.Lock()
	b := s.buckets[r.Endpoint]
	if previous := b.keys[r.KeyID]; previous != nil {
		previous.InFlight--
	}
	b.keys[id].InFlight++
	b.keys[id].Attempts++
	b.total.Attempts++
	s.mu.Unlock()
	r.KeyID = id
	// A pool tries each configured key at most once per request.
	if len(r.Attempts) < 32 {
		r.Attempts = append(r.Attempts, Attempt{KeyID: id, Result: "accepted"})
	} else {
		r.OmittedAttempts++
	}
}

func AttemptFailed(ctx context.Context) {
	if r := FromContext(ctx); r != nil && len(r.Attempts) > 0 && r.OmittedAttempts == 0 {
		r.Attempts[len(r.Attempts)-1].Result = "failed"
	}
}

func SetResult(ctx context.Context, result string) {
	if r := FromContext(ctx); r != nil {
		r.Result = result
	}
}

func (r *Request) Finish(status int, canceled, writeFailed bool) {
	r.Status = status
	r.LatencyMS = time.Since(r.StartedAt).Milliseconds()
	switch {
	case canceled:
		r.Result = "canceled"
	case writeFailed:
		r.Result = "downstream_error"
	case r.Result == "":
		if status >= 400 {
			r.Result = fmt.Sprintf("http_%d", status)
		} else {
			r.Result = "ok"
		}
	}
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[r.Endpoint]
	b.total.InFlight--
	b.total.add(r.Record)
	if k := b.keys[r.KeyID]; k != nil {
		k.InFlight--
		k.add(r.Record)
	}
	model := r.Model
	if b.models[model] == nil && len(b.models) >= maxModels {
		model = "[other models]"
	}
	if b.models[model] == nil {
		b.models[model] = &Aggregate{}
	}
	b.models[model].add(r.Record)
	s.recent[s.next] = r.Record
	s.next = (s.next + 1) % len(s.recent)
	if s.size < len(s.recent) {
		s.size++
	}
}

func (a *Aggregate) add(r Record) {
	a.Requests++
	switch r.Result {
	case "ok":
		a.Succeeded++
	case "canceled":
		a.Canceled++
	default:
		a.Failed++
	}
	a.LatencyMS += r.LatencyMS
	if r.Usage != nil {
		a.UsageReports++
		a.Tokens.Input += r.Usage.Input
		a.Tokens.Output += r.Usage.Output
		a.Tokens.Cached += r.Usage.Cached
		a.Tokens.CacheWrite += r.Usage.CacheWrite
		a.Tokens.Reasoning += r.Usage.Reasoning
	}
	if r.CostUSD != nil {
		a.CostReports++
		a.CostUSD += *r.CostUSD
		if math.IsInf(a.CostUSD, 1) {
			a.CostUSD = math.MaxFloat64
		}
	}
}

func (s *Store) Snapshot(endpoint string) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[endpoint]
	out := Snapshot{InstanceID: s.instanceID, StartedAt: s.startedAt, Now: time.Now(), Endpoint: endpoint, Totals: b.total, Keys: []KeyStats{}, Models: []ModelStats{}, Recent: []Record{}, Capacity: len(s.recent)}
	for id, a := range b.keys {
		out.Keys = append(out.Keys, KeyStats{ID: id, Aggregate: *a})
	}
	for model, a := range b.models {
		out.Models = append(out.Models, ModelStats{Model: model, Aggregate: *a})
	}
	sort.Slice(out.Keys, func(i, j int) bool { return out.Keys[i].ID < out.Keys[j].ID })
	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].Requests == out.Models[j].Requests {
			return out.Models[i].Model < out.Models[j].Model
		}
		return out.Models[i].Requests > out.Models[j].Requests
	})
	for i := 0; i < s.size; i++ {
		r := s.recent[(s.next-1-i+len(s.recent))%len(s.recent)]
		if r.Endpoint == endpoint {
			out.Recent = append(out.Recent, r)
		}
	}
	return out
}
