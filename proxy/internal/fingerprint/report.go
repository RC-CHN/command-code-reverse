package fingerprint

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/ids"
)

const (
	reportWorkers   = 2
	reportQueueSize = 64
	maxReportedKeys = 4096
	reportTimeout   = 15 * time.Second
)

// Client reports telemetry with the version of the accepted inference request.
type Client interface {
	RecordFingerprint(context.Context, string, any, string) error
	RecordCLISession(context.Context, string, string, string, string) error
}

type activity struct {
	session, version  string
	last              time.Time
	idle              time.Duration
	inflight          int
	queued, attempted bool
}

type reportJob struct {
	key      string
	id       [sha256.Size]byte
	activity *activity
}

// Reporter models logical CLI sessions driven only by accepted inference.
// Idle expiration never sends traffic; a later real request starts a new
// session on the same device. No periodic heartbeat is sent.
type Reporter struct {
	client         Client
	fp             *Fingerprint
	ctx            context.Context
	cancel         context.CancelFunc
	queue          chan reportJob
	wg             sync.WaitGroup
	mu             sync.Mutex
	active         map[[sha256.Size]byte]*activity
	idle           time.Duration
	seed           string
	now            func() time.Time
	capacityLogged bool
}

// NewReporter starts bounded workers. idle is the base idle window; each
// account gets a stable +/-20% offset so there is no fixed renewal cadence.
func NewReporter(ctx context.Context, client Client, fp *Fingerprint, idle time.Duration, seed string) *Reporter {
	ctx, cancel := context.WithCancel(ctx)
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	if seed == "" {
		seed = ids.NewUUID()
	}
	r := &Reporter{
		client: client, fp: fp, ctx: ctx, cancel: cancel,
		queue:  make(chan reportJob, reportQueueSize),
		active: make(map[[sha256.Size]byte]*activity),
		idle:   idle, seed: seed, now: time.Now,
	}
	for range reportWorkers {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				select {
				case <-r.ctx.Done():
					return
				case job := <-r.queue:
					if r.ctx.Err() != nil {
						return
					}
					r.reportOnce(job)
				}
			}
		}()
	}
	return r
}

// Use marks a real, accepted inference response active until finish is called.
// It never waits for network I/O. Only hashes are retained between reports.
func (r *Reporter) Use(key, version string) (finish func()) {
	if key == "" || r.ctx.Err() != nil {
		return nil
	}
	id := sha256.Sum256([]byte(key))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return nil
	}
	now := r.now()
	a := r.active[id]
	if a == nil && len(r.active) >= maxReportedKeys {
		for oldID, old := range r.active {
			if old.inflight == 0 && now.Sub(old.last) >= old.idle {
				delete(r.active, oldID)
			}
		}
		if len(r.active) >= maxReportedKeys {
			if !r.capacityLogged {
				r.capacityLogged = true
				slog.Warn("fingerprint active key capacity reached; skipping new keys")
			}
			return nil
		}
	}
	if a == nil || a.version != version || a.inflight == 0 && now.Sub(a.last) >= a.idle {
		// Stable per seed/account; this never schedules background traffic.
		offset := profilePick(r.seed, "idle:"+key, 401) - 200
		a = &activity{
			session: "sess_" + strings.ReplaceAll(ids.NewUUID(), "-", "")[:16],
			version: version, last: now,
			idle: r.idle + r.idle/1000*time.Duration(offset),
		}
		r.active[id] = a
	}
	a.inflight++
	a.last = now
	if !a.queued && !a.attempted {
		select {
		case r.queue <- reportJob{key, id, a}:
			a.queued = true
		default: // A later real request can enqueue the same session.
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			a.inflight--
			a.last = r.now()
		})
	}
}

func (r *Reporter) eligible(job reportJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := job.activity
	return r.ctx.Err() == nil && r.active[job.id] == a &&
		(a.inflight > 0 || r.now().Sub(a.last) < a.idle)
}

// Close cancels workers and releases queued passthrough credentials.
func (r *Reporter) Close() {
	r.cancel()
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.queue) > 0 {
		<-r.queue
	}
}

func (r *Reporter) reportOnce(job reportJob) {
	if !r.eligible(job) {
		return
	}
	r.mu.Lock()
	job.activity.attempted = true
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, reportTimeout)
	err := r.client.RecordFingerprint(ctx, job.key, r.fp, job.activity.version)
	cancel()
	if err != nil {
		// Error bodies can quote credentials or fingerprint data.
		slog.Warn("fingerprint report failed")
	} else {
		slog.Info("fingerprint reported")
	}
	if !r.eligible(job) {
		return
	}
	ctx, cancel = context.WithTimeout(r.ctx, reportTimeout)
	defer cancel()
	if err := r.client.RecordCLISession(ctx, job.key, job.activity.session, r.fp.Components.Platform+"-"+r.fp.Components.Arch, job.activity.version); err != nil {
		slog.Warn("lifecycle event failed")
	}
}
