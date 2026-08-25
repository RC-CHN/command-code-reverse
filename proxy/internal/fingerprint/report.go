package fingerprint

import (
	"context"
	"crypto/rand"
	"log/slog"
	"math/big"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/ids"
)

const (
	// heartbeatBase mirrors the old proxy's refresh cadence.
	heartbeatBase = 8 * time.Hour
	// heartbeatJitter bounds the additive random jitter.
	heartbeatJitter = 2 * time.Hour
)

// Client is the telemetry slice of commandcode.Client.
type Client interface {
	RecordFingerprint(ctx context.Context, apiKey string, payload any) error
	RecordLifecycleEvent(ctx context.Context, apiKey string, payload any) error
}

// Reporter posts the fingerprint at startup and heartbeats afterwards.
// All failures are log-and-continue, matching the real CLI.
type Reporter struct {
	client Client
	key    string
	fp     *Fingerprint
}

// NewReporter builds a reporter for one upstream key (pool key[0]).
func NewReporter(client Client, apiKey string, fp *Fingerprint) *Reporter {
	return &Reporter{client: client, key: apiKey, fp: fp}
}

// Start runs the report loop until ctx is cancelled. Call as a goroutine.
func (r *Reporter) Start(ctx context.Context) {
	r.reportOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(heartbeatBase + randJitter()):
			r.reportOnce(ctx)
		}
	}
}

// reportOnce posts the fingerprint plus a lifecycle heartbeat.
func (r *Reporter) reportOnce(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := r.client.RecordFingerprint(callCtx, r.key, r.fp); err != nil {
		slog.Warn("fingerprint report failed", "error", err)
	} else {
		slog.Info("fingerprint reported")
	}

	if err := r.client.RecordLifecycleEvent(callCtx, r.key, map[string]any{
		"eventType": "cli_session_exists",
		"metadata": map[string]any{
			"sessionId":  "sess_" + ids.NewUUID()[:16],
			"cliVersion": "",
			"mode":       "interactive",
			"os":         r.fp.Components.Platform + "-" + r.fp.Components.Arch,
		},
	}); err != nil {
		slog.Warn("lifecycle event failed", "error", err)
	}
}

// randJitter returns a uniform duration in [0, heartbeatJitter).
func randJitter() time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(heartbeatJitter)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}
