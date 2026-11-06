package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/convert"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stats"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stream"
)

type chatDiagnosticsKey struct{}

// All fields are request-local. The pool updates selection synchronously when
// it acquires a key; logging never guesses the key from the pool's next route.
type chatDiagnostics struct {
	requestID      string
	start          time.Time
	stage          string
	selection      *stats.KeySelection
	idleTimeout    time.Duration
	ioWait         time.Duration
	ioErrorKind    string
	http2ErrorCode string
	receivedBytes  int64
	lastByte       time.Time
	lastEvent      string
	lastEventAt    time.Time
	eventCount     int64
	finishAt       time.Time
	finishReason   string
	state          *streamState
}

// observeIO runs on the handler goroutine after Generate or a body Read. The
// watchdog's cancellation cause takes precedence over its transport wakeup.
func (d *chatDiagnostics) observeIO(start time.Time, n int, err error) {
	now := time.Now()
	d.ioWait = now.Sub(start)
	d.receivedBytes += int64(n)
	if n > 0 {
		d.lastByte = now
	}
	d.ioErrorKind, d.http2ErrorCode = classifyChatIOError(err)
}

func (d *chatDiagnostics) observeEvent(ev *stream.Event) {
	if d == nil {
		return
	}
	d.eventCount++
	d.lastEventAt = time.Now()
	// Never retain an arbitrary upstream type or finish reason in diagnostics.
	switch ev.Type {
	case "start", "start-step", "text-start", "text-delta", "text-end",
		"reasoning-start", "reasoning-delta", "reasoning-end",
		"tool-input-start", "tool-input-delta", "tool-input-end", "tool-call", "tool-result",
		"finish-step", "finish", "provider-metadata", "cache-write-tokens", "abort", "error":
		d.lastEvent = ev.Type
	default:
		d.lastEvent = "unknown"
	}
	if ev.Type == "finish" && d.finishAt.IsZero() {
		d.finishAt = d.lastEventAt
		d.finishReason = ev.OpenAIFinishReason()
		if d.finishReason == "" {
			d.finishReason = "unknown"
		}
	}
}

func beginChatDiagnostics(w http.ResponseWriter, r *http.Request, keyCount int, start time.Time) (*http.Request, *chatDiagnostics) {
	ctx, selection := stats.ObserveKeys(r.Context(), keyCount)
	d := &chatDiagnostics{requestID: commandcode.NewTraceID(), start: start, stage: "generate", selection: selection, lastEvent: "none", ioErrorKind: "none"}
	// Generate our own ID rather than logging an arbitrary caller-supplied header.
	w.Header().Set("X-Request-ID", d.requestID)
	return r.WithContext(context.WithValue(ctx, chatDiagnosticsKey{}, d)), d
}

// logChatFailure receives only locally classified fields. Raw errors, upstream
// code/type strings, payloads, URLs and credential prefixes must not enter it.
// errorStatus is the classified failure status, even when SSE already sent 200.
func logChatFailure(r *http.Request, req *convert.ChatRequest, started bool, status int, body openAIErrorBody, upstreamStatus int) {
	d, _ := r.Context().Value(chatDiagnosticsKey{}).(*chatDiagnostics)
	if d == nil {
		return
	}
	model := req.Model
	if len(model) > 160 {
		model = "[long model ID]"
	}
	keyID := d.selection.ID
	if keyID == "" {
		keyID = "unselected"
	}
	attrs := []slog.Attr{
		slog.String("requestId", d.requestID),
		slog.String("model", model),
		slog.Bool("stream", req.Stream),
		slog.String("stage", d.stage),
		slog.String("keyId", keyID),
		slog.Int("attempts", d.selection.Attempts),
		slog.Bool("responseStarted", started),
		slog.Int("errorStatus", status),
		slog.String("errorType", body.Type),
		slog.String("code", diagnosticErrorCode(body)),
		slog.Int64("elapsedMs", time.Since(d.start).Milliseconds()),
		slog.String("ioErrorKind", d.ioErrorKind),
		slog.Int64("idleTimeoutMs", d.idleTimeout.Milliseconds()),
		slog.Int64("ioWaitMs", d.ioWait.Milliseconds()),
		slog.Int64("receivedBytes", d.receivedBytes),
		slog.Int64("eventCount", d.eventCount),
		slog.String("lastEvent", d.lastEvent),
		slog.Bool("finishSeen", !d.finishAt.IsZero()),
	}
	if d.http2ErrorCode != "" {
		attrs = append(attrs, slog.String("http2ErrorCode", d.http2ErrorCode))
	}
	if !d.lastByte.IsZero() {
		attrs = append(attrs, slog.Int64("sinceLastByteMs", time.Since(d.lastByte).Milliseconds()))
	}
	if !d.lastEventAt.IsZero() {
		attrs = append(attrs, slog.Int64("sinceLastEventMs", time.Since(d.lastEventAt).Milliseconds()))
	}
	if !d.finishAt.IsZero() {
		attrs = append(attrs, slog.String("finishReason", d.finishReason), slog.Int64("sinceFinishMs", time.Since(d.finishAt).Milliseconds()))
	}
	if d.state != nil {
		attrs = append(attrs, slog.Int("pendingToolCalls", len(d.state.pendingTools)))
	}
	if upstreamStatus >= 400 && upstreamStatus <= 599 {
		attrs = append(attrs, slog.Int("upstreamStatus", upstreamStatus))
	}
	if observed := stats.FromContext(r.Context()); observed != nil {
		attrs = append(attrs, slog.String("recordId", observed.ID))
	}
	slog.LogAttrs(r.Context(), slog.LevelWarn, "chat request failed", attrs...)
}

// These additional diagnostic codes do not change the downstream error body.
func diagnosticErrorCode(body openAIErrorBody) string {
	if body.Code != "" {
		return body.Code
	}
	switch body.Type {
	case "billing_error":
		return "upstream_insufficient_credits"
	case "rate_limit_error":
		return "upstream_rate_limited"
	case "plan_error":
		return "upstream_model_not_in_plan"
	case "invalid_request_error":
		return "upstream_model_not_recognized"
	case "upstream_auth_error":
		return "upstream_auth_rejected"
	case "server_error":
		return "upstream_server_error"
	default:
		return "upstream_error"
	}
}

func logChatAPIError(r *http.Request, req *convert.ChatRequest, started bool, err error) {
	var ae *commandcode.APIError
	if errors.As(err, &ae) {
		status, body, _ := classifyUpstreamError(ae)
		logChatFailure(r, req, started, status, openAIErrorBody{Type: body.Type, Code: body.Code}, ae.Status)
		return
	}
	var unavailable *keypool.UnavailableError
	if errors.As(err, &unavailable) {
		logChatFailure(r, req, started, http.StatusServiceUnavailable,
			openAIErrorBody{Type: "server_error", Code: "keypool_unavailable"}, 0)
	}
}
