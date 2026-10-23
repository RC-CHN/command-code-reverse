package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/convert"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/ids"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stats"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/stream"
)

// Upstream abstracts the generate call so the keypool (step 6) can plug in
// without touching the handler.
type Upstream interface {
	// Generate starts an upstream stream. The hint carries the downstream
	// identity for key affinity; it may be empty. meta carries the
	// conversation root and trace ID for disguise identity derivation.
	Generate(ctx context.Context, hint string, meta commandcode.CallMeta, req *commandcode.GenerateRequest) (io.ReadCloser, error)
}

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req convert.ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)).Decode(&req); err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("Request body exceeds the configured limit of %d bytes; reduce image sizes or increase MAX_BODY_BYTES", sizeErr.Limit), 0)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body: "+err.Error(), 0)
		return
	}
	stats.SetModel(r.Context(), req.Model, req.Stream)
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required", 0)
		return
	}
	if commandcode.ResolveSystemOneModel(req.Model) != "" {
		writeCodedError(w, 400, "invalid_request_error", "unsupported_endpoint", "Jev uses POST /v1/systemone with state and questions", 0)
		return
	}

	// Conversation-root-derived identity: stable within a conversation
	// (prefix-chain growth keeps the root constant), fresh across them.
	// One trace ID per request; retry attempts share it (fresh span each),
	// mirroring the CLI's per-iteration trace.
	meta := commandcode.CallMeta{
		Root:    convert.ConversationRoot(req.Messages),
		TraceID: commandcode.NewTraceID(),
	}
	wire, err := convert.ToWire(&req, s.deps.Sessions.ThreadID(meta.Root), s.cfg.MaxTokensClamp, 64000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
		return
	}

	idle := s.cfg.StreamIdleTimeout
	if !req.Stream {
		idle = s.cfg.NonStreamIdleTimeout
	}
	watch := newChatIdle(r.Context(), idle)
	defer watch.close()
	watch.begin()
	body, err := s.deps.Upstream.Generate(watch.ctx, downstreamKey(r.Context()), meta, wire)
	err = watch.end(err)
	if body != nil {
		defer func() { _ = body.Close() }()
	}
	if err != nil {
		var ae *commandcode.APIError
		var unavailable *keypool.UnavailableError
		if r.Context().Err() != nil || (!errors.As(err, &ae) && !errors.As(err, &unavailable)) {
			s.handleStreamReadError(w, r, &req, false, err, start)
			return
		}
		slog.Warn("upstream generate failed", "model", req.Model, "error", err)
		s.recordFailure(r, &req, req.Stream, "upstream_error")
		writeUpstreamError(w, err)
		return
	}
	reader := &chatIdleReader{reader: body, watch: watch}
	if req.Stream {
		s.serveStream(w, r, reader, &req, start)
	} else {
		s.serveNonStream(w, r, reader, &req, start)
	}
}

// streamState tracks an in-flight upstream stream for both handlers.
type streamState struct {
	fullText                 strings.Builder
	reasoning                strings.Builder
	toolCalls                []map[string]any
	toolCallIdx              int
	finishReason             string
	usage                    *stream.Usage
	costUSD                  float64
	costReported             bool
	chunkCount               int
	terminalSeen             bool // a finish event; abort is an explicit failure
	hasText                  bool
	hasTool                  bool
	pendingTools             map[string]struct{}
	capturedCacheWriteTokens stream.TokenCount
}

// absorb folds bookkeeping-only events (usage, finish reason, cost) into
// the state. Returns nothing; visible events are handled by the caller.
func (st *streamState) absorb(ev *stream.Event) {
	reason := ev.OpenAIFinishReason()
	switch ev.Type {
	case "finish-step":
		if !st.terminalSeen && (ev.FinishReason != "" || reason == "content_filter") {
			st.finishReason = reason
		}
		if ev.Usage != nil {
			st.usage = ev.Usage
		}
		st.absorbCost(ev)
	case "finish":
		st.terminalSeen = true
		if ev.FinishReason != "" || reason == "content_filter" || st.finishReason == "" {
			st.finishReason = reason
		}
		if ev.TotalUsage != nil {
			st.usage = ev.TotalUsage
		}
	case "provider-metadata":
		st.absorbCost(ev)
	case "cache-write-tokens":
		if ev.CacheWriteTokens.Valid {
			st.capturedCacheWriteTokens = ev.CacheWriteTokens
		}
	}
}

