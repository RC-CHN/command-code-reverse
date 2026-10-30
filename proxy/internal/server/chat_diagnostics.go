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
)

type chatDiagnosticsKey struct{}

// All fields are request-local. The pool updates selection synchronously when
// it acquires a key; logging never guesses the key from the pool's next route.
type chatDiagnostics struct {
	requestID string
	start     time.Time
	stage     string
	selection *stats.KeySelection
}

func beginChatDiagnostics(w http.ResponseWriter, r *http.Request, keyCount int, start time.Time) (*http.Request, *chatDiagnostics) {
	ctx, selection := stats.ObserveKeys(r.Context(), keyCount)
	d := &chatDiagnostics{requestID: commandcode.NewTraceID(), start: start, stage: "generate", selection: selection}
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
