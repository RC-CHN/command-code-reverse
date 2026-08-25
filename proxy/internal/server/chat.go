package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/convert"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/ids"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stream"
)

// Upstream abstracts the generate call so the keypool (step 6) can plug in
// without touching the handler.
type Upstream interface {
	// Generate starts an upstream stream. The hint carries the downstream
	// identity for key affinity; it may be empty.
	Generate(ctx context.Context, hint string, req *commandcode.GenerateRequest) (io.ReadCloser, error)
}

// timeoutReduceThreshold is how many consecutive idle timeouts trigger the
// "reduce context" hint (inherited from the old proxy).
const timeoutReduceThreshold = 3

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req convert.ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body: "+err.Error(), 0)
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required", 0)
		return
	}

	wire, err := convert.ToWire(&req, ids.NewUUID(), s.cfg.MaxTokensClamp, 64000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
		return
	}

	// The upstream context dies with the downstream connection.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Idle watchdog: cancel the upstream request when no event arrives
	// within the configured window. Reset on every event.
	idle := s.cfg.StreamIdleTimeout
	if !req.Stream {
		idle = s.cfg.NonStreamIdleTimeout
	}
	idleTimer := time.AfterFunc(idle, func() { cancel() })
	defer idleTimer.Stop()

	body, err := s.deps.Upstream.Generate(ctx, downstreamKey(r.Context()), wire)
	if err != nil {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return // downstream hung up before the first byte
		}
		slog.Warn("upstream generate failed", "model", req.Model, "error", err)
		s.recordFailure(&req, req.Stream, "upstream_error")
		writeUpstreamError(w, err)
		return
	}
	defer func() { _ = body.Close() }()

	if req.Stream {
		s.serveStream(w, r, body, &req, start, idleTimer, idle)
	} else {
		s.serveNonStream(w, r, body, &req, start, idleTimer, idle)
	}
}

// streamState tracks an in-flight upstream stream for both handlers.
type streamState struct {
	fullText     string
	reasoning    string
	toolCalls    []map[string]any
	toolCallIdx  int
	finishReason string
	usage        *stream.Usage
	costUSD      float64
	chunkCount   int
}

// absorb folds bookkeeping-only events (usage, finish reason, cost) into
// the state. Returns nothing; visible events are handled by the caller.
func (st *streamState) absorb(ev *stream.Event) {
	switch ev.Type {
	case "finish-step":
		if ev.FinishReason != "" {
			st.finishReason = stream.MapFinishReason(ev.FinishReason)
		}
		if ev.Usage != nil {
			st.usage = ev.Usage
		}
		st.absorbCost(ev)
	case "finish":
		if st.finishReason == "" {
			st.finishReason = stream.MapFinishReason(ev.FinishReason)
		}
		if ev.TotalUsage != nil {
			st.usage = ev.TotalUsage
		}
	case "provider-metadata":
		st.absorbCost(ev)
	}
}

// absorbCost captures gateway cost from finish-step / provider-metadata.
func (st *streamState) absorbCost(ev *stream.Event) {
	if c := ev.ProviderMetadata.CostUSD(); c > 0 {
		st.costUSD = c
	}
}

// toolCallArgs renders a tool-call input as an OpenAI arguments JSON string.
func toolCallArgs(input json.RawMessage) string {
	if len(input) > 0 {
		return string(input)
	}
	return "{}"
}