// protocolFailure rejects incomplete events before they can become a successful
// finish or a downstream tool call. Omitted finish reasons retain compatibility
// with upstream finish events that report only totalUsage.
func (st *streamState) protocolFailure(ev *stream.Event) *openAIErrorBody {
	if st.terminalSeen {
		switch ev.Type {
		case "text-delta", "reasoning-delta", "tool-call", "tool-input-start", "tool-input-delta", "tool-input-end":
			return &openAIErrorBody{Message: "Upstream response continued after its finish event", Type: "server_error", Code: "upstream_invalid_sequence"}
		}
	}
	switch ev.Type {
	case "abort":
		return &openAIErrorBody{Message: "Upstream response was aborted before completion", Type: "server_error", Code: "upstream_aborted"}
	case "finish", "finish-step":
		reason := ev.OpenAIFinishReason()
		if reason == "" {
			return &openAIErrorBody{Message: "Upstream response has an invalid finish reason", Type: "server_error", Code: "upstream_invalid_finish"}
		}
		if ev.Type == "finish" {
			if len(st.pendingTools) != 0 {
				return &openAIErrorBody{Message: "Upstream response ended with an incomplete tool call", Type: "server_error", Code: "upstream_invalid_tool_call"}
			}
			if st.terminalSeen && (ev.FinishReason != "" || reason == "content_filter") && reason != st.finishReason {
				return &openAIErrorBody{Message: "Upstream response has conflicting finish events", Type: "server_error", Code: "upstream_invalid_finish"}
			}
		}
	case "tool-input-start", "tool-input-delta", "tool-input-end":
		if strings.TrimSpace(ev.ID) == "" {
			return &openAIErrorBody{Message: "Upstream response contains an incomplete tool call", Type: "server_error", Code: "upstream_invalid_tool_call"}
		}
		if st.pendingTools == nil {
			st.pendingTools = make(map[string]struct{})
		}
		// Even tool-input-end only closes the raw input stream. A complete,
		// validated tool-call with the same ID is required before success.
		st.pendingTools[ev.ID] = struct{}{}
	case "tool-call":
		input := strings.TrimSpace(string(ev.Input))
		if strings.TrimSpace(ev.ToolCallID) == "" || strings.TrimSpace(ev.ToolName) == "" || len(input) == 0 || input[0] != '{' || !json.Valid([]byte(input)) {
			return &openAIErrorBody{Message: "Upstream response contains an incomplete tool call", Type: "server_error", Code: "upstream_invalid_tool_call"}
		}
		delete(st.pendingTools, ev.ToolCallID)
	}
	return nil
}

func (st *streamState) completionFailure() *openAIErrorBody {
	if len(st.pendingTools) != 0 {
		return &openAIErrorBody{Message: "Upstream response ended with an incomplete tool call", Type: "server_error", Code: "upstream_invalid_tool_call"}
	}
	if st.finishReason == "tool_calls" && !st.hasTool {
		return &openAIErrorBody{Message: "Upstream response ended without a complete tool call", Type: "server_error", Code: "upstream_invalid_tool_call"}
	}
	// The output budget may run out entirely during reasoning, even when the
	// provider does not expose it. Preserve an explicit length-limited finish.
	if !st.hasText && !st.hasTool && st.finishReason != "length" && st.finishReason != "content_filter" {
		return &openAIErrorBody{Message: "Upstream response ended without text or a complete tool call", Type: "server_error", Code: "upstream_incomplete_response"}
	}
	return nil
}