// serveStream relays the upstream NDJSON stream as OpenAI SSE. The 200
// header is delayed until the first visible chunk so early failures can
// still be reported as JSON errors.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, body io.Reader, req *convert.ChatRequest, start time.Time, idleTimer *time.Timer, idle time.Duration) {
	reader := stream.NewReader(body)
	st := &streamState{}
	completionID := ids.NewCompletionID()
	created := time.Now().Unix()
	started := false
	flusher, _ := w.(http.Flusher)

	writeChunk := func(c *stream.Chunk) {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		_, _ = io.WriteString(w, stream.SSE(c))
		if flusher != nil {
			flusher.Flush()
		}
	}

	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.handleStreamReadError(w, r, req, started, err, start)
			return
		}
		idleTimer.Reset(idle)

		// In-band terminal markers (billing/plan) — the old proxy swallowed
		// these; we surface them with explicit semantics.
		if te := stream.TerminalError(ev); te != nil {
			status, typ, msg := terminalErrorStatus(te.Message)
			slog.Warn("terminal marker in stream", "marker", te.Message, "model", req.Model)
			s.recordFailure(req, true, "terminal_marker")
			if !started {
				writeError(w, status, typ, msg, 0)
			} else {
				_, _ = io.WriteString(w, streamErrorChunk(msg, typ))
				_, _ = io.WriteString(w, stream.DoneSSE)
			}
			return
		}

		switch ev.Type {
		case "text-delta":
			if ev.Text == "" {
				continue
			}
			st.fullText += ev.Text
			delta := map[string]any{"content": ev.Text}
			if st.chunkCount == 0 {
				delta["role"] = "assistant"
			}
			st.chunkCount++
			writeChunk(stream.NewChunk(completionID, created, req.Model, delta, nil, nil))

		case "reasoning-delta":
			if ev.Text == "" {
				continue
			}
			st.reasoning += ev.Text
			delta := map[string]any{"reasoning_content": ev.Text}
			if st.chunkCount == 0 {
				delta["role"] = "assistant"
			}
			st.chunkCount++
			writeChunk(stream.NewChunk(completionID, created, req.Model, delta, nil, nil))

		case "tool-call":
			tc := map[string]any{
				"index": st.toolCallIdx,
				"id":    ev.ToolCallID,
				"type":  "function",
				"function": map[string]any{
					"name":      ev.ToolName,
					"arguments": toolCallArgs(ev.Input),
				},
			}
			st.toolCallIdx++
			delta := map[string]any{"tool_calls": []map[string]any{tc}}
			if st.chunkCount == 0 {
				delta["role"] = "assistant"
			}
			st.chunkCount++
			writeChunk(stream.NewChunk(completionID, created, req.Model, delta, nil, nil))

		case "finish":
			st.absorb(ev)
			writeChunk(stream.NewChunk(completionID, created, req.Model,
				map[string]any{}, &st.finishReason, stream.UsageFromUpstream(st.usage)))

		case "finish-step", "provider-metadata":
			st.absorb(ev)
		}
		// start / start-step / reasoning-start / reasoning-end / text-start /
		// text-end / provider-metadata: intentionally silent. start-step and
		// provider-metadata leak upstream internals — never forwarded.
	}

	if !started {
		// Stream ended with nothing visible: treat as retryable (the old
		// proxy's zero-output guard).
		s.recordFailure(req, true, "empty_response")
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"Empty response from upstream (zero output tokens)", 10)
		return
	}
	_, _ = io.WriteString(w, stream.DoneSSE)
	s.consecutiveTimeouts.Store(0)
	s.recordSuccess(req, true, st, start)
	slog.Info("chat completion served",
		"model", req.Model, "stream", true,
		"inputTokens", usageIn(st.usage), "outputTokens", usageOut(st.usage),
		"cachedInputTokens", usageCached(st.usage), "costUSD", st.costUSD,
		"elapsedMs", time.Since(start).Milliseconds(),
	)
}