// finalizeUsage follows the CLI: the last valid captured count only fills a
// zero total after the entire stream, including events after finish, is read.
func (st *streamState) finalizeUsage() {
	if !st.capturedCacheWriteTokens.Valid {
		return
	}
	if st.usage == nil {
		st.usage = &stream.Usage{}
	}
	if st.usage.CacheWriteTokens == 0 {
		st.usage.CacheWriteTokens = st.capturedCacheWriteTokens.Value
	}
}

// absorbCost captures gateway cost from finish-step / provider-metadata.
func (st *streamState) absorbCost(ev *stream.Event) {
	if c, ok := ev.ProviderMetadata.ReportedCostUSD(); ok {
		st.costReported = true
		if c > 0 {
			st.costUSD = c
		}
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
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, body io.Reader, req *convert.ChatRequest, start time.Time) {
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
		// In-band terminal markers (billing/plan) — the old proxy swallowed
		// these; we surface them with explicit semantics.
		if te := stream.TerminalError(ev); te != nil {
			status, typ, msg := terminalErrorStatus(te.Message)
			slog.Warn("terminal marker in stream", "marker", te.Message, "model", req.Model)
			s.recordFailure(r, req, true, "terminal_marker")
			if !started {
				writeError(w, status, typ, msg, 0)
			} else {
				_, _ = io.WriteString(w, streamErrorChunk(msg, typ))
				_, _ = io.WriteString(w, stream.DoneSSE)
			}
			return
		}

		if ae := ev.APIError(); ae != nil {
			s.recordFailure(r, req, req.Stream, "upstream_error")
			if !started {
				writeUpstreamError(w, ae)
			} else {
				_, body, _ := classifyUpstreamError(ae)
				b, _ := json.Marshal(openAIError{Error: body})
				_, _ = io.WriteString(w, "data: "+string(b)+"\n\n"+stream.DoneSSE)
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
		if failure := st.protocolFailure(ev); failure != nil {
			s.handleProtocolFailure(w, r, req, started, *failure)
			return
		}

		switch ev.Type {
		case "text-delta":
			if ev.Text == "" {
				continue
			}
			st.hasText = st.hasText || strings.TrimSpace(ev.Text) != ""
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
			delta := map[string]any{"reasoning_content": ev.Text}
			if st.chunkCount == 0 {
				delta["role"] = "assistant"
			}
			st.chunkCount++
			writeChunk(stream.NewChunk(completionID, created, req.Model, delta, nil, nil))

		case "tool-call":
			st.hasTool = true
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

		case "finish", "finish-step", "provider-metadata", "cache-write-tokens":
			st.absorb(ev)
		}
		// start / start-step / reasoning-start / reasoning-end / text-start /
		// text-end / provider-metadata: intentionally silent. start-step and
		// provider-metadata leak upstream internals — never forwarded.
	}

	if !st.terminalSeen {
		s.handleTruncatedStream(w, r, req, started)
		return
	}
	st.finalizeUsage()
	if failure := st.completionFailure(); failure != nil {
		s.handleProtocolFailure(w, r, req, started, *failure)
		return
	}
	// Read the complete tail before declaring success: an abort/error may follow
	// finish, and provider metadata can still update final accounting.
	writeChunk(stream.NewChunk(completionID, created, req.Model,
		map[string]any{}, &st.finishReason, stream.UsageFromUpstream(st.usage)))
	_, _ = io.WriteString(w, stream.DoneSSE)
	if flusher != nil {
		flusher.Flush()
	}
	s.recordSuccess(r, req, true, st, start)
	slog.Info("chat completion served",
		"model", req.Model, "stream", true,
		"inputTokens", usageIn(st.usage), "outputTokens", usageOut(st.usage),
		"cachedInputTokens", usageCached(st.usage), "costUSD", st.costUSD,
		"cacheWriteTokens", usageCacheWrite(st.usage),
		"elapsedMs", time.Since(start).Milliseconds(),
	)
}

// serveNonStream aggregates the upstream stream into a single JSON response.
func (s *Server) serveNonStream(w http.ResponseWriter, r *http.Request, body io.Reader, req *convert.ChatRequest, start time.Time) {
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
		if te := stream.TerminalError(ev); te != nil {
			status, typ, msg := terminalErrorStatus(te.Message)
			slog.Warn("terminal marker in stream", "marker", te.Message, "model", req.Model)
			s.recordFailure(r, req, false, "terminal_marker")
			writeError(w, status, typ, msg, 0)
			return
		}

		if ae := ev.APIError(); ae != nil {
			s.recordFailure(r, req, req.Stream, "upstream_error")
			writeUpstreamError(w, ae)
			return
		}
		if failure := st.protocolFailure(ev); failure != nil {
			s.handleProtocolFailure(w, r, req, false, *failure)
			return
		}

		switch ev.Type {
		case "text-delta":
			st.hasText = st.hasText || strings.TrimSpace(ev.Text) != ""
			st.fullText.WriteString(ev.Text)
		case "reasoning-delta":
			st.reasoning.WriteString(ev.Text)
		case "tool-call":
			st.hasTool = true
			st.toolCalls = append(st.toolCalls, map[string]any{
				"id":   ev.ToolCallID,
				"type": "function",
				"function": map[string]any{
					"name":      ev.ToolName,
					"arguments": toolCallArgs(ev.Input),
				},
			})
		case "finish", "finish-step", "provider-metadata", "cache-write-tokens":
			st.absorb(ev)
		}
	}

	if !st.terminalSeen {
		s.handleTruncatedStream(w, r, req, false)
		return
	}
	st.finalizeUsage()
	if failure := st.completionFailure(); failure != nil {
		s.handleProtocolFailure(w, r, req, false, *failure)
		return
	}
	if usageOut(st.usage) == 0 && st.finishReason != "length" && st.finishReason != "content_filter" {
		s.recordFailure(r, req, false, "empty_response")
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"Empty response from upstream (zero output tokens)", 10)
		return
	}

	message := map[string]any{"role": "assistant"}
	if st.fullText.Len() > 0 {
		message["content"] = st.fullText.String()
	} else {
		message["content"] = nil
	}
	if len(st.toolCalls) > 0 {
		message["tool_calls"] = st.toolCalls
	}
	if st.reasoning.Len() > 0 {
		message["reasoning_content"] = st.reasoning.String()
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
	s.recordSuccess(r, req, false, st, start)
	slog.Info("chat completion served",
		"model", req.Model, "stream", false,
		"inputTokens", usageIn(st.usage), "outputTokens", usageOut(st.usage),
		"cachedInputTokens", usageCached(st.usage), "costUSD", st.costUSD,
		"cacheWriteTokens", usageCacheWrite(st.usage),
		"elapsedMs", time.Since(start).Milliseconds(),
	)
}

// Failure frames contain only fixed protocol diagnostics, never raw upstream
// event fields. Once streaming has begun, keep the existing error + DONE shape.
func (s *Server) handleProtocolFailure(w http.ResponseWriter, r *http.Request, req *convert.ChatRequest, started bool, body openAIErrorBody) {
	if r.Context().Err() != nil {
		return
	}
	s.recordFailure(r, req, req.Stream, body.Code)
	if !started {
		writeErrorBody(w, http.StatusBadGateway, body, 0)
		return
	}
	b, _ := json.Marshal(openAIError{Error: body})
	_, _ = io.WriteString(w, "data: "+string(b)+"\n\n"+stream.DoneSSE)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// recordSuccess feeds the metrics recorder after a completed request.
func (s *Server) recordSuccess(r *http.Request, req *convert.ChatRequest, streamed bool, st *streamState, start time.Time) {
	if observed := stats.FromContext(r.Context()); observed != nil {
		observed.Result = "ok"
		if u := st.usage; u != nil {
			observed.Usage = &stats.Tokens{Input: int64(u.InputTokens), Output: int64(u.OutputTokens), Cached: int64(u.CachedInputTokens), CacheWrite: int64(u.CacheWriteTokens), Reasoning: int64(u.ReasoningTokens)}
		}
		if st.costReported {
			cost := st.costUSD
			observed.CostUSD = &cost
		}
	}
	if s.deps.Metrics == nil {
		return
	}
	s.deps.Metrics.IncRequest(req.Model, streamed, "ok")
	s.deps.Metrics.AddTokens("input", usageIn(st.usage))
	s.deps.Metrics.AddTokens("output", usageOut(st.usage))
	s.deps.Metrics.AddTokens("cached", usageCached(st.usage))
	s.deps.Metrics.AddTokens("cache_write", usageCacheWrite(st.usage))
	s.deps.Metrics.ObserveLatency(req.Model, streamed, time.Since(start).Milliseconds())
	s.deps.Metrics.ObserveCost(req.Model, st.costUSD)
}

// recordFailure feeds the metrics recorder for a failed request.
func (s *Server) recordFailure(r *http.Request, req *convert.ChatRequest, streamed bool, result string) {
	stats.SetResult(r.Context(), result)
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

// handleTruncatedStream reports a clean EOF without a protocol terminal event.
// Partial output must not become a successful completion or trigger a replay.
func (s *Server) handleTruncatedStream(w http.ResponseWriter, r *http.Request, req *convert.ChatRequest, started bool) {
	if r.Context().Err() != nil {
		return
	}
	s.recordFailure(r, req, req.Stream, "truncated_response")
	slog.Warn("upstream stream ended without finish or abort", "model", req.Model, "stream", req.Stream)
	body := openAIErrorBody{
		Message: "Upstream response was truncated before completion (no finish event)",
		Type:    "server_error",
		Code:    "upstream_truncated",
	}
	if !started {
		writeErrorBody(w, http.StatusBadGateway, body, 0)
		return
	}
	b, _ := json.Marshal(openAIError{Error: body})
	_, _ = io.WriteString(w, "data: "+string(b)+"\n\n"+stream.DoneSSE)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// handleStreamReadError reports a read failure (idle timeout or transport).
// The I/O watchdog preserves its cancellation cause; downstream cancellation
// belongs to r.Context() and must not become an upstream failure.
func (s *Server) handleStreamReadError(w http.ResponseWriter, r *http.Request, req *convert.ChatRequest, started bool, err error, start time.Time) {
	if r.Context().Err() != nil {
		s.recordFailure(r, req, req.Stream, "canceled")
		slog.Warn("client disconnected mid-stream", "model", req.Model,
			"elapsedMs", time.Since(start).Milliseconds())
		return // nothing sensible left to write
	}

	if errors.Is(err, stream.ErrMalformedEvent) {
		s.handleProtocolFailure(w, r, req, started, openAIErrorBody{
			Message: "Upstream response contains a malformed protocol event",
			Type:    "server_error",
			Code:    "upstream_malformed_event",
		})
		return
	}

	status := http.StatusBadGateway
	body := openAIErrorBody{Type: "server_error", Code: "upstream_connection_error", Message: "Upstream response connection interrupted"}
	result := body.Code
	var networkError net.Error
	switch {
	case errors.Is(err, errUpstreamIdle):
		status, result = http.StatusGatewayTimeout, "timeout"
		body.Code, body.Message = "upstream_idle_timeout", "Upstream response idle timeout while waiting for data"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkError) && networkError.Timeout():
		status, result = http.StatusGatewayTimeout, "timeout"
		body.Code, body.Message = "upstream_timeout", "Upstream request timed out"
	}
	s.recordFailure(r, req, req.Stream, result)
	slog.Warn("stream read failed",
		"model", req.Model, "stream", req.Stream,
		"code", body.Code, "error", err,
		"elapsedMs", time.Since(start).Milliseconds(),
	)
	if !started {
		writeErrorBody(w, status, body, 0)
		return
	}
	b, _ := json.Marshal(openAIError{Error: body})
	_, _ = io.WriteString(w, "data: "+string(b)+"\n\n"+stream.DoneSSE)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
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

func usageCacheWrite(u *stream.Usage) int {
	if u == nil {
		return 0
	}
	return u.CacheWriteTokens
}