// serveNonStream aggregates the upstream stream into a single JSON response.
func (s *Server) serveNonStream(w http.ResponseWriter, r *http.Request, body io.Reader, req *convert.ChatRequest, start time.Time, idleTimer *time.Timer, idle time.Duration) {
	reader := stream.NewReader(body)
	st := &streamState{}

	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.handleStreamReadError(w, r, req, false, err, start)
			return
		}
		idleTimer.Reset(idle)

		if te := stream.TerminalError(ev); te != nil {
			status, typ, msg := terminalErrorStatus(te.Message)
			slog.Warn("terminal marker in stream", "marker", te.Message, "model", req.Model)
			s.recordFailure(req, false, "terminal_marker")
			writeError(w, status, typ, msg, 0)
			return
		}

		switch ev.Type {
		case "text-delta":
			st.fullText += ev.Text
		case "reasoning-delta":
			st.reasoning += ev.Text
		case "tool-call":
			st.toolCalls = append(st.toolCalls, map[string]any{
				"id":   ev.ToolCallID,
				"type": "function",
				"function": map[string]any{
					"name":      ev.ToolName,
					"arguments": toolCallArgs(ev.Input),
				},
			})
		case "finish", "finish-step", "provider-metadata":
			st.absorb(ev)
		}
	}

	if usageOut(st.usage) == 0 {
		s.recordFailure(req, false, "empty_response")
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"Empty response from upstream (zero output tokens)", 10)
		return
	}

	message := map[string]any{"role": "assistant"}
	if st.fullText != "" {
		message["content"] = st.fullText
	} else {
		message["content"] = nil
	}
	if len(st.toolCalls) > 0 {
		message["tool_calls"] = st.toolCalls
	}
	if st.reasoning != "" {
		message["reasoning_content"] = st.reasoning
	}

	resp := map[string]any{
		"id":      ids.NewCompletionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": st.finishReason,
		}},
		"usage": stream.UsageFromUpstream(st.usage),
	}

	if s.cfg.CostHeaderEnabled && st.costUSD > 0 {
		w.Header().Set("x-commandcode-cost", fmtCost(st.costUSD))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	s.consecutiveTimeouts.Store(0)
	s.recordSuccess(req, false, st, start)
	slog.Info("chat completion served",
		"model", req.Model, "stream", false,
		"inputTokens", usageIn(st.usage), "outputTokens", usageOut(st.usage),
		"cachedInputTokens", usageCached(st.usage), "costUSD", st.costUSD,
		"elapsedMs", time.Since(start).Milliseconds(),
	)
}

// recordSuccess feeds the metrics recorder after a completed request.
func (s *Server) recordSuccess(req *convert.ChatRequest, streamed bool, st *streamState, start time.Time) {
	if s.deps.Metrics == nil {
		return
	}
	s.deps.Metrics.IncRequest(req.Model, streamed, "ok")
	s.deps.Metrics.AddTokens("input", usageIn(st.usage))
	s.deps.Metrics.AddTokens("output", usageOut(st.usage))
	s.deps.Metrics.AddTokens("cached", usageCached(st.usage))
	s.deps.Metrics.ObserveLatency(req.Model, streamed, time.Since(start).Milliseconds())
	s.deps.Metrics.ObserveCost(req.Model, st.costUSD)
}

// recordFailure feeds the metrics recorder for a failed request.
func (s *Server) recordFailure(req *convert.ChatRequest, streamed bool, result string) {
	if s.deps.Metrics == nil {
		return
	}
	s.deps.Metrics.IncRequest(req.Model, streamed, result)
}

// fmtCost renders a USD float with enough precision for tiny costs.
func fmtCost(usd float64) string {
	return strings.TrimRight(strings.TrimRight(
		strconv.FormatFloat(usd, 'f', 8, 64), "0"), ".")
}

// handleStreamReadError reports a read failure (idle timeout or transport).
// The idle watchdog cancels the request context; a genuine downstream
// disconnect surfaces through r.Context() instead.
func (s *Server) handleStreamReadError(w http.ResponseWriter, r *http.Request, req *convert.ChatRequest, started bool, err error, start time.Time) {
	if r.Context().Err() != nil {
		slog.Warn("client disconnected mid-stream", "model", req.Model,
			"elapsedMs", time.Since(start).Milliseconds())
		return // nothing sensible left to write
	}

	n := s.consecutiveTimeouts.Add(1)
	msg := "Response timeout - request timed out"
	if n >= timeoutReduceThreshold {
		msg = "Response timeout - try reducing context length (summarize earlier messages)"
	}
	s.recordFailure(req, req.Stream, "timeout")
	slog.Warn("stream read failed",
		"model", req.Model, "stream", req.Stream,
		"consecutiveTimeouts", n, "error", err,
		"elapsedMs", time.Since(start).Milliseconds(),
	)
	if !started {
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", msg, 5)
		return
	}
	_, _ = io.WriteString(w, streamErrorChunk(msg, "rate_limit_error"))
	_, _ = io.WriteString(w, stream.DoneSSE)
}

func usageIn(u *stream.Usage) int {
	if u == nil {
		return 0
	}
	return u.InputTokens
}

func usageOut(u *stream.Usage) int {
	if u == nil {
		return 0
	}
	return u.OutputTokens
}

func usageCached(u *stream.Usage) int {
	if u == nil {
		return 0
	}
	return u.CachedInputTokens
}
